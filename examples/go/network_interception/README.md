# Go SDK Network Interception Example

This example is a guided walkthrough of Matchlock network interception features:

1. request mutation (`before` hook)
2. response mutation (`after` hook)
3. runtime allow-list tightening
4. runtime allow-list expansion + SSE body shaping

## Run

From the repository root:

```bash
mise run build

# Linux only (one-time setup)
sudo ./bin/matchlock setup linux

go run ./examples/go/network_interception
```

The example uses `./bin/matchlock` by default.
If you want a different binary, set:

```bash
export MATCHLOCK_BIN=/path/to/matchlock
```

## What To Expect

You should see a step-by-step flow:

- `1) Request hook...`
  The outbound request is rewritten from `/anything/v1?drop=1` to `/anything/v2?trace=hooked`,
  header `X-Hook` is added, and header `X-Remove` is removed.

- `2) Response hook...`
  The response includes `X-Intercepted: true`, removes `X-Upstream`, and replaces `foo` -> `bar` in body content.

- `3) Runtime allow-list update...`
  After restricting to `example.com`, a request to `httpbin.org` prints `BLOCKED`.

- `4) Expand allow-list and run SSE body replacement hook`
  SSE `data:` lines are transformed (`"id"` -> `"sid"`).

The program validates expected output at each step and exits with an error if behavior differs.
