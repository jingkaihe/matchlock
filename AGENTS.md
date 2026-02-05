# Matchlock - Go-Based Cross-Platform Sandbox

A lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Tech Stack

- **Language**: Go 1.25
- **VM Backend**: Firecracker micro-VMs (Linux)
- **Network**: gVisor tcpip userspace TCP/IP stack with HTTP/TLS MITM
- **Filesystem**: Pluggable VFS providers (Memory, RealFS, Readonly, Overlay)
- **Communication**: Vsock for host-guest, JSON-RPC 2.0 for API

## Project Structure

```
matchlock/
├── cmd/
│   ├── sandbox/          # CLI entrypoint
│   ├── guest-agent/      # In-VM agent for command execution
│   └── guest-fused/      # In-VM FUSE daemon for VFS
├── pkg/
│   ├── api/              # Core types (Config, VM, Events, Hooks)
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
go build -o bin/sandbox ./cmd/sandbox

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
# Run a command in sandbox
sandbox run python script.py

# With network allowlist
sandbox run --allow-host "api.openai.com" python agent.py

# List sandboxes
sandbox list

# Kill a sandbox
sandbox kill vm-abc123

# RPC mode (for programmatic access)
sandbox --rpc
```

## Key Components

### VM Backend (`pkg/vm/linux`)
- Creates TAP devices for network virtualization
- Generates Firecracker configuration with vsock
- Manages VM lifecycle (start, stop, exec)
- Vsock-based command execution and ready signaling

### Guest Agent (`cmd/guest-agent`)
- Runs inside VM to handle exec requests
- Ready signal service on vsock port 5002
- Command execution service on vsock port 5000

### Guest FUSE Daemon (`cmd/guest-fused`)
- Mounts VFS from host via vsock at /workspace
- Full FUSE implementation (read, write, mkdir, etc.)
- Connects to VFS server on vsock port 5001

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
- gVisor tcpip userspace TCP/IP stack for packet interception
- HTTP/HTTPS interception and MITM via TCP forwarder
- Dynamic certificate generation with CA caching
- `CAInjector`: Scripts and env vars for CA trust in guest
- Policy-based request/response modification
- DNS forwarding to 8.8.8.8

### Vsock Layer (`pkg/vsock`)
- Pure Go vsock implementation (AF_VSOCK=40)
- Host-guest communication without network
- Message protocol for exec requests/responses

## Vsock Ports

| Port | Service |
|------|---------|
| 5000 | Command execution |
| 5001 | VFS protocol (FUSE) |
| 5002 | Ready signal |

## Environment Variables

- `SANDBOX_KERNEL`: Path to kernel image
- `SANDBOX_ROOTFS`: Path to rootfs image

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

Requirements: gcc, make, kernel headers, wget

```bash
KERNEL_VERSION=6.1.94 OUTPUT_DIR=/opt/sandbox ./scripts/build-kernel.sh
```

Enables: virtio-net, virtio-vsock, FUSE, ext4

### Rootfs

Requirements: root, apk (Alpine package manager)

```bash
IMAGE=standard OUTPUT_DIR=/opt/sandbox sudo ./scripts/build-rootfs.sh
```

Image variants:
- `minimal`: Base Alpine only
- `standard`: Python, Node.js, Git
- `full`: Go, Rust, dev tools

## Notes

- Requires root/CAP_NET_ADMIN for TAP device creation
- Firecracker binary must be installed for VM operation
- Guest agent and FUSE daemon auto-start via OpenRC

## Known Limitations

### gVisor Dependency
Uses gVisor's `go` branch (`gvisor.dev/gvisor@go`) which is specifically maintained for Go imports. The `master` branch has test file conflicts (`bridge_test.go` declares wrong package). See [PR #10593](https://github.com/google/gvisor/pull/10593) for details.

### Test Coverage
Tests implemented for: vfs (memory, overlay, readonly, router), policy, net (tls, ca_inject). Additional tests needed for: vm/linux, rpc, state, vsock (require mocking).
