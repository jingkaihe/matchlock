# Matchlock

A lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Features

- **Secure Execution**: Code runs in isolated Firecracker micro-VMs
- **Container Images**: Run any Docker/OCI image (Alpine, Ubuntu, Python, etc.)
- **Network MITM**: All HTTP/HTTPS traffic intercepted via transparent proxy (iptables on Linux, gVisor userspace TCP/IP on macOS)
- **Secret Protection**: Secrets never enter the VM, only placeholders
- **Host Allowlisting**: Control which hosts code can access
- **Programmable VFS**: Overlay filesystems with copy-on-write
- **Fast Boot**: <1 second VM startup time

## Quick Start

### Prerequisites

- Linux x86_64 with KVM support
- Go 1.21+
- Root access (for TAP devices and image building)

### Install

```bash
# Clone
git clone https://github.com/jingkaihe/matchlock.git
cd matchlock

# Build binaries
make build-all

# Install Firecracker
make install-firecracker

# Build kernel (one-time, ~10 min)
make kernel
```

### Usage

```bash
# Run with any container image (auto-builds rootfs on first use)
sudo matchlock run --image alpine:latest cat /etc/os-release
sudo matchlock run --image python:3.12-alpine python3 --version
sudo matchlock run --image ubuntu:22.04 uname -a

# Interactive shell
sudo matchlock run --image alpine:latest -it sh

# Pre-build an image for faster subsequent runs
sudo matchlock build python:3.12-alpine

# With network allowlist
sudo matchlock run --image python:3.12-alpine --allow-host "api.openai.com" python script.py

# List running sandboxes
matchlock list

# Kill a sandbox
matchlock kill vm-abc123
```

### How Container Images Work

When you run `matchlock run --image <container-image>`:

1. **First run**: Pulls the image, extracts layers, injects matchlock components, creates ext4 rootfs
2. **Subsequent runs**: Uses cached rootfs (instant startup)

Images are cached in `~/.cache/matchlock/images/` by digest.

```bash
# First run - builds rootfs (~30s for alpine, longer for larger images)
$ sudo matchlock run --image alpine:latest cat /etc/alpine-release
Built rootfs from alpine:latest (527.0 MB)
3.21.0

# Second run - uses cache (instant)
$ sudo matchlock run --image alpine:latest cat /etc/alpine-release
Using cached image alpine:latest
3.21.0
```

## Architecture

```
┌─────────────────────────────────────────────────┐
│                    Host                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────┐  │
│  │  Matchlock  │  │  Policy     │  │   VFS   │  │
│  │    CLI      │──│  Engine     │  │ Server  │  │
│  └─────────────┘  └─────────────┘  └─────────┘  │
│         │              │                 │       │
│         ▼              ▼                 │       │
│  ┌─────────────────────────────┐        │       │
│  │  Transparent Proxy + TLS MITM │        │       │
│  └─────────────────────────────┘        │       │
│              │                          │       │
├──────────────│──────────────────────────│───────┤
│              │      Vsock               │       │
│  ┌───────────┴──────────────────────────┴─────┐ │
│  │            Firecracker VM                  │ │
│  │  ┌─────────────┐  ┌─────────────────────┐  │ │
│  │  │ Guest Agent │  │ /workspace (FUSE)   │  │ │
│  │  └─────────────┘  └─────────────────────┘  │ │
│  │       Any OCI Image (Alpine, Ubuntu, etc)  │ │
│  └────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────┘
```

## Documentation

- [AGENTS.md](AGENTS.md) - Developer reference

## Build Commands

```bash
make build          # Build CLI
make build-all      # Build CLI + guest binaries
make test           # Run tests
make kernel         # Build kernel
make help           # Show all targets
```

## Configuration

Environment variables:

```bash
export MATCHLOCK_KERNEL=~/.cache/matchlock/kernel
```

## Project Structure

```
matchlock/
├── cmd/
│   ├── matchlock/        # CLI
│   ├── guest-agent/      # In-VM command executor
│   └── guest-fused/      # In-VM FUSE daemon
├── pkg/
│   ├── api/              # Core types
│   ├── image/            # OCI image builder
│   ├── sandbox/          # Sandbox management
│   ├── vm/linux/         # Firecracker backend
│   ├── net/              # Network interception + TLS MITM
│   ├── policy/           # Security policies
│   ├── vfs/              # Virtual filesystem
│   ├── vsock/            # Host-guest communication
│   ├── state/            # VM state management
│   └── rpc/              # JSON-RPC handler
├── scripts/              # Build scripts
└── examples/             # SDK examples
```

## Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| Linux Kernel | 4.14 | 5.10+ |
| KVM | Required | - |
| RAM | 4GB | 8GB |
| Disk | 10GB | 20GB |

## License

MIT
