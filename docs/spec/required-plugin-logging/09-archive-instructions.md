# 09 -- Archive Instructions

## When to Archive

This specification should be moved to `docs/archive/specs/` when:

- [ ] All components are implemented and merged
- [ ] `go test ./...` passes from the project root
- [ ] `go vet ./...` is clean
- [ ] The event log shows `route_decision`, `request_transform`, and
      `response_transform` events for a representative request flow
- [ ] No plugin file imports `pkg/logging` directly

## Archive Commands

Run these commands after implementation is complete:

```bash
# Create archive directories if they don't exist
mkdir -p docs/archive/specs
mkdir -p docs/archive/drafts

# Move this spec to archive
mv docs/spec/required-plugin-logging docs/archive/specs/

# Move related draft
mv docs/draft/required-plugin-logging.md docs/archive/drafts/

# Verify the files are moved
ls docs/archive/specs/required-plugin-logging/
ls docs/archive/drafts/required-plugin-logging.md
```

## Why Archive?

Outdated specifications pollute LLM context and can cause AI agents to:
- Suggest deprecated patterns (e.g., passing emitter to plugins)
- Reference non-existent code (e.g., `EventKeyInjection` emission sites)
- Conflict with current implementations

Archiving removes specs from active context while preserving history.

## Related Files

- **Draft:** `docs/draft/required-plugin-logging.md`
- **Spec:** `docs/spec/required-plugin-logging/`
- **Implementation files:**
  - `pkg/logging/event.go`
  - `pkg/policy/plugin.go`
  - `pkg/policy/registry.go`
  - `pkg/policy/engine.go`
  - `pkg/policy/secret_injector.go`
  - `pkg/policy/local_model_router.go`
  - `pkg/policy/usage_logger.go`
  - `pkg/policy/host_filter.go`
- **Test files:**
  - `pkg/policy/engine_test.go`
  - `pkg/policy/secret_injector_test.go`
  - `pkg/policy/local_model_router_test.go`
  - `pkg/policy/usage_logger_test.go`
  - `pkg/policy/host_filter_test.go`
