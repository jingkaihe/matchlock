# Component A: Event Types and Data Structs

## Purpose

Define the canonical event struct, type constants, and per-type data structs for the policy engine logging standard. This is the foundation layer with no internal dependencies.

## Codebase References

- **Pattern Reference:** `pkg/api/vm.go:47-79` -- the existing `api.Event` struct with typed sub-structs (`NetworkEvent`, `FileEvent`, `ExecEvent`)
- **JSON Tag Convention:** Follow the same `json:"field_name,omitempty"` convention used throughout `pkg/api/config.go`
- **Package Naming:** New package `pkg/logging/` -- follows the flat package structure (`pkg/policy/`, `pkg/state/`, `pkg/net/`)

## File Location

`pkg/logging/event.go`

## Implementation

```go
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
    EventLLMRequest   = "llm_request"
    EventLLMResponse  = "llm_response"
    EventBudgetAction = "budget_action"
    EventKeyInjection = "key_injection"
)

// LLMRequestData is the data payload for llm_request events.
type LLMRequestData struct {
    Method   string `json:"method"`
    Host     string `json:"host"`
    Path     string `json:"path"`
    Model    string `json:"model,omitempty"`
    Routed   bool   `json:"routed"`
    RoutedTo string `json:"routed_to,omitempty"`
}

// LLMResponseData is the data payload for llm_response events.
type LLMResponseData struct {
    Method     string `json:"method"`
    Host       string `json:"host"`
    Path       string `json:"path"`
    StatusCode int    `json:"status_code"`
    DurationMS int64  `json:"duration_ms"`
    BodyBytes  int64  `json:"body_bytes"`
    Model      string `json:"model,omitempty"`
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
```

## Dependencies

- `encoding/json` (standard library)
- `time` (standard library)

## Test Criteria

1. `Event` struct serializes to JSON with correct field names (`ts` not `timestamp`, `run_id` not `runId`)
2. `omitempty` fields are absent from JSON when zero-valued (empty `Plugin`, nil `Tags`, nil `Data`)
3. `Timestamp` serializes as RFC 3339 with sub-second precision
4. Each data struct serializes with correct field names
5. `LLMRequestData` with `Routed: false` still includes the `routed` field (not omitempty)
6. `KeyInjectionData.Action` is always present (not omitempty)
7. Golden file comparison for a complete event with all fields populated
8. Golden file comparison for a minimal event (only required fields)

## Acceptance Criteria

- [ ] All four event type constants are defined
- [ ] All four data structs are defined with correct JSON tags
- [ ] `Event.Data` uses `json.RawMessage` (not a typed field)
- [ ] No dependency on any other `pkg/logging/` file
- [ ] Compiles independently
