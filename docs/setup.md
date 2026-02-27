# Setup

This guide covers building Matchlock from source and configuring your system for both macOS and Linux.

## System Requirements

| Platform | Requirement |
|----------|------------|
| **macOS** | Apple Silicon (arm64). Intel Macs are not supported. |
| **Linux** | KVM support (`/dev/kvm` must exist). x86_64 or arm64. |

## Prerequisites

### Tooling

Matchlock uses [mise](https://mise.jdx.dev/) to manage development tooling. Install it first:

```bash
# macOS
brew install mise

# Linux (any distro)
curl https://mise.run | sh
```

Mise manages Go, golangci-lint, uv (Python toolchain), and crane (OCI tool). You do not need to install these manually.

### One-Time Setup

From the matchlock repository root:

```bash
mise install
```

This installs all pinned tool versions defined in `mise.toml`:

- **Go 1.25** (compiler)
- **golangci-lint** (linter)
- **uv** (Python SDK toolchain)
- **crane** (OCI image tool)

## Building from Source

> **Important:** Always build with `mise`, not raw `go build`. The build task handles codesigning (macOS), guest binary cross-compilation, and version injection via ldflags.

### macOS

```bash
mise run build
```

This produces two binaries:

- `bin/matchlock` -- the host CLI, codesigned with the `com.apple.security.virtualization` entitlement
- `bin/guest-init` -- the in-VM runtime binary (cross-compiled for Linux arm64)

The guest binary is also cached to `~/.cache/matchlock/guest-init` for runtime use.

### Linux

```bash
mise run build
sudo ./bin/matchlock setup linux
```

The `build` task produces the same two binaries. The `setup linux` command performs one-time system configuration (see below).

> **Warning:** Never run `matchlock run`, `matchlock exec`, or any other runtime command with `sudo`. Only `matchlock setup linux` requires root. Running the matchlock runtime as root is unsupported and may cause permission issues.

### What `mise run build` Does

1. Compiles `cmd/matchlock` with version ldflags (`-X` flags for `Version`, `GitCommit`, `BuildTime`)
2. On macOS: codesigns the binary with the `com.apple.security.virtualization` entitlement (required by Virtualization.framework)
3. Cross-compiles `cmd/guest-init` for Linux (auto-detects host arch for target)
4. On macOS: copies `guest-init` to `~/.cache/matchlock/guest-init`

## Platform-Specific Setup

### macOS (Apple Silicon)

After `mise run build`, macOS requires no additional system configuration. The codesigning step in the build task handles the Virtualization.framework entitlement automatically.

**Verify the binary is codesigned:**

```bash
codesign -d --entitlements - bin/matchlock
```

You should see the `com.apple.security.virtualization` entitlement set to `true`.

**Manual codesigning** (if needed outside the build task):

```bash
codesign --entitlements matchlock.entitlements -f -s - bin/matchlock
```

The entitlements file (`matchlock.entitlements`) is in the repository root:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.virtualization</key>
    <true/>
</dict>
</plist>
```

### Linux

The `sudo ./bin/matchlock setup linux` command performs six operations:

1. **Installs Firecracker** from GitHub releases into `/usr/local/bin/`
2. **Adds the current user to the `kvm` group** for `/dev/kvm` access
3. **Sets Linux capabilities** on the matchlock binary: `cap_net_admin,cap_net_raw+ep`
4. **Configures `/dev/net/tun`**: creates a `netdev` group, adds the user, sets group ownership and mode `0660`
5. **Enables IP forwarding**: writes `net.ipv4.ip_forward = 1` to `/etc/sysctl.d/99-matchlock.conf`
6. **Loads nftables kernel module**: runs `modprobe nf_tables`

After running setup, **log out and back in** for group changes (`kvm`, `netdev`) to take effect.

**Setup flags:**

```bash
sudo ./bin/matchlock setup linux \
  --user <username>              # Override target user (default: $SUDO_USER or current)
  --binary <path>                # Override binary path for setcap (default: auto-detect)
  --install-dir /usr/local/bin   # Firecracker install directory
  --skip-firecracker             # Skip Firecracker installation
  --skip-permissions             # Skip permission/capability setup
  --skip-network                 # Skip network configuration
```

**Capabilities note:** The `cap_net_admin,cap_net_raw` capabilities are required for creating TAP interfaces and configuring nftables rules. These capabilities are **lost on every rebuild** -- you must re-run `sudo setcap cap_net_admin,cap_net_raw+ep <path>` after rebuilding the binary, or re-run `sudo ./bin/matchlock setup linux --skip-firecracker --skip-network`.

**KVM verification:**

```bash
ls -la /dev/kvm
# Should show: crw-rw---- root kvm
```

If `/dev/kvm` does not exist, enable virtualization in your BIOS/UEFI, or load the KVM module:

```bash
sudo modprobe kvm kvm_intel   # Intel CPUs
sudo modprobe kvm kvm_amd     # AMD CPUs
```

## Installation

### Install to System Path

```bash
mise run install
# Installs bin/matchlock to /usr/local/bin/matchlock (requires sudo)
```

### Install to User Path

If you prefer `~/.local/bin/`:

```bash
mkdir -p ~/.local/bin
cp bin/matchlock ~/.local/bin/matchlock

# macOS: re-codesign after copying
codesign --entitlements matchlock.entitlements -f -s - ~/.local/bin/matchlock

# Linux: re-apply capabilities after copying
sudo setcap cap_net_admin,cap_net_raw+ep ~/.local/bin/matchlock
```

### Homebrew (Pre-Built)

Pre-built binaries are available via Homebrew:

```bash
brew tap jingkaihe/essentials
brew install matchlock
```

## Kernel and Image Cache

Matchlock downloads a pre-built Linux kernel (version 6.1.137) from GHCR on first use. It is cached at:

```
~/.cache/matchlock/kernels/<version>/kernel-arm64   # macOS
~/.cache/matchlock/kernels/<version>/kernel          # Linux x86_64
```

Override the kernel path with the `MATCHLOCK_KERNEL` environment variable:

```bash
export MATCHLOCK_KERNEL=/path/to/custom/kernel
```

Image rootfs layers and metadata are stored at:

```
~/.cache/matchlock/images/
  blobs/sha256_<digest>.erofs     # Shared EROFS layer blobs
  metadata.db                      # SQLite image metadata
```

VM state and lifecycle data are stored at:

```
~/.matchlock/
  state.db                         # SQLite: VMs, subnets, lifecycle
  vms/<vm-id>/                     # Per-VM sockets, logs, overlay disks
```

## Environment Variables

| Variable | Description |
|----------|------------|
| `MATCHLOCK_KERNEL` | Override kernel path (skip auto-download) |
| `MATCHLOCK_BIN` | Override binary path for SDK client auto-detection |

Matchlock also reads environment variables prefixed with `MATCHLOCK_` for any CLI flag (via Viper). Dashes and dots in flag names become underscores. For example, `--allow-host` can be set via `MATCHLOCK_RUN_ALLOW_HOST`.

## Verifying Your Setup

### macOS

```bash
./bin/matchlock run --image alpine:latest cat /etc/os-release
```

### Linux

```bash
matchlock run --image alpine:latest cat /etc/os-release
```

On first run, Matchlock will:

1. Download the Linux kernel from GHCR (if not cached)
2. Pull the `alpine:latest` image from a container registry
3. Build EROFS layer blobs and store them in the image cache
4. Boot a micro-VM and execute the command

Subsequent runs with the same image will skip steps 1-3 and boot in under a second.

## Troubleshooting

### macOS: "process is not entitled"

The binary is not codesigned with the virtualization entitlement. Re-run `mise run build` or codesign manually:

```bash
codesign --entitlements matchlock.entitlements -f -s - bin/matchlock
```

### Linux: "operation not permitted" on TAP creation

Capabilities are missing from the binary. Re-apply them:

```bash
sudo setcap cap_net_admin,cap_net_raw+ep $(which matchlock)
```

### Linux: "permission denied" on /dev/kvm

Your user is not in the `kvm` group, or you have not logged out and back in after setup:

```bash
sudo usermod -aG kvm $USER
# Then log out and back in
```

### Alpine/musl images: Node.js segfaults

Alpine Linux uses musl libc, which can cause segfaults in some Node.js versions when running inside Matchlock VMs. Use Debian-based images instead:

```bash
# Bad: may segfault
matchlock run --image node:22-alpine ...

# Good: stable
matchlock run --image node:22-bookworm-slim ...
```

### Stale image cache after Docker rebuild

If you rebuild a Docker image and import it into Matchlock, you may need to clear the image cache:

```bash
rm -rf ~/.cache/matchlock/images
```

### Full local reset

To completely reset Matchlock state and cache:

```bash
matchlock kill --all
matchlock prune
rm -rf ~/.matchlock
rm -rf ~/.cache/matchlock
```
