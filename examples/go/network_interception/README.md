# Go SDK Network Interception Example

This example mirrors the [basic example](../basic) — it streams a response from the Anthropic API — but instead of using the `AddSecret` primitive to inject the API key, it uses a **network interception hook** to inject the `x-api-key` header on the host side so the real secret never enters the VM.

It registers one `before` hook callback that:

- runs for every outbound request to `api.anthropic.com`
- copies the original request headers and injects `X-Api-Key` with the real secret
- the Python script inside the VM only sees a placeholder value for the API key

## Run

From the repository root:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run ./examples/go/network_interception
```

The example uses `./bin/matchlock` by default.
To override the binary path, set:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

## What To Expect

The output should be identical to the basic example:

- `Python 3.12.x` version line
- A streamed explanation of TCP from Claude
- A final log line with exit code and duration

The key difference is that the API key is injected via network interception rather than the secret primitive, demonstrating how `WithNetworkInterception` can be used for host-side credential injection.
