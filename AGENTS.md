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
- Linux VM backend: Firecracker (`pkg/vm/linux`), preferred when `/dev/kvm` is usable
- QEMU TCG fallback backend (`pkg/vm/qemu`), used when `/dev/kvm` is absent/unusable; selected by `MATCHLOCK_BACKEND` or automatically
- macOS VM backend: Virtualization.framework (`pkg/vm/darwin`)
- Network:
  - Linux: nftables transparent proxy + HTTP/TLS MITM
  - macOS: native NAT or gVisor userspace stack when interception is required
- VFS: pluggable providers in `pkg/vfs`

## Repo Map (High Signal)

- `cmd/matchlock`: CLI
- `cmd/guest-init`: unified in-VM runtime entrypoint (init/agent/fused dispatch)
- `internal/guestruntime/agent`: in-VM exec agent runtime
- `internal/guestruntime/fused`: in-VM FUSE daemon runtime
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

# Linux (usable binary + one-time setup)
mise run build && sudo ./bin/matchlock setup linux
```

Linux sudo rule:
- Use `sudo` only for the one-time `setup linux` / `setup user` commands above.
- Do not run `matchlock run` or `matchlock exec` with `sudo`.
- NEVER EVER run `matchlock` with `sudo`.

## Packaging Notes

- Linux package artifacts are generated with GoReleaser/nfpm via `mise run package:linux`.
- Use `mise run package:linux:snapshot` for local snapshot/test artifacts.
- Starter package config lives in `.goreleaser.yaml` and `packaging/linux/`.
- Package lifecycle scripts must stay machine-safe: capabilities/sysctl/module loading are okay; user-specific `usermod` logic is not.

## Test and Check

```bash
mise run test
mise run test:acceptance
mise run test:coverage
mise run check
mise run check:errx
mise run fmt
mise run package:linux
mise run package:linux:snapshot
```

Testing standard:
- Use `testify/require` and `testify/assert`.
- Use `require` for hard preconditions; `assert` for follow-on checks.

Lint scope:
- `golangci-lint run` (repo-wide) reports pre-existing `unused` findings in
  files unrelated to any current change, so `mise run lint` exits non-zero.
  When a change touches a Go package, prove that package is lint-clean with a
  scoped run, e.g. `golangci-lint run ./internal/guestruntime/agent/...`, and
  require 0 issues there.

FUSE-dependent tests (`internal/guestruntime/fused` `TestFuseMount_*`):
- They self-SKIP when `fusermount3` cannot mount; a skip is not a pass. A
  session with `NoNewPrivs: 1` (e.g. the DSH sandbox) prints
  `fusermount3: mount failed: Operation not permitted` and skips — run the gate
  where FUSE is usable.
- FUSE-capable local recipe (no host privileges needed): compile
  `CGO_ENABLED=0 go test -c -o /tmp/fused.test ./internal/guestruntime/fused/`,
  then run that static binary in `docker run --rm --privileged
  -v /tmp/us006:/out:ro ubuntu:24.04` after installing `fuse3 git`, as a
  NON-root user (`runuser -u <user>`) with `HOME`/`TMPDIR` under `/tmp`.
  `--device /dev/fuse --cap-add SYS_ADMIN` mounts but fails unmount with EACCES;
  `--privileged` is required for the go-fuse `fusermount3 -u` path. The host
  AppArmor `fusermount3` profile only permits mounts under `$HOME`, `/mnt`,
  `/media`, `/run/user/<uid>` and `/tmp`, so keep `TMPDIR=/tmp`.

TAP/network acceptance tests under `NoNewPrivs: 1` (e.g. the DSH Linux sandbox):
- A session with `NoNewPrivs: 1` cannot gain `CAP_NET_ADMIN`, so any acceptance
  test that creates a TAP fails at launch with
  `create TAP device: TUNSETIFF: operation not permitted`
  (e.g. `TestSDKAltPortTCPPolicyDifferential`). `--no-network` acceptance tests
  (e.g. `TestCLIRunInteractivePTYResize`) do not need a TAP and still run.
- `matchlock rm`/`prune` also fail in that session:
  `reconcile nftables rule: netlink receive: operation not permitted`, which
  leaves the stopped VM row in `~/.matchlock/vms/state.db`. There is no
  passwordless sudo to work around it. To close such a stopped VM, call
  `state.NewManager().Remove(id)` directly (bypasses the reconciler), then
  confirm `matchlock list` is empty. Never run matchlock with `sudo`.
- When a TAP test must actually run from a `NoNewPrivs: 1` sandbox, run the
  compiled acceptance binary inside a privileged container that shares the host
  network namespace, e.g.
  `docker run --rm --privileged --network host -v /nix:/nix:ro -v "$HOME:$HOME"
  -v "$PWD:$PWD" -v /tmp/acc:/tmp/acc:ro -e HOME="$HOME"
  -e MATCHLOCK_BACKEND=qemu -e MATCHLOCK_BIN="$PWD/bin/matchlock"
  -e PATH="$HOME/.nix-profile/bin:$PATH" -w "$PWD" node:22-bookworm-slim
  sh -c 'apt-get update -qq && apt-get install -y -qq e2fsprogs ca-certificates &&
  exec /tmp/acc/acceptance.test -test.v -test.parallel 2'`.
  The container needs the host `/nix` store (the Linux `bin/matchlock` is
  dynamically linked against a Nix glibc and QEMU/erofs live there),
  `e2fsprogs` (`mke2fs`/`debugfs`), and `MATCHLOCK_BACKEND=qemu` (Firecracker is
  not installed). It also needs `ca-certificates`: `node:22-bookworm-slim` ships
  no system trust store, so the MITM proxy cannot verify upstream TLS and every
  acceptance test that expects a permitted host to succeed over HTTPS fails with
  `wget: error getting response: Invalid argument` (or `Resource temporarily
  unavailable`). The HTTP `permits` tests still pass, which makes the failure
  look like a policy bug when it is only a missing trust store.
  Run as root, so afterwards `find "$HOME/.matchlock"
  "$HOME/.cache/matchlock" -user root` and `chown`/remove anything the container
  created before trusting host state; `matchlock list` must be empty.
- Under the QEMU TCG fallback an interactive zsh (`zsh -ic ...`) in the Ubuntu
  bedlam image is slow — tens of seconds — because its `/etc/zsh/zshrc` runs a
  `compinit` sweep. Give such an exec a >=120s timeout instead of the usual 30s;
  `TestHomeResolutionCdAndZshRc` uses 180s.

Cross-platform test compilation (darwin/arm64):
- There is no macOS runner on the Linux host; prove the darwin test surface
  compiles with cross-compile checks:
  ```bash
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go list ./...
  for p in $(CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go list ./...); do
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go test -c -o /dev/null "$p" || echo "FAIL $p"
  done
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go test -c -tags acceptance -o /dev/null ./tests/acceptance/
  ```
  Linux-only packages (`cmd/guest-init`, `cmd/vsockprobe`, `cmd/crossconnect`,
  `internal/guestruntime/agent`, `pkg/firecracker`, `pkg/kvm`, `pkg/vm/linux`,
  `pkg/vm/qemu`, `tests/acceptance/probe/ip6probe`) are absent from the darwin
  `go list` output and need no check; `pkg/vm/darwin` is darwin-only.
- `scripts/darwin-crosscompile-gate.sh` (also `mise run check:darwin`) codifies
  this gate: it compiles every darwin package's test binary plus the acceptance
  binary, tolerates only failures whose compiler errors point into
  `Code-Hex/vz`, runs `go vet ./...`, and supports `--self-test` for its
  classification helpers. It exits non-zero on any genuine non-cgo failure.
- `scripts/validate-darwin-altport-contract.py` (with `--self-test`) is the
  executable shape check for the out-of-repo darwin delivery contract
  `/home/kaladin/matchlock-work/matchlock-darwin-altport-contract.json`. It
  asserts the required keys, that the alt-port finding names
  `pkg/net/stack_darwin.go` `handlePassthrough`, that the resize finding names
  `pkg/vsock` `SendMessage` plus `pkg/vm/darwin` `ExecInteractive`, and that the
  coordinator action re-runs the macOS acceptance suite and compares
  `TestSDKAltPortTCPPolicyDifferential` and `TestCLIRunInteractivePTYResize`.
  Run it against the published contract; `--self-test` exercises every
  classifier without the artifact.
- `github.com/Code-Hex/vz/v3` is cgo/Objective-C and needs the macOS SDK, so
  `cmd/matchlock`, `pkg/diagnose`, `pkg/rpc`, `pkg/sandbox` and `pkg/vm/darwin`
  cannot cross-compile test binaries with `CGO_ENABLED=0`. On a real macOS host
  they build with cgo enabled; a cross-compile/vet failure whose only errors
  point into the `Code-Hex/vz/v3` module is expected, not a repo bug.
- macOS caps `sockaddr_un.sun_path` at 104 bytes (Linux: 108). Any cross-platform
  test that binds a Unix socket must create it under a short directory (with
  `t.Cleanup` removal) and assert the path length, never under `t.TempDir()`
  (which resolves under `/var/folders/...` on macOS and embeds the long test
  name). `os.MkdirTemp("", "<short>-*")` alone is not enough when TMPDIR itself
  is long; use `internal/testutil.ShortTempDir(t)` (plus
  `testutil.RequireSockPathFits`) instead of hand-rolling it. See
  `pkg/policy/network_callback_test.go`,
  `pkg/sandbox/exec_relay_test.go` and
  `internal/guestruntime/fused/caller_owner_e2e_test.go`.
- When a `//go:build linux` file defines a symbol that a cross-platform
  (acceptance) test imports, add a `//go:build !linux` sibling exposing the same
  symbol instead of gating the test linux-only. Example:
  `internal/assets/qemu-kernel/assets_other.go` defines `Ensure` returning the
  shared `ErrNotQEMUKernel` sentinel.

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

### Guest agent startup ordering (critical)

The guest-agent must bind the exec service (port `5000`) **synchronously before**
it serves the ready signal (port `5002`). The host's `waitReady` dials `5002` and
returns the instant it accepts, then immediately dials `5000` to run the
entrypoint. If `5000` is not yet bound, the kernel answers the dial with
`ECONNRESET` ("connection reset by peer") and a cold sandbox Launch fails
intermittently. This is a one-shot ordering rule — the exec listener must be in
place before the first ready accept, otherwise the QEMU/Firecracker launch path
is flaky. The regression is locked by `internal/guestruntime/agent/init_order_test.go`.

### Firecracker vsock connection model

- Host-initiated calls use `CONNECT <port>` on base `vsock.sock`.
- Guest-initiated calls use `{uds_path}_{port}` listener sockets.
- Do not mix the two patterns.

### Interactive-exec frame serialization

`pkg/vsock.SendMessage` writes the 5-byte header and the payload with two
separate `conn.Write` calls. That is safe only when a single goroutine writes
the connection. The interactive exec paths (`pkg/vm/darwin/machine.go` and
`pkg/vm/linux/backend.go` `ExecInteractive`) send `MsgTypeStdin`, `MsgTypeResize`
and `MsgTypeSignal` from independent goroutines, so they must use
`pkg/vsock.FrameWriter` (`NewFrameWriter(conn).Send(msgType, data)`): one buffer,
one locked `conn.Write` per frame, `io.ErrShortWrite` on a short write. Create
one `FrameWriter` per dialed session and route the initial `MsgTypeExecTTY`
frame through it too. The QEMU backend predates this with `vsockConn.writeFrame`
(`pkg/vm/qemu/vsockconn.go`); do not replace its semantics.

### macOS networking modes

- Default: native NAT (no interception).
- Interception mode activates when policy/secret features require it (for example `--allow-host`, `--secret`).

### Build VM resource caps

`matchlock build` treats `0` as "all host resources", which is unsafe on large
hosts. Two absolute caps apply before any VM is created
(`cmd/matchlock/cmd_build.go`):

- `--build-cpus`: the default resolves to `min(hostCPUs, api.MaxFirecrackerVCPUs)`
  (`pkg/api/cpu.go`, currently `32`); an explicit request above the limit fails
  fast. `pkg/vm/linux` `Create` independently rejects `vcpus > 32` wrapping
  `ErrInvalidCPUCount`, and the `matchlock.cpus=` boot arg uses the effective
  (capped) count.
- `--build-memory`: the default (and explicit `0`) is capped at
  `defaultMaxBuildMemoryMB` (32 GiB). All host RAM on a very large host makes the
  guest kernel memory init outlast the 30s ready window, which surfaces only as a
  ready-signal timeout.

### VMM readiness and teardown

- `pkg/vm/linux.LinuxMachine.Start` spawns exactly ONE `cmd.Wait` goroutine and
  publishes the result through a memoized `done` channel. `Stop`, `Wait`, `Close`
  and `waitForReady` all observe that channel; never call `cmd.Wait()` twice.
- If the VMM exits before the ready signal, `Start` returns an error wrapping
  `ErrVMNotReady` that includes the VMM log tail (the real `VM config error`),
  not just `ErrVMReadyTimeout`.
- `Close` must reap the VMM **independently of the caller's context** before it
  deletes a persistent `fc-*` TAP: `--graceful-shutdown` defaults to 0, so the
  normal `matchlock run` close context is already expired. `waitForVMMExit`
  provides that bounded wait; skipping it leaks the TAP with `TUNSETIFF: EBUSY`.

### VFS caller ownership

`internal/guestruntime/fused` sends the FUSE caller uid/gid in
`VFSRequest.UID/GID`; `pkg/vfs` server dispatch forwards them via an optional
`withCaller(uid, gid) Provider` hook. Ownership precedence on `RealFSProvider` is
explicit `WithOwner` > caller > upstream `0/0`. Every provider wrapper
(`MountRouter`, `interceptProvider`, `ReadonlyProvider`) must forward
`withCaller` or the caller identity is silently dropped. This is what lets
non-root git trust a FUSE-mounted repo without `safe.directory`.

### QEMU-only acceptance gating

Tests that derive a `qm-*` TAP or need QEMU devices must gate on
`MATCHLOCK_BACKEND=qemu` (see `tests/acceptance/qemu_backend_gate_test.go`,
which has no build tag and is unit-tested by `TestQEMUBackendGate`). On a KVM host
the default Firecracker backend creates `fc-*` TAPs, so a `qm-*` assertion can
never pass.

### Guest swap device isolation

`matchlock run --swap <MB>` attaches a raw swap block device that `guest-init`
(PID 1) enables before the launcher drops `CAP_SYS_ADMIN`. Access to the block
device is DAC-governed, not `CAP_SYS_ADMIN`-governed, so after `swapon` guest-init
pins the node to `root:root` `0600` (`restrictSwapDevice` in
`cmd/guest-init/main.go`); devtmpfs already defaults to `0600 0:0`, so the chmod
is a forward guarantee. A non-root workload gets EACCES; a root workload keeps
`CAP_DAC_OVERRIDE`/`CAP_MKNOD` (only SYS_PTRACE/SYS_ADMIN/SYS_MODULE/SYS_RAWIO/
SYS_BOOT are dropped) and can read the device or recreate the node from
`/sys/block/<dev>/dev` — residual risk, bindable only by a device cgroup/BPF
policy. Do not hide the node; a present 0600 node yields a clear EACCES.

Live acceptance coverage lives in `tests/acceptance/swap_test.go`
(`//go:build acceptance`, `TestSwap*`). It uses `--no-network` because swap is a
block-device feature independent of TAP networking, which also lets it run under
a `NoNewPrivs` harness; it drives a detached `--rm=false` sandbox, reads
`/proc/swaps`, and falls back to `state.NewManager().Remove(id)` when
`matchlock rm` cannot reconcile nftables. The pressure case allocates and touches
384 MB in a 256 MB guest with `python3` (the bedlam image has no stress-ng) and
asserts `/proc/swaps` `used > 0` while the workload survives, and that the same
workload without `--swap` fails.

## JSON-RPC Surface (Current)

- `create`
- `exec`
- `exec_stream`
- `write_file`
- `read_file`
- `list_files`
- `allow_list_add`
- `allow_list_delete`
- `port_forward`
- `cancel`
- `close`

`cancel` should reliably stop in-flight execution via context cancellation and connection teardown.

## Kernel and Images (Minimal)

- Kernel version is pinned in `pkg/kernel/kernel.go` and distributed via GHCR.
- Guest kernel configs live under `guest/kernel/`.
- Image cache/local store lives under `~/.cache/matchlock/images/`.
- **Two kernel artifacts coexist** under `~/.cache/matchlock/kernels/<ver>/`: `kernel` (ELF vmlinux, Firecracker/generic) and `kernel-qemu` (bzImage, QEMU amd64). They are NOT interchangeable — QEMU's `-kernel` requires the bzImage. When a `file://` kernel ref is supplied on the amd64 QEMU backend it must point at a bootable bzImage, or QEMU exits (`qemu exited: exit status 1`); the default (no override) path resolves the vendored `kernel-qemu` automatically.

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
