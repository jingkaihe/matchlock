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
	EventHTTPRequest   = "http_request"
	EventHTTPResponse  = "http_response"
	EventBudgetAction  = "budget_action"
	EventKeyInjection  = "key_injection"
	EventGateDecision  = "gate_decision"
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

// BudgetActionData is the data payload for budget_action events.
// Placeholder for v0 -- no emission site exists yet.
type BudgetActionData struct {
	Action     string  `json:"action"`
	TokensUsed int64   `json:"tokens_used,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	Remaining  float64 `json:"remaining,omitempty"`
}

// KeyInjectionData is the data payload for key_injection events.
type KeyInjectionData struct {
	SecretName string `json:"secret_name"`
	Host       string `json:"host"`
	Action     string `json:"action"` // "injected", "skipped", "leak_blocked"
}
