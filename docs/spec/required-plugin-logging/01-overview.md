# 01 -- Overview

## Problem Statement

The current structured event logging system (`*logging.Emitter`) is optional and
ad-hoc. Plugins receive a `*logging.Emitter` via their factory function and
choose whether to use it. This produces three concrete problems:

1. **Silent plugins.** `localModelRouterPlugin` accepts an `emitter` parameter
   in its factory but never stores or uses it. `usageLoggerPlugin` and
   `budgetGatePlugin` do not emit structured events at all. There is no
   compile-time or runtime mechanism to catch this.

2. **Inconsistent boilerplate.** `secretInjectorPlugin` manually emits
   `key_injection` events at three separate call sites, each guarded with
   `if p.emitter != nil`. This pattern must be repeated for every future plugin.

3. **No enforcement.** A new plugin can satisfy its interface without recording
   any decision logic whatsoever.

The `GatePlugin` phase already demonstrates the solution: `GatePlugin.Gate()`
returns `*GateVerdict`, the engine reads the verdict fields, and the engine
emits a `gate_decision` event. The plugin never touches the emitter. This spec
extends that pattern to the remaining three phases: Route, Request, and Response.

## Goals

1. **Every plugin phase call produces a structured event.** No plugin can
   execute without its decision being logged by the engine.
2. **Plugin authors provide domain-specific rationale.** The plugin returns
   a `Reason` string explaining "why." The engine handles "what," "when,"
   "who," and "where."
3. **Minimal boilerplate for plugin authors.** Return a struct with 2-3 fields.
   No emitter, no nil checks, no event construction.
4. **Compile-time enforcement.** If a plugin does not return the decision
   struct, it fails to compile against the interface.
5. **Simplify PluginFactory.** Remove the `emitter` parameter entirely since
   plugins no longer need it.

## Scope

### In Scope

- New decision structs: `RouteDecision`, `RequestDecision`, `ResponseDecision`
- Changed interface signatures for `RoutePlugin`, `RequestPlugin`, `ResponsePlugin`
- New event types: `route_decision`, `request_transform`, `response_transform`
- Engine emission logic for route, request, and response phases
- Migration of `secret_injector` from manual emitter to `RequestDecision`
- Migration of `local_model_router` to return `RouteDecision` and `RequestDecision`
- Migration of `usage_logger` to return `ResponseDecision`
- Removal of `emitter` from `PluginFactory` signature
- Update of `pkg/net/http.go` for new engine return types
- Update of all affected test files
- Deprecation/removal of `key_injection` event type

### Out of Scope

- Adding a `Detail map[string]any` field to `GateVerdict` (noted for future)
- Changing the `http_request` / `http_response` events (emitted by the HTTP
  interceptor, not by plugins)
- Changing the usage logger's own JSONL file output (independent of event log)
- External plugin API stability (no external consumers exist)

## Constraints

- **Go 1.22+** (current project minimum)
- **No new dependencies** -- uses only existing `pkg/logging` infrastructure
- **Backward compatibility is NOT a constraint** -- the plugin system has zero
  external consumers (per draft Q2 decision)

## Key Decision from Draft

**Approach B: Structured Decision Return Types.** Plugins return structured
decision objects; the engine emits events using that metadata. Plugins never
touch the emitter directly. This was resolved in the draft as Q1.
