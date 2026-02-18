# ADR-002: VFS Interception and Hook Execution Model

**Status:** Proposed  
**Date:** 2026-02-13  
**Author:** Jingkai He

## Context

Issue [#7](https://github.com/jingkaihe/matchlock/issues/7) requests interception of VM filesystem operations for logging, fault injection, and custom behavior at mount points.

Current state:

- VFS traffic flows through `guest-fused` to host `pkg/vfs/server.go` over vsock.
- `pkg/api/hooks.go` defines `VFSHooks`, but hooks are not wired anywhere.
- SDKs communicate via JSON-RPC subprocess (`matchlock rpc`), so in-memory Go/Python callback functions are not directly transferable across process boundaries.
- VFS request handling is effectively serialized per connection:
  - guest side `VFSClient.Request` is guarded by a mutex
  - host side `VFSServer.HandleConnection` processes one request at a time for that connection

We want to support:

1. allow/block operation policy
2. before/after hooks (safe callbacks and explicit dangerous re-entrant callbacks)
3. mutable write hooks (override write payload)

## Decision

We will implement VFS interception as host-side middleware in `pkg/vfs`, using declarative hook/rule configuration exposed through API/SDK, with strict execution boundaries to prevent recursion and deadlock.

### 1. Interception Layer Placement

Introduce an interceptor provider/handle wrapper in `pkg/vfs`:

- Wrap `Provider` methods (`Stat`, `ReadDir`, `Open`, `Create`, `Mkdir`, `Chmod`, `Remove`, `Rename`, ...)
- Wrap `Handle` methods (`ReadAt`, `WriteAt`, `Close`, `Truncate`, ...)
- Attach wrapper at mount router composition time in sandbox creation so both paths are covered:
  - guest FUSE operations
  - host RPC file operations (`write_file`, `read_file`, `list_files`)

This keeps providers (`memory`, `host_fs`, `overlay`, `readonly`) unchanged and centralizes interception behavior.

### 2. Hook Model

Split hooks into two classes:

- **Policy hooks (synchronous, inline):**
  - allow/block decision
  - request mutation (e.g., mutate write payload)
- **Side-effect hooks (asynchronous, out-of-band):**
  - logging
  - external notifications
  - optional hook-triggered exec

Execution rules:

- `before` can block/mutate and must stay fast.
- `after` runs asynchronously and must not delay the filesystem op response path.
- Hook errors in policy stage fail closed for blocked ops and fail open for non-critical telemetry hooks (configurable later if needed).

### 3. Preventing Recursive Trigger and Deadlock

Because VFS requests are serialized per connection, inline `exec` from a `before` hook can deadlock if exec touches VFS.

Therefore:

- We will **not** allow inline `exec` in synchronous `before` hooks.
- Hook-triggered exec is allowed only from async `after` hooks.
- Add VM-scoped side-effect suppression guard during hook-exec:
  - policy hooks remain active
  - side-effect hooks are suppressed for VFS ops produced by hook-exec
- Add safety limits:
  - non-reentrant side-effect suppression
  - hook timeout
  - cycle detection by hook identity chain

Suppression/limit events are emitted for observability.

### 4. API/SDK Surface

Expose declarative wire rules (`allow` / `block`) plus SDK-local callbacks that are compiled client-side:

- `hook(event)` for safe after callbacks (suppressed while hook side effects are active)
- `dangerous_hook(client, event)` for explicit re-entrant callbacks
- `mutate_hook(request)` for before-write content mutation
- `action_hook(request)` for before-op allow/block decisions

Rationale: subprocess SDK architecture still requires JSON-RPC-safe wire payloads, while local callbacks provide ergonomics for SDK users.

### 5. Eventing

Emit `api.Event{Type:"file"}` from interception layer for:

- operation allowed/blocked
- mutation applied
- hook side-effect executed/failed
- suppression or cycle-limit triggered

This activates existing file event schema and aligns with network interception observability.

## Consequences

### Positive

- Single interception point for all VFS operations, independent of backend/provider.
- Supports allow/block, before/after, and write mutation without changing guest protocol.
- Avoids callback transport problems across SDK subprocess boundary.
- Avoids deadlock-prone inline exec in serialized VFS path.

### Negative

- Declarative hook model is less flexible than arbitrary in-process callbacks.
- Async `after` semantics are eventually consistent; hook side effects are not transactionally coupled with FS success.
- Additional complexity in rule engine, suppression state, and eventing.

### Performance Impact

- Matching rules on each VFS op adds overhead.
- Current serialized VFS channel remains a throughput limiter for metadata-heavy workloads.
- Design minimizes extra latency by keeping policy stage lightweight and offloading side effects async.

## Alternatives Considered

### 1. `io.FS` as Mount Interceptor

Rejected. `io.FS` is read-oriented and does not model write/update/remove/chmod/rename semantics required by Matchlock VFS.

### 2. Inline Callback Hooks in SDK

Rejected for initial implementation. SDK currently talks to a child RPC process; callback functions cannot be marshaled robustly across this boundary.

### 3. Inline Exec in `before` Hook

Rejected due to recursion/deadlock risk under serialized VFS request handling.

### 4. Guest-Side Hooking in `guest-fused`

Rejected for now. We need host authority for policy enforcement, eventing, and integration with host-side SDK/RPC state.

## Implementation Plan

### Phase 1 (MVP)

- Add VFS interceptor middleware (`Provider` + `Handle` wrappers).
- Add allow/block operation rules.
- Add write mutation for `OpWrite`.
- Emit file events for allow/block/mutate.

### Phase 2

- Add async after-hooks with suppression guard.
- Add explicit dangerous re-entrant callbacks.
- Add timeout/cycle safety controls.

### Phase 3

- Expose rules via Go SDK + Python SDK builders.
- Add acceptance tests for:
  - allow/block correctness
  - write mutation correctness
  - no recursive side-effect hooks during hook-exec
  - cancellation/timeout behavior for async hook actions
