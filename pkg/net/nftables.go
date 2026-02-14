//go:build linux

package net

import (
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/jingkaihe/matchlock/internal/errx"
	"golang.org/x/sys/unix"
)

const (
	tableName   = "matchlock"
	chainPreNAT = "prerouting"
	chainFwd    = "forward"
)

type NFTablesRules struct {
	tapInterface    string
	gatewayIP       net.IP
	httpPort        uint16
	httpsPort       uint16
	passthroughPort uint16
	dnsProxyPort    uint16
	interceptL7     bool
	dnsServers      []net.IP
	conn            *nftables.Conn
	table           *nftables.Table
}

func NewNFTablesRules(tapInterface, gatewayIP string, httpPort, httpsPort, passthroughPort int, dnsServers []string, dnsProxyPort int, interceptL7 bool) *NFTablesRules {
	var dnsIPs []net.IP
	for _, s := range dnsServers {
		if ip := net.ParseIP(s).To4(); ip != nil {
			dnsIPs = append(dnsIPs, ip)
		}
	}
	return &NFTablesRules{
		tapInterface:    tapInterface,
		gatewayIP:       net.ParseIP(gatewayIP).To4(),
		httpPort:        uint16(httpPort),
		httpsPort:       uint16(httpsPort),
		passthroughPort: uint16(passthroughPort),
		dnsProxyPort:    uint16(dnsProxyPort),
		interceptL7:     interceptL7,
		dnsServers:      dnsIPs,
	}
}

func (r *NFTablesRules) Setup() error {
	conn, err := nftables.New()
	if err != nil {
		return errx.Wrap(ErrNFTablesConn, err)
	}
	r.conn = conn

	r.table = conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   tableName + "_" + r.tapInterface,
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

	if r.interceptL7 {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildDNATRule(80, r.httpPort, unix.IPPROTO_TCP),
		})

		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildDNATRule(443, r.httpsPort, unix.IPPROTO_TCP),
		})
	}

	// In Tailscale mode, forward guest DNS to the host-side DNS proxy so
	// lookups are made from the host network namespace (required for quad100).
	if r.dnsProxyPort > 0 {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildDNATRule(53, r.dnsProxyPort, unix.IPPROTO_UDP),
		})
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildDNATRule(53, r.dnsProxyPort, unix.IPPROTO_TCP),
		})
	} else {
		// Allow DNS (UDP port 53) from the VM to configured resolvers only.
		// Must come before the UDP drop rule. Restricting destination IPs
		// prevents DNS tunneling to attacker-controlled nameservers.
		for _, dnsIP := range r.dnsServers {
			conn.AddRule(&nftables.Rule{
				Table: r.table,
				Chain: fwdChain,
				Exprs: r.buildUDPDNSAcceptRule(dnsIP),
			})
		}
	}

	if r.passthroughPort > 0 {
		conn.AddRule(&nftables.Rule{
			Table: r.table,
			Chain: preChain,
			Exprs: r.buildCatchAllDNATRule(r.passthroughPort),
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

	if err := conn.Flush(); err != nil {
		return errx.Wrap(ErrNFTablesApply, err)
	}

	return nil
}

func (r *NFTablesRules) buildDNATRule(srcPort, dstPort uint16, protocol byte) []expr.Any {
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
			Data:     []byte{protocol},
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

// buildUDPDNSAcceptRule accepts UDP port 53 traffic from the TAP interface
// to a specific destination IP (e.g. 8.8.8.8).
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
		// Match destination IP
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
		// Match destination port 53
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

	tableName := tableName + "_" + r.tapInterface
	for _, t := range tables {
		if t.Name == tableName && t.Family == nftables.TableFamilyIPv4 {
			r.conn.DelTable(t)
			break
		}
	}

	return r.conn.Flush()
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
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

	return nil
}

func (n *NFTablesNAT) Cleanup() error {
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
