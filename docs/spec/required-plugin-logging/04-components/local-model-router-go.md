# Component: pkg/policy/local_model_router.go

**File:** `pkg/policy/local_model_router.go`
**Agent:** 2 (Plugin Migration)
**Pattern Reference:** Current implementation in
[`pkg/policy/local_model_router.go`](../../../pkg/policy/local_model_router.go).

## Purpose

Migrate `localModelRouterPlugin` to return `*RouteDecision` from `Route()` and
`*RequestDecision` from `TransformRequest()`. Remove the unused `emitter`
parameter from the factory function.

## Changes Required

### 1. Change NewLocalModelRouterPluginFromConfig Signature

```go
// Before:
func NewLocalModelRouterPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {

// After:
func NewLocalModelRouterPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
```

### 2. Remove logging Import

Remove `"github.com/jingkaihe/matchlock/pkg/logging"` since it was only used
for the `emitter` parameter type.

### 3. Change Route() Return Type

The method currently returns `(*RouteDirective, error)`. Change to
`(*RouteDecision, error)`.

Every `return` statement must be wrapped in a `RouteDecision`:

**Early returns for no-match cases (nil directive):**

Each `return nil, nil` becomes a `return &RouteDecision{...}, nil` with a
passthrough reason. The key behavior change: the engine currently
short-circuits on the first non-nil `*RouteDirective`. With `*RouteDecision`,
the engine short-circuits on `decision.Directive != nil`. So passthrough returns
must still have `Directive: nil`.

```go
func (p *localModelRouterPlugin) Route(req *http.Request, host string) (*RouteDecision, error) {
	if len(p.routes) == 0 {
		return &RouteDecision{Reason: "no routes configured"}, nil
	}

	host = strings.Split(host, ":")[0]

	for _, route := range p.routes {
		if route.SourceHost != host {
			continue
		}

		if req.Method != "POST" || req.URL.Path != route.GetPath() {
			return &RouteDecision{Reason: fmt.Sprintf("passthrough: method=%s path=%s", req.Method, req.URL.Path)}, nil
		}

		if req.Body == nil {
			return &RouteDecision{Reason: "passthrough: no request body"}, nil
		}
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return &RouteDecision{Reason: "passthrough: failed to read body"}, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			return &RouteDecision{Reason: "passthrough: invalid JSON body"}, nil
		}

		modelRoute, ok := route.Models[payload.Model]
		if !ok {
			p.logger.Debug("model not in route table, passing through",
				"model", payload.Model, "source_host", host)
			return &RouteDecision{
				Reason: fmt.Sprintf("no matching route for %s", payload.Model),
			}, nil
		}

		backendHost := modelRoute.EffectiveBackendHost(route.GetBackendHost())
		backendPort := modelRoute.EffectiveBackendPort(route.GetBackendPort())

		p.logger.Debug("model matched, rewriting request",
			"model", payload.Model,
			"target", modelRoute.Target,
			"backend", fmt.Sprintf("%s:%d", backendHost, backendPort),
		)

		rewriteRequestForLocal(req, bodyBytes, payload.Model, modelRoute.Target, backendHost, backendPort)

		return &RouteDecision{
			Directive: &RouteDirective{
				Host:   backendHost,
				Port:   backendPort,
				UseTLS: false,
			},
			Reason: fmt.Sprintf("matched model %s -> %s at %s:%d",
				payload.Model, modelRoute.Target, backendHost, backendPort),
		}, nil
	}

	return &RouteDecision{Reason: fmt.Sprintf("no route entry for host %s", host)}, nil
}
```

### 4. Change TransformRequest() Return Type

The current implementation is a no-op passthrough. It stays a no-op but now
returns `*RequestDecision`:

```go
func (p *localModelRouterPlugin) TransformRequest(req *http.Request, host string) (*RequestDecision, error) {
	return &RequestDecision{
		Request: req,
		Action:  "no_op",
		Reason:  "request transform is handled in Route()",
	}, nil
}
```

## Key Behavior Notes

1. **Route() already does request rewriting as a side effect.** The call to
   `rewriteRequestForLocal()` remains inside `Route()`. This means the
   `TransformRequest()` is correctly a no-op. No change to this design.

2. **All return paths produce a RouteDecision.** Even error-like conditions
   (no body, bad JSON) return a decision with `Directive: nil` and a descriptive
   `Reason`, not an error. This matches the existing behavior where those cases
   returned `(nil, nil)` -- passthrough, not error.

3. **The `continue` in the source_host loop.** When iterating routes and the
   source host does not match, we `continue`. This is not a return point. Only
   if we exhaust all routes without a match do we return the final passthrough
   decision.

## Verification

- Plugin compiles against new `RoutePlugin` and `RequestPlugin` interfaces
- No references to `logging.Emitter` remain
- Route behavior is identical: same directive returned for same inputs
- `rewriteRequestForLocal` is still called on match
