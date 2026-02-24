# Component F: HTTP Interceptor Integration

## Purpose

Add `llm_request` and `llm_response` event emission to `HTTPInterceptor.HandleHTTPS()` and `HandleHTTP()`. These events capture every LLM API call passing through the TLS MITM proxy along with timing, routing, and response metadata.

## Codebase References

- **Existing Interceptor:** `pkg/net/http.go:18-37` -- `HTTPInterceptor` struct and constructor
- **HandleHTTPS:** `pkg/net/http.go:141-293` -- the TLS interception flow where events are emitted
- **HandleHTTP:** `pkg/net/http.go:39-139` -- the plain HTTP interception flow
- **Existing slog calls:** Lines 267-282 -- the `i.logger.Info(...)` calls that the new events sit alongside
- **Existing api.Event emission:** Lines 295-330 -- `emitEvent` and `emitBlockedEvent` patterns

## File Locations

- `pkg/net/http.go` -- modify `HTTPInterceptor`, `NewHTTPInterceptor`, `HandleHTTPS`, `HandleHTTP`
- `pkg/net/stack_darwin.go` -- pass emitter through `Config`
- `pkg/net/proxy.go` -- pass emitter through `ProxyConfig`

## Changes Required

### 1. Add emitter field to HTTPInterceptor

```go
type HTTPInterceptor struct {
    policy   *policy.Engine
    events   chan api.Event
    caPool   *CAPool
    connPool *upstreamConnPool
    logger   *slog.Logger
    emitter  *logging.Emitter  // NEW
}
```

### 2. Update constructor

```go
func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event, caPool *CAPool, logger *slog.Logger, emitter *logging.Emitter) *HTTPInterceptor {
    if logger == nil {
        logger = slog.Default()
    }
    return &HTTPInterceptor{
        policy:   pol,
        events:   events,
        caPool:   caPool,
        connPool: newUpstreamConnPool(),
        logger:   logger.With("component", "net"),
        emitter:  emitter,  // NEW: may be nil
    }
}
```

### 3. Emit llm_request in HandleHTTPS

After the routing decision (line 177) but before forwarding upstream, emit the request event:

```go
// After: routeDirective, err := i.policy.RouteRequest(req, serverName)
// After: modifiedReq, err := i.policy.OnRequest(req, effectiveHost)
// Before: upstream connection

if i.emitter != nil {
    data := &logging.LLMRequestData{
        Method: req.Method,
        Host:   serverName,
        Path:   req.URL.Path,
        Routed: routeDirective != nil,
    }
    if routeDirective != nil {
        data.RoutedTo = fmt.Sprintf("%s:%d", routeDirective.Host, routeDirective.Port)
    }
    summary := fmt.Sprintf("%s %s%s", req.Method, serverName, req.URL.Path)
    _ = i.emitter.Emit(logging.EventLLMRequest, summary, "", []string{"tls"}, data)
}
```

### 4. Emit llm_response in HandleHTTPS

After the response is buffered and timing is captured (around line 265), emit the response event:

```go
// After: duration := time.Since(start)
// Before: the existing i.logger.Info(...) calls

if i.emitter != nil {
    data := &logging.LLMResponseData{
        Method:     req.Method,
        Host:       serverName,
        Path:       req.URL.Path,
        StatusCode: modifiedResp.StatusCode,
        DurationMS: duration.Milliseconds(),
        BodyBytes:  int64(len(body)),
    }
    summary := fmt.Sprintf("%s %s%s -> %d (%dms)",
        req.Method, serverName, req.URL.Path,
        modifiedResp.StatusCode, duration.Milliseconds())
    _ = i.emitter.Emit(logging.EventLLMResponse, summary, "", []string{"tls"}, data)
}
```

### 5. Emit events in HandleHTTP

Apply the same pattern to `HandleHTTP` (plain HTTP interception). The events are identical except the tag is `["http"]` instead of `["tls"]`:

**llm_request** -- after `OnRequest` but before forwarding:

```go
if i.emitter != nil {
    data := &logging.LLMRequestData{
        Method: req.Method,
        Host:   host,
        Path:   req.URL.Path,
        Routed: false, // HTTP path does not do routing
    }
    summary := fmt.Sprintf("%s %s%s", req.Method, host, req.URL.Path)
    _ = i.emitter.Emit(logging.EventLLMRequest, summary, "", []string{"http"}, data)
}
```

**llm_response** -- after response buffering:

```go
if i.emitter != nil {
    data := &logging.LLMResponseData{
        Method:     req.Method,
        Host:       host,
        Path:       req.URL.Path,
        StatusCode: modifiedResp.StatusCode,
        DurationMS: duration.Milliseconds(),
        BodyBytes:  int64(len(body)),
    }
    summary := fmt.Sprintf("%s %s%s -> %d (%dms)",
        req.Method, host, req.URL.Path,
        modifiedResp.StatusCode, duration.Milliseconds())
    _ = i.emitter.Emit(logging.EventLLMResponse, summary, "", []string{"http"}, data)
}
```

### 6. Update network config structs

**`pkg/net/stack_darwin.go`** -- add `Emitter` to `Config`:

```go
type Config struct {
    // ... existing fields ...
    Emitter    *logging.Emitter  // NEW
}
```

Update the `NewNetworkStack` call to pass emitter to interceptor:

```go
// Line 318 currently:
ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool, cfg.Logger)

// New:
ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool, cfg.Logger, cfg.Emitter)
```

**`pkg/net/proxy.go`** -- add `Emitter` to `ProxyConfig`:

```go
type ProxyConfig struct {
    // ... existing fields ...
    Emitter    *logging.Emitter  // NEW
}
```

Update interceptor creation:

```go
// Line 91 currently:
interceptor: NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool, cfg.Logger),

// New:
interceptor: NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool, cfg.Logger, cfg.Emitter),
```

## Dependencies

- `github.com/jingkaihe/matchlock/pkg/logging`
- All existing imports unchanged

## Test Criteria

1. **Nil emitter:** All existing behavior is preserved when emitter is nil
2. **HandleHTTPS llm_request:** Event emitted with correct method, host, path, routed flag
3. **HandleHTTPS llm_response:** Event emitted with correct status code, duration, body bytes
4. **HandleHTTP events:** Same structure as HTTPS but with `["http"]` tag
5. **Routing metadata:** When `routeDirective` is non-nil, `RoutedTo` field is populated
6. **Tags:** HTTPS events have `["tls"]` tag, HTTP events have `["http"]` tag
7. **Error discarded:** Emission errors do not affect request/response flow

## Acceptance Criteria

- [ ] `HTTPInterceptor` has `emitter *logging.Emitter` field
- [ ] `NewHTTPInterceptor` accepts `*logging.Emitter` parameter
- [ ] `HandleHTTPS` emits `llm_request` after routing + secret injection
- [ ] `HandleHTTPS` emits `llm_response` after response buffering
- [ ] `HandleHTTP` emits both events
- [ ] Events use `_ = i.emitter.Emit(...)` (error discarded)
- [ ] Events include correct tags (`["tls"]` or `["http"]`)
- [ ] Network config structs updated with `Emitter` field
- [ ] Existing `slog` calls and `api.Event` emissions are unchanged
