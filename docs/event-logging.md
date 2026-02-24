# Event Logging

Matchlock can write a persistent JSON-L event log capturing network activity through the policy engine. Each line in the log file is a single JSON object representing one event.

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
| `--event-log` | Yes (to enable) | — | Path to the JSON-L output file. Enables event logging. |
| `--agent-system` | No | `""` | Label identifying the agent system (e.g., `openclaw`, `aider`). |
| `--run-id` | No | sandbox VM ID | Session identifier. Defaults to the VM ID (e.g., `vm-a1b2c3d4`). |

---

## Event Schema

Every event has the same top-level shape:

```json
{
  "ts":           "2026-02-24T17:49:40.619586Z",
  "run_id":       "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type":   "http_request",
  "summary":      "POST openrouter.ai/api/v1/chat/completions",
  "plugin":       "secret_injector",
  "tags":         ["tls"],
  "data":         { ... }
}
```

### Required fields (present on every event)

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | RFC 3339 timestamp with sub-second precision. |
| `run_id` | string | Session/run identifier. Defaults to the sandbox VM ID. |
| `agent_system` | string | The agent system label set at startup. |
| `event_type` | string | One of the event types listed below. |
| `summary` | string | Human-readable one-line description. |

### Optional fields (present when applicable)

| Field | Type | Description |
|-------|------|-------------|
| `plugin` | string | Which policy plugin emitted the event. |
| `tags` | string[] | Arbitrary tags for filtering (e.g., `"tls"`, `"http"`). |
| `data` | object | Structured payload. Shape depends on `event_type`. |

---

## Event Types

### `gate_decision`

**Source:** Policy engine (`pkg/policy/engine.go`), triggered by `host_filter` plugin

Emitted when the policy engine evaluates whether an outbound connection to a host is allowed or blocked. One event per gate plugin evaluation.

**Triggers:**

| Decision | When |
|----------|------|
| `allowed=true` | Host matched the allowlist or no allowlist is configured. |
| `allowed=false` | Host is not in the allowlist, or is a private IP that is not explicitly allowed. |

**`data` shape:**

```json
{
  "host": "openrouter.ai",
  "allowed": true,
  "reason": "",
  "pattern": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `host` | string | The hostname being evaluated. |
| `allowed` | bool | Whether the connection was permitted. |
| `reason` | string | Why the host was blocked (e.g., `"host not in allowlist"`, `"private IP blocked"`). Empty if allowed. |
| `pattern` | string | The allowlist pattern that matched, if allowed. Empty if blocked. |

**Example events:**

```json
{
  "ts": "2026-02-24T17:49:40.619000Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "gate_decision",
  "summary": "gate allowed openrouter.ai by host_filter",
  "plugin": "host_filter",
  "data": {
    "host": "openrouter.ai",
    "allowed": true
  }
}
```

```json
{
  "ts": "2026-02-24T17:49:40.619000Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "gate_decision",
  "summary": "gate blocked evil.com by host_filter: host not in allowlist",
  "plugin": "host_filter",
  "data": {
    "host": "evil.com",
    "allowed": false,
    "reason": "host not in allowlist"
  }
}
```

---

### `key_injection`

**Source:** `secret_injector` plugin (`pkg/policy/secret_injector.go`)

Emitted when the secret injector processes an outbound request against a configured secret. One event per secret per request.

**Triggers:**

| Action | When |
|--------|------|
| `"injected"` | Secret placeholder was replaced with the real value for an allowed host. |
| `"skipped"` | Secret exists but is not allowed for this host. Request passes through unchanged. |
| `"leak_blocked"` | Request to a disallowed host contains a secret placeholder. Request is blocked. |

**`data` shape:**

```json
{
  "secret_name": "OPENROUTER_API_KEY",
  "host": "openrouter.ai",
  "action": "injected"
}
```

| Field | Type | Values | Description |
|-------|------|--------|-------------|
| `secret_name` | string | — | Environment variable name of the secret. Never the secret value. |
| `host` | string | — | Target hostname of the request. |
| `action` | string | `"injected"`, `"skipped"`, `"leak_blocked"` | What the injector did. |

**Example event:**

```json
{
  "ts": "2026-02-24T17:49:40.619586Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "key_injection",
  "summary": "secret \"OPENROUTER_API_KEY\" injected for openrouter.ai",
  "plugin": "secret_injector",
  "data": {
    "secret_name": "OPENROUTER_API_KEY",
    "host": "openrouter.ai",
    "action": "injected"
  }
}
```

---

### `http_request`

**Source:** HTTP interceptor (`pkg/net/http.go`)

Emitted when an outbound HTTP or HTTPS request to an allowed host is forwarded upstream (or to a local model backend). Emitted after gate decisions, secret injection, and routing, before the request leaves matchlock.

**`data` shape:**

```json
{
  "method": "POST",
  "host": "openrouter.ai",
  "path": "/api/v1/chat/completions",
  "model": "anthropic/claude-3.5-haiku",
  "routed": false,
  "routed_to": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `method` | string | HTTP method (`GET`, `POST`, etc.). |
| `host` | string | Target hostname. |
| `path` | string | Request path. |
| `model` | string | Model name if detected from the request body. Empty if not applicable. |
| `routed` | bool | `true` if the request was redirected to a local model backend. |
| `routed_to` | string | `host:port` of the local backend, if routed. Empty otherwise. |

**Tags:** `["tls"]` for HTTPS requests, `["http"]` for plain HTTP.

**Example event:**

```json
{
  "ts": "2026-02-24T17:49:40.619659Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "http_request",
  "summary": "POST openrouter.ai/api/v1/chat/completions",
  "tags": ["tls"],
  "data": {
    "method": "POST",
    "host": "openrouter.ai",
    "path": "/api/v1/chat/completions",
    "routed": false
  }
}
```

---

### `http_response`

**Source:** HTTP interceptor (`pkg/net/http.go`)

Emitted when the upstream response is received and buffered, before it is forwarded back to the guest VM. Always paired with a preceding `http_request`.

**`data` shape:**

```json
{
  "method": "POST",
  "host": "openrouter.ai",
  "path": "/api/v1/chat/completions",
  "status_code": 200,
  "duration_ms": 1234,
  "body_bytes": 8192,
  "model": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `method` | string | HTTP method of the original request. |
| `host` | string | Target hostname. |
| `path` | string | Request path. |
| `status_code` | int | HTTP response status code. |
| `duration_ms` | int | Round-trip time in milliseconds (request sent to response received). |
| `body_bytes` | int | Response body size in bytes. |
| `model` | string | Model name if detected. Empty if not applicable. |

**Tags:** `["tls"]` for HTTPS, `["http"]` for plain HTTP.

**Example event:**

```json
{
  "ts": "2026-02-24T17:49:40.675155Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "http_response",
  "summary": "POST openrouter.ai/api/v1/chat/completions -> 200 (1234ms)",
  "tags": ["tls"],
  "data": {
    "method": "POST",
    "host": "openrouter.ai",
    "path": "/api/v1/chat/completions",
    "status_code": 200,
    "duration_ms": 1234,
    "body_bytes": 8192
  }
}
```

---

### `budget_action`

**Source:** None (v0 placeholder)

Defined for future use by a budget/cost tracking plugin. The struct is available but no code emits this event type yet.

**`data` shape (planned):**

```json
{
  "action": "charge",
  "tokens_used": 1500,
  "cost_usd": 0.0023,
  "remaining": 4.97
}
```

| Field | Type | Description |
|-------|------|-------------|
| `action` | string | Budget action taken. |
| `tokens_used` | int | Token count for this action. |
| `cost_usd` | float | Cost in USD. |
| `remaining` | float | Remaining budget. |

---

## Typical Event Sequence

A single outbound API call through matchlock produces this sequence:

```
gate_decision  →  gate allowed openrouter.ai by host_filter
key_injection  →  secret "OPENROUTER_API_KEY" injected for openrouter.ai
http_request   →  POST openrouter.ai/api/v1/chat/completions
http_response  →  POST openrouter.ai/api/v1/chat/completions -> 200 (1234ms)
```

If the request is routed to a local model backend:

```
gate_decision  →  gate allowed openrouter.ai by host_filter
key_injection  →  secret "OPENROUTER_API_KEY" injected for openrouter.ai
http_request   →  POST openrouter.ai/api/v1/chat/completions  (routed=true, routed_to=192.168.1.10:11434)
http_response  →  POST openrouter.ai/api/v1/chat/completions -> 200 (350ms)
```

If the host is blocked by the allowlist:

```
gate_decision  →  gate blocked evil.com by host_filter: host not in allowlist
```

The connection is refused; no further events follow.

If a secret leak is detected (placeholder sent to a disallowed host):

```
gate_decision  →  gate allowed suspicious.com by host_filter
key_injection  →  secret "OPENROUTER_API_KEY" leak blocked for suspicious.com
```

The request is blocked by the secret injector; no `http_request` or `http_response` follows.

---

## Querying the Log

The file is JSON-L (one JSON object per line), parseable with `jq`:

```bash
# Count events by type
jq -r '.event_type' events.jsonl | sort | uniq -c

# Show all HTTP requests
jq 'select(.event_type == "http_request")' events.jsonl

# Total response time across all requests
jq 'select(.event_type == "http_response") | .data.duration_ms' events.jsonl | paste -sd+ | bc

# Filter by host
jq 'select(.data.host == "openrouter.ai")' events.jsonl

# Show only errors (non-2xx responses)
jq 'select(.event_type == "http_response" and .data.status_code >= 400)' events.jsonl

# Show blocked hosts
jq 'select(.event_type == "gate_decision" and .data.allowed == false)' events.jsonl

# Watch live
tail -f events.jsonl | jq '.'
```

---

## File Lifecycle

- Created when `matchlock run` starts with `--event-log`.
- Events are appended one per line during the sandbox lifetime.
- File is synced and closed when the sandbox shuts down.
- File persists after the sandbox is removed.
