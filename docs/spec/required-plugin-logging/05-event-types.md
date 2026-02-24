# 05 -- Event Types

**File:** `pkg/logging/event.go`
**Agent:** 1 (Foundation)
**Pattern Reference:** Existing event types and data structs in
[`pkg/logging/event.go`](../../pkg/logging/event.go).

## New Event Type Constants

Add three new constants to the existing `const` block:

```go
const (
	EventHTTPRequest        = "http_request"
	EventHTTPResponse       = "http_response"
	EventBudgetAction       = "budget_action"
	EventKeyInjection       = "key_injection"       // DEPRECATED: replaced by EventRequestTransform
	EventGateDecision       = "gate_decision"
	EventRouteDecision      = "route_decision"       // NEW
	EventRequestTransform   = "request_transform"    // NEW
	EventResponseTransform  = "response_transform"   // NEW
)
```

### Migration: key_injection

`EventKeyInjection` is retained as a constant with a deprecation comment but is
no longer emitted by any code path. It can be removed in a future cleanup pass.
Keeping it avoids breaking any external log parsers that filter on
`"key_injection"` (defense in depth, even though no external consumers exist
today).

## New Data Structs

### RouteDecisionData

```go
// RouteDecisionData is the data payload for route_decision events.
type RouteDecisionData struct {
	Host     string `json:"host"`
	Action   string `json:"action"`              // "passthrough" or "redirected"
	RoutedTo string `json:"routed_to,omitempty"` // "host:port" when redirected
	Reason   string `json:"reason,omitempty"`
}
```

**Pattern Reference:** Follows `GateDecisionData` structure with
`Host` + `Action` + domain-specific fields.

### RequestTransformData

```go
// RequestTransformData is the data payload for request_transform events.
type RequestTransformData struct {
	Host   string `json:"host"`
	Action string `json:"action"`          // "injected", "skipped", "no_op", etc.
	Reason string `json:"reason,omitempty"`
}
```

**Pattern Reference:** Follows the same `Host` + `Action` + `Reason` pattern.

### ResponseTransformData

```go
// ResponseTransformData is the data payload for response_transform events.
type ResponseTransformData struct {
	Host   string `json:"host"`
	Action string `json:"action"`          // "logged_usage", "no_op", etc.
	Reason string `json:"reason,omitempty"`
}
```

## Existing Structs: No Changes

The following are unchanged:

- `Event` struct
- `HTTPRequestData`
- `HTTPResponseData`
- `GateDecisionData`
- `BudgetActionData`
- `KeyInjectionData` (retained for backward compatibility; no longer emitted)

## Event Examples

### route_decision (redirect)

```json
{
  "ts": "2025-01-15T10:30:00.123Z",
  "run_id": "sandbox-abc123",
  "agent_system": "openclaw",
  "event_type": "route_decision",
  "summary": "route redirected openrouter.ai -> 127.0.0.1:11434 by local_model_router",
  "plugin": "local_model_router",
  "data": {
    "host": "openrouter.ai",
    "action": "redirected",
    "routed_to": "127.0.0.1:11434",
    "reason": "matched model meta-llama/llama-3.1-8b-instruct -> llama3.1:8b at 127.0.0.1:11434"
  }
}
```

### route_decision (passthrough)

```json
{
  "ts": "2025-01-15T10:30:00.123Z",
  "run_id": "sandbox-abc123",
  "agent_system": "openclaw",
  "event_type": "route_decision",
  "summary": "route passthrough openrouter.ai by local_model_router",
  "plugin": "local_model_router",
  "data": {
    "host": "openrouter.ai",
    "action": "passthrough",
    "reason": "no matching route for openai/gpt-4o"
  }
}
```

### request_transform (injected)

```json
{
  "ts": "2025-01-15T10:30:00.124Z",
  "run_id": "sandbox-abc123",
  "agent_system": "openclaw",
  "event_type": "request_transform",
  "summary": "secret_injector: injected for openrouter.ai",
  "plugin": "secret_injector",
  "data": {
    "host": "openrouter.ai",
    "action": "injected",
    "reason": "1 secret(s) injected for openrouter.ai"
  }
}
```

### response_transform (logged_usage)

```json
{
  "ts": "2025-01-15T10:30:01.456Z",
  "run_id": "sandbox-abc123",
  "agent_system": "openclaw",
  "event_type": "response_transform",
  "summary": "usage_logger: logged_usage for openrouter.ai",
  "plugin": "usage_logger",
  "data": {
    "host": "openrouter.ai",
    "action": "logged_usage",
    "reason": "recorded $0.0049 cost for anthropic/claude-sonnet-4 via openrouter"
  }
}
```

## Full Event Sequence for a Typical Request

A routed request through the full pipeline produces this event sequence:

```
1. gate_decision      (host_filter: allowed openrouter.ai)
2. gate_decision      (budget_gate: allowed openrouter.ai)
3. route_decision     (local_model_router: redirected -> 127.0.0.1:11434)
4. request_transform  (secret_injector: skipped for 127.0.0.1)
5. request_transform  (local_model_router: no_op)
6. http_request       (POST openrouter.ai/api/v1/chat/completions)
7. http_response      (POST openrouter.ai -> 200)
8. response_transform (usage_logger: logged_usage)
```

A non-routed request produces:

```
1. gate_decision      (host_filter: allowed openrouter.ai)
2. gate_decision      (budget_gate: allowed openrouter.ai)
3. route_decision     (local_model_router: passthrough)
4. request_transform  (secret_injector: injected for openrouter.ai)
5. request_transform  (local_model_router: no_op)
6. http_request       (POST openrouter.ai/api/v1/chat/completions)
7. http_response      (POST openrouter.ai -> 200)
8. response_transform (usage_logger: logged_usage)
```
