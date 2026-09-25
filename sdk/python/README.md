# Matchlock Python SDK

A Python client for [Matchlock](https://github.com/jingkaihe/matchlock) — a lightweight micro-VM sandbox for running AI-generated code securely with network interception and secret protection.

## Requirements

- Python 3.10+
- The `matchlock` CLI binary installed and available on `$PATH` (or specify its path via `Config`)

## Installation

```bash
pip install matchlock
```

Or install from source:

```bash
pip install -e sdk/python
```

## Quick Start

```python
from matchlock import Client, Sandbox

sandbox = Sandbox("python:3.12-alpine")

with Client() as client:
    client.launch(sandbox)

    result = client.exec("echo hello from the sandbox")
    print(result.stdout)  # "hello from the sandbox\n"
```

## Usage

### Builder API

The `Sandbox` class provides a fluent builder for configuring sandboxes:

```python
import os
from matchlock import Client, Sandbox

sandbox = (
    Sandbox("python:3.12-alpine")
    .with_privileged()
    .with_cpus(2)
    .with_memory(512)
    .with_disk_size(2048)
    .with_timeout(300)
    .allow_host("api.openai.com", "pypi.org")
    .with_port_forward(18080, 8080)
    .with_port_forward_addresses("127.0.0.1")
    .with_network_mtu(1200)
    .block_private_ips()
    .add_secret("API_KEY", os.environ["API_KEY"], "api.openai.com")
)

with Client() as client:
    vm_id = client.launch(sandbox)
    result = client.exec("python3 -c 'print(1+1)'")
    print(result.exit_code)  # 0
    print(result.stdout)     # "2\n"
```

### Streaming Output

Use `exec_stream` for real-time stdout/stderr streaming:

```python
import sys
from matchlock import Client, Sandbox

with Client() as client:
    client.launch(Sandbox("alpine:latest"))

    result = client.exec_stream(
        "for i in 1 2 3; do echo $i; sleep 1; done",
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    print(f"Exit code: {result.exit_code}")
    print(f"Duration: {result.duration_ms}ms")
```

### Guest Filesystem and File Operations

Files live on the guest's native filesystem, not in a shared host directory.
The writable root filesystem belongs to the sandbox; copy out results before
removing it, or use a managed block volume for persistent data. File operations
are explicit transfers over the SDK's JSON-RPC connection and the guest-agent
vsock service. They do not expose a host filesystem or install filesystem hooks.

`/workspace` is an ordinary guest path, not an automatically mounted directory.
Create it before using it. Commands use the image's `WORKDIR` by default; pass
`working_dir` to select an existing guest directory for a particular execution.

```python
from matchlock import Client, Sandbox

with Client() as client:
    client.launch(Sandbox("alpine:latest"))

    client.exec("mkdir -p /workspace")

    # Write a file
    client.write_file("/workspace/hello.txt", "Hello, world!")
    client.write_file("/workspace/script.sh", "#!/bin/sh\necho hi", mode=0o755)

    # Read a file
    content = client.read_file("/workspace/hello.txt")
    print(content.decode())  # "Hello, world!"

    # List files
    files = client.list_files("/workspace")
    for f in files:
        print(f"{f.name} ({f.size} bytes, dir={f.is_dir})")

    result = client.exec("./script.sh", working_dir="/workspace")
    print(result.stdout)  # "hi\n"
```

### Stdin/Stdout File Transfers

Use `exec_pipe` to send an input stream to a guest process and receive its output
without a PTY. This example uploads a local ASCII text file while streaming its
contents back to another local file:

```python
import sys
from matchlock import Client, Sandbox

with Client() as client:
    client.launch(Sandbox("alpine:latest"))
    client.exec("mkdir -p /workspace")

    with open("input.txt", "rb") as source, open(
        "output.txt", "w", encoding="utf-8"
    ) as output:
        result = client.exec_pipe(
            "tee /workspace/input.txt",
            stdin=source,
            stdout=output,
            stderr=sys.stderr,
        )
        if result.exit_code != 0:
            raise RuntimeError(f"File transfer exited with {result.exit_code}")
```

Python `exec_pipe` accepts text or binary stdin, but its stdout/stderr writers
receive decoded UTF-8 text. `exec_stream` also delivers text. Do not use these
output writers for raw binary downloads; use `read_file`, which returns `bytes`,
when byte-exact file contents are needed. `write_file` accepts `bytes` or `str`.

### Managed Block Volumes

Named volumes are managed ext4 disk images, separate from the sandbox's writable
root filesystem. When attached, the guest accesses them as block devices; they
are not shared host directories. They persist independently of a VM until removed.

```python
from matchlock import Client

client = Client()
volume = client.volume_create("sdk-data", size_mb=1024)
print(volume.name, volume.size, volume.path)
for existing in client.volume_list():
    print(existing.name)

# When the volume is no longer attached or needed:
# client.volume_remove("sdk-data")
```

These helpers manage volumes without creating a VM. `VolumeInfo.path` is the host
path of the backing disk image, not a directory exposed to the guest. The Python
builder and `CreateOptions` do not currently expose block-disk attachment. Use
the CLI's `--disk` option to launch a VM with a volume created above:

```bash
matchlock run --image alpine:latest --disk @sdk-data:/data -- sh -c 'echo persisted > /data/hello.txt'
```

### Network Policy & Secrets

Control network access and inject secrets securely:

```python
import os
from matchlock import Client, Sandbox

sandbox = (
    Sandbox("python:3.12-alpine")
    # Only allow these hosts
    .allow_host("api.anthropic.com", "pypi.org", "files.pythonhosted.org")
    # Optional MTU override
    .with_network_mtu(1200)
    # Explicitly block or allow private IPs (10.x, 172.16.x, 192.168.x)
    .with_block_private_ips(True)
    # Inject secret — the MITM proxy replaces the placeholder with the real
    # value only when requests go to the specified host
    .add_secret("ANTHROPIC_API_KEY", os.environ["ANTHROPIC_API_KEY"], "api.anthropic.com")
)

with Client() as client:
    client.launch(sandbox)
    result = client.exec("python3 call_api.py")

    # Optional: forward local 18080 -> guest 8080 after VM creation
    bindings = client.port_forward("18080:8080")
    print(bindings[0].address, bindings[0].local_port, bindings[0].remote_port)
```

Private-IP behavior in the Python SDK:

- Default (unset): private IPs are blocked whenever a `network` config is sent.
- Explicit block: `.block_private_ips()` or `.with_block_private_ips(True)`.
- Explicit allow: `.allow_private_ips()` or `.with_block_private_ips(False)`.
- Reset to default behavior: `.unset_block_private_ips()`.

If you use `CreateOptions(...)` directly instead of the builder, set both:

- `block_private_ips_set=True`
- `block_private_ips=<True|False>`

For fully offline sandboxes (no guest NIC / no egress), use:

- Builder: `.with_no_network()`
- Direct options: `CreateOptions(no_network=True)`

### Network Interception Rules

Use typed network hook rules for host-side request/response mutation:

```python
from matchlock import (
    NetworkBodyTransform,
    NetworkHookRule,
    NetworkInterceptionConfig,
    Sandbox,
)

sandbox = Sandbox("alpine:latest").with_network_interception(
    NetworkInterceptionConfig(
        rules=[
            NetworkHookRule(
                phase="after",
                hosts=["api.example.com"],
                action="mutate",
                set_response_headers={"X-Intercepted": "true"},
                body_replacements=[NetworkBodyTransform(find="foo", replace="bar")],
            )
        ]
    )
)
```

Callback-based interception:

```python
from matchlock import (
    NetworkHookRequest,
    NetworkHookResult,
    NetworkHookResponseMutation,
    NetworkHookRule,
    NetworkInterceptionConfig,
    Sandbox,
)

def after_hook(req: NetworkHookRequest) -> NetworkHookResult | None:
    if req.status_code != 200:
        return None
    return NetworkHookResult(
        action="mutate",
        response=NetworkHookResponseMutation(
            headers={"X-Intercepted": ["callback"]},
            set_body=b'{"msg":"from-callback"}',
        ),
    )

sandbox = Sandbox("alpine:latest").with_network_interception(
    NetworkInterceptionConfig(
        rules=[
            NetworkHookRule(
                phase="after",
                hosts=["api.example.com"],
                hook=after_hook,
                timeout_ms=1500,
            )
        ]
    )
)
```

### Custom Configuration

```python
from matchlock import Client, Config

config = Config(
    binary_path="/usr/local/bin/matchlock",
    use_sudo=False,  # Run as your normal user after Linux setup
)

with Client(config) as client:
    ...
```

The binary path can also be set via the `MATCHLOCK_BIN` environment variable.

### Lifecycle Management

```python
from matchlock import Client, Sandbox

client = Client()
client.start()

client.launch(Sandbox("alpine:latest"))
print(client.vm_id)  # e.g., "vm-abc12345"

result = client.exec("echo hello")

# Shut down the sandbox VM
client.close()

# Remove the stopped VM's state directory
client.remove()
```

### Error Handling

```python
from matchlock import Client, Sandbox, MatchlockError, RPCError

with Client() as client:
    try:
        client.launch(Sandbox("alpine:latest"))
        result = client.exec("exit 1")
        if result.exit_code != 0:
            print(f"Command failed: {result.stderr}")
    except RPCError as e:
        print(f"RPC error [{e.code}]: {e.message}")
        if e.is_vm_error():
            print("VM-level failure")
        elif e.is_exec_error():
            print("Execution failure")
        elif e.is_file_error():
            print("File operation failure")
    except MatchlockError as e:
        print(f"Matchlock error: {e}")
```

## API Reference

### `Sandbox(image: str)`

Fluent builder for sandbox configuration.

| Method | Description |
|---|---|
| `.with_cpus(n)` | Set number of vCPUs (supports fractional values, e.g. `0.5`) |
| `.with_privileged()` | Enable privileged mode (skip in-guest seccomp/cap-drop/no_new_privs) |
| `.with_memory(mb)` | Set memory in MB |
| `.with_disk_size(mb)` | Set disk size in MB |
| `.with_timeout(seconds)` | Set max execution time |
| `.allow_host(*hosts)` | Add allowed network hosts (supports wildcards) |
| `.block_private_ips()` | Block access to private IP ranges |
| `.with_block_private_ips(enabled)` | Explicitly set private IP blocking true/false |
| `.allow_private_ips()` | Explicitly allow private IP ranges |
| `.unset_block_private_ips()` | Reset private IP behavior to SDK default semantics |
| `.with_network_mtu(mtu)` | Override guest network stack/interface MTU |
| `.with_no_network()` | Disable guest network egress entirely |
| `.with_port_forward(local_port, remote_port)` | Add a host-to-guest port mapping |
| `.with_port_forward_addresses(*addresses)` | Set bind addresses for configured port mappings |
| `.with_network_interception(config=None)` | Force interception and optionally apply typed network hook rules |
| `.add_secret(name, value, *hosts)` | Inject a secret for specific hosts |
| `.with_user(user)` | Set the guest execution user |
| `.with_image_config(config)` | Merge image metadata, including `working_dir`; does not create directories |
| `.options()` | Return the built `CreateOptions` |

### `Client(config: Config | None = None)`

JSON-RPC client for interacting with Matchlock sandboxes. All public methods are thread-safe.

| Method | Description |
|---|---|
| `.start()` | Start the matchlock RPC subprocess |
| `.launch(sandbox)` | Create a VM and start image ENTRYPOINT/CMD in detached mode — returns VM ID |
| `.create(opts)` | Create a VM from `CreateOptions` (does not auto-start ENTRYPOINT unless `launch_entrypoint=True`) — returns VM ID |
| `.exec(command, working_dir="")` | Execute a command, returns `ExecResult` |
| `.exec_stream(command, stdout=None, stderr=None, working_dir="")` | Stream text command output, returns `ExecStreamResult` |
| `.log()` | Return the current buffered VM log as `str` |
| `.log_stream(stdout=None)` | Stream VM log output until cancelled |
| `.exec_pipe(command, stdin=None, stdout=None, stderr=None, working_dir="", timeout=None, user="")` | Text/binary stdin and text stdout/stderr (no PTY) as an optional user (uid, uid:gid, or username), returns `ExecPipeResult` |
| `.exec_interactive(command, stdin=None, stdout=None, working_dir="", rows=24, cols=80, resize=None, timeout=None, user="")` | Interactive PTY exec as an optional user (uid, uid:gid, or username), returns `ExecInteractiveResult` |
| `.write_file(path, content, mode=0o644)` | Write a file into the sandbox |
| `.read_file(path)` | Read a file from the sandbox — returns `bytes` |
| `.list_files(path)` | List directory contents — returns `list[FileInfo]` |
| `.volume_create(name, size_mb=10240)` | Create a named ext4 block volume — returns `VolumeInfo` |
| `.volume_list()` | List managed block volumes — returns `list[VolumeInfo]` |
| `.volume_remove(name)` | Delete a managed block volume that is no longer in use |
| `.port_forward(*specs)` | Apply one or more `[LOCAL_PORT:]REMOTE_PORT` mappings |
| `.port_forward_with_addresses(addresses, *specs)` | Apply port mappings bound on specific host addresses |
| `.close(timeout=0)` | Shut down the sandbox VM. `timeout` in seconds; 0 = kill immediately |
| `.remove()` | Remove the stopped VM's state directory |
| `.vm_id` | The current VM ID (property) |

### Types

| Type | Fields |
|---|---|
| `Config` | `binary_path: str`, `use_sudo: bool` |
| `CreateOptions` | `image`, `kernel_ref`, `privileged`, `cpus`, `memory_mb`, `disk_size_mb`, `timeout_seconds`, `allowed_hosts`, `add_hosts`, `block_private_ips`, `block_private_ips_set`, `no_network`, `force_interception`, `network_interception`, `env`, `secrets`, `dns_servers`, `hostname`, `network_mtu`, `port_forwards`, `port_forward_addresses`, `image_config`, `launch_entrypoint` |
| `ImageConfig` | `user`, `working_dir`, `entrypoint`, `cmd`, `env` |
| `ExecResult` | `exit_code: int`, `stdout: str`, `stderr: str`, `duration_ms: int` |
| `ExecStreamResult` | `exit_code: int`, `duration_ms: int` |
| `ExecPipeResult` | `exit_code: int`, `duration_ms: int` |
| `ExecInteractiveResult` | `exit_code: int`, `duration_ms: int` |
| `FileInfo` | `name: str`, `size: int`, `mode: int`, `is_dir: bool` |
| `VolumeInfo` | `name: str`, `size: str`, `path: str` (backing ext4 image) |
| `PortForward` | `local_port: int`, `remote_port: int` |
| `PortForwardBinding` | `address: str`, `local_port: int`, `remote_port: int` |
| `Secret` | `name: str`, `value: str`, `hosts: list[str]` |
| `MatchlockError` | Base exception for all Matchlock errors |
| `RPCError` | RPC error with `code: int` and `message: str` |

## License

MIT
