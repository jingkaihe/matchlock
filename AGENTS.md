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
│   ├── matchlock/        # CLI entrypoint
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
# Run a command in sandbox
matchlock run python script.py

# With network allowlist
matchlock run --allow-host "api.openai.com" python agent.py

# List sandboxes
matchlock list

# Kill a sandbox
matchlock kill vm-abc123

# RPC mode (for programmatic access)
matchlock --rpc
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

The kernel build uses Docker with Ubuntu 22.04 (GCC 11) for compatibility with older kernel sources.

```bash
# Build kernel 6.1.137 (default)
OUTPUT_DIR=~/.cache/matchlock ./scripts/build-kernel.sh

# Custom version
KERNEL_VERSION=6.1.140 OUTPUT_DIR=~/.cache/matchlock ./scripts/build-kernel.sh
```

Required kernel options for Firecracker v1.8+:
- `CONFIG_ACPI=y` and `CONFIG_PCI=y` - Required for virtio device initialization
- `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y` - Parse `virtio_mmio.device=` from cmdline
- `CONFIG_VSOCKETS=y` and `CONFIG_VIRTIO_VSOCKETS=y` - Host-guest communication
- `CONFIG_FUSE_FS=y` - VFS support

### Rootfs

Requirements: root, apk (Alpine package manager)

```bash
IMAGE=standard OUTPUT_DIR=~/.cache/matchlock sudo ./scripts/build-rootfs.sh
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
