# Policy Engine Logging Standard

# Open Questions

## Unresolved

(All blocking questions resolved. This draft is ready for spec-architect handoff.)

## Resolved

- [x] Q1: Should the new structured event log coexist alongside existing `slog` output, or replace it?
  - Resolution: Coexist. Three separate concerns.
  - Rationale: `slog` serves operational diagnostics (human-readable, ephemeral stderr). `api.Event` channel serves the programmatic Go SDK API. The JSON-L writer serves persistent structured event logging. Each system has a different audience and lifecycle. Adding JSON-L is additive -- no existing behavior changes.
  - Codebase evidence: `slog` calls in `pkg/policy/engine.go` (26 calls) and `pkg/net/http.go` provide real-time dev diagnostics. `api.Event` channel in `pkg/api/vm.go:47-79` serves SDK consumers via `sandbox.Events()`. Neither provides persistent file output.

- [x] Q2: Where does `run_id` come from and what is its lifecycle?
  - Resolution: `run_id` is caller-supplied. If not provided, it defaults to the sandbox VM ID (`vm-<uuid[:8]>`).
  - Rationale: The sandbox VM ID (generated in `pkg/api/config.go:252`) is always known and serves as a sensible default. An orchestrator like OpenClaw may supply its own session ID that spans multiple sandboxes.
  - Codebase evidence: `config.GetID()` in `pkg/api/config.go:250-254` generates `vm-` prefixed UUIDs. This becomes the default `run_id` when the caller does not provide one.

- [x] Q3: Should `agent_system` be determined at sandbox creation time or per-event?
  - Resolution: Static metadata map configured at sandbox startup time. The `Emitter` accepts a metadata map (including `agent_system`) when constructed during `sandbox.New()`. Matchlock remains agent-agnostic -- it does not enumerate or validate agent system names.
  - Rationale: The consumer sets `agent_system` at VM startup (via config or CLI flag), and the emitter stamps it on every event automatically. This keeps matchlock decoupled from specific agent implementations while still producing well-labeled events. No per-event overhead for the caller.
  - Codebase evidence: The existing DI pattern (`*slog.Logger` passed to constructors with nil fallback, e.g., `pkg/policy/engine.go:26-29`) provides a natural precedent for passing the `Emitter` into components at construction time.

- [x] Q4: Where should the JSON-L file be written?
  - Resolution: Persistent logs directory, separate from the sandbox state directory. Default path: `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl`. The `--event-log <path>` flag allows override.
  - Rationale: The sandbox state directory (`~/.local/share/matchlock/<vm-id>/`) is cleaned up by `sandbox.Close()`. Event logs must survive sandbox removal for post-run analysis, cost dashboards, and audit trails. A dedicated `logs/` directory alongside the per-VM state directories provides persistence without interfering with the existing cleanup lifecycle.
  - Codebase evidence: `state.Manager` in `pkg/state/state.go` manages the `~/.local/share/matchlock/` root directory. The `logs/` subdirectory follows the same convention. `sandbox.Close()` in `pkg/sandbox/sandbox_darwin.go:475-582` removes state directory contents but would not touch the separate `logs/` directory.

- [x] Q5: Should the v0 event types include `gate_decision` and `route_decision`?
  - Resolution: No. v0 is strictly the four TLS/L7 lane types: `llm_request`, `llm_response`, `budget_action`, `key_injection`. Policy engine decisions (`gate_decision`, `route_decision`) are planned as v1 additions. See section 9 (v1 Roadmap) for details.
  - Rationale: v0 targets the minimum viable set for answering "what LLM calls happened, what keys were injected, what did they cost." Gate/route decisions are operational diagnostics already well-served by `slog` at Debug/Warn level. Adding them to JSON-L increases scope without proportional value for the first iteration.
  - Codebase evidence: Policy engine decisions are currently logged via `slog` in `pkg/policy/engine.go` lines 178-208 (gate allowed/blocked, route redirect/passthrough/error). These remain unchanged in v0.

- [x] Q6: How should sensitive data (secret values, API keys) be handled in event logs?
  - Resolution: Log secret names but never values. The `KeyInjectionData` struct contains `SecretName` (the env var name, e.g., `"ANTHROPIC_API_KEY"`) and `Host`, but no `Value` field. The `summary` field also contains only the name and host, never the secret value.
  - Rationale: Secret names are not themselves sensitive -- they are environment variable names that are visible in process listings and config files. The actual secret value (e.g., `sk-ant-...`) must never appear in any log output. The schema enforces this by omission: there is no field to put a value in.
  - Codebase evidence: The current `slog.Debug` calls in `pkg/policy/secret_injector.go:80-87` already follow this pattern: they log `"name", name, "host", host` but never the secret value. The value is only handled in `replaceInRequest()` (line 127) and never passed to any logging call.

- [x] Q7: Should the JSON-L writer use buffered or unbuffered writes?
  - Resolution: Start simple with unbuffered writes via `json.Encoder` writing directly to `*os.File`. Add an explicit `file.Sync()` call in `Close()` to flush the final state. No periodic timer, no `bufio.Writer` complexity.
  - Rationale: Event rates in the TLS/L7 lane are low (3-10 events per LLM interaction with pauses between). Go's `os.File.Write` goes to the OS write syscall, which for append-only files is effectively line-buffered by the kernel. This gives good enough durability without `fsync` per event. If performance becomes a concern at higher event rates in the future, `bufio.Writer` can be added as an optimization.
  - Codebase evidence: The existing `api.Event` channel in `pkg/net/http.go:313-329` uses fire-and-forget semantics (non-blocking select with default discard), suggesting the codebase already accepts best-effort event delivery. The JSON-L writer is more durable than the channel by virtue of being a file.

---

## 1. Current State of Logging in Matchlock

### 1.1 Logging Libraries

Matchlock uses **Go's standard library `log/slog`** (introduced in Go 1.21) as its sole structured logging library. There are no third-party logging frameworks (no zap, zerolog, or logrus in direct dependencies; logrus appears only as a transitive dependency from `docker/cli`).

The `go.mod` declares `go 1.25.5`, so `log/slog` is fully available.

### 1.2 Logging Patterns

The codebase has **two distinct output systems**:

#### Pattern 1: `slog` for operational diagnostics (structured, human-oriented)

Used in the policy engine and network packages. Loggers are created via dependency injection with component/plugin scoping:

```go
// Engine creates a scoped logger
logger.With("component", "policy")

// Plugins get further-scoped loggers
e.logger.With("plugin", "host_filter")

// HTTPInterceptor gets its own scope
logger.With("component", "net")
```

Common field patterns:
- `"component"` -- identifies the subsystem (e.g., "policy", "net")
- `"plugin"` -- identifies the policy plugin (e.g., "host_filter", "secret_injector")
- `"host"` -- the target hostname
- `"method"`, `"path"`, `"status"`, `"duration_ms"`, `"bytes"` -- HTTP request fields
- `"error"` -- error details
- `"name"` -- secret name
- `"model"`, `"target"`, `"backend"` -- model routing fields

#### Pattern 2: `api.Event` channel for machine-readable events

A typed event struct emitted via a buffered channel (`chan api.Event`, capacity 100). Events are fire-and-forget (non-blocking `select` with `default` discard):

```go
type Event struct {
    Type      string        `json:"type"`
    Timestamp int64         `json:"timestamp"`
    Network   *NetworkEvent `json:"network,omitempty"`
    File      *FileEvent    `json:"file,omitempty"`
    Exec      *ExecEvent    `json:"exec,omitempty"`
}
```

Currently, the Event channel is exposed via `sandbox.Events()` for SDK consumers to read, but there is no built-in file sink or persistent storage.

#### Pattern 3: `fmt.Fprintf(os.Stderr, ...)` for CLI user messages

The CLI commands (cmd/matchlock/) use raw `fmt` printing to stderr/stdout for user-facing messages. These are not structured logs -- they are UI output (progress messages, confirmation, errors).

#### Pattern 4: `fmt.Println` / `fmt.Fprintf(os.Stderr, ...)` in guest-runtime binaries

Guest-runtime code (internal/guestruntime/) uses raw `fmt` since it runs inside the VM where `slog` infrastructure is not available.

### 1.3 Complete Logging Inventory

#### `slog` calls (structured logging)

| File | Line(s) | Level | Message/Context |
|------|---------|-------|-----------------|
| `pkg/policy/engine.go` | 50 | Debug | "plugin registered from flat config" (host_filter) |
| `pkg/policy/engine.go` | 58 | Debug | "plugin registered from flat config" (secret_injector) |
| `pkg/policy/engine.go` | 75 | Debug | "plugin registered from flat config" (local_model_router) |
| `pkg/policy/engine.go` | 87 | Warn | "duplicate plugin type in flat fields and plugins array" |
| `pkg/policy/engine.go` | 93 | Warn | "unknown plugin type, skipping" |
| `pkg/policy/engine.go` | 100 | Warn | "plugin creation failed, skipping" |
| `pkg/policy/engine.go` | 106 | Debug | "plugin registered from config array" |
| `pkg/policy/engine.go` | 113 | Info | "engine ready" (gate/router/request/response counts) |
| `pkg/policy/engine.go` | 178 | Debug | "gate allowed" |
| `pkg/policy/engine.go` | 181 | Warn | "gate blocked" |
| `pkg/policy/engine.go` | 192 | Warn | "route error" |
| `pkg/policy/engine.go` | 196 | Info | "local model redirect" (formatted string) |
| `pkg/policy/engine.go` | 204 | Debug | "route passthrough" |
| `pkg/policy/engine.go` | 218 | Warn | "request transform failed" |
| `pkg/policy/engine.go` | 232 | Warn | "response transform failed" |
| `pkg/policy/host_filter.go` | 66 | Debug | "private IP allowed via exception" |
| `pkg/policy/host_filter.go` | 76 | Debug | "matched allowlist pattern" |
| `pkg/policy/local_model_router.go` | 95 | Debug | "model not in route table, passing through" |
| `pkg/policy/local_model_router.go` | 103 | Debug | "model matched, rewriting request" |
| `pkg/policy/secret_injector.go` | 80 | Debug | "secret leak detected" |
| `pkg/policy/secret_injector.go` | 83 | Debug | "secret skipped for host" |
| `pkg/policy/secret_injector.go` | 87 | Debug | "secret injected" |
| `pkg/net/http.go` | 267 | Info | "local model redirect complete" (formatted string) |
| `pkg/net/http.go` | 274 | Info | "request complete" (method, host, path, status, duration, bytes) |
| `pkg/sandbox/paths.go` | 23 | Warn | "failed to resolve kernel path" |
| `pkg/sandbox/paths.go` | 107 | Warn | "kernel has wrong architecture, skipping" |

#### `api.Event` emissions (channel events)

| File | Line(s) | Event Type | Description |
|------|---------|------------|-------------|
| `pkg/net/http.go` | 295-330 | "network" | `emitEvent` -- successful HTTP/HTTPS request completion |
| `pkg/net/http.go` | 332-356 | "network" | `emitBlockedEvent` -- request blocked by policy |
| `pkg/net/proxy.go` | 188-203 | "network" | `emitBlockedEvent` -- passthrough blocked (Linux proxy) |
| `pkg/net/stack_darwin.go` | 459-473 | "network" | `emitBlockedEvent` -- connection blocked (macOS gVisor stack) |

#### `fmt` CLI output (not structured logs, UI only)

| File | Lines | Purpose |
|------|-------|---------|
| `cmd/matchlock/cmd_run.go` | 227, 385, 390-392, 432, 489, 500, 523, 535 | Run command user output |
| `cmd/matchlock/cmd_build.go` | 114, 149, 209, 262, 302, 361, 375-377 | Build progress output |
| `cmd/matchlock/cmd_list.go` | 38, 49 | Table-formatted VM listing |
| `cmd/matchlock/cmd_pull.go` | 44, 54, 57-58 | Pull progress output |
| `cmd/matchlock/cmd_gc.go` | 44-68 | GC reconciliation output |
| `cmd/matchlock/cmd_prune.go` | 25-27 | Prune result output |
| `cmd/matchlock/cmd_inspect.go` | 59 | JSON inspect output |
| `cmd/matchlock/cmd_get.go` | 31 | Get field output |
| `cmd/matchlock/cmd_rm.go` | 36, 38 | Remove confirmation |
| `cmd/matchlock/cmd_kill.go` | 35, 37, 51 | Kill confirmation |
| `cmd/matchlock/cmd_version.go` | 15 | Version output |
| `cmd/matchlock/cmd_port_forward.go` | 114, 137 | Port forward status |
| `cmd/matchlock/image.go` | 74, 81, 90, 106, 112, 124, 130-132, 141 | Image management output |
| `cmd/matchlock/setup_linux.go` | 86-367 | Linux setup wizard output |
| `cmd/matchlock/main.go` | 37 | Fatal error output |

#### Guest-runtime `fmt` output (runs inside VM)

| File | Lines | Purpose |
|------|-------|---------|
| `internal/guestruntime/agent/main.go` | 100, 122, 127, 142, 147, 152 | Agent startup messages |
| `internal/guestruntime/agent/sandbox_proc.go` | 175, 186, 208, 215, 227, 231, 235, 239, 259 | Sandbox process errors |
| `internal/guestruntime/fused/main.go` | 760, 763, 778, 782, 802, 806, 814 | FUSE daemon startup messages |
| `cmd/guest-init/main.go` | 146, 151 | Guest init fatal/warning |

### 1.4 Logger Injection Pattern

The codebase follows a consistent dependency-injection pattern for loggers:

1. Components accept `*slog.Logger` as a constructor parameter
2. If nil, they fall back to `slog.Default()`
3. Components add scoping with `.With("component", "...")` and `.With("plugin", "...")`
4. The logger propagates down: Engine -> Plugin, ProxyConfig -> HTTPInterceptor

Currently, all callers pass `nil` for the logger parameter:

```go
// pkg/sandbox/sandbox_darwin.go:281
policyEngine := policy.NewEngine(config.Network, nil)

// pkg/sandbox/sandbox_darwin.go:305
Logger: nil,

// pkg/sandbox/sandbox_linux.go:316
Logger: nil,
```

This means all slog output goes to the default handler (text format on stderr).

---

## 2. Proposed Common Logging Structure

### 2.1 Event Schema

Every structured event conforms to this JSON shape:

```json
{
  "ts": "2026-02-23T14:30:00.123Z",
  "run_id": "session-9f8e7d6c",
  "agent_system": "openclaw",
  "event_type": "llm_request",
  "summary": "POST api.anthropic.com/v1/messages (claude-sonnet-4-20250514)",
  "plugin": "secret_injector",
  "tags": ["tls", "mitm"],
  "data": {
    "method": "POST",
    "host": "api.anthropic.com",
    "path": "/v1/messages",
    "model": "claude-sonnet-4-20250514"
  }
}
```

### 2.2 Required Fields

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string (RFC 3339 with ms) | Timestamp of the event |
| `run_id` | string | Caller-supplied session/run identifier. Defaults to sandbox VM ID if not provided. |
| `agent_system` | string | Set at sandbox startup via static metadata (e.g., `"openclaw"`, `"aider"`). Matchlock does not validate values. |
| `event_type` | string | Categorizes the event (see v0 types below) |
| `summary` | string | Human-readable one-line summary |

### 2.3 Optional Fields

| Field | Type | Description |
|-------|------|-------------|
| `plugin` | string | Which policy plugin emitted the event (e.g., `"secret_injector"`, `"host_filter"`) |
| `tags` | []string | Arbitrary tags for filtering (e.g., `["tls", "local-model"]`) |
| `data` | object | Small structured payload, schema depends on `event_type` |

### 2.4 v0 Event Types (TLS/L7 Lane)

#### `llm_request`

Emitted when an LLM API request is intercepted and forwarded upstream (or locally).

```go
type LLMRequestData struct {
    Method    string `json:"method"`
    Host      string `json:"host"`
    Path      string `json:"path"`
    Model     string `json:"model,omitempty"`
    Routed    bool   `json:"routed"`
    RoutedTo  string `json:"routed_to,omitempty"`
}
```

Where it would be emitted: `pkg/net/http.go` in `HandleHTTPS` after request processing, before forwarding upstream. The model can be extracted from the JSON body (already parsed by `local_model_router`).

#### `llm_response`

Emitted when an LLM API response is received from upstream and forwarded to the guest.

```go
type LLMResponseData struct {
    Method     string `json:"method"`
    Host       string `json:"host"`
    Path       string `json:"path"`
    StatusCode int    `json:"status_code"`
    DurationMS int64  `json:"duration_ms"`
    BodyBytes  int64  `json:"body_bytes"`
    Model      string `json:"model,omitempty"`
}
```

Where it would be emitted: `pkg/net/http.go` in `HandleHTTPS` after response is buffered and before writing to guest. This corresponds to the existing `i.logger.Info("request complete", ...)` call on line 274.

#### `budget_action`

Emitted when a budget-related decision is made (e.g., token counting, cost tracking).

```go
type BudgetActionData struct {
    Action     string  `json:"action"`
    TokensUsed int64   `json:"tokens_used,omitempty"`
    CostUSD    float64 `json:"cost_usd,omitempty"`
    Remaining  float64 `json:"remaining,omitempty"`
}
```

**Note**: There is currently no budget/cost tracking in matchlock. This event type would need a new budget plugin or external integration. This is a placeholder for v0 that would be implemented as a new policy plugin.

#### `key_injection`

Emitted when a secret/API key is injected into an outbound request.

```go
type KeyInjectionData struct {
    SecretName string `json:"secret_name"`
    Host       string `json:"host"`
    Action     string `json:"action"` // "injected", "skipped", "leak_blocked"
}
```

Where it would be emitted: `pkg/policy/secret_injector.go` in `TransformRequest`, replacing the current `p.logger.Debug("secret injected/skipped/leak detected", ...)` calls.

---

## 3. Go Implementation Proposal

### 3.1 Event Type Definition

```go
// pkg/logging/event.go
package logging

import (
    "encoding/json"
    "time"
)

// Event is the canonical structured event for the policy engine logging standard.
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

// Event type constants
const (
    EventLLMRequest   = "llm_request"
    EventLLMResponse  = "llm_response"
    EventBudgetAction = "budget_action"
    EventKeyInjection = "key_injection"
)
```

### 3.2 JSON-L Writer (File Sink)

```go
// pkg/logging/jsonl_writer.go
package logging

import (
    "encoding/json"
    "io"
    "os"
    "sync"
)

// JSONLWriter writes structured events as JSON-L to a file.
// It is safe for concurrent use.
type JSONLWriter struct {
    mu   sync.Mutex
    file *os.File
    enc  *json.Encoder
}

// NewJSONLWriter creates a new JSON-L writer that appends to the given file path.
// The file is created if it does not exist.
func NewJSONLWriter(path string) (*JSONLWriter, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
    if err != nil {
        return nil, err
    }
    return &JSONLWriter{
        file: f,
        enc:  json.NewEncoder(f),
    }, nil
}

// Write serializes the event as a single JSON line and writes it to the file.
func (w *JSONLWriter) Write(event *Event) error {
    w.mu.Lock()
    defer w.mu.Unlock()
    return w.enc.Encode(event)
}

// Close syncs and closes the underlying file.
func (w *JSONLWriter) Close() error {
    w.mu.Lock()
    defer w.mu.Unlock()
    _ = w.file.Sync()
    return w.file.Close()
}
```

### 3.3 Event Emitter (Bridge between policy engine and JSON-L writer)

```go
// pkg/logging/emitter.go
package logging

import (
    "encoding/json"
    "fmt"
    "time"
)

// EmitterConfig holds the static metadata configured at sandbox startup.
// All fields are stamped onto every event automatically.
type EmitterConfig struct {
    RunID       string            // Caller-supplied; defaults to sandbox VM ID if empty
    AgentSystem string            // Set by consumer at startup (e.g., "openclaw", "aider")
    ExtraFields map[string]string // Future-proof: additional static key-value pairs
}

// Emitter provides convenience methods for emitting typed events.
// It holds static metadata configured at sandbox startup and one or more sinks.
type Emitter struct {
    config EmitterConfig
    sinks  []Sink
}

// Sink is the interface for event consumers.
type Sink interface {
    Write(event *Event) error
    Close() error
}

// NewEmitter creates an emitter with the given configuration.
// RunID should be set by the caller; if empty, the caller should default it
// to the sandbox VM ID before passing the config.
func NewEmitter(cfg EmitterConfig, sinks ...Sink) *Emitter {
    return &Emitter{
        config: cfg,
        sinks:  sinks,
    }
}

// Emit sends a fully formed event to all sinks.
func (e *Emitter) Emit(eventType, summary string, plugin string, tags []string, data interface{}) error {
    var rawData json.RawMessage
    if data != nil {
        b, err := json.Marshal(data)
        if err != nil {
            return fmt.Errorf("marshal event data: %w", err)
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

// Close closes all sinks.
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

### 3.4 Concrete Event Emission Examples

#### llm_request (in `pkg/net/http.go`)

```go
// In HandleHTTPS, after RouteRequest but before forwarding upstream:
if emitter != nil {
    data := &LLMRequestData{
        Method: req.Method,
        Host:   serverName,
        Path:   req.URL.Path,
        Routed: routeDirective != nil,
    }
    if routeDirective != nil {
        data.RoutedTo = fmt.Sprintf("%s:%d", routeDirective.Host, routeDirective.Port)
    }
    summary := fmt.Sprintf("%s %s%s", req.Method, serverName, req.URL.Path)
    _ = emitter.Emit(EventLLMRequest, summary, "", []string{"tls"}, data)
}
```

#### llm_response (in `pkg/net/http.go`)

```go
// After response is buffered, before writing to guest:
if emitter != nil {
    data := &LLMResponseData{
        Method:     req.Method,
        Host:       serverName,
        Path:       req.URL.Path,
        StatusCode: modifiedResp.StatusCode,
        DurationMS: duration.Milliseconds(),
        BodyBytes:  int64(len(body)),
    }
    summary := fmt.Sprintf("%s %s%s -> %d (%dms)",
        req.Method, serverName, req.URL.Path,
        modifiedResp.StatusCode, duration.Milliseconds())
    _ = emitter.Emit(EventLLMResponse, summary, "", []string{"tls"}, data)
}
```

#### key_injection (in `pkg/policy/secret_injector.go`)

```go
// Replace p.logger.Debug("secret injected", ...) with:
if emitter != nil {
    data := &KeyInjectionData{
        SecretName: name,
        Host:       host,
        Action:     "injected",
    }
    summary := fmt.Sprintf("secret %q injected for %s", name, host)
    _ = emitter.Emit(EventKeyInjection, summary, "secret_injector", nil, data)
}
```

---

## 4. JSON-L File Format

### 4.1 Format

Each line in the `.jsonl` file is a single JSON object, newline-terminated. No trailing comma, no array wrapper.

Example file content:

```jsonl
{"ts":"2026-02-23T14:30:00.123Z","run_id":"session-9f8e7d6c","agent_system":"openclaw","event_type":"llm_request","summary":"POST api.anthropic.com/v1/messages","data":{"method":"POST","host":"api.anthropic.com","path":"/v1/messages","model":"claude-sonnet-4-20250514","routed":false}}
{"ts":"2026-02-23T14:30:01.456Z","run_id":"session-9f8e7d6c","agent_system":"openclaw","event_type":"key_injection","summary":"secret \"ANTHROPIC_API_KEY\" injected for api.anthropic.com","plugin":"secret_injector","data":{"secret_name":"ANTHROPIC_API_KEY","host":"api.anthropic.com","action":"injected"}}
{"ts":"2026-02-23T14:30:02.789Z","run_id":"session-9f8e7d6c","agent_system":"openclaw","event_type":"llm_response","summary":"POST api.anthropic.com/v1/messages -> 200 (1234ms)","data":{"method":"POST","host":"api.anthropic.com","path":"/v1/messages","status_code":200,"duration_ms":1234,"body_bytes":8192}}
```

### 4.2 File Location and Lifecycle

Default path: `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl`

Override: `--event-log <path>` CLI flag or `config.Logging.EventLogPath`

1. The `logs/` directory is created on first use (alongside the per-VM state directories)
2. The file is created at sandbox startup when event logging is configured
3. Events are appended one per line during the sandbox lifetime
4. The file is synced and closed when the sandbox shuts down
5. The file **persists after sandbox cleanup** -- it is not deleted by `sandbox.Close()` because it lives outside the per-VM state directory

### 4.3 Parsing

JSON-L files can be parsed line-by-line with any JSON parser:

```bash
# Count events by type
jq -r '.event_type' events.jsonl | sort | uniq -c

# Filter LLM requests
jq 'select(.event_type == "llm_request")' events.jsonl

# Total response time
jq 'select(.event_type == "llm_response") | .data.duration_ms' events.jsonl | paste -sd+ | bc
```

---

## 5. Integration Points

### 5.1 Where the Emitter Gets Wired In

The emitter needs to be created during sandbox construction and passed through to the components that emit events. The current wiring path is:

```
cmd_run.go -> sandbox.New() -> policy.NewEngine() -> plugins
                             -> sandboxnet.NewNetworkStack() -> HTTPInterceptor
```

The `Emitter` would be:
1. Constructed in `sandbox.New()` using an `EmitterConfig` populated from the sandbox config:
   - `RunID` = caller-supplied via config, defaults to sandbox VM ID
   - `AgentSystem` = caller-supplied via config (e.g., `"openclaw"`)
2. Passed to `policy.NewEngine()` (which distributes it to plugins)
3. Passed to `HTTPInterceptor` via `sandboxnet.Config`
4. The JSON-L `Sink` is created if an event log path is configured

### 5.2 Relationship to Existing `api.Event` Channel (Decided: Parallel)

The three output systems serve different audiences and remain independent:

| System | Audience | Persistence | Format |
|--------|----------|-------------|--------|
| `slog` | Developer on stderr | Ephemeral | Text (default handler) |
| `api.Event` channel | Go SDK consumers | In-memory channel | Typed Go structs |
| JSON-L event log | Post-run analysis, dashboards | Persistent file | JSON-L |

The new JSON-L emitter operates in parallel to the `api.Event` channel. They share no data flow. The emitter is called directly from the same code paths that currently call `slog` and `emitEvent`, but the JSON-L events carry richer metadata (run_id, agent_system).

---

## 6. Migration Path

### Phase 1: Add the logging package (non-breaking)

1. Create `pkg/logging/` with `Event`, `JSONLWriter`, `Emitter`, and `Sink` types
2. No changes to existing code
3. Unit tests for serialization and file writing

### Phase 2: Wire the emitter into the sandbox (opt-in)

1. Add a `LoggingConfig` struct to `api.Config`:
   ```go
   type LoggingConfig struct {
       EventLogPath string `json:"event_log_path,omitempty"` // Override path; empty = default logs dir
       Enabled      bool   `json:"enabled,omitempty"`        // Enable JSON-L event logging
       RunID        string `json:"run_id,omitempty"`         // Caller-supplied session ID
       AgentSystem  string `json:"agent_system,omitempty"`   // e.g., "openclaw", "aider"
   }
   ```
2. In `sandbox.New()`, if logging is enabled:
   - Determine path: if `EventLogPath` is set, use it; otherwise default to `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl`
   - Create the `logs/` directory if it does not exist
   - Create `JSONLWriter` + `Emitter` with `EmitterConfig{RunID: runID, AgentSystem: config.Logging.AgentSystem}` (where `runID` defaults to `config.GetID()` if not provided)
3. Pass the emitter through to `policy.NewEngine()` and `HTTPInterceptor`
4. Add CLI flags: `--event-log <path>` (implies enabled), `--run-id <id>`, `--agent-system <name>`

### Phase 3: Emit v0 events alongside existing slog calls

1. In `HTTPInterceptor.HandleHTTPS()`, emit `llm_request` and `llm_response` events
2. In `secretInjectorPlugin.TransformRequest()`, emit `key_injection` events
3. Keep existing `slog` calls unchanged -- the new events are additive
4. The `budget_action` event type remains a stub until a budget plugin is built

### Phase 4: v1 event types and tooling

1. Gather feedback on the v0 event schema from real OpenClaw/aider usage
2. Implement `gate_decision` and `route_decision` event types (see section 9 -- v1 Roadmap)
3. Evaluate whether to converge `slog` and the structured event system
4. Add consumer tooling (event viewer, cost dashboard, `matchlock logs` subcommand)

---

## 7. Design Considerations

### 7.1 Performance

- JSON-L encoding is fast (one `json.Encoder.Encode` call per event)
- Mutex contention is minimal (events are infrequent relative to compute)
- File I/O is append-only, which is efficient on modern filesystems
- For SSE streaming responses, events are emitted per-response, not per-chunk

### 7.2 Thread Safety

The `JSONLWriter` uses a `sync.Mutex` to serialize writes. Since events are emitted from multiple goroutines (HTTP handler, plugin chain), this is necessary. The lock scope is minimal (one JSON encode + file write).

### 7.3 Error Handling

Event emission errors should not crash the sandbox. The emitter should use best-effort semantics:
- Log write errors via `slog.Warn`
- Never block the request/response path
- Consider a `Sink` wrapper that drops events on error

### 7.4 Testing

- Unit tests for `Event` serialization
- Unit tests for `JSONLWriter` (file creation, concurrent writes, close)
- Integration test: run a sandbox with event logging enabled, verify JSON-L output
- Golden file tests for event format stability

---

## 8. Files Affected by This Change

| File | Change Type | Description |
|------|-------------|-------------|
| `pkg/logging/event.go` | **New** | Event types, constants |
| `pkg/logging/jsonl_writer.go` | **New** | JSON-L file sink |
| `pkg/logging/emitter.go` | **New** | Event emitter with static metadata |
| `pkg/logging/event_test.go` | **New** | Unit tests |
| `pkg/logging/jsonl_writer_test.go` | **New** | Unit tests |
| `pkg/api/config.go` | Modified | Add `LoggingConfig` struct with `EventLogPath`, `RunID`, `AgentSystem` |
| `pkg/policy/engine.go` | Modified | Accept and distribute `Emitter` |
| `pkg/policy/secret_injector.go` | Modified | Emit `key_injection` events |
| `pkg/net/http.go` | Modified | Emit `llm_request` and `llm_response` events |
| `pkg/sandbox/sandbox_darwin.go` | Modified | Create emitter, wire through |
| `pkg/sandbox/sandbox_linux.go` | Modified | Create emitter, wire through |
| `cmd/matchlock/cmd_run.go` | Modified | Add `--event-log`, `--run-id`, `--agent-system` flags |

---

## 9. v1 Roadmap: Future Event Types

The following event types are explicitly planned for v1. They are not included in v0 to keep initial scope tight, but the schema and infrastructure are designed to accommodate them without breaking changes.

### `gate_decision`

Emitted when a policy gate plugin makes an allow/block decision on a host.

**Current slog calls that would become `gate_decision` events:**
- `pkg/policy/engine.go:178` -- `slog.Debug("gate allowed", "plugin", g.Name(), "host", host)`
- `pkg/policy/engine.go:181` -- `slog.Warn("gate blocked", "plugin", g.Name(), "host", host, "reason", reason)`
- `pkg/policy/host_filter.go:66` -- `slog.Debug("private IP allowed via exception", "host", host)`
- `pkg/policy/host_filter.go:76` -- `slog.Debug("matched allowlist pattern", "host", host, "pattern", pattern)`

**Proposed data schema:**

```go
type GateDecisionData struct {
    Host    string `json:"host"`
    Allowed bool   `json:"allowed"`
    Reason  string `json:"reason,omitempty"` // Why blocked, e.g., "host not in allowlist"
    Pattern string `json:"pattern,omitempty"` // Matching pattern, if allowed
}
```

**Expected volume:** One per outbound connection attempt. Low frequency.

### `route_decision`

Emitted when a route plugin makes a routing decision (redirect to local backend vs. passthrough to origin).

**Current slog calls that would become `route_decision` events:**
- `pkg/policy/engine.go:196` -- `slog.Info("local model redirect: ...")` (route applied)
- `pkg/policy/engine.go:204` -- `slog.Debug("route passthrough", "host", host, "method", req.Method, "path", req.URL.Path)` (no route matched)
- `pkg/policy/engine.go:192` -- `slog.Warn("route error", "plugin", r.Name(), "host", host, "error", err)` (route failed)
- `pkg/policy/local_model_router.go:95` -- `slog.Debug("model not in route table, passing through", ...)`
- `pkg/policy/local_model_router.go:103` -- `slog.Debug("model matched, rewriting request", ...)`

**Proposed data schema:**

```go
type RouteDecisionData struct {
    Host        string `json:"host"`
    Method      string `json:"method"`
    Path        string `json:"path"`
    Action      string `json:"action"` // "redirected", "passthrough", "error"
    Model       string `json:"model,omitempty"`
    TargetHost  string `json:"target_host,omitempty"`
    TargetPort  int    `json:"target_port,omitempty"`
    TargetModel string `json:"target_model,omitempty"`
    Error       string `json:"error,omitempty"`
}
```

**Expected volume:** One per HTTP/HTTPS request through the interceptor. Moderate frequency during active LLM use.

### Other candidates for future versions

- `sandbox_lifecycle` -- sandbox start, stop, timeout, crash events
- `vfs_operation` -- VFS hook triggers (file create blocked, write mutated, etc.)
- `plugin_error` -- plugin creation failures, transform errors (currently `slog.Warn` in `pkg/policy/engine.go:87-101`)
- `dns_query` -- DNS queries resolved by the userspace DNS handler (`pkg/net/stack_darwin.go:421-457`)
