# Component: pkg/net/http.go

**File:** `pkg/net/http.go`
**Agent:** 2 (Plugin Migration)
**Pattern Reference:** Current implementation in
[`pkg/net/http.go`](../../../pkg/net/http.go).

## Purpose

Adapt the HTTP interceptor to work with the engine's updated method signatures.

## Changes Required

### None

The engine's public method signatures (`RouteRequest`, `OnRequest`,
`OnResponse`) are unchanged in their return types:

| Method | Returns | Changed? |
|--------|---------|----------|
| `IsHostAllowed(host string)` | `*GateVerdict` | No |
| `RouteRequest(req, host)` | `(*RouteDirective, error)` | No |
| `OnRequest(req, host)` | `(*http.Request, error)` | No |
| `OnResponse(resp, req, host)` | `(*http.Response, error)` | No |

The engine unwraps decision structs internally and returns the same types to
callers. This was a deliberate design decision (see
[03-interfaces.md](../03-interfaces.md) section 3.3) to minimize churn in
this file.

## Verification

- `pkg/net/http.go` compiles without changes
- The HTTP interceptor's own `http_request` and `http_response` events are
  unaffected

## Note on Emitter

The `HTTPInterceptor` struct holds its own `emitter` field, received via
`NewHTTPInterceptor()`. This is separate from the engine's emitter and is used
for `http_request` / `http_response` events. This is unchanged.
