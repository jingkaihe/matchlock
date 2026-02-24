# 08 - Changelog: Draft to Spec

## Specification Created

- **Date:** 2026-02-24
- **Source Draft:** `docs/draft/policy-engine-logging.md`
- **Spec Location:** `docs/spec/policy-engine-logging/`

## Summary of What Changed from Draft

### Unchanged from Draft

The following elements were carried forward from the draft without modification:

- Event schema (required fields: `ts`, `run_id`, `agent_system`, `event_type`, `summary`)
- Optional fields (`plugin`, `tags`, `data`)
- All four v0 event types and their data struct definitions
- JSON-L format specification
- `JSONLWriter` implementation (unbuffered, `json.Encoder`, `Sync()` on close)
- `Emitter` design with `EmitterConfig` and `Sink` interface
- Emission examples for `llm_request`, `llm_response`, `key_injection`
- v1 roadmap items (`gate_decision`, `route_decision`)
- All seven resolved open questions and their decisions

### Refined from Draft

1. **Sink extracted to own file:** The draft put `Sink` interface in `emitter.go`. The spec separates it into `sink.go` for cleaner imports and interface discovery.

2. **Errors file added:** The draft did not specify sentinel errors. The spec adds `pkg/logging/errors.go` following the project's `internal/errx` convention per `AGENTS.md`.

3. **Log path derivation clarified:** The draft specified `~/.local/share/matchlock/logs/<sandbox-id>/events.jsonl` but the codebase uses `~/.matchlock/vms/` (not XDG `~/.local/share/`). The spec derives the log path relative to `state.Manager.baseDir` to stay consistent: `~/.matchlock/logs/<sandbox-id>/events.jsonl`.

4. **Non-fatal emitter construction:** The draft implied the emitter construction could fail sandbox creation. The spec makes it explicit: emitter construction failures are logged via `slog.Warn` but do not prevent the sandbox from starting.

5. **PluginFactory signature update:** The draft mentioned passing emitter to plugins but did not address the `PluginFactory` function type in `registry.go`. The spec explicitly updates this type signature and all three built-in factory implementations.

6. **HTTP handler coverage:** The draft focused on `HandleHTTPS` for event emission. The spec adds equivalent emission to `HandleHTTP` (plain HTTP interception) with `["http"]` tags instead of `["tls"]`.

7. **Merge() handling:** The draft did not address `Config.Merge()`. The spec adds `Logging` field handling to the merge function.

8. **Test strategy fully specified:** The draft mentioned "unit tests for serialization and file writing" generally. The spec provides complete test code, golden file strategy, concurrent write tests, and regression test requirements for existing files.

### Added Beyond Draft

1. **Golden file tests** for JSON-L format stability
2. **captureSink** test helper for emission verification in unit tests
3. **Concurrent write test** (100 goroutines x 10 events)
4. **Explicit regression test list** for existing test files that need updating
5. **Agent assignment strategy** with file-level execution ordering
6. **Build verification gates** between agent phases

## New Interfaces Defined

| Interface/Type | File | Purpose |
|---|---|---|
| `Sink` | `pkg/logging/sink.go` | Event consumer interface |
| `Event` | `pkg/logging/event.go` | Canonical event struct |
| `EmitterConfig` | `pkg/logging/emitter.go` | Static metadata config |
| `Emitter` | `pkg/logging/emitter.go` | Metadata stamping + sink dispatch |
| `JSONLWriter` | `pkg/logging/jsonl_writer.go` | File-backed sink |
| `LoggingConfig` | `pkg/api/config.go` | API config struct |

## New Patterns Introduced

1. **Nil-safe emitter pattern:** The emitter uses nil checks at call sites (`if emitter != nil`) rather than a no-op default, because creating a default emitter would require a sink. This is consistent with the intent to have zero overhead when logging is disabled.

2. **Best-effort event emission:** All `emitter.Emit()` calls discard errors with `_ =`. This is a deliberate pattern for observability that should not affect functionality.

## Files to Be Created (Full Paths)

```
pkg/logging/errors.go
pkg/logging/event.go
pkg/logging/sink.go
pkg/logging/jsonl_writer.go
pkg/logging/emitter.go
pkg/logging/event_test.go
pkg/logging/jsonl_writer_test.go
pkg/logging/emitter_test.go
pkg/logging/golden_test.go
pkg/logging/testdata/event_full.golden
pkg/logging/testdata/event_minimal.golden
```

## Files to Be Modified (Full Paths)

```
pkg/api/config.go
pkg/policy/registry.go
pkg/policy/engine.go
pkg/policy/host_filter.go
pkg/policy/secret_injector.go
pkg/policy/local_model_router.go
pkg/net/http.go
pkg/net/stack_darwin.go
pkg/net/proxy.go
pkg/sandbox/sandbox_darwin.go
pkg/sandbox/sandbox_linux.go
cmd/matchlock/cmd_run.go
pkg/policy/engine_test.go
pkg/policy/secret_injector_test.go
pkg/policy/registry_test.go
```

## Estimated Complexity

| Component | Files | Lines (est.) | Complexity |
|---|---|---|---|
| Event types + data structs | 1 | ~80 | Low |
| Sink interface | 1 | ~15 | Trivial |
| JSONLWriter | 1 | ~50 | Low |
| Emitter | 1 | ~70 | Low |
| Errors | 1 | ~15 | Trivial |
| Config integration | 1 modified | ~20 added | Low |
| Engine integration | 2 modified | ~30 added | Medium (signature cascade) |
| HTTP interceptor integration | 3 modified | ~60 added | Medium |
| Secret injector integration | 1 modified | ~40 added | Medium |
| CLI flags | 1 modified | ~20 added | Low |
| Tests (new) | 5 | ~400 | Medium |
| Tests (regression updates) | 3 modified | ~50 changed | Low |
| **Total** | **21 files** | **~850 lines** | **Medium overall** |
