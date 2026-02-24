# Component E: Policy Engine Integration

## Purpose

Modify `policy.Engine` to accept, store, and distribute the `*logging.Emitter` to plugins that need it. The engine itself does not emit events in v0 (gate/route decisions are v1), but it serves as the distribution point for the emitter to request/response plugins.

## Codebase References

- **Existing Constructor:** `pkg/policy/engine.go:26-121` -- `NewEngine` already accepts `*slog.Logger` and distributes it to plugins
- **Plugin Logger Distribution:** Lines 42-75 show the pattern: `pluginLogger := e.logger.With("plugin", "...")` passed to each plugin constructor
- **Factory Pattern:** `pkg/policy/registry.go:13` -- `PluginFactory` signature currently takes `(config json.RawMessage, logger *slog.Logger)`

## File Location

`pkg/policy/engine.go`

## Changes Required

### 1. Add emitter field to Engine struct

```go
type Engine struct {
    gates     []GatePlugin
    routers   []RoutePlugin
    requests  []RequestPlugin
    responses []ResponsePlugin

    placeholders map[string]string
    logger       *slog.Logger
    emitter      *logging.Emitter  // NEW: nil means no event logging
}
```

### 2. Modify NewEngine signature

```go
func NewEngine(config *api.NetworkConfig, logger *slog.Logger, emitter *logging.Emitter) *Engine {
    if logger == nil {
        logger = slog.Default()
    }
    e := &Engine{
        placeholders: make(map[string]string),
        logger:       logger.With("component", "policy"),
        emitter:      emitter,  // NEW: may be nil
    }
    // ... rest unchanged except plugin construction
```

### 3. Pass emitter to plugins that need it

For the secret injector (the only plugin emitting events in v0):

```go
// In the flat-field compilation section (line 53):
if len(config.Secrets) > 0 {
    pluginLogger := e.logger.With("plugin", "secret_injector")
    p := NewSecretInjectorPlugin(config.Secrets, pluginLogger, e.emitter)  // NEW: pass emitter
    e.addPlugin(p)
    // ... rest unchanged
}
```

For the explicit plugin config section, the `PluginFactory` signature needs updating:

### 4. Update PluginFactory signature

**File:** `pkg/policy/registry.go`

```go
// Current:
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)

// New:
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)
```

Update the factory call in `NewEngine`:

```go
// Line 98 currently:
p, err := factory(pluginCfg.Config, pluginLogger)

// New:
p, err := factory(pluginCfg.Config, pluginLogger, e.emitter)
```

### 5. Update all existing factory functions

Each built-in factory must accept the new parameter (even if they ignore it in v0):

**`pkg/policy/host_filter.go`:**
```go
func NewHostFilterPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {
    // emitter unused in v0 -- host_filter events are v1
    // ... existing implementation unchanged
}
```

**`pkg/policy/secret_injector.go`:**
```go
func NewSecretInjectorPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {
    // ... parse config
    return NewSecretInjectorPlugin(cfg.Secrets, logger, emitter), nil
}
```

**`pkg/policy/local_model_router.go`:**
```go
func NewLocalModelRouterPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {
    // emitter unused in v0 -- route_decision events are v1
    // ... existing implementation unchanged
}
```

### 6. Expose emitter for HTTPInterceptor

The `HTTPInterceptor` is not a policy plugin -- it receives the emitter via a separate path (through the network config struct). However, the engine should expose its emitter for callers that need it:

```go
// Emitter returns the engine's event emitter. May be nil.
func (e *Engine) Emitter() *logging.Emitter {
    return e.emitter
}
```

This is NOT used in v0 (the emitter is passed directly to the interceptor via the network config), but provides a clean accessor for future use.

## Dependencies

- `github.com/jingkaihe/matchlock/pkg/logging` (for `*logging.Emitter` type)
- All existing dependencies unchanged

## Test Criteria

1. **Nil emitter:** `NewEngine(config, nil, nil)` works identically to the current behavior
2. **Existing tests pass:** All tests in `pkg/policy/engine_test.go` continue to pass with the new signature (pass `nil` for emitter)
3. **Factory signature:** All three built-in factories accept the new `*logging.Emitter` parameter
4. **Registry tests:** `pkg/policy/registry_test.go` tests continue to pass
5. **Emitter accessor:** `engine.Emitter()` returns the emitter passed to constructor

## Acceptance Criteria

- [ ] `Engine` struct has `emitter *logging.Emitter` field
- [ ] `NewEngine` accepts `*logging.Emitter` as third parameter
- [ ] `PluginFactory` type signature updated to include `*logging.Emitter`
- [ ] All three built-in factory functions updated
- [ ] `init()` registrations in `registry.go` still compile
- [ ] Emitter passed to `NewSecretInjectorPlugin`
- [ ] All existing tests pass with `nil` emitter
- [ ] No behavior change when emitter is nil
