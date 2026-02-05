# ADR-0001: Go-Based Cross-Platform Sandbox Architecture

## Status

Proposed

## Context

AI agents are generating code that runs immediately and increasingly without human review. That code often calls external APIs, which means it needs credentials and network access. Sandboxing the compute isn't enough—you need to control network egress and protect secrets from exfiltration. You also want to tightly control the filesystem for convenience and persistence management.

We need a sandbox solution that provides:

1. **Lightweight micro-VMs** that boot quickly on both macOS and Linux
2. **Programmable network stack** with host-side HTTP/TLS interception for secret injection and allowlisting
3. **Programmable filesystem** with pluggable providers for different storage backends
4. **Single binary distribution** without runtime dependencies

### Requirements

| Requirement | Priority | Notes |
|-------------|----------|-------|
| macOS + Linux parity | Must have | Same API, same security guarantees |
| Host-side network interception | Must have | MITM for HTTP/TLS, secret injection |
| Programmable filesystem | Must have | Mount providers, hooks |
| Sub-second boot time | Should have | Critical for agentic workloads |
| Single binary distribution | Should have | Simplify installation |
| Extensible hook system | Should have | Custom policies without recompilation |

### Threat Model

AI-generated code runs inside the sandbox with potential root access. The sandbox must:

1. Prevent secret exfiltration to unauthorized hosts
2. Enforce network allowlists at the host level (not bypassable from guest)
3. Control filesystem access and mutations
4. Protect against DNS rebinding attacks

In-guest security measures are insufficient as the agent may have privileged access.

## Decision

We will rewrite the host controller in **Go** with platform-specific VM backends:

- **macOS**: Apple Virtualization.framework via [code-hex/vz](https://github.com/Code-Hex/vz)
- **Linux**: [Firecracker](https://github.com/firecracker-microvm/firecracker-go-sdk) micro-VMs

The network stack will use **gVisor's tcpip package** instead of a custom implementation. MITM and secret injection will be implemented at the application layer on top of this stack.

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Go Host Controller                                │
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                         Public API (pkg/api)                          │  │
│  │  VM.Create() / VM.Exec() / VM.Close() / VM.Mount() / VM.SetPolicy()   │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                                    │                                        │
│          ┌─────────────────────────┼─────────────────────────┐              │
│          ▼                         ▼                         ▼              │
│  ┌───────────────┐       ┌─────────────────┐       ┌─────────────────┐      │
│  │  VM Backend   │       │  Network Stack  │       │   VFS Layer     │      │
│  │  (pkg/vm)     │       │  (pkg/net)      │       │   (pkg/vfs)     │      │
│  │               │       │                 │       │                 │      │
│  │ ┌───────────┐ │       │ ┌─────────────┐ │       │ ┌─────────────┐ │      │
│  │ │ macOS:    │ │       │ │ gvisor/tcpip│ │       │ │ Provider    │ │      │
│  │ │ code-hex/ │ │       │ │ (L2-L4)     │ │       │ │ Interface   │ │      │
│  │ │ vz        │ │       │ └──────┬──────┘ │       │ ├─────────────┤ │      │
│  │ ├───────────┤ │       │        │        │       │ │ Memory      │ │      │
│  │ │ Linux:    │ │       │ ┌──────▼──────┐ │       │ │ RealFS      │ │      │
│  │ │Firecracker│ │       │ │ HTTP/TLS    │ │       │ │ Readonly    │ │      │
│  │ └───────────┘ │       │ │ Interceptor │ │       │ │ Overlay     │ │      │
│  │               │       │ │ (L7 MITM)   │ │       │ └─────────────┘ │      │
│  └───────┬───────┘       │ └──────┬──────┘ │       └────────┬────────┘      │
│          │               │        │        │                │               │
│          │               │ ┌──────▼──────┐ │                │               │
│          │               │ │ Policy      │ │                │               │
│          │               │ │ Engine      │ │                │               │
│          │               │ │ - Allowlist │ │                │               │
│          │               │ │ - Secrets   │ │                │               │
│          │               │ │ - Hooks     │ │                │               │
│          │               │ └─────────────┘ │                │               │
│          │               └─────────────────┘                │               │
└──────────┼─────────────────────────┬────────────────────────┼───────────────┘
           │                         │                        │
           ▼                         ▼                        ▼
┌─────────────────────┐    ┌─────────────────────┐    ┌───────────────────────┐
│ Guest VM            │    │ Internet            │    │ Host Filesystem       │
│ (Linux)             │    │ (filtered)          │    │ (controlled access)   │
└─────────────────────┘    └─────────────────────┘    └───────────────────────┘
```

### Component Details

#### 1. VM Backend (`pkg/vm`)

Platform-specific VM lifecycle management behind a common interface:

```go
type Backend interface {
    Create(config *VMConfig) (*VM, error)
    Start(vm *VM) error
    Stop(vm *VM) error
    Exec(vm *VM, cmd string, opts *ExecOptions) (*ExecResult, error)
    
    // Returns file descriptor for raw Ethernet frames
    NetworkFD(vm *VM) (uintptr, error)
    
    // Returns file descriptor for VFS protocol
    VfsFD(vm *VM) (uintptr, error)
}
```

**macOS Implementation** (`pkg/vm/darwin`):

```go
import "github.com/Code-Hex/vz/v4"

type DarwinBackend struct{}

func (b *DarwinBackend) Create(config *VMConfig) (*VM, error) {
    vzConfig, _ := vz.NewVirtualMachineConfiguration(
        bootLoader,
        config.CPUs,
        config.MemoryMB * 1024 * 1024,
    )
    
    // Raw Ethernet frame access for network interception
    sockPair, _ := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
    netAttachment, _ := vz.NewFileHandleNetworkDeviceAttachment(
        os.NewFile(uintptr(sockPair[1]), "guest-net"),
    )
    
    // Virtio-vsock for VFS protocol
    vsockDevice, _ := vz.NewVirtioSocketDeviceConfiguration()
    
    // ...configure and return VM
}
```

**Linux Implementation** (`pkg/vm/linux`):

```go
import firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

type LinuxBackend struct{}

func (b *LinuxBackend) Create(config *VMConfig) (*VM, error) {
    cfg := firecracker.Config{
        SocketPath:      socketPath,
        KernelImagePath: config.KernelPath,
        KernelArgs:      "console=ttyS0 reboot=k panic=1",
        Drives:          []firecracker.Drive{{...}},
        MachineCfg: firecracker.MachineConfig{
            VcpuCount:  config.CPUs,
            MemSizeMib: config.MemoryMB,
        },
        NetworkInterfaces: []firecracker.NetworkInterface{{
            // TAP device - we'll intercept at the TAP level
            StaticConfiguration: &firecracker.StaticNetworkConfiguration{
                MacAddress:  guestMAC,
                HostDevName: tapDevice,
            },
        }},
        VsockDevices: []firecracker.VsockDevice{{
            Path: vsockPath,
            CID:  3,
        }},
    }
    
    machine, _ := firecracker.NewMachine(ctx, cfg)
    // ...
}
```

#### 2. Network Stack (`pkg/net`)

Built on gVisor's userspace TCP/IP stack with application-layer interception.

**Platform Abstraction:**

Both VM backends expose raw Ethernet frames via file descriptors. gVisor's `fdbased` link endpoint consumes these uniformly:

| Platform | Source | Mechanism |
|----------|--------|-----------|
| Linux | TAP device (`/dev/net/tun`) | Firecracker attaches guest virtio-net to TAP |
| macOS | Unix socket pair | `VZFileHandleNetworkDeviceAttachment` sends frames over socket |

```
┌─────────────────────────────────────────────────────────────────────┐
│  Linux                                                              │
│  ┌─────────────┐     ┌─────────────┐     ┌────────────────────────┐ │
│  │ Firecracker │────▶│ TAP device  │────▶│ gVisor fdbased.New()   │ │
│  │   (Guest)   │     │  (tap0)     │     │                        │ │
│  └─────────────┘     └─────────────┘     └───────────┬────────────┘ │
└──────────────────────────────────────────────────────┼──────────────┘
                                                       │
┌──────────────────────────────────────────────────────┼──────────────┐
│  macOS                                               │              │
│  ┌─────────────┐     ┌─────────────┐     ┌───────────▼────────────┐ │
│  │ VZ Guest    │────▶│ Socket pair │────▶│ gVisor fdbased.New()   │ │
│  │ (virtio-net)│     │ (FD)        │     │                        │ │
│  └─────────────┘     └─────────────┘     └───────────┬────────────┘ │
└──────────────────────────────────────────────────────┼──────────────┘
                                                       │
                                                       ▼
                                          ┌────────────────────────┐
                                          │     tcpip.Stack        │
                                          │  (shared across both)  │
                                          └───────────┬────────────┘
                                                      │
                                                      ▼
                                          ┌────────────────────────┐
                                          │   HTTP/TLS MITM        │
                                          │   Policy Engine        │
                                          └────────────────────────┘
```

**Linux TAP Device Setup:**

```go
import (
    "syscall"
    "unsafe"
)

func createTAP(name string) (int, error) {
    // Open /dev/net/tun
    fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
    if err != nil {
        return 0, err
    }
    
    // Configure as TAP (Ethernet frames, not IP packets)
    var ifr struct {
        name  [16]byte
        flags uint16
        _     [22]byte
    }
    copy(ifr.name[:], name)
    ifr.flags = syscall.IFF_TAP | syscall.IFF_NO_PI
    
    _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
        uintptr(syscall.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
    if errno != 0 {
        syscall.Close(fd)
        return 0, errno
    }
    
    return fd, nil
}
```

**Firecracker Network Configuration:**

```go
import firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

cfg := firecracker.Config{
    NetworkInterfaces: []firecracker.NetworkInterface{{
        StaticConfiguration: &firecracker.StaticNetworkConfiguration{
            MacAddress:  "AA:BB:CC:DD:EE:FF",
            HostDevName: tapName,  // TAP device created above
        },
    }},
}
```

**macOS Socket Pair Setup:**

```go
import "github.com/Code-Hex/vz/v4"

// Create socket pair for Ethernet frame exchange
sockPair, _ := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
hostFD := sockPair[0]   // Our end - pass to gVisor
guestFD := sockPair[1]  // Guest end - pass to VZ

netAttachment, _ := vz.NewFileHandleNetworkDeviceAttachment(
    os.NewFile(uintptr(guestFD), "guest-net"),
)
networkDevice, _ := vz.NewVirtioNetworkDeviceConfiguration(netAttachment)
```

**Unified gVisor Stack Initialization:**

```go
import (
    "gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
    "gvisor.dev/gvisor/pkg/tcpip/stack"
    "gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
    "gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// Works identically for TAP FD (Linux) or socket FD (macOS)
func createNetworkStack(fd int) (*stack.Stack, error) {
    s := stack.New(stack.Options{
        NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
        TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
    })
    
    // fdbased works with any FD that sends/receives Ethernet frames
    linkEP, _ := fdbased.New(&fdbased.Options{
        FDs:            []int{fd},
        MTU:            1500,
        EthernetHeader: true,
    })
    
    s.CreateNIC(1, linkEP)
    s.AddAddress(1, ipv4.ProtocolNumber, tcpip.AddrFrom4([4]byte{192, 168, 100, 1}))
    s.SetRouteTable([]tcpip.Route{{
        Destination: header.IPv4EmptySubnet,
        NIC:         1,
    }})
    
    return s, nil
}
```

**Full Linux Backend Integration:**

```go
func (b *LinuxBackend) Create(config *VMConfig) (*VM, error) {
    tapName := fmt.Sprintf("sandbox%d", os.Getpid())
    
    // 1. Create TAP device
    tapFD, _ := createTAP(tapName)
    
    // 2. Configure host side of TAP
    configureInterface(tapName, "192.168.100.1/24")
    
    // 3. Create Firecracker VM attached to TAP
    machine, _ := firecracker.NewMachine(ctx, firecracker.Config{
        NetworkInterfaces: []firecracker.NetworkInterface{{
            StaticConfiguration: &firecracker.StaticNetworkConfiguration{
                MacAddress:  guestMAC,
                HostDevName: tapName,
            },
        }},
        // ... other config
    })
    
    // 4. Create gVisor stack from TAP FD
    netStack, _ := createNetworkStack(tapFD)
    
    // 5. Install TCP forwarder for interception
    tcpForwarder := tcp.NewForwarder(netStack, 0, 65535, b.handleTCPConnection)
    netStack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
    
    return &VM{machine: machine, netStack: netStack}, nil
}
```

**Network Stack Implementation:**

```go
import (
    "gvisor.dev/gvisor/pkg/tcpip"
    "gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
    "gvisor.dev/gvisor/pkg/tcpip/stack"
    "gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
    "gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type NetworkStack struct {
    stack       *stack.Stack
    policy      *PolicyEngine
    interceptor *HTTPInterceptor
    mitmCA      *x509.Certificate
    mitmKey     *rsa.PrivateKey
}

func NewNetworkStack(fd int, policy *PolicyEngine) (*NetworkStack, error) {
    // Create gVisor network stack
    s := stack.New(stack.Options{
        NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
        TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
    })
    
    // Attach to raw Ethernet FD from VM backend
    linkEP, _ := fdbased.New(&fdbased.Options{
        FDs:            []int{fd},
        MTU:            1500,
        EthernetHeader: true,
    })
    
    // Create NIC
    s.CreateNIC(1, linkEP)
    s.AddAddress(1, ipv4.ProtocolNumber, tcpip.Address(guestGatewayIP))
    
    // Set up TCP forwarder for interception
    tcpForwarder := tcp.NewForwarder(s, 0, 65535, ns.handleTCPConnection)
    s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
    
    return &NetworkStack{stack: s, policy: policy}, nil
}

func (ns *NetworkStack) handleTCPConnection(r *tcp.ForwarderRequest) {
    // Determine if HTTP (80) or HTTPS (443)
    if r.ID().LocalPort == 443 {
        go ns.handleTLS(r)
    } else if r.ID().LocalPort == 80 {
        go ns.handleHTTP(r)
    } else {
        // Direct passthrough for non-HTTP ports (if allowed by policy)
        go ns.handlePassthrough(r)
    }
}
```

**TLS MITM Implementation**:

```go
func (ns *NetworkStack) handleTLS(r *tcp.ForwarderRequest) {
    // Accept the connection from guest
    ep, _ := r.CreateEndpoint(&waiter.Queue{})
    guestConn := gonet.NewTCPConn(&wq, ep)
    
    // Perform TLS handshake with guest using dynamic cert
    guestTLS := tls.Server(guestConn, &tls.Config{
        GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
            // Generate certificate for requested SNI
            return ns.generateCert(hello.ServerName)
        },
    })
    
    // Check policy before connecting to real server
    if !ns.policy.IsAllowed(hello.ServerName) {
        guestTLS.Close()
        return
    }
    
    // Connect to real server
    realConn, _ := tls.Dial("tcp", hello.ServerName+":443", &tls.Config{})
    
    // Intercept HTTP over the TLS connections
    ns.interceptHTTP(guestTLS, realConn, hello.ServerName)
}

func (ns *NetworkStack) interceptHTTP(guest, real net.Conn, host string) {
    // Parse HTTP request from guest
    req, _ := http.ReadRequest(bufio.NewReader(guest))
    
    // Apply request hooks (secret injection)
    req = ns.policy.OnRequest(req, host)
    
    // Forward to real server
    req.Write(real)
    
    // Read response
    resp, _ := http.ReadResponse(bufio.NewReader(real), req)
    
    // Apply response hooks
    resp = ns.policy.OnResponse(resp, host)
    
    // Send to guest
    resp.Write(guest)
}
```

#### 3. Policy Engine (`pkg/policy`)

Handles allowlists, secret injection, and extensible hooks:

```go
type Policy struct {
    AllowedHosts  []string              // Glob patterns: ["*.openai.com", "api.anthropic.com"]
    BlockPrivate  bool                  // Block RFC1918 addresses (default: true)
    Secrets       map[string]Secret     // Name -> Secret mapping
    RequestHooks  []RequestHookFunc     // Custom request interceptors
    ResponseHooks []ResponseHookFunc    // Custom response interceptors
}

type Secret struct {
    Value       string   // Real secret value
    Placeholder string   // Random placeholder exposed to guest
    Hosts       []string // Hosts where injection is allowed
}

type PolicyEngine struct {
    policy *Policy
}

func (pe *PolicyEngine) IsAllowed(host string) bool {
    // Check against allowlist patterns
    for _, pattern := range pe.policy.AllowedHosts {
        if matchGlob(pattern, host) {
            return true
        }
    }
    return false
}

func (pe *PolicyEngine) OnRequest(req *http.Request, host string) *http.Request {
    // Inject secrets (replace placeholders with real values)
    for name, secret := range pe.policy.Secrets {
        if !matchesAny(secret.Hosts, host) {
            // Secret not allowed for this host - if placeholder is present, block
            if containsPlaceholder(req, secret.Placeholder) {
                return nil // Will cause connection to be blocked
            }
            continue
        }
        req = replaceInRequest(req, secret.Placeholder, secret.Value)
    }
    
    // Run custom hooks
    for _, hook := range pe.policy.RequestHooks {
        req = hook(req, host)
    }
    
    return req
}
```

**Extensibility via Starlark** (optional):

```go
import "go.starlark.net/starlark"

func (pe *PolicyEngine) LoadStarlarkHooks(script string) error {
    thread := &starlark.Thread{Name: "policy"}
    globals, _ := starlark.ExecFile(thread, "policy.star", script, nil)
    
    if onReq, ok := globals["on_request"]; ok {
        pe.policy.RequestHooks = append(pe.policy.RequestHooks, func(req *http.Request, host string) *http.Request {
            // Call Starlark function
            result, _ := starlark.Call(thread, onReq, starlark.Tuple{...}, nil)
            // ...
        })
    }
}
```

#### 4. VFS Layer (`pkg/vfs`)

Provider-based virtual filesystem with two mounting mechanisms:

**Hybrid Approach:**

| Mechanism | Use Case | Performance | Programmability |
|-----------|----------|-------------|-----------------|
| virtio-fs | Bulk data, host directories | Near-native | Limited (direct mapping) |
| virtio-vsock + FUSE | Custom providers, hooks | Higher latency | Full |

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Guest                                                                      │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                            Application                              │    │
│  └──────────────────┬────────────────────────────────┬─────────────────┘    │
│                     │                                │                      │
│                     ▼                                ▼                      │
│  ┌──────────────────────────────┐    ┌──────────────────────────────────┐   │
│  │  /mnt/data (virtio-fs)       │    │  /workspace (FUSE)               │   │
│  │  Fast, direct host access    │    │  Programmable providers          │   │
│  │  Kernel driver, no daemon    │    │  fused daemon over vsock         │   │
│  └──────────────┬───────────────┘    └───────────────┬──────────────────┘   │
└─────────────────┼────────────────────────────────────┼──────────────────────┘
                  │ virtio                             │ virtio-vsock
                  ▼                                    ▼
┌─────────────────────────────────┐    ┌──────────────────────────────────────┐
│  Host (virtiofsd / VZ built-in) │    │  Host (VFS RPC Server)               │
│  /host/data/ (real directory)   │    │  MountRouterProvider                 │
│                                 │    │    /workspace → MemoryProvider       │
│                                 │    │    /config    → ReadonlyProvider     │
│                                 │    │    /secrets   → MemoryProvider       │
└─────────────────────────────────┘    └──────────────────────────────────────┘
```

**Configuration API:**

```go
type VMConfig struct {
    // Fast direct mounts via virtio-fs (host directories only)
    DirectMounts map[string]DirectMount
    
    // Programmable mounts via VFS providers
    VFSMounts map[string]Provider
}

type DirectMount struct {
    HostPath string
    Readonly bool
}
```

```go
vm, _ := sandbox.Create(&sandbox.VMConfig{
    // Fast: virtio-fs direct sharing
    DirectMounts: map[string]DirectMount{
        "/mnt/data": {HostPath: "/host/large-dataset", Readonly: true},
    },
    
    // Programmable: custom providers with hooks
    VFSMounts: map[string]Provider{
        "/workspace": NewMemoryProvider(),
        "/config":    NewReadonlyProvider(NewRealFSProvider("/etc/app")),
        "/secrets":   NewMemoryProvider(),  // CA certs injected here
    },
})
```

**virtio-fs Setup (macOS):**

```go
import "github.com/Code-Hex/vz/v4"

func setupVirtioFS(tag, hostPath string, readonly bool) (*vz.VirtioFileSystemDeviceConfiguration, error) {
    share, _ := vz.NewSingleDirectoryShare(
        vz.NewSharedDirectory(hostPath, readonly),
    )
    
    fsConfig, _ := vz.NewVirtioFileSystemDeviceConfiguration(tag)
    fsConfig.SetDirectoryShare(share)
    
    return fsConfig, nil
}

// Guest mounts with: mount -t virtiofs <tag> /mnt/path
```

**virtio-fs Setup (Linux/Firecracker):**

Firecracker doesn't have built-in virtio-fs. Options:

1. Use external `virtiofsd` daemon
2. Use virtio-vsock for all mounts (simpler, consistent with macOS)

```go
// Option 1: External virtiofsd
func setupVirtioFSD(socketPath, sharedDir string) (*exec.Cmd, error) {
    cmd := exec.Command("virtiofsd",
        "--socket-path", socketPath,
        "--shared-dir", sharedDir,
        "--cache", "auto",
    )
    return cmd, cmd.Start()
}

// Option 2: Skip virtio-fs on Linux, use virtio-vsock for everything
// This simplifies the implementation at cost of some performance
```

**virtio-vsock + FUSE Architecture:**

```
┌─────────────────────────────────────────────────────────────────┐
│  Guest                                                          │
│  ┌───────────┐     ┌────────────┐     ┌──────────────────────┐  │
│  │ App       │────▶│ FUSE       │◄───▶│ fused                │  │
│  │ open()    │     │ (kernel)   │     │ (Zig/Rust daemon)    │  │
│  └───────────┘     └────────────┘     └──────────┬───────────┘  │
└───────────────────────────────────────────────────┼─────────────┘
                                                    │ virtio-vsock
                                                    │ CID=2 (host), Port=5000
                                                    ▼
┌───────────────────────────────────────────────────────────────────┐
│  Host                                                             │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                    VFS RPC Server                           │  │
│  │  ┌───────────────────────────────────────────────────────┐  │  │
│  │  │                 MountRouterProvider                   │  │  │
│  │  │                                                       │  │  │
│  │  │  /workspace ──▶ MemoryProvider                        │  │  │
│  │  │  /data ──────▶ ReadonlyProvider(RealFSProvider)       │  │  │
│  │  │  /secrets ───▶ MemoryProvider (CA certs)              │  │  │
│  │  │  /output ────▶ OverlayProvider(Memory, RealFS)        │  │  │
│  │  └───────────────────────────────────────────────────────┘  │  │
│  └─────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────┘
```

**VFS Protocol (CBOR over virtio-vsock):**

```go
type VFSRequest struct {
    Op     OpCode `cbor:"op"`
    Path   string `cbor:"path,omitempty"`
    Handle uint64 `cbor:"fh,omitempty"`
    Offset int64  `cbor:"off,omitempty"`
    Size   uint32 `cbor:"sz,omitempty"`
    Data   []byte `cbor:"data,omitempty"`
    Flags  uint32 `cbor:"flags,omitempty"`
    Mode   uint32 `cbor:"mode,omitempty"`
}

type VFSResponse struct {
    Err     int32    `cbor:"err"`            // 0 = success, negative = errno
    Stat    *Stat    `cbor:"stat,omitempty"`
    Data    []byte   `cbor:"data,omitempty"`
    Written uint32   `cbor:"written,omitempty"`
    Handle  uint64   `cbor:"fh,omitempty"`
    Entries []Dirent `cbor:"entries,omitempty"`
}

type OpCode uint8

const (
    OpLookup OpCode = iota
    OpGetattr
    OpSetattr
    OpRead
    OpWrite
    OpCreate
    OpMkdir
    OpUnlink
    OpRmdir
    OpRename
    OpOpen
    OpRelease
    OpReaddir
    OpFsync
)
```

**Host VFS Server:**

```go
import (
    "github.com/fxamacker/cbor/v2"
    "github.com/mdlayher/vsock"
)

type VFSServer struct {
    provider Provider
    handles  map[uint64]Handle
    nextFH   uint64
}

func (s *VFSServer) Start() error {
    l, _ := vsock.Listen(5000, nil)
    
    for {
        conn, _ := l.Accept()
        go s.handleConnection(conn)
    }
}

func (s *VFSServer) handleConnection(conn net.Conn) {
    decoder := cbor.NewDecoder(conn)
    encoder := cbor.NewEncoder(conn)
    
    for {
        var req VFSRequest
        if err := decoder.Decode(&req); err != nil {
            return
        }
        
        resp := s.dispatch(&req)
        encoder.Encode(resp)
    }
}

func (s *VFSServer) dispatch(req *VFSRequest) *VFSResponse {
    switch req.Op {
    case OpLookup:
        info, err := s.provider.Stat(req.Path)
        if err != nil {
            return &VFSResponse{Err: errnoFromError(err)}
        }
        return &VFSResponse{Stat: statFromInfo(info)}
        
    case OpOpen:
        handle, err := s.provider.Open(req.Path, int(req.Flags), os.FileMode(req.Mode))
        if err != nil {
            return &VFSResponse{Err: errnoFromError(err)}
        }
        fh := atomic.AddUint64(&s.nextFH, 1)
        s.handles[fh] = handle
        return &VFSResponse{Handle: fh}
        
    case OpRead:
        handle := s.handles[req.Handle]
        buf := make([]byte, req.Size)
        n, err := handle.ReadAt(buf, req.Offset)
        if err != nil && err != io.EOF {
            return &VFSResponse{Err: errnoFromError(err)}
        }
        return &VFSResponse{Data: buf[:n]}
        
    case OpWrite:
        handle := s.handles[req.Handle]
        n, err := handle.WriteAt(req.Data, req.Offset)
        if err != nil {
            return &VFSResponse{Err: errnoFromError(err)}
        }
        return &VFSResponse{Written: uint32(n)}
        
    case OpReaddir:
        entries, err := s.provider.ReadDir(req.Path)
        if err != nil {
            return &VFSResponse{Err: errnoFromError(err)}
        }
        return &VFSResponse{Entries: direntsFromEntries(entries)}
        
    case OpRelease:
        if handle, ok := s.handles[req.Handle]; ok {
            handle.Close()
            delete(s.handles, req.Handle)
        }
        return &VFSResponse{}
        
    // ... other operations
    }
    
    return &VFSResponse{Err: -int32(syscall.ENOSYS)}
}
```

**Guest FUSE Daemon (fused):**

Written in Zig or Rust for minimal binary size and fast startup:

```zig
const std = @import("std");
const fuse = @cImport(@cInclude("fuse_lowlevel.h"));

const VFSClient = struct {
    sock: std.os.socket_t,
    
    pub fn connect() !VFSClient {
        // Connect to host via vsock (CID 2 = host)
        const sock = try std.os.socket(std.os.AF.VSOCK, std.os.SOCK.STREAM, 0);
        const addr = std.os.sockaddr.vsock{ .cid = 2, .port = 5000 };
        try std.os.connect(sock, @ptrCast(&addr), @sizeOf(@TypeOf(addr)));
        return .{ .sock = sock };
    }
    
    pub fn lookup(self: *VFSClient, path: []const u8) !Stat {
        try self.sendRequest(.{ .op = .Lookup, .path = path });
        const resp = try self.recvResponse();
        if (resp.err != 0) return error.VFSError;
        return resp.stat.?;
    }
    
    pub fn read(self: *VFSClient, fh: u64, offset: i64, size: u32) ![]u8 {
        try self.sendRequest(.{ .op = .Read, .handle = fh, .offset = offset, .size = size });
        const resp = try self.recvResponse();
        if (resp.err != 0) return error.VFSError;
        return resp.data;
    }
    
    // ... other operations
};

pub fn main() !void {
    var client = try VFSClient.connect();
    
    const ops = fuse.fuse_lowlevel_ops{
        .lookup = fuseLookupcbor,
        .getattr = fuseGetattr,
        .read = fuseRead,
        .write = fuseWrite,
        .readdir = fuseReaddir,
        // ...
    };
    
    const args = [_][*:0]const u8{ "fused", "-f", "/data" };
    _ = fuse.fuse_main(@intCast(args.len), @ptrCast(&args), &ops, &client);
}
```

**Provider Interface:**

```go
type Provider interface {
    Readonly() bool
    
    // Metadata
    Stat(path string) (FileInfo, error)
    Readdir(path string) ([]DirEntry, error)
    
    // File operations
    Open(path string, flags int, mode os.FileMode) (Handle, error)
    Create(path string, mode os.FileMode) (Handle, error)
    
    // Directory operations
    Mkdir(path string, mode os.FileMode) error
    Remove(path string) error
    Rename(oldPath, newPath string) error
    
    // Optional
    Symlink(target, link string) error
    Readlink(path string) (string, error)
}

type Handle interface {
    Read(p []byte, offset int64) (int, error)
    Write(p []byte, offset int64) (int, error)
    Sync() error
    Close() error
}
```

**Built-in Providers**:

| Provider | Description |
|----------|-------------|
| `MemoryProvider` | In-memory filesystem |
| `RealFSProvider` | Proxy to host directory |
| `ReadonlyProvider` | Wraps any provider, blocks writes |
| `OverlayProvider` | Copy-on-write over base provider |
| `MountRouter` | Routes paths to different providers |

```go
// Example: Mount configuration
vfs := NewMountRouter(map[string]Provider{
    "/workspace": NewMemoryProvider(),
    "/data":      NewReadonlyProvider(NewRealFSProvider("/host/data")),
    "/secrets":   NewReadonlyProvider(NewMemoryProvider()), // Injected CA certs
})
```

**VFS Protocol** (over virtio-vsock):

```go
type VFSServer struct {
    provider Provider
    handles  map[uint64]Handle
}

func (s *VFSServer) HandleRequest(req *VFSRequest) *VFSResponse {
    switch req.Op {
    case OpLookup:
        info, err := s.provider.Stat(req.Path)
        return &VFSResponse{Stat: info, Err: err}
    case OpRead:
        h := s.handles[req.Handle]
        buf := make([]byte, req.Size)
        n, err := h.Read(buf, req.Offset)
        return &VFSResponse{Data: buf[:n], Err: err}
    // ...
    }
}
```

### Programmatic API

The sandbox uses a **daemonless architecture** (like Podman). Each sandbox runs as a standalone subprocess, communicating via **JSON-RPC over stdin/stdout**. This provides:

- **Simplicity** - No daemon lifecycle management
- **Isolation** - Each sandbox is independent; crashes don't affect others
- **Security** - Runs as invoking user, no privilege escalation
- **Debuggability** - JSON-RPC is easy to inspect and test

#### Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Client Application                             │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                         sandbox.Create()                            │    │
│  │                               │                                     │    │
│  │                               ▼                                     │    │
│  │                    fork/exec "sandbox --rpc"                        │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                                  │                                          │
│                       stdin/stdout (JSON-RPC 2.0)                           │
│                                  │                                          │
└──────────────────────────────────┼──────────────────────────────────────────┘
                                   ▼
┌───────────────────────────────────────────────────────────────────────────────┐
│                      sandbox process (child of client)                        │
│                                                                               │
│  ┌─────────────────────────────────────────────────────────────────────────┐  │
│  │                    JSON-RPC Handler (stdin/stdout)                      │  │
│  │  • Requests: create, exec, write_file, read_file, close                 │  │
│  │  • Notifications: event (network, file, exec)                           │  │
│  └─────────────────────────────────────────────────────────────────────────┘  │
│                                    │                                          │
│  ┌──────────────┐  ┌───────────────┴───────────────┐  ┌──────────────────┐    │
│  │  VM Backend  │  │       Network Stack           │  │    VFS Layer     │    │
│  └──────────────┘  └───────────────────────────────┘  └──────────────────┘    │
│                                    │                                          │
│                           Single VM instance                                  │
└───────────────────────────────────────────────────────────────────────────────┘
```

#### JSON-RPC Protocol

Communication uses [JSON-RPC 2.0](https://www.jsonrpc.org/specification) over stdin/stdout, one message per line.

**Request:**
```json
{"jsonrpc": "2.0", "method": "exec", "params": {"command": "echo hello"}, "id": 1}
```

**Response:**
```json
{"jsonrpc": "2.0", "result": {"exit_code": 0, "stdout": "aGVsbG8K"}, "id": 1}
```

**Event Notification (no id, streamed asynchronously):**
```json
{"jsonrpc": "2.0", "method": "event", "params": {"type": "network", "network": {...}}}
```

**Error:**
```json
{"jsonrpc": "2.0", "error": {"code": -32000, "message": "VM failed to start"}, "id": 1}
```

#### RPC Methods

**create** - Initialize the VM with configuration:
```json
{
  "method": "create",
  "params": {
    "image": "standard",
    "resources": {"cpus": 2, "memory_mb": 1024, "timeout_seconds": 300},
    "network": {
      "allowed_hosts": ["api.openai.com", "*.amazonaws.com"],
      "block_private_ips": true,
      "secrets": {
        "OPENAI_API_KEY": {
          "value": "sk-...",
          "hosts": ["api.openai.com"]
        }
      },
      "policy_script": "def is_allowed(host): return True"
    },
    "vfs": {
      "direct_mounts": {
        "/mnt/data": {"host_path": "/host/data", "readonly": true}
      },
      "mounts": {
        "/workspace": {"type": "memory"},
        "/config": {"type": "real_fs", "host_path": "/etc/app", "readonly": true},
        "/output": {"type": "overlay", "upper": {"type": "memory"}, "lower": {"type": "real_fs", "host_path": "/base"}}
      }
    },
    "env": {"OPENAI_API_KEY": "${OPENAI_API_KEY}"}
  }
}
```

**Response:**
```json
{
  "result": {
    "env": {"OPENAI_API_KEY": "SANDBOX_PLACEHOLDER_a1b2c3..."}
  }
}
```

**exec** - Execute a command:
```json
{"method": "exec", "params": {"command": "python /workspace/script.py", "working_dir": "/workspace"}}
```

**Response:**
```json
{"result": {"exit_code": 0, "stdout": "base64...", "stderr": "", "duration_ms": 1234}}
```

**exec_stream** - Execute with streaming output (returns multiple responses):
```json
{"method": "exec_stream", "params": {"command": "python script.py"}}
```

**Responses (streamed):**
```json
{"result": {"stdout": "base64chunk1..."}, "id": 1}
{"result": {"stdout": "base64chunk2..."}, "id": 1}
{"result": {"stderr": "base64error..."}, "id": 1}
{"result": {"exit_code": 0, "duration_ms": 5000}, "id": 1}
```

**write_file** - Write file to sandbox:
```json
{"method": "write_file", "params": {"path": "/workspace/hello.py", "content": "base64...", "mode": 420}}
```

**read_file** - Read file from sandbox:
```json
{"method": "read_file", "params": {"path": "/workspace/output.json"}}
```

**Response:**
```json
{"result": {"content": "base64..."}}
```

**list_files** - List directory contents:
```json
{"method": "list_files", "params": {"path": "/workspace"}}
```

**Response:**
```json
{"result": {"files": [{"name": "hello.py", "size": 42, "mode": 420, "is_dir": false}]}}
```

**close** - Shutdown the VM:
```json
{"method": "close", "params": {}}
```

#### Event Notifications

Events are sent as JSON-RPC notifications (no `id` field) and can arrive at any time:

**Network event:**
```json
{
  "jsonrpc": "2.0",
  "method": "event",
  "params": {
    "type": "network",
    "timestamp": 1699900000,
    "network": {
      "method": "POST",
      "url": "https://api.openai.com/v1/chat/completions",
      "status_code": 200,
      "request_bytes": 1024,
      "response_bytes": 4096,
      "duration_ms": 850,
      "blocked": false
    }
  }
}
```

**File event:**
```json
{
  "jsonrpc": "2.0",
  "method": "event",
  "params": {
    "type": "file",
    "timestamp": 1699900001,
    "file": {
      "op": "write",
      "path": "/workspace/output.json",
      "size": 2048
    }
  }
}
```

#### CLI Usage

```bash
# Interactive mode
sandbox run python script.py

# RPC mode (for programmatic access)
sandbox --rpc

# With configuration file
sandbox --rpc --config sandbox.yaml

# One-shot with inline config
sandbox --rpc --image standard --allow-host "api.openai.com"
```

#### VM Accounting

Without a central daemon, VM state is tracked via a **state directory**. Each sandbox process registers itself on startup and cleans up on exit.

**State Directory Structure:**

```
~/.sandbox/
├── vms/
│   ├── vm-a1b2c3d4/
│   │   ├── pid             # Process ID of sandbox process
│   │   ├── config.json     # VM configuration
│   │   ├── status          # "running" | "stopped" | "crashed"
│   │   ├── created_at      # Unix timestamp
│   │   ├── socket          # Optional Unix socket for IPC
│   │   └── logs/
│   │       └── vm.log      # VM stdout/stderr
│   └── vm-e5f6g7h8/
│       └── ...
├── cache/                  # Downloaded images
└── config.yaml             # Global config
```

**CLI Commands:**

```bash
# List all VMs (running and stopped)
sandbox list
# ID           STATUS    IMAGE      CREATED         PID     COMMAND
# vm-a1b2c3d4  running   standard   5 minutes ago   12345   python script.py
# vm-e5f6g7h8  stopped   minimal    2 hours ago     -       bash

# List only running VMs
sandbox list --running

# Get details of a specific VM
sandbox get vm-a1b2c3d4
# {
#   "id": "vm-a1b2c3d4",
#   "status": "running",
#   "pid": 12345,
#   "image": "standard",
#   "created_at": "2024-01-15T10:30:00Z",
#   "config": {
#     "network": {"allowed_hosts": ["api.openai.com"]},
#     "vfs": {"mounts": {"/workspace": {"type": "memory"}}}
#   }
# }

# Kill a running VM
sandbox kill vm-a1b2c3d4

# Kill all running VMs
sandbox kill --all

# Remove stopped VM state (cleanup)
sandbox rm vm-e5f6g7h8

# Remove all stopped VMs
sandbox rm --stopped

# Prune stale state (crashed VMs, orphaned directories)
sandbox prune
```

**State Management Implementation:**

```go
// pkg/state/state.go

type VMState struct {
    ID        string            `json:"id"`
    PID       int               `json:"pid"`
    Status    string            `json:"status"` // running, stopped, crashed
    Image     string            `json:"image"`
    CreatedAt time.Time         `json:"created_at"`
    Config    json.RawMessage   `json:"config"`
}

type StateManager struct {
    baseDir string // ~/.sandbox/vms
}

func NewStateManager() *StateManager {
    home, _ := os.UserHomeDir()
    return &StateManager{
        baseDir: filepath.Join(home, ".sandbox", "vms"),
    }
}

// Register called when sandbox process starts
func (m *StateManager) Register(id string, config *Config) error {
    dir := filepath.Join(m.baseDir, id)
    os.MkdirAll(dir, 0755)
    
    // Write PID
    os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0644)
    
    // Write config
    configJSON, _ := json.Marshal(config)
    os.WriteFile(filepath.Join(dir, "config.json"), configJSON, 0644)
    
    // Write status
    os.WriteFile(filepath.Join(dir, "status"), []byte("running"), 0644)
    
    // Write timestamp
    os.WriteFile(filepath.Join(dir, "created_at"), []byte(time.Now().Format(time.RFC3339)), 0644)
    
    return nil
}

// Unregister called when sandbox process exits
func (m *StateManager) Unregister(id string) error {
    dir := filepath.Join(m.baseDir, id)
    os.WriteFile(filepath.Join(dir, "status"), []byte("stopped"), 0644)
    os.Remove(filepath.Join(dir, "pid"))
    return nil
}

// List returns all VM states
func (m *StateManager) List() ([]VMState, error) {
    entries, _ := os.ReadDir(m.baseDir)
    var states []VMState
    
    for _, entry := range entries {
        if !entry.IsDir() {
            continue
        }
        
        state, err := m.Get(entry.Name())
        if err != nil {
            continue
        }
        
        // Check if process is actually running
        if state.Status == "running" {
            if !m.isProcessRunning(state.PID) {
                state.Status = "crashed"
                os.WriteFile(filepath.Join(m.baseDir, state.ID, "status"), []byte("crashed"), 0644)
            }
        }
        
        states = append(states, state)
    }
    
    return states, nil
}

// Get returns state for a specific VM
func (m *StateManager) Get(id string) (VMState, error) {
    dir := filepath.Join(m.baseDir, id)
    
    var state VMState
    state.ID = id
    
    if pidBytes, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
        state.PID, _ = strconv.Atoi(string(pidBytes))
    }
    
    if statusBytes, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
        state.Status = string(statusBytes)
    }
    
    if configBytes, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
        state.Config = configBytes
        
        var cfg struct {
            Image string `json:"image"`
        }
        json.Unmarshal(configBytes, &cfg)
        state.Image = cfg.Image
    }
    
    if createdBytes, err := os.ReadFile(filepath.Join(dir, "created_at")); err == nil {
        state.CreatedAt, _ = time.Parse(time.RFC3339, string(createdBytes))
    }
    
    return state, nil
}

// Kill sends SIGTERM to a running VM
func (m *StateManager) Kill(id string) error {
    state, err := m.Get(id)
    if err != nil {
        return err
    }
    
    if state.PID == 0 {
        return fmt.Errorf("VM %s is not running", id)
    }
    
    process, err := os.FindProcess(state.PID)
    if err != nil {
        return err
    }
    
    return process.Signal(syscall.SIGTERM)
}

// Remove deletes VM state directory
func (m *StateManager) Remove(id string) error {
    state, _ := m.Get(id)
    if state.Status == "running" {
        return fmt.Errorf("cannot remove running VM %s, kill it first", id)
    }
    
    return os.RemoveAll(filepath.Join(m.baseDir, id))
}

// Prune removes stale/crashed VM states
func (m *StateManager) Prune() ([]string, error) {
    states, _ := m.List()
    var pruned []string
    
    for _, state := range states {
        if state.Status == "crashed" || state.Status == "stopped" {
            m.Remove(state.ID)
            pruned = append(pruned, state.ID)
        }
    }
    
    return pruned, nil
}

func (m *StateManager) isProcessRunning(pid int) bool {
    if pid == 0 {
        return false
    }
    process, err := os.FindProcess(pid)
    if err != nil {
        return false
    }
    // On Unix, FindProcess always succeeds, so we send signal 0 to check
    err = process.Signal(syscall.Signal(0))
    return err == nil
}
```

**Automatic Cleanup:**

The sandbox process registers cleanup handlers:

```go
func main() {
    vmID := generateVMID()
    state := state.NewStateManager()
    
    // Register on startup
    state.Register(vmID, config)
    
    // Cleanup on exit (normal or crash)
    defer state.Unregister(vmID)
    
    // Handle signals for graceful shutdown
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
    go func() {
        <-sigCh
        state.Unregister(vmID)
        os.Exit(0)
    }()
    
    // ... run VM ...
}
```

**Optional: Unix Socket for Out-of-Band Communication**

For advanced use cases, each VM can expose a Unix socket for direct communication:

```
~/.sandbox/vms/vm-a1b2c3d4/socket
```

This allows:
- Attaching to a running VM from another process
- Sending commands without going through the parent process
- Implementing `sandbox attach vm-a1b2c3d4`

```bash
# Attach to running VM (connects to Unix socket)
sandbox attach vm-a1b2c3d4

# Execute command in running VM
sandbox exec vm-a1b2c3d4 -- ls -la /workspace
```

**Client Library Support:**

```python
from sandbox import Sandbox, list_vms, get_vm, kill_vm

# List all running VMs
for vm in list_vms(status='running'):
    print(f"{vm.id}: {vm.image} (PID {vm.pid})")

# Get VM details
vm = get_vm('vm-a1b2c3d4')
print(vm.config)

# Kill a VM
kill_vm('vm-a1b2c3d4')

# Attach to existing VM (if socket available)
sb = Sandbox.attach('vm-a1b2c3d4')
result = sb.exec('echo hello')
```

```go
import "github.com/example/sandbox/state"

// List VMs
mgr := state.NewStateManager()
vms, _ := mgr.List()
for _, vm := range vms {
    fmt.Printf("%s: %s (PID %d)\n", vm.ID, vm.Image, vm.PID)
}

// Kill VM
mgr.Kill("vm-a1b2c3d4")

// Prune crashed VMs
pruned, _ := mgr.Prune()
fmt.Printf("Pruned %d VMs\n", len(pruned))
```

#### Client Libraries

**Python:**

```python
import subprocess
import json
import base64
import threading

class Sandbox:
    def __init__(self, config=None):
        self.proc = subprocess.Popen(
            ['sandbox', '--rpc'],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            bufsize=1,  # Line buffered
        )
        self._id = 0
        self._lock = threading.Lock()
        self._event_handlers = []
        
        # Start event reader thread
        self._reader_thread = threading.Thread(target=self._read_loop, daemon=True)
        self._pending = {}
        self._reader_thread.start()
        
        # Initialize VM
        self._call('create', config or {})
    
    def _read_loop(self):
        for line in self.proc.stdout:
            msg = json.loads(line)
            if 'id' in msg:
                # Response to a request
                with self._lock:
                    if msg['id'] in self._pending:
                        self._pending[msg['id']].append(msg)
            elif msg.get('method') == 'event':
                # Event notification
                for handler in self._event_handlers:
                    handler(msg['params'])
    
    def _call(self, method, params=None):
        with self._lock:
            self._id += 1
            req_id = self._id
            self._pending[req_id] = []
        
        request = {'jsonrpc': '2.0', 'method': method, 'params': params or {}, 'id': req_id}
        self.proc.stdin.write(json.dumps(request).encode() + b'\n')
        self.proc.stdin.flush()
        
        # Wait for response
        while True:
            with self._lock:
                if self._pending[req_id]:
                    response = self._pending[req_id].pop(0)
                    if 'error' in response:
                        raise Exception(response['error']['message'])
                    return response.get('result')
    
    def exec(self, command, working_dir=None):
        result = self._call('exec', {'command': command, 'working_dir': working_dir})
        return {
            'exit_code': result['exit_code'],
            'stdout': base64.b64decode(result.get('stdout', '')).decode(),
            'stderr': base64.b64decode(result.get('stderr', '')).decode(),
            'duration_ms': result.get('duration_ms', 0),
        }
    
    def write_file(self, path, content):
        if isinstance(content, str):
            content = content.encode()
        self._call('write_file', {
            'path': path,
            'content': base64.b64encode(content).decode(),
        })
    
    def read_file(self, path):
        result = self._call('read_file', {'path': path})
        return base64.b64decode(result['content'])
    
    def list_files(self, path):
        result = self._call('list_files', {'path': path})
        return result['files']
    
    def on_event(self, handler):
        self._event_handlers.append(handler)
    
    def close(self):
        self._call('close')
        self.proc.wait()
    
    def __enter__(self):
        return self
    
    def __exit__(self, *args):
        self.close()


# Usage
with Sandbox({
    'image': 'standard',
    'network': {
        'allowed_hosts': ['api.openai.com'],
        'secrets': {
            'OPENAI_API_KEY': {
                'value': os.environ['OPENAI_API_KEY'],
                'hosts': ['api.openai.com'],
            }
        }
    },
    'vfs': {
        'mounts': {
            '/workspace': {'type': 'memory'}
        }
    },
    'env': {'OPENAI_API_KEY': '${OPENAI_API_KEY}'}
}) as sb:
    
    # Handle events
    sb.on_event(lambda e: print(f"Event: {e['type']}") if e['type'] == 'network' else None)
    
    # Write and execute
    sb.write_file('/workspace/hello.py', 'print("Hello from sandbox!")')
    result = sb.exec('python /workspace/hello.py')
    print(result['stdout'])  # "Hello from sandbox!"
    
    # Read output
    sb.write_file('/workspace/script.py', '''
import json
with open('/workspace/output.json', 'w') as f:
    json.dump({"status": "ok"}, f)
''')
    sb.exec('python /workspace/script.py')
    output = sb.read_file('/workspace/output.json')
    print(output)  # b'{"status": "ok"}'
```

**TypeScript/Node.js:**

```typescript
import { spawn, ChildProcess } from 'child_process';
import * as readline from 'readline';

interface Config {
  image?: string;
  resources?: { cpus?: number; memory_mb?: number; timeout_seconds?: number };
  network?: {
    allowed_hosts?: string[];
    secrets?: Record<string, { value: string; hosts: string[] }>;
  };
  vfs?: {
    mounts?: Record<string, { type: string; host_path?: string; readonly?: boolean }>;
  };
  env?: Record<string, string>;
}

interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  duration_ms: number;
}

type Event = { type: 'network'; network: NetworkEvent } | { type: 'file'; file: FileEvent };
type NetworkEvent = { method: string; url: string; status_code: number; blocked: boolean };
type FileEvent = { op: string; path: string; size: number };

class Sandbox {
  private proc: ChildProcess;
  private rl: readline.Interface;
  private nextId = 0;
  private pending = new Map<number, { resolve: Function; reject: Function }>();
  private eventHandlers: ((event: Event) => void)[] = [];

  private constructor() {}

  static async create(config: Config = {}): Promise<Sandbox> {
    const sb = new Sandbox();
    await sb.init(config);
    return sb;
  }

  private async init(config: Config) {
    this.proc = spawn('sandbox', ['--rpc'], { stdio: ['pipe', 'pipe', 'inherit'] });
    this.rl = readline.createInterface({ input: this.proc.stdout! });

    this.rl.on('line', (line) => {
      const msg = JSON.parse(line);
      if (msg.id !== undefined && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)!;
        this.pending.delete(msg.id);
        if (msg.error) {
          reject(new Error(msg.error.message));
        } else {
          resolve(msg.result);
        }
      } else if (msg.method === 'event') {
        this.eventHandlers.forEach((h) => h(msg.params));
      }
    });

    await this.call('create', config);
  }

  private call<T>(method: string, params?: any): Promise<T> {
    return new Promise((resolve, reject) => {
      const id = ++this.nextId;
      this.pending.set(id, { resolve, reject });
      const request = JSON.stringify({ jsonrpc: '2.0', method, params: params || {}, id });
      this.proc.stdin!.write(request + '\n');
    });
  }

  async exec(command: string, workingDir?: string): Promise<ExecResult> {
    const result = await this.call<any>('exec', { command, working_dir: workingDir });
    return {
      exit_code: result.exit_code,
      stdout: Buffer.from(result.stdout || '', 'base64').toString(),
      stderr: Buffer.from(result.stderr || '', 'base64').toString(),
      duration_ms: result.duration_ms || 0,
    };
  }

  async writeFile(path: string, content: string | Buffer): Promise<void> {
    const data = typeof content === 'string' ? Buffer.from(content) : content;
    await this.call('write_file', { path, content: data.toString('base64') });
  }

  async readFile(path: string): Promise<Buffer> {
    const result = await this.call<{ content: string }>('read_file', { path });
    return Buffer.from(result.content, 'base64');
  }

  async listFiles(path: string): Promise<{ name: string; size: number; is_dir: boolean }[]> {
    const result = await this.call<{ files: any[] }>('list_files', { path });
    return result.files;
  }

  onEvent(handler: (event: Event) => void) {
    this.eventHandlers.push(handler);
  }

  async close(): Promise<void> {
    await this.call('close');
    this.proc.kill();
  }
}

// Usage
async function main() {
  const sb = await Sandbox.create({
    image: 'standard',
    network: {
      allowed_hosts: ['api.openai.com'],
      secrets: {
        OPENAI_API_KEY: { value: process.env.OPENAI_API_KEY!, hosts: ['api.openai.com'] },
      },
    },
    vfs: {
      mounts: { '/workspace': { type: 'memory' } },
    },
    env: { OPENAI_API_KEY: '${OPENAI_API_KEY}' },
  });

  sb.onEvent((event) => {
    if (event.type === 'network') {
      console.log(`HTTP: ${event.network.method} ${event.network.url} -> ${event.network.status_code}`);
    }
  });

  await sb.writeFile('/workspace/hello.py', 'print("Hello from sandbox!")');
  const result = await sb.exec('python /workspace/hello.py');
  console.log(result.stdout); // "Hello from sandbox!"

  await sb.close();
}

main();
```

**Go:**

```go
package sandbox

import (
    "bufio"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "os/exec"
    "sync"
    "sync/atomic"
)

type Sandbox struct {
    cmd     *exec.Cmd
    stdin   io.WriteCloser
    stdout  *bufio.Reader
    nextID  uint64
    pending sync.Map
    events  chan Event
    mu      sync.Mutex
}

type Config struct {
    Image     string            `json:"image,omitempty"`
    Resources *Resources        `json:"resources,omitempty"`
    Network   *NetworkConfig    `json:"network,omitempty"`
    VFS       *VFSConfig        `json:"vfs,omitempty"`
    Env       map[string]string `json:"env,omitempty"`
}

type Resources struct {
    CPUs           int `json:"cpus,omitempty"`
    MemoryMB       int `json:"memory_mb,omitempty"`
    TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type NetworkConfig struct {
    AllowedHosts    []string          `json:"allowed_hosts,omitempty"`
    BlockPrivateIPs bool              `json:"block_private_ips,omitempty"`
    Secrets         map[string]Secret `json:"secrets,omitempty"`
    PolicyScript    string            `json:"policy_script,omitempty"`
}

type Secret struct {
    Value string   `json:"value"`
    Hosts []string `json:"hosts"`
}

type VFSConfig struct {
    DirectMounts map[string]DirectMount `json:"direct_mounts,omitempty"`
    Mounts       map[string]Mount       `json:"mounts,omitempty"`
}

type DirectMount struct {
    HostPath string `json:"host_path"`
    Readonly bool   `json:"readonly,omitempty"`
}

type Mount struct {
    Type     string `json:"type"` // "memory", "real_fs", "overlay"
    HostPath string `json:"host_path,omitempty"`
    Readonly bool   `json:"readonly,omitempty"`
    Upper    *Mount `json:"upper,omitempty"`
    Lower    *Mount `json:"lower,omitempty"`
}

type ExecResult struct {
    ExitCode   int    `json:"exit_code"`
    Stdout     string `json:"stdout"`
    Stderr     string `json:"stderr"`
    DurationMS int64  `json:"duration_ms"`
}

type Event struct {
    Type      string        `json:"type"`
    Timestamp int64         `json:"timestamp"`
    Network   *NetworkEvent `json:"network,omitempty"`
    File      *FileEvent    `json:"file,omitempty"`
}

type NetworkEvent struct {
    Method     string `json:"method"`
    URL        string `json:"url"`
    StatusCode int    `json:"status_code"`
    Blocked    bool   `json:"blocked"`
}

type FileEvent struct {
    Op   string `json:"op"`
    Path string `json:"path"`
    Size int64  `json:"size"`
}

func Create(config *Config) (*Sandbox, error) {
    cmd := exec.Command("sandbox", "--rpc")
    stdin, _ := cmd.StdinPipe()
    stdout, _ := cmd.StdoutPipe()

    if err := cmd.Start(); err != nil {
        return nil, err
    }

    sb := &Sandbox{
        cmd:    cmd,
        stdin:  stdin,
        stdout: bufio.NewReader(stdout),
        events: make(chan Event, 100),
    }

    go sb.readLoop()

    if _, err := sb.call("create", config); err != nil {
        cmd.Process.Kill()
        return nil, err
    }

    return sb, nil
}

func (sb *Sandbox) readLoop() {
    for {
        line, err := sb.stdout.ReadBytes('\n')
        if err != nil {
            close(sb.events)
            return
        }

        var msg struct {
            ID     *uint64         `json:"id"`
            Method string          `json:"method"`
            Result json.RawMessage `json:"result"`
            Error  *struct {
                Message string `json:"message"`
            } `json:"error"`
            Params json.RawMessage `json:"params"`
        }
        json.Unmarshal(line, &msg)

        if msg.ID != nil {
            if ch, ok := sb.pending.Load(*msg.ID); ok {
                ch.(chan json.RawMessage) <- line
            }
        } else if msg.Method == "event" {
            var event Event
            json.Unmarshal(msg.Params, &event)
            select {
            case sb.events <- event:
            default:
            }
        }
    }
}

func (sb *Sandbox) call(method string, params interface{}) (json.RawMessage, error) {
    id := atomic.AddUint64(&sb.nextID, 1)
    ch := make(chan json.RawMessage, 1)
    sb.pending.Store(id, ch)
    defer sb.pending.Delete(id)

    req := map[string]interface{}{
        "jsonrpc": "2.0",
        "method":  method,
        "params":  params,
        "id":      id,
    }

    sb.mu.Lock()
    data, _ := json.Marshal(req)
    sb.stdin.Write(append(data, '\n'))
    sb.mu.Unlock()

    line := <-ch
    var resp struct {
        Result json.RawMessage `json:"result"`
        Error  *struct {
            Message string `json:"message"`
        } `json:"error"`
    }
    json.Unmarshal(line, &resp)

    if resp.Error != nil {
        return nil, fmt.Errorf("%s", resp.Error.Message)
    }
    return resp.Result, nil
}

func (sb *Sandbox) Exec(command string) (*ExecResult, error) {
    result, err := sb.call("exec", map[string]string{"command": command})
    if err != nil {
        return nil, err
    }

    var raw struct {
        ExitCode   int    `json:"exit_code"`
        Stdout     string `json:"stdout"`
        Stderr     string `json:"stderr"`
        DurationMS int64  `json:"duration_ms"`
    }
    json.Unmarshal(result, &raw)

    stdout, _ := base64.StdEncoding.DecodeString(raw.Stdout)
    stderr, _ := base64.StdEncoding.DecodeString(raw.Stderr)

    return &ExecResult{
        ExitCode:   raw.ExitCode,
        Stdout:     string(stdout),
        Stderr:     string(stderr),
        DurationMS: raw.DurationMS,
    }, nil
}

func (sb *Sandbox) WriteFile(path string, content []byte) error {
    _, err := sb.call("write_file", map[string]string{
        "path":    path,
        "content": base64.StdEncoding.EncodeToString(content),
    })
    return err
}

func (sb *Sandbox) ReadFile(path string) ([]byte, error) {
    result, err := sb.call("read_file", map[string]string{"path": path})
    if err != nil {
        return nil, err
    }

    var raw struct {
        Content string `json:"content"`
    }
    json.Unmarshal(result, &raw)

    return base64.StdEncoding.DecodeString(raw.Content)
}

func (sb *Sandbox) Events() <-chan Event {
    return sb.events
}

func (sb *Sandbox) Close() error {
    sb.call("close", nil)
    return sb.cmd.Wait()
}
```

**Usage:**

```go
sb, err := sandbox.Create(&sandbox.Config{
    Image: "standard",
    Network: &sandbox.NetworkConfig{
        AllowedHosts: []string{"api.openai.com"},
        Secrets: map[string]sandbox.Secret{
            "OPENAI_API_KEY": {
                Value: os.Getenv("OPENAI_API_KEY"),
                Hosts: []string{"api.openai.com"},
            },
        },
    },
    VFS: &sandbox.VFSConfig{
        Mounts: map[string]sandbox.Mount{
            "/workspace": {Type: "memory"},
        },
    },
    Env: map[string]string{
        "OPENAI_API_KEY": "${OPENAI_API_KEY}",
    },
})
if err != nil {
    log.Fatal(err)
}
defer sb.Close()

// Handle events in background
go func() {
    for event := range sb.Events() {
        if event.Network != nil {
            log.Printf("HTTP: %s %s -> %d", event.Network.Method, event.Network.URL, event.Network.StatusCode)
        }
    }
}()

// Write and execute
sb.WriteFile("/workspace/hello.py", []byte(`print("Hello from sandbox!")`))
result, _ := sb.Exec("python /workspace/hello.py")
fmt.Println(result.Stdout) // "Hello from sandbox!"
```

#### Go Library (Native, No Subprocess)

For Go applications, a native library is also available that embeds the sandbox directly without subprocess overhead:

```go
import "github.com/example/sandbox/embed"

// Native embedding - no subprocess, direct VM management
vm, err := embed.Create(ctx, &embed.Config{
    Image: "standard",
    Network: embed.NetworkConfig{
        AllowedHosts: []string{"api.openai.com"},
        Hooks: embed.HTTPHooks{
            OnRequest: func(req *http.Request) (*http.Request, error) {
                log.Printf("Request: %s %s", req.Method, req.URL)
                return req, nil
            },
        },
    },
})
```

This provides the same API but with:
- No process spawn overhead
- Direct access to hooks (functions, not just config)
- Better performance for high-frequency operations

**Basic Usage:**

```go
package main

import (
    "context"
    "fmt"
    "github.com/example/sandbox"
)

func main() {
    vm, _ := sandbox.Create(context.Background(), &sandbox.Config{
        Image: "standard",
        Resources: sandbox.Resources{
            CPUs:     2,
            MemoryMB: 1024,
        },
    })
    defer vm.Close()

    result, _ := vm.Exec(context.Background(), "echo hello")
    fmt.Println(result.Stdout)  // "hello\n"
}
```

**Network Policy Configuration:**

```go
vm, _ := sandbox.Create(ctx, &sandbox.Config{
    Network: sandbox.NetworkConfig{
        // Allowlist specific hosts
        AllowedHosts: []string{
            "api.openai.com",
            "api.anthropic.com",
            "*.amazonaws.com",
        },
        
        // Block private/internal IPs (default: true)
        BlockPrivateIPs: true,
        
        // Secrets with host-scoped injection
        Secrets: map[string]sandbox.Secret{
            "OPENAI_API_KEY": {
                Value: os.Getenv("OPENAI_API_KEY"),
                Hosts: []string{"api.openai.com"},
            },
            "AWS_SECRET_KEY": {
                Value: os.Getenv("AWS_SECRET_ACCESS_KEY"),
                Hosts: []string{"*.amazonaws.com"},
            },
        },
    },
})
```

**Custom HTTP Hooks:**

```go
vm, _ := sandbox.Create(ctx, &sandbox.Config{
    Network: sandbox.NetworkConfig{
        AllowedHosts: []string{"*"},  // Allow all, but intercept
        
        Hooks: sandbox.HTTPHooks{
            // Intercept and modify requests
            OnRequest: func(req *sandbox.HTTPRequest) (*sandbox.HTTPRequest, error) {
                // Add custom header to all requests
                req.Headers.Set("X-Sandbox-ID", vm.ID())
                
                // Log requests
                log.Printf("Request: %s %s", req.Method, req.URL)
                
                // Block specific paths
                if strings.Contains(req.URL.Path, "/admin") {
                    return nil, sandbox.ErrBlocked
                }
                
                return req, nil
            },
            
            // Intercept and modify responses
            OnResponse: func(resp *sandbox.HTTPResponse, req *sandbox.HTTPRequest) (*sandbox.HTTPResponse, error) {
                // Redact sensitive data from responses
                if strings.Contains(resp.Headers.Get("Content-Type"), "application/json") {
                    resp.Body = redactSensitiveFields(resp.Body)
                }
                
                return resp, nil
            },
            
            // Custom TLS verification (optional)
            OnTLSVerify: func(host string, certs []*x509.Certificate) error {
                // Custom certificate pinning
                if host == "api.critical-service.com" {
                    return verifyCertPin(certs, expectedPin)
                }
                return nil
            },
        },
    },
})
```

**VFS Provider Configuration:**

```go
vm, _ := sandbox.Create(ctx, &sandbox.Config{
    VFS: sandbox.VFSConfig{
        // Fast direct mounts (virtio-fs)
        DirectMounts: map[string]sandbox.DirectMount{
            "/mnt/datasets": {
                HostPath: "/data/ml-datasets",
                Readonly: true,
            },
        },
        
        // Programmable mounts
        Mounts: map[string]sandbox.Provider{
            // In-memory workspace (ephemeral)
            "/workspace": sandbox.NewMemoryProvider(),
            
            // Read-only config from host
            "/config": sandbox.NewReadonlyProvider(
                sandbox.NewRealFSProvider("/etc/myapp"),
            ),
            
            // Copy-on-write overlay
            "/data": sandbox.NewOverlayProvider(
                sandbox.NewMemoryProvider(),                    // Upper (writes go here)
                sandbox.NewRealFSProvider("/data/base-image"),  // Lower (read-only base)
            ),
        },
        
        // VFS hooks for all operations
        Hooks: sandbox.VFSHooks{
            BeforeOpen: func(path string, flags int) error {
                log.Printf("Opening: %s", path)
                return nil
            },
            AfterWrite: func(path string, n int) {
                log.Printf("Wrote %d bytes to %s", n, path)
            },
        },
    },
})
```

**Custom Provider Implementation:**

```go
// Implement sandbox.Provider interface for custom storage backends
type S3Provider struct {
    bucket string
    client *s3.Client
}

func (p *S3Provider) Stat(path string) (sandbox.FileInfo, error) {
    head, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
        Bucket: &p.bucket,
        Key:    aws.String(path),
    })
    if err != nil {
        return sandbox.FileInfo{}, err
    }
    return sandbox.FileInfo{
        Size:    *head.ContentLength,
        ModTime: *head.LastModified,
        Mode:    0644,
    }, nil
}

func (p *S3Provider) Open(path string, flags int, mode os.FileMode) (sandbox.Handle, error) {
    // Stream from S3
    result, err := p.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: &p.bucket,
        Key:    aws.String(path),
    })
    if err != nil {
        return nil, err
    }
    return &s3Handle{body: result.Body}, nil
}

// ... implement other Provider methods

// Usage:
vm, _ := sandbox.Create(ctx, &sandbox.Config{
    VFS: sandbox.VFSConfig{
        Mounts: map[string]sandbox.Provider{
            "/s3-data": &S3Provider{bucket: "my-bucket", client: s3Client},
        },
    },
})
```

**Starlark Policy Scripts:**

For dynamic policy without recompilation:

```go
vm, _ := sandbox.Create(ctx, &sandbox.Config{
    PolicyScript: `
# Starlark policy script

def on_request(req):
    # Block requests to specific domains
    if "facebook.com" in req.host:
        return None  # Block
    
    # Add auth header for specific hosts
    if req.host == "api.internal.com":
        req.headers["Authorization"] = "Bearer " + secrets["INTERNAL_TOKEN"]
    
    return req

def on_response(resp, req):
    # Log response sizes
    print("Response from %s: %d bytes" % (req.host, len(resp.body)))
    return resp

def is_host_allowed(host):
    allowed = ["api.openai.com", "api.anthropic.com"]
    return host in allowed or host.endswith(".amazonaws.com")
`,
    PolicySecrets: map[string]string{
        "INTERNAL_TOKEN": os.Getenv("INTERNAL_TOKEN"),
    },
})
```

**Building Custom CLI:**

Embed your configuration into a custom binary:

```go
// cmd/my-sandbox/main.go
package main

import (
    "os"
    "github.com/example/sandbox"
    "github.com/example/sandbox/cli"
)

func main() {
    // Pre-configured sandbox with your policies baked in
    config := &sandbox.Config{
        Image: "standard",
        Network: sandbox.NetworkConfig{
            AllowedHosts: []string{"api.openai.com"},
            Secrets: map[string]sandbox.Secret{
                "OPENAI_API_KEY": {
                    Value: os.Getenv("OPENAI_API_KEY"),
                    Hosts: []string{"api.openai.com"},
                },
            },
        },
        VFS: sandbox.VFSConfig{
            Mounts: map[string]sandbox.Provider{
                "/workspace": sandbox.NewMemoryProvider(),
            },
        },
    }
    
    // Run CLI with pre-baked config
    cli.RunWithConfig(config, os.Args[1:])
}
```

```bash
# Build your custom sandbox
go build -o my-sandbox ./cmd/my-sandbox

# Use it
./my-sandbox run python script.py
```

**Event Streaming:**

```go
vm, _ := sandbox.Create(ctx, &sandbox.Config{...})

// Subscribe to VM events
events := vm.Events()
go func() {
    for event := range events {
        switch e := event.(type) {
        case *sandbox.NetworkEvent:
            log.Printf("Network: %s %s -> %d", e.Method, e.URL, e.StatusCode)
        case *sandbox.FileEvent:
            log.Printf("File: %s %s (%d bytes)", e.Op, e.Path, e.Size)
        case *sandbox.ExecEvent:
            log.Printf("Exec: %s (exit=%d)", e.Command, e.ExitCode)
        }
    }
}()
```

**Complete Example - AI Agent Sandbox:**

```go
package main

import (
    "context"
    "log"
    "os"
    
    "github.com/example/sandbox"
)

func main() {
    ctx := context.Background()
    
    vm, err := sandbox.Create(ctx, &sandbox.Config{
        Image: "standard",
        Resources: sandbox.Resources{
            CPUs:     2,
            MemoryMB: 2048,
            Timeout:  5 * time.Minute,
        },
        
        Network: sandbox.NetworkConfig{
            AllowedHosts: []string{
                "api.openai.com",
                "api.anthropic.com",
                "pypi.org",
                "files.pythonhosted.org",
            },
            Secrets: map[string]sandbox.Secret{
                "OPENAI_API_KEY": {
                    Value: os.Getenv("OPENAI_API_KEY"),
                    Hosts: []string{"api.openai.com"},
                },
            },
            Hooks: sandbox.HTTPHooks{
                OnRequest: func(req *sandbox.HTTPRequest) (*sandbox.HTTPRequest, error) {
                    log.Printf("[HTTP] %s %s", req.Method, req.URL)
                    return req, nil
                },
            },
        },
        
        VFS: sandbox.VFSConfig{
            Mounts: map[string]sandbox.Provider{
                "/workspace": sandbox.NewMemoryProvider(),
            },
            Hooks: sandbox.VFSHooks{
                AfterWrite: func(path string, n int) {
                    log.Printf("[FS] Wrote %d bytes to %s", n, path)
                },
            },
        },
        
        // Pass environment to guest (placeholders for secrets)
        Env: map[string]string{
            "OPENAI_API_KEY": sandbox.SecretPlaceholder("OPENAI_API_KEY"),
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    defer vm.Close()
    
    // Copy code into sandbox
    vm.WriteFile("/workspace/agent.py", agentCode)
    
    // Run agent
    result, err := vm.Exec(ctx, "cd /workspace && python agent.py")
    if err != nil {
        log.Fatal(err)
    }
    
    log.Printf("Exit code: %d", result.ExitCode)
    log.Printf("Output: %s", result.Stdout)
    
    // Read results from sandbox
    output, _ := vm.ReadFile("/workspace/output.json")
    log.Printf("Result: %s", output)
}
```

### Directory Structure

```
sandbox/
├── cmd/
│   └── sandbox/            # CLI entrypoint
│       └── main.go
├── pkg/
│   ├── rpc/                # JSON-RPC handler
│   │   ├── handler.go
│   │   └── methods.go
│   ├── state/              # VM state management (list, get, kill, prune)
│   │   └── state.go
│   ├── client/             # Go client library (subprocess wrapper)
│   │   └── client.go
│   ├── embed/              # Native Go embedding (no subprocess)
│   │   └── embed.go
│   ├── api/                # Internal API types
│   │   ├── vm.go           # VM type and methods
│   │   ├── options.go      # Configuration options
│   │   └── hooks.go        # Hook types
│   ├── vm/                 # VM backends
│   │   ├── backend.go      # Backend interface
│   │   ├── darwin/         # macOS (code-hex/vz)
│   │   │   └── backend.go
│   │   └── linux/          # Linux (Firecracker)
│   │       └── backend.go
│   ├── net/                # Network stack
│   │   ├── stack.go        # gVisor stack wrapper
│   │   ├── http.go         # HTTP interception
│   │   ├── tls.go          # TLS MITM
│   │   └── dns.go          # DNS handling
│   ├── policy/             # Policy engine
│   │   ├── engine.go
│   │   ├── secrets.go
│   │   └── starlark.go     # Optional Starlark hooks
│   └── vfs/                # Virtual filesystem
│       ├── provider.go     # Provider interface
│       ├── memory.go
│       ├── realfs.go
│       ├── readonly.go
│       ├── overlay.go
│       ├── router.go
│       └── server.go       # VFS protocol server
├── internal/
│   ├── images/             # Image download and cache management
│   ├── guest/              # Guest image building (CI/release only)
│   └── mitm/               # Certificate generation
├── guest/                  # Guest components (kernel, initramfs, FUSE daemon)
│   ├── kernel/             # Kernel config and build scripts
│   ├── initramfs/          # Minimal init system
│   └── fused/              # FUSE daemon for VFS protocol
└── docs/
    └── adr/
```

### Guest Image Specification

The guest image must be minimal for fast boot times while providing a familiar Linux environment for AI-generated code.

#### Kernel

| Property | Specification |
|----------|---------------|
| Version | Linux 6.x LTS |
| Architecture | arm64 (Apple Silicon, Graviton), amd64 (x86_64) |
| Size target | < 10 MB compressed |
| Required features | virtio-net, virtio-vsock, FUSE, overlayfs, tmpfs |
| Disabled features | Most hardware drivers, debugging, unused filesystems |

**Kernel config highlights:**
```
# Virtio (required for VM communication)
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_VSOCK=y

# Filesystem
CONFIG_FUSE_FS=y
CONFIG_OVERLAY_FS=y
CONFIG_TMPFS=y

# Networking
CONFIG_NET=y
CONFIG_INET=y
CONFIG_IPV6=y

# Disable unnecessary features
CONFIG_SOUND=n
CONFIG_USB=n
CONFIG_WIRELESS=n
CONFIG_BLUETOOTH=n
CONFIG_DEBUG_INFO=n
```

#### Root Filesystem

| Property | Specification |
|----------|---------------|
| Base | Alpine Linux (musl-based, minimal) |
| Size target | < 50 MB compressed |
| Init system | OpenRC or custom minimal init |
| Shell | BusyBox ash + bash (for compatibility) |

**Required packages:**
```
# Core
busybox
bash
coreutils
musl

# Networking
curl
wget
ca-certificates
openssh-client

# Development (commonly needed by agents)
python3
python3-pip
nodejs
npm
git

# Build tools
gcc
musl-dev
make
```

**Optional package sets** (loaded on demand):
```
# Data science
python3-numpy
python3-pandas

# Web development
ruby
go
```

#### Boot Sequence

Target: **< 500ms from VM start to shell ready**

```
1. Kernel loads (< 200ms)
   └── Minimal initramfs, no module loading

2. Init starts (< 100ms)
   └── Mount tmpfs on /tmp, /run
   └── Mount overlayfs for root (COW over readonly base)

3. Network setup (< 50ms)
   └── DHCP via virtio-net (host responds immediately)
   └── Configure DNS resolver

4. VFS mount (< 100ms)
   └── Start fused daemon
   └── Connect to host via virtio-vsock
   └── Mount FUSE filesystem at /data
   └── Bind mounts for configured paths

5. Ready (< 50ms)
   └── Signal ready to host via vsock
   └── Accept commands
```

#### Image Variants

| Variant | Size | Use Case |
|---------|------|----------|
| `minimal` | ~30 MB | Basic shell, curl, Python |
| `standard` | ~80 MB | + Node.js, Git, build tools |
| `full` | ~200 MB | + Data science, multiple languages |

#### Image Distribution

Both kernel and rootfs are downloaded on first run. This keeps the CLI binary small, allows independent updates, and ensures kernel/rootfs versions are tested together.

**Download flow:**
```
$ sandbox run python3 -c "print('hello')"
Downloading sandbox images (first run only)...
  kernel-arm64     [================] 8.2 MB / 8.2 MB
  rootfs-standard  [=========>      ] 45 MB / 82 MB (12s remaining)

Starting VM...
hello
```

**Distribution infrastructure:**

| Component | Format | Hosting |
|-----------|--------|---------|
| Kernel | Compressed vmlinux (gzip) | GitHub Releases + CDN |
| Rootfs | SquashFS or ext4 image | GitHub Releases + CDN |
| Manifest | JSON with versions + checksums | GitHub Releases + CDN |

**Manifest format:**
```json
{
  "version": "1.0.0",
  "artifacts": {
    "kernel-arm64": {
      "url": "https://cdn.example.com/v1.0.0/kernel-arm64.gz",
      "sha256": "abc123...",
      "size": 8500000
    },
    "kernel-amd64": {
      "url": "https://cdn.example.com/v1.0.0/kernel-amd64.gz",
      "sha256": "def456...",
      "size": 9200000
    },
    "rootfs-minimal-arm64": {
      "url": "https://cdn.example.com/v1.0.0/rootfs-minimal-arm64.squashfs",
      "sha256": "...",
      "size": 31000000
    },
    "rootfs-standard-arm64": {
      "url": "https://cdn.example.com/v1.0.0/rootfs-standard-arm64.squashfs",
      "sha256": "...",
      "size": 84000000
    }
  }
}
```

**Local cache structure:**
```
~/.sandbox/
├── cache/
│   ├── kernel-arm64-1.0.0.gz
│   ├── kernel-amd64-1.0.0.gz
│   ├── rootfs-minimal-arm64-1.0.0.squashfs
│   ├── rootfs-standard-arm64-1.0.0.squashfs
│   └── manifest-1.0.0.json
├── config.yaml           # User preferences
└── mitm/
    ├── ca.crt            # Generated MITM CA
    └── ca.key
```

**CLI commands for image management:**
```bash
# Pre-download images (for CI or offline use)
sandbox prefetch --variant=standard

# List cached images
sandbox images list

# Remove old versions
sandbox images prune

# Use custom cache directory
SANDBOX_CACHE_DIR=/opt/sandbox sandbox run ...
```

**Download resilience:**
- Resume interrupted downloads via HTTP Range requests
- Retry with exponential backoff on transient failures
- Verify SHA256 before marking download complete
- Atomic rename to prevent partial cache entries

#### MITM CA Certificate Injection

The host-generated MITM CA certificate is injected into the guest at boot:

```
1. Host generates/loads CA certificate
2. Certificate written to VFS MemoryProvider at /ca-certs/
3. Guest mounts /ca-certs/ via FUSE
4. Bind mount: /ca-certs/ca.crt → /etc/ssl/certs/ca-certificates.crt
5. Guest trusts MITM proxy for all HTTPS connections
```

### Implementation Roadmap

1. **Phase 1**: Core infrastructure
   - Implement VM backend interface and Firecracker backend (Linux)
   - Implement network stack using gVisor tcpip with HTTP/TLS MITM
   - Implement policy engine with secret injection
   - Basic CLI for testing

2. **Phase 2**: Filesystem layer
   - Implement VFS provider interface
   - Build core providers (Memory, RealFS, Readonly, Overlay)
   - Implement VFS protocol over virtio-vsock
   - Guest-side FUSE daemon

3. **Phase 3**: macOS support
   - Implement Darwin backend using code-hex/vz
   - Validate network interception via VZFileHandleNetworkDeviceAttachment
   - Test feature parity with Linux backend

4. **Phase 4**: Production hardening
   - Comprehensive test suite
   - Performance benchmarking and optimization
   - Documentation and examples
   - Single binary releases for macOS (arm64) and Linux (amd64, arm64)

## Consequences

### Positive

| Benefit | Description |
|---------|-------------|
| **Battle-tested network stack** | gVisor's tcpip is used in production at Google scale |
| **Faster boot (macOS)** | Native Virtualization.framework vs QEMU emulation |
| **Faster boot (Linux)** | Firecracker micro-VMs vs full QEMU |
| **Single binary** | No Node.js runtime dependency |
| **Type safety** | Go's static typing catches errors at compile time |
| **Memory efficiency** | Lower overhead than Node.js |
| **Cross-compilation** | Easy to build for multiple platforms from one host |

### Negative

| Drawback | Mitigation |
|----------|------------|
| **Development effort** | Phased implementation, start with Linux |
| **Two VM backends to maintain** | Shared interface, most code is common |
| **gVisor dependency** | Well-maintained, used in production at Google scale |
| **Starlark learning curve** | Simple Python-like syntax, good documentation |

### Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| code-hex/vz API instability | Medium | High | Pin versions, contribute upstream |
| gVisor tcpip edge cases | Low | Medium | Extensive testing, well-documented stack |
| Firecracker feature gaps | Low | Low | Feature is mature, wide adoption |
| VM boot time targets not met | Low | Medium | Profile and optimize critical path |

## Alternatives Considered

### 1. TypeScript/Node.js Implementation

Implement the host controller in TypeScript with a custom userspace TCP/IP stack.

**Rejected because**: 
- Requires Node.js runtime, complicates distribution
- Custom TCP/IP stack is complex and may have edge cases
- No battle-tested network stack equivalent in the JS ecosystem

### 2. Rust Implementation

Build in Rust using rust-vmm components.

**Rejected because**: 
- Less mature macOS virtualization bindings
- gVisor tcpip has no Rust equivalent; would need custom network stack
- Higher development velocity with Go for this use case

### 3. Use Container Runtimes (gVisor/Kata)

Use gVisor or Kata Containers directly instead of custom VM management.

**Rejected because**: 
- gVisor doesn't run on macOS
- Kata Containers requires Linux host
- Neither provides the network interception hooks we need for secret injection

### 4. eBPF-Based Interception (Linux Only)

Use eBPF to intercept network traffic without full userspace stack.

**Considered for future**: Could be added as optimization for Linux path, but doesn't help macOS and adds complexity. Not suitable as primary approach.

### 5. In-Guest Proxy (mitmproxy)

Run a transparent proxy inside the guest VM.

**Rejected because**:
- Guest with root access can bypass or compromise the proxy
- Secrets would exist in guest memory
- Violates the threat model of protecting secrets from untrusted guest code

## References

- [code-hex/vz](https://github.com/Code-Hex/vz) - Go bindings for Apple Virtualization.framework
- [Firecracker](https://firecracker-microvm.github.io/) - AWS micro-VM hypervisor
- [gVisor netstack](https://gvisor.dev/docs/user_guide/networking/) - Userspace TCP/IP stack
- [Apple Virtualization.framework](https://developer.apple.com/documentation/virtualization)
- [Starlark](https://github.com/bazelbuild/starlark) - Python-like configuration language
