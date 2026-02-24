# Component: pkg/policy/host_filter.go

**File:** `pkg/policy/host_filter.go`
**Agent:** 2 (Plugin Migration)
**Pattern Reference:** Current implementation in
[`pkg/policy/host_filter.go`](../../../pkg/policy/host_filter.go).

## Purpose

Update the factory function signature to match the new `PluginFactory` type.
The `GatePlugin` interface and `Gate()` method are unchanged.

## Changes Required

### 1. Change NewHostFilterPluginFromConfig Signature

```go
// Before:
func NewHostFilterPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {

// After:
func NewHostFilterPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
```

### 2. Remove logging Import

Remove `"github.com/jingkaihe/matchlock/pkg/logging"` since it was only used
for the `emitter` parameter type (which was never used in the function body).

## No Other Changes

- `Gate()` return type (`*GateVerdict`) is unchanged
- `NewHostFilterPlugin()` constructor is unchanged (it never took an emitter)
- All internal logic is unchanged

## Verification

- Factory compiles against new `PluginFactory` type
- `init()` registration in `registry.go` still works
