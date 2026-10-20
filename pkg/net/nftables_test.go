//go:build linux

package net

import (
	"net"
	"testing"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNewNFTablesRulesParsesDNSIPv4Servers(t *testing.T) {
	rules := NewNFTablesRules(
		"tap0",
		"192.168.100.1",
		8080,
		8443,
		0,
		[]string{"1.1.1.1", "8.8.8.8:53", "resolver.local", "[2606:4700:4700::1111]:53"},
	)

	require.Equal(t, []net.IP{
		net.IPv4(1, 1, 1, 1).To4(),
		net.IPv4(8, 8, 8, 8).To4(),
	}, rules.dnsServers)
}

func TestBuildDNSForwarderInputAcceptRule(t *testing.T) {
	rules := &NFTablesRules{
		tapInterface: "tap0",
		gatewayIP:    net.IPv4(192, 168, 100, 1).To4(),
	}

	exprs := rules.buildDNSForwarderInputAcceptRule(5353)
	require.Len(t, exprs, 9)

	iifMeta, ok := exprs[0].(*expr.Meta)
	require.True(t, ok)
	require.Equal(t, expr.MetaKeyIIFNAME, iifMeta.Key)

	iifCmp, ok := exprs[1].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, ifname("tap0"), iifCmp.Data)

	protoMeta, ok := exprs[2].(*expr.Meta)
	require.True(t, ok)
	require.Equal(t, expr.MetaKeyL4PROTO, protoMeta.Key)

	protoCmp, ok := exprs[3].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, []byte{unix.IPPROTO_UDP}, protoCmp.Data)

	dstIPCmp, ok := exprs[5].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, []byte(net.IPv4(192, 168, 100, 1).To4()), dstIPCmp.Data)

	dstPortCmp, ok := exprs[7].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, binaryutil.BigEndian.PutUint16(5353), dstPortCmp.Data)

	verdict, ok := exprs[8].(*expr.Verdict)
	require.True(t, ok)
	require.Equal(t, expr.VerdictAccept, verdict.Kind)
}

func TestBuildDNSForwarderOutputAcceptRule(t *testing.T) {
	rules := &NFTablesRules{}

	exprs := rules.buildDNSForwarderOutputAcceptRule(net.IPv4(8, 8, 8, 8))
	require.Len(t, exprs, 7)

	protoMeta, ok := exprs[0].(*expr.Meta)
	require.True(t, ok)
	require.Equal(t, expr.MetaKeyL4PROTO, protoMeta.Key)

	protoCmp, ok := exprs[1].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, []byte{unix.IPPROTO_UDP}, protoCmp.Data)

	dstIPCmp, ok := exprs[3].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, []byte(net.IPv4(8, 8, 8, 8).To4()), dstIPCmp.Data)

	dstPortCmp, ok := exprs[5].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, binaryutil.BigEndian.PutUint16(53), dstPortCmp.Data)

	verdict, ok := exprs[6].(*expr.Verdict)
	require.True(t, ok)
	require.Equal(t, expr.VerdictAccept, verdict.Kind)
}

// TestBuildIPv6DropRuleInput qualifies that the guest-initiated IPv6 drop rule
// matches the TAP ingress interface and drops the packet (no privileges needed —
// this verifies the rule builder, not the kernel installation).
func TestBuildIPv6DropRuleInput(t *testing.T) {
	rules := &NFTablesRules{tapInterface: "tap0"}

	exprs := rules.buildIPv6DropRule(true)
	require.Len(t, exprs, 3)

	iifMeta, ok := exprs[0].(*expr.Meta)
	require.True(t, ok)
	require.Equal(t, expr.MetaKeyIIFNAME, iifMeta.Key)

	iifCmp, ok := exprs[1].(*expr.Cmp)
	require.True(t, ok)
	require.Equal(t, expr.CmpOpEq, iifCmp.Op)
	require.Equal(t, ifname("tap0"), iifCmp.Data)

	verdict, ok := exprs[2].(*expr.Verdict)
	require.True(t, ok)
	require.Equal(t, expr.VerdictDrop, verdict.Kind)
}

// TestBuildIPv6DropRuleOutput qualifies that the host->guest IPv6 drop rule
// matches the TAP egress interface and drops the packet.
func TestBuildIPv6DropRuleOutput(t *testing.T) {
	rules := &NFTablesRules{tapInterface: "tap0"}

	exprs := rules.buildIPv6DropRule(false)
	require.Len(t, exprs, 3)

	oifMeta, ok := exprs[0].(*expr.Meta)
	require.True(t, ok)
	require.Equal(t, expr.MetaKeyOIFNAME, oifMeta.Key)

	verdict, ok := exprs[2].(*expr.Verdict)
	require.True(t, ok)
	require.Equal(t, expr.VerdictDrop, verdict.Kind)
}

func requireMetaExpr(t *testing.T, got expr.Any, key expr.MetaKey) {
	t.Helper()
	meta, ok := got.(*expr.Meta)
	require.Truef(t, ok, "want meta expression, got %T", got)
	require.Equal(t, key, meta.Key)
}

func requireCmpExpr(t *testing.T, got expr.Any, data []byte) {
	t.Helper()
	cmp, ok := got.(*expr.Cmp)
	require.Truef(t, ok, "want cmp expression, got %T", got)
	require.Equal(t, expr.CmpOpEq, cmp.Op)
	require.Equal(t, data, cmp.Data)
}

func requirePayloadExpr(t *testing.T, got expr.Any, base expr.PayloadBase, offset, length uint32) {
	t.Helper()
	payload, ok := got.(*expr.Payload)
	require.Truef(t, ok, "want payload expression, got %T", got)
	require.Equal(t, base, payload.Base)
	require.Equal(t, offset, payload.Offset)
	require.Equal(t, length, payload.Len)
}

func requireImmediateExpr(t *testing.T, got expr.Any, register uint32, data []byte) {
	t.Helper()
	immediate, ok := got.(*expr.Immediate)
	require.Truef(t, ok, "want immediate expression, got %T", got)
	require.Equal(t, register, immediate.Register)
	require.Equal(t, data, immediate.Data)
}

func requireVerdictExpr(t *testing.T, got expr.Any, kind expr.VerdictKind) {
	t.Helper()
	verdict, ok := got.(*expr.Verdict)
	require.Truef(t, ok, "want verdict expression, got %T", got)
	require.Equal(t, kind, verdict.Kind)
}

func requireNATExpr(t *testing.T, got expr.Any, regAddrMin, regProtoMin uint32) {
	t.Helper()
	nat, ok := got.(*expr.NAT)
	require.Truef(t, ok, "want nat expression, got %T", got)
	require.Equal(t, expr.NATTypeDestNAT, nat.Type)
	require.Equal(t, uint32(unix.NFPROTO_IPV6), nat.Family)
	require.Equal(t, regAddrMin, nat.RegAddrMin)
	require.Equal(t, regProtoMin, nat.RegProtoMin)
	require.Equal(t, uint32(0), nat.RegAddrMax)
	require.Equal(t, uint32(0), nat.RegProtoMax)
}

// newIPv6TestRules returns rules armed with both the IPv4 and the IPv6 gateway,
// i.e. the shape the sandbox uses once interception is wired for IPv6.
func newIPv6TestRules(t *testing.T, httpPort, httpsPort, passthroughPort, dnsForwarderPort int) *NFTablesRules {
	t.Helper()
	rules := NewNFTablesRules("tap0", "192.168.100.1", httpPort, httpsPort, passthroughPort, nil)
	rules.SetGatewayIPv6("fd00:200::1")
	rules.SetDNSForwarderPort(dnsForwarderPort)
	return rules
}

// TestNFTablesRulesAccessors pins the observation API the sandbox wiring and the
// lifecycle records rely on: the ip6 table name, the configured gateway and the
// ports the rules carry (the same ones the ip6 redirects target).
func TestNFTablesRulesAccessors(t *testing.T) {
	rules := NewNFTablesRules("fc-abc12345", "192.168.100.1", 8080, 8443, 15000, nil)
	require.Equal(t, "matchlock6_fc-abc12345", rules.TableNameV6())
	require.Equal(t, "matchlock_fc-abc12345", FirewallTableName("fc-abc12345"))
	require.Nil(t, rules.GatewayIPv6(), "unset gateway leaves the ip6 table fail-closed")

	rules.SetGatewayIPv6("fd00:100::1")
	rules.SetDNSForwarderPort(5353)
	require.Equal(t, "fd00:100::1", rules.GatewayIPv6().String())

	httpPort, httpsPort, passthroughPort, dnsPort := rules.InterceptionPorts()
	require.Equal(t, 8080, httpPort)
	require.Equal(t, 8443, httpsPort)
	require.Equal(t, 15000, passthroughPort)
	require.Equal(t, 5353, dnsPort)

	// The name helper and the table Setup installs agree.
	require.Equal(t, rules.TableNameV6(), FirewallTableV6Name("fc-abc12345"))
}

// TestSetGatewayIPv6AcceptsOnlyIPv6Addresses pins the fail-closed setter: only a
// real IPv6 address arms the ip6 redirect. Anything else (empty, malformed,
// IPv4, IPv4-mapped) must leave the table without a gateway, in which case the
// plan redirects nothing and drops all guest IPv6.
func TestSetGatewayIPv6AcceptsOnlyIPv6Addresses(t *testing.T) {
	tests := []struct {
		name    string
		gateway string
		want    []byte
	}{
		{name: "unique local gateway", gateway: "fd00:200::1", want: net.ParseIP("fd00:200::1").To16()},
		{name: "uncompressed", gateway: "fd00:0200:0000:0000:0000:0000:0000:0001", want: net.ParseIP("fd00:200::1").To16()},
		{name: "link local", gateway: "fe80::1", want: net.ParseIP("fe80::1").To16()},
		{name: "empty", gateway: ""},
		{name: "garbage", gateway: "not-an-address"},
		{name: "ipv4", gateway: "192.168.100.1"},
		{name: "ipv4 mapped", gateway: "::ffff:192.168.100.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := &NFTablesRules{tapInterface: "tap0"}
			rules.SetGatewayIPv6(tt.gateway)

			if tt.want == nil {
				require.Nil(t, rules.gatewayIPv6)
				return
			}
			require.Len(t, rules.gatewayIPv6, 16)
			require.Equal(t, tt.want, []byte(rules.gatewayIPv6))
		})
	}
}

// TestBuildIPv6PlanOrdersRedirectsBeforeDrops pins the whole ip6 rule plan: the
// redirects (DNAT to the gateway) come first, the residual drops last, and the
// catch-all passthrough redirect is ordered after the 80/443 redirects.
func TestBuildIPv6PlanOrdersRedirectsBeforeDrops(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	plan := rules.buildIPv6Plan()

	var kinds []ipv6RuleKind
	var chains []string
	for _, planned := range plan {
		kinds = append(kinds, planned.kind)
		chains = append(chains, planned.chain)
	}

	require.Equal(t, []ipv6RuleKind{
		// nat/prerouting: 80, 443, DNS (UDP and TCP), then the catch-all TCP
		ipv6DNATHTTP, ipv6DNATHTTPS, ipv6DNATDNS, ipv6DNATDNS, ipv6DNATPassthrough,
		// prerouting filter: link plumbing, the redirected traffic, then the drop
		ipv6AcceptNeighbor, ipv6AcceptNeighbor, ipv6AcceptGateway, ipv6DropIngress,
		// forward: both directions dropped (defense in depth)
		ipv6DropIngress, ipv6DropEgress,
		// output: only the gateway may speak to the guest, then the drop
		ipv6AcceptNeighbor, ipv6AcceptNeighbor, ipv6AcceptGateway, ipv6DropEgress,
		// input: the DNS forwarder port on the gateway address
		ipv6AcceptDNS, ipv6AcceptDNS,
	}, kinds)
	require.Equal(t, []string{
		chainPreNAT, chainPreNAT, chainPreNAT, chainPreNAT, chainPreNAT,
		chainPreFilterV6, chainPreFilterV6, chainPreFilterV6, chainPreFilterV6,
		chainFwd, chainFwd,
		chainOutput, chainOutput, chainOutput, chainOutput,
		chainInput, chainInput,
	}, chains)

	lastRedirect, firstDrop := -1, len(plan)
	for i, planned := range plan {
		if planned.kind.redirects() {
			lastRedirect = i
		}
		if planned.kind.drops() && i < firstDrop {
			firstDrop = i
		}
	}
	require.GreaterOrEqual(t, lastRedirect, 0, "the plan must contain redirect rules")
	require.Less(t, lastRedirect, firstDrop,
		"every redirect must precede every residual drop, or redirected traffic is dropped")

	// The catch-all TCP redirect must not shadow the port-specific ones.
	var passthroughAt, httpsAt, dnsAt int
	for i, planned := range plan {
		switch planned.kind {
		case ipv6DNATHTTPS:
			httpsAt = i
		case ipv6DNATDNS:
			dnsAt = i
		case ipv6DNATPassthrough:
			passthroughAt = i
		}
	}
	require.Greater(t, passthroughAt, httpsAt)
	require.Greater(t, passthroughAt, dnsAt, "DNS must be redirected before the catch-all TCP rule")

	// Each filter chain ends in its residual drop.
	require.True(t, plan[firstDrop].kind.drops())
	lastChain := ""
	for _, planned := range plan {
		switch planned.chain {
		case chainPreFilterV6, chainFwd, chainOutput:
			lastChain = planned.chain
		}
	}
	require.Equal(t, chainOutput, lastChain)
	require.True(t, plan[len(plan)-3].kind.drops(), "the output chain must end in the egress drop")
}

// TestBuildIPv6PlanWithoutPassthroughOrDNSForwarder covers the optional
// redirects: no passthrough port means no catch-all, no DNS forwarder port
// means no DNS redirect and no input chain.
func TestBuildIPv6PlanWithoutPassthroughOrDNSForwarder(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 0, 0)
	plan := rules.buildIPv6Plan()

	var kinds []ipv6RuleKind
	chains := map[string]int{}
	for _, planned := range plan {
		kinds = append(kinds, planned.kind)
		chains[planned.chain]++
	}

	require.Equal(t, []ipv6RuleKind{
		ipv6DNATHTTP, ipv6DNATHTTPS,
		ipv6AcceptNeighbor, ipv6AcceptNeighbor, ipv6AcceptGateway, ipv6DropIngress,
		ipv6DropIngress, ipv6DropEgress,
		ipv6AcceptNeighbor, ipv6AcceptNeighbor, ipv6AcceptGateway, ipv6DropEgress,
	}, kinds)
	require.NotContains(t, kinds, ipv6DNATPassthrough)
	require.NotContains(t, kinds, ipv6DNATDNS)
	require.NotContains(t, chains, chainInput, "no DNS forwarder port means no input chain")
}

// TestBuildIPv6PlanWithoutGatewayFailsClosed pins the fail-closed default: a
// caller that never set an IPv6 gateway gets no redirect and no accept rule, only
// the previous drop rules — exactly today's behaviour for IPv6.
func TestBuildIPv6PlanWithoutGatewayFailsClosed(t *testing.T) {
	rules := NewNFTablesRules("tap0", "192.168.100.1", 8080, 8443, 15000, nil)
	rules.SetDNSForwarderPort(5353)

	// An IPv4 gateway must not arm the IPv6 redirect either.
	rules.SetGatewayIPv6("192.168.100.1")

	plan := rules.buildIPv6Plan()

	var kinds []ipv6RuleKind
	var chains []string
	for _, planned := range plan {
		kinds = append(kinds, planned.kind)
		chains = append(chains, planned.chain)
		require.Truef(t, planned.kind.drops(), "only drop rules are allowed without a gateway, got %s", planned.kind)
	}

	require.Equal(t, []ipv6RuleKind{
		ipv6DropIngress, ipv6DropIngress, ipv6DropEgress, ipv6DropEgress,
	}, kinds)
	require.Equal(t, []string{chainPreFilterV6, chainFwd, chainFwd, chainOutput}, chains)
}

// TestBuildIPv6DNATRuleShape decodes the redirect rule the kernel receives: the
// TAP ingress match, the protocol and port match, the 16-byte gateway address in
// register 1, the proxy port in register 2, and a DNAT expression in the IPv6
// family.
func TestBuildIPv6DNATRuleShape(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	gateway := net.ParseIP("fd00:200::1").To16()

	got := rules.buildIPv6DNATRule(unix.IPPROTO_TCP, 80, 8080)
	require.Len(t, got, 9)

	requireMetaExpr(t, got[0], expr.MetaKeyIIFNAME)
	requireCmpExpr(t, got[1], ifname("tap0"))
	requireMetaExpr(t, got[2], expr.MetaKeyL4PROTO)
	requireCmpExpr(t, got[3], []byte{unix.IPPROTO_TCP})
	requirePayloadExpr(t, got[4], expr.PayloadBaseTransportHeader, 2, 2)
	requireCmpExpr(t, got[5], binaryutil.BigEndian.PutUint16(80))
	requireImmediateExpr(t, got[6], 1, gateway)
	require.Len(t, got[6].(*expr.Immediate).Data, 16, "an IPv6 address is 16 bytes")
	requireImmediateExpr(t, got[7], 2, binaryutil.BigEndian.PutUint16(8080))
	requireNATExpr(t, got[8], 1, 2)
}

// TestBuildIPv6DNATRuleCatchAllMatchesAnyPort covers the passthrough redirect:
// without a source port match it applies to every TCP port, which is what makes
// it a catch-all. It must stay ordered after the port-specific redirects (see
// TestBuildIPv6PlanOrdersRedirectsBeforeDrops).
func TestBuildIPv6DNATRuleCatchAllMatchesAnyPort(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	gateway := net.ParseIP("fd00:200::1").To16()

	got := rules.buildIPv6DNATRule(unix.IPPROTO_TCP, 0, 15000)
	require.Len(t, got, 7)

	requireMetaExpr(t, got[0], expr.MetaKeyIIFNAME)
	requireCmpExpr(t, got[1], ifname("tap0"))
	requireMetaExpr(t, got[2], expr.MetaKeyL4PROTO)
	requireCmpExpr(t, got[3], []byte{unix.IPPROTO_TCP})
	requireImmediateExpr(t, got[4], 1, gateway)
	requireImmediateExpr(t, got[5], 2, binaryutil.BigEndian.PutUint16(15000))
	requireNATExpr(t, got[6], 1, 2)
}

// TestBuildIPv6DNSDNATRuleCoversUDPAndTCP pins the DNS redirect: port 53 of both
// transports goes to the DNS forwarder port on the gateway address.
func TestBuildIPv6DNSDNATRuleCoversUDPAndTCP(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	gateway := net.ParseIP("fd00:200::1").To16()

	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		got := rules.buildIPv6DNATRule(proto, 53, 5353)
		require.Len(t, got, 9)
		requireCmpExpr(t, got[3], []byte{proto})
		requireCmpExpr(t, got[5], binaryutil.BigEndian.PutUint16(53))
		requireImmediateExpr(t, got[6], 1, gateway)
		requireImmediateExpr(t, got[7], 2, binaryutil.BigEndian.PutUint16(5353))
		requireNATExpr(t, got[8], 1, 2)
	}
}

// TestBuildIPv6GatewayAcceptRule pins the accept that keeps redirected traffic
// alive past the residual drop: guest packets addressed to the gateway on
// ingress, and gateway-sourced packets toward the guest on egress.
func TestBuildIPv6GatewayAcceptRule(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	gateway := net.ParseIP("fd00:200::1").To16()

	ingress := rules.buildIPv6GatewayAcceptRule(true)
	require.Len(t, ingress, 5)
	requireMetaExpr(t, ingress[0], expr.MetaKeyIIFNAME)
	requireCmpExpr(t, ingress[1], ifname("tap0"))
	requirePayloadExpr(t, ingress[2], expr.PayloadBaseNetworkHeader, 24, 16) // IPv6 destination
	requireCmpExpr(t, ingress[3], gateway)
	requireVerdictExpr(t, ingress[4], expr.VerdictAccept)

	egress := rules.buildIPv6GatewayAcceptRule(false)
	require.Len(t, egress, 5)
	requireMetaExpr(t, egress[0], expr.MetaKeyOIFNAME)
	requireCmpExpr(t, egress[1], ifname("tap0"))
	requirePayloadExpr(t, egress[2], expr.PayloadBaseNetworkHeader, 8, 16) // IPv6 source
	requireCmpExpr(t, egress[3], gateway)
	requireVerdictExpr(t, egress[4], expr.VerdictAccept)
}

// TestBuildIPv6NDPAcceptRule pins the only other traffic the ip6 table lets
// through: ICMPv6 neighbor solicitation/advertisement, without which the guest
// could not resolve the gateway's link-layer address.
func TestBuildIPv6NDPAcceptRule(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)

	for _, tt := range []struct {
		name      string
		isIngress bool
		icmpType  byte
	}{
		{name: "neighbor solicitation ingress", isIngress: true, icmpType: icmpv6NeighborSolicit},
		{name: "neighbor advertisement ingress", isIngress: true, icmpType: icmpv6NeighborAdvert},
		{name: "neighbor solicitation egress", isIngress: false, icmpType: icmpv6NeighborSolicit},
		{name: "neighbor advertisement egress", isIngress: false, icmpType: icmpv6NeighborAdvert},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := rules.buildIPv6NDPAcceptRule(tt.isIngress, tt.icmpType)
			require.Len(t, got, 7)

			wantMeta := expr.MetaKeyOIFNAME
			if tt.isIngress {
				wantMeta = expr.MetaKeyIIFNAME
			}
			requireMetaExpr(t, got[0], wantMeta)
			requireCmpExpr(t, got[1], ifname("tap0"))
			requireMetaExpr(t, got[2], expr.MetaKeyL4PROTO)
			requireCmpExpr(t, got[3], []byte{unix.IPPROTO_ICMPV6})
			requirePayloadExpr(t, got[4], expr.PayloadBaseTransportHeader, 0, 1)
			requireCmpExpr(t, got[5], []byte{tt.icmpType})
			requireVerdictExpr(t, got[6], expr.VerdictAccept)
		})
	}
}

// TestBuildIPv6DNSInputAcceptRule pins the input accept for the DNS forwarder
// port: after the DNAT the query is addressed to the gateway on the forwarder
// port, which is what this rule admits.
func TestBuildIPv6DNSInputAcceptRule(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	gateway := net.ParseIP("fd00:200::1").To16()

	got := rules.buildIPv6DNSInputAcceptRule(unix.IPPROTO_UDP, 5353)
	require.Len(t, got, 9)

	requireMetaExpr(t, got[0], expr.MetaKeyIIFNAME)
	requireCmpExpr(t, got[1], ifname("tap0"))
	requireMetaExpr(t, got[2], expr.MetaKeyL4PROTO)
	requireCmpExpr(t, got[3], []byte{unix.IPPROTO_UDP})
	requirePayloadExpr(t, got[4], expr.PayloadBaseNetworkHeader, 24, 16)
	requireCmpExpr(t, got[5], gateway)
	requirePayloadExpr(t, got[6], expr.PayloadBaseTransportHeader, 2, 2)
	requireCmpExpr(t, got[7], binaryutil.BigEndian.PutUint16(5353))
	requireVerdictExpr(t, got[8], expr.VerdictAccept)
}
