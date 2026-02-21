# ADR-003: OCI Layer-Aware Content Store and Overlay Root Assembly

**Status:** Proposed  
**Date:** 2026-02-21  
**Author:** Jingkai He

## Context

Matchlock currently materializes registry and imported images into a single ext4 rootfs image per digest/tag, then creates a per-VM writable copy at sandbox creation time.

Current behavior:

- Image build/pull path flattens the OCI filesystem into one `*.ext4` artifact.
- Linux sandbox creation attempts reflink clone for per-VM rootfs copy, with full copy fallback.
- macOS sandbox creation performs a full copy for per-VM rootfs.
- `overlay` in VFS mounts is a host-path snapshot copy model, not kernel overlayfs root assembly.

This design is simple but has structural storage inefficiency:

- Duplicate data across images that share OCI layers.
- Duplicate data across concurrently running VMs of the same image when clone is unavailable or ineffective.
- Additional copy latency on sandbox startup.

Matchlock is still in research preview. Backward compatibility with current on-disk image format is not a requirement for this change.

## Decision

Adopt an OCI layer-aware image storage model and boot sandboxes using overlay root assembly:

1. Store image layers in a content-addressed local store keyed by OCI digest/DiffID.
2. Stop producing monolithic rootfs ext4 images for normal runtime.
3. At VM boot, attach:
- a small bootstrap root disk (`vda`) that runs `/init`
- N read-only layer disks (lowerdirs)
- one per-VM writable upper disk (upperdir/workdir)
4. In guest bootstrap init, mount read-only layer disks and writable upper disk, assemble kernel overlayfs root, `pivot_root`, then continue normal guest-init flow.

Platform goal:

- Same logical model on Linux and macOS.
- Backend-specific differences remain only in block-device attachment APIs.

## Architecture

### Host-Side Image Store

Proposed layout:

```text
~/.cache/matchlock/images/
  blobs/
    sha256_<layer-digest>.ext4
  metadata.db
```

`metadata.db` tracks:

- image identities/manifests
- tag/reference mappings (for example `scope + tag -> image`)
- ordered layer list per image (for OCI lowerdir semantics)
- OCI config (entrypoint/cmd/env/user/workdir)
- media type and provenance

Rules:

- Layers are immutable and content-addressed.
- Multiple tags/images may reference the same stored layer digest.
- GC is reference-based from metadata roots.
- All image entry paths (`matchlock pull`, `matchlock build`, and `matchlock image import`) normalize into the same records in `metadata.db`.

### Boot Model

`vda` is no longer application rootfs. It is a minimal bootstrap disk that contains:

- `/init` bootstrap logic
- required early userspace tools for mount/overlay/pivot

Per sandbox:

1. Create writable upper ext4 disk (sparse, small initial size, growable).
2. Inject runtime-specific files into upper disk before VM start:
- matchlock guest runtime binaries
- network CA cert when interception is enabled
- other per-sandbox generated files currently injected into rootfs
3. Attach image layer disks as read-only block devices.
4. Pass layer/upper mapping via kernel cmdline parameters.
5. Bootstrap `/init` mounts lowerdirs and upper/work, assembles overlay mount, performs `pivot_root`, and continues current guest lifecycle initialization.

### Filesystem Format Strategy

Initial format:

- Read-only layers: ext4 images attached read-only.
- Writable upper: ext4 image attached read-write.

Future optional optimization:

- Evaluate EROFS for lower layers after enabling/testing required kernel config.

## Consequences

### Positive

- Strong deduplication across images sharing base layers.
- Lower disk usage for concurrent VMs of same image (small per-VM upper only).
- Faster startup by removing large monolithic rootfs copy path.
- Cleaner separation between immutable image content and mutable runtime state.

### Negative

- Significant boot-path redesign (two-stage root setup).
- More complex metadata and GC model.
- Higher implementation and test complexity around OCI whiteouts and layer correctness.

### Risk Areas

- Correct OCI semantics per layer (whiteouts, opaque dirs, permissions, xattrs, hardlinks, device nodes).
- Device-count limits for very deep layer chains.
- Operational complexity if layer disk creation/extraction is not robust.

## Alternatives Considered

### 1. Keep Monolithic ext4 + Improve Cloning

Pros:

- Minimal code churn.
- Immediate gains on reflink-capable filesystems.

Cons:

- No cross-image layer dedupe.
- Startup copy cost still present in many environments.

### 2. Host Filesystem-Specific Storage Pools (LVM thin, ZFS, btrfs snapshots)

Pros:

- Efficient snapshots on supported hosts.

Cons:

- Platform-specific operational burden.
- Weak cross-platform parity.

### 3. Full containerd/snapshotter Integration

Pros:

- Mature OCI storage model and lazy-pull ecosystem.

Cons:

- Much larger dependency and integration surface than needed for current scope.

## Implementation Plan

### Phase 1: Layer Store Foundation

- Add content-addressed layer blob store and metadata schema.
- Refactor image build/import/pull paths to persist per-layer artifacts through one canonical ingest pipeline.
- Add reference counting and basic GC.

### Phase 2: Bootstrap Root and Overlay Assembly

- Introduce bootstrap root disk artifact and kernel cmdline contract for layer mapping.
- Implement guest bootstrap overlay assembly and `pivot_root`.
- Move runtime/CA injections from monolithic rootfs into per-VM upper disk preparation.

### Phase 3: Backend Wiring

- Update Linux and macOS sandbox builders to attach lower/upper disks per VM.
- Enforce read-only attachment for lower layer disks.
- Add safeguards for layer-count limits and optional layer squashing threshold.

### Phase 4: Validation and Tuning

- Add unit + acceptance coverage for:
- layer sharing/dedupe behavior
- overlay boot correctness
- whiteout/opaque semantics
- xattr/device/hardlink preservation
- startup latency and disk-usage regression checks
- Evaluate optional EROFS lower-layer format as follow-up.

## Acceptance Criteria

- Running two images sharing base layers stores shared layers only once on disk.
- Running multiple VMs from same image creates only per-VM upper disks, not full rootfs copies.
- Linux and macOS pass the same image filesystem behavior acceptance suite.
- Existing lifecycle cleanup and GC correctly reclaim orphaned uppers and unreferenced layers.
