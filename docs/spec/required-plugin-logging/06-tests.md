# 06 -- Tests

**Agent:** 2 (Plugin Migration + Tests)

All test changes are assigned to Agent 2 because they depend on the new
interface signatures (Agent 1) being complete first. Agent 2 should implement
plugin changes and tests together.

## Test Strategy Overview

The migration fundamentally simplifies plugin testing:

| Before | After |
|--------|-------|
| Plugin tests set up mock emitters | Plugin tests assert on returned decision structs |
| Engine tests check slog output | Engine tests use `captureSink` to verify events |
| Event emission tested indirectly | Event emission tested directly via captured events |

## Existing Test Patterns to Follow

**Pattern Reference:** `captureSink` in
[`pkg/policy/secret_injector_test.go`](../../pkg/policy/secret_injector_test.go)
lines 19-38.

The `captureSink` type already exists in the test file and should be reused
(or moved to a shared test helper if needed by engine tests). It implements
`logging.Sink` and captures events in memory.

**Pattern Reference:** Table-driven tests throughout
[`pkg/policy/engine_test.go`](../../pkg/policy/engine_test.go) and
[`pkg/policy/host_filter_test.go`](../../pkg/policy/host_filter_test.go).

**Pattern Reference:** Mock plugins (e.g., `mockGatePlugin`) in
[`pkg/policy/engine_test.go`](../../pkg/policy/engine_test.go) lines 19-25.

## File-by-File Test Changes

### 6.1 pkg/policy/secret_injector_test.go

**What changes:** The `TransformRequest()` return type changes from
`(*http.Request, error)` to `(*RequestDecision, error)`. All call sites in
tests must be updated to unwrap the decision.

#### Updated Existing Tests

Every test that calls `p.TransformRequest()` changes from:

```go
// Before:
result, err := p.TransformRequest(req, "api.example.com")
require.NoError(t, err)
assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))

// After:
decision, err := p.TransformRequest(req, "api.example.com")
require.NoError(t, err)
assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
```

#### New Decision Struct Tests

```go
func TestSecretInjectorPlugin_Decision_Injected(t *testing.T) {
    // Setup: plugin with one secret scoped to api.example.com
    // Input: request with placeholder in Authorization header, host=api.example.com
    // Assert:
    //   decision.Action == "injected"
    //   decision.Reason contains "1 secret(s) injected"
    //   decision.Request is non-nil
    //   decision.Request.Header.Get("Authorization") contains real secret
}

func TestSecretInjectorPlugin_Decision_Skipped(t *testing.T) {
    // Setup: plugin with one secret scoped to api.example.com
    // Input: request without placeholder, host=other.com
    // Assert:
    //   decision.Action == "skipped"
    //   decision.Reason contains "1 secret(s) skipped"
    //   decision.Request is non-nil
}

func TestSecretInjectorPlugin_Decision_NoOp(t *testing.T) {
    // Setup: plugin with no secrets (empty map)
    // Input: any request
    // Assert:
    //   decision.Action == "no_op"
    //   decision.Request is non-nil
}

func TestSecretInjectorPlugin_Decision_LeakReturnsError(t *testing.T) {
    // Setup: plugin with secret scoped to api.example.com
    // Input: request with placeholder, host=evil.com
    // Assert:
    //   err is api.ErrSecretLeak
    //   decision is nil
}

func TestSecretInjectorPlugin_Decision_NoSecretValues(t *testing.T) {
    // Setup: plugin with secret value "super-secret-value-12345"
    // Input: request with placeholder, host=api.example.com
    // Assert:
    //   decision.Reason does NOT contain "super-secret-value-12345"
    //   decision.Reason does NOT contain the placeholder string
}
```

#### Removed Tests

The following tests are removed because the plugin no longer emits events:

- `TestSecretInjectorPlugin_EmitsInjectedEvent`
- `TestSecretInjectorPlugin_EmitsSkippedEvent`
- `TestSecretInjectorPlugin_EmitsLeakBlockedEvent`
- `TestSecretInjectorPlugin_NilEmitterNoEvents`
- `TestSecretInjectorPlugin_NoSecretValuesInEvents`

These are replaced by the decision struct tests above and engine-level event
emission tests.

#### Updated Constructor Calls

All calls to `NewSecretInjectorPlugin` and `NewSecretInjectorPluginFromConfig`
lose the `emitter` parameter:

```go
// Before:
p := NewSecretInjectorPlugin(secrets, nil, nil)
p := NewSecretInjectorPlugin(secrets, nil, emitter)
plugin, err := NewSecretInjectorPluginFromConfig(raw, nil, nil)

// After:
p := NewSecretInjectorPlugin(secrets, nil)
plugin, err := NewSecretInjectorPluginFromConfig(raw, nil)
```

### 6.2 pkg/policy/local_model_router_test.go

**What changes:** `Route()` returns `(*RouteDecision, error)` instead of
`(*RouteDirective, error)`. `TransformRequest()` returns
`(*RequestDecision, error)` instead of `(*http.Request, error)`.

#### Updated Existing Tests

Every test that calls `p.Route()` changes from:

```go
// Before:
directive, err := p.Route(req, "openrouter.ai")
require.NotNil(t, directive)
assert.Equal(t, "127.0.0.1", directive.Host)

// After:
decision, err := p.Route(req, "openrouter.ai")
require.NotNil(t, decision.Directive)
assert.Equal(t, "127.0.0.1", decision.Directive.Host)
```

For nil directive (passthrough) cases:

```go
// Before:
directive, err := p.Route(req, "other-api.com")
assert.Nil(t, directive)

// After:
decision, err := p.Route(req, "other-api.com")
assert.Nil(t, decision.Directive)
assert.NotEmpty(t, decision.Reason)
```

For `TransformRequest()`:

```go
// Before:
result, err := p.TransformRequest(req, "example.com")
assert.Equal(t, req, result)

// After:
decision, err := p.TransformRequest(req, "example.com")
assert.Equal(t, req, decision.Request)
assert.Equal(t, "no_op", decision.Action)
```

#### New Decision Struct Tests

```go
func TestLocalModelRouterPlugin_Decision_Redirected(t *testing.T) {
    // Setup: router with matching model
    // Assert:
    //   decision.Directive is non-nil
    //   decision.Reason contains "matched model"
}

func TestLocalModelRouterPlugin_Decision_Passthrough_WrongHost(t *testing.T) {
    // Setup: router, request to non-matching host
    // Assert:
    //   decision.Directive is nil
    //   decision.Reason contains "no route entry"
}

func TestLocalModelRouterPlugin_Decision_Passthrough_NoMatch(t *testing.T) {
    // Setup: router, request with non-matching model
    // Assert:
    //   decision.Directive is nil
    //   decision.Reason contains "no matching route"
}

func TestLocalModelRouterPlugin_Decision_TransformRequestNoOp(t *testing.T) {
    // Assert:
    //   decision.Action == "no_op"
    //   decision.Request == original request
}
```

#### Updated Factory Test

```go
// Before:
plugin, err := NewLocalModelRouterPluginFromConfig(raw, nil, nil)

// After:
plugin, err := NewLocalModelRouterPluginFromConfig(raw, nil)
```

### 6.3 pkg/policy/usage_logger_test.go

**What changes:** `TransformResponse()` returns `(*ResponseDecision, error)`
instead of `(*http.Response, error)`.

#### Updated Existing Tests

Every test that calls `p.TransformResponse()` changes from:

```go
// Before:
result, err := p.TransformResponse(resp, req, "openrouter.ai")
assert.Equal(t, resp, result)

// After:
decision, err := p.TransformResponse(resp, req, "openrouter.ai")
assert.Equal(t, resp, decision.Response)
```

#### New Decision Struct Tests

```go
func TestUsageLoggerPlugin_Decision_LoggedUsage(t *testing.T) {
    // Setup: valid OpenRouter response
    // Assert:
    //   decision.Action == "logged_usage"
    //   decision.Reason contains "recorded $"
    //   decision.Reason contains model name
}

func TestUsageLoggerPlugin_Decision_NoOp_WrongHost(t *testing.T) {
    // Assert: decision.Action == "no_op"
}

func TestUsageLoggerPlugin_Decision_NoOp_WrongPath(t *testing.T) {
    // Assert: decision.Action == "no_op"
}

func TestUsageLoggerPlugin_Decision_NoOp_Non200(t *testing.T) {
    // Assert: decision.Action == "no_op"
}
```

#### Updated Factory Test

```go
// Before:
plugin, err := NewUsageLoggerPluginFromConfig(raw, nil, nil)

// After:
plugin, err := NewUsageLoggerPluginFromConfig(raw, nil)
```

### 6.4 pkg/policy/host_filter_test.go

**What changes:** Factory call signature only.

```go
// Before:
plugin, err := NewHostFilterPluginFromConfig(raw, nil, nil)

// After:
plugin, err := NewHostFilterPluginFromConfig(raw, nil)
```

No other changes. `Gate()` return type is unchanged.

### 6.5 pkg/policy/engine_test.go

**What changes:** Multiple areas.

#### Updated NewEngine Calls

No change -- `NewEngine` signature is unchanged.

#### Updated OnRequest Assertions

Tests that call `engine.OnRequest()` continue to receive `(*http.Request, error)`
because the engine unwraps the decision internally. Most engine tests are
unchanged in their assertions.

#### Updated RouteRequest Assertions

Tests that call `engine.RouteRequest()` continue to receive
`(*RouteDirective, error)`. No changes needed.

#### New Engine Event Emission Tests

These test that the engine emits events when calling plugins. Use `captureSink`
(move it to engine_test.go or keep in secret_injector_test.go -- both files are
in `package policy`).

```go
func TestEngine_RouteRequest_EmitsRouteDecisionEvent(t *testing.T) {
    // Setup: engine with local_model_router, captureSink emitter
    // Action: RouteRequest with matching model
    // Assert:
    //   captureSink has 1 event with EventType == "route_decision"
    //   event.Plugin == "local_model_router"
    //   data.Action == "redirected"
    //   data.RoutedTo contains backend host:port
}

func TestEngine_RouteRequest_EmitsPassthroughEvent(t *testing.T) {
    // Setup: engine with local_model_router, captureSink emitter
    // Action: RouteRequest with non-matching model
    // Assert:
    //   captureSink has 1 event with EventType == "route_decision"
    //   data.Action == "passthrough"
}

func TestEngine_OnRequest_EmitsRequestTransformEvent(t *testing.T) {
    // Setup: engine with secret_injector, captureSink emitter
    // Action: OnRequest with matching secret
    // Assert:
    //   captureSink has 1 event with EventType == "request_transform"
    //   event.Plugin == "secret_injector"
    //   data.Action == "injected"
}

func TestEngine_OnResponse_EmitsResponseTransformEvent(t *testing.T) {
    // Setup: engine with usage_logger, captureSink emitter
    // Action: OnResponse with valid OpenRouter response
    // Assert:
    //   captureSink has 1 event with EventType == "response_transform"
    //   event.Plugin == "usage_logger"
    //   data.Action == "logged_usage"
}

func TestEngine_NilEmitter_NoEventsPanic(t *testing.T) {
    // Setup: engine with nil emitter
    // Action: Run full pipeline (gate, route, request, response)
    // Assert: No panic, all operations succeed
}
```

## Test Execution Order

Tests should be runnable in any order. No shared state between test functions.

```bash
# Run all policy tests
go test ./pkg/policy/...

# Run all logging tests
go test ./pkg/logging/...

# Run specific test file
go test ./pkg/policy/ -run TestSecretInjectorPlugin
go test ./pkg/policy/ -run TestEngine_RouteRequest_Emits
```

## Acceptance Criteria

1. All existing tests pass after migration (with updated signatures)
2. New decision struct tests cover all Action values for each plugin
3. Engine emission tests verify event type, plugin name, and data fields
4. No test references `logging.Emitter` in plugin constructor calls
5. No test references `EventKeyInjection` in assertions
6. `go test ./...` passes with zero failures from the project root
