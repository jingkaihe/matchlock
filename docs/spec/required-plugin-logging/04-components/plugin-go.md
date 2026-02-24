# Component: pkg/policy/plugin.go

**File:** `pkg/policy/plugin.go`
**Agent:** 1 (Foundation)
**Pattern Reference:** Existing `GateVerdict` struct and `GatePlugin` interface
in the same file.

## Purpose

Add three new decision structs and change three interface signatures so that
every plugin phase returns structured decision metadata alongside its
operational result.

## Current State

- `GatePlugin` and `GateVerdict` already follow the target pattern.
- `RoutePlugin.Route()` returns `(*RouteDirective, error)`
- `RequestPlugin.TransformRequest()` returns `(*http.Request, error)`
- `ResponsePlugin.TransformResponse()` returns `(*http.Response, error)`
- `RouteDirective`, `Plugin`, `PlaceholderProvider` are unchanged.

## Changes Required

### 1. Add RouteDecision Struct

Place after `RouteDirective` (after line 111):

```go
// RouteDecision captures a routing plugin's full decision.
// Returned by RoutePlugin.Route().
type RouteDecision struct {
	Directive *RouteDirective
	Reason    string
}
```

### 2. Add RequestDecision Struct

```go
// RequestDecision captures what a request transform plugin did.
// Returned by RequestPlugin.TransformRequest().
type RequestDecision struct {
	Request *http.Request
	Action  string
	Reason  string
}
```

### 3. Add ResponseDecision Struct

```go
// ResponseDecision captures what a response transform plugin did.
// Returned by ResponsePlugin.TransformResponse().
type ResponseDecision struct {
	Response *http.Response
	Action   string
	Reason   string
}
```

### 4. Change RoutePlugin Interface

```go
// Before:
Route(req *http.Request, host string) (*RouteDirective, error)

// After:
Route(req *http.Request, host string) (*RouteDecision, error)
```

### 5. Change RequestPlugin Interface

```go
// Before:
TransformRequest(req *http.Request, host string) (*http.Request, error)

// After:
TransformRequest(req *http.Request, host string) (*RequestDecision, error)
```

### 6. Change ResponsePlugin Interface

```go
// Before:
TransformResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error)

// After:
TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error)
```

## Import Changes

None. `net/http` is already imported.

## Verification

After this change, the project will NOT compile until the plugin implementations
and engine are updated. This is expected and intentional -- the compiler enforces
the migration.
