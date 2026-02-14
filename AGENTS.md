# Matchlock Agent Guide

Concise contributor guide for working in this repo.

## What Matchlock Is

Matchlock is a Go-based micro-VM sandbox for running AI-generated code with:
- cross-platform VM backends (Linux + macOS/Apple Silicon)
- network interception/policy controls
- secret protection
- host-guest communication over vsock
- JSON-RPC control surface

## Core Stack

- Go 1.25
- Linux VM backend: Firecracker (`pkg/vm/linux`)
- macOS VM backend: Virtualization.framework (`pkg/vm/darwin`)
- Network:
  - Linux: nftables transparent proxy + HTTP/TLS MITM
  - macOS: native NAT or gVisor userspace stack when interception is required
- VFS: pluggable providers in `pkg/vfs`

## Repo Map (High Signal)

- `cmd/matchlock`: CLI
- `cmd/guest-agent`: in-VM exec agent
- `cmd/guest-fused`: in-VM FUSE daemon
- `pkg/sandbox`: sandbox lifecycle + exec relay
- `pkg/image`: image pull/import/build + rootfs prep
- `pkg/net`: interception, MITM, policy plumbing
- `pkg/rpc`: JSON-RPC server
- `pkg/policy`: allowlist + secret replacement
- `pkg/state`: VM/subnet state on host
- `internal/errx`: sentinel error wrapping helpers

## Build and Setup (Must Follow)

Always build with `mise`, not raw `go build`.

```bash
# one-time local tooling install
mise install
```

```bash
# macOS (usable, codesigned binary for usage and acceptance tests)
mise run build

# Linux (usable binary + one-time capability/network setup)
mise run build && sudo ./bin/matchlock setup linux
```

Linux sudo rule:
- Use `sudo` only for the one-time `setup linux` command above.
- Do not run `matchlock run` or `matchlock exec` with `sudo`.
- NEVER EVER run `matchlock` with `sudo`.

## Test and Check

```bash
mise run test
mise run test:acceptance
mise run test:coverage
mise run check
mise run check:errx
mise run fmt
```

Testing standard:
- Use `testify/require` and `testify/assert`.
- Use `require` for hard preconditions; `assert` for follow-on checks.

## Coding Standards (Explicit)

### Error handling

Use sentinel errors per package (`errors.go`) and wrap with `internal/errx`.

- Define sentinels with `errors.New`.
- Wrap underlying errors with `errx.Wrap`.
- Add context with `errx.With`.
- Use `errors.Is` at call sites.
- Avoid direct `%w` `fmt.Errorf` in packages (enforced by `mise run check:errx`).

Example pattern:

```go
var ErrParseReference = errors.New("parse image reference")

if err != nil {
    return errx.Wrap(ErrParseReference, err)
}
```

### CLI/runtime behavior

- Keep host-side behavior cross-platform unless platform-specific behavior is required.
- Preserve parity between Linux/macOS guest-agent exec semantics where feasible.
- Keep cancellation semantics intact (host cancel -> guest process termination).

## Runtime Facts Worth Remembering

### Vsock ports

- `5000`: exec service (host -> guest)
- `5001`: VFS service (guest -> host)
- `5002`: ready signal (host -> guest)

### Firecracker vsock connection model

- Host-initiated calls use `CONNECT <port>` on base `vsock.sock`.
- Guest-initiated calls use `{uds_path}_{port}` listener sockets.
- Do not mix the two patterns.

### macOS networking modes

- Default: native NAT (no interception).
- Interception mode activates when policy/secret features require it (for example `--allow-host`, `--secret`).

## JSON-RPC Surface (Current)

- `create`
- `exec`
- `exec_stream`
- `write_file`
- `read_file`
- `list_files`
- `port_forward`
- `cancel`
- `close`

`cancel` should reliably stop in-flight execution via context cancellation and connection teardown.

## Kernel and Images (Minimal)

- Kernel version is pinned in `pkg/kernel/kernel.go` and distributed via GHCR.
- Guest kernel configs live under `guest/kernel/`.
- Image cache/local store lives under `~/.cache/matchlock/images/`.

## Useful CLI Examples

```bash
matchlock run --image alpine:latest cat /etc/os-release
matchlock run --image alpine:latest -it sh
matchlock run --image alpine:latest --rm=false
matchlock exec <vm-id> echo hello
matchlock list
matchlock kill <vm-id>
matchlock prune
matchlock rpc
```

## Known Constraints

- macOS backend supports Apple Silicon only (not Intel).
- gVisor userspace stack is used on macOS interception path; Linux uses nftables.
- Some subsystems still need deeper tests (see package tests and acceptance coverage).
