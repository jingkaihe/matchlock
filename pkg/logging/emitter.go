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
//	if emitter != nil {
//	    _ = emitter.Emit(...)
//	}
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
//   - eventType: one of the Event* constants (e.g., EventHTTPRequest)
//   - summary: human-readable one-line summary
//   - plugin: the emitting plugin name (empty string if not from a plugin)
//   - tags: optional tags for filtering (nil is fine)
//   - data: the typed data struct (e.g., *HTTPRequestData); nil for no payload
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
