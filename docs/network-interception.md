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

## Private-IP Blocking (`block_private_ips`)

When `block_private_ips` is enabled, a destination is denied if any address it
targets or resolves to is in one of these ranges:

| Range | Meaning |
| --- | --- |
| `10.0.0.0/8` | RFC 1918 |
| `172.16.0.0/12` | RFC 1918 |
| `192.168.0.0/16` | RFC 1918 |
| `127.0.0.0/8` | loopback |
| `169.254.0.0/16` | IPv4 link-local |
| `100.64.0.0/10` | CGNAT / Tailscale-style |
| `::1/128` | IPv6 loopback |
| `fc00::/7` | IPv6 unique local |
| `fe80::/10` | IPv6 link-local |
| `200::/7` | Yggdrasil overlay |

IPv4-mapped IPv6 literals (`::ffff:0:0/96`) are handled by checking the embedded
IPv4 address against the IPv4 ranges above: `::ffff:192.168.1.1` is private and
`::ffff:8.8.8.8` is public. The mapped prefix is matched explicitly rather than
listed as a CIDR, because `net.ParseCIDR("::ffff:0:0/96")` normalizes to
`0.0.0.0/0` and would otherwise mark every IPv4 address private.

### Private-IP Exemptions (`allow_private`)

`allow_private` is an explicit exception list evaluated **before** the
`block_private_ips` refusal, so a listed destination is reachable while the
private block still applies to everything else.

Entry syntax: a host name, an IP literal or a CIDR, optionally suffixed with a
port (`host:port`, `[v6]:port`). A bare entry means **any port**; when an entry
carries a port, the destination port must match:

| Entry form | Example | Matches |
| --- | --- | --- |
| IP literal, any port | `192.168.107.74` | that address on any port |
| IP literal + port | `192.168.107.74:8888` | that address only on 8888 |
| IPv6 literal (bare) | `200:1234::1` | that address on any port |
| IPv6 literal + port | `[200:1234::1]:443` | that address only on 443 |
| CIDR, any port | `200::/7` | any address in the range, any port |
| CIDR + port | `192.168.0.0/16:8080` | addresses in the range on 8080 |
| host name, any port | `ai.internal` | that name, any port |
| host name + port | `ai.internal:8888` | that name only on 8888 |

A private destination is exempt only when its resolved address is inside a
listed CIDR or equals a listed IP literal. A host name additionally needs a
matching name entry, and a name that resolves to **any** unlisted private
address is still refused — even when another of its addresses is listed. This is
the DNS-rebinding guard: a listed name never authorizes an unlisted private
address.

`allow_private` only lifts the private block. `allowed_hosts` keeps its meaning
(a non-empty allow-list still applies to the public side), and `no_network`
still wins over every exemption.

CLI:

```bash
# Reach one LAN endpoint and one Yggdrasil address while keeping the block on.
matchlock run --image alpine:latest \
  --allow-private 192.168.107.74:8888 \
  --allow-private 200:1234::1 -- curl http://192.168.107.74:8888/health
```

Go SDK: call `.WithAllowPrivate("<entry>", ...)` (repeatable) to keep the
private block on and lift it only for the listed destinations.

```go
sandbox := sdk.New("alpine:latest").
	WithBlockPrivateIPs(true).
	WithAllowPrivate("192.168.107.74:8888", "[200:1234::1]:443")
```

Wire API (`create`): set the `network.allow_private` array of strings. The CLI
flag is repeatable; the JSON-RPC field takes the same entries.

## IPv6 Interception

On Linux the interception path is dual-stack. An intercepted sandbox is leased a
per-VM unique-local IPv6 /64 next to its IPv4 /24 — octet `N` yields
`fd00:N::/64`, with the gateway `fd00:N::1` on the sandbox TAP and the guest
address `fd00:N::2` — the Linux backends put the gateway address on the TAP, and
`guest-init` configures the guest address and its `::/0` route from the
`matchlock.ipv6=<guest>/<prefix>,<gateway>` kernel cmdline field, because the
kernel's `ip=` boot argument configures IPv4 only.

Guest IPv6 is redirected into the same host-side stack by an `ip6` nftables table
(`matchlock6_<tap>`) that mirrors the IPv4 table:

| Guest IPv6 destination | Redirect target |
| --- | --- |
| TCP 80 | HTTP interception proxy on the sandbox's IPv6 gateway |
| TCP 443 | HTTPS interception proxy (TLS MITM) on the gateway |
| TCP, every other port | passthrough proxy on the gateway (only when passthrough is enabled) |
| UDP and TCP 53 | DNS forwarder on the gateway |
| anything else | dropped |

Rule model:

- The DNAT rules precede the residual drops, and the drops stay in place for
  guest-initiated traffic, for forwarded traffic in both directions and for
  host-emitted traffic toward the TAP. Redirected packets are accepted before the
  drop, so what gets through is exactly what the proxy or the forwarder handles.
- ICMPv6 neighbour solicitation/advertisement is the one non-redirected exception:
  without neighbour discovery the guest could not resolve the gateway at all.
  The address and the default route are configured statically, so no router
  solicitation/advertisement is involved.
- The table is fail closed: it is applied in the same netlink batch as the IPv4
  table, so if the `ip6` rules cannot be installed sandbox creation fails, and no
  sandbox starts with a partially applied policy. A table without a gateway
  (no interception, `--no-network`) carries no redirect and keeps the previous
  all-dropped shape.

Policy parity — an IPv6 destination is decided exactly like its IPv4 equivalent:

- The proxy binds the IPv6 gateway on the same ports and recovers the original
  destination with `IP6T_SO_ORIGINAL_DST`, so the allow-list, hook rules and
  secrets apply identically, and the dial keeps the destination's address family.
- The DNS forwarder is reachable over IPv6 and relays AAAA answers unchanged. The
  private-address check runs on the resolved addresses, so a name that resolves
  only to an unlisted private IPv6 address is refused — the same DNS-rebinding
  guard as IPv4.
- `block_private_ips` covers `::1/128`, `fc00::/7`, `fe80::/10` and `200::/7`,
  and `allow_private` exempts them with the same entry syntax, including the
  `[v6]:port` form.
- A raw IPv6 connect that the table did not redirect never reaches the proxy and
  is dropped, so there is no IPv6 leak path around the policy.

The IPv6 link and the `ip6` table are Linux-only; the macOS interception path
keeps its existing IPv4 behaviour.

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
- [`examples/python/network_interception/main.py`](../examples/python/network_interception/main.py)
- [`examples/typescript/network_interception/main.ts`](../examples/typescript/network_interception/main.ts)
