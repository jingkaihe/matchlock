# Matchlock

A lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Features

- **Secure Execution**: Code runs in isolated Firecracker micro-VMs
- **Network MITM**: All HTTP/HTTPS traffic intercepted via gVisor userspace TCP/IP
- **Secret Protection**: Secrets never enter the VM, only placeholders
- **Host Allowlisting**: Control which hosts code can access
- **Programmable VFS**: Overlay filesystems with copy-on-write
- **Fast Boot**: <1 second VM startup time

## Quick Start

### Prerequisites

- Linux x86_64 with KVM support
- Go 1.21+
- Root access (for TAP devices)

### Install

```bash
# Clone
git clone https://github.com/jingkaihe/matchlock.git
cd matchlock

# Full setup (installs Firecracker, builds kernel/rootfs, installs CLI)
make setup

# Or step by step:
make install-firecracker    # Install Firecracker
make images                 # Build kernel + rootfs (~30 min)
make install                # Install sandbox CLI
```

### Quick Test

```bash
# Download pre-built images for testing (faster than building)
./scripts/download-kernel.sh
./scripts/download-rootfs.sh

# Test Firecracker directly
firecracker --config-file /tmp/fc-config.json
```

### Usage

```bash
# Run a command
sudo sandbox run python -c "print('Hello from sandbox')"

# With network allowlist
sudo sandbox run --allow-host "api.openai.com" python script.py

# List running sandboxes
sandbox list

# Kill a sandbox
sandbox kill vm-abc123
```

## Architecture

```
┌─────────────────────────────────────────────────┐
│                    Host                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────┐  │
│  │  Sandbox    │  │  Policy     │  │   VFS   │  │
│  │    CLI      │──│  Engine     │  │ Server  │  │
│  └─────────────┘  └─────────────┘  └─────────┘  │
│         │              │                 │       │
│         ▼              ▼                 │       │
│  ┌─────────────────────────────┐        │       │
│  │   gVisor TCP/IP + TLS MITM  │        │       │
│  └─────────────────────────────┘        │       │
│              │                          │       │
├──────────────│──────────────────────────│───────┤
│              │      Vsock               │       │
│  ┌───────────┴──────────────────────────┴─────┐ │
│  │            Firecracker VM                  │ │
│  │  ┌─────────────┐  ┌─────────────────────┐  │ │
│  │  │ Guest Agent │  │ /workspace (FUSE)   │  │ │
│  │  └─────────────┘  └─────────────────────┘  │ │
│  │         Alpine Linux + Python/Node.js      │ │
│  └────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────┘
```

## Documentation

- [Setup Guide](docs/SETUP.md) - Complete installation instructions
- [Architecture](docs/ARCHITECTURE.md) - Technical design details
- [AGENTS.md](AGENTS.md) - Developer reference

## Build Commands

```bash
make build          # Build CLI
make build-all      # Build CLI + guest binaries
make test           # Run tests
make images         # Build kernel + rootfs
make help           # Show all targets
```

## Configuration

Environment variables:

```bash
export SANDBOX_KERNEL=/opt/sandbox/kernel
export SANDBOX_ROOTFS=/opt/sandbox/rootfs-standard.ext4
```

## Project Structure

```
matchlock/
├── cmd/
│   ├── sandbox/          # CLI
│   ├── guest-agent/      # In-VM command executor
│   └── guest-fused/      # In-VM FUSE daemon
├── pkg/
│   ├── api/              # Core types
│   ├── vm/linux/         # Firecracker backend
│   ├── net/              # gVisor network + TLS MITM
│   ├── policy/           # Security policies
│   ├── vfs/              # Virtual filesystem
│   ├── vsock/            # Host-guest communication
│   ├── state/            # VM state management
│   └── rpc/              # JSON-RPC handler
├── scripts/              # Build scripts
└── docs/                 # Documentation
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
