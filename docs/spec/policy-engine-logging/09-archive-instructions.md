# Archive Instructions

## When to Archive

This specification should be moved to `docs/archive/specs/` when:
- [ ] All components are implemented and merged to main
- [ ] All tests pass (`mise run test`)
- [ ] Event logging verified working in a real sandbox run
- [ ] JSON-L output verified parseable with `jq`

## Archive Commands

Run these commands after implementation is complete:

```bash
# Move this spec to archive
mv docs/spec/policy-engine-logging docs/archive/specs/

# Move related draft (if exists)
mv docs/draft/policy-engine-logging.md docs/archive/drafts/

# Verify archive exclusion is working
# (Claude Code should not be able to read archived docs)
```

## Why Archive?

Outdated specifications pollute LLM context and can cause AI agents to:
- Suggest deprecated patterns
- Reference non-existent code
- Conflict with current implementations

Archiving removes specs from Claude's context while preserving history.

## Related Files

- **Draft:** `docs/draft/policy-engine-logging.md`
- **Spec:** `docs/spec/policy-engine-logging/`
- **Implementation (new files):**
  - `pkg/logging/errors.go`
  - `pkg/logging/event.go`
  - `pkg/logging/sink.go`
  - `pkg/logging/jsonl_writer.go`
  - `pkg/logging/emitter.go`
  - `pkg/logging/event_test.go`
  - `pkg/logging/jsonl_writer_test.go`
  - `pkg/logging/emitter_test.go`
  - `pkg/logging/golden_test.go`
  - `pkg/logging/testdata/event_full.golden`
  - `pkg/logging/testdata/event_minimal.golden`
- **Implementation (modified files):**
  - `pkg/api/config.go`
  - `pkg/policy/registry.go`
  - `pkg/policy/engine.go`
  - `pkg/policy/host_filter.go`
  - `pkg/policy/secret_injector.go`
  - `pkg/policy/local_model_router.go`
  - `pkg/net/http.go`
  - `pkg/net/stack_darwin.go`
  - `pkg/net/proxy.go`
  - `pkg/sandbox/sandbox_darwin.go`
  - `pkg/sandbox/sandbox_linux.go`
  - `cmd/matchlock/cmd_run.go`
