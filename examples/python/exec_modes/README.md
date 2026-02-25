# Python SDK Exec Modes Example

Demonstrates all three execution styles:

- `exec_stream` for live stdout/stderr streaming
- `exec_pipe` for bidirectional stdin/stdout/stderr without a PTY
- `exec_interactive` for an interactive shell with PTY semantics

## Run

From the repository root:

```bash
uv run examples/python/exec_modes/main.py
```

## Notes

- The interactive section requires a real POSIX TTY.
- If you run in a non-interactive environment, the script still runs stream + pipe and skips interactive mode.
- By default it uses `matchlock`; set `MATCHLOCK_BIN` to override.
- If your PATH `matchlock` is an older release, point `MATCHLOCK_BIN` at your local build (for example `MATCHLOCK_BIN=./bin/matchlock uv run --with-editable ./sdk/python examples/python/exec_modes/main.py`).
