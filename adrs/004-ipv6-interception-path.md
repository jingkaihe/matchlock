# ADR-004: First-Class IPv6 for Intercepted Guests

**Status:** Proposed  
**Date:** 2026-09-22  
**Author:** Jingkai He

## Context

Matchlock's restricted (intercepted) network mode is IPv4-only:

- a sandbox is leased an IPv4 /24 from the subnet allocator and the guest is
  addressed statically on the TAP (`192.168.<octet>.0/24`, gateway
  `192.168.<octet>.1`, guest `192.168.<octet>.2`);
- the interception stack binds IPv4 only: `pkg/net`'s transparent proxy listens
  on the IPv4 gateway and recovers the pre-DNAT destination with
  `SO_ORIGINAL_DST`, and the DNS forwarder serves A answers;
- `pkg/net/nftables.go` installs one IPv4 table per TAP (`matchlock_<tap>`) whose
  nat/prerouting chain DNATs TCP 80/443 to the proxy, other TCP to the
  passthrough proxy and DNS 53 to the forwarder;
- toggling IPv6 on inside the guest would be a policy bypass, so the backend
  installed a second, drops-only `ip6` table (then `setupIPv6Drop`, now the
  fail-closed part of `setupIPv6Interception`) that discarded **all** guest
  IPv6.

Two pressures made that state untenable:

1. Guests increasingly need IPv6 destinations, and operators with an IPv6
   overlay (e.g. the Yggdrasil `200::/7` range) want to reach it under the same
   allow-listing they already use for IPv4.
2. "All IPv6 is dropped" is a hard switch with no middle ground: it cannot
   express "IPv6 is allowed, but only through the policy path".

The requirement is therefore not "turn IPv6 on" but "give the guest IPv6 while
keeping exactly the same enforcement". The kernel's `ip=` boot argument
configures IPv4 only, so guest-side addressing has to be done differently from
the IPv4 path.

## Decision

Implement a first-class IPv6 path for intercepted Linux sandboxes that is
policy-equivalent to the IPv4 path, and keep the macOS (Virtualization.framework
/ gVisor) interception path IPv4-only for now.

### 1. Per-VM ULA addressing from the existing lease

Each VM gets an IPv6 unique-local /64 derived from the same per-VM octet that
selects its IPv4 /24, in `pkg/state` next to the IPv4 values: octet `N` yields
`fd00:N::/64`, with the host gateway `fd00:N::1` on the TAP and the guest
address `fd00:N::2`. One lease yields both families, so the v6 /64 is as unique
as the IPv4 /24, it is persisted with the lease, and it is derived by one helper
that `Allocate`, `Get` and the read-time backfill all call.

Rejected the alternative of handing out per-VM global addresses from the host's
routed prefix: it depends on the host having IPv6 connectivity, it needs
per-VM prefix delegation or proxy NDP, and it would make sandbox creation fail
on hosts without IPv6. A ULA /64 is deterministic, self-contained, and needs no
host IPv6 egress.

### 2. Host-side link installation

The Linux backends (Firecracker and QEMU TCG) install the gateway address on the
VM's TAP. The address is set with rtnetlink `RTM_NEWADDR` — the `SIOCSIFADDR`
ioctl used for IPv4 cannot carry a 128-bit address — and with `IFA_F_NODAD`:
DAD never completes on a carrier-less TAP, and a tentative address cannot be
bound or used as a source. Because the interception proxy and the DNS forwarder
bind the guest-visible gateway address *before* the VM starts, a tentative
address would fail sandbox creation with `EADDRNOTAVAIL`.

### 3. Guest-side configuration from the kernel cmdline

`guest-init` (still privileged, PID 1) configures the guest address and the `::/0`
default route over the gateway with rtnetlink `RTM_NEWADDR` / `RTM_NEWROUTE`,
driven by an additive kernel cmdline field
`matchlock.ipv6=<guest>/<prefix>,<gateway>`. `ip=` is IPv4-only, so the kernel
cannot do it; doing it in `guest-init` means the workload only ever starts with
the route in place. A malformed field is fatal; a runtime failure to install the
address/route is a warning, because it only removes IPv6 reachability and the
guest stays usable over IPv4.

### 4. ip6 interception table, fail closed

The former drops-only `ip6` table becomes the IPv6 mirror of the IPv4
interception table (`matchlock6_<tap>`):

- nat/prerouting DNAT: TCP 80 and 443 to the HTTP/HTTPS proxy ports, UDP and TCP
  53 to the DNS forwarder port, and a catch-all TCP rule to the passthrough port
  (ordered after DNS so it cannot shadow DNS over TCP);
- filter chain: accept the redirected traffic (now addressed to the gateway) and
  ICMPv6 neighbour solicitation/advertisement, then drop every other
  guest-initiated packet;
- forward chain: keep dropping both directions — nothing legitimately traverses
  the forward path because the DNAT target is the host itself;
- output chain: accept the gateway's own traffic and neighbour discovery, drop
  the rest; input chain: accept the DNS forwarder port.

This is exactly the "drop what is not redirected" model: the only IPv6 traffic
that leaves the guest is traffic the proxy or the forwarder will see. The table
is installed in the same netlink batch as the IPv4 table, so an install failure
fails sandbox creation (the pre-existing fail-closed contract) instead of
leaving a half-applied policy. A config without an IPv6 gateway (no
interception, `--no-network`) installs no redirect at all and keeps the previous
all-dropped shape byte for byte.

### 5. Dual-stack interception stack, one policy path

- The transparent proxy binds the IPv6 gateway on the same ports as the IPv4
  listeners and shares the IPv4 handler funcs, so both families run one policy
  path. The original destination is recovered per connection family via
  `SO_DOMAIN` dispatch: `IPPROTO_IPV6` / `IP6T_SO_ORIGINAL_DST` for an IPv6
  socket, `SOL_IP` / `SO_ORIGINAL_DST` for IPv4. The family is never inferred
  from an address, and the verified dial target keeps the destination's family
  instead of falling back to an unrelated IPv4 literal.
- The DNS forwarder is dual-stack on the one port both families' DNAT rules
  point at, replies on the socket the query arrived on, and relays answers
  verbatim — AAAA records are not stripped or rewritten. The private-address
  check runs on the resolved addresses, so a name resolving only to an unlisted
  private IPv6 address is refused exactly like its IPv4 equivalent.
- `block_private_ips` already covers `::1/128`, `fc00::/7`, `fe80::/10` and
  `200::/7`; `allow_private` exempts them with the same entry syntax, including
  the `[v6]:port` form. The policy engine needed one fix: host-scoped decisions
  must normalize a bracketed IPv6 host instead of splitting on `":"` (which
  turned `[fd00:1::5]:8080` into `[` and silently skipped every host rule).

### 6. Wiring scope

The IPv6 half of the addressing is derived in one place from the lease and is
filled only when host-side interception is active. A plain NAT sandbox and a
`--no-network` sandbox keep the IPv4-only backend config, TAP configuration and
kernel args identical to before. The same helper feeds the backend config, the
proxy bind address, the DNS bind address and the `ip6` table gateway, so those
consumers cannot disagree; the table is recorded in the lifecycle resources so
reconcile removes an orphaned `matchlock6_<tap>` for a dead TAP.

### 7. macOS unchanged

`pkg/sandbox/sandbox_linux.go` is the only producer of the IPv6 fields; on
darwin the interception path keeps its existing IPv4 behaviour and the v6 fields
stay empty. The policy engine's IPv6 handling is cross-platform, but the guest
link and the `ip6` table are Linux-only.

## Consequences

### Positive

- An intercepted guest can reach IPv6 destinations with the same allow-list,
  `block_private_ips` and `allow_private` enforcement as IPv4 — no second policy
  implementation to keep in sync.
- The former blanket IPv6 drop is replaced by an explicit redirect-plus-drop
  model, which removes the "IPv6 is unusable" limitation without opening an
  unfiltered path.
- One policy path for both families: the v6 listeners share the v4 handlers, the
  v6 DNAT rules share the ports and the forwarder, and the addressing comes from
  one lease, so parity is structural rather than duplicated.

### Negative

- More per-VM state (a second address pair and prefix) and more per-VM netfilter
  state (a second table, more chains/rules).
- More listeners on the host, and the proxy/DNS bind now require the gateway
  address to be present and usable before the VM starts (it is installed with
  `IFA_F_NODAD` for that reason).
- Guest-side addressing is a second configuration mechanism next to `ip=`, with
  its own parse/validate surface in `guest-init`.

### Risk Areas

- Any TAP-scoped fail-closed drop must keep accepting ICMPv6 neighbour
  discovery, or the guest cannot resolve the gateway at all.
- A Linux backend that installs the IPv4 link but not the IPv6 one leaves the
  proxy bound to a non-existent address (`EADDRNOTAVAIL`) — the install is a
  parity contract across every Linux backend (Firecracker and QEMU TCG both
  implement it).
- Public-IPv6 acceptance coverage depends on the host having IPv6 egress; the
  probe skips with an explicit reason when it does not, while the private-range
  refusal and no-leak cases run against host-independent fixtures.

## Alternatives Considered

### 1. Keep dropping all guest IPv6 (status quo)

Rejected as the end state: safe but useless. It remains the behaviour when
interception is not active (a `--no-network` sandbox), which is the one case
where a drop is the whole policy.

### 2. SLAAC / router advertisements for guest addressing

Rejected. The guest link is a point-to-point TAP with a known static address, so
RA/RS would add a daemon (or a kernel RA path) and a timing dependency for no
benefit. Static netlink configuration in `guest-init` is deterministic and
already privileged at that point in boot.

### 3. 6to4/Teredo or v4-mapped destinations

Rejected. Tunnelling or mapping IPv6 to IPv4 would collapse the destination
family and make an IPv6-only destination unreachable; the family-aware dial and
`IP6T_SO_ORIGINAL_DST` exist precisely to avoid that.

### 4. Guest-side IPv6 firewall rules instead of host-side DNAT

Rejected. Guest-side rules (e.g. nft inside the guest with `CAP_NET_ADMIN`)
would split policy authority: the workload can change its own rules, while the
host-side proxy is the component that actually holds the allow-list, the secrets
and the private-range check.

### 5. Extend the macOS interception path to IPv6 in the same change

Deferred. The darwin path runs gVisor userspace TCP/IP at L4 rather than
netfilter, so it is a separate piece of work; darwin keeps its current IPv4
behaviour and must keep compiling and passing its tests.

## Implementation Plan

### Phase 1: Addressing and guest link

- Allocate the per-VM ULA /64 with the lease and persist it (done).
- Put the gateway address on the TAP in both Linux backends with `IFA_F_NODAD`
  and emit the `matchlock.ipv6=` cmdline field (done).
- Configure the guest address and `::/0` route in `guest-init` via netlink (done).

### Phase 2: Interception path

- Convert the `ip6` table into the fail-closed redirect table and keep one
  all-or-nothing install (done).
- Make the proxy dual-stack with the IPv6 original-destination lookup and a
  family-aware dial (done).
- Make the DNS forwarder dual-stack with verbatim AAAA relay (done).

### Phase 3: Lifecycle and cleanup

- Wire the per-VM addressing through sandbox creation into the backend, the
  proxy, the forwarder and the `ip6` table, and record the table for reconcile
  (done).

### Phase 4: Verification

- Unit tests for the rule plan, the socketopt dispatch, the forwarder and the
  netlink builders (done).
- Real-VM acceptance tests: an allow_private-exempt IPv6 destination is reached
  through the proxy, unlisted private ranges (`fc00::/7`, `200::/7`) are
  refused, a non-redirected IPv6 connect leaks nothing, and the IPv4
  network/policy/interception subsets stay green (done).
- macOS cross-compile gate (`mise run check:darwin`) stays green (done).

## Acceptance Criteria

- A guest reaches an `allow_private`-exempt IPv6 destination through the
  interception proxy and gets the payload back.
- An unlisted private IPv6 destination (`fc00::/7` or `200::/7`) is refused and
  the destination fixture is never dialed.
- A raw IPv6 connect that the `ip6` table did not redirect yields no payload.
- The IPv4 network/policy/interception acceptance tests pass unchanged.
- A `--no-network` sandbox and a plain NAT sandbox carry no IPv6 configuration.
- Documentation (README, `docs/network-interception.md`) describes the
  addressing scheme, the rule model and the policy behaviour.