# Lifecycle and Cleanup Runbook

This document explains how Matchlock tracks VM lifecycle state, which resources
are owned by a VM, and how to recover from leaked host resources.

## Why this exists

Sandbox shutdown can fail partway through (host signal, process crash, network
permission issues, etc.). Matchlock now persists lifecycle state so cleanup can
be resumed safely and auditable later.

## Lifecycle record

Each VM has persistent lifecycle rows in:

- `~/.matchlock/state.db`
- table: `vm_lifecycle`

The record includes:

- current lifecycle phase
- last lifecycle error (if any)
- known resource identifiers/paths (rootfs, subnet allocation, TAP table names)
- per-step cleanup status snapshot for close/reconcile operations

## Metadata storage backend

Lifecycle phase/audit data, VM runtime metadata, and subnet allocation metadata
are stored in the same SQLite DB:

- `~/.matchlock/state.db`
- tables: `vms`, `subnet_allocations`, `vm_lifecycle`, `schema_migrations`

Image metadata is stored in a separate SQLite DB:

- `~/.cache/matchlock/images/metadata.db`
- tables: `images`, `schema_migrations`
- `images.scope` distinguishes `local` vs `registry` metadata

Large artifacts remain filesystem-based:

- VM directories, logs, sockets, and per-VM rootfs copies
- image rootfs files under image cache directories

## Schema migrations

Both DBs use automatic startup migrations with:

- `journal_mode=WAL`
- `foreign_keys=ON`
- `busy_timeout`

Migration behavior:

- applied versions are tracked in `schema_migrations`
- each migration runs in a transaction
- before pending migrations, Matchlock creates a pre-migration backup
- if migration fails, Matchlock restores from that backup

## Phases

Phases are validated through an allowed-transition state machine.

Primary phases:

- `creating`
- `created`
- `starting`
- `running`
- `stopping`
- `stopped`
- `cleaning`
- `cleaned`

Failure phases:

- `create_failed`
- `start_failed`
- `stop_failed`
- `cleanup_failed`

Typical success path:

1. `creating -> created`
2. `created -> starting -> running`
3. `running -> stopping -> cleaning -> cleaned`

## Resource ownership

The latest lifecycle snapshot tracks resources needed for deterministic cleanup:

- VM state directory: `~/.matchlock/vms/<vm-id>/`
- per-VM rootfs copy: `rootfs.ext4` under VM state dir
- subnet allocation row in `~/.matchlock/state.db` (`subnet_allocations` table)
- Linux-only network artifacts:
  - TAP interface (`fc-<suffix>`)
  - nftables tables (`matchlock_<tap>`, `matchlock_nat_<tap>`)

## Cleanup behavior

`Sandbox.Close()` now reports cleanup failures instead of silently ignoring
them. Failures are stored in lifecycle cleanup entries.

CLI behavior now preserves cleanup semantics:

- command exit codes are propagated without bypassing deferred cleanup
- `run --rm=false -it` keeps VM alive until signal, then performs close cleanup

## Reconciliation (`matchlock gc`)

Use `gc` to clean leaked resources for stopped/crashed VMs:

```bash
# Reconcile one VM
matchlock gc vm-abc12345

# Reconcile all VMs
matchlock gc

# Also reconcile currently running VMs (dangerous; use sparingly)
matchlock gc --force-running
```

`gc` reconciles:

- subnet allocation row
- rootfs copy
- Linux: TAP + nftables artifacts

If a VM is still running, reconciliation is skipped unless `--force-running`
is provided.

## `rm` and `prune` semantics

`rm`/`prune` now run reconciliation before removing VM metadata:

- if reconcile succeeds, VM state can be removed
- if reconcile fails, removal is aborted and error is returned

This prevents losing VM metadata while leaking host resources.

## Subnet allocator safety

Subnet allocation is now DB-backed with:

- unique subnet octet constraint at the DB level
- atomic writes via SQLite transactions
- WAL mode for robust cross-process concurrency

This replaces the previous lock-file + JSON-scan allocator model.

## Platform notes

- Linux: reconciles subnet/rootfs/TAP/nftables.
- macOS and non-Linux platforms: reconciles subnet/rootfs; platform-specific
  network artifact reconciliation is currently a no-op.

## Operational troubleshooting

1. Inspect VM states:
   - `matchlock list`
2. Inspect lifecycle history (append-only):
   - `sqlite3 ~/.matchlock/state.db 'select vm_id,version,phase,updated_at,last_error from vm_lifecycle where vm_id=\"<vm-id>\" order by version;'`
   - `sqlite3 ~/.matchlock/state.db 'select version,cleanup_json from vm_lifecycle where vm_id=\"<vm-id>\" order by version;'`
3. (Optional) Inspect VM/subnet DB metadata:
   - `sqlite3 ~/.matchlock/state.db 'select id,status,pid from vms;'`
   - `sqlite3 ~/.matchlock/state.db 'select vm_id,octet,subnet from subnet_allocations;'`
4. (Optional) Inspect image metadata:
   - `sqlite3 ~/.cache/matchlock/images/metadata.db 'select scope,tag,digest,size from images;'`
5. Reconcile leaked resources:
   - `matchlock gc <vm-id>`
6. Remove stopped VM metadata after successful reconcile:
   - `matchlock rm <vm-id>`

If `gc` still fails, check failed cleanup steps in `vm_lifecycle.cleanup_json`
for recent versions and fix host permissions/network prerequisites before
retrying.
