package logging

import (
	"encoding/json"
	"time"
)

// Event is the canonical structured event for the policy engine logging standard.
// Required fields: Timestamp, RunID, AgentSystem, EventType, Summary.
// Optional fields use omitempty tags.
type Event struct {
	Timestamp   time.Time       `json:"ts"`
	RunID       string          `json:"run_id"`
	AgentSystem string          `json:"agent_system"`
	EventType   string          `json:"event_type"`
	Summary     string          `json:"summary"`
	Plugin      string          `json:"plugin,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

// Event type constants for v0.
const (
	EventHTTPRequest       = "http_request"
	EventHTTPResponse      = "http_response"
	EventKeyInjection      = "key_injection" // DEPRECATED: replaced by EventRequestTransform
	EventGateDecision      = "gate_decision"
	EventRouteDecision     = "route_decision"     // NEW
	EventRequestTransform  = "request_transform"  // NEW
	EventResponseTransform = "response_transform" // NEW
)

// HTTPRequestData is the data payload for http_request events.
type HTTPRequestData struct {
	Method   string `json:"method"`
	Host     string `json:"host"`
	Path     string `json:"path"`
	Model    string `json:"model,omitempty"`
	Routed   bool   `json:"routed"`
	RoutedTo string `json:"routed_to,omitempty"`
}

// HTTPResponseData is the data payload for http_response events.
type HTTPResponseData struct {
	Method     string `json:"method"`
	Host       string `json:"host"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
	DurationMS int64  `json:"duration_ms"`
	BodyBytes  int64  `json:"body_bytes"`
	Model      string `json:"model,omitempty"`
}

// GateDecisionData is the data payload for gate_decision events.
type GateDecisionData struct {
	Host    string `json:"host"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// KeyInjectionData is the data payload for key_injection events.
type KeyInjectionData struct {
	SecretName string `json:"secret_name"`
	Host       string `json:"host"`
	Action     string `json:"action"` // "injected", "skipped", "leak_blocked"
}

// RouteDecisionData is the data payload for route_decision events.
type RouteDecisionData struct {
	Host     string `json:"host"`
	Action   string `json:"action"`              // "passthrough" or "redirected"
	RoutedTo string `json:"routed_to,omitempty"` // "host:port" when redirected
	Reason   string `json:"reason,omitempty"`
}

// RequestTransformData is the data payload for request_transform events.
type RequestTransformData struct {
	Host   string `json:"host"`
	Action string `json:"action"` // "injected", "skipped", "no_op", etc.
	Reason string `json:"reason,omitempty"`
}

// ResponseTransformData is the data payload for response_transform events.
type ResponseTransformData struct {
	Host   string `json:"host"`
	Action string `json:"action"` // "logged_usage", "no_op", etc.
	Reason string `json:"reason,omitempty"`
}
