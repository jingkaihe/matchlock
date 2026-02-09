# Release Notes

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
