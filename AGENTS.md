# Matchlock - Go-Based Cross-Platform Sandbox

A lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Tech Stack

- **Language**: Go 1.25
- **VM Backend**: Firecracker micro-VMs (Linux), Virtualization.framework (macOS/Apple Silicon)
- **Network**: gVisor tcpip userspace TCP/IP stack with HTTP/TLS MITM
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
│   ├── image/            # OCI/Docker image builder
│   ├── kernel/           # Kernel version management and OCI distribution
│   ├── sandbox/          # Core sandbox management
│   ├── vm/               # VM backend interface
│   │   └── linux/        # Linux/Firecracker implementation
│   ├── net/              # Network stack (TAP, HTTP/TLS MITM, CA injection)
│   ├── policy/           # Policy engine (allowlists, secrets)
│   ├── vfs/              # Virtual filesystem providers and server
│   ├── vsock/            # Vsock communication layer
│   ├── state/            # VM state management
│   └── rpc/              # JSON-RPC handler
├── scripts/              # Build scripts for kernel/rootfs
└── bin/                  # Built binaries
```

## Build Commands

```bash
# Build all packages
go build ./...

# Build CLI binary
go build -o bin/matchlock ./cmd/matchlock

# Build guest binaries (static for rootfs)
CGO_ENABLED=0 go build -o bin/guest-agent ./cmd/guest-agent
CGO_ENABLED=0 go build -o bin/guest-fused ./cmd/guest-fused

# Run tests
go test ./...

# Format code
go fmt ./...

# Build kernel (requires kernel build tools)
./scripts/build-kernel.sh

# Build rootfs (requires root and Alpine tools)
sudo ./scripts/build-rootfs.sh
```

## CLI Usage

```bash
# Build rootfs from container image
matchlock build alpine:latest
matchlock build python:3.12-alpine
matchlock build ubuntu:22.04

# Run with container image
matchlock run --image alpine:latest cat /etc/os-release
matchlock run --image python:3.12-alpine python3 --version

# Run with pre-built image variants
matchlock run python script.py
matchlock run --image standard python script.py

# Interactive mode (like docker -it)
matchlock run -it python3
matchlock run --image alpine:latest -it sh

# With network allowlist
matchlock run --allow-host "api.openai.com" python agent.py

# HTTPS with automatic CA injection
matchlock run --allow-host "httpbin.org" curl https://httpbin.org/get

# With secrets (MITM proxy replaces placeholder with real value)
export ANTHROPIC_API_KEY=sk-xxx
matchlock run --image python:3.12-alpine \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  python call_api.py

# Inline secret value
matchlock run --secret "API_KEY=sk-xxx@api.example.com" python script.py

# List sandboxes
matchlock list

# Kill a sandbox
matchlock kill vm-abc123

# RPC mode (for programmatic access)
matchlock --rpc
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
- Unix socket pairs for network I/O (passed to gVisor stack)
- Native virtio-vsock for host-guest communication
- Same guest agent protocol as Linux (full feature parity)

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
- Transparent proxy for HTTP/HTTPS interception using iptables DNAT
- HTTP interception with Host header-based policy checking
- HTTPS MITM via dynamic certificate generation
- `CAPool`: CA certificate generation and per-domain cert caching
- `TransparentProxy`: Listens on host ports, uses SO_ORIGINAL_DST for destination
- `IPTablesRules`: Manages PREROUTING DNAT and FORWARD rules
- Policy-based request/response modification
- NAT masquerade auto-detects default interface

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

## Environment Variables

- `MATCHLOCK_KERNEL`: Path to kernel image
- `MATCHLOCK_ROOTFS`: Path to rootfs image

## JSON-RPC Methods

- `create`: Initialize VM with configuration
- `exec`: Execute command in sandbox
- `write_file`: Write file to sandbox
- `read_file`: Read file from sandbox
- `list_files`: List directory contents
- `close`: Shutdown VM

## CA Certificate Injection

The sandbox intercepts HTTPS traffic via MITM. To trust the CA in guest:

```bash
# Environment variables (auto-injected)
SSL_CERT_FILE=/etc/ssl/certs/sandbox-ca.crt
REQUESTS_CA_BUNDLE=/etc/ssl/certs/sandbox-ca.crt
NODE_EXTRA_CA_CERTS=/etc/ssl/certs/sandbox-ca.crt

# Or run install script
/tmp/install-ca.sh
```

## Building Images

### Kernel

Kernels are automatically downloaded from GHCR on first run. No manual setup required.

**Automatic Download (recommended):**
```bash
# Kernel is auto-downloaded when you run matchlock
matchlock run --image alpine:latest echo hello

# Kernels are cached at ~/.cache/matchlock/kernels/{version}/
# x86_64: kernel
# arm64:  kernel-arm64
```

**Manual Override:**
```bash
# Use environment variable to specify custom kernel
MATCHLOCK_KERNEL=/path/to/kernel matchlock run ...
```

**Building Kernels Locally:**

The kernel build uses Docker with Ubuntu 22.04 (GCC 11) for cross-compilation.

```bash
# Build both architectures (default)
./scripts/build-kernel.sh

# Build specific architecture
ARCH=x86_64 ./scripts/build-kernel.sh
ARCH=arm64 ./scripts/build-kernel.sh

# Custom version
KERNEL_VERSION=6.1.140 ./scripts/build-kernel.sh
```

Output: `~/.cache/matchlock/kernels/{version}/kernel[-arm64]`

**Publishing Kernels to GHCR:**

```bash
# Build and publish (requires GHCR authentication)
./scripts/build-kernel.sh
./scripts/publish-kernel.sh

# With custom version
KERNEL_VERSION=6.1.140 TAG_LATEST=true ./scripts/publish-kernel.sh
```

**Kernel Distribution (pkg/kernel):**
- Version: `6.1.137` (constant in `pkg/kernel/kernel.go`)
- Registry: `ghcr.io/jingkaihe/matchlock/kernel:{version}`
- Multi-platform manifest with x86_64 and arm64 variants
- Pulled using go-containerregistry with platform selection

Required kernel options for Firecracker v1.8+:
- `CONFIG_ACPI=y` and `CONFIG_PCI=y` - Required for virtio device initialization
- `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y` - Parse `virtio_mmio.device=` from cmdline
- `CONFIG_VSOCKETS=y` and `CONFIG_VIRTIO_VSOCKETS=y` - Host-guest communication
- `CONFIG_FUSE_FS=y` - VFS support
- `CONFIG_IP_PNP=y` - Required for kernel `ip=` boot parameter (network configuration)

### Rootfs

Requirements: root, apk (Alpine package manager)

```bash
IMAGE=standard OUTPUT_DIR=~/.cache/matchlock sudo ./scripts/build-rootfs.sh
```

Image variants:
- `minimal`: Base Alpine only
- `standard`: Python, Node.js, Git
- `full`: Go, Rust, dev tools

## macOS Setup (Apple Silicon)

### Prerequisites
- macOS 11+ (Big Sur or later) on Apple Silicon
- Go 1.25+ with CGO enabled
- Code signing with virtualization entitlement
- e2fsprogs (for `--image` option to build rootfs from container images)

```bash
# Install e2fsprogs for ext4 filesystem creation
brew install e2fsprogs
brew link e2fsprogs  # Makes mke2fs and debugfs available in PATH
```

### Build & Sign CLI

```bash
# Build CLI
go build -o bin/matchlock ./cmd/matchlock

# Build guest binaries for ARM64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ~/.cache/matchlock/guest-agent ./cmd/guest-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ~/.cache/matchlock/guest-fused ./cmd/guest-fused

# Sign with virtualization entitlement (required!)
cat > matchlock.entitlements << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.virtualization</key>
    <true/>
</dict>
</plist>
EOF
codesign --entitlements matchlock.entitlements -f -s - bin/matchlock
```

### Kernel & Rootfs

The macOS backend requires:
1. **ARM64 Linux kernel** - Automatically downloaded from GHCR on first run
2. **ext4 rootfs** - Either built from container image (`--image` option) or custom rootfs

Kernels are auto-downloaded to `~/.cache/matchlock/kernels/{version}/kernel-arm64`.

**Option A: Use container images (recommended)**

With e2fsprogs installed, use `--image` to automatically pull and build rootfs from any container image:

```bash
# Run with container image - automatically pulls linux/arm64 variant
matchlock run --image alpine:latest echo hello
matchlock run --image python:3.12-alpine python3 --version
matchlock run --image ubuntu:24.04 cat /etc/os-release
```

**Option B: Build custom rootfs via Lima**

For a pre-built rootfs without container image support:

```bash
limactl start --name=rootfs-builder template://alpine
limactl shell rootfs-builder -- sudo ./scripts/create-rootfs-lima.sh
limactl copy rootfs-builder:/tmp/rootfs.ext4 ~/.cache/matchlock/rootfs.ext4
limactl delete rootfs-builder
```

### Usage

```bash
# Run with container image (recommended)
matchlock run --image alpine:latest echo 'Hello from macOS VM!'

# Interactive shell
matchlock run --image alpine:latest -it sh

# Run a command with pre-built rootfs
matchlock run echo 'Hello from macOS VM!'

# With explicit rootfs path
MATCHLOCK_ROOTFS=~/.cache/matchlock/rootfs.ext4 matchlock run uname -a
```

### macOS-Specific Notes
- Uses Apple Virtualization.framework via `github.com/Code-Hex/vz/v3`
- Native virtio-vsock (no Unix socket CONNECT protocol like Firecracker)
- Network uses NAT mode with DHCP (no traffic interception yet)
- Image builder uses `mke2fs` and `debugfs` from e2fsprogs to create ext4 without mounting

## Configuration

### Workspace Path
The VFS mount point in the guest is configurable via `VFSConfig.Workspace`. Default is `/workspace`.

```go
// Go SDK example with custom workspace
opts := sdk.CreateOptions{
    Workspace: "/home/user/code",
    // ...
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

## Notes

- Requires root/CAP_NET_ADMIN for TAP device creation
- Firecracker binary must be installed for VM operation
- Guest agent and FUSE daemon auto-start via OpenRC

## Known Limitations

### gVisor Dependency
Uses gVisor's `go` branch (`gvisor.dev/gvisor@go`) which is specifically maintained for Go imports. The `master` branch has test file conflicts (`bridge_test.go` declares wrong package). See [PR #10593](https://github.com/google/gvisor/pull/10593) for details.

### Test Coverage
Tests implemented for: vfs (memory, overlay, readonly, router), policy, net (tls, ca_inject). Additional tests needed for: vm/linux, rpc, state, vsock (require mocking).
