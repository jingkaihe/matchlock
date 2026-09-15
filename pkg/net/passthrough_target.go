package net

import (
	"net"
	"strconv"
)

// Darwin resolves the guest's network stack in userspace: the guest's default
// route points at the gVisor netstack's virtual gateway (Config.GatewayIP),
// which is assigned to the stack NIC in NewNetworkStack. That address is NOT
// bound on the host — macOS has no TAP — so a direct host dial to it can never
// connect, even when the guest-visible address is allowlisted. Mapping the
// guest-visible gateway to host loopback lets the guest reach a host listener
// bound to 127.0.0.1/::1.
//
// Linux must NOT use this mapping: there the transparent proxy's TAP gateway is
// a real host address assigned to the bridge, so callers pass an empty
// gatewayIP and the destination is dialed unchanged.
//
// The mapping is deliberately a pure string transformation with no build tag so
// it compiles and unit-tests on every platform.

// mapGatewayDialHost returns the host address to dial for a guest-visible
// destination. When dstIP equals the stack's configured gatewayIP — compared
// with net.IP.Equal so IPv4 and IPv4-mapped-IPv6 forms such as
// ::ffff:192.168.100.1 match — it maps to host loopback: 127.0.0.1 for an IPv4
// gateway and ::1 for an IPv6 gateway. In every other case (empty gateway,
// unparseable input, or a non-gateway destination) dstIP is returned unchanged
// so an unrecognized target is never rewritten.
func mapGatewayDialHost(dstIP, gatewayIP string) string {
	if gatewayIP == "" {
		return dstIP
	}
	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return dstIP
	}
	dst := net.ParseIP(dstIP)
	if dst == nil {
		return dstIP
	}
	if !dst.Equal(gw) {
		return dstIP
	}
	if gw.To4() != nil {
		return "127.0.0.1"
	}
	return "::1"
}

// resolvePassthroughTarget resolves the host address:port to dial for a
// passthrough destination, preserving the requested port. See
// mapGatewayDialHost for the darwin gateway-to-loopback rationale and the
// Linux "pass no gateway" requirement.
func resolvePassthroughTarget(dstIP string, dstPort int, gatewayIP string) string {
	return net.JoinHostPort(mapGatewayDialHost(dstIP, gatewayIP), strconv.Itoa(dstPort))
}
