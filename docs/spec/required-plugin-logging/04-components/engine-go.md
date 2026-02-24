# Component: pkg/policy/engine.go

**File:** `pkg/policy/engine.go`
**Agent:** 1 (Foundation)
**Pattern Reference:** Existing `IsHostAllowed()` emission pattern in
[`pkg/policy/engine.go`](../../../pkg/policy/engine.go) lines 195-233.

## Purpose

Add event emission to `RouteRequest()`, `OnRequest()`, and `OnResponse()`.
Adapt these methods to unwrap decision structs returned by the updated plugin
interfaces. Remove `emitter` from plugin construction calls inside `NewEngine`.

## Changes Required

### 1. NewEngine -- Remove Emitter from Plugin Construction

In `NewEngine()`, the engine currently passes `e.emitter` to plugins:

**Line 58 (secret_injector flat-field construction):**
```go
// Before:
p := NewSecretInjectorPlugin(config.Secrets, pluginLogger, e.emitter)

// After:
p := NewSecretInjectorPlugin(config.Secrets, pluginLogger)
```

**Line 122 (factory call for explicit plugins):**
```go
// Before:
p, err := factory(pluginCfg.Config, pluginLogger, e.emitter)

// After:
p, err := factory(pluginCfg.Config, pluginLogger)
```

The `e.emitter` field on the `Engine` struct and its use in `NewEngine`'s
signature remain unchanged. The engine still holds the emitter for its own
emission logic.

### 2. RouteRequest -- Add Event Emission

**Pattern:** Mirror the `IsHostAllowed()` emission pattern. Emit one
`route_decision` event per router plugin evaluated.

Replace the current `RouteRequest` method body. The external signature
`(*RouteDirective, error)` does NOT change -- the engine unwraps
`RouteDecision.Directive` internally.

```go
func (e *Engine) RouteRequest(req *http.Request, host string) (*RouteDirective, error) {
	for _, r := range e.routers {
		decision, err := r.Route(req, host)
		if err != nil {
			e.logger.Warn("route error", "plugin", r.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			action := "passthrough"
			routedTo := ""
			if decision.Directive != nil {
				action = "redirected"
				routedTo = fmt.Sprintf("%s:%d", decision.Directive.Host, decision.Directive.Port)
			}
			summary := fmt.Sprintf("route %s %s by %s", action, host, r.Name())
			if routedTo != "" {
				summary = fmt.Sprintf("route %s %s -> %s by %s", action, host, routedTo, r.Name())
			}
			_ = e.emitter.Emit(logging.EventRouteDecision,
				summary,
				r.Name(),
				nil,
				&logging.RouteDecisionData{
					Host:     host,
					Action:   action,
					RoutedTo: routedTo,
					Reason:   decision.Reason,
				})
		}

		if decision.Directive != nil {
			e.logger.Info(
				fmt.Sprintf("local model redirect: %s request to %s%s redirected to -> %s:%d (local-backend)",
					req.Method, host, req.URL.Path, decision.Directive.Host, decision.Directive.Port),
				"plugin", r.Name(),
			)
			return decision.Directive, nil
		}
	}
	e.logger.Debug("route passthrough",
		"host", host,
		"method", req.Method,
		"path", req.URL.Path,
	)
	return nil, nil
}
```

### 3. OnRequest -- Add Event Emission

Replace the current `OnRequest` method body. The external signature
`(*http.Request, error)` does NOT change.

```go
func (e *Engine) OnRequest(req *http.Request, host string) (*http.Request, error) {
	for _, p := range e.requests {
		decision, err := p.TransformRequest(req, host)
		if err != nil {
			e.logger.Warn("request transform failed",
				"plugin", p.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			_ = e.emitter.Emit(logging.EventRequestTransform,
				fmt.Sprintf("%s: %s for %s", p.Name(), decision.Action, host),
				p.Name(),
				nil,
				&logging.RequestTransformData{
					Host:   host,
					Action: decision.Action,
					Reason: decision.Reason,
				})
		}

		req = decision.Request
	}
	return req, nil
}
```

### 4. OnResponse -- Add Event Emission

Replace the current `OnResponse` method body. The external signature
`(*http.Response, error)` does NOT change.

```go
func (e *Engine) OnResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error) {
	for _, p := range e.responses {
		decision, err := p.TransformResponse(resp, req, host)
		if err != nil {
			e.logger.Warn("response transform failed",
				"plugin", p.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			_ = e.emitter.Emit(logging.EventResponseTransform,
				fmt.Sprintf("%s: %s for %s", p.Name(), decision.Action, host),
				p.Name(),
				nil,
				&logging.ResponseTransformData{
					Host:   host,
					Action: decision.Action,
					Reason: decision.Reason,
				})
		}

		resp = decision.Response
	}
	return resp, nil
}
```

### 5. Remove Emitter() Accessor (Optional)

The `Emitter()` method on `Engine` (line 304) exposes the emitter to callers.
After this change, no plugin needs it. However, `pkg/net/http.go` may still
use `engine.Emitter()` to get the emitter for its own HTTP events.

**Decision:** Keep `Emitter()` for now. The HTTP interceptor currently receives
its emitter via its own constructor, so it does not call `engine.Emitter()`. But
removing it now is a separate cleanup. Leave it.

## Import Changes

No new imports needed. `fmt`, `logging`, and all current imports remain.

## Error Handling

The emission pattern follows the existing convention:
```go
_ = e.emitter.Emit(...)
```

Emission errors are discarded (best-effort semantics), matching the existing
`IsHostAllowed()` pattern.

## Edge Cases

1. **Plugin returns error:** Emission does NOT happen for the erroring plugin.
   The error short-circuits the loop, matching current behavior.

2. **Decision with empty Reason/Action:** Valid. The event is still emitted with
   empty fields. The engine does not panic or skip.

3. **Nil emitter:** Guarded by `if e.emitter != nil`. Safe when event logging is
   disabled.
