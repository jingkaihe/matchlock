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
// Semantics: First non-nil RouteDirective wins. Subsequent route plugins
// are not called once a directive is returned.
type RoutePlugin interface {
	Plugin
	// Route inspects a request and optionally returns a RouteDirective.
	// Returning (nil, nil) means "use the original destination."
	// Returning (nil, err) means "block the request with an error."
	Route(req *http.Request, host string) (*RouteDirective, error)
}

// RequestPlugin transforms outbound requests before they are sent upstream.
// Request plugins run during the OnRequest phase.
//
// Semantics: Plugins are chained -- the output of one feeds into the next.
// Returning an error blocks the request.
type RequestPlugin interface {
	Plugin
	// TransformRequest modifies the request in-place or returns a new request.
	// Return a non-nil error to block the request (e.g., secret leak detection).
	TransformRequest(req *http.Request, host string) (*http.Request, error)
}

// ResponsePlugin transforms inbound responses before they reach the guest.
// Response plugins run during the OnResponse phase.
//
// Semantics: Plugins are chained -- the output of one feeds into the next.
// Returning an error drops the response.
type ResponsePlugin interface {
	Plugin
	// TransformResponse modifies the response in-place or returns a new response.
	TransformResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error)
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
