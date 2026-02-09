# Release Notes

## Unreleased

### Bug Fixes

- **`matchlock rm` now errors when VM ID is not found** ([#14](https://github.com/jingkaihe/matchlock/issues/14))
- **Fix 2-3s exit delay and "file already closed" warning on macOS** — `Close(ctx)` now accepts a context so callers control the graceful shutdown budget (By default 0s for CLI and SDK); `SocketPair.Close()` is idempotent to prevent double-close errors ([#13](https://github.com/jingkaihe/matchlock/issues/13))
