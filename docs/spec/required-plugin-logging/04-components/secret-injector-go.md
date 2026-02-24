# Component: pkg/policy/secret_injector.go

**File:** `pkg/policy/secret_injector.go`
**Agent:** 2 (Plugin Migration)
**Pattern Reference:** Current implementation in
[`pkg/policy/secret_injector.go`](../../../pkg/policy/secret_injector.go).

## Purpose

Migrate `secretInjectorPlugin` to return `*RequestDecision` from
`TransformRequest()`. Remove the stored `emitter` field and all manual
`Emit()` calls. Remove `emitter` from constructor signatures.

This is the largest single-file migration because the plugin currently has
three emission sites that must be replaced with structured return values.

## Changes Required

### 1. Remove emitter Field from Struct

```go
// Before:
type secretInjectorPlugin struct {
	secrets      map[string]api.Secret
	placeholders map[string]string
	logger       *slog.Logger
	emitter      *logging.Emitter
}

// After:
type secretInjectorPlugin struct {
	secrets      map[string]api.Secret
	placeholders map[string]string
	logger       *slog.Logger
}
```

### 2. Remove emitter from NewSecretInjectorPlugin

```go
// Before:
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger, emitter *logging.Emitter) *secretInjectorPlugin {

// After:
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger) *secretInjectorPlugin {
```

Remove `emitter: emitter` from the struct literal inside.

### 3. Change NewSecretInjectorPluginFromConfig Signature

```go
// Before:
func NewSecretInjectorPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {

// After:
func NewSecretInjectorPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
```

Update the inner call:
```go
// Before:
return NewSecretInjectorPlugin(cfg.Secrets, logger, emitter), nil

// After:
return NewSecretInjectorPlugin(cfg.Secrets, logger), nil
```

### 4. Change TransformRequest Return Type and Logic

The method currently iterates over secrets and for each one:
- Checks host allowance
- Detects leaks
- Injects secrets
- Manually emits events at each branch

The new version returns a `*RequestDecision` summarizing the overall outcome.
The one-event-per-request model (not one-event-per-secret) is the design
decision from the draft.

```go
func (p *secretInjectorPlugin) TransformRequest(req *http.Request, host string) (*RequestDecision, error) {
	host = strings.Split(host, ":")[0]

	var injected []string
	var skipped []string

	for name, secret := range p.secrets {
		if !p.isSecretAllowedForHost(name, host) {
			if p.requestContainsPlaceholder(req, secret.Placeholder) {
				p.logger.Debug("secret leak detected", "name", name, "host", host)
				return nil, api.ErrSecretLeak
			}
			p.logger.Debug("secret skipped for host", "name", name, "host", host)
			skipped = append(skipped, name)
			continue
		}
		p.replaceInRequest(req, secret.Placeholder, secret.Value)
		p.logger.Debug("secret injected", "name", name, "host", host)
		injected = append(injected, name)
	}

	// Determine overall action and reason
	action := "no_op"
	reason := fmt.Sprintf("no secrets applicable for %s", host)

	if len(injected) > 0 {
		action = "injected"
		reason = fmt.Sprintf("%d secret(s) injected for %s", len(injected), host)
	} else if len(skipped) > 0 {
		action = "skipped"
		reason = fmt.Sprintf("%d secret(s) skipped for %s", len(skipped), host)
	}

	return &RequestDecision{
		Request: req,
		Action:  action,
		Reason:  reason,
	}, nil
}
```

**Key behavior changes:**

1. **Leak detection still returns error.** When a leak is detected, the method
   returns `(nil, api.ErrSecretLeak)`. The engine does NOT emit an event for
   error cases (matching the existing pattern in `IsHostAllowed` where errors
   short-circuit). The `slog.Debug` leak message is preserved.

2. **One event per request, not per secret.** The previous code emitted one
   `key_injection` event per secret. The new code returns a single
   `RequestDecision` summarizing all secrets processed. The engine emits one
   `request_transform` event.

3. **Secret names are NOT in the Reason string.** The reason says "2 secret(s)
   injected" rather than listing names, to avoid leaking secret metadata. The
   count provides auditability without specificity. (If per-secret detail is
   needed later, a `Details` field can be added to `RequestDecision`.)

### 5. Remove logging Import

Remove the `"github.com/jingkaihe/matchlock/pkg/logging"` import since the
plugin no longer references `logging.Emitter`, `logging.EventKeyInjection`, or
`logging.KeyInjectionData`.

### 6. Ensure fmt Import

The `fmt` package is already imported (line 6). Verify it remains after changes.

## Secret Safety

The `Reason` field MUST NOT contain:
- Secret values
- Secret placeholders
- Individual secret names (use counts instead)

This is enforced by the implementation above and verified in tests.

## Verification

- Plugin compiles against new `RequestPlugin` interface
- `api.ErrSecretLeak` is still returned on leak detection
- No references to `logging.Emitter`, `logging.EventKeyInjection`, or
  `logging.KeyInjectionData` remain in this file
