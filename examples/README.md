# Matchlock Examples

This directory contains examples of programmatic access to Matchlock sandboxes.

## JSON-RPC Protocol

Matchlock exposes a JSON-RPC 2.0 interface over stdin/stdout when run with `--rpc`:

```bash
sudo matchlock --rpc
```

### Available Methods

| Method | Description | Parameters |
|--------|-------------|------------|
| `create` | Create and start a VM | `image`, `resources`, `network`, `vfs` |
| `exec` | Execute a command | `command`, `working_dir` |
| `write_file` | Write a file | `path`, `content` (base64), `mode` |
| `read_file` | Read a file | `path` |
| `list_files` | List directory | `path` |
| `close` | Close the VM | - |

### Request Format

```json
{
  "jsonrpc": "2.0",
  "method": "create",
  "params": {
    "image": "standard",
    "resources": {
      "cpus": 1,
      "memory_mb": 512,
      "timeout_seconds": 300
    }
  },
  "id": 1
}
```

### Response Format

```json
{
  "jsonrpc": "2.0",
  "result": {
    "id": "vm-abc12345"
  },
  "id": 1
}
```

### Error Response

```json
{
  "jsonrpc": "2.0",
  "error": {
    "code": -32000,
    "message": "VM failed to start"
  },
  "id": 1
}
```

## Python Example

The Python example uses no external dependencies (just stdlib).

```bash
# Run from the matchlock directory (uses ./bin/matchlock by default)
uv run examples/sandbox_client.py

# Or specify the binary path
MATCHLOCK_BIN=/path/to/matchlock python3 examples/sandbox_client.py
```

### Python Client Usage

```python
from sandbox_client import MatchlockClient

with MatchlockClient() as client:
    # Create sandbox
    vm_id = client.create(image="standard", cpus=1, memory_mb=512)
    
    # Execute commands
    result = client.exec("echo 'Hello!'")
    print(result.stdout)
    
    # Write files
    client.write_file("/workspace/script.py", "print('Hello')")
    
    # Read files
    content = client.read_file("/workspace/script.py")
    
    # List files
    files = client.list_files("/workspace")
    for f in files:
        print(f.name, f.size)
```

## Go Example

The Go example is a standalone file with no external dependencies.

```bash
# Run from the matchlock directory (uses ./bin/matchlock by default)
go run examples/sandbox_client.go

# Or specify the binary path
MATCHLOCK_BIN=/path/to/matchlock go run examples/sandbox_client.go
```

### Go Client Usage

```go
import "path/to/matchlock/examples"

client, _ := NewClient("matchlock")
defer client.Close()

// Create sandbox
vmID, _ := client.Create(CreateConfig{
    Image:    "standard",
    CPUs:     1,
    MemoryMB: 512,
})

// Execute commands
result, _ := client.Exec("echo 'Hello!'")
fmt.Println(result.Stdout)

// Write files
client.WriteFile("/workspace/script.sh", []byte("echo hello"), 0755)

// Read files
content, _ := client.ReadFile("/workspace/script.sh")

// List files
files, _ := client.ListFiles("/workspace")
for _, f := range files {
    fmt.Println(f.Name, f.Size)
}
```

## Network Access Example

To allow network access from the sandbox:

```python
# Python
client.create(
    allowed_hosts=["*.openai.com", "api.anthropic.com"]
)
result = client.exec("curl https://api.openai.com/v1/models")
```

```go
// Go
client.Create(CreateConfig{
    AllowedHosts: []string{"*.openai.com", "api.anthropic.com"},
})
result, _ := client.Exec("curl https://api.openai.com/v1/models")
```

## Error Codes

| Code | Description |
|------|-------------|
| -32700 | Parse error |
| -32600 | Invalid request |
| -32601 | Method not found |
| -32602 | Invalid params |
| -32603 | Internal error |
| -32000 | VM failed |
| -32001 | Exec failed |
| -32002 | File operation failed |
