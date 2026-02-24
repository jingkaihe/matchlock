# Required Plugin Logging -- Implementation Specification

## Quick Reference

| Document | Purpose |
|----------|---------|
| [01-overview.md](./01-overview.md) | Problem statement, goals, scope |
| [02-architecture.md](./02-architecture.md) | Event flow, component relationships |
| [03-interfaces.md](./03-interfaces.md) | Go struct/interface definitions (exact code) |
| [04-components/](./04-components/) | Per-file change specifications |
| [05-event-types.md](./05-event-types.md) | New event constants and data structs |
| [06-tests.md](./06-tests.md) | Test strategy and specific test cases |
| [07-agent-assignments.md](./07-agent-assignments.md) | Work distribution for agents |
| [08-changelog.md](./08-changelog.md) | What changed from the draft |
| [09-archive-instructions.md](./09-archive-instructions.md) | Post-implementation cleanup |

## Source Draft

`docs/draft/required-plugin-logging.md`

## Summary

Make structured event logging a required part of every plugin phase by changing
plugin interfaces to return structured decision types. The engine (not plugins)
emits events using decision metadata. This eliminates silent plugins, removes
ad-hoc emitter usage from plugin code, and simplifies the PluginFactory
signature.

## Agents Required

**2 agents** can implement this feature with no file conflicts.

| Agent | Role | Key Files |
|-------|------|-----------|
| Agent 1 | Foundation + Interfaces | `plugin.go`, `event.go`, `registry.go`, `engine.go` |
| Agent 2 | Plugin Migration + Tests | `secret_injector.go`, `local_model_router.go`, `usage_logger.go`, `http.go`, all `*_test.go` |

See [07-agent-assignments.md](./07-agent-assignments.md) for full details.
