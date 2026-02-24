# Component C: Emitter

## Purpose

The `Emitter` is the bridge between event-producing code (HTTP interceptor, policy plugins) and event-consuming sinks (JSONLWriter). It stamps static metadata (run ID, agent system, timestamp) onto every event and dispatches to one or more sinks. It provides a clean, simple API for emission sites.

## Codebase References

- **DI Pattern:** Follows the `*slog.Logger` injection pattern in `pkg/policy/engine.go:26-29` -- passed to constructors, nil means "disabled"
- **Config Struct Pattern:** `EmitterConfig` follows the same plain-struct convention as `api.Resources` in `pkg/api/config.go:63-69`
- **Close Pattern:** Multi-resource close with first-error collection, same as `sandbox.Close()` in `pkg/sandbox/sandbox_darwin.go:475-582`

## File Location

`pkg/logging/emitter.go`

## Implementation

```go
package logging

import (
    "encoding/json"
    "time"

    "github.com/jingkaihe/matchlock/internal/errx"
)

// EmitterConfig holds the static metadata configured at sandbox startup.
// All fields are stamped onto every event automatically.
type EmitterConfig struct {
    RunID       string // Caller-supplied; defaults to sandbox VM ID if empty
    AgentSystem string // Set by consumer at startup (e.g., "openclaw", "aider")
}

// Emitter provides convenience methods for emitting typed events.
// It holds static metadata and dispatches to one or more sinks.
//
// A nil *Emitter is safe to hold; callers guard emission with:
//
//     if emitter != nil {
//         _ = emitter.Emit(...)
//     }
type Emitter struct {
    config EmitterConfig
    sinks  []Sink
}

// NewEmitter creates an emitter with the given configuration and sinks.
// The RunID should be pre-defaulted by the caller (to sandbox VM ID)
// before passing the config.
func NewEmitter(cfg EmitterConfig, sinks ...Sink) *Emitter {
    return &Emitter{
        config: cfg,
        sinks:  sinks,
    }
}

// Emit constructs an event with the emitter's static metadata and writes
// it to all registered sinks.
//
// Parameters:
//   - eventType: one of the Event* constants (e.g., EventLLMRequest)
//   - summary: human-readable one-line summary
//   - plugin: the emitting plugin name (empty string if not from a plugin)
//   - tags: optional tags for filtering (nil is fine)
//   - data: the typed data struct (e.g., *LLMRequestData); nil for no payload
//
// Returns the first error encountered. Callers should discard errors
// with _ = (best-effort semantics).
func (e *Emitter) Emit(eventType, summary, plugin string, tags []string, data interface{}) error {
    var rawData json.RawMessage
    if data != nil {
        b, err := json.Marshal(data)
        if err != nil {
            return errx.Wrap(ErrMarshalData, err)
        }
        rawData = b
    }

    event := &Event{
        Timestamp:   time.Now().UTC(),
        RunID:       e.config.RunID,
        AgentSystem: e.config.AgentSystem,
        EventType:   eventType,
        Summary:     summary,
        Plugin:      plugin,
        Tags:        tags,
        Data:        rawData,
    }

    for _, sink := range e.sinks {
        if err := sink.Write(event); err != nil {
            return err
        }
    }
    return nil
}

// Close closes all sinks. Returns the first error encountered.
func (e *Emitter) Close() error {
    var firstErr error
    for _, sink := range e.sinks {
        if err := sink.Close(); err != nil && firstErr == nil {
            firstErr = err
        }
    }
    return firstErr
}
```

## Dependencies

- `encoding/json` (standard library)
- `time` (standard library)
- `github.com/jingkaihe/matchlock/internal/errx`
- `pkg/logging/event.go` (Event, constants)
- `pkg/logging/sink.go` (Sink interface)
- `pkg/logging/errors.go` (ErrMarshalData)

## Test Criteria

1. **Metadata stamping:** `Emit()` populates `ts`, `run_id`, `agent_system` from config on every event
2. **Timestamp is UTC:** `event.Timestamp` is always in UTC
3. **Data marshaling:** Passing a `*LLMRequestData` produces correct `json.RawMessage` in the event
4. **Nil data:** Passing `nil` for data results in `Data: nil` (omitted from JSON)
5. **Multi-sink dispatch:** With two sinks, both receive the same event
6. **Sink error propagation:** If the first sink errors, the error is returned (and subsequent sinks may or may not be called -- implementation detail)
7. **Close propagation:** `Close()` calls `Close()` on all sinks, returns first error
8. **Close error collection:** If both sinks error on close, only the first error is returned
9. **Empty sinks:** Emitter with no sinks does not panic, `Emit()` returns nil

## Test Helper: CaptureSink

For testing, provide an in-memory sink:

```go
// pkg/logging/emitter_test.go (test-only helper)

type captureSink struct {
    mu     sync.Mutex
    events []*Event
    closed bool
}

func (s *captureSink) Write(event *Event) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.events = append(s.events, event)
    return nil
}

func (s *captureSink) Close() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.closed = true
    return nil
}
```

## Acceptance Criteria

- [ ] `Emitter` is a struct with `NewEmitter` constructor
- [ ] `Emit` stamps `ts`, `run_id`, `agent_system` automatically
- [ ] `Emit` marshals the `data` parameter to `json.RawMessage`
- [ ] `Emit` dispatches to all sinks
- [ ] `Close` closes all sinks and returns first error
- [ ] No goroutines, no channels, no buffering -- synchronous dispatch
- [ ] Works correctly with zero sinks (no-op)
