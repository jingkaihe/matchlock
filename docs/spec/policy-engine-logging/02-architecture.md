# 02 - Architecture: Policy Engine Logging Standard

## System Design

### Component Diagram

```
cmd/matchlock/cmd_run.go
  |
  | --event-log, --run-id, --agent-system flags
  |     populate api.Config.Logging
  v
pkg/sandbox/sandbox_{darwin,linux}.go  (sandbox.New)
  |
  | 1. Read config.Logging
  | 2. Determine log path (flag override or default)
  | 3. Create JSONLWriter(path)
  | 4. Create Emitter(config, sinks...)
  |
  +---> pkg/policy/engine.go  (NewEngine)
  |       |
  |       | Stores *logging.Emitter (nil-safe)
  |       | Distributes to plugins via constructor
  |       |
  |       +---> pkg/policy/secret_injector.go
  |               |
  |               | Emits: key_injection
  |               | (injected, skipped, leak_blocked)
  |
  +---> pkg/net/{stack_darwin,proxy}.go
          |
          +---> pkg/net/http.go  (HTTPInterceptor)
                  |
                  | Emits: llm_request, llm_response
                  | (from HandleHTTPS and HandleHTTP)
```

### Data Flow

```
Event origin (HTTPInterceptor / secretInjectorPlugin)
  |
  | emitter.Emit(eventType, summary, plugin, tags, data)
  v
Emitter
  |
  | 1. Marshal data to json.RawMessage
  | 2. Stamp ts, run_id, agent_system
  | 3. Construct Event struct
  |
  | for each sink:
  +---> Sink.Write(*Event)
          |
          v
        JSONLWriter
          |
          | 1. mu.Lock()
          | 2. json.Encoder.Encode(event)  -->  *os.File (append)
          | 3. mu.Unlock()
          v
        ~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl
```

### Three Output Systems (Independent)

```
                    +---> slog (stderr, text)
                    |     Audience: developer
Network event  -----+---> api.Event channel (in-memory)
                    |     Audience: Go SDK consumer
                    +---> JSON-L Emitter (persistent file)
                          Audience: post-run analysis
```

These systems share no data flow. The JSON-L emitter is called from the same code paths that already call `slog` and `emitEvent`, but they are independent invocations.

## Dependency Graph

```
pkg/logging/event.go          (no internal deps)
pkg/logging/sink.go           (depends on: event.go)
pkg/logging/jsonl_writer.go   (depends on: sink.go, event.go)
pkg/logging/emitter.go        (depends on: sink.go, event.go)
pkg/logging/errors.go         (no internal deps)

pkg/api/config.go             (adds LoggingConfig struct -- no dep on pkg/logging)
pkg/policy/engine.go          (imports pkg/logging for *Emitter type)
pkg/policy/secret_injector.go (imports pkg/logging for Emit calls)
pkg/net/http.go               (imports pkg/logging for *Emitter type and Emit calls)
pkg/net/stack_darwin.go       (imports pkg/logging for *Emitter type in Config)
pkg/net/proxy.go              (imports pkg/logging for *Emitter type in ProxyConfig)
pkg/sandbox/sandbox_darwin.go (imports pkg/logging for construction)
pkg/sandbox/sandbox_linux.go  (imports pkg/logging for construction)
cmd/matchlock/cmd_run.go      (no direct pkg/logging import -- passes config)
```

## Nil-Safety Design

The emitter follows the same nil-safety pattern as the existing `*slog.Logger` injection:

```go
// Existing pattern in pkg/policy/engine.go:26-29
func NewEngine(config *api.NetworkConfig, logger *slog.Logger) *Engine {
    if logger == nil {
        logger = slog.Default()
    }
    ...
}
```

For the emitter, nil means "no event logging":

```go
// All emission sites guard with nil check
if emitter != nil {
    _ = emitter.Emit(...)
}
```

This means:
- **No emitter** = zero overhead, zero behavioral change
- **Emitter with no sinks** = metadata stamping only, events discarded
- **Emitter with JSONLWriter** = events persisted to file

## File Location Strategy

### Persistent Log Path

Default: `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl`

This is deliberately separate from the per-VM state directory (`~/.local/share/matchlock/<vm-id>/`) which is managed by `state.Manager` and cleaned up by `sandbox.Close()`. The `logs/` directory sits alongside the per-VM directories under the same root but is not subject to VM lifecycle cleanup.

**Pattern Reference:** The base directory `~/.local/share/matchlock/` is already established by `state.Manager` in `pkg/state/state.go:32-33` which uses `~/.matchlock/vms` (but the XDG convention would be `~/.local/share`). Note: The draft specifies `~/.local/share/matchlock/logs/` as the path. This should use `state.Manager.baseDir` as the root to stay consistent with whatever base directory the state manager uses, so the actual path is `<state.Manager.baseDir>/../logs/<sandbox-id>/events.jsonl` resolved relative to the state base.

**Simplification:** Since `state.Manager` already manages `~/.matchlock/vms/`, the logs directory should be `~/.matchlock/logs/<sandbox-id>/events.jsonl` to remain under the same root. This avoids creating a parallel directory tree. The implementation should derive the log path from the state manager's base directory.

### Override

The `--event-log <path>` CLI flag or `config.Logging.EventLogPath` allows specifying an exact file path for the JSON-L output. When set, the default path is not used.

## Error Handling Strategy

Event emission errors must never crash or block the sandbox. The strategy is:

1. `Emitter.Emit()` returns an error, but all call sites discard it with `_ =`
2. The `JSONLWriter.Write()` method holds the mutex only for the duration of one `json.Encoder.Encode()` call
3. If the file write fails, the event is lost (best-effort semantics, consistent with the existing `api.Event` channel's fire-and-forget pattern in `pkg/net/http.go:313-329`)
4. `Emitter.Close()` calls `file.Sync()` then `file.Close()` and returns the first error

## Concurrency Model

- `JSONLWriter` uses `sync.Mutex` to serialize writes (events come from multiple goroutines in the HTTP handler)
- The mutex scope is minimal: one JSON encode + one file write
- Event rates are low (3-10 events per LLM interaction with pauses between), so contention is negligible
- No buffering, no periodic flush, no background goroutines
