# Working on Matchlock

Matchlock runs code in microVMs: Firecracker on Linux/KVM, Virtualization.framework on macOS/Apple Silicon. The host and guest runtime are Go; SDKs are Go, Python, and TypeScript.

## Where to look

- `cmd/matchlock/`: CLI; `pkg/sandbox/`: orchestration; `pkg/vm/{linux,darwin}/`: backends.
- `cmd/guest-init/`, `internal/guestruntime/`: guest runtime; `pkg/vsock/`: host/guest transport.
- `pkg/net/`, `pkg/policy/`, `internal/mitm/`: networking, policy, and secret injection.
- `pkg/api/`, `pkg/rpc/`: shared configuration and JSON-RPC; `pkg/sdk/`, `sdk/{python,typescript}/`: SDKs.
- `pkg/image/`, `pkg/kernel/`: images and kernels; `guest/kernel/`: kernel configs.
- `docs/`, `adrs/`: behavior and design decisions; `tests/acceptance/`: real-VM tests.

## Build and verify

`mise.toml` defines tooling and tasks; `.github/workflows/` defines CI coverage. Install tools with `mise install` when needed. Use `mise run build` for runnable binaries, not bare `go build`: it builds the guest runtime too and codesigns the host binary on macOS.

Choose checks for the changed surface; start with the affected package or test.

```bash
mise exec -- go test ./path/to/package -run TestName # focused Go test; substitute package/test
mise run test # Go suite
mise run vet && mise run lint && mise run check:errx # Go static checks
mise run check:python-sdk # Python SDK
npm ci && npm run typecheck && npm test && npm run build # from sdk/typescript/
mise run build && mise run test:acceptance # real-VM acceptance
```

Acceptance tests require working virtualization and host setup; the acceptance task does **not** rebuild binaries. On Linux, use `sudo ./bin/matchlock setup linux` when host setup is needed; run normal sandbox commands and tests **without sudo**.

Format touched Go files with `gofmt`. `mise run check` is broader: it formats the entire repository and checks Go/Python, but does **not** cover TypeScript or acceptance tests. Report checks actually run and any platform coverage gaps.

## Constraints worth preserving

- Real secrets stay on the host; guests receive placeholders, with credentials injected into allowed outbound requests. Do not expose them in guest state or logs.
- No host-directory sharing or filesystem interception. File transfer uses guest APIs/streams; persistence uses attached block volumes.
- Keep Linux/macOS behavior aligned. Host cancellation must terminate guest work, not merely stop waiting for it.
- When changing shared configuration or RPC contracts, check all three SDKs and their tests for corresponding changes.
- Go errors use package sentinels (`errors.go`) and `internal/errx.Wrap` / `With`, preserving `errors.Is`. Follow nearby tests' `testify/require` and `assert` style.
- Linux package hooks must not perform user-specific setup or group enrollment; leave that to explicit admin commands. See `docs/linux-packaging.md`.

Keep this file short: repository-specific constraints and non-obvious workflow details belong here; usage examples and detailed architecture belong in docs.
