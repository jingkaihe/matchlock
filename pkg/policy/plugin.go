package policy

import (
	"net/http"
)

// Plugin is the base interface all policy plugins implement.
// A single plugin can implement multiple phase interfaces
// (e.g., local_model_router implements both RoutePlugin and RequestPlugin).
type Plugin interface {
	// Name returns the plugin's unique identifier (e.g., "host_filter").
	// Names are used for logging and conflict detection.
	Name() string
}

// GatePlugin decides whether a request to a host should proceed.
// Gate plugins run during the IsHostAllowed phase.
//
// Semantics: If multiple gate plugins are registered, the engine uses
// AND logic -- ALL gates must allow the host for the request to proceed.
// If ANY gate denies the host, the request is blocked and the first
// denying gate's verdict is returned to the caller.
// If no gate plugins are registered, all hosts are allowed.
type GatePlugin interface {
	Plugin
	// Gate evaluates whether the given host is allowed.
	// Return a verdict with Allowed=true to permit the request.
	// Return a verdict with Allowed=false to block it; set the optional
	// HTTP response fields (StatusCode, ContentType, Body) to customize
	// the error sent to the guest.
	Gate(host string) *GateVerdict
}

// GateVerdict carries the result of a gate evaluation.
//
// When returned from Engine.IsHostAllowed:
//   - nil means the host is allowed (all gates passed or no gates registered).
//   - non-nil means the host was blocked by a gate.
//
// When returned from GatePlugin.Gate:
//   - Allowed=true means this gate permits the host.
//   - Allowed=false means this gate denies the host.
type GateVerdict struct {
	Allowed bool   // Whether the host is allowed.
	Reason  string // Human-readable reason for denial (used in logs; empty if allowed).

	// HTTP error response fields (optional, used only when Allowed=false).
	// When set, the HTTP interceptor sends this response to the guest
	// instead of the default behavior. When unset (zero values), the
	// interceptor falls back to its default (HTTP 403 "Blocked by policy").
	StatusCode  int    // HTTP status code (e.g., 429, 403). 0 = use default (403).
	ContentType string // Content-Type header. Empty = "text/plain".
	Body        string // Response body. Empty = "Blocked by policy".
}

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

// PlaceholderProvider is an optional interface for plugins that manage
// secret placeholders. These placeholders are injected into guest
// environment variables via sandbox_common.go prepareExecEnv().
//
// After all plugins are registered, the engine collects placeholders
// from all PlaceholderProvider implementations and exposes them via
// GetPlaceholders().
type PlaceholderProvider interface {
	// GetPlaceholders returns a map of env-var-name -> placeholder-string.
	GetPlaceholders() map[string]string
}

// RouteDirective tells the HTTP interceptor to send a request to an
// alternative backend instead of the original destination.
// A nil *RouteDirective means "use the original destination."
type RouteDirective struct {
	Host   string // Target host, e.g., "127.0.0.1"
	Port   int    // Target port, e.g., 11434
	UseTLS bool   // Whether to use TLS for the upstream connection
}

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
