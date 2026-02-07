# Matchlock

Matchlock is a CLI tool for running AI agents in ephemeral micro-VMs - with network allowlisting, secret injection via MITM proxy, and everything else blocked by default. Your secrets never enter the VM.

## Why Matchlock?

AI agents need to run code, but giving them unrestricted access to your machine is a risk. Matchlock lets you hand an agent a full Linux environment that boots in under a second - isolated, disposable, and locked down by default.

When your agent calls an API the real credentials are injected in-flight by the host. The sandbox only ever sees a placeholder. The network is sealed by default and nothing gets out unless you say so. Even if the agent is tricked into running something malicious your keys don't leak and there's nowhere for data to go. Inside the agent gets a full Linux environment to do whatever it needs. It can install packages and write files and make a mess. Outside your machine doesn't feel a thing. Every sandbox runs on its own copy-on-write filesystem that vanishes when you're done. Same CLI and same behaviour whether you're on a Linux server or a MacBook.

## Quick Start

### Prerequisites

- **Linux**: KVM support
- **macOS**: Apple Silicon

### Install

```bash
brew install jingkaihe/essentials/matchlock
```

Or add the tap first:

```bash
brew tap jingkaihe/essentials
brew install matchlock
```

#### Updating

```bash
brew update
brew upgrade matchlock
```

#### Build from source

If you prefer to build from source, see the [developer reference](AGENTS.md) for instructions using [mise](https://mise.jdx.dev/).

### Usage

```bash
# Basic
matchlock run --image alpine:latest cat /etc/os-release
matchlock run --image alpine:latest -it sh

# Network allowlist
matchlock run --image python:3.12-alpine \
  --allow-host "api.openai.com" python agent.py

# Secret injection (never enters the VM)
export ANTHROPIC_API_KEY=sk-xxx
matchlock run --image python:3.12-alpine \
  --secret ANTHROPIC_API_KEY@api.anthropic.com python call_api.py

# Long-lived sandboxes
matchlock run --image alpine:latest --rm=false   # prints VM ID
matchlock exec vm-abc12345 -it sh                # attach to it

# Lifecycle
matchlock list | kill | rm | prune

# Build from Dockerfile (uses BuildKit-in-VM)
matchlock build -f Dockerfile -t myapp:latest .

# Pre-build rootfs from registry image (caches for faster startup)
matchlock build alpine:latest

# Image management
matchlock image ls                                           # List all images
matchlock image rm myapp:latest                              # Remove a local image
docker save myapp:latest | matchlock image import myapp:latest  # Import from tarball
```

## SDK

Beyond the CLI, Matchlock ships with Go and Python SDKs for embedding sandboxes directly in your application. Launch a VM, exec commands, stream output, and write files - all programmatically.

**Go**

```go
import "github.com/jingkaihe/matchlock/pkg/sdk"

client, _ := sdk.NewClient(sdk.DefaultConfig())
defer client.Close()

sandbox := sdk.New("python:3.12-alpine").
    AllowHost("api.anthropic.com").
    AddSecret("ANTHROPIC_API_KEY", os.Getenv("ANTHROPIC_API_KEY"), "api.anthropic.com")

client.Launch(sandbox)

// The VM only ever sees a placeholder - the real key never enters the sandbox
result, _ := client.Exec("echo $ANTHROPIC_API_KEY")
fmt.Print(result.Stdout) // prints "SANDBOX_SECRET_a1b2c3d4..."

client.WriteFile("/workspace/ask.py", script)
client.ExecStream("uv run /workspace/ask.py", os.Stdout, os.Stderr)
```

**Python**

```python
from matchlock import Client, Config, Sandbox

sandbox = (
    Sandbox("python:3.12-alpine")
    .allow_host("api.anthropic.com")
    .add_secret("ANTHROPIC_API_KEY", os.environ["ANTHROPIC_API_KEY"], "api.anthropic.com")
)

with Client(Config()) as client:
    client.launch(sandbox)
    client.write_file("/workspace/ask.py", script)
    client.exec_stream("uv run /workspace/ask.py", stdout=sys.stdout, stderr=sys.stderr)
```

See full examples in [`examples/go`](examples/go/main.go) and [`examples/python`](examples/python/main.py).

## Architecture

```mermaid
graph LR
    subgraph Host
        CLI["Matchlock CLI"]
        Policy["Policy Engine"]
        Proxy["Transparent Proxy + TLS MITM"]
        VFS["VFS Server"]

        CLI --> Policy
        CLI --> Proxy
        Policy --> Proxy
    end

    subgraph VM["Micro-VM (Firecracker / Virtualization.framework)"]
        Agent["Guest Agent"]
        FUSE["/workspace (FUSE)"]
        Image["Any OCI Image (Alpine, Ubuntu, etc.)"]

        Agent --- Image
        FUSE --- Image
    end

    Proxy -- "vsock :5000" --> Agent
    VFS -- "vsock :5001" --> FUSE
```

### Network Modes

| Platform | Mode | Mechanism |
|----------|------|-----------|
| Linux | Transparent proxy | nftables DNAT on ports 80/443 |
| macOS | NAT (default) | Virtualization.framework built-in NAT |
| macOS | Interception (with `--allow-host`/`--secret`) | gVisor userspace TCP/IP at L4 |

## Docs

See [AGENTS.md](AGENTS.md) for the full developer reference.

## License

MIT
