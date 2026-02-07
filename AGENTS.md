# Matchlock - Go-Based Cross-Platform Sandbox

A lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Tech Stack

- **Language**: Go 1.25
- **VM Backend**: Firecracker micro-VMs (Linux), Virtualization.framework (macOS/Apple Silicon only, Intel not supported)
- **Network**: nftables transparent proxy (Linux), Apple native NAT or gVisor userspace TCP/IP stack (macOS), HTTP/TLS MITM
- **Filesystem**: Pluggable VFS providers (Memory, RealFS, Readonly, Overlay)
- **Communication**: Vsock for host-guest, JSON-RPC 2.0 for API

## Project Structure

```
matchlock/
├── cmd/
│   ├── matchlock/        # CLI entrypoint
│   ├── guest-agent/      # In-VM agent for command execution
│   └── guest-fused/      # In-VM FUSE daemon for VFS
├── pkg/
│   ├── api/              # Core types (Config, VM, Events, Hooks)
│   ├── client/           # Client library for connecting to sandboxes
│   ├── image/            # OCI/Docker image builder
│   ├── kernel/           # Kernel version management and OCI distribution
│   ├── net/              # Network stack (TAP, HTTP/TLS MITM, CA injection)
│   ├── policy/           # Policy engine (allowlists, secrets)
│   ├── rpc/              # JSON-RPC handler
│   ├── sandbox/          # Core sandbox management + exec relay
│   ├── sdk/              # Go SDK for programmatic sandbox usage
│   ├── state/            # VM state management
│   ├── vfs/              # Virtual filesystem providers and server
│   │   └── client/       # VFS client for guest FUSE
│   ├── vm/               # VM backend interface
│   │   ├── darwin/       # macOS/Virtualization.framework implementation
│   │   └── linux/        # Linux/Firecracker implementation
│   └── vsock/            # Vsock communication layer
├── internal/
│   ├── images/           # Internal image handling utilities
│   └── mitm/             # MITM proxy internals
├── guest/
│   ├── kernel/           # Guest kernel Dockerfile + per-arch .config files
│   ├── initramfs/        # Guest initramfs setup
│   └── fused/            # Guest FUSE daemon resources
├── sdk/
│   └── python/           # Python SDK
├── examples/
│   ├── go/               # Go usage examples
│   └── python/           # Python usage examples
├── scripts/              # Build scripts for kernel/rootfs
└── bin/                  # Built binaries
```

## Build & Development

This project uses [mise](https://mise.jdx.dev/) for task management and dev dependencies. Run `mise tasks` to list all available tasks.

```bash
mise install         # Install dev dependencies (Go, golangci-lint, crane)
mise run build       # Build CLI (codesigned on macOS) + guest binaries
mise run test        # Run tests
mise run check       # Run all checks (fmt, vet, lint, test)
mise run fmt         # Format code
```

**macOS:**
```bash
mise run setup:darwin   # Build + codesign CLI + guest binaries
```

**Linux:**
```bash
mise run setup          # Full setup (firecracker + images + install)
# Or step by step:
sudo ./bin/matchlock setup linux   # Configure Firecracker, KVM, capabilities, nftables
```

**Kernels** are auto-downloaded from GHCR on first run. Override with `MATCHLOCK_KERNEL=/path/to/kernel`.

**Testing:**
```bash
mise run test              # Unit tests
mise run test:acceptance   # Acceptance tests (requires VM support, builds first)
mise run test:coverage     # Coverage report
```

**Release:**
```bash
mise run push-tag          # Push v$VERSION tag → triggers GitHub Actions release
mise run release           # Manual release (cross-builds + uploads to GitHub)
```

## CI/CD

- **CI** (`.github/workflows/ci.yml`): Runs on PRs and pushes to `main`. Builds + tests on Linux (ubuntu-latest) and macOS (macos-latest ARM64 only).
- **Release** (`.github/workflows/release.yml`): Triggered by `v*` tags. Cross-builds for Linux amd64/arm64, macOS arm64 only, publishes GitHub release with all binaries.
- **Kernel** (`.github/workflows/kernel.yml`): Builds and publishes kernel images to GHCR.

## CLI Usage

```bash
# Run with container image (--image is required)
matchlock run --image alpine:latest cat /etc/os-release
matchlock run --image python:3.12-alpine python3 --version

# Keep sandbox alive after command exits (like docker run without --rm)
matchlock run --image alpine:latest --rm=false echo hello
# Prints VM ID (e.g., vm-abc12345), VM stays running

# Start sandbox without running a command
matchlock run --image alpine:latest --rm=false

# Execute command in a running sandbox
matchlock exec vm-abc12345 echo hello
matchlock exec vm-abc12345 -it sh

# Interactive mode (like docker -it)
matchlock run --image alpine:latest -it sh

# With network allowlist
matchlock run --image python:3.12-alpine --allow-host "api.openai.com" python agent.py

# With secrets (MITM proxy replaces placeholder with real value)
export ANTHROPIC_API_KEY=sk-xxx
matchlock run --image python:3.12-alpine \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  python call_api.py

# Lifecycle management
matchlock list                     # List sandboxes
matchlock kill vm-abc123           # Kill a sandbox
matchlock kill --all               # Kill all running sandboxes
matchlock rm vm-abc123             # Remove stopped sandbox state
matchlock prune                    # Remove all stopped/crashed state

# Pre-build rootfs (caches for faster startup)
matchlock build alpine:latest

# RPC mode (for programmatic access)
matchlock rpc
```

## Key Components

### VM Backend

**Linux (`pkg/vm/linux`):**
- Creates TAP devices for network virtualization
- Generates Firecracker configuration with vsock
- Manages VM lifecycle (start, stop, exec)
- Vsock-based command execution and ready signaling

**macOS (`pkg/vm/darwin`):**
- Uses Apple Virtualization.framework via code-hex/vz
- Two network modes: native NAT (no interception) or Unix socket pairs (passed to gVisor userspace TCP/IP stack for interception)
- Native virtio-vsock for host-guest communication
- Same guest agent protocol as Linux (full feature parity)
- NAT mode selected by default; interception mode activated when `--allow-host` or `--secret` flags are used

### Guest Agent (`cmd/guest-agent`)
- Runs inside VM to handle exec requests
- Ready signal service on vsock port 5002
- Command execution service on vsock port 5000

### Guest FUSE Daemon (`cmd/guest-fused`)
- Mounts VFS from host via vsock at configurable workspace (default: /workspace)
- Uses go-fuse library for POSIX-compliant FUSE implementation
- Reads workspace path from kernel cmdline (`matchlock.workspace=`)
- Connects to VFS server on vsock port 5001

### Image Builder (`pkg/image`)
- Pulls OCI/Docker images from any registry (Docker Hub, GHCR, etc.)
- Extracts image layers and converts to ext4 rootfs
- Injects matchlock guest components (guest-agent, guest-fused)
- Creates minimal init script that runs as PID 1
- Caches built images by digest in `~/.cache/matchlock/images/`
- Supports any Linux container image (Alpine, Ubuntu, Debian, etc.)

### Exec Relay (`pkg/sandbox/exec_relay.go`)
- Unix socket server (`~/.matchlock/vms/{id}/exec.sock`) enabling cross-process exec
- The `run --rm=false` host process serves the relay; `matchlock exec` connects to it
- Supports both non-interactive and interactive (TTY) exec
- Works cross-platform (Linux + macOS) without direct vsock access from external processes

### Policy Engine (`pkg/policy`)
- Host allowlisting with glob patterns
- Secret injection with placeholder replacement
- Private IP blocking

### VFS Providers (`pkg/vfs`)
- `MemoryProvider`: In-memory filesystem
- `RealFSProvider`: Host directory mapping
- `ReadonlyProvider`: Read-only wrapper
- `OverlayProvider`: Copy-on-write overlay
- `MountRouter`: Route paths to providers
- `VFSServer`: CBOR protocol server for guest FUSE

### Network Stack (`pkg/net`)

**Linux:**
- `TransparentProxy`: nftables DNAT redirects ports 80/443 to host proxy, uses `SO_ORIGINAL_DST` to recover original destination
- `NFTablesRules`: Manages PREROUTING DNAT and FORWARD rules via netlink (no shell commands)
- NAT masquerade auto-detects default interface
- Kernel handles TCP/IP; only HTTP/HTTPS traffic goes through userspace

**macOS (two modes):**
- **NAT mode** (default): Uses Apple Virtualization.framework's built-in NAT with DHCP — no traffic interception, simplest path for unrestricted networking
- **Interception mode** (when `--allow-host` or `--secret` is used): Unix socket pairs pass raw Ethernet frames to gVisor userspace TCP/IP stack (`socketPairEndpoint`); promiscuous + spoofing mode lets gVisor act as transparent gateway with TCP/UDP forwarders intercepting all connections at L4

**Shared:**
- `HTTPInterceptor`: HTTP interception with Host header-based policy checking
- HTTPS MITM via dynamic certificate generation
- `CAPool`: CA certificate generation and per-domain cert caching
- Policy-based request/response modification

### Vsock Layer (`pkg/vsock`)
- Pure Go vsock implementation (AF_VSOCK=40)
- Host-guest communication without network
- Message protocol for exec requests/responses

### State Management (`pkg/state`)
- VM state tracking in `~/.matchlock/vms/`
- **SubnetAllocator**: Dynamic subnet allocation for multiple VMs
  - Allocates unique /24 subnets from 192.168.100.0 to 192.168.254.0
  - Persists allocations to `~/.matchlock/subnets/`
  - Auto-released on VM close

## Vsock Ports

| Port | Service | Direction |
|------|---------|-----------|
| 5000 | Command execution | Host → Guest |
| 5001 | VFS protocol (FUSE) | Guest → Host |
| 5002 | Ready signal | Host → Guest |

## Firecracker Vsock Protocol

Firecracker exposes vsock via Unix domain sockets with two connection patterns:

### Host-Initiated Connections (exec, ready)
1. Host connects to base UDS socket (`vsock.sock`)
2. Host sends `CONNECT <port>\n` (e.g., `CONNECT 5000\n`)
3. Firecracker responds with `OK <assigned_port>\n`
4. Connection is established to guest service on that port

### Guest-Initiated Connections (VFS)
1. Host listens on `{uds_path}_{port}` (e.g., `vsock.sock_5001`)
2. Guest connects to CID 2 (host) and port
3. Firecracker forwards to the Unix socket

**Important**: The `{uds_path}_{port}` sockets are only for guest-initiated connections. Host-initiated connections must use the CONNECT protocol on the base socket.

## JSON-RPC Methods

- `create`: Initialize VM with configuration
- `exec`: Execute command in sandbox (buffered — returns all stdout/stderr in response)
- `exec_stream`: Execute command with streaming output (stdout/stderr sent as notifications before final response)
- `write_file`: Write file to sandbox
- `read_file`: Read file from sandbox
- `list_files`: List directory contents
- `close`: Shutdown VM

The RPC handler dispatches `exec`, `exec_stream`, file, and list operations concurrently. `create` and `close` are serialized (drain in-flight requests first).

### exec_stream protocol

Stream notifications (no `id`):
```json
{"jsonrpc":"2.0","method":"exec_stream.stdout","params":{"id":2,"data":"<base64>"}}
{"jsonrpc":"2.0","method":"exec_stream.stderr","params":{"id":2,"data":"<base64>"}}
```

Final response:
```json
{"jsonrpc":"2.0","id":2,"result":{"exit_code":0,"duration_ms":123}}
```

## CA Certificate Injection

The sandbox intercepts HTTPS traffic via MITM. The CA certificate is automatically injected into the rootfs at `/etc/ssl/certs/matchlock-ca.crt` before the VM starts.

Environment variables are auto-injected:

```bash
SSL_CERT_FILE=/etc/ssl/certs/matchlock-ca.crt
REQUESTS_CA_BUNDLE=/etc/ssl/certs/matchlock-ca.crt
CURL_CA_BUNDLE=/etc/ssl/certs/matchlock-ca.crt
NODE_EXTRA_CA_CERTS=/etc/ssl/certs/matchlock-ca.crt
```

## Kernel

- Version: `6.1.137` (constant in `pkg/kernel/kernel.go`)
- Registry: `ghcr.io/jingkaihe/matchlock/kernel:{version}`
- Multi-platform manifest with x86_64 and arm64 variants
- Build: `mise run kernel:build` (uses Docker BuildKit, configs in `guest/kernel/{x86_64,arm64}.config`)
- Publish: `mise run kernel:publish`

Required kernel options for Firecracker v1.8+:
- `CONFIG_ACPI=y` and `CONFIG_PCI=y` - Required for virtio device initialization
- `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y` - Parse `virtio_mmio.device=` from cmdline
- `CONFIG_VSOCKETS=y` and `CONFIG_VIRTIO_VSOCKETS=y` - Host-guest communication
- `CONFIG_FUSE_FS=y` - VFS support
- `CONFIG_IP_PNP=y` - Required for kernel `ip=` boot parameter (network configuration)
- `CONFIG_CGROUPS=y` - Cgroup support (v1 and v2) with cpu, memory, pids, io, cpuset, freezer controllers

## Configuration

### Workspace Path
The VFS mount point in the guest is configurable via `VFSConfig.Workspace`. Default is `/workspace`.

```go
opts := sdk.CreateOptions{
    Workspace: "/home/user/code",
}
```

The workspace path is passed to the guest FUSE daemon via kernel cmdline parameter `matchlock.workspace=`.

### API Config Structure
```go
type VFSConfig struct {
    Workspace    string                 `json:"workspace,omitempty"`  // Guest mount point (default: /workspace)
    Mounts       map[string]MountConfig `json:"mounts,omitempty"`     // VFS provider mounts
}
```

## Known Limitations

### gVisor Dependency
Uses gVisor's `go` branch (`gvisor.dev/gvisor@go`) which is specifically maintained for Go imports. The `master` branch has test file conflicts (`bridge_test.go` declares wrong package). See [PR #10593](https://github.com/google/gvisor/pull/10593) for details. gVisor's userspace TCP/IP stack is only used on macOS (where nftables is unavailable); Linux uses nftables-based transparent proxy instead.

### Test Coverage
Tests implemented for: vfs (memory, overlay, readonly, router), policy, net (tls, ca_inject). Additional tests needed for: vm/linux, rpc, state, vsock (require mocking).
