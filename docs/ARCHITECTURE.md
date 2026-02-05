# Matchlock Architecture

## Overview

Matchlock is a secure sandbox for running AI-generated code using Firecracker micro-VMs. It provides:

- **Network Isolation**: All traffic passes through a userspace TCP/IP stack with MITM interception
- **Secret Protection**: Secrets are never exposed to guest code, only placeholders
- **Filesystem Control**: Programmable VFS with copy-on-write overlays
- **Policy Enforcement**: Host allowlists, private IP blocking, request modification

```
┌─────────────────────────────────────────────────────────────────┐
│                          Host System                             │
├─────────────────────────────────────────────────────────────────┤
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────────────┐  │
│  │  Sandbox    │    │   Policy    │    │    VFS Server       │  │
│  │    CLI      │───▶│   Engine    │    │  (CBOR Protocol)    │  │
│  └─────────────┘    └─────────────┘    └─────────────────────┘  │
│         │                  │                      │              │
│         ▼                  ▼                      │              │
│  ┌─────────────────────────────────────┐         │              │
│  │         Network Stack               │         │              │
│  │  ┌─────────┐  ┌─────────┐  ┌─────┐ │         │              │
│  │  │  TAP    │  │  gVisor │  │ TLS │ │         │              │
│  │  │ Device  │──│  tcpip  │──│ MITM│ │         │              │
│  │  └─────────┘  └─────────┘  └─────┘ │         │              │
│  └──────────────────│──────────────────┘         │              │
│                     │                            │              │
├─────────────────────│────────────────────────────│──────────────┤
│                     │        Vsock               │              │
│              ┌──────┴──────┐              ┌──────┴──────┐       │
│              │   Port 5002 │              │   Port 5001 │       │
│              │    Ready    │              │     VFS     │       │
│              └──────┬──────┘              └──────┬──────┘       │
│                     │                            │              │
│  ┌──────────────────┴────────────────────────────┴────────────┐ │
│  │                    Firecracker VM                          │ │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │ │
│  │  │   Guest     │  │   Guest     │  │     /workspace      │ │ │
│  │  │   Agent     │  │   FUSED     │──│    (FUSE mount)     │ │ │
│  │  │ (Port 5000) │  │ (Port 5001) │  └─────────────────────┘ │ │
│  │  └─────────────┘  └─────────────┘                          │ │
│  │                                                             │ │
│  │  ┌───────────────────────────────────────────────────────┐ │ │
│  │  │              Alpine Linux Rootfs                       │ │ │
│  │  │   Python, Node.js, Git, etc.                          │ │ │
│  │  └───────────────────────────────────────────────────────┘ │ │
│  └─────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

## Component Details

### 1. VM Backend (`pkg/vm/linux`)

The VM backend manages Firecracker micro-VMs:

```go
type LinuxBackend struct{}

func (b *LinuxBackend) Create(ctx context.Context, config *VMConfig) (Machine, error)
```

**Responsibilities:**
- Create TAP network devices
- Generate Firecracker JSON configuration
- Start/stop VM processes
- Execute commands via vsock

**TAP Device Setup:**
```
┌─────────────┐         ┌─────────────┐
│    Host     │         │   Guest     │
│  TAP (fd)   │◀───────▶│    eth0     │
│192.168.100.1│         │192.168.100.2│
└─────────────┘         └─────────────┘
```

### 2. Network Stack (`pkg/net`)

Uses gVisor's userspace TCP/IP stack to intercept all network traffic:

```go
type NetworkStack struct {
    stack    *stack.Stack
    policy   *policy.Engine
    caPool   *CAPool
}
```

**Traffic Flow:**
```
Guest eth0 → TAP FD → gVisor Stack → TCP Forwarder → HTTP/TLS Handler → Internet
                                           │
                                           ▼
                                    Policy Engine
                                    (allow/block)
```

**Port Routing:**
- Port 80 → HTTP interceptor (request modification)
- Port 443 → TLS MITM (certificate spoofing)
- Port 53 → DNS forwarder (8.8.8.8)
- Other → Direct passthrough

### 3. TLS MITM (`pkg/net/tls.go`)

Intercepts HTTPS traffic using dynamic certificate generation:

```go
type CAPool struct {
    caCert    *x509.Certificate
    caKey     *rsa.PrivateKey
    certCache sync.Map  // serverName -> *tls.Certificate
}
```

**Certificate Chain:**
```
Root CA (generated once, cached)
    └── Per-domain certificate (generated on-demand)
            └── Client connection (spoofed)
```

The CA certificate is injected into the guest via `CAInjector`.

### 4. Policy Engine (`pkg/policy`)

Enforces security policies on all HTTP/HTTPS requests:

```go
type Engine struct {
    config       *api.NetworkConfig
    placeholders map[string]string  // secret name -> placeholder
}
```

**Features:**
- **Host Allowlisting**: Glob patterns (e.g., `*.openai.com`)
- **Secret Injection**: Replace placeholders with real values
- **Leak Prevention**: Block secrets to unauthorized hosts
- **Private IP Blocking**: Prevent SSRF attacks

**Secret Flow:**
```
Guest Code: curl -H "Authorization: Bearer $API_KEY" https://api.openai.com/...
                                            │
                                            ▼
                                   Placeholder: SANDBOX_SECRET_abc123...
                                            │
                                            ▼
                            Policy Engine (host: api.openai.com ✓)
                                            │
                                            ▼
                                   Real Value: sk-real-api-key...
                                            │
                                            ▼
                                        Internet
```

### 5. VFS System (`pkg/vfs`)

Programmable filesystem with multiple providers:

```go
type Provider interface {
    Readonly() bool
    Stat(path string) (FileInfo, error)
    ReadDir(path string) ([]DirEntry, error)
    Open(path string, flags int, mode os.FileMode) (Handle, error)
    Create(path string, mode os.FileMode) (Handle, error)
    Mkdir(path string, mode os.FileMode) error
    Remove(path string) error
    Rename(oldPath, newPath string) error
}
```

**Provider Types:**

| Provider | Description | Use Case |
|----------|-------------|----------|
| `MemoryProvider` | In-memory filesystem | Temporary workspace |
| `RealFSProvider` | Maps to host directory | Shared data |
| `ReadonlyProvider` | Wraps another provider | Read-only access |
| `OverlayProvider` | Copy-on-write layer | Isolated modifications |
| `MountRouter` | Routes paths to providers | Multiple mounts |

**Example Configuration:**
```go
router := NewMountRouter(map[string]Provider{
    "/workspace": NewOverlayProvider(
        NewMemoryProvider(),                    // Writes go here
        NewReadonlyProvider(                    // Reads from here
            NewRealFSProvider("/host/project"),
        ),
    ),
    "/data": NewReadonlyProvider(
        NewRealFSProvider("/host/shared"),
    ),
})
```

### 6. VFS Protocol (CBOR)

Guest FUSE daemon communicates with host VFS server over vsock:

```
┌─────────────┐         ┌─────────────┐
│   Guest     │  vsock  │    Host     │
│  FUSED      │◀───────▶│  VFSServer  │
│ (FUSE ops)  │  CBOR   │ (providers) │
└─────────────┘         └─────────────┘
```

**Message Format:**
```
┌──────────────┬───────────────────┐
│ Length (4B)  │ CBOR Payload      │
│ big-endian   │ (VFSRequest)      │
└──────────────┴───────────────────┘
```

**Operations:**
```go
const (
    OpLookup  // Stat a path
    OpGetattr // Get file attributes
    OpRead    // Read file data
    OpWrite   // Write file data
    OpCreate  // Create new file
    OpMkdir   // Create directory
    OpUnlink  // Delete file
    OpReaddir // List directory
    // ...
)
```

### 7. Vsock Communication (`pkg/vsock`)

Pure Go implementation of AF_VSOCK for host-guest communication:

```go
const (
    AF_VSOCK        = 40
    VMADDR_CID_HOST = 2
)

func Dial(cid, port uint32) (*Conn, error)
func Listen(port uint32) (*Listener, error)
```

**Port Assignments:**
| Port | Service | Direction |
|------|---------|-----------|
| 5000 | Command Execution | Host → Guest |
| 5001 | VFS Protocol | Guest → Host |
| 5002 | Ready Signal | Host → Guest |

### 8. Guest Agent (`cmd/guest-agent`)

Runs inside the VM to handle host requests:

```go
func main() {
    go serveReady()  // Port 5002: Accept = ready
    serveExec()      // Port 5000: JSON exec requests
}
```

**Exec Protocol:**
```
Request:  [MsgType:1][Length:4][JSON ExecRequest]
Response: [MsgType:1][Length:4][JSON ExecResponse]
```

### 9. Guest FUSE Daemon (`cmd/guest-fused`)

Mounts host VFS at `/workspace`:

```go
func main() {
    client := NewVFSClient()           // Connect to host
    fs := NewFUSEServer("/workspace")  // Mount FUSE
    fs.Serve()                         // Handle FUSE ops
}
```

**FUSE Operations Supported:**
- `LOOKUP`, `GETATTR`, `SETATTR`
- `OPEN`, `READ`, `WRITE`, `RELEASE`
- `CREATE`, `MKDIR`, `UNLINK`, `RMDIR`
- `RENAME`, `READDIR`, `FSYNC`

## Data Flows

### Command Execution

```
1. CLI: sandbox run python script.py
2. CLI → VM Backend: Create VM with TAP + vsock
3. VM Backend → Firecracker: Start with config
4. Guest Agent: Listen on port 5002 (ready)
5. Host: Connect to port 5002 (VM is ready)
6. CLI → Guest Agent (port 5000): ExecRequest{Command: "python script.py"}
7. Guest Agent: Run command, stream output
8. Guest Agent → CLI: ExecResponse{ExitCode, Stdout, Stderr}
```

### File Access

```
1. Guest Code: open("/workspace/data.json")
2. FUSE Kernel: FUSE_LOOKUP + FUSE_OPEN
3. Guest FUSED: VFSRequest{Op: OpLookup, Path: "/data.json"}
4. Host VFSServer: provider.Stat("/data.json")
5. Guest FUSED: VFSRequest{Op: OpOpen, Path: "/data.json"}
6. Host VFSServer: provider.Open("/data.json")
7. Host → Guest: VFSResponse{Handle: 1}
8. Guest Code: read(fd, buf, size)
9. [Similar flow for READ operation]
```

### Network Request

```
1. Guest Code: requests.get("https://api.openai.com/v1/models")
2. Guest eth0 → TAP FD: TCP SYN to api.openai.com:443
3. gVisor Stack: Accept connection
4. TCP Forwarder: Route to TLS handler
5. TLS Handler: Complete TLS handshake with spoofed cert
6. HTTP Handler: Parse request
7. Policy Engine: Check host allowed? Replace secrets?
8. HTTP Handler → Internet: Forward modified request
9. [Response flows back through same path]
```

## Security Model

### Threat Model

| Threat | Mitigation |
|--------|------------|
| Code escapes VM | Firecracker's minimal attack surface |
| Secret exfiltration | Placeholder-only secrets, host verification |
| SSRF attacks | Private IP blocking, host allowlist |
| Malicious network | All traffic through MITM |
| File access | VFS with overlay isolation |

### Trust Boundaries

```
Untrusted: Guest code, network responses
Trusted: Host, Firecracker, Policy engine
```

### Secret Protection

1. Real secrets never enter the VM
2. Guest code uses placeholder tokens
3. MITM proxy substitutes at request time
4. Unauthorized destinations → blocked
5. Secrets in responses → optional scrubbing

## Performance Considerations

### Boot Time
- Firecracker cold start: ~125ms
- Guest agent ready: ~500ms
- Total sandbox ready: <1s

### Network Latency
- gVisor TCP/IP adds ~1ms per connection
- TLS MITM adds ~5ms for certificate generation (cached after first)

### File System
- FUSE adds syscall overhead
- VFS server batches small reads
- Memory provider: O(1) access
- Overlay provider: O(n) for writes, O(1) for reads

## Extensibility

### Adding a New VFS Provider

```go
type CustomProvider struct{}

func (p *CustomProvider) Readonly() bool { return false }
func (p *CustomProvider) Stat(path string) (FileInfo, error) { ... }
// Implement all Provider interface methods
```

### Adding Policy Rules

```go
engine.AddRule(func(req *http.Request, host string) error {
    if strings.Contains(req.URL.Path, "/admin") {
        return errors.New("admin paths blocked")
    }
    return nil
})
```

### Custom Network Handlers

```go
stack.AddHandler(8080, func(conn net.Conn) {
    // Custom protocol handler
})
```
