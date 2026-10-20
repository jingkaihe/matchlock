package sandbox

import "github.com/jingkaihe/matchlock/pkg/state"

// sandboxNetLink is the per-VM network addressing the sandbox hands to every
// consumer of the guest's network link: the backend VMConfig (guest kernel
// arguments) and, on darwin, the userspace network stack Config (the NIC
// address and route the stack is brought up with).
//
// Deriving it in ONE place from the subnet lease is what keeps those consumers
// from drifting apart. The IPv6 half is live only when interception is active:
// a plain-NAT sandbox and a --no-network sandbox keep the IPv4-only shape, so
// their backend config and boot args are byte-identical to before (mirroring
// the Linux interceptionWiring gate).
//
// The helper carries no build tag so the gating is unit-testable on every
// platform; both the darwin and the Linux sandbox wiring consume the same
// shape.
type sandboxNetLink struct {
	GatewayIPv4 string // guest-side IPv4 gateway (boot arg / stack NIC address)
	GuestIPv4   string // guest IPv4 address (boot arg)
	SubnetCIDR  string // v4 address and prefix, e.g. 192.168.100.1/24
	GatewayIPv6 string // guest-visible IPv6 gateway; "" = IPv6 path not wired
	GuestIPv6   string // guest IPv6 address (boot arg)
	Subnet6CIDR string // per-VM ULA /64 as the lease records it (fd00:100::/64)
}

// newSandboxNetLink derives the link from the VM's subnet lease. intercept is
// the sandbox's needsInterception decision: only an intercepted sandbox gets
// IPv6 addressing, because interception (the userspace stack on darwin, the
// per-TAP ip6 table on Linux) is what keeps guest IPv6 inside the policy path.
// A nil lease (--no-network) yields an empty, fully inert link.
func newSandboxNetLink(info *state.SubnetInfo, intercept bool) sandboxNetLink {
	var l sandboxNetLink
	if info == nil {
		return l
	}
	l.GatewayIPv4 = info.GatewayIP
	l.GuestIPv4 = info.GuestIP
	l.SubnetCIDR = info.GatewayIP + "/24"
	if intercept {
		l.GatewayIPv6 = info.GatewayIPv6
		l.GuestIPv6 = info.GuestIPv6
		l.Subnet6CIDR = info.Subnet6
	}
	return l
}
