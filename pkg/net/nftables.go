//go:build linux

package net

import (
	"fmt"
	"net"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/jingkaihe/matchlock/internal/errx"
	"golang.org/x/sys/unix"
)

const (
	tableName   = "matchlock"
	tableNameV6 = "matchlock6"
	chainPreNAT = "prerouting"
	// chainPreFilterV6 is the ip6 table's filter chain at the prerouting hook.
	// The nat chain (chainPreNAT) redirects at ChainPriorityNATDest; this one
	// runs at ChainPriorityFilter, i.e. after the redirect, which is what lets it
	// accept the redirected traffic and drop everything that was not redirected.
	// nftables requires unique chain names inside a table, so it cannot reuse
	// chainPreNAT.
	chainPreFilterV6 = "prerouting_filter"
	chainFwd         = "forward"
	chainInput       = "input"
	chainOutput      = "output"
)

// ICMPv6 neighbor discovery types accepted on the TAP link (see buildIPv6Plan).
const (
	icmpv6NeighborSolicit = 135
	icmpv6NeighborAdvert  = 136
)

type NFTablesRules struct {
	tapInterface     string
	gatewayIP        net.IP
	gatewayIPv6      net.IP
	httpPort         uint16
	httpsPort        uint16
	passthroughPort  uint16
	dnsForwarderPort uint16
	dnsServers       []net.IP
	conn             *nftables.Conn
	table            *nftables.Table
	tableV6          *nftables.Table
	// newConn opens the nftables connection; nil means nftables.New. It is a
	// test seam: it lets the ip6 table's netlink batch, and the fail-closed
	// install path, be exercised without privileges.
	newConn func() (*nftables.Conn, error)
}

func NewNFTablesRules(tapInterface, gatewayIP string, httpPort, httpsPort, passthroughPort int, dnsServers []string) *NFTablesRules {
	var dnsIPs []net.IP
	for _, s := range dnsServers {
		if ip := parseDNSIPv4(s); ip != nil {
			dnsIPs = append(dnsIPs, ip)
		}
	}
	return &NFTablesRules{
		tapInterface:    tapInterface,
		gatewayIP:       net.ParseIP(gatewayIP).To4(),
		httpPort:        uint16(httpPort),
		httpsPort:       uint16(httpsPort),
		passthroughPort: uint16(passthroughPort),
		dnsServers:      dnsIPs,
	}
}

func (r *NFTablesRules) SetDNSForwarderPort(port int) {
	r.dnsForwarderPort = uint16(port)
}

// FirewallTableName is the per-TAP IPv4 interception table (matchlock_<tap>).
func FirewallTableName(tapInterface string) string {
	return tableName + "_" + tapInterface
}

// FirewallTableV6Name is the per-TAP IPv6 interception table
// (matchlock6_<tap>). Sandbox creation records it for cleanup and reconcile
// removes it for a dead TAP, so both must agree with the table Setup installs.
func FirewallTableV6Name(tapInterface string) string {
	return tableNameV6 + "_" + tapInterface
}

// TableNameV6 is the ip6 table this rule set installs for its TAP.
func (r *NFTablesRules) TableNameV6() string {
	return FirewallTableV6Name(r.tapInterface)
}

// GatewayIPv6 is the address the ip6 table redirects guest IPv6 traffic to
// (both the proxy and the DNS forwarder listen there). It is nil when the table
// is fail-closed: no redirection, every guest IPv6 packet dropped.
func (r *NFTablesRules) GatewayIPv6() net.IP {
	return r.gatewayIPv6
}

// InterceptionPorts reports the ports the rules redirect to: the HTTP and HTTPS
// interception ports, the catch-all TCP passthrough port and the DNS forwarder
// port (0 when that redirect is not installed). The ip6 rules redirect to the
// same ports as the IPv4 ones.
func (r *NFTablesRules) InterceptionPorts() (http, https, passthrough, dns int) {
	return int(r.httpPort), int(r.httpsPort), int(r.passthroughPort), int(r.dnsForwarderPort)
}

// SetGatewayIPv6 sets the per-VM IPv6 gateway address: the TAP's IPv6 address
// that the ip6 interception table redirects guest IPv6 traffic to. An empty,
// malformed or IPv4 value leaves the table in its fail-closed shape (no
// redirection, all guest IPv6 dropped), so a caller that forgets to set it
// cannot accidentally leave an IPv6 path around the proxy.
func (r *NFTablesRules) SetGatewayIPv6(gateway string) {
	ip := net.ParseIP(gateway)
	if ip == nil || ip.To4() != nil {
		r.gatewayIPv6 = nil
		return
	}
	r.gatewayIPv6 = ip.To16()
}

func (r *NFTablesRules) openConn() (*nftables.Conn, error) {
	if r.newConn != nil {
		return r.newConn()
	}
	return nftables.New()
}

func (r *NFTablesRules) Setup() error {
	conn, err := r.openConn()
	if err != nil {
		return errx.Wrap(ErrNFTablesConn, err)
	}
	r.conn = conn

	r.table = conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   FirewallTableName(r.tapInterface),
	})

	preChain := conn.AddChain(&nftables.Chain{
		Name:     chainPreNAT,
		Table:    r.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	})

	fwdChain := conn.AddChain(&nftables.Chain{
		Name:     chainFwd,
		Table:    r.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	var inputChain *nftables.Chain
	var outputChain *nftables.Chain
	if r.dnsForwarderPort > 0 {
		inputChain = conn.AddChain(&nftables.Chain{
			Name:     chainInput,
			Table:    r.table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookInput,
			Priority: nftables.ChainPriorityFilter,
		})
		outputChain = conn.AddChain(&nftables.Chain{
			Name:     chainOutput,
			Table:    r.table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookOutput,
			Priority: nftables.ChainPriorityFilter,
		})
	}

	conn.AddRule(&nftables.Rule{
		Table: r.table,
		Chain: preChain,
		Exprs: r.buildDNATRule(80, r.httpPort),
	})

	conn.AddRule(&nftables.Rule{
		Table: r.table,
		Chain: preChain,
		Exprs: r.buildDNATRule(443, r.httpsPort),
	})

	if r.passthroughPort > 0 {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildCatchAllDNATRule(r.passthroughPort),
		})
	}

	if r.dnsForwarderPort > 0 {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildDNSForwarderDNATRule(r.dnsForwarderPort),
		})

		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: inputChain,
			Exprs: r.buildDNSForwarderInputAcceptRule(r.dnsForwarderPort),
		})

		for _, dnsIP := range r.dnsServers {
			conn.AddRule(&nftables.Rule{
				Table: r.table,
				Chain: outputChain,
				Exprs: r.buildDNSForwarderOutputAcceptRule(dnsIP),
			})
		}
	}

	for _, dnsIP := range r.dnsServers {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: fwdChain,
			Exprs: r.buildUDPDNSAcceptRule(dnsIP),
		})
	}

	// Drop all other UDP from the VM to match macOS behavior where gVisor
	// silently discards non-DNS UDP. This prevents UDP-based data exfiltration.
	conn.AddRule(&nftables.Rule{
		Table: r.table,
		Chain: fwdChain,
		Exprs: r.buildUDPDropRule(),
	})

	conn.AddRule(&nftables.Rule{
		Table: r.table,
		Chain: fwdChain,
		Exprs: r.buildForwardRule(true),
	})

	conn.AddRule(&nftables.Rule{
		Table: r.table,
		Chain: fwdChain,
		Exprs: r.buildForwardRule(false),
	})

	// The ip6 table mirrors the IPv4 interception for IPv6: it redirects guest
	// IPv6 into the same transparent proxy and DNS forwarder and drops whatever
	// it did not redirect, so a guest cannot escape the restricted policy over
	// IPv6 (e.g. by enabling an IPv6 route/address with CAP_NET_ADMIN and
	// reaching a network the proxy cannot filter). It is fail closed: the table
	// is applied in the same netlink batch as the IPv4 table, so if it cannot be
	// installed Setup returns an error and the sandbox is not created.
	r.setupIPv6Interception()

	if err := conn.Flush(); err != nil {
		return errx.Wrap(ErrNFTablesApply, err)
	}

	return nil
}

func (r *NFTablesRules) buildDNATRule(srcPort, dstPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_TCP},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(srcPort),
		},
		&expr.Immediate{
			Register: 1,
			Data:     r.gatewayIP,
		},
		&expr.Immediate{
			Register: 2,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	}
}

// buildCatchAllDNATRule redirects all TCP traffic from the TAP interface to the
// passthrough proxy port. This rule must be added after port-specific DNAT rules
// (80→HTTP, 443→HTTPS) so they match first; this catches everything else.
func (r *NFTablesRules) buildCatchAllDNATRule(dstPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_TCP},
		},
		&expr.Immediate{
			Register: 1,
			Data:     r.gatewayIP,
		},
		&expr.Immediate{
			Register: 2,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	}
}

func (r *NFTablesRules) buildForwardRule(isInput bool) []expr.Any {
	metaKey := expr.MetaKeyIIFNAME
	if !isInput {
		metaKey = expr.MetaKeyOIFNAME
	}

	return []expr.Any{
		&expr.Meta{Key: metaKey, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// ipv6RuleKind classifies one rule of the ip6 interception table. Tests assert
// on the kind — in particular that every redirect comes before the residual
// drops — as well as on the built expressions.
type ipv6RuleKind string

const (
	ipv6DNATHTTP        ipv6RuleKind = "dnat-tcp-80"
	ipv6DNATHTTPS       ipv6RuleKind = "dnat-tcp-443"
	ipv6DNATPassthrough ipv6RuleKind = "dnat-tcp-passthrough"
	ipv6DNATDNS         ipv6RuleKind = "dnat-dns-53"
	ipv6AcceptNeighbor  ipv6RuleKind = "accept-icmpv6-neighbor"
	ipv6AcceptGateway   ipv6RuleKind = "accept-gateway"
	ipv6AcceptDNS       ipv6RuleKind = "accept-dns-forwarder"
	ipv6DropIngress     ipv6RuleKind = "drop-ingress"
	ipv6DropEgress      ipv6RuleKind = "drop-egress"
)

// redirects reports whether the rule sends guest IPv6 into the proxy or the DNS
// forwarder on the gateway address.
func (k ipv6RuleKind) redirects() bool {
	switch k {
	case ipv6DNATHTTP, ipv6DNATHTTPS, ipv6DNATPassthrough, ipv6DNATDNS:
		return true
	}
	return false
}

// drops reports whether the rule belongs to the residual fail-closed drop of
// traffic the ip6 table did not redirect.
func (k ipv6RuleKind) drops() bool {
	return k == ipv6DropIngress || k == ipv6DropEgress
}

// plannedIPv6Rule is one rule of the ip6 interception table: the chain it
// belongs to, its purpose, and the expressions to install.
type plannedIPv6Rule struct {
	chain string
	kind  ipv6RuleKind
	exprs []expr.Any
}

// buildIPv6Plan builds the ordered rule plan of the per-VM ip6 interception
// table. It is pure — no privileges and no kernel involved — which is what makes
// the rule shape (family, address bytes, port immediates) and the ordering
// (redirects first, residual drops last) testable on any host.
//
// It mirrors the IPv4 table with the same chains and the same ports:
//
//	nat    prerouting         DNAT 80/443/catch-all TCP/DNS 53 to the gateway
//	filter prerouting_filter  accept what was redirected, drop the rest
//	filter forward            drop both directions (defense in depth)
//	filter output             accept the gateway's own traffic, drop the rest
//	filter input              accept the DNS forwarder port
//
// Everything that was not redirected is dropped, so an IPv6 destination the
// proxy will not filter stays unreachable. The one exception is link-local
// neighbor discovery: without it the guest could not resolve the gateway at all,
// so ICMPv6 neighbor solicitation/advertisement is accepted in both directions.
// The guest configures its address and default route statically, so no router
// solicitation/advertisement is needed.
//
// A missing gateway yields no redirect and no accept rule at all: the table
// keeps its previous fail-closed shape, in which all guest IPv6 is dropped.
func (r *NFTablesRules) buildIPv6Plan() []plannedIPv6Rule {
	var plan []plannedIPv6Rule
	add := func(chain string, kind ipv6RuleKind, exprs []expr.Any) {
		plan = append(plan, plannedIPv6Rule{chain: chain, kind: kind, exprs: exprs})
	}

	redirecting := r.gatewayIPv6 != nil

	if redirecting {
		if r.httpPort > 0 {
			add(chainPreNAT, ipv6DNATHTTP, r.buildIPv6DNATRule(unix.IPPROTO_TCP, 80, r.httpPort))
		}
		if r.httpsPort > 0 {
			add(chainPreNAT, ipv6DNATHTTPS, r.buildIPv6DNATRule(unix.IPPROTO_TCP, 443, r.httpsPort))
		}
		// DNS is redirected before the catch-all TCP rule, otherwise the
		// catch-all would swallow DNS over TCP into the passthrough port.
		if r.dnsForwarderPort > 0 {
			add(chainPreNAT, ipv6DNATDNS, r.buildIPv6DNATRule(unix.IPPROTO_UDP, 53, r.dnsForwarderPort))
			add(chainPreNAT, ipv6DNATDNS, r.buildIPv6DNATRule(unix.IPPROTO_TCP, 53, r.dnsForwarderPort))
		}
		if r.passthroughPort > 0 {
			add(chainPreNAT, ipv6DNATPassthrough, r.buildIPv6DNATRule(unix.IPPROTO_TCP, 0, r.passthroughPort))
		}
	}

	// The nat chain handed the redirected traffic to the host itself, so by the
	// time this chain runs those packets are addressed to the gateway address:
	// accept them, and drop every other guest-initiated packet.
	if redirecting {
		add(chainPreFilterV6, ipv6AcceptNeighbor, r.buildIPv6NDPAcceptRule(true, icmpv6NeighborSolicit))
		add(chainPreFilterV6, ipv6AcceptNeighbor, r.buildIPv6NDPAcceptRule(true, icmpv6NeighborAdvert))
		add(chainPreFilterV6, ipv6AcceptGateway, r.buildIPv6GatewayAcceptRule(true))
	}
	add(chainPreFilterV6, ipv6DropIngress, r.buildIPv6DropRule(true))

	// Nothing legitimately traverses the forward path: the DNAT target is the
	// host itself, so forwarded IPv6 is guest traffic that was not redirected.
	// Both directions keep the previous drop.
	add(chainFwd, ipv6DropIngress, r.buildIPv6DropRule(true))
	add(chainFwd, ipv6DropEgress, r.buildIPv6DropRule(false))

	// The host may only send IPv6 toward the guest from the gateway address
	// (the proxy and the DNS forwarder both listen there), plus neighbor
	// discovery for the TAP link itself.
	if redirecting {
		add(chainOutput, ipv6AcceptNeighbor, r.buildIPv6NDPAcceptRule(false, icmpv6NeighborSolicit))
		add(chainOutput, ipv6AcceptNeighbor, r.buildIPv6NDPAcceptRule(false, icmpv6NeighborAdvert))
		add(chainOutput, ipv6AcceptGateway, r.buildIPv6GatewayAcceptRule(false))
	}
	add(chainOutput, ipv6DropEgress, r.buildIPv6DropRule(false))

	if redirecting && r.dnsForwarderPort > 0 {
		add(chainInput, ipv6AcceptDNS, r.buildIPv6DNSInputAcceptRule(unix.IPPROTO_UDP, r.dnsForwarderPort))
		add(chainInput, ipv6AcceptDNS, r.buildIPv6DNSInputAcceptRule(unix.IPPROTO_TCP, r.dnsForwarderPort))
	}

	return plan
}

// setupIPv6Interception creates the per-VM ip6 table (matchlock6_<tap>) and
// installs the plan from buildIPv6Plan into it. Chains are created lazily from
// the plan, so a table without a gateway has no nat chain and a table without a
// DNS forwarder port has no input chain — exactly the chains its rules need.
func (r *NFTablesRules) setupIPv6Interception() {
	tableV6 := r.conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv6,
		Name:   FirewallTableV6Name(r.tapInterface),
	})
	r.tableV6 = tableV6

	chains := make(map[string]*nftables.Chain)
	for _, planned := range r.buildIPv6Plan() {
		chain, ok := chains[planned.chain]
		if !ok {
			chain = r.conn.AddChain(r.chainV6(tableV6, planned.chain))
			chains[planned.chain] = chain
		}
		r.conn.AddRule(&nftables.Rule{
			Table: tableV6,
			Chain: chain,
			Exprs: planned.exprs,
		})
	}
}

// chainV6 returns the definition of one chain of the ip6 table. Every chain is a
// base chain: the nat chain redirects at prerouting, the filter chains are
// hooked where the corresponding IPv4 chains are.
func (r *NFTablesRules) chainV6(table *nftables.Table, name string) *nftables.Chain {
	chain := &nftables.Chain{Name: name, Table: table}
	switch name {
	case chainPreNAT:
		chain.Type = nftables.ChainTypeNAT
		chain.Hooknum = nftables.ChainHookPrerouting
		chain.Priority = nftables.ChainPriorityNATDest
	case chainPreFilterV6:
		chain.Type = nftables.ChainTypeFilter
		chain.Hooknum = nftables.ChainHookPrerouting
		chain.Priority = nftables.ChainPriorityFilter
	case chainFwd:
		chain.Type = nftables.ChainTypeFilter
		chain.Hooknum = nftables.ChainHookForward
		chain.Priority = nftables.ChainPriorityFilter
	case chainInput:
		chain.Type = nftables.ChainTypeFilter
		chain.Hooknum = nftables.ChainHookInput
		chain.Priority = nftables.ChainPriorityFilter
	case chainOutput:
		chain.Type = nftables.ChainTypeFilter
		chain.Hooknum = nftables.ChainHookOutput
		chain.Priority = nftables.ChainPriorityFilter
	}
	return chain
}

// buildIPv6DNATRule redirects IPv6 traffic arriving on the TAP to a port on the
// IPv6 gateway, where the proxy or DNS forwarder listens. proto is
// unix.IPPROTO_TCP or unix.IPPROTO_UDP; srcPort 0 means every port (the
// catch-all passthrough rule).
func (r *NFTablesRules) buildIPv6DNATRule(proto byte, srcPort, dstPort uint16) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{proto},
		},
	}
	if srcPort > 0 {
		exprs = append(exprs,
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       2,
				Len:          2,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     binaryutil.BigEndian.PutUint16(srcPort),
			},
		)
	}

	return append(exprs,
		&expr.Immediate{
			Register: 1,
			Data:     r.gatewayIPv6,
		},
		&expr.Immediate{
			Register: 2,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV6,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	)
}

// buildIPv6GatewayAcceptRule accepts what the ip6 table must let through: guest
// packets the nat chain already redirected to the gateway address (isIngress),
// and host packets the gateway emits back toward the guest (!isIngress).
//
// Note that this is deliberately narrower than the IPv4 forward accept: an
// accept for the bare TAP interface would neutralize the residual drops below
// it and reopen the IPv6 path around the proxy.
func (r *NFTablesRules) buildIPv6GatewayAcceptRule(isIngress bool) []expr.Any {
	metaKey := expr.MetaKeyIIFNAME
	payloadOffset := 24 // IPv6 header destination address
	if !isIngress {
		metaKey = expr.MetaKeyOIFNAME
		payloadOffset = 8 // IPv6 header source address
	}

	return []expr.Any{
		&expr.Meta{Key: metaKey, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       uint32(payloadOffset),
			Len:          16,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     r.gatewayIPv6,
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// buildIPv6NDPAcceptRule accepts one ICMPv6 neighbor discovery type on the TAP
// link. Neighbor solicitation/advertisement is what resolves the gateway's
// link-layer address, so the link cannot work without it.
func (r *NFTablesRules) buildIPv6NDPAcceptRule(isIngress bool, icmpType byte) []expr.Any {
	metaKey := expr.MetaKeyIIFNAME
	if !isIngress {
		metaKey = expr.MetaKeyOIFNAME
	}

	return []expr.Any{
		&expr.Meta{Key: metaKey, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_ICMPV6},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       0,
			Len:          1,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{icmpType},
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// buildIPv6DNSInputAcceptRule accepts DNS that reached the host on the gateway
// address (the nat chain rewrote the destination port to the forwarder port)
// in the input hook.
func (r *NFTablesRules) buildIPv6DNSInputAcceptRule(proto byte, dstPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{proto},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       24,
			Len:          16,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     r.gatewayIPv6,
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// buildIPv6DropRule builds a rule that drops IPv6 packets whose input (isInput)
// or output interface is the TAP interface.
func (r *NFTablesRules) buildIPv6DropRule(isInput bool) []expr.Any {
	metaKey := expr.MetaKeyIIFNAME
	if !isInput {
		metaKey = expr.MetaKeyOIFNAME
	}

	return []expr.Any{
		&expr.Meta{Key: metaKey, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

func (r *NFTablesRules) buildUDPDNSAcceptRule(dstIP net.IP) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_UDP},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     dstIP.To4(),
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(53),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

func (r *NFTablesRules) buildDNSForwarderDNATRule(dstPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_UDP},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(53),
		},
		&expr.Immediate{
			Register: 1,
			Data:     r.gatewayIP,
		},
		&expr.Immediate{
			Register: 2,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	}
}

func (r *NFTablesRules) buildDNSForwarderInputAcceptRule(dstPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_UDP},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     r.gatewayIP,
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(dstPort),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

func (r *NFTablesRules) buildDNSForwarderOutputAcceptRule(dstIP net.IP) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_UDP},
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     dstIP.To4(),
		},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(53),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// buildUDPDropRule drops all UDP traffic from the TAP interface. Must be placed
// after any port-specific UDP accept rules (e.g. DNS on port 53).
func (r *NFTablesRules) buildUDPDropRule() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(r.tapInterface),
		},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{unix.IPPROTO_UDP},
		},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

func (r *NFTablesRules) Cleanup() error {
	if r.conn == nil {
		conn, err := nftables.New()
		if err != nil {
			return err
		}
		r.conn = conn
	}

	tables, err := r.conn.ListTables()
	if err != nil {
		return err
	}

	tableName := FirewallTableName(r.tapInterface)
	tableNameV6 := FirewallTableV6Name(r.tapInterface)
	for _, t := range tables {
		if t.Name == tableName && t.Family == nftables.TableFamilyIPv4 {
			r.conn.DelTable(t)
			continue
		}
		if t.Name == tableNameV6 && t.Family == nftables.TableFamilyIPv6 {
			r.conn.DelTable(t)
		}
	}

	return r.conn.Flush()
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
}

func parseDNSIPv4(server string) net.IP {
	if ip := net.ParseIP(server).To4(); ip != nil {
		return ip
	}

	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return nil
	}
	return net.ParseIP(host).To4()
}

type NFTablesNAT struct {
	tapInterface string
	conn         *nftables.Conn
	table        *nftables.Table
}

func NewNFTablesNAT(tapInterface string) *NFTablesNAT {
	return &NFTablesNAT{
		tapInterface: tapInterface,
	}
}

func (n *NFTablesNAT) Setup() error {
	conn, err := nftables.New()
	if err != nil {
		return errx.Wrap(ErrNFTablesConn, err)
	}
	n.conn = conn

	n.table = conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "matchlock_nat_" + n.tapInterface,
	})

	postChain := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    n.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	fwdChain := conn.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    n.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	conn.AddRule(&nftables.Rule{
		Table: n.table,
		Chain: postChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     ifname(n.tapInterface),
			},
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ifname(n.tapInterface),
			},
			&expr.Masq{},
		},
	})

	conn.AddRule(&nftables.Rule{
		Table: n.table,
		Chain: fwdChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ifname(n.tapInterface),
			},
			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	})

	conn.AddRule(&nftables.Rule{
		Table: n.table,
		Chain: fwdChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ifname(n.tapInterface),
			},
			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	})

	if err := conn.Flush(); err != nil {
		return errx.Wrap(ErrNFTablesApply, err)
	}

	// On Docker hosts the FORWARD policy drops the sandbox's forwarded guest
	// traffic unless the TAP is accepted in the DOCKER-USER chain. Install the
	// bidirectional accept so the guest can reach the internet. Non-fatal: if it
	// fails (e.g. not a Docker host) the sandbox still runs, just without
	// forwarded egress on such hosts.
	if err := n.installForwardAccept(); err != nil {
		fmt.Fprintf(os.Stderr, "matchlock: warning: TAP forward-accept not installed: %v\n", err)
	}

	return nil
}

func (n *NFTablesNAT) Cleanup() error {
	n.removeForwardAccept()

	if n.conn == nil {
		conn, err := nftables.New()
		if err != nil {
			return err
		}
		n.conn = conn
	}

	tables, err := n.conn.ListTables()
	if err != nil {
		return err
	}

	tableName := "matchlock_nat_" + n.tapInterface
	for _, t := range tables {
		if t.Name == tableName && t.Family == nftables.TableFamilyIPv4 {
			n.conn.DelTable(t)
			break
		}
	}

	return n.conn.Flush()
}
