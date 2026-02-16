# ADR-001: Local Image Build and Management

**Status:** Accepted (Phase 1 + Phase 2 implemented)  
**Date:** 2026-02-07  
**Author:** Jingkai He

## Context

Matchlock currently only supports running images pulled from OCI/Docker registries. Users cannot build images from a Dockerfile or manage locally-built images. The desired UX is:

```bash
matchlock build -f Dockerfile -t myapp:latest .
matchlock image ls
matchlock run --image myapp:latest ...
```

A natural question is whether we can use BuildKit inside the matchlock VM itself to perform builds. This requires investigating the guest agent's security restrictions and kernel capabilities.

### Current Guest Security Model

The guest agent applies the following restrictions to sandboxed processes:

| Mechanism | Detail |
|---|---|
| Capabilities dropped | `CAP_SYS_PTRACE`, `CAP_SYS_ADMIN`, `CAP_SYS_MODULE`, `CAP_SYS_RAWIO`, `CAP_SYS_BOOT` |
| Seccomp filter | Blocks `process_vm_readv`, `process_vm_writev`, `ptrace`, `kexec_load`, `kexec_file_load` |
| Namespaces | New PID + mount namespaces (`CLONE_NEWPID \| CLONE_NEWNS`) per exec |
| Privilege escalation | `no_new_privs` set unconditionally |

### BuildKit-in-VM Feasibility

| Requirement | Status |
|---|---|
| `overlayfs` | ✅ Kernel has `CONFIG_OVERLAY_FS=y` |
| `CAP_SYS_ADMIN` (mount, overlay) | ❌ Dropped by guest-agent |
| `no_new_privs` bypass | ❌ Set unconditionally |
| User namespaces (rootless BuildKit) | ✅ `CONFIG_USER_NS=y` added to kernel config |
| Container runtime (`runc`) | ❌ Not injected into rootfs |

Running BuildKit inside the VM requires relaxing the security model (privileged mode) and adding kernel support for user namespaces. This is acceptable because the micro-VM itself is the primary security boundary — the in-guest seccomp/capability restrictions are defence-in-depth, not the isolation primitive.

## Decision

We will implement local image build and management in two phases.

### Phase 1: Local Image Store + Import

Add a local image store, CLI commands for image management, and the ability to import images from Docker/OCI tarballs.

**Local image store** (`pkg/image/store.go`):

```go
type Store struct { baseDir string }

func (s *Store) Save(tag string, rootfsPath string, metadata ImageMeta) error
func (s *Store) Get(tag string) (*BuildResult, error)
func (s *Store) List() ([]ImageInfo, error)
func (s *Store) Remove(tag string) error
```

Images are stored under `~/.cache/matchlock/images/local/{sanitized_tag}/` alongside a `metadata.json` with tag, creation time, size, and build context info.

**CLI commands:**

```bash
matchlock image ls                  # List all images (local + cached registry)
matchlock image rm myapp:latest     # Remove a local image

# Import from Docker/OCI tarball
docker save myapp:latest | matchlock image import myapp:latest
matchlock image import myapp:latest < image.tar

# Tag a registry image locally (existing build extended with -t)
matchlock build -t myapp:latest alpine:latest
```

**Builder change** — `Build()` checks the local store first before falling back to the registry:

```go
func (b *Builder) Build(ctx context.Context, imageRef string) (*BuildResult, error) {
    // 1. Check local store
    if result, err := b.store.Get(imageRef); err == nil {
        return result, nil
    }
    // 2. Fall back to registry (existing code)
    ...
}
```

**Import pipeline** — `matchlock image import` reads a Docker/OCI tarball from stdin, extracts the layers using the existing `extractImage` + `createExt4` machinery, and saves the result to the local store. The tarball format is the same as `docker save` output (OCI image layout or Docker archive), which `go-containerregistry` already supports via `tarball.Image()`.

### Phase 2: Privileged Mode + BuildKit-in-VM

Add a `--privileged` flag that skips in-guest security restrictions, enabling BuildKit (or any tool requiring full root capabilities) to run inside the VM.

**Changes:**

1. **`pkg/api/config.go`** — Add `Privileged bool` to `Config`
2. **Guest agent protocol** — Pass privileged flag via kernel cmdline (`matchlock.privileged=1`)
3. **`internal/guestruntime/agent/sandbox_proc.go`** — When privileged, skip capability drops, seccomp filter, and `no_new_privs`
4. ~~**Kernel config** — Add `CONFIG_USER_NS=y` to both `arm64.config` and `x86_64.config` for rootless BuildKit support~~ ✅ Done

**Usage:**

```bash
# Interactive BuildKit session
matchlock run --privileged --image moby/buildkit:rootless \
  -v ./context:/workspace -it sh
# Inside VM: buildctl build --frontend dockerfile.v0 --local context=/workspace ...
```

**Dockerfile build UX** (convenience wrapper, future):

```bash
# Equivalent to: boot privileged VM with buildkit, run build, extract result
matchlock build -f Dockerfile -t myapp:latest .
```

This wraps the BuildKit-in-VM flow into a single command: boot a privileged VM with BuildKit, mount the build context, run the build, extract the resulting image, and save it to the local store. The actual Dockerfile parsing and execution is handled entirely by BuildKit — matchlock only orchestrates the VM lifecycle.

## Consequences

### Positive

- Users can import and run local images without pushing to a registry (`image import`)
- BuildKit-in-VM (Phase 2) provides full Dockerfile support using a proven tool, with no custom parser needed
- The local image store (Phase 1) is a foundation for both import and future build workflows
- Privileged mode is a general-purpose feature useful beyond image building (e.g. running Docker-in-VM, systemd, etc.)

### Negative

- Privileged mode weakens in-guest isolation, though the VM boundary remains
- BuildKit-in-VM requires downloading the BuildKit image (~50MB), adding to first-build latency
- Kernel rebuild required to add `CONFIG_USER_NS=y` for rootless BuildKit

### Risks

- BuildKit rootless mode may have edge cases on the matchlock kernel (minimal config)
- The `matchlock build -f Dockerfile` convenience wrapper (Phase 2) needs to handle BuildKit output extraction reliably — getting the built image out of the VM and into the local store

## Alternatives Considered

### 1. Host-Side Dockerfile Build (VM per RUN Step)

Parse Dockerfiles on the host and execute each `RUN` step in a separate matchlock VM, snapshotting the rootfs between steps. Rejected for three reasons:

- **Rootfs round-tripping is lossy** — The `debugfs`-based ext4 creation doesn't preserve file permissions, xattrs, hardlinks, or device nodes. Real `RUN` steps (e.g. `apt-get install`) create files with specific ownership/permissions that would silently break.
- **Dockerfile parsing is a tarpit** — Even "minimal" support quickly hits multi-line `RUN`, `${}` substitution, `ARG`/`ENV` interaction, `.dockerignore`, multi-stage builds, etc. This would amount to a poor reimplementation of BuildKit.
- **Performance** — Each `RUN` step would require: boot VM → exec → shut down → extract rootfs from ext4 → rebuild ext4. For a 10-step Dockerfile this is unacceptably slow.

### 2. Shell Out to Docker/Podman for Builds

Use the host's Docker or Podman to build images, then import the result. Rejected as the primary path because it introduces an external dependency. However, `matchlock image import` (Phase 1) provides this as an opt-in escape hatch for users who already have Docker/Podman.

### 3. Use `umoci` or OCI Image Tools

Build OCI images on the host using `umoci` for rootfs manipulation. This would execute `RUN` commands on the host without sandboxing, defeating matchlock's purpose.
