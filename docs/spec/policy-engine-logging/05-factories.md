# 05 - Factory Patterns

## Overview

This feature uses two factory patterns:

1. **Emitter Construction Factory** -- in the sandbox construction path, creates the `Emitter` + `JSONLWriter` based on configuration
2. **Plugin Factory Signature Update** -- the existing `PluginFactory` type in `pkg/policy/registry.go` gains an `*Emitter` parameter

## Factory 1: Emitter Construction

### Location

Inline in `pkg/sandbox/sandbox_darwin.go` and `pkg/sandbox/sandbox_linux.go` within the `New()` function.

This is NOT a standalone factory function because:
- It is called exactly once per sandbox
- It depends on sandbox-local state (VM ID, state manager base dir)
- The logic is ~20 lines and does not benefit from abstraction

### Pattern Reference

Follows the pattern of inline policy engine construction already in `sandbox_darwin.go:281`:
```go
policyEngine := policy.NewEngine(config.Network, nil)
```

The emitter construction follows immediately after with the same inline pattern.

### Construction Flow

```
1. Check config.Logging != nil && config.Logging.Enabled
2. Resolve RunID (config value or fallback to VM ID)
3. Resolve log path (config value or derive default from state manager)
4. os.MkdirAll for the log directory
5. logging.NewJSONLWriter(logPath)
6. logging.NewEmitter(cfg, writer)
7. Store on Sandbox struct
```

### Non-Fatal Semantics

If any step in the construction fails, the emitter is `nil` and the sandbox starts without event logging. Failures are logged via `slog.Warn` but do not propagate as errors. This is critical -- event logging is observability, not functionality.

## Factory 2: Plugin Factory Signature

### Location

`pkg/policy/registry.go`

### Current Signature

```go
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

### New Signature

```go
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)
```

### Pattern Reference

The existing factory registration in `pkg/policy/registry.go:20-25`:

```go
func init() {
    Register("host_filter", NewHostFilterPluginFromConfig)
    Register("secret_injector", NewSecretInjectorPluginFromConfig)
    Register("local_model_router", NewLocalModelRouterPluginFromConfig)
}
```

All three factory functions must be updated to accept the new parameter. The `host_filter` and `local_model_router` factories accept but ignore the emitter in v0 (they will use it in v1 for `gate_decision` and `route_decision` events).

### Factory Call Site

In `pkg/policy/engine.go`, the factory is called at line 98:

```go
// Current:
p, err := factory(pluginCfg.Config, pluginLogger)

// New:
p, err := factory(pluginCfg.Config, pluginLogger, e.emitter)
```

### Why Not a Separate EmitterFactory?

The emitter does not need its own factory registry because:
- There is exactly one emitter per sandbox (not one per plugin)
- The emitter's construction depends on sandbox-level state, not plugin-level config
- Future sink types (remote endpoint, aggregation buffer) would be added as additional `Sink` implementations passed to the same `NewEmitter` call, not as separate emitter types

## No New Factory Files

This feature does not introduce any new factory files. All factory-related changes are modifications to existing files:

| File | Change |
|---|---|
| `pkg/policy/registry.go` | Update `PluginFactory` type signature |
| `pkg/policy/engine.go` | Update factory call site |
| `pkg/policy/host_filter.go` | Update `NewHostFilterPluginFromConfig` signature |
| `pkg/policy/secret_injector.go` | Update `NewSecretInjectorPluginFromConfig` signature |
| `pkg/policy/local_model_router.go` | Update `NewLocalModelRouterPluginFromConfig` signature |
| `pkg/sandbox/sandbox_darwin.go` | Inline emitter construction |
| `pkg/sandbox/sandbox_linux.go` | Inline emitter construction |
