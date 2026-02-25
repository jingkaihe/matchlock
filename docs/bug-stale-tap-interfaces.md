# Bug: Crashed VMs Leave Stale Tap Interfaces, Breaking Networking

**Platform:** Linux (Firecracker backend)
**Severity:** High
**Status:** Open

## Summary

On Linux (Firecracker backend), when a VM crashes or is killed without proper cleanup, its tap interface and nftables rules are left behind. If a new VM is assigned the same subnet (e.g. `192.168.101.0/24`), the kernel routes return traffic to the **stale** tap interface instead of the active one, silently breaking all networking (DNS, TCP, HTTPS) for the new VM.

## Impact

All outbound traffic from the new VM works (packets leave correctly), but **no responses ever arrive**. This is extremely difficult to diagnose because:
- Outbound packets appear on the external interface with correct masquerade
- DNS responses arrive at the host
- Conntrack shows the connection as tracked with replies seen
- nftables forward/accept rules all look correct
- The only symptom is silent packet loss on the return path

## Root Cause

When multiple tap interfaces share the same subnet, the Linux kernel's routing table has multiple entries:

```
192.168.101.0/24 dev fc-24556ba3 proto kernel scope link src 192.168.101.1 linkdown
192.168.101.0/24 dev fc-07ebb069 proto kernel scope link src 192.168.101.1
```

The stale route (with `linkdown`) takes precedence in some cases, causing the kernel to forward de-masqueraded response packets to the wrong interface. This was confirmed via `nft monitor trace`:

```
trace id abcf2634 ip filter FORWARD packet: iif "ens4" oif "fc-24556ba3" ...
                                                         ^^^^^^^^^^^^^^^^
                                              WRONG — should be fc-07ebb069
```

The packet is accepted by DOCKER-USER (which has accept rules for both tap names), forwarded to the stale tap, and silently dropped because no VM is listening on the other end.

## Artifacts Left Behind by Crashed VMs

1. **Tap interface** — `fc-<vmid>` device stays UP (or in `NO-CARRIER` state) with IP `192.168.101.1/24`
2. **nftables tables** — `matchlock_fc-<vmid>` and `matchlock_nat_fc-<vmid>` persist with DNAT, forward, and masquerade rules
3. **DOCKER-USER rules** — Accept rules for the stale tap name remain in Docker's DOCKER-USER chain (inserted via `ensureDockerUserRules`)
4. **Kernel routes** — `192.168.101.0/24 dev fc-<vmid>` route persists

## Manual Workaround

```bash
# 1. Delete stale tap interfaces
sudo ip link delete fc-<stale-vmid>

# 2. Delete stale nftables tables
sudo nft delete table ip matchlock_fc-<stale-vmid>
sudo nft delete table ip matchlock_nat_fc-<stale-vmid>

# 3. Flush conntrack (force re-establishment of NAT mappings)
sudo conntrack -F

# 4. Clean up DOCKER-USER rules (optional, they're harmless once tap is gone)
```

After deleting the stale tap, the kernel route is automatically removed, and return traffic routes correctly to the active VM's tap.

## Proposed Fix

Several complementary approaches:

### 1. Cleanup on `matchlock kill` / `matchlock rm`

When killing or removing a VM on Linux, explicitly clean up:
- Delete the tap interface (`ip link delete`)
- Delete the nftables tables (`matchlock_fc-<vmid>`, `matchlock_nat_fc-<vmid>`)
- Remove tagged DOCKER-USER rules (already implemented in `cleanupDockerUserRules`)

The `NFTablesRules.Cleanup()` and `NFTablesNAT.Cleanup()` methods exist but may not be called on the kill/rm path. Check `sandbox_linux.go` shutdown flow.

### 2. Startup cleanup of orphaned taps

On `matchlock run`, before creating a new VM:
- List existing `fc-*` tap interfaces
- Cross-reference against known running VMs (from state DB)
- Delete any orphaned taps and their associated nftables tables

### 3. Unique subnets per VM

Instead of reusing `192.168.101.0/24`, assign unique subnets per VM (e.g. `192.168.<N>.0/24` where N is derived from the VM ID). This prevents the routing collision entirely, even if stale taps exist.

This is already partially implemented — the subnet allocator in `sandbox_linux.go` assigns subnets. The issue is that a crashed VM's subnet isn't released, and a new VM may get a different subnet but the stale tap's subnet still interferes if there's any overlap.

### 4. `matchlock list` cleanup hint

When `matchlock list` shows crashed VMs, it could warn about stale network resources and suggest `matchlock rm <vmid>` to clean up.

## Reproduction Steps

1. Start a VM on Linux: `matchlock run --image node:22-bookworm-slim -- sleep infinity`
2. Note the VM ID (e.g. `vm-abc123`) and tap name (`fc-abc123`)
3. Kill the matchlock process (SIGKILL) or crash the VM
4. Verify stale tap exists: `ip link show fc-abc123`
5. Start a new VM that gets the same subnet
6. Try DNS from the new VM — it will silently fail
7. Check `nft monitor trace` — return packets route to the old tap

## Diagnostic Commands

```bash
# List all matchlock tap interfaces
ip link show | grep 'fc-'

# Check for duplicate subnet routes
ip route show | grep '192.168.10'

# List matchlock nftables tables
sudo nft list tables | grep matchlock

# Trace packet flow for DNS responses
sudo nft add table ip trace_table
sudo nft add chain ip trace_table trace_chain '{ type filter hook prerouting priority -350; }'
sudo nft add rule ip trace_table trace_chain udp sport 53 meta nftrace set 1
sudo nft monitor trace
# (cleanup: sudo nft delete table ip trace_table)
```

## Environment

- Linux (Debian 12, x86_64)
- Docker CE with iptables-nft backend
- Firecracker VM backend
- GCP n2-standard-16 with nested virtualization

## Related Code

- `pkg/sandbox/sandbox_linux.go` — VM lifecycle, tap/nftables setup
- `pkg/net/nftables.go` — `NFTablesRules`, `NFTablesNAT`, cleanup methods
- `pkg/net/nftables.go:430` — `ensureDockerUserRules` / `cleanupDockerUserRules`
