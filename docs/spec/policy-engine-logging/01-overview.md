# 01 - Overview: Policy Engine Logging Standard

## Feature Summary

Add a persistent, structured JSON-L event logging system to matchlock's network policy engine. This system operates alongside the existing `slog` (operational diagnostics) and `api.Event` channel (Go SDK) as a third, independent output concern focused on persistent, post-run-analyzable event data.

## Problem Statement

Matchlock currently provides two output systems for network policy activity:

1. **`slog`** -- human-readable, ephemeral stderr diagnostics for developers
2. **`api.Event` channel** -- in-memory, typed Go structs for SDK consumers via `sandbox.Events()`

Neither provides persistent, structured event data suitable for post-run analysis, cost tracking, or audit trails. When a sandbox is destroyed, all network activity data is lost.

## Solution

Introduce a `pkg/logging` package that provides:

- A canonical `Event` struct with required fields (`ts`, `run_id`, `agent_system`, `event_type`, `summary`) and optional fields (`plugin`, `tags`, `data`)
- A `Sink` interface for event consumers, with a `JSONLWriter` implementation that writes one JSON object per line to a persistent file
- An `Emitter` that stamps static metadata (run ID, agent system) onto events and dispatches to sinks
- Integration points in `HTTPInterceptor` (for `llm_request`/`llm_response`) and `secretInjectorPlugin` (for `key_injection`)
- CLI flags (`--event-log`, `--run-id`, `--agent-system`) and config struct (`LoggingConfig`) for opt-in activation

## v0 Scope

Four event types, strictly from the TLS/L7 interception lane:

| Event Type | Source | Description |
|---|---|---|
| `llm_request` | `pkg/net/http.go` | LLM API request intercepted and forwarded |
| `llm_response` | `pkg/net/http.go` | LLM API response received from upstream |
| `budget_action` | (stub) | Budget/cost tracking placeholder -- no emitter in v0 |
| `key_injection` | `pkg/policy/secret_injector.go` | Secret injected, skipped, or leak blocked |

## Out of Scope (v1)

- `gate_decision` and `route_decision` event types
- Convergence of `slog` and structured events
- Consumer tooling (`matchlock logs` subcommand, dashboards)
- `sandbox_lifecycle`, `vfs_operation`, `plugin_error`, `dns_query` events

## Key Decisions (Closed)

These decisions were made during draft iteration and are not open for reconsideration:

1. JSON-L event log coexists alongside `slog` and `api.Event` -- three separate concerns
2. `run_id` is caller-supplied, defaults to sandbox VM ID (`config.GetID()`)
3. `agent_system` set at sandbox startup via static metadata map
4. Persistent logs at `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl`, survives VM cleanup
5. v0 is strictly 4 event types; `gate_decision`/`route_decision` are v1
6. Log secret names, never values
7. Simple unbuffered writes via `json.Encoder`, `Sync()` on close

## File Inventory

### New Files

| File | Purpose |
|---|---|
| `pkg/logging/event.go` | Event struct, type constants, data structs |
| `pkg/logging/sink.go` | `Sink` interface definition |
| `pkg/logging/jsonl_writer.go` | `JSONLWriter` -- file-backed `Sink` implementation |
| `pkg/logging/emitter.go` | `Emitter` -- metadata stamping and sink dispatch |
| `pkg/logging/errors.go` | Sentinel errors for the logging package |
| `pkg/logging/event_test.go` | Unit tests for event serialization |
| `pkg/logging/jsonl_writer_test.go` | Unit tests for JSON-L file writing |
| `pkg/logging/emitter_test.go` | Unit tests for emitter |
| `pkg/logging/testdata/` | Golden files for event format stability |

### Modified Files

| File | Change |
|---|---|
| `pkg/api/config.go` | Add `LoggingConfig` struct to `Config` |
| `pkg/policy/engine.go` | Accept and distribute `*logging.Emitter` |
| `pkg/policy/secret_injector.go` | Emit `key_injection` events |
| `pkg/net/http.go` | Accept `*logging.Emitter`, emit `llm_request`/`llm_response` |
| `pkg/net/stack_darwin.go` | Pass emitter through `Config` struct |
| `pkg/net/proxy.go` | Pass emitter through `ProxyConfig` struct |
| `pkg/sandbox/sandbox_darwin.go` | Create emitter, wire through |
| `pkg/sandbox/sandbox_linux.go` | Create emitter, wire through |
| `cmd/matchlock/cmd_run.go` | Add `--event-log`, `--run-id`, `--agent-system` flags |
