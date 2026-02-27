# Go SDK Interactive Terminal Example

This example starts a real interactive shell inside the sandbox using
`ExecInteractive`, attached directly to your local terminal.

It configures:

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

`ExecPipe` is still available for bidirectional stdin/stdout/stderr without PTY.
Use `ExecInteractive` when you need terminal semantics (prompt handling, readline,
and resize behavior).
