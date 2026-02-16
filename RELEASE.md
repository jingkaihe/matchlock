# Release Notes

## Unreleased

* Added port forward support with `matchlock run -p LOCAL_PORT:REMOTE_PORT ...`
* Updated default working directory semantics for `run`/`exec`: when `--workdir` is not set, Matchlock now uses the image `WORKDIR` first, then falls back to workspace path ([#40](https://github.com/jingkaihe/matchlock/issues/40)).
* Fixed Alpine/musl `git` failures in `/workspace` (`unable to get current working directory`) by returning full stat metadata for FUSE `mkdir`/`create` entries and hardening workspace mount readiness checks (observed on macOS) ([#43](https://github.com/jingkaihe/matchlock/issues/43)).
* Added configurable guest network MTU (CLI `--mtu` and SDK `NetworkMTU`) to mitigate path-MTU/TLS handshake issues on some VM networking paths.
* Refactored guest runtime startup to a unified `guest-init` binary that dispatches init/agent/fused roles, replacing separate guest binaries and simplifying rootfs injection.

## 0.1.19

* Added support for vfs interception [#7](https://github.com/jingkaihe/matchlock/issues/7) 
* Added non-secret environment variable support across CLI and SDKs ([#34](https://github.com/jingkaihe/matchlock/issues/34)): `matchlock run -e/--env`, `--env-file`, persisted visibility via `get`/`inspect`, plus Go/Python SDK create-time `env` support.
* Mount path validation and normalization fixes for [#32](https://github.com/jingkaihe/matchlock/issues/32) and [#33](https://github.com/jingkaihe/matchlock/issues/33) ([#35](https://github.com/jingkaihe/matchlock/pull/35)).
* VM lifecycle revamp: append-only lifecycle records in SQLite, reconciliation flow, and new `gc`/`inspect` commands.
* Metadata migration to SQLite for VM state, subnet allocations, and image metadata.
* Fixed `matchlock list` hang when stale `running` VMs (dead PID) are encountered.
* Breaking change: legacy filesystem-only VM metadata under `~/.matchlock/vms/<id>/` is not auto-backfilled into `state.db`; pre-migration VMs may not appear in `list/kill/rm/prune` until migrated or cleaned up.
  * Quick cleanup after upgrade:
    * `matchlock gc --force` (best-effort host resource cleanup)
    * `matchlock prune` (remove stopped/crashed VMs known to DB)
    * If legacy dirs still remain, remove them manually: `rm -rf ~/.matchlock/vms/<id>`

## 0.1.12

* Added end-to-end context cancellation support across the entire matchlock stack.
* Added init=/init kernel arg for Linux backend and prevent /sbin/init overwrites for systemd compatibility
* Intoroduced standalone `-i` pipe mode to allow stdio based communication with the Agent running inside the sandbox
* Added examples of launching docker container inside the sandbox
* Added examples of launching agent from local ACP client over ACP protocol

## 0.1.11

* Image extraction now uses pure Go instead of shelling out to `tar`**, preserving file ownership (uid/gid), permissions (including setuid/setgid/sticky bits) and symlinks when building ext4 rootfs images. This fixes symlink loop crashes (e.g. Playwright/Chromium images) by replacing symlink directories with real ones during extraction.
* Added example of browser usage using [playwright](https://playwright.dev/) driven by agent running in [mcp code mode](https://blog.cloudflare.com/code-mode/) to cover the use cases of https://github.com/jingkaihe/matchlock/issues/6

## 0.1.10

- **User and entrypoint overrides** — Added `--user` (`-u`) flag to `run` and `exec` to run as a specific user (uid, uid:gid, or username), and `--entrypoint` flag to override the image's ENTRYPOINT. Commands are now composed from OCI image config (ENTRYPOINT + CMD) when no explicit command is given, matching Docker behavior.
- **VFS chmod support** — Added `Chmod` operation across all VFS providers and the guest FUSE daemon, enabling proper file permission management inside sandboxes.
- **Image config improvements** — OCI image config (USER, ENTRYPOINT, CMD, WORKDIR, ENV) is now properly extracted, cached, and merged through both the Go and Python SDKs, with in-guest user resolution via `/etc/passwd`.

## 0.1.9

### Bug Fixes

- **Fix concurrent sandbox launches failing with port conflict** — The transparent proxy (Linux) no longer binds to hardcoded ports 18080/18443/18081. Proxy listeners now use OS-assigned ephemeral ports (port 0), with actual ports read back and passed to nftables rules. This allows multiple matchlock instances to run simultaneously without `bind: address already in use` errors.
- Fixing `matchlock image rm` as per https://github.com/jingkaihe/matchlock/issues/19

## 0.1.8

### Breaking Changes

- **Split `matchlock build` into `build` and `pull`** — `matchlock build` is now exclusively for Dockerfile builds (using BuildKit-in-VM). Image pulling has moved to the new `matchlock pull` command. The `-f` flag now defaults to `Dockerfile`, so `matchlock build -t myapp:latest .` works without specifying `-f`.

### Bug Fixes

- **`matchlock rm` now errors when VM ID is not found** ([#14](https://github.com/jingkaihe/matchlock/issues/14))
- **Fix 2-3s exit delay and "file already closed" warning on macOS** — `Close(ctx)` now accepts a context so callers control the graceful shutdown budget (By default 0s for CLI and SDK); `SocketPair.Close()` is idempotent to prevent double-close errors ([#13](https://github.com/jingkaihe/matchlock/issues/13))
- **`--rm` flag now properly removes VM state on exit** — previously `sb.Close()` only marked the VM as stopped without removing the state directory, causing stale entries in `matchlock list` ([#12](https://github.com/jingkaihe/matchlock/issues/12))
