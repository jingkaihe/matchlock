# Network Interception

Network interception is the host-side HTTP(S) control plane for Matchlock.
It powers:

- outbound host allow-list enforcement
- secret placeholder replacement (real secret never enters the VM)
- request/response hook rules (block or mutate)
- runtime allow-list updates for running sandboxes

## When It Is Enabled

Interception is enabled when policy requires it, or when forced:

- allow-list is configured (`--allow-host`, `.AllowHost(...)`)
- secrets are configured (`--secret`, `.AddSecret(...)`)
- hook rules are configured (`.WithNetworkInterception(&cfg)`)
- interception is forced (`--network-intercept`, `.WithNetworkInterception()` with no args)

Notes:

- `--network-intercept` forces interception even with an empty allow-list.
- `--no-network` cannot be combined with allow-list, secrets, or interception.
- Hook rules are currently configured through the Go SDK (or wire API), not via dedicated CLI rule flags.
- In the Go SDK, use `sdk.New(...).WithNetworkInterception(...)` plus context-aware client methods (`Launch`, `AllowListAdd`, `AllowListDelete`).

## Runtime Allow-List Mutation

Use this when you want to start a VM and evolve egress policy while it runs.

CLI example:

```bash
# Start a long-lived VM with interception enabled.
matchlock run --image alpine:latest --rm=false --network-intercept

# Add or remove hosts at runtime (comma-separated values accepted).
matchlock allow-list add <vm-id> api.openai.com,api.anthropic.com
matchlock allow-list delete <vm-id> api.openai.com
```

Go SDK example:

```go
client, _ := sdk.NewClient(sdk.DefaultConfig())
defer client.Close(0)
defer client.Remove()

vm := sdk.New("alpine:latest").WithNetworkInterception()
_, _ = client.Launch(vm)

ctx := context.Background()
added, _ := client.AllowListAdd(ctx, "api.openai.com", "api.anthropic.com")
removed, _ := client.AllowListDelete(ctx, "api.openai.com")

_ = added
_ = removed
```

Behavior:

- add/delete input is normalized (comma-splitting, trim, de-dup)
- empty allow-list means "allow all hosts"
- for CLI usage, a running VM with an available exec relay socket is required (`--rm=false` is the practical mode)

## Rule Model

Rules live under `network.interception.rules` (wire API) or `sdk.NetworkInterceptionConfig` (Go SDK).

Each rule has:

- `phase`: `before` or `after`
- `action`: `allow`, `block`, or `mutate`
- optional matchers: `hosts`, `methods`, `path`

Matcher semantics:

- `hosts`: glob patterns (`*.example.com`), empty means all hosts
- `methods`: HTTP methods, empty means all methods
- `path`: URL path glob, empty means all paths

Defaults:

- omitted `phase` defaults to `before`
- omitted `action` defaults to `allow`

If mutation fields are present and `action` is `allow`, the rule is treated as mutate.

## SDK Callback Hooks

SDK rules can attach a local callback for dynamic mutation.

- Static filters (`phase`, `hosts`, `methods`, `path`) are evaluated first.
- Only matching traffic invokes the callback.
- Callback hooks are SDK-local and run in the SDK process (not inside the VM).
- For callback rules, keep `action` empty or `allow`; return the effective action from the callback.
- `timeout_ms` bounds callback execution time.
- Callback can return dynamic action/mutations:
  - request edits (`Request.Headers`, `Request.Query`, `Request.Path`)
  - response edits (`Response.Headers`, `Response.BodyReplacements`)
  - full response body replacement (`Response.SetBody`)
- For callback object fields, `Headers`/`Query` are full replacements when set (non-nil).

## Traffic Scope

- Hook rules apply to HTTP and HTTPS traffic handled by Matchlock interception.
- HTTPS hooks run on decrypted traffic inside the host MITM path.
- Non-HTTP protocols are not mutated by hook rules.

## Before-Phase Request Controls

For `phase=before`, you can mutate the outbound request with:

- `set_headers`
- `delete_headers`
- `set_query`
- `delete_query`
- `rewrite_path`

You can also block outright with `action=block`.

## After-Phase Response Controls

For `phase=after`, you can mutate the inbound response with:

- `set_response_headers`
- `delete_response_headers`
- `body_replacements` (literal find/replace)

You can also block at response time with `action=block`.

### SSE Behavior

For `text/event-stream` responses, `body_replacements` are applied only to each `data:` line payload, preserving SSE framing.

For non-SSE responses, replacements are applied to the full response body.

## Secret Replacement Scope

Secret placeholders are replaced in:

- request headers
- URL/query string

Request body replacement is intentionally not performed for secrets.

## Go SDK Example With Hooks

```go
sandbox := sdk.New("alpine:latest").
	AllowHost("httpbin.org").
	WithNetworkInterception(&sdk.NetworkInterceptionConfig{
		Rules: []sdk.NetworkHookRule{
			{
				Phase: sdk.NetworkHookPhaseAfter,
				Hosts: []string{"httpbin.org"},
				Path:  "/response-headers",
				Hook: func(ctx context.Context, req sdk.NetworkHookRequest) (*sdk.NetworkHookResult, error) {
					if req.StatusCode != 200 {
						return nil, nil
					}
					return &sdk.NetworkHookResult{
						Action: sdk.NetworkHookActionMutate,
						Response: &sdk.NetworkHookResponseMutation{
							Headers: map[string][]string{
								"X-Intercepted": []string{"callback"},
							},
							SetBody: []byte(`{"msg":"from-callback"}`),
						},
					}, nil
				},
			},
		},
	})
```

## Current Scope

- Static and callback-based hook-rule APIs are available in the Go SDK and wire API.
- Python and TypeScript SDK builders now support typed static network hook-rule builders.
- Callback hook execution (SDK-local hook functions) is available in Go, Python, and TypeScript SDKs.

See runnable examples:
- [`examples/go/network_interception/main.go`](../examples/go/network_interception/main.go)
