# TypeScript SDK Exec Modes Example

Demonstrates all three execution styles:

- `execStream` for live stdout/stderr streaming
- `execPipe` for bidirectional stdin/stdout/stderr without PTY
- `execInteractive` for an interactive PTY shell

## Run

From the repository root:

```bash
cd examples/typescript/exec_modes
npm install
npm run start
```

## Notes

- The interactive section requires a TTY (`process.stdin.isTTY` and `process.stdout.isTTY`).
- In non-interactive environments, the script still runs stream + pipe and skips interactive mode.
- This example imports the local SDK source from `sdk/typescript/src` so it tracks unreleased APIs in this repo.
