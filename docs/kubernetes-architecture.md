# Matchlock on Kubernetes: Architecture Design

## 1. Problem Statement

Matchlock today runs as a single-host CLI. It manages Firecracker microVMs locally, stores state in per-user SQLite databases (`~/.matchlock/`), allocates subnets from a local pool, and communicates with SDKs over stdin/stdout JSON-RPC. This works well for single-machine use, but does not support:

- **Multi-tenant VM scheduling** across a fleet of nodes
- **Elastic scaling** of sandbox capacity
- **Centralized API access** for remote SDK clients
- **High availability** and fault tolerance
- **Resource accounting** and quota enforcement

This document designs a Kubernetes-native architecture that preserves matchlock's core security model (VM isolation, network interception, secret injection, VFS hooks) while enabling multi-node, multi-tenant operation.

---

## 2. Constraints and Requirements

### 2.1 Hard Constraints (from current codebase)

| Constraint | Source | Implication |
|---|---|---|
| Firecracker requires `/dev/kvm` | `pkg/vm/linux/backend.go` | Nodes must expose KVM; pods need device access |
| TAP device creation | `pkg/vm/linux/tap.go` | Pods need `NET_ADMIN` capability |
| nftables rules (DNAT, NAT, forward) | `pkg/net/nftables.go` | Pods need `NET_ADMIN` + `NET_RAW` capabilities |
| Subnet allocation (192.168.X.0/24, X=100-254) | `pkg/state/subnet.go` | Max 155 VMs per allocation domain; needs per-node scoping |
| vsock UDS communication | `pkg/vm/linux/backend.go:217-248` | Host-guest comms are local Unix sockets; not networkable |
| VFS over vsock UDS | `pkg/sandbox/sandbox_linux.go:285-286` | FUSE server binds to `{vsock_path}_{port}` |
| rootfs copy-on-write per VM | `pkg/sandbox/sandbox_linux.go:97-101` | Local disk I/O; rootfs images must be on node |
| Firecracker binary required on host | `pkg/vm/linux/backend.go:122` | `firecracker` must be in PATH on worker nodes |

### 2.2 Design Goals

1. **Preserve the security model**: VM-level isolation, MITM proxy, secret injection, VFS interception all remain unchanged inside the worker pod.
2. **Kubernetes-native**: Use CRDs, controllers, and standard K8s patterns.
3. **Minimal code changes**: Re-use the existing `pkg/sandbox`, `pkg/vm/linux`, `pkg/net`, `pkg/vfs`, and `pkg/rpc` packages as-is where possible. New code is primarily the API layer, controller, and scheduler.
4. **Horizontal scaling**: Add capacity by adding KVM-enabled nodes.
5. **Multi-tenancy**: Namespace-level isolation, resource quotas, and RBAC.

---

## 3. Architecture Overview

```
                                    Kubernetes Cluster
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                                                                          │
 │  ┌─────────────────────┐     ┌──────────────────────────────┐           │
 │  │  matchlock-api       │     │  matchlock-controller        │           │
 │  │  (Deployment)        │     │  (Deployment)                │           │
 │  │                      │     │                              │           │
 │  │  - REST/gRPC API     │────▶│  - Watches Sandbox CRDs      │           │
 │  │  - AuthN/AuthZ       │     │  - Schedules to worker pods  │           │
 │  │  - Admission control │     │  - Reconciles lifecycle      │           │
 │  │  - SDK gateway       │     │  - Garbage collection        │           │
 │  └─────────┬────────────┘     └──────────────┬───────────────┘           │
 │            │                                  │                          │
 │            │  Creates Sandbox CRs             │  Assigns to worker,      │
 │            │                                  │  updates status           │
 │            ▼                                  ▼                          │
 │  ┌──────────────────────────────────────────────────────────┐           │
 │  │                    Kubernetes API Server                  │           │
 │  │                    (Sandbox CRD storage)                  │           │
 │  └──────────────────────────────────────────────────────────┘           │
 │            │                                                             │
 │            │  Worker pods watch for assigned Sandboxes                   │
 │            ▼                                                             │
 │  ┌─────────────────────────────────────────────────────────────────┐    │
 │  │  KVM-enabled Node Pool                                          │    │
 │  │                                                                  │    │
 │  │  ┌──────────────────────┐  ┌──────────────────────┐             │    │
 │  │  │ matchlock-worker pod │  │ matchlock-worker pod  │   ...       │    │
 │  │  │ (DaemonSet)          │  │ (DaemonSet)           │             │    │
 │  │  │                      │  │                       │             │    │
 │  │  │ ┌──────────────────┐ │  │ ┌───────────────────┐ │             │    │
 │  │  │ │ Firecracker VM 1 │ │  │ │ Firecracker VM 1  │ │             │    │
 │  │  │ │ ┌──────────────┐ │ │  │ │                   │ │             │    │
 │  │  │ │ │ guest-init   │ │ │  │ │                   │ │             │    │
 │  │  │ │ │ agent+fused  │ │ │  │ │                   │ │             │    │
 │  │  │ │ └──────────────┘ │ │  │ └───────────────────┘ │             │    │
 │  │  │ ├──────────────────┤ │  │ ┌───────────────────┐ │             │    │
 │  │  │ │ Firecracker VM 2 │ │  │ │ Firecracker VM 2  │ │             │    │
 │  │  │ │                  │ │  │ │                   │ │             │    │
 │  │  │ └──────────────────┘ │  │ └───────────────────┘ │             │    │
 │  │  │                      │  │                       │             │    │
 │  │  │ - MITM proxy         │  │                       │             │    │
 │  │  │ - nftables rules     │  │                       │             │    │
 │  │  │ - VFS/FUSE server    │  │                       │             │    │
 │  │  │ - Image cache        │  │                       │             │    │
 │  │  │ - Local subnet alloc │  │                       │             │    │
 │  │  └──────────────────────┘  └───────────────────────┘             │    │
 │  └─────────────────────────────────────────────────────────────────┘    │
 └──────────────────────────────────────────────────────────────────────────┘
```

---

## 4. Components

### 4.1 Sandbox CRD

The `Sandbox` Custom Resource Definition is the central abstraction. It maps directly to matchlock's existing `api.Config` struct (`pkg/api/config.go`).

```yaml
apiVersion: matchlock.dev/v1alpha1
kind: Sandbox
metadata:
  name: sb-a1b2c3d4
  namespace: tenant-acme
  labels:
    matchlock.dev/owner: "user-123"
spec:
  image: "alpine:latest"
  resources:
    cpus: 2
    memoryMB: 1024
    diskSizeMB: 5120
    timeoutSeconds: 600
  network:
    allowedHosts:
      - "api.anthropic.com"
      - "*.github.com"
    secrets:
      ANTHROPIC_API_KEY:
        secretRef:
          name: anthropic-credentials    # K8s Secret reference
          key: api-key
        hosts:
          - "api.anthropic.com"
    dnsServers:
      - "8.8.8.8"
      - "8.8.4.4"
    blockPrivateIPs: true
  vfs:
    workspace: "/workspace"
    mounts:
      /workspace:
        type: memory
    interception:
      rules:
        - operation: write
          pathPattern: "**/*.env"
          phase: before
          action: block
  privileged: false
  env:
    MY_VAR: "my-value"
status:
  phase: Running          # Pending | Scheduling | Creating | Running | Stopping | Stopped | Failed
  workerNode: node-01
  workerPod: matchlock-worker-node01-xyz
  vmID: vm-a1b2c3d4
  guestIP: "192.168.105.2"
  startedAt: "2026-02-17T10:00:00Z"
  conditions:
    - type: VMReady
      status: "True"
      lastTransitionTime: "2026-02-17T10:00:02Z"
    - type: NetworkReady
      status: "True"
      lastTransitionTime: "2026-02-17T10:00:01Z"
```

**Key design decisions:**

- **Secrets via `secretRef`**: Instead of accepting raw secret values in the CRD spec (which would be stored in etcd), secrets reference Kubernetes `Secret` objects. The worker pod resolves these at VM creation time. This is analogous to how `matchlock run --secret API_KEY@host` works, but uses K8s-native secret management.
- **Phase lifecycle** mirrors matchlock's existing lifecycle phases (`pkg/lifecycle/`) but adds Kubernetes-specific states (`Pending`, `Scheduling`).
- **Namespace scoping** provides multi-tenant isolation at the Kubernetes level.

### 4.2 matchlock-controller

A Kubernetes controller (single replica Deployment with leader election) that watches `Sandbox` CRDs and drives the lifecycle.

**Responsibilities:**

1. **Scheduling**: When a new `Sandbox` CR is created, select a worker node based on:
   - Available capacity (CPU, memory, VM count per node)
   - Node labels/taints (KVM-enabled nodes)
   - Affinity/anti-affinity rules from the Sandbox spec

2. **Assignment**: Set `status.workerNode` and `status.workerPod` on the Sandbox CR. The assigned worker pod picks this up.

3. **Lifecycle reconciliation**: Watch for Sandbox phase transitions. Handle:
   - Timeout enforcement (from `spec.resources.timeoutSeconds`)
   - Failed VM detection (worker pod reports failure via status)
   - Cleanup of orphaned Sandboxes when worker pods are evicted

4. **Garbage collection**: Periodically reconcile Sandboxes against live worker pods. Clean up Sandboxes whose worker pods no longer exist.

5. **Quota enforcement**: Validate against namespace-level `ResourceQuota` for matchlock-specific resources (e.g., `matchlock.dev/sandboxes`, `matchlock.dev/cpus`, `matchlock.dev/memory`).

**Implementation approach:**

```go
// pkg/k8s/controller/sandbox_controller.go

type SandboxReconciler struct {
    client    client.Client
    scheme    *runtime.Scheme
    scheduler *Scheduler
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var sandbox v1alpha1.Sandbox
    if err := r.client.Get(ctx, req.NamespacedName, &sandbox); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    switch sandbox.Status.Phase {
    case "":
        // New Sandbox - schedule it
        return r.handlePending(ctx, &sandbox)
    case v1alpha1.PhasePending:
        return r.handleScheduling(ctx, &sandbox)
    case v1alpha1.PhaseRunning:
        return r.handleRunning(ctx, &sandbox)
    case v1alpha1.PhaseFailed:
        return r.handleFailed(ctx, &sandbox)
    }
    return ctrl.Result{}, nil
}
```

Built with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) / kubebuilder.

### 4.3 matchlock-worker (DaemonSet)

A privileged DaemonSet pod running on each KVM-enabled node. This is where Firecracker VMs actually run.

**Why DaemonSet over per-Sandbox pods:**

Running Firecracker inside a Kubernetes pod requires `/dev/kvm` access, `NET_ADMIN`/`NET_RAW` capabilities, and host networking for TAP devices. Creating a new pod per sandbox would add ~5-10s of pod scheduling overhead on top of matchlock's fast VM boot (~200ms). A DaemonSet that manages multiple VMs per node avoids this overhead and amortizes image caching.

**Pod spec:**

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: matchlock-worker
  namespace: matchlock-system
spec:
  selector:
    matchLabels:
      app: matchlock-worker
  template:
    metadata:
      labels:
        app: matchlock-worker
    spec:
      nodeSelector:
        matchlock.dev/kvm: "true"        # Only KVM-enabled nodes
      serviceAccountName: matchlock-worker
      hostNetwork: true                   # Required for TAP + nftables
      hostPID: false
      containers:
        - name: worker
          image: ghcr.io/jingkaihe/matchlock/worker:latest
          securityContext:
            privileged: true              # Required for /dev/kvm, TAP, nftables
          resources:
            requests:
              cpu: "500m"
              memory: "256Mi"
            limits:
              cpu: "2"
              memory: "1Gi"
          volumeMounts:
            - name: dev-kvm
              mountPath: /dev/kvm
            - name: matchlock-data
              mountPath: /var/lib/matchlock
            - name: image-cache
              mountPath: /var/cache/matchlock
          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
      volumes:
        - name: dev-kvm
          hostPath:
            path: /dev/kvm
            type: CharDevice
        - name: matchlock-data
          hostPath:
            path: /var/lib/matchlock
            type: DirectoryOrCreate
        - name: image-cache
          hostPath:
            path: /var/cache/matchlock
            type: DirectoryOrCreate
```

**Internal architecture of the worker pod:**

```
matchlock-worker pod
┌─────────────────────────────────────────────────────────────────┐
│                                                                  │
│  ┌──────────────────────────────────┐                           │
│  │ CRD Watcher                      │                           │
│  │ - Watches Sandbox CRs assigned   │                           │
│  │   to this pod                    │                           │
│  │ - Drives create/exec/stop        │                           │
│  └───────────────┬──────────────────┘                           │
│                  │                                               │
│  ┌───────────────▼──────────────────┐                           │
│  │ gRPC Server (:50051)             │                           │
│  │ - Exec, WriteFile, ReadFile      │                           │
│  │ - ExecStream (bidirectional)     │                           │
│  │ - PortForward                    │                           │
│  └───────────────┬──────────────────┘                           │
│                  │                                               │
│  ┌───────────────▼──────────────────┐                           │
│  │ Sandbox Manager                   │                           │
│  │ (wraps existing pkg/sandbox)      │                           │
│  │                                   │                           │
│  │ sandbox_id → *sandbox.Sandbox     │                           │
│  │                                   │                           │
│  │ ┌─────────┐ ┌─────────┐         │                           │
│  │ │ VM 1    │ │ VM 2    │  ...     │                           │
│  │ │ FC+TAP  │ │ FC+TAP  │         │                           │
│  │ │ proxy   │ │ proxy   │         │                           │
│  │ │ VFS     │ │ VFS     │         │                           │
│  │ └─────────┘ └─────────┘         │                           │
│  └──────────────────────────────────┘                           │
│                                                                  │
│  ┌──────────────────────────────────┐                           │
│  │ Image Manager                     │                           │
│  │ (wraps existing pkg/image)        │                           │
│  │ - Shared image cache on hostPath  │                           │
│  │ - Pre-pull via ImageCache CRD     │                           │
│  └──────────────────────────────────┘                           │
│                                                                  │
│  ┌──────────────────────────────────┐                           │
│  │ Health Reporter                   │                           │
│  │ - Node capacity (free KVM slots)  │                           │
│  │ - VM count, CPU/mem utilization   │                           │
│  │ - Updates WorkerNode CR status    │                           │
│  └──────────────────────────────────┘                           │
└─────────────────────────────────────────────────────────────────┘
```

The worker pod's primary loop:

1. Watch `Sandbox` CRDs filtered by `status.workerPod == MY_POD_NAME`
2. On new assignment: resolve K8s secrets, call `sandbox.New()` + `sandbox.Start()`
3. Update `Sandbox` CR status to `Running`
4. Serve exec/file operations via gRPC (called by the API server)
5. On delete/stop: call `sandbox.Close()`, update status

### 4.4 matchlock-api

A Deployment (2+ replicas) that provides the external API surface. This replaces the current `matchlock rpc` stdin/stdout model for remote access.

**API design:**

The API translates between HTTP/gRPC and the Kubernetes CRD + worker gRPC backend. Two protocols are supported:

**gRPC (primary, for SDK clients):**

```protobuf
service MatchlockService {
  // Sandbox lifecycle
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
  rpc GetSandbox(GetSandboxRequest) returns (Sandbox);
  rpc ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse);
  rpc DeleteSandbox(DeleteSandboxRequest) returns (DeleteSandboxResponse);

  // Execution (proxied to worker pod)
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc ExecStream(ExecStreamRequest) returns (stream ExecStreamResponse);

  // File operations (proxied to worker pod)
  rpc WriteFile(WriteFileRequest) returns (WriteFileResponse);
  rpc ReadFile(ReadFileRequest) returns (ReadFileResponse);
  rpc ListFiles(ListFilesRequest) returns (ListFilesResponse);

  // Port forwarding
  rpc PortForward(PortForwardRequest) returns (PortForwardResponse);
}
```

**REST (secondary, for dashboards and integrations):**

```
POST   /v1/sandboxes                  → CreateSandbox
GET    /v1/sandboxes                  → ListSandboxes
GET    /v1/sandboxes/{id}             → GetSandbox
DELETE /v1/sandboxes/{id}             → DeleteSandbox
POST   /v1/sandboxes/{id}/exec        → Exec
POST   /v1/sandboxes/{id}/exec-stream → ExecStream (WebSocket)
POST   /v1/sandboxes/{id}/files       → WriteFile
GET    /v1/sandboxes/{id}/files       → ReadFile
GET    /v1/sandboxes/{id}/files/list   → ListFiles
```

**Request routing:**

```
SDK Client ──gRPC──▶ matchlock-api ──K8s API──▶ Sandbox CR (create/delete)
                          │
                          │ (exec/file ops)
                          │
                          ├──gRPC──▶ matchlock-worker pod (node-01)
                          └──gRPC──▶ matchlock-worker pod (node-02)
```

The API server resolves `status.workerPod` from the Sandbox CR, then connects directly to the worker pod's gRPC endpoint for exec/file operations. This avoids double-hop latency.

**Authentication and authorization:**

- **External**: API key or OIDC token validated at the API server
- **Internal**: K8s ServiceAccount tokens for worker-to-API-server communication
- **Multi-tenancy**: API requests are scoped to a K8s namespace. Users can only interact with Sandboxes in namespaces they have RBAC access to.

### 4.5 Image Cache (optional CRD)

```yaml
apiVersion: matchlock.dev/v1alpha1
kind: ImageCache
metadata:
  name: alpine-latest
  namespace: matchlock-system
spec:
  images:
    - "alpine:latest"
    - "ubuntu:22.04"
    - "python:3.12-slim"
  nodeSelector:
    matchlock.dev/kvm: "true"
status:
  cachedNodes:
    - node: node-01
      images:
        - ref: "alpine:latest"
          digest: "sha256:abc..."
          cachedAt: "2026-02-17T08:00:00Z"
```

The worker pod watches `ImageCache` CRs and pre-pulls images to the local `hostPath` cache. This eliminates cold-start image pull latency for commonly used images.

---

## 5. Key Design Decisions

### 5.1 hostNetwork: true

**Decision**: Worker pods use `hostNetwork: true`.

**Rationale**: Matchlock creates TAP devices and nftables rules per VM (`pkg/vm/linux/tap.go`, `pkg/net/nftables.go`). TAP devices are host network namespace objects. The DNAT rules redirect traffic from the TAP interface to the transparent proxy running in the worker pod. With pod networking, the TAP device would exist in the pod's network namespace, but nftables rules need to reference it from the host's perspective for NAT masquerading to work (`pkg/net/nftables.go:335-441`, the `NFTablesNAT` which masquerades traffic from the TAP to the outside). Using `hostNetwork` keeps the current networking stack unchanged.

**Alternative considered**: Using a CNI plugin or network namespace bridging. This would require significant rework of the nftables and proxy code, adding complexity without clear benefit since Firecracker already provides VM-level network isolation.

### 5.2 DaemonSet vs. Pod-per-Sandbox

**Decision**: DaemonSet with multiple VMs per pod.

**Rationale**:
- **Boot latency**: Matchlock boots Firecracker VMs in ~200ms. Pod scheduling adds 5-10s. A DaemonSet amortizes scheduling cost.
- **Image cache sharing**: Multiple VMs on the same node share the image cache (`~/.cache/matchlock/images/`). A pod-per-sandbox model would need a shared PersistentVolume or re-pull images per pod.
- **Subnet allocator**: The current `SubnetAllocator` (`pkg/state/subnet.go`) uses a per-host SQLite database. With a DaemonSet, each node has one allocator managing 155 subnets (192.168.100-254.0/24). This is sufficient for the expected VM density per node.
- **Privileged pod overhead**: Each privileged pod is a security surface. Fewer privileged pods (one per node) is better than one per sandbox.

### 5.3 State Management

**Decision**: Replace local SQLite with Kubernetes CRD status as the source of truth. Keep node-local SQLite for transient operational state only.

**Rationale**: The current `pkg/state/` package stores VM metadata in `~/.matchlock/state.db`. In Kubernetes, the Sandbox CRD status field is the authoritative state. The worker pod's local SQLite (`/var/lib/matchlock/state.db`) is used only for:
- Subnet allocation tracking (node-local concern)
- Lifecycle event logging (for debugging)
- Transient operational data (vsock paths, TAP names)

If a worker pod restarts, it reconciles its local state against the Sandbox CRDs assigned to it, cleaning up orphaned VMs.

### 5.4 Secret Handling

**Decision**: Sandbox CRD references K8s Secrets; worker pod resolves them at VM creation time.

```
Sandbox CR spec.network.secrets.ANTHROPIC_API_KEY.secretRef
    → K8s Secret "anthropic-credentials" in namespace "tenant-acme"
    → Worker pod reads secret value via K8s API
    → Passes to sandbox.New() as api.Config.Network.Secrets
    → MITM proxy injects in-flight (existing behavior)
```

The secret value never appears in the Sandbox CRD, only the reference. The worker pod's ServiceAccount needs RBAC to read Secrets in tenant namespaces. This is scoped via a ClusterRole binding that only permits reading Secrets that are explicitly referenced by a Sandbox CR.

### 5.5 Exec/File Proxying

**Decision**: API server proxies exec/file operations to worker pods via gRPC. The worker pod wraps the existing `sandbox.Exec()`, `sandbox.WriteFile()`, `sandbox.ReadFile()` methods.

```
SDK ──gRPC──▶ matchlock-api ──gRPC──▶ matchlock-worker ──vsock──▶ guest-agent
```

This is a thin wrapper. The worker pod already has the `*sandbox.Sandbox` in memory. The gRPC call translates 1:1 to the existing sandbox methods. Streaming exec uses gRPC bidirectional streaming, which maps to the existing `exec_stream` RPC method in `pkg/rpc/handler.go:354-418`.

---

## 6. Networking Deep Dive

### 6.1 Per-VM Network Isolation (unchanged)

Each Firecracker VM gets its own TAP device and /24 subnet, identical to today:

```
                     Host Network Namespace (= pod with hostNetwork)
┌──────────────────────────────────────────────────────────────────────┐
│                                                                      │
│  eth0 (host NIC)                                                     │
│    │                                                                  │
│    │ masquerade (nftables NAT postrouting)                           │
│    │                                                                  │
│  fc-a1b2c3d4 (TAP)  ◄── 192.168.105.1/24                           │
│    │                                                                  │
│    │  DNAT :80  → transparent proxy HTTP port                        │
│    │  DNAT :443 → transparent proxy HTTPS port                       │
│    │  catch-all → passthrough port                                    │
│    │                                                                  │
│    │            ┌──────────────────────────────────────┐              │
│    │            │  Transparent Proxy (per-VM)           │              │
│    │            │  - Policy engine evaluation           │              │
│    │            │  - Secret injection via MITM           │              │
│    │            │  - Ephemeral CA per VM                │              │
│    │            └──────────────────────────────────────┘              │
│    │                                                                  │
│    └──────▶  Firecracker VM (192.168.105.2)                          │
│              eth0 in guest sees 192.168.105.2                        │
│              gateway: 192.168.105.1                                  │
│              DNS: 8.8.8.8 (forwarded)                                │
│                                                                      │
│  fc-b3c4d5e6 (TAP)  ◄── 192.168.106.1/24   (second VM, same node)  │
│    │                                                                  │
│    └──────▶  Firecracker VM (192.168.106.2)                          │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

### 6.2 Inter-Pod Communication

```
                    ┌──────────────┐
                    │ K8s Service  │
                    │ (headless)   │
                    │ matchlock-   │
                    │ worker       │
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
        ┌─────▼────┐ ┌────▼─────┐ ┌───▼──────┐
        │worker    │ │worker    │ │worker    │
        │node-01   │ │node-02   │ │node-03   │
        │:50051    │ │:50051    │ │:50051    │
        └──────────┘ └──────────┘ └──────────┘
```

Worker pods expose gRPC on a fixed port. The API server connects directly to the specific worker pod (not through the Service) using the pod IP from the Sandbox CR's `status.workerPod`. The headless Service is used for DNS resolution and health checking.

---

## 7. Scheduling

### 7.1 Worker Capacity Reporting

Each worker pod periodically reports its capacity via a `WorkerNode` CRD (or annotations on its own pod):

```yaml
apiVersion: matchlock.dev/v1alpha1
kind: WorkerNode
metadata:
  name: node-01
  namespace: matchlock-system
status:
  capacity:
    sandboxes: 155      # Max subnets (192.168.100-254)
    cpus: 32            # Node CPUs available for VMs
    memoryMB: 65536     # Node memory available for VMs
  allocated:
    sandboxes: 12
    cpus: 18
    memoryMB: 12288
  conditions:
    - type: Ready
      status: "True"
    - type: KVMAvailable
      status: "True"
```

### 7.2 Scheduling Algorithm

The controller uses a simple scoring algorithm:

```
score(node) = (free_cpus / requested_cpus) * 0.4
            + (free_memory / requested_memory) * 0.4
            + (free_sandbox_slots / total_sandbox_slots) * 0.2
```

Nodes are filtered first (KVM available, sufficient resources), then ranked by score. Highest score wins. This is similar to the Kubernetes scheduler's LeastAllocated strategy.

For v1, this is a simple in-controller scheduler. If scheduling becomes a bottleneck, it can be extracted into a separate scheduler extender.

---

## 8. Failure Modes and Recovery

### 8.1 Worker Pod Restart

1. Worker pod starts, queries K8s API for Sandbox CRs assigned to it
2. For each assigned Sandbox: check if the Firecracker process is still running (PID file or process scan)
3. If running: re-attach (re-build in-memory sandbox state from local files + CRD status)
4. If not running: mark Sandbox CR as `Failed`, run cleanup (`sandbox.Close()` equivalent)

### 8.2 Worker Pod Eviction/Node Failure

1. Controller watches worker pod health via Pod conditions
2. If a worker pod disappears, all its assigned Sandboxes transition to `Failed`
3. Sandbox CRDs are not auto-rescheduled (VMs are ephemeral, state is lost)
4. Clients receive an error on their next exec/file call and must create a new Sandbox

### 8.3 Controller Restart

No impact. The controller is stateless; it reconstructs its view from Sandbox CRDs on startup. Leader election ensures only one replica is active.

### 8.4 API Server Restart

No impact. API servers are stateless proxies. In-flight gRPC streams are interrupted; clients retry.

---

## 9. Observability

### 9.1 Metrics (Prometheus)

**Controller metrics:**
- `matchlock_sandboxes_total{namespace, phase}` - gauge of sandbox count by phase
- `matchlock_sandbox_create_duration_seconds` - histogram of time from CR creation to Running
- `matchlock_sandbox_schedule_duration_seconds` - histogram of scheduling latency
- `matchlock_scheduling_failures_total{reason}` - counter of scheduling failures

**Worker metrics:**
- `matchlock_worker_vms_active` - gauge of active Firecracker VMs on this node
- `matchlock_worker_vm_boot_duration_seconds` - histogram of VM boot time
- `matchlock_worker_exec_duration_seconds{sandbox}` - histogram of exec latency
- `matchlock_worker_network_requests_total{sandbox, host, status}` - counter of proxied HTTP requests
- `matchlock_worker_network_blocked_total{sandbox, host}` - counter of blocked requests
- `matchlock_worker_subnet_utilization` - gauge of subnet pool usage

### 9.2 Logging

Structured JSON logging at all layers:
- **Controller**: scheduling decisions, lifecycle transitions
- **Worker**: VM create/start/stop events, exec requests, network interception events
- **Per-VM**: Firecracker console output (from `vm.log`), guest-agent output

### 9.3 Events

Kubernetes Events on Sandbox CRDs for lifecycle transitions:

```
Normal   Scheduled    Sandbox assigned to worker node-01
Normal   VMCreated    Firecracker VM vm-a1b2c3d4 created
Normal   VMStarted    VM ready in 215ms
Warning  ExecTimeout  Exec command exceeded timeout
Normal   VMStopped    VM stopped gracefully
```

---

## 10. Codebase Changes

### 10.1 New Packages

| Package | Purpose |
|---|---|
| `pkg/k8s/api/v1alpha1/` | CRD type definitions (Sandbox, WorkerNode, ImageCache) |
| `pkg/k8s/controller/` | Sandbox controller (reconciler, scheduler) |
| `pkg/k8s/worker/` | Worker daemon (CRD watcher, gRPC server, sandbox manager) |
| `cmd/matchlock-controller/` | Controller binary entry point |
| `cmd/matchlock-worker/` | Worker daemon binary entry point |
| `cmd/matchlock-api/` | API server binary entry point |

### 10.2 Existing Package Modifications

| Package | Change | Scope |
|---|---|---|
| `pkg/sandbox/` | Make state/lifecycle deps injectable (interface) | Minor refactor |
| `pkg/state/` | Accept configurable base directory | Small change |
| `pkg/image/` | Accept configurable cache directory | Small change |
| `pkg/rpc/` | No changes (worker wraps sandbox directly) | None |
| `pkg/vm/linux/` | No changes | None |
| `pkg/net/` | No changes | None |
| `pkg/vfs/` | No changes | None |
| `pkg/policy/` | No changes | None |

The key insight is that the `pkg/sandbox/` package already encapsulates the full lifecycle (VM creation, network setup, VFS, proxy). The Kubernetes layer is purely orchestration on top. The worker pod calls the same `sandbox.New()` → `sandbox.Start()` → `sandbox.Exec()` → `sandbox.Close()` sequence that the CLI does.

### 10.3 Changes to `pkg/sandbox/`

The current `sandbox_linux.go:New()` hardcodes `state.NewManager()` and `state.NewSubnetAllocator()` which use `~/.matchlock/` paths. These need to become injectable:

```go
// Current:
func New(ctx context.Context, config *api.Config, opts *Options) (*Sandbox, error) {
    stateMgr := state.NewManager()           // hardcoded ~/.matchlock/
    subnetAlloc := state.NewSubnetAllocator() // hardcoded ~/.matchlock/subnets/
    ...
}

// Proposed:
type Options struct {
    KernelPath  string
    RootfsPath  string
    StateDir    string // NEW: override state directory (default: ~/.matchlock/)
    CacheDir    string // NEW: override cache directory (default: ~/.cache/matchlock/)
}

func New(ctx context.Context, config *api.Config, opts *Options) (*Sandbox, error) {
    stateMgr := state.NewManagerWithDir(opts.StateDir)
    subnetAlloc := state.NewSubnetAllocatorWithDir(opts.StateDir + "/subnets")
    ...
}
```

This is a small, backward-compatible change. `NewSubnetAllocatorWithDir` already exists in the codebase (`pkg/state/subnet.go:39`).

---

## 11. Deployment Topology

### 11.1 Minimal (Development/Test)

```
1 node (KVM-enabled):
  - matchlock-controller (Deployment, 1 replica)
  - matchlock-api (Deployment, 1 replica)
  - matchlock-worker (DaemonSet, 1 pod)
```

### 11.2 Production

```
Control plane nodes (no KVM needed):
  - matchlock-controller (Deployment, 2 replicas, leader election)
  - matchlock-api (Deployment, 3+ replicas, behind LoadBalancer)

Worker node pool (KVM-enabled, bare metal or nested virt):
  - matchlock-worker (DaemonSet, 1 pod per node)
  - 5-50+ nodes depending on scale

Shared infrastructure:
  - OCI registry (image source)
  - Prometheus + Grafana (monitoring)
  - K8s Secrets (secret management)
```

### 11.3 Node Requirements

Worker nodes need:
- **KVM support**: Bare metal or VMs with nested virtualization enabled
- **Kernel modules**: `kvm`, `kvm_intel`/`kvm_amd`, `tun` (for TAP), `nf_tables`
- **Firecracker binary**: Bundled in the worker container image
- **Linux kernel + guest-init**: Bundled in the worker container image or pulled at startup
- **Disk**: Local SSD for image cache and rootfs copies (fast I/O matters for boot time)

---

## 12. SDK Changes

### 12.1 Go SDK

The existing `pkg/sdk/client.go` launches `matchlock rpc` as a subprocess. For Kubernetes, an alternative client that speaks gRPC to the API server:

```go
// Current (local):
client, _ := matchlock.NewClient()  // spawns matchlock rpc subprocess

// Kubernetes:
client, _ := matchlock.NewRemoteClient("matchlock-api.example.com:443",
    matchlock.WithAPIKey("mlk_..."),
    matchlock.WithNamespace("tenant-acme"),
)

// Same interface from here:
sandbox, _ := client.CreateSandbox(ctx, &matchlock.Config{
    Image: "alpine:latest",
    ...
})
result, _ := sandbox.Exec(ctx, "echo hello")
```

Both clients implement the same `Client` interface. The local client uses stdin/stdout JSON-RPC; the remote client uses gRPC. Application code doesn't change.

### 12.2 Python SDK

Same pattern: a `RemoteClient` class that speaks gRPC (via `grpcio`) alongside the existing subprocess-based client.

---

## 13. Migration Path

### Phase 1: Refactor `pkg/sandbox/` for injectable dependencies
- Make `StateDir`, `CacheDir` configurable via `Options`
- No behavioral changes; CLI continues to work identically
- Add integration tests with custom directories

### Phase 2: Build worker daemon
- `cmd/matchlock-worker/` that runs `sandbox.New/Start/Exec/Close` driven by CRD watches
- gRPC server wrapping sandbox operations
- Health reporting

### Phase 3: Build controller
- Sandbox CRD definitions
- Scheduling logic
- Lifecycle reconciliation

### Phase 4: Build API server
- gRPC + REST API
- AuthN/AuthZ
- SDK proxy layer

### Phase 5: SDK clients
- Go `RemoteClient`
- Python `RemoteClient`
- Documentation

---

## 14. Open Questions

1. **Persistent workspaces**: Should Sandboxes support PersistentVolumeClaims for the workspace mount, allowing workspace state to survive across Sandbox restarts? This would require passing a PVC-backed host path into the VFS provider.

2. **Live migration**: Should Sandboxes support migration between nodes? Firecracker supports snapshot/restore, but the networking state (TAP, nftables, proxy) makes this complex. Initial recommendation: don't support it; treat Sandboxes as ephemeral.

3. **GPU passthrough**: Some AI workloads need GPU access. Firecracker doesn't support GPU passthrough. If needed, this would require a different VM backend (e.g., Cloud Hypervisor or QEMU) behind the existing `Backend` interface.

4. **Multi-cluster**: Should the API server support federating Sandboxes across multiple Kubernetes clusters? This is likely out of scope for v1.

5. **Billing/metering**: Detailed per-sandbox resource usage tracking for chargeback. The metrics pipeline provides the data; the billing integration is application-specific.
