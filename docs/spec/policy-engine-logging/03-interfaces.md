# 03 - Interfaces: Policy Engine Logging Standard

All interfaces follow Go conventions established in the matchlock codebase. Type definitions use standard library types (`time.Time`, `encoding/json.RawMessage`). The package uses `log/slog` for internal diagnostics (consistent with the rest of the codebase) and the `internal/errx` pattern for sentinel errors.

## Core Interfaces

### Sink

The `Sink` interface is the extension point for event consumers. The `JSONLWriter` is the v0 implementation; future sinks could include remote endpoints, aggregation buffers, or test capture.

**Pattern Reference:** This follows the `io.WriteCloser` decomposition pattern. The matchlock codebase does not have a direct analog, but the `vfs.Provider` interface in `pkg/vfs/` demonstrates the same "small interface, multiple implementations" approach.

```go
// pkg/logging/sink.go
package logging

// Sink consumes structured events.
// Implementations must be safe for concurrent use.
type Sink interface {
    // Write persists or forwards a single event.
    // Implementations should not modify the event.
    Write(event *Event) error

    // Close flushes any buffered data and releases resources.
    Close() error
}
```

### Event (Struct, not interface)

**Pattern Reference:** Follows the `api.Event` struct pattern in `pkg/api/vm.go:47-53` -- a typed struct with JSON tags and optional nested data.

```go
// pkg/logging/event.go
package logging

import (
    "encoding/json"
    "time"
)

// Event is the canonical structured event for the policy engine logging standard.
// Every event carries the required fields; optional fields use omitempty.
type Event struct {
    Timestamp   time.Time       `json:"ts"`
    RunID       string          `json:"run_id"`
    AgentSystem string          `json:"agent_system"`
    EventType   string          `json:"event_type"`
    Summary     string          `json:"summary"`
    Plugin      string          `json:"plugin,omitempty"`
    Tags        []string        `json:"tags,omitempty"`
    Data        json.RawMessage `json:"data,omitempty"`
}
```

### Event Type Constants

```go
// pkg/logging/event.go (continued)

const (
    EventLLMRequest   = "llm_request"
    EventLLMResponse  = "llm_response"
    EventBudgetAction = "budget_action"
    EventKeyInjection = "key_injection"
)
```

### Data Structs (Per Event Type)

These structs are marshaled into the `Data` field as `json.RawMessage`.

```go
// pkg/logging/event.go (continued)

// LLMRequestData is the data payload for llm_request events.
type LLMRequestData struct {
    Method   string `json:"method"`
    Host     string `json:"host"`
    Path     string `json:"path"`
    Model    string `json:"model,omitempty"`
    Routed   bool   `json:"routed"`
    RoutedTo string `json:"routed_to,omitempty"`
}

// LLMResponseData is the data payload for llm_response events.
type LLMResponseData struct {
    Method     string `json:"method"`
    Host       string `json:"host"`
    Path       string `json:"path"`
    StatusCode int    `json:"status_code"`
    DurationMS int64  `json:"duration_ms"`
    BodyBytes  int64  `json:"body_bytes"`
    Model      string `json:"model,omitempty"`
}

// BudgetActionData is the data payload for budget_action events.
// This is a placeholder for v0; no emitter calls this type yet.
type BudgetActionData struct {
    Action     string  `json:"action"`
    TokensUsed int64   `json:"tokens_used,omitempty"`
    CostUSD    float64 `json:"cost_usd,omitempty"`
    Remaining  float64 `json:"remaining,omitempty"`
}

// KeyInjectionData is the data payload for key_injection events.
type KeyInjectionData struct {
    SecretName string `json:"secret_name"`
    Host       string `json:"host"`
    Action     string `json:"action"` // "injected", "skipped", "leak_blocked"
}
```

### EmitterConfig

```go
// pkg/logging/emitter.go

// EmitterConfig holds the static metadata configured at sandbox startup.
// All fields are stamped onto every event automatically.
type EmitterConfig struct {
    RunID       string // Caller-supplied; defaults to sandbox VM ID if empty
    AgentSystem string // Set by consumer at startup (e.g., "openclaw", "aider")
}
```

### Emitter

```go
// pkg/logging/emitter.go (continued)

// Emitter provides convenience methods for emitting typed events.
// It holds static metadata and dispatches to one or more sinks.
// A nil *Emitter is safe; callers guard with `if emitter != nil`.
type Emitter struct {
    config EmitterConfig
    sinks  []Sink
}

// NewEmitter creates an emitter with the given configuration and sinks.
func NewEmitter(cfg EmitterConfig, sinks ...Sink) *Emitter

// Emit constructs an event with the emitter's metadata and writes to all sinks.
// The data parameter is marshaled to JSON; pass nil for events with no data payload.
// Returns the first error encountered, but callers should discard errors (best-effort).
func (e *Emitter) Emit(eventType, summary, plugin string, tags []string, data interface{}) error

// Close closes all sinks. Returns the first error encountered.
func (e *Emitter) Close() error
```

### JSONLWriter

```go
// pkg/logging/jsonl_writer.go

// JSONLWriter writes structured events as JSON-L to a file.
// It implements Sink and is safe for concurrent use.
type JSONLWriter struct {
    mu   sync.Mutex
    file *os.File
    enc  *json.Encoder
}

// NewJSONLWriter creates a new JSON-L writer that appends to the given file path.
// The parent directory must already exist. The file is created if it does not exist.
func NewJSONLWriter(path string) (*JSONLWriter, error)

// Write serializes the event as a single JSON line and writes it to the file.
func (w *JSONLWriter) Write(event *Event) error

// Close syncs and closes the underlying file.
func (w *JSONLWriter) Close() error
```

## Config Integration Interface

### LoggingConfig

**Pattern Reference:** Follows the same struct-with-JSON-tags pattern as `api.NetworkConfig` in `pkg/api/config.go:79-96`.

```go
// Added to pkg/api/config.go

// LoggingConfig configures the structured event logging system.
type LoggingConfig struct {
    EventLogPath string `json:"event_log_path,omitempty"` // Override file path; empty = default
    Enabled      bool   `json:"enabled,omitempty"`        // Enable JSON-L event logging
    RunID        string `json:"run_id,omitempty"`         // Caller-supplied session ID
    AgentSystem  string `json:"agent_system,omitempty"`   // e.g., "openclaw", "aider"
}
```

Added to `Config` struct:

```go
type Config struct {
    // ... existing fields ...
    Logging    *LoggingConfig    `json:"logging,omitempty"`
}
```

## Modified Constructor Signatures

### policy.NewEngine

**Current signature** (from `pkg/policy/engine.go:26`):

```go
func NewEngine(config *api.NetworkConfig, logger *slog.Logger) *Engine
```

**New signature:**

```go
func NewEngine(config *api.NetworkConfig, logger *slog.Logger, emitter *logging.Emitter) *Engine
```

The emitter is stored on the `Engine` struct and passed to plugins that need it. Nil means no event logging.

### NewSecretInjectorPlugin

**Current signature** (from `pkg/policy/secret_injector.go:27`):

```go
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger) *secretInjectorPlugin
```

**New signature:**

```go
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger, emitter *logging.Emitter) *secretInjectorPlugin
```

### NewHTTPInterceptor

**Current signature** (from `pkg/net/http.go:26`):

```go
func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event, caPool *CAPool, logger *slog.Logger) *HTTPInterceptor
```

**New signature:**

```go
func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event, caPool *CAPool, logger *slog.Logger, emitter *logging.Emitter) *HTTPInterceptor
```

### sandboxnet.Config (Darwin)

**Current struct** (from `pkg/net/stack_darwin.go:55-66`):

```go
type Config struct {
    FD         int
    File       *os.File
    GatewayIP  string
    GuestIP    string
    MTU        uint32
    Policy     *policy.Engine
    Events     chan api.Event
    CAPool     *CAPool
    DNSServers []string
    Logger     *slog.Logger
}
```

**New field added:**

```go
type Config struct {
    // ... existing fields ...
    Emitter    *logging.Emitter
}
```

### sandboxnet.ProxyConfig (Linux)

**Current struct** (from `pkg/net/proxy.go:43-50`):

```go
type ProxyConfig struct {
    BindAddr        string
    HTTPPort        int
    HTTPSPort       int
    PassthroughPort int
    Policy          *policy.Engine
    Events          chan api.Event
    CAPool          *CAPool
    Logger          *slog.Logger
}
```

**New field added:**

```go
type ProxyConfig struct {
    // ... existing fields ...
    Emitter    *logging.Emitter
}
```

## Sentinel Errors

**Pattern Reference:** Follows `internal/errx` pattern documented in `AGENTS.md:79-95`. All sentinel errors use `errors.New` and are wrapped with `errx.Wrap` at call sites.

```go
// pkg/logging/errors.go
package logging

import "errors"

var (
    ErrCreateLogFile = errors.New("logging: create log file")
    ErrWriteEvent    = errors.New("logging: write event")
    ErrMarshalData   = errors.New("logging: marshal event data")
    ErrCloseWriter   = errors.New("logging: close writer")
)
```
