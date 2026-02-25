# Python SDK Network Interception Example

This mirrors the Go interception demo with a **callback-based** `after` hook.

It shows one callback that:

- runs only for `host=httpbin.org` and `path=/response-headers`
- removes response header `X-Upstream`
- adds response header `X-Intercepted: callback`
- fully replaces the response body with `{"msg":"from-callback"}`

## Run

From the repository root:

```bash
uv run examples/python/network_interception/main.py
```

The example uses `matchlock` from `PATH` by default.
If you want to override the binary path, set:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

## What To Expect

The command output should include:

- response body `{"msg":"from-callback"}`
- header `X-Intercepted: callback`
- final line: `OK: callback hook intercepted and mutated the response`

The script raises an error if those expectations are not met.
