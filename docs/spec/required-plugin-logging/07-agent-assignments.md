# 07 -- Agent Assignments

## Agent Count: 2

This feature can be implemented by 2 agents with no file conflicts. A third
agent is not needed because the test changes are tightly coupled to the plugin
changes and should be done by the same agent.

## Execution Order

Agent 1 runs first. Agent 2 depends on Agent 1's output because the interface
changes must compile before plugin implementations can be updated.

```
Agent 1 (Foundation) --> Agent 2 (Plugin Migration + Tests)
```

---

## Agent 1: Foundation

**Role:** Define new types, change interfaces, wire engine emission logic.

### Files Owned

| File | Action |
|------|--------|
| `pkg/logging/event.go` | Add 3 event type constants + 3 data structs |
| `pkg/policy/plugin.go` | Add 3 decision structs, change 3 interface signatures |
| `pkg/policy/registry.go` | Change `PluginFactory` signature, remove logging import |
| `pkg/policy/engine.go` | Add emission in `RouteRequest`, `OnRequest`, `OnResponse`; remove emitter from plugin construction |

### Specification Documents

- [03-interfaces.md](./03-interfaces.md) -- Exact Go definitions to implement
- [04-components/plugin-go.md](./04-components/plugin-go.md)
- [04-components/engine-go.md](./04-components/engine-go.md)
- [04-components/registry-go.md](./04-components/registry-go.md)
- [05-event-types.md](./05-event-types.md) -- Event constants and data structs

### Implementation Order

1. `pkg/logging/event.go` -- Add event types and data structs (no dependencies)
2. `pkg/policy/plugin.go` -- Add decision structs, change interfaces (depends on nothing)
3. `pkg/policy/registry.go` -- Change PluginFactory (depends on nothing, breaks factories)
4. `pkg/policy/engine.go` -- Wire emission + remove emitter from construction (depends on 1-3)

### Verification

After Agent 1 completes, the project will NOT compile. This is expected. The
compiler errors will be in:
- `secret_injector.go` (factory signature, TransformRequest return type)
- `local_model_router.go` (factory signature, Route return type, TransformRequest return type)
- `usage_logger.go` (factory signature, TransformResponse return type)
- `host_filter.go` (factory signature)
- All `*_test.go` files

Agent 2 resolves these.

### Agent 1 Checklist

#### Pre-Implementation
- [ ] Read [03-interfaces.md](./03-interfaces.md) and [05-event-types.md](./05-event-types.md)
- [ ] Review existing `GateVerdict` struct in `pkg/policy/plugin.go` as the pattern
- [ ] Review existing `GateDecisionData` in `pkg/logging/event.go` as the pattern
- [ ] Review existing `IsHostAllowed()` emission in `pkg/policy/engine.go` as the pattern

#### Implementation
- [ ] Add `RouteDecisionData`, `RequestTransformData`, `ResponseTransformData` to `event.go`
- [ ] Add `EventRouteDecision`, `EventRequestTransform`, `EventResponseTransform` constants
- [ ] Add deprecation comment to `EventKeyInjection`
- [ ] Add `RouteDecision`, `RequestDecision`, `ResponseDecision` structs to `plugin.go`
- [ ] Change `RoutePlugin`, `RequestPlugin`, `ResponsePlugin` interface signatures
- [ ] Change `PluginFactory` type, remove logging import from `registry.go`
- [ ] Replace `RouteRequest()` body with emission logic
- [ ] Replace `OnRequest()` body with emission logic
- [ ] Replace `OnResponse()` body with emission logic
- [ ] Remove `e.emitter` from `NewSecretInjectorPlugin()` call in `NewEngine()`
- [ ] Remove `e.emitter` from factory call in `NewEngine()`

#### Completion
- [ ] All 4 files changed, no other files touched
- [ ] Commit with descriptive message

---

## Agent 2: Plugin Migration + Tests

**Role:** Update all plugin implementations to satisfy new interfaces. Update
all test files. Verify the full pipeline compiles and passes tests.

### Files Owned

| File | Action |
|------|--------|
| `pkg/policy/secret_injector.go` | Return `*RequestDecision`, remove emitter |
| `pkg/policy/local_model_router.go` | Return `*RouteDecision` + `*RequestDecision`, remove emitter |
| `pkg/policy/usage_logger.go` | Return `*ResponseDecision`, remove emitter |
| `pkg/policy/host_filter.go` | Change factory signature |
| `pkg/policy/secret_injector_test.go` | Update for new return types, add decision tests |
| `pkg/policy/local_model_router_test.go` | Update for new return types, add decision tests |
| `pkg/policy/usage_logger_test.go` | Update for new return types, add decision tests |
| `pkg/policy/host_filter_test.go` | Update factory call |
| `pkg/policy/engine_test.go` | Add engine emission tests, update constructor calls |

### Specification Documents

- [04-components/secret-injector-go.md](./04-components/secret-injector-go.md)
- [04-components/local-model-router-go.md](./04-components/local-model-router-go.md)
- [04-components/usage-logger-go.md](./04-components/usage-logger-go.md)
- [04-components/host-filter-go.md](./04-components/host-filter-go.md)
- [04-components/http-go.md](./04-components/http-go.md)
- [06-tests.md](./06-tests.md) -- Test strategy and specific test cases

### Implementation Order

1. `pkg/policy/host_filter.go` -- Simplest: just change factory signature
2. `pkg/policy/local_model_router.go` -- Change Route() + TransformRequest()
3. `pkg/policy/secret_injector.go` -- Largest: remove emitter, rewrite TransformRequest()
4. `pkg/policy/usage_logger.go` -- Change TransformResponse()
5. `pkg/policy/host_filter_test.go` -- Update factory calls
6. `pkg/policy/local_model_router_test.go` -- Update assertions + add decision tests
7. `pkg/policy/secret_injector_test.go` -- Remove event tests, add decision tests
8. `pkg/policy/usage_logger_test.go` -- Update assertions + add decision tests
9. `pkg/policy/engine_test.go` -- Add emission tests

### Verification

After Agent 2 completes:
```bash
go build ./...      # Must succeed
go test ./...       # Must pass all tests
go vet ./...        # Must have no warnings
```

### Agent 2 Checklist

#### Pre-Implementation
- [ ] Read all component specs in `04-components/`
- [ ] Read [06-tests.md](./06-tests.md)
- [ ] Verify Agent 1's changes are committed and available
- [ ] Review existing `captureSink` in `secret_injector_test.go` for reuse

#### Implementation -- Plugins
- [ ] `host_filter.go`: Change factory signature, remove logging import
- [ ] `local_model_router.go`: Return `*RouteDecision` from `Route()`, return `*RequestDecision` from `TransformRequest()`, change factory signature, remove logging import
- [ ] `secret_injector.go`: Remove emitter field, change constructors, return `*RequestDecision`, remove all `Emit()` calls, remove logging import
- [ ] `usage_logger.go`: Return `*ResponseDecision`, change factory signature, remove logging import

#### Implementation -- Tests
- [ ] `host_filter_test.go`: Update `NewHostFilterPluginFromConfig` calls
- [ ] `local_model_router_test.go`: Update `Route()` assertions to use `decision.Directive`, update `TransformRequest()` to use `decision.Request`, add decision struct tests, update factory call
- [ ] `secret_injector_test.go`: Update all `TransformRequest()` calls to use `decision.Request`, remove 5 emitter-based tests, add decision struct tests, update all constructor calls
- [ ] `usage_logger_test.go`: Update all `TransformResponse()` calls to use `decision.Response`, add decision struct tests, update factory call
- [ ] `engine_test.go`: Add 5 engine emission tests, update `NewSecretInjectorPlugin` call in `TestEngine_OnRequest_SecretReplacement` etc.

#### Testing
- [ ] `go test ./pkg/policy/...` passes
- [ ] `go test ./pkg/logging/...` passes
- [ ] `go vet ./...` clean
- [ ] No references to `logging.Emitter` in any plugin file
- [ ] No references to `EventKeyInjection` in any assertion

#### Completion
- [ ] All files changed per spec
- [ ] Commit with descriptive message
- [ ] Run `go test ./...` from project root -- all green

---

## File Ownership Matrix

This matrix confirms no file is owned by both agents:

| File | Agent 1 | Agent 2 |
|------|:-------:|:-------:|
| `pkg/logging/event.go` | W | - |
| `pkg/policy/plugin.go` | W | - |
| `pkg/policy/registry.go` | W | - |
| `pkg/policy/engine.go` | W | - |
| `pkg/policy/host_filter.go` | - | W |
| `pkg/policy/secret_injector.go` | - | W |
| `pkg/policy/local_model_router.go` | - | W |
| `pkg/policy/usage_logger.go` | - | W |
| `pkg/policy/budget_gate.go` | - | - |
| `pkg/net/http.go` | - | - |
| `pkg/policy/engine_test.go` | - | W |
| `pkg/policy/secret_injector_test.go` | - | W |
| `pkg/policy/local_model_router_test.go` | - | W |
| `pkg/policy/usage_logger_test.go` | - | W |
| `pkg/policy/host_filter_test.go` | - | W |

W = writes to file, - = does not touch file
