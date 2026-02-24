# Component: pkg/policy/registry.go

**File:** `pkg/policy/registry.go`
**Agent:** 1 (Foundation)
**Pattern Reference:** Current signature in
[`pkg/policy/registry.go`](../../../pkg/policy/registry.go) line 16.

## Purpose

Simplify the `PluginFactory` type signature by removing the `emitter` parameter.
Plugins no longer need access to the emitter.

## Changes Required

### 1. Change PluginFactory Type

```go
// Before:
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)

// After:
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

### 2. Remove logging Import

The `"github.com/jingkaihe/matchlock/pkg/logging"` import is no longer needed
in this file and should be removed.

### 3. Update Comment

Update the doc comment on `PluginFactory` to remove the emitter description:

```go
// PluginFactory creates a plugin from its JSON config blob.
// The logger is pre-scoped with component=policy and plugin=<name>
// by the engine before calling the factory. Plugins should store
// it directly and use it for Debug-level logging.
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

## No Changes to init()

The `init()` function registers factories by name. The factory function values
themselves will be updated in their respective plugin files (e.g.,
`NewSecretInjectorPluginFromConfig` signature change). The `Register()` call
syntax does not change:

```go
Register("host_filter", NewHostFilterPluginFromConfig)
Register("secret_injector", NewSecretInjectorPluginFromConfig)
Register("local_model_router", NewLocalModelRouterPluginFromConfig)
Register("usage_logger", NewUsageLoggerPluginFromConfig)
```

## Verification

After this change, any factory function that still accepts 3 parameters will
fail to compile at the `Register()` call site. This enforces migration of all
factory signatures.
