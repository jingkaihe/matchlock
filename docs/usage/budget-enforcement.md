# Budget Enforcement

Budget enforcement lets you set a maximum USD spend per sandbox session. When cumulative API costs reach the limit, all outbound requests are blocked with an HTTP 429 response.

## How It Works

Two plugins work together:

1. **`usage_logger`** -- a `ResponsePlugin` that intercepts OpenRouter API responses, extracts token counts and cost from the response body, and writes them to a JSONL log file. It maintains a running cost total in memory.

2. **`budget_gate`** -- a `GatePlugin` that checks the running cost total before each request. If `current_cost >= limit`, the request is blocked before it ever leaves the sandbox.

The budget gate runs during the `IsHostAllowed` phase, which means blocked requests never reach the upstream API. The guest application receives an HTTP 429 with a JSON error body.

## CLI Usage

Both flags are required together:

```bash
matchlock run \
  --image node:22-bookworm-slim \
  --allow-host "openrouter.ai" \
  --secret "OPENROUTER_API_KEY@openrouter.ai" \
  --usage-log-path /path/to/usage.jsonl \
  --budget-limit-usd 5.00 \
  -- node app.js
```

| Flag | Required | Description |
|------|----------|-------------|
| `--usage-log-path` | Yes (if budget is set) | Path to the JSONL file for logging API usage and costs |
| `--budget-limit-usd` | No | Maximum spend in USD. 0 = unlimited (default) |

Using `--budget-limit-usd` without `--usage-log-path` is a startup error:

```
Error: --budget-limit-usd requires --usage-log-path to be set
```

## Gate Behavior

The budget gate uses a `>=` comparison:

- If the limit is $5.00 and current spend is $4.99, the next request is **allowed**
- If the limit is $5.00 and current spend is exactly $5.00, the next request is **blocked**
- The request that pushed the total to $5.00 already completed (cost is recorded in the response phase). The gate prevents the _next_ request.

The gate applies to **all hosts**, not just API hosts. Once the budget is exceeded, all outbound requests are blocked.

## Error Response

When a request is blocked, the guest receives:

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Connection: close

{"error":{"message":"Budget limit exceeded. Spent $5.0100 of $5.00 limit.","type":"budget_exceeded","code":429}}
```

The JSON body follows the OpenAI/OpenRouter error format so LLM client applications (Claude Code, aider, etc.) can parse and display it.

## Usage Log Format

The usage log is a JSONL file (one JSON object per line). Each entry records one API response:

```json
{
  "ts": "2026-02-24T15:00:00.000Z",
  "generation_id": "gen-abc123",
  "model": "anthropic/claude-sonnet-4",
  "backend": "openrouter",
  "host": "openrouter.ai",
  "path": "/api/v1/chat/completions",
  "status_code": 200,
  "prompt_tokens": 1500,
  "completion_tokens": 250,
  "total_tokens": 1750,
  "cost_usd": 0.0125,
  "cached_tokens": null,
  "reasoning_tokens": null
}
```

Token fields are `null` (not 0) for responses from local backends (e.g., Ollama via `local_model_router`), where token counts may be unreliable. Cost is `0.0` for local backend responses.

## Session Persistence

The usage logger restores the running total from an existing log file on startup. This means:

- If you reuse the same `--usage-log-path` across sessions, spending accumulates
- To reset the budget, delete or move the usage log file before starting the sandbox
- The log file is append-only during a session

## Gate Semantics

The budget gate is one of potentially many gate plugins. The engine uses **AND semantics** for gates: all gates must allow a request for it to proceed. If any gate denies, the request is blocked.

In practice, with `host_filter` and `budget_gate` both registered:

1. `host_filter` checks if the host is in the allowlist
2. `budget_gate` checks if the budget is exceeded
3. Both must allow for the request to proceed

If the host isn't in the allowlist, `host_filter` blocks it (403). If the budget is exceeded, `budget_gate` blocks it (429). The first denier's verdict is returned to the guest.

## Supported Backends

| Backend | Cost Tracking | Token Tracking |
|---------|--------------|----------------|
| OpenRouter | Full (uses `usage.cost` from response) | Full (`prompt_tokens`, `completion_tokens`, `total_tokens`) |
| Ollama (via `local_model_router`) | $0.00 (local) | Null (tokens not reliably reported) |

Only responses to `POST /api/v1/chat/completions` or `POST /v1/chat/completions` on OpenRouter hosts are logged.

## Log Output

When the budget gate blocks a request:

```
WARN budget gate blocking request  plugin=budget_gate  host=openrouter.ai  current_cost_usd=5.0100  limit_usd=5.00
WARN gate blocked  plugin=budget_gate  host=openrouter.ai  reason="budget exceeded: $5.0100 spent of $5.00 limit"
```

On startup, if restoring from an existing log:

```
INFO restored usage total from existing log  plugin=usage_logger  path=/path/to/usage.jsonl  total_cost_usd=3.250000
INFO engine ready  gates=2  routers=0  requests=1  responses=1
```
