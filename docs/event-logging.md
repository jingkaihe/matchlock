# Event Logging

Matchlock writes a persistent JSON-L event log capturing every decision made by the policy engine. Every plugin call, every gate evaluation, every HTTP request and response is recorded. Nothing passes through the policy engine silently.

## Architecture

```
                         Guest VM
                            |
                            v
                   +-----------------+
                   | HTTP Interceptor |  <-- emits http_request, http_response
                   +-----------------+
                            |
          +-----------------+-----------------+
          |                                   |
          v                                   |
  +------------------+                        |
  | Engine           |                        |
  | (holds Emitter)  |                        |
  +------------------+                        |
          |                                   |
          |  1. IsHostAllowed (Gate phase)     |
          |     +---------------------------+ |
          |     | host_filter  -> GateVerdict| |
          |     | budget_gate  -> GateVerdict| |
          |     +---------------------------+ |
          |     Engine emits: gate_decision   |
          |                                   |
          |  2. RouteRequest (Route phase)     |
          |     +-------------------------------+
          |     | local_model_router -> RouteDecision
          |     +-------------------------------+
          |     Engine emits: route_decision    |
          |                                     |
          |  3. OnRequest (Request phase)       |
          |     +-------------------------------+
          |     | secret_injector -> RequestDecision
          |     | local_model_router -> RequestDecision
          |     +-------------------------------+
          |     Engine emits: request_transform |
          |                                     |
          v                                     |
  +------------------+                          |
  | Upstream / Local |  (actual HTTP call)      |
  |     Backend      |                          |
  +------------------+                          |
          |                                     |
          v                                     |
          |  4. OnResponse (Response phase)     |
          |     +-------------------------------+
          |     | usage_logger -> ResponseDecision
          |     +-------------------------------+
          |     Engine emits: response_transform
          |
          v
     Back to Guest VM
```

**Key principle:** Plugins never emit events. They return structured decision objects (`GateVerdict`, `RouteDecision`, `RequestDecision`, `ResponseDecision`). The engine reads those decisions and emits events. This is enforced at compile time -- the plugin interfaces require returning these structs.

The HTTP interceptor is the only non-engine component that emits events (`http_request` and `http_response`), because it operates outside the plugin phase pipeline.

---

## Enabling

Pass `--event-log <path>` to `matchlock run`:

```bash
matchlock run \
  --event-log ./events.jsonl \
  --agent-system openclaw \
  --run-id my-session-123 \
  ...
```

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--event-log` | Yes (to enable) | -- | Path to the JSON-L output file. |
| `--agent-system` | No | `""` | Label for the agent system (e.g., `openclaw`, `aider`). |
| `--run-id` | No | sandbox VM ID | Session identifier. Defaults to `vm-<id>`. |

---

## Event Schema

Every event has the same top-level shape:

```json
{
  "ts":           "2026-02-24T17:49:40.619586Z",
  "run_id":       "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type":   "gate_decision",
  "summary":      "gate allowed openrouter.ai by host_filter",
  "plugin":       "host_filter",
  "tags":         ["tls"],
  "data":         { ... }
}
```

### Required fields (present on every event)

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | RFC 3339 timestamp with sub-second precision. |
| `run_id` | string | Session/run identifier. |
| `agent_system` | string | Agent system label set at startup. |
| `event_type` | string | One of the event types listed below. |
| `summary` | string | Human-readable one-line description. |

### Optional fields (present when applicable)

| Field | Type | Description |
|-------|------|-------------|
| `plugin` | string | Which policy plugin produced the decision. |
| `tags` | string[] | Tags for filtering (e.g., `"tls"`, `"http"`). |
| `data` | object | Structured payload. Shape depends on `event_type`. |

---

## Event Type Reference

| Event Type | Source | Phase | Emitted By | Description |
|------------|--------|-------|------------|-------------|
| `gate_decision` | Policy engine | Gate | Engine | Host allowed or blocked by a gate plugin. One per gate plugin per connection. |
| `route_decision` | Policy engine | Route | Engine | Routing decision: passthrough or redirect to local backend. One per route plugin per request. |
| `request_transform` | Policy engine | Request | Engine | Request transformation: secret injection, rewrite, or no-op. One per request plugin per request. |
| `response_transform` | Policy engine | Response | Engine | Response transformation: usage logging or no-op. One per response plugin per request. |
| `http_request` | HTTP interceptor | Transport | Interceptor | Outbound HTTP/HTTPS request forwarded upstream. Emitted after all policy phases complete. |
| `http_response` | HTTP interceptor | Transport | Interceptor | Upstream response received before forwarding to guest. Paired with `http_request`. |
| `budget_action` | -- | -- | -- | Placeholder for future budget tracking. Not emitted yet. |

---

## Event Types

### `gate_decision`

Emitted by the engine when a gate plugin evaluates whether an outbound connection to a host is allowed or blocked.

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | Hostname being evaluated. |
| `allowed` | bool | Whether the connection was permitted. |
| `reason` | string | Why the host was blocked. Empty if allowed. |
| `pattern` | string | Allowlist pattern that matched. Empty if blocked. |

**Example:**

```json
{
  "event_type": "gate_decision",
  "summary": "gate blocked evil.com by host_filter: host not in allowlist",
  "plugin": "host_filter",
  "data": { "host": "evil.com", "allowed": false, "reason": "host not in allowlist" }
}
```

---

### `route_decision`

Emitted by the engine when a route plugin decides whether to redirect a request to a local backend or let it pass through.

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | Target hostname of the request. |
| `action` | string | `"passthrough"` or `"redirected"`. |
| `routed_to` | string | `host:port` of the local backend, if redirected. Empty otherwise. |
| `reason` | string | Why the decision was made (e.g., `"matched model llama3.1:8b"`). |

**Example:**

```json
{
  "event_type": "route_decision",
  "summary": "route redirected openrouter.ai -> 127.0.0.1:11434 by local_model_router",
  "plugin": "local_model_router",
  "data": { "host": "openrouter.ai", "action": "redirected", "routed_to": "127.0.0.1:11434", "reason": "matched model llama3.1:8b -> 127.0.0.1:11434" }
}
```

---

### `request_transform`

Emitted by the engine when a request plugin transforms (or inspects) an outbound request.

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | Target hostname of the request. |
| `action` | string | `"injected"`, `"skipped"`, `"no_op"`, or `"rewritten"`. |
| `reason` | string | What happened (e.g., `"1 secret(s) injected for 1 allowed host(s)"`). |

**Example:**

```json
{
  "event_type": "request_transform",
  "summary": "secret_injector: injected for openrouter.ai",
  "plugin": "secret_injector",
  "data": { "host": "openrouter.ai", "action": "injected", "reason": "1 secret(s) injected for 1 allowed host(s)" }
}
```

Note: Secret names and values are never included in the event data. Only counts are logged.

---

### `response_transform`

Emitted by the engine when a response plugin transforms (or inspects) an inbound response.

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | Target hostname of the original request. |
| `action` | string | `"logged_usage"`, `"no_op"`, or `"modified"`. |
| `reason` | string | What happened (e.g., `"recorded $0.0023 cost for claude-3.5-haiku via openrouter"`). |

**Example:**

```json
{
  "event_type": "response_transform",
  "summary": "usage_logger: logged_usage for openrouter.ai",
  "plugin": "usage_logger",
  "data": { "host": "openrouter.ai", "action": "logged_usage", "reason": "recorded $0.0023 cost for anthropic/claude-3.5-haiku via openrouter" }
}
```

---

### `http_request`

Emitted by the HTTP interceptor when an outbound request is forwarded upstream (after all policy phases complete).

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `method` | string | HTTP method (`GET`, `POST`, etc.). |
| `host` | string | Target hostname. |
| `path` | string | Request path. |
| `model` | string | Model name if detected from the request body. Empty otherwise. |
| `routed` | bool | `true` if redirected to a local backend. |
| `routed_to` | string | `host:port` of the local backend, if routed. |

**Tags:** `["tls"]` for HTTPS, `["http"]` for plain HTTP.

---

### `http_response`

Emitted by the HTTP interceptor when the upstream response is received. Paired with a preceding `http_request`.

**`data` shape:**

| Field | Type | Description |
|-------|------|-------------|
| `method` | string | HTTP method of the original request. |
| `host` | string | Target hostname. |
| `path` | string | Request path. |
| `status_code` | int | HTTP response status code. |
| `duration_ms` | int | Round-trip time in milliseconds. |
| `body_bytes` | int | Response body size in bytes. |
| `model` | string | Model name if detected. |

**Tags:** `["tls"]` for HTTPS, `["http"]` for plain HTTP.

---

## Plugin Directory

Every plugin that participates in event logging, what phase it runs in, what decisions it makes, and what events those produce.

### `host_filter`

| | |
|---|---|
| **Phase** | Gate |
| **Interface** | `GatePlugin` |
| **Returns** | `*GateVerdict` |
| **Event** | `gate_decision` |
| **Source** | `pkg/policy/host_filter.go` |

Evaluates outbound connections against a configured allowlist. Blocks private IPs unless explicitly allowed.

| Decision | `allowed` | `reason` |
|----------|-----------|----------|
| Host matches allowlist | `true` | -- |
| Host not in allowlist | `false` | `"host not in allowlist"` |
| Private IP blocked | `false` | `"private IP blocked"` |

---

### `budget_gate`

| | |
|---|---|
| **Phase** | Gate |
| **Interface** | `GatePlugin` |
| **Returns** | `*GateVerdict` |
| **Event** | `gate_decision` |
| **Source** | `pkg/policy/budget_gate.go` |

Blocks all requests once cumulative API spend exceeds the configured USD limit. Returns HTTP 429 with an OpenAI-format JSON error body.

| Decision | `allowed` | `reason` |
|----------|-----------|----------|
| Under budget | `true` | -- |
| Budget exceeded | `false` | `"budget exceeded: $5.0012 spent of $5.00 limit"` |

---

### `local_model_router`

| | |
|---|---|
| **Phases** | Route, Request |
| **Interfaces** | `RoutePlugin`, `RequestPlugin` |
| **Returns** | `*RouteDecision`, `*RequestDecision` |
| **Events** | `route_decision`, `request_transform` |
| **Source** | `pkg/policy/local_model_router.go` |

Redirects requests for specific models to a local backend (e.g., Ollama). Also implements `RequestPlugin` for host header rewriting (currently no-op; rewriting is handled in the route phase).

**Route phase decisions:**

| Decision | `action` | `reason` |
|----------|----------|----------|
| Model matched, redirecting | `"redirected"` | `"matched model llama3.1:8b -> 127.0.0.1:11434"` |
| No matching model | `"passthrough"` | `"no matching route for openai/gpt-4o"` |
| Wrong host, no route entry | `"passthrough"` | `"no route entry for api.openai.com"` |

**Request phase decisions:**

| Decision | `action` | `reason` |
|----------|----------|----------|
| Always | `"no_op"` | `"request transform is handled in Route()"` |

---

### `secret_injector`

| | |
|---|---|
| **Phase** | Request |
| **Interfaces** | `RequestPlugin`, `PlaceholderProvider` |
| **Returns** | `*RequestDecision` |
| **Event** | `request_transform` |
| **Source** | `pkg/policy/secret_injector.go` |

Replaces secret placeholders in outbound requests with real secret values, scoped to allowed hosts. Blocks requests that would leak a secret placeholder to an unauthorized host.

| Decision | `action` | `reason` |
|----------|----------|----------|
| Secrets injected | `"injected"` | `"1 secret(s) injected for 1 allowed host(s)"` |
| No secrets for this host | `"skipped"` | `"1 secret(s) skipped, host not in allowed list"` |
| No secrets configured | `"no_op"` | `"no secrets to process"` |
| Leak detected | *(error returned, no event)* | Request blocked with `api.ErrSecretLeak` |

Note: When a leak is detected, the plugin returns an error instead of a decision. The engine does not emit an event for errors. The connection is terminated.

---

### `usage_logger`

| | |
|---|---|
| **Phase** | Response |
| **Interface** | `ResponsePlugin` |
| **Returns** | `*ResponseDecision` |
| **Event** | `response_transform` |
| **Source** | `pkg/policy/usage_logger.go` |

Intercepts OpenRouter API responses to extract token usage and cost data. Writes detailed usage entries to a separate JSONL file (`usage.jsonl`). Also tracks cumulative cost for budget enforcement.

| Decision | `action` | `reason` |
|----------|----------|----------|
| Usage recorded | `"logged_usage"` | `"recorded $0.0023 cost for anthropic/claude-3.5-haiku via openrouter"` |
| Wrong host | `"no_op"` | `"skipped: host api.openai.com is not openrouter.ai"` |
| Wrong path | `"no_op"` | `"skipped: path /api/v1/models is not a chat completions endpoint"` |
| Non-200 status | `"no_op"` | `"skipped: status 400 is not 200"` |
| Parse failure | `"no_op"` | `"skipped: invalid JSON in response body"` |

---

## Typical Event Sequences

### Standard API call (allowed, no routing)

```
gate_decision      →  gate allowed openrouter.ai by host_filter
gate_decision      →  gate allowed openrouter.ai by budget_gate
route_decision     →  route passthrough openrouter.ai by local_model_router
request_transform  →  secret_injector: injected for openrouter.ai
request_transform  →  local_model_router: no_op for openrouter.ai
http_request       →  POST openrouter.ai/api/v1/chat/completions
http_response      →  POST openrouter.ai/api/v1/chat/completions -> 200 (1234ms)
response_transform →  usage_logger: logged_usage for openrouter.ai
```

### Routed to local backend (Ollama)

```
gate_decision      →  gate allowed openrouter.ai by host_filter
gate_decision      →  gate allowed openrouter.ai by budget_gate
route_decision     →  route redirected openrouter.ai -> 127.0.0.1:11434 by local_model_router
request_transform  →  secret_injector: injected for openrouter.ai
request_transform  →  local_model_router: no_op for openrouter.ai
http_request       →  POST openrouter.ai/api/v1/chat/completions (routed=true)
http_response      →  POST openrouter.ai/api/v1/chat/completions -> 200 (350ms)
response_transform →  usage_logger: logged_usage for openrouter.ai
```

### Host blocked by allowlist

```
gate_decision      →  gate blocked evil.com by host_filter: host not in allowlist
```

Connection refused. No further events.

### Budget exceeded

```
gate_decision      →  gate allowed openrouter.ai by host_filter
gate_decision      →  gate blocked openrouter.ai by budget_gate: budget exceeded: $5.00 spent of $5.00 limit
```

Connection refused with HTTP 429. No further events.

### Secret leak detected

```
gate_decision      →  gate allowed suspicious.com by host_filter
gate_decision      →  gate allowed suspicious.com by budget_gate
route_decision     →  route passthrough suspicious.com by local_model_router
```

Request blocked by `secret_injector` (returns error). No `request_transform` event, no `http_request`, no `http_response`.

---

## Querying the Log

The file is JSON-L (one JSON object per line), parseable with `jq`:

```bash
# Count events by type
jq -r '.event_type' events.jsonl | sort | uniq -c

# Show all plugin decisions
jq 'select(.plugin != null)' events.jsonl

# Show all blocked hosts
jq 'select(.event_type == "gate_decision" and .data.allowed == false)' events.jsonl

# Show all route redirects
jq 'select(.event_type == "route_decision" and .data.action == "redirected")' events.jsonl

# Show all secret injections
jq 'select(.event_type == "request_transform" and .data.action == "injected")' events.jsonl

# Show usage logging with costs
jq 'select(.event_type == "response_transform" and .data.action == "logged_usage")' events.jsonl

# Show only errors (non-2xx responses)
jq 'select(.event_type == "http_response" and .data.status_code >= 400)' events.jsonl

# Total response time
jq 'select(.event_type == "http_response") | .data.duration_ms' events.jsonl | paste -sd+ | bc

# Full event sequence for a single request (follow the timestamps)
jq -r '[.ts, .event_type, .summary] | @tsv' events.jsonl

# Watch live
tail -f events.jsonl | jq '.'
```

---

## File Lifecycle

- Created when `matchlock run` starts with `--event-log`.
- Events are appended one per line during the sandbox lifetime.
- File is synced and closed when the sandbox shuts down.
- File persists after the sandbox is removed.
