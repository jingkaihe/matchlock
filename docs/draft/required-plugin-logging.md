# Required Plugin Logging

# Open Questions

## Unresolved

(All questions resolved.)

## Resolved

- [x] Q1: Engine-level wrapper vs. plugin-level responsibility -- who emits the event?
  - **Decision:** Approach B (Structured Decision Return Types). Plugins return structured decision objects; the engine emits events using that metadata. Plugins never touch the emitter directly.

- [x] Q2: Should we change the plugin interfaces (breaking change) or add a parallel set of "logging-aware" interfaces?
  - **Decision:** Break the interfaces now. The plugin system is brand new with zero external consumers. This is the lowest-cost moment to get the interfaces right.
  - **Note:** The `feat/budget-enforcement` merge has already partially implemented this -- `GatePlugin.Gate()` now returns `*GateVerdict` instead of `(bool, string)`. The remaining interfaces (`RoutePlugin`, `RequestPlugin`, `ResponsePlugin`) still use bare return types.

- [x] Q3: What level of detail should the engine auto-capture vs. require from plugins?
  - **Decision:** Rich engine capture. The engine stamps common fields (plugin name, phase, host, method, path, duration). Plugins provide only domain-specific rationale (the "why").

---

## 1. Problem Statement

The current structured event logging system (JSON-L via `*logging.Emitter`) is optional and ad-hoc. Plugins receive a `*logging.Emitter` and choose whether to use it. This creates three problems:

1. **Silent plugins.** A plugin author can forget to emit events entirely (as `localModelRouterPlugin` currently does -- it accepts an `emitter` parameter in its factory but never stores or uses it). The new `usageLoggerPlugin` and `budgetGatePlugin` also do not emit structured events.
2. **Inconsistent detail.** Even plugins that do emit events (like `secretInjectorPlugin`) must repeat boilerplate patterns: nil-check the emitter, construct the data struct, choose an event type, format a summary.
3. **No enforcement.** There is no compile-time or runtime mechanism to ensure a plugin records its decision logic.

The goal is to make logging a **structural requirement** of the plugin system, so that every plugin's decision logic is recorded to the event log without relying on the plugin author to remember.

## 2. Current State Analysis (post budget-enforcement merge)

### 2.1 Plugin Interfaces

Defined in `pkg/policy/plugin.go`:

```go
type Plugin interface {
    Name() string
}

type GatePlugin interface {
    Plugin
    Gate(host string) *GateVerdict  // ALREADY returns structured type
}

type GateVerdict struct {
    Allowed     bool
    Reason      string
    StatusCode  int     // HTTP error fields (optional, budget_gate uses 429)
    ContentType string
    Body        string
}

type RoutePlugin interface {
    Plugin
    Route(req *http.Request, host string) (*RouteDirective, error)  // bare return
}

type RequestPlugin interface {
    Plugin
    TransformRequest(req *http.Request, host string) (*http.Request, error)  // bare return
}

type ResponsePlugin interface {
    Plugin
    TransformResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error)  // bare return
}
```

Key observation: `GatePlugin` already returns a structured type (`*GateVerdict`) thanks to the budget-enforcement work. The other three interfaces still return bare values with no place for decision metadata.

### 2.2 How Plugins Currently Handle Logging

**host_filter** (GatePlugin):
- Returns `*GateVerdict` with `Allowed` and `Reason` fields.
- The **engine** emits `gate_decision` events in `IsHostAllowed()` using the verdict data.
- The plugin itself does not touch the emitter. This is the target pattern.

**budget_gate** (GatePlugin):
- Returns `*GateVerdict` with `Allowed`, `Reason`, and HTTP error fields (429).
- Like host_filter, the engine emits `gate_decision` events using the verdict.
- Does not touch the emitter. Already follows the target pattern.

**secret_injector** (RequestPlugin):
- Stores `emitter *logging.Emitter` on the struct.
- Emits `key_injection` events with three actions: `"injected"`, `"skipped"`, `"leak_blocked"`.
- Every emission site has the pattern: `if p.emitter != nil { _ = p.emitter.Emit(...) }`.
- This is the **anti-pattern** we want to eliminate -- the plugin manually emits events.

**local_model_router** (RoutePlugin + RequestPlugin):
- Accepts `emitter` in its factory but **never stores it**.
- Zero structured event emissions. Routing decisions are only logged via `slog.Debug`.

**usage_logger** (ResponsePlugin):
- Does not accept or use the emitter at all.
- Writes its own JSONL log to a separate file (`usage.jsonl`). This is independent of the event log system and should remain so.
- No structured events emitted to the event log.

### 2.3 How the Engine Currently Emits Events

The engine emits `gate_decision` events in `IsHostAllowed()` after calling `g.Gate(host)`. It reads the `*GateVerdict` fields and constructs a `GateDecisionData` struct for the emitter. This is exactly the pattern we want to extend to the other three phases.

`RouteRequest()`, `OnRequest()`, and `OnResponse()` do **not** emit events at the engine level. Only the `secret_injector` plugin emits events during `OnRequest()` (manually, via its stored emitter reference).

### 2.4 The PluginFactory Signature

```go
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)
```

The `emitter` parameter exists but is used inconsistently. Once all event emission moves to the engine, this parameter can be removed.

## 3. Design Goals

1. **Every plugin phase call produces a structured event.** No plugin can execute without its decision being logged.
2. **Plugin authors provide domain-specific rationale.** The "why" behind a decision must come from the plugin, not generic metadata.
3. **Minimal boilerplate for plugin authors.** The framework should handle the repetitive parts (timestamps, plugin name, phase, common request fields).
4. **Compile-time enforcement where possible.** If a plugin does not satisfy the logging contract, it should fail to compile.
5. **Backward compatibility is not a constraint.** The plugin system is new and has no external consumers.

## 4. Chosen Approach: Structured Decision Return Types

### 4.1 What Already Exists (GatePlugin)

`GatePlugin` already follows Approach B. The `*GateVerdict` struct carries both the operational result (`Allowed`, `StatusCode`, etc.) and the decision rationale (`Reason`). The engine reads the verdict and emits a `gate_decision` event.

The only gap: `GateVerdict` has no `Detail map[string]any` field for arbitrary plugin-specific metadata (e.g., which allowlist pattern matched). This is acceptable for v0 -- the `Reason` string covers the current use cases. A `Detail` field can be added later without breaking the interface.

### 4.2 What Needs to Change (Route, Request, Response)

#### RoutePlugin

```go
// RouteDecision captures the routing plugin's full decision.
type RouteDecision struct {
    Directive *RouteDirective  // nil = passthrough (no redirect)
    Reason    string           // Human-readable: "matched model llama3.1:8b", "no matching route", etc.
}

type RoutePlugin interface {
    Plugin
    Route(req *http.Request, host string) (*RouteDecision, error)
}
```

The engine emits a `route_decision` event after each route plugin call:
- Summary: `"route passthrough for openrouter.ai by local_model_router"` or `"route redirected openrouter.ai -> 192.168.1.10:11434 by local_model_router"`
- Data: `{ "host": "...", "action": "passthrough"|"redirected", "routed_to": "host:port", "reason": "..." }`

#### RequestPlugin

```go
// RequestDecision captures what a request transform did.
type RequestDecision struct {
    Request *http.Request  // The (possibly modified) request
    Action  string         // What happened: "injected", "skipped", "leak_blocked", "no_op"
    Reason  string         // Why: "secret OPENROUTER_API_KEY injected for openrouter.ai"
}

type RequestPlugin interface {
    Plugin
    TransformRequest(req *http.Request, host string) (*RequestDecision, error)
}
```

The engine emits a `request_transform` event after each request plugin call:
- Summary: `"secret_injector: injected secret for openrouter.ai"` or `"secret_injector: leak blocked for suspicious.com"`
- Data: `{ "host": "...", "action": "...", "reason": "..." }`

Note: The `secret_injector` currently emits multiple `key_injection` events per request (one per secret). With the new pattern, it would return a single `RequestDecision` summarizing the overall action. If we need per-secret detail, the `Reason` field can list them, or we add an `Actions []string` field.

**Open design question:** Should `secret_injector` emit one event per secret or one event per request? The current behavior (one per secret) provides finer granularity. The proposed `RequestDecision` pattern naturally produces one event per plugin per request. We could support both by having the engine emit the `RequestDecision` event AND allowing the plugin to return additional detail that the engine unpacks into multiple events. For v0, one event per plugin per request is simpler.

#### ResponsePlugin

```go
// ResponseDecision captures what a response transform did.
type ResponseDecision struct {
    Response *http.Response  // The (possibly modified) response
    Action   string          // What happened: "logged_usage", "no_op"
    Reason   string          // Why: "recorded $0.0023 cost for claude-3.5-haiku"
}

type ResponsePlugin interface {
    Plugin
    TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error)
}
```

The engine emits a `response_transform` event after each response plugin call:
- Summary: `"usage_logger: recorded $0.0023 cost for claude-3.5-haiku"`
- Data: `{ "host": "...", "action": "...", "reason": "..." }`

### 4.3 Engine Emission Pattern

The engine already does this for gates. Extend to all phases:

```go
// In engine.go RouteRequest():
for _, r := range e.routers {
    decision, err := r.Route(req, host)
    if err != nil { ... }
    if e.emitter != nil {
        _ = e.emitter.Emit(logging.EventRouteDecision,
            formatRouteSummary(r.Name(), decision),
            r.Name(), nil,
            &logging.RouteDecisionData{
                Host:     host,
                Action:   routeAction(decision),
                RoutedTo: routedTo(decision),
                Reason:   decision.Reason,
            })
    }
    if decision.Directive != nil {
        return decision.Directive, nil
    }
}
```

### 4.4 Removing Emitter from PluginFactory

Once all event emission is in the engine:

```go
// Before:
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)

// After:
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

This removes the temptation for plugins to emit ad-hoc events and simplifies the factory signature.

## 5. New Event Types

Current event types:
- `gate_decision` -- already emitted by engine for GatePlugin calls
- `key_injection` -- currently emitted by secret_injector (will be replaced)
- `http_request` -- emitted by HTTP interceptor
- `http_response` -- emitted by HTTP interceptor
- `budget_action` -- placeholder (not yet emitted)

New event types to add:
- `route_decision` -- emitted by engine for RoutePlugin calls
- `request_transform` -- emitted by engine for RequestPlugin calls (replaces `key_injection`)
- `response_transform` -- emitted by engine for ResponsePlugin calls

Migration: `key_injection` becomes a deprecated alias or is removed entirely. The `request_transform` event with `action: "injected"` captures the same information.

## 6. Impact Assessment

### 6.1 Files That Change

| File | Change |
|------|--------|
| `pkg/policy/plugin.go` | Add `RouteDecision`, `RequestDecision`, `ResponseDecision` structs. Change `RoutePlugin`, `RequestPlugin`, `ResponsePlugin` interfaces. |
| `pkg/policy/engine.go` | Emit events in `RouteRequest()`, `OnRequest()`, `OnResponse()`. Remove `emitter` from `NewEngine` param (move to `PluginFactory` removal). |
| `pkg/policy/host_filter.go` | No change (already returns `*GateVerdict`). |
| `pkg/policy/budget_gate.go` | No change (already returns `*GateVerdict`). |
| `pkg/policy/secret_injector.go` | Return `*RequestDecision` instead of `(*http.Request, error)`. Remove stored `emitter` field and all manual `Emit()` calls. |
| `pkg/policy/local_model_router.go` | Return `*RouteDecision` from `Route()` and `*RequestDecision` from `TransformRequest()`. |
| `pkg/policy/usage_logger.go` | Return `*ResponseDecision` from `TransformResponse()`. |
| `pkg/policy/registry.go` | Remove `emitter` from `PluginFactory` signature. |
| `pkg/logging/event.go` | Add `EventRouteDecision`, `EventRequestTransform`, `EventResponseTransform` constants and data structs. |
| `pkg/net/http.go` | Update calls to `engine.OnRequest()` and `engine.OnResponse()` for new return types. |
| All `*_test.go` files | Update for new return types. Plugin tests become simpler (assert on returned decision struct, no emitter mock). |

### 6.2 Plugin Inventory (5 built-in plugins)

| Plugin | Interfaces | Current Logging | After Change |
|--------|-----------|----------------|-------------|
| `host_filter` | GatePlugin | Engine emits `gate_decision` via `*GateVerdict` | No change needed |
| `budget_gate` | GatePlugin | Engine emits `gate_decision` via `*GateVerdict` | No change needed |
| `secret_injector` | RequestPlugin, PlaceholderProvider | Plugin manually emits `key_injection` | Return `*RequestDecision`; engine emits `request_transform` |
| `local_model_router` | RoutePlugin, RequestPlugin | No events | Return `*RouteDecision` + `*RequestDecision`; engine emits both |
| `usage_logger` | ResponsePlugin | No events (writes own JSONL) | Return `*ResponseDecision`; engine emits `response_transform` |

## 7. Testing Strategy

1. **Plugin tests become assertion-only.** Instead of setting up mock emitters and checking side effects, tests assert directly on the returned `*RouteDecision` / `*RequestDecision` / `*ResponseDecision` structs.

2. **Engine tests verify event emission.** A `CaptureSink` test helper collects emitted events. Engine integration tests wire up real plugins with a CaptureSink and assert the full event sequence.

3. **Negative tests.** Verify that plugins returning empty Reason/Action fields still produce valid events (the system should not panic or skip the event).

4. **Regression test for event sequence.** A golden-file test captures the full event sequence for a representative request flow (gate -> route -> request transform -> HTTP request -> HTTP response -> response transform) and compares against a known-good baseline.

## 8. Migration Plan

### Phase 1: Add new decision structs and event types (no interface changes yet)
- Add `RouteDecision`, `RequestDecision`, `ResponseDecision` to `plugin.go`
- Add `EventRouteDecision`, `EventRequestTransform`, `EventResponseTransform` to `event.go`
- Add corresponding data structs to `event.go`

### Phase 2: Change interfaces and update plugins
- Change `RoutePlugin`, `RequestPlugin`, `ResponsePlugin` interfaces
- Update all 5 plugins to return the new decision types
- Remove `emitter` from `secret_injector` struct; remove all manual `Emit()` calls

### Phase 3: Wire engine emission for all phases
- Add event emission in `RouteRequest()`, `OnRequest()`, `OnResponse()`
- Update callers in `pkg/net/http.go` for new return types

### Phase 4: Clean up
- Remove `emitter` from `PluginFactory` signature
- Remove `emitter` param from `NewEngine`
- Remove unused `key_injection` event type (or keep as deprecated alias)
- Update `docs/event-logging.md` with new event types
