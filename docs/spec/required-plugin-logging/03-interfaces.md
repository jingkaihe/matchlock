# 03 -- Interfaces

All Go definitions in this document are exact code to be placed in the specified
files. Agents should copy these verbatim.

## 3.1 Decision Structs (pkg/policy/plugin.go)

**Pattern Reference:** Follow `GateVerdict` in
[`pkg/policy/plugin.go`](../../pkg/policy/plugin.go) lines 43-54.

These three structs are added to `pkg/policy/plugin.go` alongside the existing
`GateVerdict` and `RouteDirective` structs.

```go
// RouteDecision captures a routing plugin's full decision.
// Returned by RoutePlugin.Route().
//
// The engine reads Directive to determine routing behavior and emits
// a route_decision event using the Reason field.
type RouteDecision struct {
	// Directive is the routing instruction. nil means passthrough
	// (use the original destination).
	Directive *RouteDirective

	// Reason is a human-readable explanation of the routing decision.
	// Examples: "matched model llama3.1:8b -> 127.0.0.1:11434",
	//           "no matching route for openai/gpt-4o",
	//           "passthrough (wrong host)"
	Reason string
}

// RequestDecision captures what a request transform plugin did.
// Returned by RequestPlugin.TransformRequest().
//
// The engine reads Request to continue the chain and emits a
// request_transform event using Action and Reason.
type RequestDecision struct {
	// Request is the (possibly modified) outbound request.
	// Must be non-nil on success.
	Request *http.Request

	// Action describes what happened in machine-readable form.
	// Conventions: "injected", "skipped", "leak_blocked", "no_op",
	// "rewritten". Plugins should pick from these or define new
	// lowercase_snake actions.
	Action string

	// Reason is a human-readable explanation.
	// Examples: "secret OPENROUTER_API_KEY injected for openrouter.ai",
	//           "no secrets applicable for this host"
	// MUST NOT contain secret values.
	Reason string
}

// ResponseDecision captures what a response transform plugin did.
// Returned by ResponsePlugin.TransformResponse().
//
// The engine reads Response to continue the chain and emits a
// response_transform event using Action and Reason.
type ResponseDecision struct {
	// Response is the (possibly modified) inbound response.
	// Must be non-nil on success.
	Response *http.Response

	// Action describes what happened in machine-readable form.
	// Conventions: "logged_usage", "no_op", "modified".
	Action string

	// Reason is a human-readable explanation.
	// Examples: "recorded $0.0023 cost for claude-3.5-haiku",
	//           "skipped: non-openrouter host"
	Reason string
}
```

## 3.2 Changed Interface Signatures (pkg/policy/plugin.go)

**Pattern Reference:** Follow `GatePlugin` interface in
[`pkg/policy/plugin.go`](../../pkg/policy/plugin.go) lines 24-32.

Replace the existing `RoutePlugin`, `RequestPlugin`, and `ResponsePlugin`
interfaces with these definitions. The `Plugin`, `GatePlugin`,
`PlaceholderProvider`, `GateVerdict`, and `RouteDirective` types are unchanged.

```go
// RoutePlugin can redirect requests to alternative backends.
// Route plugins run during the RouteRequest phase.
//
// Semantics: First non-nil RouteDecision.Directive wins. Subsequent route
// plugins are not called once a directive is returned.
type RoutePlugin interface {
	Plugin
	// Route inspects a request and returns a RouteDecision.
	// Set Directive to nil for passthrough (use original destination).
	// Return a non-nil error to block the request.
	Route(req *http.Request, host string) (*RouteDecision, error)
}

// RequestPlugin transforms outbound requests before they are sent upstream.
// Request plugins run during the OnRequest phase.
//
// Semantics: Plugins are chained -- the output Request of one feeds into the
// next. Returning an error blocks the request.
type RequestPlugin interface {
	Plugin
	// TransformRequest modifies the request and returns a decision describing
	// what was done. The Request field in the returned decision must be non-nil.
	// Return a non-nil error to block the request (e.g., secret leak detection).
	TransformRequest(req *http.Request, host string) (*RequestDecision, error)
}

// ResponsePlugin transforms inbound responses before they reach the guest.
// Response plugins run during the OnResponse phase.
//
// Semantics: Plugins are chained -- the output Response of one feeds into the
// next. Returning an error drops the response.
type ResponsePlugin interface {
	Plugin
	// TransformResponse modifies the response and returns a decision describing
	// what was done. The Response field in the returned decision must be non-nil.
	TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error)
}
```

## 3.3 Changed Engine Method Signatures (pkg/policy/engine.go)

**Pattern Reference:** Follow `IsHostAllowed()` in
[`pkg/policy/engine.go`](../../pkg/policy/engine.go) lines 195-233.

### RouteRequest

```go
// RouteRequest inspects a request and returns a RouteDirective if a router
// plugin wants to redirect it. First non-nil directive wins.
// Emits a route_decision event for each router plugin evaluated.
func (e *Engine) RouteRequest(req *http.Request, host string) (*RouteDirective, error)
```

**Note:** The external return type stays `*RouteDirective` (not `*RouteDecision`)
to minimize churn in `pkg/net/http.go`. The engine unwraps the decision
internally.

### OnRequest

```go
// OnRequest runs request transform plugins in chain order.
// Emits a request_transform event for each plugin.
func (e *Engine) OnRequest(req *http.Request, host string) (*http.Request, error)
```

**Note:** The external return type stays `(*http.Request, error)`. The engine
unwraps `RequestDecision.Request` internally.

### OnResponse

```go
// OnResponse runs response transform plugins in chain order.
// Emits a response_transform event for each plugin.
func (e *Engine) OnResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error)
```

**Note:** The external return type stays `(*http.Response, error)`. The engine
unwraps `ResponseDecision.Response` internally.

### Why Keep External Return Types Unchanged

The engine methods are called by `pkg/net/http.go`. By keeping the return types
the same, `http.go` needs zero changes for the engine method signatures. The
`RequestDecision`/`ResponseDecision` types are internal to the engine<->plugin
boundary. The HTTP interceptor does not need to know about decisions.

## 3.4 Changed PluginFactory Signature (pkg/policy/registry.go)

**Pattern Reference:** Current signature in
[`pkg/policy/registry.go`](../../pkg/policy/registry.go) line 16.

```go
// Before:
type PluginFactory func(config json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error)

// After:
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)
```

This removes the `emitter` parameter and the `logging` import from `registry.go`.

## 3.5 Changed NewEngine Signature (pkg/policy/engine.go)

**Pattern Reference:** Current signature in
[`pkg/policy/engine.go`](../../pkg/policy/engine.go) line 28.

The `emitter` parameter moves from being passed to individual plugins to being
retained solely by the engine.

```go
// Signature is UNCHANGED (engine still receives the emitter):
func NewEngine(config *api.NetworkConfig, logger *slog.Logger, emitter *logging.Emitter) *Engine
```

The internal change is that `NewEngine` no longer passes `e.emitter` to plugin
constructors or factory calls. The engine is the sole consumer of the emitter.

## 3.6 Unchanged Interfaces

The following are explicitly unchanged:

- `Plugin` interface (just `Name() string`)
- `GatePlugin` interface (already returns `*GateVerdict`)
- `GateVerdict` struct
- `PlaceholderProvider` interface
- `RouteDirective` struct
- `CostProvider` interface (in `budget_gate.go`)
