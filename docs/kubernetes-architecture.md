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
| vsock UDS communication | `pkg/vm/linux/backend.go` | Host-guest comms are local Unix sockets; not networkable |
| VFS over vsock UDS | `pkg/sandbox/sandbox_linux.go:285-286` | FUSE server binds to `{vsock_path}_{port}` |
| rootfs copy-on-write per VM | `pkg/sandbox/sandbox_linux.go:97-101` | Local disk I/O; rootfs images must be on node |
| Firecracker binary required on host | `pkg/vm/linux/backend.go` | `firecracker` must be in PATH inside the pod |

### 2.2 Design Goals

1. **Preserve the security model**: VM-level isolation, MITM proxy, secret injection, VFS interception all remain unchanged inside the sandbox pod.
2. **Kubernetes-native**: Use CRDs, controllers, and standard K8s patterns. Lean on the K8s scheduler, resource limits, quotas, and pod lifecycle instead of reimplementing them.
3. **Minimal code changes**: Re-use the existing `pkg/sandbox`, `pkg/vm/linux`, `pkg/net`, `pkg/vfs`, and `pkg/rpc` packages as-is where possible.
4. **Horizontal scaling**: Add capacity by adding KVM-enabled nodes.
5. **Multi-tenancy**: Namespace-level isolation, resource quotas, and RBAC.

---

## 3. Architecture Overview

The core idea: **one Pod per Sandbox**. Each Sandbox CR triggers creation of a Kubernetes Pod that runs a single Firecracker VM inside it. The Pod's resource requests/limits map directly to the VM's CPU and memory, so the Kubernetes scheduler handles placement, bin-packing, and resource accounting natively.

```
                                    Kubernetes Cluster
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                                                                          │
 │  ┌─────────────────────┐     ┌──────────────────────────────┐           │
 │  │  matchlock-api       │     │  matchlock-controller        │           │
 │  │  (Deployment)        │     │  (Deployment)                │           │
 │  │                      │     │                              │           │
 │  │  - REST/gRPC API     │     │  - Watches Sandbox CRDs      │           │
 │  │  - AuthN/AuthZ       │     │  - Creates sandbox pods      │           │
 │  │  - SDK gateway       │     │  - Reconciles lifecycle      │           │
 │  └─────────┬────────────┘     └──────────────┬───────────────┘           │
 │            │                                  │                          │
 │            │  Creates Sandbox CRs             │  Creates/deletes Pods    │
 │            ▼                                  ▼                          │
 │  ┌──────────────────────────────────────────────────────────┐           │
 │  │                    Kubernetes API Server                  │           │
 │  │                    (Sandbox CRD + Pod storage)            │           │
 │  └──────────────────────────────────────────────────────────┘           │
 │                                                                          │
 │  KVM-enabled Node Pool                                                   │
 │  ┌─────────────────────────────────────────────────────────────────┐    │
 │  │  Node 1                          Node 2                         │    │
 │  │  ┌───────────────────────────┐  ┌───────────────────────────┐  │    │
 │  │  │ sandbox-pod-a1b2          │  │ sandbox-pod-e5f6          │  │    │
 │  │  │ resources:                │  │ resources:                │  │    │
 │  │  │   cpu: 2, mem: 1Gi       │  │   cpu: 4, mem: 2Gi       │  │    │
 │  │  │                          │  │                          │  │    │
 │  │  │ ┌──────────────────────┐ │  │ ┌──────────────────────┐ │  │    │
 │  │  │ │ matchlock-sandbox    │ │  │ │ matchlock-sandbox    │ │  │    │
 │  │  │ │ (thin Go binary)    │ │  │ │ (thin Go binary)    │ │  │    │
 │  │  │ │                     │ │  │ │                     │ │  │    │
 │  │  │ │ sandbox.New()       │ │  │ │ sandbox.New()       │ │  │    │
 │  │  │ │ sandbox.Start()     │ │  │ │ sandbox.Start()     │ │  │    │
 │  │  │ │ gRPC :50051         │ │  │ │ gRPC :50051         │ │  │    │
 │  │  │ │                     │ │  │ │                     │ │  │    │
 │  │  │ │  ┌──────────────┐  │ │  │ │  ┌──────────────┐  │ │  │    │
 │  │  │ │  │ Firecracker  │  │ │  │ │  │ Firecracker  │  │ │  │    │
 │  │  │ │  │ microVM      │  │ │  │ │  │ microVM      │  │ │  │    │
 │  │  │ │  │ + TAP + MITM │  │ │  │ │  │ + TAP + MITM │  │ │  │    │
 │  │  │ │  │ + VFS/FUSE   │  │ │  │ │  │ + VFS/FUSE   │  │ │  │    │
 │  │  │ │  └──────────────┘  │ │  │ │  └──────────────┘  │ │  │    │
 │  │  │ └────────────────────┘ │  │ └────────────────────┘ │  │    │
 │  │  └───────────────────────┘  └───────────────────────┘  │    │
 │  │                                                          │    │
 │  │  ┌───────────────────────────┐                           │    │
 │  │  │ sandbox-pod-c3d4          │                           │    │
 │  │  │ resources:                │                           │    │
 │  │  │   cpu: 1, mem: 512Mi     │                           │    │
 │  │  │ ...                       │                           │    │
 │  │  └───────────────────────────┘                           │    │
 │  │                                                          │    │
 │  │  ┌───────────────────────────┐  (optional, lightweight)  │    │
 │  │  │ matchlock-image-warmer    │                           │    │
 │  │  │ (DaemonSet)               │                           │    │
 │  │  │ - Pre-pulls OCI images    │                           │    │
 │  │  │ - Populates shared cache  │                           │    │
 │  │  └───────────────────────────┘                           │    │
 │  └─────────────────────────────────────────────────────────┘    │
 └──────────────────────────────────────────────────────────────────┘
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
  phase: Running          # Pending | Creating | Running | Stopping | Stopped | Failed
  podName: sandbox-sb-a1b2c3d4
  nodeName: node-01
  podIP: "10.244.1.15"
  vmID: sb-a1b2c3d4
  startedAt: "2026-02-17T10:00:00Z"
  conditions:
    - type: PodReady
      status: "True"
      lastTransitionTime: "2026-02-17T10:00:01Z"
    - type: VMReady
      status: "True"
      lastTransitionTime: "2026-02-17T10:00:02Z"
```

**Key design decisions:**

- **Secrets via `secretRef`**: Instead of accepting raw secret values in the CRD spec (which would be stored in etcd), secrets reference Kubernetes `Secret` objects. The sandbox pod resolves these at startup. This is analogous to how `matchlock run --secret API_KEY@host` works, but uses K8s-native secret management.
- **Phase lifecycle** mirrors matchlock's existing lifecycle phases (`pkg/lifecycle/`) but drops Kubernetes-redundant states (no `Scheduling` — K8s pod scheduling handles that).
- **Namespace scoping** provides multi-tenant isolation at the Kubernetes level.

### 4.2 matchlock-controller

A Kubernetes controller (single replica Deployment with leader election) that watches `Sandbox` CRDs and manages the corresponding Pods.

**Responsibilities:**

1. **Pod creation**: When a new `Sandbox` CR appears, create a Pod spec with:
   - Resource requests/limits matching the Sandbox's CPU/memory
   - `nodeSelector` for KVM-enabled nodes
   - Sandbox config injected via environment/configmap
   - Appropriate volumes, security context, and tolerations

2. **Status synchronization**: Watch the Pod's status and reflect it back onto the Sandbox CR (phase, podIP, nodeName, conditions).

3. **Timeout enforcement**: If `spec.resources.timeoutSeconds` is set, delete the Pod after timeout.

4. **Cleanup**: When a Sandbox CR is deleted, delete the corresponding Pod (via ownerReference, this is mostly automatic).

5. **Subnet coordination**: Assign a unique subnet octet to each Sandbox before pod creation. Store in `status.subnetOctet`. This avoids per-node SQLite coordination (see section 5.4).

**Implementation:**

```go
// pkg/k8s/controller/sandbox_controller.go

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var sandbox v1alpha1.Sandbox
    if err := r.client.Get(ctx, req.NamespacedName, &sandbox); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    switch sandbox.Status.Phase {
    case "", v1alpha1.PhasePending:
        return r.ensurePod(ctx, &sandbox)
    case v1alpha1.PhaseCreating, v1alpha1.PhaseRunning:
        return r.syncPodStatus(ctx, &sandbox)
    }
    return ctrl.Result{}, nil
}

func (r *SandboxReconciler) ensurePod(ctx context.Context, sb *v1alpha1.Sandbox) (ctrl.Result, error) {
    pod := r.buildSandboxPod(sb)

    // Pod is owned by the Sandbox CR — deleted automatically on CR deletion
    controllerutil.SetControllerReference(sb, pod, r.scheme)

    if err := r.client.Create(ctx, pod); err != nil {
        return ctrl.Result{}, err
    }

    sb.Status.Phase = v1alpha1.PhaseCreating
    sb.Status.PodName = pod.Name
    return ctrl.Result{}, r.client.Status().Update(ctx, sb)
}
```

The controller is simple because it delegates scheduling to Kubernetes and VM management to the sandbox pod. No custom scheduler, no WorkerNode CRD, no capacity tracking.

### 4.3 Sandbox Pod (the thin wrapper)

Each Sandbox gets its own Pod. The pod runs a single container: `matchlock-sandbox`, a thin Go binary that wraps the existing `pkg/sandbox` package.

**Pod lifecycle:**

```
Pod Created (by controller)
    │
    ▼
Container starts matchlock-sandbox binary
    │
    ├─ Reads config from downward API / env / mounted ConfigMap
    ├─ Resolves K8s Secrets for credential injection
    ├─ Calls sandbox.New(ctx, config, opts)
    ├─ Calls sandbox.Start(ctx)
    ├─ Starts gRPC server on :50051 for exec/file ops
    ├─ Updates Sandbox CR status to Running
    │
    ▼
Serves exec/file/port-forward requests via gRPC
    │
    ▼
On SIGTERM (pod deletion) or timeout:
    ├─ Calls sandbox.Close(ctx)
    └─ Exits cleanly
```

**Pod spec (generated by controller):**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: sandbox-sb-a1b2c3d4
  namespace: tenant-acme
  labels:
    matchlock.dev/sandbox: sb-a1b2c3d4
    matchlock.dev/role: sandbox
  ownerReferences:
    - apiVersion: matchlock.dev/v1alpha1
      kind: Sandbox
      name: sb-a1b2c3d4
      uid: ...
spec:
  nodeSelector:
    matchlock.dev/kvm: "true"
  serviceAccountName: matchlock-sandbox
  hostNetwork: true                         # Required for TAP + nftables
  containers:
    - name: sandbox
      image: ghcr.io/jingkaihe/matchlock/sandbox:latest
      securityContext:
        privileged: true                    # Required for /dev/kvm, TAP, nftables
      ports:
        - containerPort: 50051              # gRPC for exec/file ops
          name: grpc
      resources:
        requests:
          cpu: "2"                          # == Sandbox spec.resources.cpus
          memory: "1124Mi"                  # == VM memory + ~100Mi overhead
        limits:
          cpu: "2"
          memory: "1224Mi"
      env:
        - name: SANDBOX_ID
          value: "sb-a1b2c3d4"
        - name: SANDBOX_CONFIG
          value: "<base64-encoded api.Config JSON>"
        - name: SUBNET_OCTET
          value: "105"                      # Pre-assigned by controller
        - name: GRPC_PORT
          value: "50051"
      volumeMounts:
        - name: dev-kvm
          mountPath: /dev/kvm
        - name: image-cache
          mountPath: /var/cache/matchlock
        - name: sandbox-data
          mountPath: /var/lib/matchlock
      readinessProbe:
        grpc:
          port: 50051
        initialDelaySeconds: 2
        periodSeconds: 5
      livenessProbe:
        grpc:
          port: 50051
        initialDelaySeconds: 10
        periodSeconds: 10
  volumes:
    - name: dev-kvm
      hostPath:
        path: /dev/kvm
        type: CharDevice
    - name: image-cache                     # Shared across pods on same node
      hostPath:
        path: /var/cache/matchlock
        type: DirectoryOrCreate
    - name: sandbox-data                    # Per-pod ephemeral storage
      emptyDir:
        sizeLimit: "6Gi"                    # Matches diskSizeMB
  restartPolicy: Never                      # Sandboxes are ephemeral, don't restart
  terminationGracePeriodSeconds: 30
  activeDeadlineSeconds: 600                # == spec.resources.timeoutSeconds
```

**What you get from Kubernetes for free:**

| Concern | K8s mechanism | Replaces |
|---|---|---|
| Scheduling / bin-packing | `resources.requests` + K8s scheduler | Custom scheduler + WorkerNode CRD |
| Resource limits | `resources.limits` + cgroup enforcement | Nothing (was missing before) |
| Timeout | `activeDeadlineSeconds` | Custom timeout logic in controller |
| Quota enforcement | `ResourceQuota` per namespace | Custom quota logic |
| Cleanup on failure | `restartPolicy: Never` + ownerReference | Orphan reconciliation |
| Health checking | `readinessProbe` / `livenessProbe` | Custom health reporting |
| Observability | Pod metrics, logs, events | Custom metrics pipeline |
| Affinity/anti-affinity | Pod affinity rules | Custom scheduling constraints |
| Priority | PriorityClass | Nothing |

**The `matchlock-sandbox` binary:**

```go
// cmd/matchlock-sandbox/main.go

func main() {
    cfg := loadConfigFromEnv()              // SANDBOX_CONFIG env var
    secrets := resolveK8sSecrets(cfg)       // Read K8s Secrets via API
    cfg = mergeSecrets(cfg, secrets)

    octet := os.Getenv("SUBNET_OCTET")     // Pre-assigned by controller

    opts := &sandbox.Options{
        RootfsPath: buildOrCacheRootfs(cfg),
        StateDir:   "/var/lib/matchlock",
        CacheDir:   "/var/cache/matchlock",
        SubnetOctet: octet,                 // Skip allocator, use assigned value
    }

    sb, err := sandbox.New(ctx, cfg, opts)
    handleErr(err)

    err = sb.Start(ctx)
    handleErr(err)

    updateSandboxCRStatus(ctx, "Running")

    // Serve gRPC until SIGTERM or timeout
    grpcServer := newGRPCServer(sb)
    grpcServer.Serve(ctx)

    sb.Close(ctx)
}
```

This is ~100 lines of glue. All the real work happens in the unchanged `pkg/sandbox` package.

### 4.4 matchlock-api

A Deployment (2+ replicas) providing the external API surface. Replaces `matchlock rpc` stdin/stdout for remote access.

**gRPC service:**

```protobuf
service MatchlockService {
  // Lifecycle (creates/reads Sandbox CRs)
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
  rpc GetSandbox(GetSandboxRequest) returns (Sandbox);
  rpc ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse);
  rpc DeleteSandbox(DeleteSandboxRequest) returns (DeleteSandboxResponse);

  // Execution (proxied directly to sandbox pod's gRPC)
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc ExecStream(ExecStreamRequest) returns (stream ExecStreamResponse);

  // File operations (proxied directly to sandbox pod's gRPC)
  rpc WriteFile(WriteFileRequest) returns (WriteFileResponse);
  rpc ReadFile(ReadFileRequest) returns (ReadFileResponse);
  rpc ListFiles(ListFilesRequest) returns (ListFilesResponse);
}
```

**Request routing:**

```
SDK ──gRPC──▶ matchlock-api
                  │
                  ├─ Create/Delete: writes Sandbox CR to K8s API
                  │
                  ├─ Exec/File ops: reads status.podIP from Sandbox CR,
                  │                 proxies gRPC to sandbox-pod:50051
                  │
                  └─ List/Get: reads Sandbox CRs from K8s API
```

The API server is stateless. It resolves which sandbox pod to talk to by reading `status.podIP` from the Sandbox CR. Since pods use `hostNetwork`, the pod is reachable via the node IP + a unique gRPC port (allocated by the controller per-pod to avoid collisions, or via a `hostPort` mapping).

### 4.5 matchlock-image-warmer (optional DaemonSet)

A lightweight, unprivileged DaemonSet that pre-pulls OCI images into the shared `hostPath` cache. This eliminates cold-start image pull latency.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: matchlock-image-warmer
spec:
  template:
    spec:
      nodeSelector:
        matchlock.dev/kvm: "true"
      containers:
        - name: warmer
          image: ghcr.io/jingkaihe/matchlock/image-warmer:latest
          volumeMounts:
            - name: image-cache
              mountPath: /var/cache/matchlock
      volumes:
        - name: image-cache
          hostPath:
            path: /var/cache/matchlock
```

Watches an `ImageCache` CR listing images to keep warm. Uses the existing `pkg/image` builder to pull and prepare ext4 rootfs images.

---

## 5. Key Design Decisions

### 5.1 Pod-per-Sandbox (not DaemonSet)

**Decision**: One Kubernetes Pod per Sandbox, each running a single Firecracker VM.

**Rationale**: This delegates scheduling, resource management, and lifecycle to Kubernetes instead of reimplementing them:

- **Native scheduling**: The K8s scheduler handles node selection, bin-packing, affinity, taints/tolerations, and topology spread. No custom scheduler needed.
- **Native resource limits**: Pod `resources.requests/limits` map to VM CPU/memory. The Firecracker process runs inside the pod's cgroup, so kubelet enforces limits automatically.
- **Native quotas**: Namespace `ResourceQuota` limits how many sandbox pods (and how much CPU/memory) a tenant can consume.
- **Native timeout**: `activeDeadlineSeconds` on the Pod handles sandbox timeout without custom timer logic.
- **Simpler failure model**: Pod dies = sandbox dies. No need to reconcile a multi-VM DaemonSet against orphaned state.
- **Simpler controller**: ~200 lines (create pod, sync status, handle deletion) vs ~1000+ lines (custom scheduler, capacity tracking, WorkerNode CRD, multi-VM manager).

**Overhead analysis**: Pod scheduling adds ~2-3s to sandbox creation (scheduler decision ~100ms, container startup ~1-2s, assuming pre-cached container image). This is acceptable because:
- Matchlock sandboxes typically run for minutes to hours (agent tasks), not sub-second.
- The container image is a static Go binary + Firecracker — small and fast to start.
- Image pre-warming via the DaemonSet eliminates OCI image pull latency for sandbox images.

### 5.2 hostNetwork: true

**Decision**: Sandbox pods use `hostNetwork: true`.

**Rationale**: Matchlock creates TAP devices and nftables rules per VM (`pkg/vm/linux/tap.go`, `pkg/net/nftables.go`). TAP devices exist in the pod's network namespace. The NFTablesNAT masquerade rule (`pkg/net/nftables.go:335-441`) needs to route traffic from the TAP through a real interface to the internet. With `hostNetwork`, the TAP and the host's outbound interface are in the same namespace, so the existing masquerade logic works unchanged.

**Trade-off**: `hostNetwork` means sandbox pods share the host's network namespace. Pods on the same node must use different gRPC ports (or Unix sockets). The controller assigns a unique port per pod via the `GRPC_PORT` env var (range 50051-50205, matching the 155 max subnets per node).

**Alternative considered**: Pod networking with `NET_ADMIN`. TAP devices would be in the pod's network namespace, but outbound masquerade would need to route through the pod's `eth0` (veth). This requires changing `NFTablesNAT` to reference the pod's veth instead of hardcoding outbound interface detection. Feasible but adds complexity to the networking code for marginal benefit, since Firecracker already isolates the guest network.

### 5.3 State Management

**Decision**: The Sandbox CRD status is the source of truth. The pod uses ephemeral local storage (`emptyDir`) for operational state.

| State | Storage | Rationale |
|---|---|---|
| Sandbox lifecycle phase | Sandbox CR `status.phase` | Survives pod restarts; visible to controller/API |
| VM metadata (PID, vsock path) | `emptyDir` in pod | Ephemeral, only needed while pod runs |
| Subnet allocation | Sandbox CR `status.subnetOctet` | Coordinated by controller (see 5.4) |
| Image cache | `hostPath` `/var/cache/matchlock` | Shared across pods on same node |
| Rootfs copy | `emptyDir` in pod | One per sandbox, cleaned up with pod |

No SQLite database is needed in the pod. The `pkg/state` and `pkg/lifecycle` packages are bypassed; the sandbox pod directly calls `sandbox.New()` with pre-computed values (subnet, paths).

### 5.4 Subnet Allocation

**Decision**: The controller assigns subnet octets centrally.

The current `SubnetAllocator` (`pkg/state/subnet.go`) uses a per-host SQLite database and allocates from 192.168.100-254.0/24. With pod-per-sandbox, multiple pods on the same node share the host network namespace and must not collide.

The controller tracks allocated octets per node:

```go
// In controller: map[nodeName]map[octet]sandboxName
// Populated from Sandbox CRs: status.nodeName + status.subnetOctet
```

When creating a pod, the controller:
1. Reads all Sandbox CRs on the target node (after K8s schedules the pod, from pod's `spec.nodeName`)
2. Finds a free octet (100-254)
3. Sets `SUBNET_OCTET` env var on the pod

**Alternative**: The sandbox pod could still use the per-node SQLite allocator via the `hostPath` at `/var/lib/matchlock/subnets/`. This is simpler and requires no controller changes, but risks race conditions between concurrent pod starts. File locking on the SQLite DB mitigates this (SQLite handles concurrent access natively). Either approach works; the SQLite approach requires fewer controller changes.

### 5.5 Secret Handling

**Decision**: Sandbox CRD references K8s Secrets; the sandbox pod resolves them at startup.

```
Sandbox CR spec.network.secrets.ANTHROPIC_API_KEY.secretRef
    → K8s Secret "anthropic-credentials" in namespace "tenant-acme"
    → Sandbox pod reads secret value via K8s API at startup
    → Passes to sandbox.New() as api.Config.Network.Secrets
    → MITM proxy injects in-flight (existing behavior)
```

The secret value never appears in the Sandbox CRD or in the pod spec, only the reference. The sandbox pod's ServiceAccount needs RBAC to read Secrets in its namespace.

### 5.6 Exec/File Proxying

**Decision**: API server proxies exec/file operations directly to the sandbox pod's gRPC endpoint.

```
SDK ──gRPC──▶ matchlock-api ──gRPC──▶ sandbox-pod:50051 ──vsock──▶ guest-agent
```

This is a 1:1 mapping to the existing sandbox methods. The sandbox pod's gRPC server wraps `sandbox.Exec()`, `sandbox.WriteFile()`, `sandbox.ReadFile()`, `sandbox.ListFiles()` directly. Streaming exec uses gRPC bidirectional streaming, mapping to the existing exec relay in `pkg/sandbox/exec_relay.go`.

---

## 6. Networking Deep Dive

### 6.1 Per-VM Network Isolation (unchanged inside the pod)

Each sandbox pod runs one Firecracker VM with its own TAP device and /24 subnet. The network stack is identical to standalone matchlock:

```
    Sandbox Pod (hostNetwork: true)
┌──────────────────────────────────────────────────────────────────────┐
│                                                                      │
│  host eth0 (real NIC)                                                │
│    │                                                                  │
│    │ masquerade (nftables NAT postrouting)                           │
│    │                                                                  │
│  fc-sb-a1b2 (TAP)  ◄── 192.168.105.1/24                            │
│    │                                                                  │
│    │  DNAT :80  → transparent proxy HTTP port                        │
│    │  DNAT :443 → transparent proxy HTTPS port                       │
│    │  catch-all → passthrough port                                    │
│    │                                                                  │
│    │            ┌──────────────────────────────────────┐              │
│    │            │  Transparent Proxy (in-pod)           │              │
│    │            │  - Policy engine evaluation           │              │
│    │            │  - Secret injection via MITM           │              │
│    │            │  - Ephemeral CA per VM                │              │
│    │            └──────────────────────────────────────┘              │
│    │                                                                  │
│    └──────▶  Firecracker VM (192.168.105.2)                          │
│              guest eth0: 192.168.105.2                               │
│              gateway: 192.168.105.1                                  │
│              DNS: 8.8.8.8 (forwarded)                                │
└──────────────────────────────────────────────────────────────────────┘
```

### 6.2 gRPC Port Allocation

With `hostNetwork`, sandbox pods share the host's port space. Each pod needs a unique gRPC port. The controller assigns ports from a per-node pool (e.g., 50051-50205, matching subnet octet range 100-254):

```
grpcPort = 50051 + (subnetOctet - 100)
```

So `SUBNET_OCTET=105` → gRPC port `50056`. The API server reads the assigned port from the Sandbox CR status and connects directly.

### 6.3 API-to-Pod Communication

```
matchlock-api
    │
    │ reads Sandbox CR → status.nodeName, status.grpcPort
    │
    └──gRPC──▶ <nodeName>:<grpcPort>
                    │
                    └── sandbox-pod process
                            │
                            └── sandbox.Exec() → vsock → guest-agent
```

---

## 7. Failure Modes and Recovery

### 7.1 Sandbox Pod Crash

- Pod status transitions to `Failed`
- Controller observes pod status, updates Sandbox CR phase to `Failed`
- `restartPolicy: Never` prevents restart (sandboxes are ephemeral)
- Clients get an error on next gRPC call, create a new Sandbox

### 7.2 Node Failure

- K8s marks all pods on the node as `Unknown` → `Failed` after timeout
- Controller updates Sandbox CRs to `Failed`
- Clients create new Sandboxes (scheduled to healthy nodes automatically)

### 7.3 Controller Restart

No impact. Controller is stateless; reconstructs state from Sandbox CRs and their owned Pods via ownerReferences.

### 7.4 API Server Restart

No impact. Stateless proxy. In-flight gRPC streams interrupted; clients retry.

### 7.5 Sandbox Pod Deletion

- `sandbox.Close()` runs in SIGTERM handler during `terminationGracePeriodSeconds` (30s)
- Cleans up: Firecracker process, TAP device, nftables rules, proxy, VFS
- If grace period exceeded, SIGKILL — TAP and nftables rules leak. Mitigated by:
  - Controller can run a cleanup job per node periodically
  - Node reboot clears all nftables rules and TAP devices

---

## 8. Observability

### 8.1 Metrics (Prometheus)

**Controller metrics:**
- `matchlock_sandboxes_total{namespace, phase}` — gauge of sandbox count by phase
- `matchlock_sandbox_create_duration_seconds` — histogram of time from CR creation to Running
- `matchlock_pod_create_failures_total{reason}` — counter of pod creation failures

**Sandbox pod metrics (per pod, scraped by Prometheus):**
- `matchlock_vm_boot_duration_seconds` — histogram of VM boot time
- `matchlock_exec_duration_seconds` — histogram of exec call latency
- `matchlock_network_requests_total{host, status}` — counter of proxied HTTP requests
- `matchlock_network_blocked_total{host}` — counter of blocked requests

### 8.2 Logging

Structured JSON logging:
- **Controller**: pod create/delete events, lifecycle transitions, subnet allocation
- **Sandbox pod**: VM create/start/stop, exec requests, network events
- Standard `kubectl logs sandbox-sb-a1b2c3d4` works out of the box

### 8.3 Events

Kubernetes Events on Sandbox CRs:

```
Normal   PodCreated    Pod sandbox-sb-a1b2c3d4 created on node-01
Normal   VMStarted     Firecracker VM ready in 215ms
Warning  ExecTimeout   Exec command exceeded timeout
Normal   VMStopped     VM stopped gracefully
```

---

## 9. Codebase Changes

### 9.1 New Packages

| Package | Purpose |
|---|---|
| `pkg/k8s/api/v1alpha1/` | CRD type definitions (Sandbox, ImageCache) |
| `pkg/k8s/controller/` | Sandbox controller (pod creation, status sync) |
| `cmd/matchlock-sandbox/` | Sandbox pod binary (~100 lines of glue) |
| `cmd/matchlock-controller/` | Controller binary entry point |
| `cmd/matchlock-api/` | API server binary entry point |

### 9.2 Existing Package Modifications

| Package | Change | Scope |
|---|---|---|
| `pkg/sandbox/` | Accept pre-computed subnet info in Options, make state dir configurable | Small |
| `pkg/state/` | Accept configurable base directory (already partially exists: `NewSubnetAllocatorWithDir`) | Small |
| `pkg/image/` | Accept configurable cache directory | Small |
| `pkg/vm/linux/` | No changes | None |
| `pkg/net/` | No changes | None |
| `pkg/vfs/` | No changes | None |
| `pkg/policy/` | No changes | None |
| `pkg/rpc/` | No changes | None |

The critical insight: `pkg/sandbox` already encapsulates the full VM lifecycle. The sandbox pod is a thin wrapper that calls the same `sandbox.New()` → `Start()` → `Exec()` → `Close()` sequence the CLI uses. The K8s layer is purely orchestration.

### 9.3 Changes to `pkg/sandbox/`

The current `sandbox_linux.go:New()` hardcodes `state.NewManager()` and `state.NewSubnetAllocator()` which use `~/.matchlock/` paths. These need small adjustments:

```go
// Proposed Options extension:
type Options struct {
    KernelPath   string
    RootfsPath   string
    StateDir     string        // Override state directory (default: ~/.matchlock/)
    CacheDir     string        // Override cache directory (default: ~/.cache/matchlock/)
    SubnetOctet  int           // Pre-assigned subnet octet (skip allocator if set)
}
```

When `SubnetOctet` is set, `sandbox.New()` skips the `SubnetAllocator.Allocate()` call and directly constructs a `SubnetInfo` from the octet. This is a ~10 line change.

---

## 10. Deployment Topology

### 10.1 Minimal (Development/Test)

```
1 node (KVM-enabled, e.g., bare metal or nested virt VM):
  - matchlock-controller (Deployment, 1 replica)
  - matchlock-api (Deployment, 1 replica)
  - matchlock-image-warmer (DaemonSet, 1 pod)
  - sandbox pods created on demand
```

### 10.2 Production

```
Control plane nodes (no KVM needed):
  - matchlock-controller (Deployment, 2 replicas, leader election)
  - matchlock-api (Deployment, 3+ replicas, behind LoadBalancer)

Worker node pool (KVM-enabled, bare metal or nested virt):
  - matchlock-image-warmer (DaemonSet, 1 pod per node)
  - Sandbox pods scheduled on demand by K8s scheduler
  - 5-50+ nodes depending on scale

Shared infrastructure:
  - OCI registry (image source)
  - Prometheus + Grafana (monitoring)
  - K8s Secrets (secret management)
```

### 10.3 Node Requirements

Worker nodes need:
- **KVM support**: Bare metal or VMs with nested virtualization enabled
- **Kernel modules**: `kvm`, `kvm_intel`/`kvm_amd`, `tun` (for TAP), `nf_tables`
- **Node label**: `matchlock.dev/kvm: "true"`
- **Disk**: Local SSD recommended for image cache and rootfs copies

The Firecracker binary, Linux kernel, and guest-init are all bundled in the `matchlock-sandbox` container image — no host-level installation required.

---

## 11. SDK Changes

### 11.1 Go SDK

```go
// Current (local — unchanged):
client, _ := matchlock.NewClient()  // spawns matchlock rpc subprocess

// Kubernetes (new RemoteClient):
client, _ := matchlock.NewRemoteClient("matchlock-api.example.com:443",
    matchlock.WithAPIKey("mlk_..."),
    matchlock.WithNamespace("tenant-acme"),
)

// Same interface from here:
sb, _ := client.CreateSandbox(ctx, &matchlock.Config{
    Image: "alpine:latest",
    ...
})
result, _ := sb.Exec(ctx, "echo hello")
sb.Close(ctx)
```

Both implement the same `Client` interface.

### 11.2 Python SDK

Same pattern: `RemoteClient` class using gRPC (`grpcio`) alongside the existing subprocess client.

---

## 12. Migration Path

### Phase 1: Refactor `pkg/sandbox/` for injectable dependencies
- Add `StateDir`, `CacheDir`, `SubnetOctet` to `Options`
- No behavioral changes; CLI continues to work identically
- Tests with custom directories

### Phase 2: Build `matchlock-sandbox` binary + container image
- Thin Go binary wrapping `sandbox.New/Start/Exec/Close`
- gRPC server for exec/file operations
- Dockerfile: static binary + Firecracker + kernel + guest-init

### Phase 3: Build controller
- Sandbox CRD definition
- Pod creation/deletion logic
- Status synchronization
- Subnet coordination

### Phase 4: Build API server
- gRPC + REST API
- AuthN/AuthZ
- Proxy to sandbox pods

### Phase 5: SDK remote clients
- Go `RemoteClient`
- Python `RemoteClient`

---

## 13. Open Questions

1. **hostNetwork port collisions**: With `hostNetwork`, sandbox pods on the same node need unique gRPC ports. The port-from-octet scheme (50051 + octet - 100) works but is fragile. Alternative: use Unix domain sockets via a shared `hostPath` directory, with the API server connecting to the socket file directly. This avoids port allocation entirely.

2. **Pod networking alternative**: Could we avoid `hostNetwork` entirely? If TAP + nftables work within a pod's network namespace (they do — the TAP is created by the pod process and lives in its namespace), the remaining issue is outbound masquerade. The `NFTablesNAT` masquerade rule just needs the outbound interface name. In pod networking, this would be `eth0` (the veth). A small change to auto-detect the default route interface would make this work. This would be cleaner for port management (each pod gets its own IP + port space) but needs testing.

3. **Persistent workspaces**: Should Sandboxes support PersistentVolumeClaims for the workspace mount? This would allow workspace state to survive across Sandbox recreations.

4. **GPU passthrough**: Firecracker doesn't support GPU passthrough. If needed, a different VM backend (Cloud Hypervisor, QEMU) behind the `Backend` interface would be required.

5. **Image pull optimization**: The image-warmer DaemonSet is optional. For high-churn environments, consider using init containers that pull the OCI image before the sandbox container starts, so the image is guaranteed warm.
