# 02 -- Architecture

## Event Flow: Before and After

### Before (Current State)

```
Guest Request
  |
  v
[HTTP Interceptor]
  |-- emits http_request event directly
  |
  v
[Engine.IsHostAllowed]
  |-- calls GatePlugin.Gate() -> *GateVerdict
  |-- Engine emits gate_decision event        <-- TARGET PATTERN
  |
  v
[Engine.RouteRequest]
  |-- calls RoutePlugin.Route() -> (*RouteDirective, error)
  |-- NO event emitted                        <-- GAP
  |
  v
[Engine.OnRequest]
  |-- calls RequestPlugin.TransformRequest() -> (*http.Request, error)
  |-- secret_injector emits key_injection MANUALLY  <-- ANTI-PATTERN
  |-- local_model_router emits NOTHING              <-- SILENT
  |
  v
[Upstream Request/Response]
  |
  v
[Engine.OnResponse]
  |-- calls ResponsePlugin.TransformResponse() -> (*http.Response, error)
  |-- usage_logger emits NOTHING                    <-- SILENT
  |
  v
[HTTP Interceptor]
  |-- emits http_response event directly
```

### After (Target State)

```
Guest Request
  |
  v
[HTTP Interceptor]
  |-- emits http_request event directly (unchanged)
  |
  v
[Engine.IsHostAllowed]
  |-- calls GatePlugin.Gate() -> *GateVerdict
  |-- Engine emits gate_decision event        (unchanged)
  |
  v
[Engine.RouteRequest]
  |-- calls RoutePlugin.Route() -> (*RouteDecision, error)
  |-- Engine emits route_decision event       (NEW)
  |-- extracts RouteDecision.Directive for routing logic
  |
  v
[Engine.OnRequest]
  |-- calls RequestPlugin.TransformRequest() -> (*RequestDecision, error)
  |-- Engine emits request_transform event    (NEW, replaces key_injection)
  |-- extracts RequestDecision.Request for chain
  |
  v
[Upstream Request/Response]
  |
  v
[Engine.OnResponse]
  |-- calls ResponsePlugin.TransformResponse() -> (*ResponseDecision, error)
  |-- Engine emits response_transform event   (NEW)
  |-- extracts ResponseDecision.Response for chain
  |
  v
[HTTP Interceptor]
  |-- emits http_response event directly (unchanged)
```

## Component Dependency Graph

```
pkg/logging/event.go          -- event type constants + data structs
        |
        v
pkg/policy/plugin.go          -- decision structs + interface definitions
        |
        v
pkg/policy/engine.go          -- emission logic (reads decisions, calls emitter)
        |
        v
   +----|----+----+----+
   |    |    |    |    |
   v    v    v    v    v
  HF   BG   SI  LMR  UL     -- plugin implementations
                              (HF=host_filter, BG=budget_gate,
                               SI=secret_injector, LMR=local_model_router,
                               UL=usage_logger)
        |
        v
pkg/policy/registry.go        -- PluginFactory signature (remove emitter)
        |
        v
pkg/net/http.go                -- caller of Engine methods (adapt return types)
```

## File Change Map

| File | Change Type | Agent |
|------|-------------|-------|
| `pkg/logging/event.go` | Add 3 event types + 3 data structs | 1 |
| `pkg/policy/plugin.go` | Add 3 decision structs, change 3 interfaces | 1 |
| `pkg/policy/registry.go` | Remove `emitter` from `PluginFactory` | 1 |
| `pkg/policy/engine.go` | Add emission in 3 methods, update factory call, remove emitter param from `NewEngine` | 1 |
| `pkg/policy/secret_injector.go` | Return `*RequestDecision`, remove emitter | 2 |
| `pkg/policy/local_model_router.go` | Return `*RouteDecision` + `*RequestDecision` | 2 |
| `pkg/policy/usage_logger.go` | Return `*ResponseDecision` | 2 |
| `pkg/policy/host_filter.go` | Remove unused `emitter` param from factory | 2 |
| `pkg/policy/budget_gate.go` | No change (no factory registered) | -- |
| `pkg/net/http.go` | Adapt to new `OnRequest`/`OnResponse` return types | 2 |
| `pkg/policy/engine_test.go` | Update for new return types | 2 |
| `pkg/policy/secret_injector_test.go` | Rewrite event tests for decision struct | 2 |
| `pkg/policy/local_model_router_test.go` | Add decision struct assertions | 2 |
| `pkg/policy/usage_logger_test.go` | Add decision struct assertions | 2 |
| `pkg/policy/host_filter_test.go` | Update factory call signature | 2 |

## Design Principles

### 1. Existing Pattern Reuse

The `GatePlugin` -> `*GateVerdict` -> engine emits `gate_decision` pattern
(implemented in `engine.go` lines 195-233) is the template. Every new phase
follows the same structure:

1. Plugin returns a structured decision
2. Engine reads the decision fields
3. Engine constructs a data struct from `pkg/logging/event.go`
4. Engine calls `e.emitter.Emit(...)` guarded by `if e.emitter != nil`

### 2. Return Type Wrapping

Each decision struct wraps the original return type plus metadata:

| Phase | Original Return | New Return | Wrapped Field |
|-------|----------------|------------|---------------|
| Gate | `*GateVerdict` | `*GateVerdict` | (already structured) |
| Route | `*RouteDirective` | `*RouteDecision` | `.Directive` |
| Request | `*http.Request` | `*RequestDecision` | `.Request` |
| Response | `*http.Response` | `*ResponseDecision` | `.Response` |

### 3. Engine Owns the Emitter

After this change, the `*logging.Emitter` is held only by:
- `Engine` struct (for plugin phase events)
- `HTTPInterceptor` struct (for `http_request` / `http_response` events)

No plugin struct holds or accesses the emitter.

### 4. PluginFactory Simplification

The `PluginFactory` signature loses its third parameter:

```go
// Before
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)

// After
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

This is safe because no plugin needs the emitter after the migration. The engine
is the sole emitter of plugin-phase events.
