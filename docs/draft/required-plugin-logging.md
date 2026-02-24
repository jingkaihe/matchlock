# Required Plugin Logging

# Open Questions

## Unresolved

- [ ] Q1: Engine-level wrapper vs. plugin-level responsibility -- who emits the event?
  - Context: The engine already calls plugins through known interfaces (`Gate`, `Route`, `TransformRequest`, `TransformResponse`). We could wrap those calls at the engine level to automatically emit events, or we could require each plugin to emit its own events internally.
  - Options considered:
    - **Option A (Engine wrapper):** The engine wraps every plugin call with pre/post event emission. Plugins never touch the emitter directly. The engine controls what gets logged.
    - **Option B (Plugin contract):** Plugins must return structured decision metadata (a `DecisionLog`) alongside their normal return values. The engine emits the event using that metadata.
    - **Option C (Hybrid):** The engine emits a baseline event for every plugin call (plugin name, phase, duration, success/failure), but plugins can optionally enrich it with domain-specific detail via a `LogDetail()` method.
  - Recommendation: Option B. It keeps plugins in control of their decision rationale while making it structurally impossible to forget -- the return type itself forces the plugin to populate the log. See section 4.2 for a concrete sketch.

- [ ] Q2: Should we change the plugin interfaces (breaking change) or add a parallel set of "logging-aware" interfaces?
  - Context: Changing `GatePlugin.Gate()` to return a `GateDecision` struct instead of `(bool, string)` is a clean approach but breaks any third-party plugins compiled against the current interface. Adding new interfaces (e.g., `LoggingGatePlugin`) creates a migration path but doubles the interface surface.
  - Options considered:
    - **Option A (Break the interface):** Change `Gate(host string) (bool, string)` to `Gate(host string) *GateDecision`. Since matchlock has no stable plugin API yet and all current plugins are built-in, this is low-cost now but becomes expensive later.
    - **Option B (Parallel interface):** Add `LoggingGatePlugin` with the new signature. Engine checks for it first, falls back to wrapping the old interface. Existing plugins continue to work.
    - **Option C (Wrapper at addPlugin):** Keep current interfaces unchanged. The engine wraps each plugin at registration time with a decorator that captures timing and basic pass/fail, then emits the event. No interface change at all.
  - Recommendation: Option A for gate/route/request/response interfaces. The plugin system is brand new (this branch). There are exactly three built-in plugins and zero third-party consumers. Now is the time to make the interface right. If we add a parallel interface, we will never remove the old one.

- [ ] Q3: What level of detail should the engine auto-capture vs. require from plugins?
  - Context: Some data can only come from the plugin (e.g., which allowlist pattern matched, which secret was injected for which host). Other data can be captured generically by the engine (e.g., plugin name, phase, duration, error). The question is where to draw the line.
  - Options considered:
    - **Minimal engine capture:** Engine logs plugin name + phase + duration + error. Plugin provides all semantic detail.
    - **Rich engine capture:** Engine also captures inputs (host, method, path) since it already has them. Plugin provides only decision-specific detail.
  - Recommendation: Rich engine capture. The engine already has the request context (`host`, `req.Method`, `req.URL.Path`). Duplicating that information in every plugin's return value is wasteful boilerplate. The plugin should only need to provide the "why" -- the decision rationale that only it knows.

## Resolved

(No questions resolved yet.)

---

## 1. Problem Statement

The current structured event logging system (JSON-L via `*logging.Emitter`) is optional and ad-hoc. Plugins receive a `*logging.Emitter` and choose whether to use it. This creates three problems:

1. **Silent plugins.** A plugin author can forget to emit events entirely (as `localModelRouterPlugin` currently does -- it accepts an `emitter` parameter in its factory but never stores or uses it).
2. **Inconsistent detail.** Even plugins that do emit events (like `secretInjectorPlugin`) must repeat boilerplate patterns: nil-check the emitter, construct the data struct, choose an event type, format a summary.
3. **No enforcement.** There is no compile-time or runtime mechanism to ensure a plugin records its decision logic.

The goal is to make logging a **structural requirement** of the plugin system, so that every plugin's decision logic is recorded to the event log without relying on the plugin author to remember.

## 2. Current State Analysis

### 2.1 Plugin Interfaces

Defined in `/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/policy/plugin.go`:

```go
type Plugin interface {
    Name() string
}

type GatePlugin interface {
    Plugin
    Gate(host string) (allowed bool, reason string)
}

type RoutePlugin interface {
    Plugin
    Route(req *http.Request, host string) (*RouteDirective, error)
}

type RequestPlugin interface {
    Plugin
    TransformRequest(req *http.Request, host string) (*http.Request, error)
}

type ResponsePlugin interface {
    Plugin
    TransformResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error)
}
```

Key observation: These interfaces return bare values. There is no structural place for decision metadata.

### 2.2 How Plugins Currently Handle Logging

**host_filter** (`/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/policy/host_filter.go`):
- Accepts `emitter` in its factory signature but **never stores it** on the struct.
- Does not emit any structured events. Decision logic is only visible through `slog.Debug` calls.

**secret_injector** (`/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/policy/secret_injector.go`):
- Stores `emitter *logging.Emitter` on the struct.
- Emits `key_injection` events with three actions: `"injected"`, `"skipped"`, `"leak_blocked"`.
- Every emission site has the pattern: `if p.emitter != nil { _ = p.emitter.Emit(...) }`.
- This is the most complete logging implementation, but it is 100% opt-in and manually maintained.

**local_model_router** (`/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/policy/local_model_router.go`):
- Accepts `emitter` in its factory but **never stores it**.
- Zero structured event emissions. Routing decisions (model match, passthrough, rewrite) are only logged via `slog.Debug`.

### 2.3 How the Engine Currently Emits Events

The engine itself emits events in `IsHostAllowed()` (`/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/policy/engine.go`, lines 182-207). After calling `g.Gate(host)`, it checks `if e.emitter != nil` and emits `gate_decision` events. This is the "engine wrapper" pattern applied manually to one phase.

Notably, `RouteRequest()`, `OnRequest()`, and `OnResponse()` do **not** emit events at the engine level. Only the `secret_injector` plugin emits events during `OnRequest()`, and nothing emits during routing or response transformation.

### 2.4 The PluginFactory Signature

```go
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)
```

The `emitter` is passed to every factory but documented as "optional (may be nil)". The factory decides whether to use it.

## 3. Design Goals

1. **Every plugin phase call produces a structured event.** No plugin can execute without its decision being logged.
2. **Plugin authors provide domain-specific rationale.** The "why" behind a decision must come from the plugin, not generic metadata.
3. **Minimal boilerplate for plugin authors.** The framework should handle the repetitive parts (timestamps, plugin name, phase, common request fields).
4. **Compile-time enforcement where possible.** If a plugin does not satisfy the logging contract, it should fail to compile.
5. **Backward compatibility is not a constraint.** The plugin system is new and has no external consumers.

## 4. Proposed Approaches

### 4.1 Approach A: Engine-Level Decorator (Zero Plugin Changes)

The engine wraps each plugin call with automatic pre/post event emission. Plugins are unaware of logging.

```go
// In engine.go IsHostAllowed():
for _, g := range e.gates {
    start := time.Now()
    allowed, reason := g.Gate(host)
    e.emitPluginEvent("gate", g.Name(), host, time.Since(start), map[string]any{
        "allowed": allowed,
        "reason":  reason,
    })
    // ... existing logic
}
```

**Strengths:**
- Zero burden on plugin authors.
- Guaranteed -- no plugin can bypass it.
- Centralizes all event emission logic.

**Weaknesses:**
- The engine can only capture what the interface returns. For `Gate`, that is `(bool, string)` -- enough. For `Route`, it is `(*RouteDirective, error)` -- we lose the "why" (e.g., which model matched, why it was a passthrough). For `TransformRequest`, we get `(*http.Request, error)` -- we have no idea what changed.
- Plugin-specific detail (which pattern matched, which secret was scoped to which host) is lost unless the interface changes.

### 4.2 Approach B: Structured Decision Return Types (Recommended)

Change the plugin interfaces to return structured decision objects that include both the operational result and the decision rationale.

```go
// GateDecision captures the gate plugin's full decision.
type GateDecision struct {
    Allowed bool
    Reason  string           // Why blocked (or empty if allowed)
    Detail  map[string]any   // Plugin-specific detail (e.g., matched pattern)
}

type GatePlugin interface {
    Plugin
    Gate(host string) *GateDecision
}
```

```go
// RouteDecision captures the routing plugin's full decision.
type RouteDecision struct {
    Directive *RouteDirective  // nil = passthrough
    Detail    map[string]any   // e.g., {"matched_model": "llama3.1:8b", "action": "redirected"}
}

type RoutePlugin interface {
    Plugin
    Route(req *http.Request, host string) (*RouteDecision, error)
}
```

```go
// TransformResult captures what a request/response transform did.
type TransformResult struct {
    Request  *http.Request    // (or Response for ResponsePlugin)
    Actions  []string         // Human-readable list of what changed, e.g., ["injected secret API_KEY", "skipped secret OTHER_KEY"]
    Detail   map[string]any   // Structured detail
}

type RequestPlugin interface {
    Plugin
    TransformRequest(req *http.Request, host string) (*TransformResult, error)
}
```

The engine then emits a structured event for every plugin call using the decision metadata. The plugin cannot skip the metadata because it is part of the return type.

**Strengths:**
- Compile-time enforcement: if the plugin returns `*GateDecision`, it must populate the struct.
- Plugin controls the narrative (detail, actions) while the engine controls the envelope (timestamp, plugin name, phase).
- Clean separation: plugins do not touch the emitter at all.
- Testable: tests can assert on the `Detail` field without needing a mock emitter.

**Weaknesses:**
- Breaking change to all plugin interfaces.
- The `map[string]any` detail field is untyped -- a plugin could return an empty map and technically satisfy the contract.
- Adds a struct allocation per call (negligible for this workload).

### 4.3 Approach C: Hybrid Decorator + Optional Enrichment

Keep current interfaces unchanged. The engine wraps calls with automatic event emission. Plugins that want to add detail implement an optional `DecisionLogger` interface.

```go
// DecisionLogger is optionally implemented by plugins that want to
// enrich the auto-emitted event with domain-specific detail.
type DecisionLogger interface {
    // LastDecisionDetail returns structured detail about the most recent
    // plugin call. Called by the engine immediately after each phase method.
    LastDecisionDetail() map[string]any
}
```

The engine does:
```go
allowed, reason := g.Gate(host)
detail := map[string]any{"allowed": allowed, "reason": reason}
if dl, ok := g.(DecisionLogger); ok {
    for k, v := range dl.LastDecisionDetail() {
        detail[k] = v
    }
}
e.emitPluginEvent(...)
```

**Strengths:**
- No breaking interface change.
- Existing plugins work unchanged with baseline logging.
- Plugins can opt into richer logging when ready.

**Weaknesses:**
- `LastDecisionDetail()` is stateful and fragile -- relies on timing between the phase call and the detail call.
- Not concurrency-safe without careful design (what if two goroutines call the same plugin?).
- The "optional" nature defeats the "must be done" requirement -- a plugin can simply not implement `DecisionLogger`.

## 5. Comparison Matrix

| Criterion | A (Decorator) | B (Decision Types) | C (Hybrid) |
|-----------|--------------|-------------------|------------|
| Compile-time enforcement | None (logging is invisible to plugin) | Strong (return type requires metadata) | Partial (baseline is automatic, enrichment is optional) |
| Plugin author burden | Zero | Moderate (populate struct fields) | Low (implement optional interface) |
| Decision detail richness | Low (only interface return values) | High (plugin controls detail) | Medium (baseline + opt-in enrichment) |
| Breaking change | None | Yes (all interfaces) | None |
| Concurrency safety | Safe (engine captures at call site) | Safe (returned values are per-call) | Fragile (`LastDecisionDetail` is stateful) |
| Testability | Requires emitter mock/spy | Assert on return struct directly | Requires emitter mock for baseline, struct assert for enrichment |

## 6. Impact Assessment

### 6.1 Files That Would Change

Regardless of approach, these files are affected:

| File | Approach A | Approach B | Approach C |
|------|-----------|-----------|-----------|
| `pkg/policy/plugin.go` | No change | Interface changes + new decision types | New optional interface |
| `pkg/policy/engine.go` | Add wrapper logic around all phase calls | Consume decision structs + emit events | Add wrapper + optional detail check |
| `pkg/policy/host_filter.go` | No change | Return `*GateDecision` instead of `(bool, string)` | No change (optional: add `DecisionLogger`) |
| `pkg/policy/secret_injector.go` | Remove manual emitter calls | Return `*TransformResult` instead of `(*http.Request, error)` | Remove manual emitter calls (engine handles it) |
| `pkg/policy/local_model_router.go` | No change | Return `*RouteDecision` and `*TransformResult` | No change |
| `pkg/policy/registry.go` | Possibly remove `emitter` from `PluginFactory` | Remove `emitter` from `PluginFactory` | No change |
| `pkg/logging/event.go` | Add new event type constant(s) | Add new event type constant(s) | Add new event type constant(s) |
| `pkg/policy/engine_test.go` | Add emitter assertions | Update for new return types | Add emitter assertions |
| `pkg/policy/host_filter_test.go` | No change | Update for `*GateDecision` return | No change |
| `pkg/policy/secret_injector_test.go` | Remove emitter setup from tests | Update for `*TransformResult` return | No change |
| `pkg/policy/local_model_router_test.go` | No change | Update for `*RouteDecision` return | No change |

### 6.2 Event Types

The current event type constants in `/Users/denver/Documents/code/agents/matchlock-policy-engine-logging/pkg/logging/event.go` include `EventGateDecision` and `EventKeyInjection`. For required plugin logging, we would likely add:

- `EventRouteDecision` -- emitted when a route plugin makes a decision
- `EventRequestTransform` -- emitted when a request plugin transforms a request
- `EventResponseTransform` -- emitted when a response plugin transforms a response

Or we could use a single `EventPluginDecision` with a `phase` field to distinguish.

### 6.3 Removing Emitter from PluginFactory

If the engine handles all event emission, plugins no longer need the `*logging.Emitter`. The `PluginFactory` signature could simplify:

```go
// Before:
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)

// After:
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

This is a clean simplification regardless of which approach we choose. It removes the temptation for plugins to emit ad-hoc events outside the engine's control.

## 7. Testing Strategy

Regardless of approach, we need:

1. **Event capture test helper.** A `Sink` implementation that collects events into a slice for assertion.
2. **Per-plugin event assertion.** Each plugin's test suite should verify that the correct events are emitted with the expected detail.
3. **Integration test.** Wire up an engine with all plugins and an event-capturing sink. Run a realistic request flow. Assert the full event sequence.
4. **Negative test.** Verify that a plugin that returns empty/missing detail still produces a valid (if sparse) event -- the system should not panic.

For Approach B specifically, plugin tests become simpler because they can assert on the returned `*GateDecision` / `*RouteDecision` / `*TransformResult` without needing any emitter mock at all.

## 8. Recommendation

**Approach B (Structured Decision Return Types)** is the recommended path, for these reasons:

1. The plugin system is brand new with zero external consumers. The cost of breaking the interface is near zero right now and will only increase over time.
2. It provides the strongest compile-time guarantee: a plugin literally cannot return a result without providing the decision struct.
3. It cleanly separates concerns: plugins own "what I decided and why," the engine owns "record that decision to the event log."
4. It simplifies the `PluginFactory` by removing the `emitter` parameter.
5. It makes plugin tests cleaner -- assert on return values, not side effects.

The main risk is the `map[string]any` detail field being left empty. This can be mitigated with:
- Documentation and code review norms.
- A `linter` or test helper that flags plugins with empty detail fields.
- The `Actions []string` field in `TransformResult` providing a lightweight narrative even if `Detail` is empty.
