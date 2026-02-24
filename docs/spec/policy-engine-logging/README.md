# Policy Engine Logging Standard -- Specification

Implementation-ready specification for adding persistent, structured JSON-L event logging to matchlock's network policy engine.

## Quick Reference

| | |
|---|---|
| **Feature** | Structured event logging (JSON-L) for policy engine activity |
| **Branch** | `feat/policy-engine-logging` |
| **Draft** | `docs/draft/policy-engine-logging.md` |
| **Agents** | 2 (Foundation + Integration) |
| **New Package** | `pkg/logging/` |
| **New Files** | 11 |
| **Modified Files** | 15 |
| **Estimated Lines** | ~850 |

## Specification Files

| File | Purpose |
|---|---|
| [01-overview.md](./01-overview.md) | Feature summary, scope, key decisions, file inventory |
| [02-architecture.md](./02-architecture.md) | Component diagram, data flow, dependency graph, concurrency model |
| [03-interfaces.md](./03-interfaces.md) | All Go interfaces, structs, and modified constructor signatures |
| [04-components/](./04-components/) | Per-component implementation specs (see below) |
| [05-factories.md](./05-factories.md) | Factory pattern usage and rationale |
| [06-tests.md](./06-tests.md) | Complete test code, golden files, regression requirements |
| [07-agent-assignments.md](./07-agent-assignments.md) | Agent work distribution with checklists |
| [08-changelog.md](./08-changelog.md) | What changed from draft to spec |
| [09-archive-instructions.md](./09-archive-instructions.md) | Post-implementation archiving steps |

## Component Specs

| Component | File | Agent |
|---|---|---|
| A. Event Types & Data Structs | [a-event-types.md](./04-components/a-event-types.md) | 1 |
| B. Sink Interface & JSONLWriter | [b-sink-and-jsonl-writer.md](./04-components/b-sink-and-jsonl-writer.md) | 1 |
| C. Emitter | [c-emitter.md](./04-components/c-emitter.md) | 1 |
| D. Config Integration | [d-config-integration.md](./04-components/d-config-integration.md) | 1 (config) + 2 (sandbox wiring) |
| E. Engine Integration | [e-engine-integration.md](./04-components/e-engine-integration.md) | 2 |
| F. HTTP Interceptor Integration | [f-http-interceptor-integration.md](./04-components/f-http-interceptor-integration.md) | 2 |
| G. Secret Injector Integration | [g-secret-injector-integration.md](./04-components/g-secret-injector-integration.md) | 2 |
| H. CLI Flags | [h-cli-flags.md](./04-components/h-cli-flags.md) | 2 |

## Agent Execution Order

```
Agent 1 (Foundation)             Agent 2 (Integration)
========================         ========================
pkg/logging/errors.go            (waits for Agent 1)
pkg/logging/event.go                   |
pkg/logging/sink.go                    |
pkg/logging/jsonl_writer.go            |
pkg/logging/emitter.go                 |
pkg/logging/*_test.go                  |
pkg/api/config.go (LoggingConfig)      |
         |                             |
         +---> DONE ------------------>+
                                 pkg/policy/registry.go
                                 pkg/policy/engine.go
                                 pkg/policy/host_filter.go
                                 pkg/policy/local_model_router.go
                                 pkg/policy/secret_injector.go
                                 pkg/net/http.go
                                 pkg/net/stack_darwin.go
                                 pkg/net/proxy.go
                                 pkg/sandbox/sandbox_darwin.go
                                 pkg/sandbox/sandbox_linux.go
                                 cmd/matchlock/cmd_run.go
                                 tests (regression + new)
```

## v0 Event Types

| Type | Emitter Location | Status |
|---|---|---|
| `llm_request` | `pkg/net/http.go` | Implemented in v0 |
| `llm_response` | `pkg/net/http.go` | Implemented in v0 |
| `key_injection` | `pkg/policy/secret_injector.go` | Implemented in v0 |
| `budget_action` | (none) | Struct defined, no emitter in v0 |

## Key Codebase References

| Reference | File | Relevance |
|---|---|---|
| Logger DI pattern | `pkg/policy/engine.go:26-29` | Model for emitter injection |
| api.Event struct | `pkg/api/vm.go:47-53` | Model for Event struct design |
| Plugin factory | `pkg/policy/registry.go:13` | Factory signature to update |
| Sandbox construction | `pkg/sandbox/sandbox_darwin.go:54-389` | Where emitter is created |
| HTTP interceptor | `pkg/net/http.go:141-293` | Where llm_request/response are emitted |
| Secret injector | `pkg/policy/secret_injector.go:74-91` | Where key_injection is emitted |
| Sentinel errors | `AGENTS.md:79-95` | Error handling convention |
| Test assertions | `AGENTS.md:73-74` | testify/require + testify/assert |
