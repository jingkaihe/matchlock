# 08 -- Changelog

## Specification Created

- **Date:** 2026-02-24
- **Source draft:** `docs/draft/required-plugin-logging.md`
- **Author:** spec-architect agent

## Summary of Draft-to-Spec Changes

### Clarifications Added

1. **Engine return types stay unchanged.** The draft implied `RouteRequest()`
   might return `*RouteDecision` to callers. The spec clarifies that the engine
   unwraps decisions internally and returns `*RouteDirective` to `http.go`,
   eliminating HTTP interceptor changes.

2. **One event per plugin per request for secret_injector.** The draft flagged
   this as an open design question. The spec resolves it: one `request_transform`
   event per plugin invocation, not one per secret. The `Reason` field includes
   a count.

3. **Secret safety in Reason fields.** The spec explicitly requires that
   `RequestDecision.Reason` must not contain secret values, placeholder strings,
   or individual secret names. Only counts are permitted.

4. **Leak detection still returns error.** The spec clarifies that when
   `secret_injector` detects a leak, it returns `(nil, api.ErrSecretLeak)`.
   No event is emitted for error cases (matching existing engine behavior).

5. **All passthrough returns produce decisions.** The spec requires that
   `Route()` passthrough cases return `&RouteDecision{Directive: nil, Reason: "..."}`
   rather than `(nil, nil)`. Every call path produces a decision for logging.

### Design Decisions Made

| Decision | Rationale |
|----------|-----------|
| Keep engine method return types unchanged | Minimizes `http.go` churn; decisions are internal to engine<->plugin boundary |
| 2 agents (not 3) | Tests are tightly coupled to plugin changes; splitting them would require extra coordination |
| Retain `EventKeyInjection` constant with deprecation comment | Defense in depth for hypothetical external log parsers |
| `captureSink` reused from existing test code | Already proven in `secret_injector_test.go` |
| `budget_gate.go` and `pkg/net/http.go` require zero changes | Gate interface unchanged; engine return types unchanged |

### New Interfaces Defined

| Type | File | Fields |
|------|------|--------|
| `RouteDecision` | `pkg/policy/plugin.go` | `Directive *RouteDirective`, `Reason string` |
| `RequestDecision` | `pkg/policy/plugin.go` | `Request *http.Request`, `Action string`, `Reason string` |
| `ResponseDecision` | `pkg/policy/plugin.go` | `Response *http.Response`, `Action string`, `Reason string` |

### New Event Types Defined

| Constant | Value | Data Struct |
|----------|-------|-------------|
| `EventRouteDecision` | `"route_decision"` | `RouteDecisionData` |
| `EventRequestTransform` | `"request_transform"` | `RequestTransformData` |
| `EventResponseTransform` | `"response_transform"` | `ResponseTransformData` |

### Files to be Modified (15 total)

| File | Change Size |
|------|-------------|
| `pkg/logging/event.go` | Small (add constants + 3 structs) |
| `pkg/policy/plugin.go` | Small (add 3 structs, change 3 interfaces) |
| `pkg/policy/registry.go` | Trivial (change PluginFactory, remove import) |
| `pkg/policy/engine.go` | Medium (add emission in 3 methods, update construction) |
| `pkg/policy/secret_injector.go` | Large (remove emitter, rewrite TransformRequest) |
| `pkg/policy/local_model_router.go` | Medium (wrap returns in decisions, change factory) |
| `pkg/policy/usage_logger.go` | Medium (wrap returns in decisions, change factory) |
| `pkg/policy/host_filter.go` | Trivial (change factory signature) |
| `pkg/policy/secret_injector_test.go` | Large (rewrite all call sites, remove 5 tests, add 5) |
| `pkg/policy/local_model_router_test.go` | Medium (update assertions, add decision tests) |
| `pkg/policy/usage_logger_test.go` | Medium (update assertions, add decision tests) |
| `pkg/policy/host_filter_test.go` | Trivial (update factory calls) |
| `pkg/policy/engine_test.go` | Medium (add 5 emission tests, update constructor calls) |

### Files Explicitly NOT Modified

| File | Reason |
|------|--------|
| `pkg/policy/budget_gate.go` | GatePlugin interface unchanged, no factory registered |
| `pkg/net/http.go` | Engine return types unchanged |
| `pkg/logging/emitter.go` | Emitter API unchanged |
| `pkg/logging/sink.go` | Sink interface unchanged |
