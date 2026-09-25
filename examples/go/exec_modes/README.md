# Go SDK Execution Modes Example

This example demonstrates pipe-mode execution as a per-command non-root user
with `ExecPipeWithOptions`, then starts a real interactive shell using
`ExecInteractive`, attached directly to your local terminal.

It configures:

- pipe-mode stdin/stdout/stderr, working directory, and user options
- raw terminal mode (so keystrokes are passed through correctly)
- initial terminal rows/cols
- `SIGWINCH` forwarding so resize events reach the guest PTY

## Run

From the repository root:

```bash
go run ./examples/go/exec_modes
```

The example uses `./bin/matchlock` by default.
To override the binary path, set:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

## What To Expect

- A shell prompt from inside the sandbox (`sh`)
- Interactive behavior similar to `matchlock run -it sh`
- Exit by typing `exit` or pressing `Ctrl-D`

## Note

`ExecPipe` remains available as the concise default API. Use
`ExecPipeWithOptions` to select a working directory or user, and use
`ExecInteractive` when you need terminal semantics (prompt handling, readline,
and resize behavior).
