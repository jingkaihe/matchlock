# 07 - Agent Assignments

## Agent Strategy

This feature is decomposed into 2 agents with a strict dependency ordering. Agent 1 (Foundation) must complete before Agent 2 (Integration) begins, because Agent 2 modifies existing files that import the new `pkg/logging` package created by Agent 1.

### Why 2 Agents, Not 3

The feature is tightly coupled: the `pkg/logging` package is small (~4 files), and the integration work modifies existing files that all import the same new package. A third agent would either overlap on files or have insufficient independent work. Two agents with a clear phase boundary is optimal.

## Agent 1: Foundation (pkg/logging + api/config)

### Scope

Create the entire `pkg/logging/` package and add `LoggingConfig` to `api.Config`. This agent works exclusively on new files (no merge conflicts possible) plus one surgical addition to `pkg/api/config.go`.

### Files Created

| File | Component Spec |
|---|---|
| `pkg/logging/event.go` | [04-components/a-event-types.md](./04-components/a-event-types.md) |
| `pkg/logging/sink.go` | [04-components/b-sink-and-jsonl-writer.md](./04-components/b-sink-and-jsonl-writer.md) |
| `pkg/logging/jsonl_writer.go` | [04-components/b-sink-and-jsonl-writer.md](./04-components/b-sink-and-jsonl-writer.md) |
| `pkg/logging/emitter.go` | [04-components/c-emitter.md](./04-components/c-emitter.md) |
| `pkg/logging/errors.go` | [04-components/b-sink-and-jsonl-writer.md](./04-components/b-sink-and-jsonl-writer.md) |
| `pkg/logging/event_test.go` | [06-tests.md](./06-tests.md) |
| `pkg/logging/jsonl_writer_test.go` | [06-tests.md](./06-tests.md) |
| `pkg/logging/emitter_test.go` | [06-tests.md](./06-tests.md) |
| `pkg/logging/golden_test.go` | [06-tests.md](./06-tests.md) |
| `pkg/logging/testdata/event_full.golden` | [06-tests.md](./06-tests.md) |
| `pkg/logging/testdata/event_minimal.golden` | [06-tests.md](./06-tests.md) |

### Files Modified

| File | Change | Component Spec |
|---|---|---|
| `pkg/api/config.go` | Add `LoggingConfig` struct + `Logging` field on `Config` + `Merge()` update | [04-components/d-config-integration.md](./04-components/d-config-integration.md) |

### Execution Order

1. Create `pkg/logging/errors.go` (no deps)
2. Create `pkg/logging/event.go` (no deps)
3. Create `pkg/logging/sink.go` (depends on event.go)
4. Create `pkg/logging/jsonl_writer.go` (depends on sink.go, errors.go)
5. Create `pkg/logging/emitter.go` (depends on event.go, sink.go, errors.go)
6. Create all test files + golden files
7. Run `go test ./pkg/logging/...` -- all tests must pass
8. Add `LoggingConfig` to `pkg/api/config.go`
9. Run `go test ./pkg/api/...` -- existing tests must pass
10. Run `go vet ./pkg/logging/...` -- no issues

### Verification

```bash
go test ./pkg/logging/... -v
go test ./pkg/api/... -v
go vet ./pkg/logging/...
```

### Agent 1 Checklist

#### Pre-Implementation
- [ ] Read component specs: a-event-types, b-sink-and-jsonl-writer, c-emitter, d-config-integration
- [ ] Review codebase references: `pkg/api/vm.go:47-79`, `pkg/api/config.go`, `internal/errx`
- [ ] Verify `pkg/logging/` directory does not exist yet
- [ ] Verify `go.mod` module path: `github.com/jingkaihe/matchlock`

#### Implementation
- [ ] Create `pkg/logging/errors.go` with sentinel errors
- [ ] Create `pkg/logging/event.go` with Event struct, constants, data structs
- [ ] Create `pkg/logging/sink.go` with Sink interface
- [ ] Create `pkg/logging/jsonl_writer.go` with JSONLWriter implementation
- [ ] Create `pkg/logging/emitter.go` with Emitter and EmitterConfig
- [ ] Add `LoggingConfig` struct to `pkg/api/config.go`
- [ ] Add `Logging *LoggingConfig` field to `Config` struct
- [ ] Update `Config.Merge()` to handle `Logging` field

#### Testing
- [ ] Create `pkg/logging/event_test.go` with serialization tests
- [ ] Create `pkg/logging/jsonl_writer_test.go` with file I/O tests
- [ ] Create `pkg/logging/emitter_test.go` with metadata and dispatch tests
- [ ] Create `pkg/logging/golden_test.go` with golden file tests
- [ ] Create `pkg/logging/testdata/` with golden files (run `UPDATE_GOLDEN=1`)
- [ ] All `pkg/logging` tests pass
- [ ] All `pkg/api` tests pass (regression)

#### Completion
- [ ] `go vet ./pkg/logging/...` clean
- [ ] No `fmt.Errorf` with `%w` in `pkg/logging/` (use `errx`)

---

## Agent 2: Integration (Wire emitter through existing code + CLI)

### Scope

Modify existing files to accept, distribute, and use the `*logging.Emitter`. This agent depends on Agent 1's `pkg/logging` package being complete and tested. This agent touches multiple existing packages but each file change is surgical and well-defined.

### Files Modified

| File | Change | Component Spec |
|---|---|---|
| `pkg/policy/registry.go` | Update `PluginFactory` type signature | [04-components/e-engine-integration.md](./04-components/e-engine-integration.md) |
| `pkg/policy/engine.go` | Add emitter field, update constructor, update factory call | [04-components/e-engine-integration.md](./04-components/e-engine-integration.md) |
| `pkg/policy/host_filter.go` | Update `NewHostFilterPluginFromConfig` signature | [04-components/e-engine-integration.md](./04-components/e-engine-integration.md) |
| `pkg/policy/secret_injector.go` | Add emitter, update constructors, emit events | [04-components/g-secret-injector-integration.md](./04-components/g-secret-injector-integration.md) |
| `pkg/policy/local_model_router.go` | Update `NewLocalModelRouterPluginFromConfig` signature | [04-components/e-engine-integration.md](./04-components/e-engine-integration.md) |
| `pkg/net/http.go` | Add emitter field, update constructor, emit events | [04-components/f-http-interceptor-integration.md](./04-components/f-http-interceptor-integration.md) |
| `pkg/net/stack_darwin.go` | Add Emitter to Config, pass to interceptor | [04-components/f-http-interceptor-integration.md](./04-components/f-http-interceptor-integration.md) |
| `pkg/net/proxy.go` | Add Emitter to ProxyConfig, pass to interceptor | [04-components/f-http-interceptor-integration.md](./04-components/f-http-interceptor-integration.md) |
| `pkg/sandbox/sandbox_darwin.go` | Construct emitter, wire through, close in Close() | [04-components/d-config-integration.md](./04-components/d-config-integration.md) |
| `pkg/sandbox/sandbox_linux.go` | Construct emitter, wire through, close in Close() | [04-components/d-config-integration.md](./04-components/d-config-integration.md) |
| `cmd/matchlock/cmd_run.go` | Add CLI flags, construct LoggingConfig | [04-components/h-cli-flags.md](./04-components/h-cli-flags.md) |
| `pkg/policy/engine_test.go` | Update calls to pass nil emitter | Regression |
| `pkg/policy/secret_injector_test.go` | Update calls + add emission tests | [06-tests.md](./06-tests.md) |
| `pkg/policy/registry_test.go` | Update factory test if needed | Regression |

### Execution Order

The order matters because of compilation dependencies:

1. **Policy layer first** (enables `pkg/net` to import `logging` via `policy`):
   a. Update `pkg/policy/registry.go` -- `PluginFactory` signature
   b. Update `pkg/policy/engine.go` -- add emitter, update constructor
   c. Update `pkg/policy/host_filter.go` -- factory signature
   d. Update `pkg/policy/local_model_router.go` -- factory signature
   e. Update `pkg/policy/secret_injector.go` -- full integration
   f. Run `go build ./pkg/policy/...` -- must compile
   g. Update test files in `pkg/policy/` -- all must pass

2. **Network layer** (depends on policy compiling):
   a. Update `pkg/net/http.go` -- add emitter, emit events
   b. Update `pkg/net/stack_darwin.go` -- Config struct
   c. Update `pkg/net/proxy.go` -- ProxyConfig struct
   d. Run `go build ./pkg/net/...` -- must compile

3. **Sandbox layer** (depends on both policy and net):
   a. Update `pkg/sandbox/sandbox_darwin.go` -- construct emitter, wire, close
   b. Update `pkg/sandbox/sandbox_linux.go` -- construct emitter, wire, close
   c. Run `go build ./pkg/sandbox/...` -- must compile

4. **CLI layer** (depends on api.Config):
   a. Update `cmd/matchlock/cmd_run.go` -- add flags, construct config
   b. Run `go build ./cmd/matchlock/...` -- must compile

5. **Full test suite:**
   ```bash
   go test ./pkg/policy/... -v
   go test ./pkg/logging/... -v
   go build ./...
   ```

### Verification

```bash
# Compilation check (all packages)
go build ./...

# Unit tests for modified packages
go test ./pkg/policy/... -v
go test ./pkg/net/... -v

# Full test suite
mise run test
```

### Agent 2 Checklist

#### Pre-Implementation
- [ ] Verify Agent 1 is complete: `go test ./pkg/logging/...` passes
- [ ] Read component specs: e-engine-integration, f-http-interceptor-integration, g-secret-injector-integration, h-cli-flags
- [ ] Review all codebase references cited in specs
- [ ] Confirm file paths don't conflict with Agent 1

#### Implementation (Policy Layer)
- [ ] Update `PluginFactory` type in `pkg/policy/registry.go`
- [ ] Add emitter to `Engine` struct and `NewEngine` in `pkg/policy/engine.go`
- [ ] Update all three factory functions (`host_filter`, `secret_injector`, `local_model_router`)
- [ ] Add emitter to `secretInjectorPlugin`, emit `key_injection` events
- [ ] `go build ./pkg/policy/...` compiles

#### Implementation (Network Layer)
- [ ] Add emitter to `HTTPInterceptor` and `NewHTTPInterceptor` in `pkg/net/http.go`
- [ ] Emit `llm_request` and `llm_response` in `HandleHTTPS` and `HandleHTTP`
- [ ] Add `Emitter` field to `Config` in `pkg/net/stack_darwin.go`
- [ ] Add `Emitter` field to `ProxyConfig` in `pkg/net/proxy.go`
- [ ] Update interceptor creation calls to pass emitter
- [ ] `go build ./pkg/net/...` compiles

#### Implementation (Sandbox Layer)
- [ ] Add emitter construction to `sandbox_darwin.go` `New()`
- [ ] Add emitter construction to `sandbox_linux.go` `New()`
- [ ] Add `emitter` field to `Sandbox` struct (both platforms)
- [ ] Close emitter in `Sandbox.Close()` (both platforms)
- [ ] Wire emitter to `NewEngine` and network config
- [ ] `go build ./pkg/sandbox/...` compiles

#### Implementation (CLI Layer)
- [ ] Add `--event-log`, `--run-id`, `--agent-system` flags
- [ ] Add viper bindings
- [ ] Construct `LoggingConfig` from flags
- [ ] `go build ./cmd/matchlock/...` compiles

#### Testing
- [ ] Update `pkg/policy/engine_test.go` -- pass nil emitter to `NewEngine`
- [ ] Update `pkg/policy/secret_injector_test.go` -- pass nil emitter to constructors
- [ ] Add secret injector emission tests with `captureSink`
- [ ] All `pkg/policy` tests pass
- [ ] `go build ./...` succeeds (full project)

#### Completion
- [ ] `go vet ./...` clean
- [ ] No unintended behavioral changes when emitter is nil
- [ ] `mise run test` passes

### Post-Implementation Archive
- [ ] Move spec to archive: `mv docs/spec/policy-engine-logging docs/archive/specs/`
- [ ] Move related draft: `mv docs/draft/policy-engine-logging.md docs/archive/drafts/`
- [ ] Include archive moves in final PR
