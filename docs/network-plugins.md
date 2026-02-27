# Network Plugins

The network policy engine uses a plugin architecture. Traffic policies (host filtering, secret injection, request routing) are implemented as plugins that the engine orchestrates in a fixed pipeline.

## How It Works

When a request flows through the proxy, the engine runs plugins in four phases:

1. **Gate** (`IsHostAllowed`) -- should this connection proceed?
2. **Route** (`RouteRequest`) -- should this request go to a different backend?
3. **Request** (`OnRequest`) -- transform the outbound request (e.g., inject secrets)
4. **Response** (`OnResponse`) -- transform the inbound response

A single plugin can participate in multiple phases. For example, `local_model_router` implements both Route and Request.

## Built-in Plugins

### `host_filter`

Gates connections based on a host allowlist and private IP blocking.

- **Interfaces:** `GatePlugin`
- **File:** `pkg/policy/host_filter.go`
- **Flat config fields:** `allowed_hosts`, `block_private_ips`, `allowed_private_hosts`

```json
{
  "type": "host_filter",
  "config": {
    "allowed_hosts": ["api.openai.com", "*.anthropic.com"],
    "block_private_ips": true,
    "allowed_private_hosts": ["192.168.64.*"]
  }
}
```

### `secret_injector`

Replaces placeholder tokens with real secret values in HTTP headers and URLs. Blocks requests that would leak secrets to unauthorized hosts.

- **Interfaces:** `RequestPlugin`, `PlaceholderProvider`
- **File:** `pkg/policy/secret_injector.go`
- **Flat config field:** `secrets`

```json
{
  "type": "secret_injector",
  "config": {
    "secrets": {
      "API_KEY": {
        "value": "sk-real-secret",
        "hosts": ["api.openai.com"]
      }
    }
  }
}
```

### `local_model_router`

Intercepts LLM API requests and redirects them to a local inference backend (e.g., Ollama). Rewrites request bodies to match the target model format.

- **Interfaces:** `RoutePlugin`, `RequestPlugin`
- **File:** `pkg/policy/local_model_router.go`
- **Flat config field:** `local_model_routing`

```json
{
  "type": "local_model_router",
  "config": {
    "routes": [
      {
        "source_host": "openrouter.ai",
        "backend_host": "127.0.0.1",
        "backend_port": 11434,
        "models": {
          "meta-llama/llama-3.1-8b-instruct": { "target": "llama3.1:8b" }
        }
      }
    ]
  }
}
```

### `usage_logger`

Intercepts OpenRouter API responses, extracts token counts and cost, and writes them to a JSONL log file. Maintains a running cost total in memory for budget enforcement.

- **Interfaces:** `ResponsePlugin`, `CostProvider`
- **File:** `pkg/policy/usage_logger.go`
- **Flat config field:** `usage_log_path`

Only responses to `POST /api/v1/chat/completions` or `POST /v1/chat/completions` on OpenRouter hosts are logged. Restores the running total from an existing log file on startup.

### `budget_gate`

Blocks all outbound requests when cumulative API costs exceed a configured USD limit. Returns HTTP 429 with an OpenAI-format JSON error body.

- **Interfaces:** `GatePlugin`
- **File:** `pkg/policy/budget_gate.go`
- **Flat config field:** `budget_limit_usd`
- **Depends on:** `usage_logger` (via `CostProvider` interface)
- **Note:** Not registered in the plugin registry. Only configurable via the `budget_limit_usd` flat field, not via the explicit `plugins` array.

Requires `usage_logger` to be active. See [Budget Enforcement](usage/budget-enforcement.md) for full details.

## Configuration

Plugins can be configured two ways. Both produce the same behavior.

### Flat fields (simple)

The familiar top-level fields compile into plugins automatically:

```json
{
  "network": {
    "allowed_hosts": ["api.openai.com"],
    "secrets": {
      "API_KEY": { "value": "sk-...", "hosts": ["api.openai.com"] }
    },
    "local_model_routing": [
      { "source_host": "openrouter.ai", "backend_host": "127.0.0.1", "backend_port": 11434, "models": { "meta-llama/llama-3.1-8b-instruct": { "target": "llama3.1:8b" } } }
    ]
  }
}
```

### Explicit plugins (advanced)

The `network.plugins` array gives full control:

```json
{
  "network": {
    "plugins": [
      {
        "type": "host_filter",
        "config": { "allowed_hosts": ["api.openai.com"] }
      },
      {
        "type": "secret_injector",
        "config": { "secrets": { "API_KEY": { "value": "sk-...", "hosts": ["api.openai.com"] } } }
      },
      {
        "type": "local_model_router",
        "enabled": false,
        "config": { "routes": [] }
      }
    ]
  }
}
```

### Disabling a plugin

Set `"enabled": false` to keep config around without activating:

```json
{ "type": "local_model_router", "enabled": false, "config": { "routes": [] } }
```

### Mixing flat fields and explicit plugins

Both can coexist. If the same plugin type appears in both, both instances run (merge semantics). The engine logs a warning.

## CLI Flags

The CLI flags compile into the same plugins as the JSON config above. Each flag maps to a specific plugin and config field:

| CLI Flag | Plugin | Config Field |
|----------|--------|-------------|
| `--allow-host` | `host_filter` | `allowed_hosts` |
| `--allow-private-host` | `host_filter` | `allowed_private_hosts` |
| `--secret` | `secret_injector` | `secrets` |
| `--local-model-backend` | `local_model_router` | `routes[].backend_host`, `routes[].backend_port` |
| `--local-model-route` | `local_model_router` | `routes[].source_host`, `routes[].models` |
| `--usage-log-path` | `usage_logger` | `log_path` |
| `--budget-limit-usd` | `budget_gate` | `limit_usd` |

### Examples

**Host filtering** -- allow two hosts and a specific private IP:

```bash
matchlock run --image alpine:latest \
  --allow-host "api.openai.com" \
  --allow-host "*.anthropic.com" \
  --allow-private-host "192.168.1.100"
```

Equivalent JSON:

```json
{
  "type": "host_filter",
  "config": {
    "allowed_hosts": ["api.openai.com", "*.anthropic.com"],
    "block_private_ips": true,
    "allowed_private_hosts": ["192.168.1.100"]
  }
}
```

**Secret injection** -- inject an API key for a specific host:

```bash
export API_KEY=sk-real-secret
matchlock run --image alpine:latest \
  --secret "API_KEY@api.openai.com"
```

Equivalent JSON:

```json
{
  "type": "secret_injector",
  "config": {
    "secrets": {
      "API_KEY": { "value": "sk-real-secret", "hosts": ["api.openai.com"] }
    }
  }
}
```

**Local model routing** -- redirect OpenRouter requests to a local Ollama backend:

```bash
matchlock run --image alpine:latest \
  --local-model-backend "127.0.0.1:11434" \
  --local-model-route "openrouter.ai/google/gemini-2.0-flash-001=llama3.1:8b"
```

Equivalent JSON:

```json
{
  "type": "local_model_router",
  "config": {
    "routes": [
      {
        "source_host": "openrouter.ai",
        "backend_host": "127.0.0.1",
        "backend_port": 11434,
        "models": {
          "google/gemini-2.0-flash-001": { "target": "llama3.1:8b" }
        }
      }
    ]
  }
}
```

The `--local-model-route` format is `SOURCE_HOST/SOURCE_MODEL=TARGET_MODEL[@HOST:PORT]`. The optional `@HOST:PORT` suffix overrides `--local-model-backend` for that specific route.

## SDK

```go
import "github.com/jingkaihe/matchlock/pkg/sdk"

sandbox := sdk.New("alpine:latest").
    AllowHost("api.openai.com").
    AddSecret("API_KEY", os.Getenv("API_KEY"), "api.openai.com").
    WithPlugin(sdk.PluginConfig{
        Type:   "local_model_router",
        Config: json.RawMessage(`{"routes": [...]}`),
    })
```

## Writing a New Plugin

### 1. Pick your interfaces

All plugins implement `Plugin` (just a `Name() string` method). Then implement one or more phase interfaces depending on what your plugin does:

```go
// pkg/policy/plugin.go -- these are the available interfaces:

type GatePlugin interface {
    Plugin
    Gate(host string) *GateVerdict
}

// GateVerdict carries the result of a gate evaluation.
// nil from Engine.IsHostAllowed = allowed; non-nil = blocked.
type GateVerdict struct {
    Allowed     bool
    Reason      string
    StatusCode  int    // 0 = default (403)
    ContentType string // "" = "text/plain"
    Body        string // "" = "Blocked by policy"
}

type RoutePlugin interface {
    Plugin
    Route(req *http.Request, host string) (*RouteDecision, error)
}

// RouteDecision wraps a routing directive with a reason for logging.
// Directive is nil for passthrough (use original destination).
type RouteDecision struct {
    Directive *RouteDirective
    Reason    string
}

type RouteDirective struct {
    Host   string // e.g., "127.0.0.1"
    Port   int    // e.g., 11434
    UseTLS bool
}

type RequestPlugin interface {
    Plugin
    TransformRequest(req *http.Request, host string) (*RequestDecision, error)
}

// RequestDecision wraps a transformed request with action/reason for logging.
type RequestDecision struct {
    Request *http.Request
    Action  string // "injected", "skipped", "leak_blocked", "no_op", "rewritten"
    Reason  string
}

type ResponsePlugin interface {
    Plugin
    TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error)
}

// ResponseDecision wraps a transformed response with action/reason for logging.
type ResponseDecision struct {
    Response *http.Response
    Action   string // "logged_usage", "no_op", "modified"
    Reason   string
}
```

If your plugin manages secrets or env vars, also implement `PlaceholderProvider`:

```go
type PlaceholderProvider interface {
    GetPlaceholders() map[string]string
}
```

### 2. Create the plugin file

Create `pkg/policy/your_plugin.go`. Follow the pattern of any existing plugin. You need:

- A config struct with JSON tags
- A private struct implementing your chosen interfaces (including a `logger *slog.Logger` field)
- Two constructors, each accepting `logger *slog.Logger` as the last parameter:
  - `NewYourPlugin(..., logger *slog.Logger)` -- typed constructor for flat-field compilation
  - `NewYourPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error)` -- factory for the registry

Example skeleton:

```go
package policy

import (
    "encoding/json"
    "log/slog"
    "net/http"
)

type YourPluginConfig struct {
    SomeSetting string `json:"some_setting"`
}

type yourPlugin struct {
    config YourPluginConfig
    logger *slog.Logger
}

func NewYourPlugin(setting string, logger *slog.Logger) *yourPlugin {
    return &yourPlugin{
        config: YourPluginConfig{SomeSetting: setting},
        logger: logger,
    }
}

func NewYourPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
    var cfg YourPluginConfig
    if err := json.Unmarshal(raw, &cfg); err != nil {
        return nil, err
    }
    return &yourPlugin{config: cfg, logger: logger}, nil
}

func (p *yourPlugin) Name() string { return "your_plugin" }

func (p *yourPlugin) TransformRequest(req *http.Request, host string) (*RequestDecision, error) {
    p.logger.Debug("processing request", "host", host, "setting", p.config.SomeSetting)
    // Your logic here
    return &RequestDecision{Request: req, Action: "no_op", Reason: "nothing to do"}, nil
}
```

### Logging

Every plugin receives a pre-scoped `*slog.Logger` from the engine. The logger is automatically tagged with `component=policy` and `plugin=<your_plugin_name>`, so your plugin does not need to add those keys.

**Convention:** Plugins log at `Debug` level only. The engine handles `Info` and `Warn` level logs for phase outcomes (gate blocked, route matched, transform failed). This separation prevents duplicate log lines.

```go
// Good: Debug level, descriptive message, structured fields
p.logger.Debug("matched custom rule", "host", host, "rule", ruleName)

// Bad: Info level in a plugin (engine already logs outcomes at Info)
p.logger.Info("request allowed", "host", host)
```

The logger is guaranteed to be non-nil. The engine creates it before calling your factory or constructor, so you do not need a nil guard.

### 3. Register the factory

In `pkg/policy/registry.go`, add your plugin to the `init()` function. Your factory must match the `PluginFactory` signature: `func(config json.RawMessage, logger *slog.Logger) (Plugin, error)`.

```go
func init() {
    Register("host_filter", NewHostFilterPluginFromConfig)
    Register("secret_injector", NewSecretInjectorPluginFromConfig)
    Register("local_model_router", NewLocalModelRouterPluginFromConfig)
    Register("your_plugin", NewYourPluginFromConfig)  // add this
}
```

That's it. The engine will automatically sort your plugin into the correct phase slices based on which interfaces it implements.

### 4. (Optional) Add flat config sugar

If you want users to configure your plugin via a top-level field (like `allowed_hosts` or `secrets`), add a field to `NetworkConfig` in `pkg/api/config.go` and compile it in `NewEngine` in `pkg/policy/engine.go`:

```go
// In NewEngine, after the existing flat-field compilation:
if config.YourField != "" {
    pluginLogger := e.logger.With("plugin", "your_plugin")
    e.addPlugin(NewYourPlugin(config.YourField, pluginLogger))
    flatTypes["your_plugin"] = true
    e.logger.Debug("plugin registered from flat config", "plugin", "your_plugin")
}
```

### 5. Write tests

Create `pkg/policy/your_plugin_test.go`. Test both constructors and the plugin behavior directly. The existing engine tests serve as integration tests -- if your plugin affects engine behavior through flat fields, those tests should cover it.

## File Map

```
pkg/policy/
  plugin.go              # Interfaces (Plugin, GatePlugin, GateVerdict, RoutePlugin, etc.)
  registry.go            # Factory registry (Register, LookupFactory)
  engine.go              # Orchestrator (compiles config -> plugins, runs pipeline)
  util.go                # Shared helpers (matchGlob, isPrivateIP, etc.)
  host_filter.go         # GatePlugin: host allowlist + private IP blocking
  secret_injector.go     # RequestPlugin + PlaceholderProvider: secret injection
  local_model_router.go  # RoutePlugin + RequestPlugin: LLM request routing
  usage_logger.go        # ResponsePlugin + CostProvider: API cost/token logging
  budget_gate.go         # GatePlugin: budget enforcement (HTTP 429)
```

## Log Output

The engine and plugins produce structured logs via `log/slog`. At default level (Info), you see operator-relevant events. Set Debug level for troubleshooting detail.

### Example: local model routing

When a request is redirected to a local backend:

```
INFO local model redirect: POST request to openrouter.ai/api/v1/chat/completions redirected to -> 172.20.10.3:11434 (local-backend)  plugin=local_model_router
INFO local model redirect complete: POST openrouter.ai/api/v1/chat/completions -> 200 172.20.10.3:11434 (345550ms, 37041 bytes)
```

With Debug enabled, you also see plugin-internal reasoning:

```
DEBUG model matched, rewriting request  plugin=local_model_router  model=meta-llama/llama-3.1-8b-instruct  target=llama3.1:8b  backend=172.20.10.3:11434
```

### Example: gate blocking

```
WARN gate blocked  plugin=host_filter  host=evil.com  reason=host not in allowlist
```

```
WARN budget gate blocking request  plugin=budget_gate  host=openrouter.ai  current_cost_usd=5.0100  limit_usd=5.00
WARN gate blocked  plugin=budget_gate  host=openrouter.ai  reason="budget exceeded: $5.0100 spent of $5.00 limit"
```

### Log levels

| Level | Who logs | What |
|-------|----------|------|
| Debug | Plugins | Internal reasoning (pattern matches, model lookups, secret decisions) |
| Info | Engine, HTTP interceptor | Route redirects, request completions, engine ready |
| Warn | Engine | Gate blocked, transform failed, unknown plugin type |

### Secret safety

Secret values and placeholder strings are **never** logged. Only secret names (e.g., `"API_KEY"`) appear in log output.

## Phase Semantics

| Phase | Multiple plugins | Behavior |
|-------|-----------------|----------|
| Gate | AND -- all gates must allow | If any gate denies, request is blocked. If no gates registered, all hosts allowed |
| Route | First non-nil directive wins | Remaining routers are skipped |
| Request | Chained -- output feeds into next | Error blocks the request |
| Response | Chained -- output feeds into next | Error drops the response |
