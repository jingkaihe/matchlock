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
    .with_cpus(2)
    .with_memory(512)
    .with_disk_size(2048)
    .with_timeout(300)
    .with_workspace("/home/user/code")
    .allow_host("api.openai.com", "pypi.org")
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

### File Operations

Read, write, and list files inside the sandbox:

```python
from matchlock import Client, Sandbox

with Client() as client:
    client.launch(Sandbox("alpine:latest"))

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
    # Block access to private IPs (10.x, 172.16.x, 192.168.x)
    .block_private_ips()
    # Inject secret — the MITM proxy replaces the placeholder with the real
    # value only when requests go to the specified host
    .add_secret("ANTHROPIC_API_KEY", os.environ["ANTHROPIC_API_KEY"], "api.anthropic.com")
)

with Client() as client:
    client.launch(sandbox)
    result = client.exec("python3 call_api.py")
```

### Filesystem Mounts

Mount host directories or use in-memory/overlay filesystems:

```python
from matchlock import Client, Sandbox, MountConfig

sandbox = (
    Sandbox("alpine:latest")
    .mount_host_dir("/workspace/src", "/home/user/project/src")
    .mount_host_dir_readonly("/workspace/config", "/home/user/project/config")
    .mount_memory("/workspace/tmp")
    .mount_overlay("/workspace/data", "/home/user/project/data")
    # Or use the generic mount() method:
    .mount("/workspace/custom", MountConfig(type="real_fs", host_path="/tmp/custom"))
)
```

### Custom Configuration

```python
from matchlock import Client, Config

config = Config(
    binary_path="/usr/local/bin/matchlock",
    use_sudo=True,  # Required for TAP devices on Linux
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
| `.with_cpus(n)` | Set number of vCPUs |
| `.with_memory(mb)` | Set memory in MB |
| `.with_disk_size(mb)` | Set disk size in MB |
| `.with_timeout(seconds)` | Set max execution time |
| `.with_workspace(path)` | Set guest VFS mount point (default: `/workspace`) |
| `.allow_host(*hosts)` | Add allowed network hosts (supports wildcards) |
| `.block_private_ips()` | Block access to private IP ranges |
| `.add_secret(name, value, *hosts)` | Inject a secret for specific hosts |
| `.mount(guest_path, config)` | Add a VFS mount with custom `MountConfig` |
| `.mount_host_dir(guest, host)` | Mount a host directory (read-write) |
| `.mount_host_dir_readonly(guest, host)` | Mount a host directory (read-only) |
| `.mount_memory(guest_path)` | Mount an in-memory filesystem |
| `.mount_overlay(guest, host)` | Mount a copy-on-write overlay |
| `.options()` | Return the built `CreateOptions` |

### `Client(config: Config | None = None)`

JSON-RPC client for interacting with Matchlock sandboxes. All public methods are thread-safe.

| Method | Description |
|---|---|
| `.start()` | Start the matchlock RPC subprocess |
| `.launch(sandbox)` | Create a VM from a `Sandbox` builder — returns VM ID |
| `.create(opts)` | Create a VM from `CreateOptions` — returns VM ID |
| `.exec(command, working_dir="")` | Execute a command, returns `ExecResult` |
| `.exec_stream(command, stdout, stderr, working_dir)` | Stream command output, returns `ExecStreamResult` |
| `.write_file(path, content, mode=0o644)` | Write a file into the sandbox |
| `.read_file(path)` | Read a file from the sandbox — returns `bytes` |
| `.list_files(path)` | List directory contents — returns `list[FileInfo]` |
| `.close(timeout=0)` | Shut down the sandbox VM. `timeout` in seconds; 0 = kill immediately |
| `.remove()` | Remove the stopped VM's state directory |
| `.vm_id` | The current VM ID (property) |

### Types

| Type | Fields |
|---|---|
| `Config` | `binary_path: str`, `use_sudo: bool` |
| `CreateOptions` | `image`, `cpus`, `memory_mb`, `disk_size_mb`, `timeout_seconds`, `allowed_hosts`, `block_private_ips`, `mounts`, `secrets`, `workspace` |
| `ExecResult` | `exit_code: int`, `stdout: str`, `stderr: str`, `duration_ms: int` |
| `ExecStreamResult` | `exit_code: int`, `duration_ms: int` |
| `FileInfo` | `name: str`, `size: int`, `mode: int`, `is_dir: bool` |
| `MountConfig` | `type: str`, `host_path: str`, `readonly: bool` |
| `Secret` | `name: str`, `value: str`, `hosts: list[str]` |
| `MatchlockError` | Base exception for all Matchlock errors |
| `RPCError` | RPC error with `code: int` and `message: str` |

## License

MIT
