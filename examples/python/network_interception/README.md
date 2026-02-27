# Python SDK Network Interception Example

This mirrors the Go network interception demo: it streams a response from the Anthropic API, but injects the real API key with a host-side `before` hook so the secret never enters the VM.

The callback:

- runs for outbound requests to `api.anthropic.com`
- clones request headers and injects `X-Api-Key` from `ANTHROPIC_API_KEY`
- lets the in-VM script use a placeholder API key value

## Run

From the repository root:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
uv run examples/python/network_interception/main.py
```

The example uses `matchlock` from `PATH` by default.
If you want to override the binary path, set:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

## What To Expect

The output should be identical to the basic Anthropic example:

- `Python 3.12.x` version line
- A streamed explanation of TCP from Claude
- A final log line with exit code and duration
