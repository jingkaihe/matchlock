# Troubleshooting

Common issues, diagnostics, and recovery procedures for Matchlock.

## Diagnostic Commands

Start with these when something goes wrong:

```bash
# Check matchlock version
matchlock version

# List all sandboxes and their states
matchlock list

# Inspect a specific sandbox
matchlock inspect <vm-id>

# Check lifecycle history (requires sqlite3)
sqlite3 ~/.matchlock/state.db \
  "SELECT vm_id, version, phase, updated_at, last_error FROM vm_lifecycle WHERE vm_id='<vm-id>' ORDER BY version;"

# Check VM metadata
sqlite3 ~/.matchlock/state.db "SELECT id, status, pid FROM vms;"

# Check subnet allocations
sqlite3 ~/.matchlock/state.db "SELECT vm_id, octet, subnet FROM subnet_allocations;"

# Check image metadata
sqlite3 ~/.cache/matchlock/images/metadata.db "SELECT scope, tag, digest, size FROM images;"
```

## Startup Issues

### macOS: "process is not entitled"

The matchlock binary is not codesigned with the Virtualization.framework entitlement.

**Fix:**

```bash
codesign --entitlements matchlock.entitlements -f -s - bin/matchlock
```

Or rebuild with `mise run build`, which handles codesigning automatically.

### Linux: "operation not permitted" creating TAP interface

The binary is missing Linux capabilities. Capabilities are lost on every rebuild.

**Fix:**

```bash
sudo setcap cap_net_admin,cap_net_raw+ep $(which matchlock)
```

Or re-run the setup:

```bash
sudo matchlock setup linux --skip-firecracker --skip-network
```

### Linux: "permission denied" on /dev/kvm

Your user is not in the `kvm` group, or the group change has not taken effect.

**Fix:**

```bash
sudo usermod -aG kvm $USER
# Log out and back in for the change to take effect
```

**Verify:**

```bash
groups | grep kvm
ls -la /dev/kvm
```

### Linux: Firecracker not found

Firecracker is not installed or not in `$PATH`.

**Fix:**

```bash
sudo matchlock setup linux --skip-permissions --skip-network
```

This downloads and installs Firecracker to `/usr/local/bin/`.

### "kernel not found" or download failure

The kernel binary has not been downloaded or is corrupt.

**Fix:**

```bash
# Clear kernel cache and let matchlock re-download
rm -rf ~/.cache/matchlock/kernels
matchlock run --image alpine:latest echo test
```

If network access is restricted, download the kernel manually:

```bash
mkdir -p ~/.cache/matchlock/kernels/6.1.137
# Download the correct kernel binary for your architecture
```

Override with:

```bash
export MATCHLOCK_KERNEL=/path/to/kernel
```

## Image Issues

### Alpine / musl libc: Node.js segfaults

Alpine Linux uses musl libc, which can cause segfaults with some Node.js versions inside Matchlock VMs.

**Fix:** Use Debian-based images instead:

```bash
# Instead of node:22-alpine, use:
matchlock run --image node:22-bookworm-slim ...
```

### Stale image after Docker rebuild

After rebuilding a Docker image and importing it into Matchlock, the old cached layers may still be used.

**Fix:**

```bash
rm -rf ~/.cache/matchlock/images
```

Then re-import:

```bash
docker save myapp:latest | matchlock image import myapp:latest
```

### Image pull fails

If pulling from a private registry, ensure your Docker credentials are configured:

```bash
docker login ghcr.io
# Matchlock uses the Docker credential store
```

### "image not found" for local builds

Images built with `matchlock build` or imported with `matchlock image import` are stored locally. Use `matchlock image ls` to verify the image exists:

```bash
matchlock image ls
```

## Network Issues

### Interception mode: TLS handshake failures

The MITM CA certificate may not be trusted by the application inside the VM. Matchlock automatically sets these environment variables:

- `SSL_CERT_FILE`
- `REQUESTS_CA_BUNDLE`
- `CURL_CA_BUNDLE`
- `NODE_EXTRA_CA_CERTS`

If your application uses a different trust store, you may need to configure it manually.

**Diagnosis:**

```bash
matchlock run --image alpine:latest \
  --allow-host "example.com" \
  -it sh

# Inside the VM:
cat /etc/ssl/certs/matchlock-ca.crt
curl -v https://example.com
```

### Interception mode: MTU / path-MTU issues

Some network paths have MTU issues that manifest as TLS handshakes stalling or large responses being truncated.

**Fix:** Lower the guest MTU:

```bash
matchlock run --image alpine:latest --mtu 1200 --allow-host "example.com" ...
```

### Private IPs blocked

When interception mode is active (`--allow-host` or `--secret`), private IPs are blocked by default.

**Fix:** Use `--allow-private-host` with `--add-host`:

```bash
matchlock run --image alpine:latest \
  --allow-host "my-service" \
  --allow-private-host "192.168.1.100" \
  --add-host "my-service:192.168.1.100" \
  ...
```

### "host not in allowlist" when using secrets

When `--secret` is used, Matchlock automatically adds the secret's bound hosts to the allowlist. However, if the agent calls other hosts, they must be explicitly allowed:

```bash
matchlock run --image python:3.12-slim \
  --secret "API_KEY@api.openai.com" \
  --allow-host "cdn.openai.com" \      # Additional host the agent needs
  ...
```

### No network connectivity in NAT mode

In NAT mode (no `--allow-host`), the guest should have full network access. If connectivity fails:

1. Check that the host has internet access
2. Verify DNS resolution works: `matchlock run --image alpine:latest -- nslookup example.com`
3. Try custom DNS servers: `--dns-servers "1.1.1.1,1.0.0.1"`

### Linux: nftables module not loaded

```bash
sudo modprobe nf_tables
```

Or re-run:

```bash
sudo matchlock setup linux --skip-firecracker --skip-permissions
```

## Lifecycle and State Issues

### `matchlock list` shows stale VMs

If `matchlock list` shows VMs in `running` state that have actually crashed:

```bash
# Try garbage collection first
matchlock gc

# Then prune stopped/crashed VMs
matchlock prune
```

### Leaked host resources (TAP, nftables)

If VMs crash without cleanup, host resources may leak. On Linux, this manifests as leftover TAP interfaces and nftables tables.

**Diagnosis:**

```bash
# Check for leaked TAP interfaces
ip link show | grep fc-

# Check for leaked nftables tables
sudo nft list tables | grep matchlock
```

**Fix:**

```bash
matchlock gc                    # Reconcile all VMs
matchlock gc --force-running    # Include running VMs (use carefully)
```

If `gc` fails, manually clean up:

```bash
# Remove TAP interface
sudo ip link delete fc-<suffix>

# Remove nftables table
sudo nft delete table inet matchlock_<name>
```

### Subnet allocation exhausted

Matchlock allocates /24 subnets from a pool. If many VMs crash without cleanup, the pool can be exhausted.

**Fix:**

```bash
matchlock gc
matchlock prune
```

Or manually clear:

```bash
sqlite3 ~/.matchlock/state.db "DELETE FROM subnet_allocations WHERE vm_id NOT IN (SELECT id FROM vms WHERE status = 'running');"
```

### State DB corruption

If the SQLite state DB is corrupted:

```bash
# Try WAL checkpoint
sqlite3 ~/.matchlock/state.db "PRAGMA wal_checkpoint(TRUNCATE);"

# If that fails, full reset:
matchlock kill --all
matchlock prune
rm -rf ~/.matchlock
rm -rf ~/.cache/matchlock
```

## SDK Issues

### SDK client: "matchlock not found"

The SDK spawns `matchlock rpc` as a subprocess. If the binary is not in `$PATH`:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

Or configure in code:

```go
cfg := sdk.Config{BinaryPath: "/path/to/matchlock"}
client, err := sdk.NewClient(cfg)
```

### VFS write fails with EIO on host_fs mounts

If host filesystem writes return EIO errors, check:

1. The host directory exists and is writable
2. The host filesystem is not full
3. No other process holds a lock on the files

This can also occur if the VFS FUSE daemon in the guest has crashed. Check the VM logs:

```bash
cat ~/.matchlock/vms/<vm-id>/vm.log
```

### Context cancellation not propagating

If `cancel` is not stopping in-flight execution:

1. Verify the context is properly wired through the SDK call chain
2. Check that the guest agent received the cancellation signal (vsock port 5000)
3. The exec relay may need to be checked if using `matchlock exec` from another process

## Performance Issues

### Slow first run

The first run of a new image performs several one-time operations:

1. Kernel download from GHCR
2. Image pull from registry
3. EROFS layer blob creation

Subsequent runs with the same image skip all of these and should boot in under a second.

**Pre-warm:**

```bash
matchlock pull alpine:latest
```

### High memory usage

Each VM consumes the configured memory (`--memory`, default 512 MB) plus host overhead for the network stack and VFS server.

Reduce with:

```bash
matchlock run --image alpine:latest --memory 256 ...
```

### Large overlay snapshots

The default `overlay` mount mode copies host directory contents into memory. For large directories, this can be slow and memory-intensive.

**Fix:** Use `host_fs` mode for large directories:

```bash
matchlock run --image alpine:latest -v ./large-dir:data:host_fs ...
```

## Full Reset

When all else fails, perform a complete reset:

```bash
matchlock kill --all
matchlock prune
rm -rf ~/.matchlock
rm -rf ~/.cache/matchlock
```

This removes all VMs, state, cached kernels, and cached images. The next run will re-download everything.
