# Go SDK Network Interception Example

This is a single, minimal example of **callback-based** response interception with the Go SDK.

It shows one `after` hook callback that:

- runs only for `host=httpbin.org` and `path=/response-headers`
- removes response header `X-Upstream`
- adds response header `X-Intercepted: callback`
- fully replaces the response body with `{"msg":"from-callback"}`

## Run

From the repository root:

```bash
go run ./examples/go/network_interception
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

The program exits with an error if those expectations are not met.
