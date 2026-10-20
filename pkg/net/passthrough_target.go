package net

import (
	"net"
	"strconv"
)

// The userspace (darwin / VZ) network stack resolves the guest's network in
// userspace: the guest's default routes point at the gVisor netstack's virtual
// gateways (Config.GatewayIP / Config.GatewayIPv6), which are assigned to the
// stack NIC in NewNetworkStack. Those addresses are NOT bound on the host —
// macOS has no TAP — so a direct host dial to them can never connect, even when
// the guest-visible address is allowlisted. Mapping the guest-visible gateway
// to host loopback lets the guest reach a host listener bound to 127.0.0.1/::1.
//
// Linux must NOT use this mapping: there the transparent proxy's TAP gateway is
// a real host address assigned to the bridge, so callers pass empty gateways
// and the destination is dialed unchanged. The Linux TransparentProxy path
// never sets either gateway field.
//
// The mapping is deliberately a pure string transformation with no build tag so
// it compiles and unit-tests on every platform.

// mapGatewayDialHost returns the host address to dial for a guest-visible
// destination. When dstIP equals the stack's configured IPv4 gatewayIP or IPv6
// gatewayIPv6 — compared with net.IP.Equal so IPv4 and IPv4-mapped-IPv6 forms
// such as ::ffff:192.168.100.1 match — it maps to host loopback: 127.0.0.1 for
// an IPv4 gateway and ::1 for an IPv6 gateway. In every other case (empty
// gateways, unparseable input, or a non-gateway destination) dstIP is returned
// unchanged so an unrecognized target is never rewritten.
func mapGatewayDialHost(dstIP, gatewayIP, gatewayIPv6 string) string {
	dst := net.ParseIP(dstIP)
	if dst == nil {
		return dstIP
	}
	// IPv4 gateway: an IPv4 or IPv4-mapped-IPv6 destination equal to it dials
	// the IPv4 loopback.
	if gatewayIP != "" {
		if gw := net.ParseIP(gatewayIP); gw != nil && dst.Equal(gw) {
			if gw.To4() != nil {
				return "127.0.0.1"
			}
			return "::1"
		}
	}
	// IPv6 gateway: only a native IPv6 destination may match (net.IP.Equal
	// already treats a v4-mapped form of a non-mapped address as unequal, but
	// the field is guarded explicitly so a v4-mapped literal can never be
	// answered from ::1).
	if gatewayIPv6 != "" && dst.To4() == nil {
		if gw := net.ParseIP(gatewayIPv6); gw != nil && dst.Equal(gw) {
			if gw.To4() != nil {
				return "127.0.0.1"
			}
			return "::1"
		}
	}
	return dstIP
}

// resolvePassthroughTarget resolves the host address:port to dial for a
// passthrough destination, preserving the requested port. See
// mapGatewayDialHost for the gateway-to-loopback rationale and the Linux
// "pass no gateways" requirement.
func resolvePassthroughTarget(dstIP string, dstPort int, gatewayIP, gatewayIPv6 string) string {
	return net.JoinHostPort(mapGatewayDialHost(dstIP, gatewayIP, gatewayIPv6), strconv.Itoa(dstPort))
}
