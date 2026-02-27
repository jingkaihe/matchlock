# Usage

This guide covers running sandboxes, configuring the network policy engine, mounting volumes, injecting secrets, logging, and lifecycle management.

## Running a Sandbox

### Basic Usage

```bash
# Run a command and exit
matchlock run --image alpine:latest cat /etc/os-release

# Interactive shell
matchlock run --image alpine:latest -it sh

# Pipe mode (stdin open, no TTY)
matchlock run --image alpine:latest -i -- sh -c 'read line; echo "got: $line"'
```

The `--image` flag is required. It accepts any OCI image reference that a container registry supports (e.g., `alpine:latest`, `python:3.12-slim`, `ghcr.io/org/image:tag`).

### Long-Lived Sandboxes

By default, sandboxes are removed when the command exits (`--rm=true`). To keep a sandbox running:

```bash
# Start a sandbox, print the VM ID, and keep it running
matchlock run --image alpine:latest --rm=false

# Connect to it from another terminal
matchlock exec vm-abc12345 -it bash

# Stop it
matchlock kill vm-abc12345

# Remove it
matchlock rm vm-abc12345
```

### Resource Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--cpus` | 1 | Number of vCPUs |
| `--memory` | 512 | Memory in MB |
| `--disk-size` | 5120 | Writable disk size in MB |
| `--timeout` | 300 | Timeout in seconds (0 = no timeout) |
| `--graceful-shutdown` | 0s | Grace period before force-stopping the VM |
| `--privileged` | false | Skip in-guest security restrictions (seccomp, cap drop, no_new_privs) |

```bash
matchlock run --image ubuntu:24.04 --cpus 4 --memory 4096 --disk-size 20480 -it bash
```

### User and Entrypoint Overrides

```bash
# Run as a specific user
matchlock run --image ubuntu:24.04 -u 1000 -- whoami
matchlock run --image ubuntu:24.04 -u 1000:1000 -- id
matchlock run --image ubuntu:24.04 -u agent -- whoami

# Override entrypoint
matchlock run --image myapp:latest --entrypoint /bin/sh -- -c 'echo hello'

# Clear entrypoint (use command directly)
matchlock run --image myapp:latest --entrypoint "" -- echo raw
```

### Working Directory

The working directory defaults to the image's `WORKDIR` (from OCI config), falling back to `/workspace`. Override with:

```bash
matchlock run --image alpine:latest -w /tmp -- pwd
```

### Hostname

```bash
matchlock run --image alpine:latest --hostname my-sandbox -- hostname
```

Defaults to the sandbox VM ID (e.g., `vm-abc12345`).

## Volume Mounts

Matchlock supports mounting host directories into the guest VM. Guest paths are relative to the workspace (`/workspace` by default).

### Mount Modes

| Syntax | Mode | Description |
|--------|------|-------------|
| `./src:code` | `overlay` (default) | Isolated snapshot -- changes do not affect the host |
| `./src:code:overlay` | `overlay` | Same as above, explicit |
| `/host/path:subdir:host_fs` | `host_fs` | Direct read-write mount via FUSE VFS |
| `/host/path:subdir:ro` | `readonly` | Read-only host mount |

```bash
# Snapshot mount (isolated, changes vanish on exit)
matchlock run --image node:22-bookworm-slim -v ./myapp:app -- node /workspace/app/index.js

# Direct host mount (changes persist on the host)
matchlock run --image node:22-bookworm-slim -v ./myapp:app:host_fs -- node /workspace/app/index.js

# Read-only mount
matchlock run --image alpine:latest -v ./config:config:ro -- cat /workspace/config/settings.json
```

> **Note:** The `overlay` mode copies the host directory contents into an isolated in-memory snapshot at startup. The guest sees the full contents but modifications are not written back to the host. This is the default because the VM IS the sandbox -- isolation is the point.

> **Note:** The `host_fs` mode uses the VFS FUSE filesystem to proxy reads and writes to the host in real time. Use this when you need the guest to modify files that persist after the sandbox exits (e.g., AI agents writing code).

### How Agents Get Mounted

When an agent system (Claude Code, Codex, etc.) runs inside Matchlock, the typical pattern is:

1. The agent's code repository is cloned inside the VM (not mounted from the host)
2. The workspace at `/workspace` is a FUSE-backed VFS mount for file I/O between host and guest
3. The host SDK or CLI uses `host_fs` mounts when it needs real-time file sync

For agent frameworks that need to read/write files between host and guest, use the VFS with `host_fs` mode. For agent isolation where you want disposable side effects, use the default `overlay` mode.

See the example wrappers in `examples/claude-code/` and `examples/codex/` for real-world agent mounting patterns.

## Network Configuration

### Network Modes

Matchlock has three network modes:

| Mode | When | Mechanism |
|------|------|-----------|
| **NAT (default)** | No `--allow-host` or `--secret` flags | Native NAT (macOS: Virtualization.framework, Linux: TAP + masquerade) |
| **Interception** | `--allow-host` or `--secret` is set | Transparent proxy with TLS MITM (macOS: gVisor userspace stack, Linux: nftables DNAT) |
| **No network** | `--no-network` flag | Guest has no network interface at all |

Interception mode activates automatically when you use policy features. You do not select it explicitly.

```bash
# NAT mode (open network, no policy)
matchlock run --image alpine:latest -- wget -qO- https://example.com

# Interception mode (sealed network, only allowed hosts reachable)
matchlock run --image alpine:latest --allow-host example.com -- wget -qO- https://example.com

# No network (fully offline)
matchlock run --image alpine:latest --no-network -- echo offline
```

> **Note:** `--no-network` cannot be combined with `--allow-host`, `--secret`, `--local-model-route`, or `--allow-private-host`.

### Host Allowlisting

When `--allow-host` is set, only traffic to listed hosts is permitted. Everything else is blocked with HTTP 403.

```bash
matchlock run --image alpine:latest \
  --allow-host "api.openai.com" \
  --allow-host "*.anthropic.com" \
  -- wget -qO- https://api.openai.com/v1/models
```

**Wildcard patterns:**

| Pattern | Matches |
|---------|---------|
| `*` | All hosts |
| `*.example.com` | All subdomains (`api.example.com`, `a.b.example.com`) |
| `api-*.example.com` | Pattern match (`api-v1.example.com`, `api-prod.example.com`) |

### Private IP Blocking

When interception mode is active, private IPs (`10/8`, `172.16/12`, `192.168/16`) are blocked by default. To allow specific private IPs:

```bash
matchlock run --image alpine:latest \
  --allow-host "ollama-host" \
  --allow-private-host "192.168.1.100" \
  --add-host "ollama-host:192.168.1.100" \
  -- curl http://ollama-host:11434/api/tags
```

### Custom Host Entries

Inject entries into the guest's `/etc/hosts`:

```bash
matchlock run --image alpine:latest \
  --add-host "api.internal:10.0.0.10" \
  --add-host "db.internal:10.0.0.11" \
  -- ping -c1 api.internal
```

### DNS Servers

Override the default DNS servers (`8.8.8.8`, `8.8.4.4`):

```bash
matchlock run --image alpine:latest \
  --dns-servers "1.1.1.1,1.0.0.1" \
  -- nslookup example.com
```

### Network MTU

Adjust the guest network MTU (default: 1500). Useful for working around path-MTU or TLS handshake issues on some VM networking paths:

```bash
matchlock run --image alpine:latest --mtu 1200 -- wget -qO- https://example.com
```

## Secret Injection

Secrets are injected via the MITM proxy -- the real value never enters the VM. The guest sees a placeholder environment variable. When the guest makes an HTTP request to an allowed host, the proxy replaces the placeholder with the real secret in-flight.

### CLI Syntax

```bash
# Read secret from environment variable, bind to specific hosts
export ANTHROPIC_API_KEY=sk-ant-xxx
matchlock run --image python:3.12-slim \
  --secret "ANTHROPIC_API_KEY@api.anthropic.com" \
  -- python call_api.py

# Inline secret value
matchlock run --image python:3.12-slim \
  --secret "API_KEY=sk-inline-value@api.openai.com" \
  -- python call_api.py

# Multiple hosts for the same secret
matchlock run --image python:3.12-slim \
  --secret "API_KEY@api.openai.com,api.anthropic.com" \
  -- python agent.py
```

**Format:** `NAME=VALUE@host1,host2` or `NAME@host1,host2` (reads from `$NAME`).

> **Important:** The `--secret` flag uses Cobra's `StringSlice` type, which splits on commas. If your secret value contains commas, use the `NAME=VALUE@host` format and set it via environment variable + `NAME@host` syntax instead.

### How It Works

1. Matchlock generates a random placeholder string (e.g., `SANDBOX_SECRET_a1b2c3d4...`) for each secret
2. The guest environment variable is set to this placeholder
3. When the guest sends an HTTP request, the MITM proxy inspects headers and URLs
4. If the placeholder appears in the request and the destination host is in the secret's allowed hosts, the real value is substituted
5. If the placeholder would be sent to an unauthorized host, the request is blocked (secret leak protection)

### Secret Safety

- Secret values are never logged -- only secret names appear in log output
- The MITM proxy generates per-session CA certificates for TLS interception
- CA certificates are injected into the guest rootfs before the VM boots
- Environment variables (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`) are set automatically so common HTTP clients trust the MITM CA

## Network Policy Plugins

The interception proxy runs a plugin pipeline with four phases:

1. **Gate** -- should this connection proceed? (AND semantics: all gates must allow)
2. **Route** -- redirect to a different backend? (first non-nil directive wins)
3. **Request** -- transform outbound request (chained)
4. **Response** -- transform inbound response (chained)

### Built-in Plugins

| Plugin | Phase(s) | CLI Flags |
|--------|----------|-----------|
| `host_filter` | Gate | `--allow-host`, `--allow-private-host` |
| `secret_injector` | Request | `--secret` |
| `local_model_router` | Route, Request | `--local-model-backend`, `--local-model-route` |
| `usage_logger` | Response | `--usage-log-path` |
| `budget_gate` | Gate | `--budget-limit-usd` |

### Local Model Routing

Redirect cloud LLM API calls to a local inference backend (e.g., Ollama):

```bash
matchlock run --image python:3.12-slim \
  --allow-host "openrouter.ai" \
  --local-model-backend "192.168.1.100:11434" \
  --local-model-route "openrouter.ai/google/gemini-2.0-flash-001=llama3.1:8b" \
  -- python agent.py
```

The agent calls `openrouter.ai` as normal, but Matchlock intercepts the request and redirects it to the local Ollama backend, rewriting the model name in the request body.

**Route format:** `SOURCE_HOST/SOURCE_MODEL=TARGET_MODEL[@HOST:PORT]`

The optional `@HOST:PORT` suffix overrides `--local-model-backend` for that specific route.

### Budget Enforcement

Limit per-session API spend:

```bash
matchlock run --image node:22-bookworm-slim \
  --allow-host "openrouter.ai" \
  --secret "OPENROUTER_API_KEY@openrouter.ai" \
  --usage-log-path ./usage.jsonl \
  --budget-limit-usd 5.00 \
  -- node app.js
```

When spend reaches the limit, all outbound requests are blocked with HTTP 429. See [Budget Enforcement](usage/budget-enforcement.md) for details.

> **Note:** `--budget-limit-usd` requires `--usage-log-path` to be set.

For the full plugin configuration reference (including JSON config and writing custom plugins), see [Network Plugins](network-plugins.md).

## Environment Variables

Non-secret environment variables are visible inside the VM and in VM state output.

```bash
# Inline
matchlock run --image alpine:latest -e FOO=bar -- sh -c 'echo $FOO'

# Read from host environment
export DATABASE_URL=postgres://localhost/db
matchlock run --image alpine:latest -e DATABASE_URL -- printenv DATABASE_URL

# From env file
matchlock run --image alpine:latest --env-file .env -- printenv
```

**Env file format:** One `KEY=VALUE` or `KEY` per line. Lines starting with `#` are comments. `KEY` without `=` reads from the host environment.

## Event Logging

Matchlock can write a structured JSON-L event log capturing every policy engine decision, HTTP request, and response.

```bash
matchlock run --image alpine:latest \
  --allow-host "api.openai.com" \
  --secret "API_KEY@api.openai.com" \
  --event-log ./events.jsonl \
  --agent-system "my-agent" \
  --run-id "session-001" \
  -- python agent.py
```

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--event-log` | Yes (to enable) | -- | Path to JSON-L output file |
| `--agent-system` | No | `""` | Agent system label (e.g., `openclaw`, `aider`) |
| `--run-id` | No | sandbox VM ID | Session identifier |

Every event has a consistent top-level shape:

```json
{
  "ts": "2026-02-24T17:49:40.619586Z",
  "run_id": "vm-48a6b77f",
  "agent_system": "openclaw",
  "event_type": "gate_decision",
  "summary": "gate allowed openrouter.ai by host_filter",
  "plugin": "host_filter",
  "tags": ["tls"],
  "data": { "host": "openrouter.ai", "allowed": true }
}
```

**Event types:** `gate_decision`, `route_decision`, `request_transform`, `response_transform`, `http_request`, `http_response`.

**Querying the log:**

```bash
# Count events by type
jq -r '.event_type' events.jsonl | sort | uniq -c

# Show all blocked hosts
jq 'select(.event_type == "gate_decision" and .data.allowed == false)' events.jsonl

# Show all secret injections
jq 'select(.event_type == "request_transform" and .data.action == "injected")' events.jsonl

# Watch live
tail -f events.jsonl | jq '.'
```

For the full event schema reference, see [Event Logging](event-logging.md).

## Port Forwarding

Forward host ports to guest ports:

```bash
# At startup
matchlock run --image node:22-bookworm-slim --rm=false -p 8080:8080 -- node server.js

# To a running sandbox
matchlock port-forward vm-abc12345 8080:8080
```

Bind to a specific address:

```bash
matchlock run --image alpine:latest --rm=false -p 8080:8080 --address 0.0.0.0
```

## Image Management

### Pulling Images

Images are pulled automatically on first use. Force a re-pull:

```bash
matchlock run --image alpine:latest --pull -- echo "freshly pulled"
```

Or pre-pull:

```bash
matchlock pull alpine:latest
```

### Building from Dockerfile

Matchlock includes BuildKit-in-VM for building images:

```bash
matchlock build -f Dockerfile -t myapp:latest .
```

### Importing from Docker

```bash
docker save myapp:latest | matchlock image import myapp:latest
```

### Listing and Removing Images

```bash
matchlock image ls
matchlock image rm myapp:latest
```

## Lifecycle Management

### Listing Sandboxes

```bash
matchlock list
```

### Inspecting a Sandbox

```bash
matchlock get vm-abc12345      # Brief status
matchlock inspect vm-abc12345  # Full configuration and state
```

### Killing and Removing

```bash
matchlock kill vm-abc12345     # Stop a running sandbox
matchlock rm vm-abc12345       # Remove a stopped sandbox
matchlock kill --all           # Stop all running sandboxes
matchlock prune                # Remove all stopped/crashed sandboxes
```

### Garbage Collection

Clean up leaked host resources (TAP interfaces, nftables rules, subnet allocations) for stopped or crashed VMs:

```bash
matchlock gc                   # Reconcile all non-running VMs
matchlock gc vm-abc12345       # Reconcile a specific VM
matchlock gc --force-running   # Also reconcile running VMs (use sparingly)
```

See [Lifecycle and Cleanup Runbook](lifecycle.md) for detailed troubleshooting.

## SDK Usage

Matchlock provides Go, Python, and TypeScript SDKs for programmatic sandbox management.

### Go SDK

```go
import "github.com/jingkaihe/matchlock/pkg/sdk"

client, _ := sdk.NewClient(sdk.DefaultConfig())
defer client.Close()

sandbox := sdk.New("alpine:latest").
    AllowHost("api.openai.com").
    AddSecret("API_KEY", os.Getenv("API_KEY"), "api.openai.com")

client.Launch(sandbox)
result, _ := client.Exec("echo hello")
fmt.Println(result.Stdout)
```

### Python SDK

```bash
pip install matchlock
```

```python
from matchlock import Client, Config, Sandbox

sandbox = (
    Sandbox("alpine:latest")
    .allow_host("api.openai.com")
    .add_secret("API_KEY", os.environ["API_KEY"], "api.openai.com")
)

with Client(Config()) as client:
    client.launch(sandbox)
    result = client.exec("echo hello")
    print(result.stdout)
```

### TypeScript SDK

```bash
npm install matchlock-sdk
```

```typescript
import { Client, Sandbox } from "matchlock-sdk";

const sandbox = new Sandbox("alpine:latest")
  .allowHost("api.openai.com")
  .addSecret("API_KEY", process.env.API_KEY ?? "", "api.openai.com");

const client = new Client();
await client.launch(sandbox);
const result = await client.exec("echo hello");
console.log(result.stdout);
await client.close();
```

### JSON-RPC Mode

The SDK communicates with Matchlock via JSON-RPC over stdin/stdout. You can also use the RPC interface directly:

```bash
matchlock rpc
```

This starts a JSON-RPC server on stdin/stdout. The available methods are:

- `create` -- Create a new sandbox
- `exec` / `exec_stream` -- Execute commands
- `write_file` / `read_file` / `list_files` -- File operations via VFS
- `port_forward` -- Forward ports
- `cancel` -- Cancel in-flight execution
- `close` -- Close the sandbox

### VFS Interception (SDK)

The SDK supports host-side VFS interception hooks. These let you inspect, block, or mutate filesystem operations on mounted guest paths from the host.

See [VFS Interception](vfs-interception.md) for the full reference including Go and Python examples.

## CLI Reference

### Commands

| Command | Description |
|---------|-------------|
| `matchlock run` | Run a command in a new sandbox |
| `matchlock exec` | Execute a command in a running sandbox |
| `matchlock build` | Build an image from a Dockerfile |
| `matchlock pull` | Pull an image from a registry |
| `matchlock image ls` | List local images |
| `matchlock image rm` | Remove a local image |
| `matchlock image import` | Import an image from a tarball (stdin) |
| `matchlock list` | List sandboxes |
| `matchlock get` | Get sandbox status |
| `matchlock inspect` | Inspect sandbox configuration |
| `matchlock kill` | Stop a running sandbox |
| `matchlock rm` | Remove a stopped sandbox |
| `matchlock prune` | Remove all stopped/crashed sandboxes |
| `matchlock gc` | Garbage-collect leaked host resources |
| `matchlock port-forward` | Forward host port to guest port |
| `matchlock rpc` | Start JSON-RPC server on stdin/stdout |
| `matchlock setup linux` | One-time Linux system setup (requires sudo) |
| `matchlock version` | Print version information |

### `matchlock run` Flags

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--image` | | string | (required) | Container image reference |
| `-t` | `--tty` | bool | false | Allocate pseudo-TTY |
| `-i` | `--interactive` | bool | false | Keep stdin open |
| `-v` | `--volume` | string[] | | Volume mounts |
| `-e` | `--env` | string[] | | Environment variables |
| `--env-file` | | string[] | | Environment files |
| `-w` | `--workdir` | string | image WORKDIR | Working directory |
| `-u` | `--user` | string | image USER | Run as user |
| `--entrypoint` | | string | image ENTRYPOINT | Override entrypoint |
| `-p` | `--publish` | string[] | | Port forwards |
| `--address` | | string[] | `127.0.0.1` | Bind address for published ports |
| `--workspace` | | string | `/workspace` | Guest VFS mount point |
| `--allow-host` | | string[] | | Allowed hosts (enables interception) |
| `--allow-private-host` | | string[] | | Allowed private IPs |
| `--add-host` | | string[] | | Custom host:ip entries |
| `--secret` | | string[] | | Secrets for MITM injection |
| `--local-model-backend` | | string | | Default local model backend |
| `--local-model-route` | | string[] | | Model routing rules |
| `--usage-log-path` | | string | | Usage log file path |
| `--budget-limit-usd` | | float64 | 0 | Budget limit in USD |
| `--dns-servers` | | string[] | `8.8.8.8,8.8.4.4` | DNS servers |
| `--hostname` | | string | sandbox ID | Guest hostname |
| `--mtu` | | int | 1500 | Network MTU |
| `--no-network` | | bool | false | Disable guest networking |
| `--cpus` | | int | 1 | vCPUs |
| `--memory` | | int | 512 | Memory in MB |
| `--disk-size` | | int | 5120 | Disk size in MB |
| `--timeout` | | int | 300 | Timeout in seconds |
| `--rm` | | bool | true | Remove sandbox on exit |
| `--pull` | | bool | false | Force image re-pull |
| `--privileged` | | bool | false | Skip guest security restrictions |
| `--graceful-shutdown` | | duration | 0s | Graceful shutdown timeout |
| `--event-log` | | string | | Event log file path |
| `--run-id` | | string | sandbox ID | Run/session ID |
| `--agent-system` | | string | | Agent system name |
