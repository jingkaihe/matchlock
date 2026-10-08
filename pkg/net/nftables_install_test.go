//go:build linux

package net

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These tests drive the real Setup path with a fake netlink dialer, so the
// netlink batch the kernel would receive (tables, chains and rules, in order)
// can be asserted without privileges and without touching the host ruleset.

// nlaTypeMask strips the NLA_F_* flags (nested, net byte order) off an
// attribute type.
const nlaTypeMask = 0x3fff

// nlAttr is one netlink attribute: a length and a type in host byte order,
// followed by the payload and padding to a 4-byte boundary.
type nlAttr struct {
	typ  uint16
	data []byte
}

type installedTable struct {
	family uint8
	name   string
}

type installedChain struct {
	table    string
	name     string
	typ      string
	hook     uint32
	priority int32
}

type installedExpr struct {
	name string
	// immediate / cmp payload (NFTA_DATA_VALUE of NFTA_IMMEDIATE_DATA /
	// NFTA_CMP_DATA)
	value []byte
	// verdict
	verdict    uint32
	hasVerdict bool
	// nat
	natType     uint32
	natFamily   uint32
	natRegAddr  uint32
	natRegProto uint32
	hasNAT      bool
}

type installedRule struct {
	table string
	chain string
	exprs []installedExpr
}

type installedNFTables struct {
	tables []installedTable
	chains []installedChain
	rules  []installedRule
}

// testSetup runs Setup against a fake dialer and returns the nftables operations
// it would have sent. The dialer echoes the batch back, which is what the
// nftables library's own tests do for a successful apply.
func testSetup(t *testing.T, rules *NFTablesRules) installedNFTables {
	t.Helper()

	var sent []netlink.Message
	rules.newConn = func() (*nftables.Conn, error) {
		return nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
			sent = append(sent, req...)
			return req, nil
		}))
	}

	require.NoError(t, rules.Setup())
	require.NotEmpty(t, sent, "Setup must send the tables in a netlink batch")

	return parseInstalledNFTables(t, sent)
}

// testSetupWithFailingApply runs Setup against a dialer that rejects the batch,
// which is how an ip6 table that cannot be installed surfaces.
func testSetupWithFailingApply(t *testing.T, rules *NFTablesRules, applyErr error) (*NFTablesRules, error) {
	t.Helper()

	rules.newConn = func() (*nftables.Conn, error) {
		return nftables.New(nftables.WithTestDial(func([]netlink.Message) ([]netlink.Message, error) {
			return nil, applyErr
		}))
	}
	return rules, rules.Setup()
}

func parseInstalledNFTables(t *testing.T, msgs []netlink.Message) installedNFTables {
	t.Helper()

	var installed installedNFTables
	for _, msg := range msgs {
		// The nftables body is an nfgenmsg (family, version, resID) followed by
		// attributes.
		require.GreaterOrEqual(t, len(msg.Data), 4)
		attrs := parseNLAttrs(t, msg.Data[4:])

		switch uint8(msg.Header.Type & 0xff) {
		case unix.NFT_MSG_NEWTABLE:
			installed.tables = append(installed.tables, installedTable{
				family: msg.Data[0],
				name:   nlAttrString(nlAttrData(attrs, unix.NFTA_TABLE_NAME)),
			})
		case unix.NFT_MSG_NEWCHAIN:
			chain := installedChain{
				table: nlAttrString(nlAttrData(attrs, unix.NFTA_CHAIN_TABLE)),
				name:  nlAttrString(nlAttrData(attrs, unix.NFTA_CHAIN_NAME)),
				typ:   nlAttrString(nlAttrData(attrs, unix.NFTA_CHAIN_TYPE)),
			}
			if hook := nlAttrData(attrs, unix.NFTA_CHAIN_HOOK); hook != nil {
				hookAttrs := parseNLAttrs(t, hook)
				chain.hook = binary.BigEndian.Uint32(nlAttrData(hookAttrs, unix.NFTA_HOOK_HOOKNUM))
				chain.priority = int32(binary.BigEndian.Uint32(nlAttrData(hookAttrs, unix.NFTA_HOOK_PRIORITY)))
			}
			installed.chains = append(installed.chains, chain)
		case unix.NFT_MSG_NEWRULE:
			rule := installedRule{
				table: nlAttrString(nlAttrData(attrs, unix.NFTA_RULE_TABLE)),
				chain: nlAttrString(nlAttrData(attrs, unix.NFTA_RULE_CHAIN)),
			}
			for _, elem := range parseNLAttrs(t, nlAttrData(attrs, unix.NFTA_RULE_EXPRESSIONS)) {
				rule.exprs = append(rule.exprs, parseInstalledExpr(t, elem.data))
			}
			installed.rules = append(installed.rules, rule)
		}
	}
	return installed
}

func parseInstalledExpr(t *testing.T, data []byte) installedExpr {
	t.Helper()

	attrs := parseNLAttrs(t, data)
	got := installedExpr{name: nlAttrString(nlAttrData(attrs, unix.NFTA_EXPR_NAME))}
	exprData := parseNLAttrs(t, nlAttrData(attrs, unix.NFTA_EXPR_DATA))

	switch got.name {
	case "immediate":
		// The nftables library encodes a verdict as an immediate expression
		// writing NFT_REG_VERDICT, so the verdict code and a plain value share
		// the expression name and are told apart by the destination register.
		dreg := binary.BigEndian.Uint32(nlAttrData(exprData, unix.NFTA_IMMEDIATE_DREG))
		dataAttrs := parseNLAttrs(t, nlAttrData(exprData, unix.NFTA_IMMEDIATE_DATA))
		if dreg == uint32(unix.NFT_REG_VERDICT) {
			got.hasVerdict = true
			got.verdict = binary.BigEndian.Uint32(nlAttrData(
				parseNLAttrs(t, nlAttrData(dataAttrs, unix.NFTA_DATA_VERDICT)), unix.NFTA_VERDICT_CODE))
			return got
		}
		got.value = nlAttrData(dataAttrs, unix.NFTA_DATA_VALUE)
	case "cmp":
		got.value = nlAttrData(parseNLAttrs(t, nlAttrData(exprData, unix.NFTA_CMP_DATA)), unix.NFTA_DATA_VALUE)
	case "nat":
		got.hasNAT = true
		got.natType = binary.BigEndian.Uint32(nlAttrData(exprData, unix.NFTA_NAT_TYPE))
		got.natFamily = binary.BigEndian.Uint32(nlAttrData(exprData, unix.NFTA_NAT_FAMILY))
		got.natRegAddr = binary.BigEndian.Uint32(nlAttrData(exprData, unix.NFTA_NAT_REG_ADDR_MIN))
		got.natRegProto = binary.BigEndian.Uint32(nlAttrData(exprData, unix.NFTA_NAT_REG_PROTO_MIN))
	}
	return got
}

// nlAttrData returns the payload of the first attribute of the given type, or
// nil when it is absent.
func nlAttrData(attrs []nlAttr, typ uint16) []byte {
	for _, attr := range attrs {
		if attr.typ == typ {
			return attr.data
		}
	}
	return nil
}

func nlAttrString(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	return string(data)
}

// parseNLAttrs decodes a netlink attribute list. The length and type of an
// attribute are in host byte order; the payload is padded to a 4-byte boundary.
func parseNLAttrs(t *testing.T, buf []byte) []nlAttr {
	t.Helper()

	var attrs []nlAttr
	for len(buf) > 0 {
		require.GreaterOrEqual(t, len(buf), 4, "truncated netlink attribute header")
		length := int(binary.NativeEndian.Uint16(buf[0:2]))
		typ := binary.NativeEndian.Uint16(buf[2:4])
		require.GreaterOrEqual(t, length, 4, "netlink attribute shorter than its header")
		require.LessOrEqual(t, length, len(buf), "netlink attribute longer than its buffer")

		attrs = append(attrs, nlAttr{typ: typ & nlaTypeMask, data: buf[4:length]})
		buf = buf[(length+3)&^3:]
	}
	return attrs
}

// ruleSummary classifies a parsed rule for ordering assertions: its chain plus
// the purpose its expressions carry.
func ruleSummary(rule installedRule) string {
	summary := rule.chain + "/"
	for _, e := range rule.exprs {
		switch {
		case e.hasNAT:
			return summary + "dnat"
		case e.hasVerdict && e.verdict == uint32(expr.VerdictAccept):
			return summary + "accept"
		case e.hasVerdict && e.verdict == uint32(expr.VerdictDrop):
			return summary + "drop"
		}
	}
	return summary + "unknown"
}

func summaries(rules []installedRule, table string) []string {
	var out []string
	for _, rule := range rules {
		if rule.table == table {
			out = append(out, ruleSummary(rule))
		}
	}
	return out
}

func tableByName(t *testing.T, installed installedNFTables, name string) installedTable {
	t.Helper()
	for _, table := range installed.tables {
		if table.name == name {
			return table
		}
	}
	require.Failf(t, "table not found", "no table named %q in %+v", name, installed.tables)
	return installedTable{}
}

func chainByName(t *testing.T, installed installedNFTables, table, name string) installedChain {
	t.Helper()
	for _, chain := range installed.chains {
		if chain.table == table && chain.name == name {
			return chain
		}
	}
	require.Failf(t, "chain not found", "no chain %q in table %q (%+v)", name, table, installed.chains)
	return installedChain{}
}

// TestSetupInstallsIPv6InterceptionTable proves the ip6 interception table is
// part of the batch Setup applies: the IPv6 table named matchlock6_<tap>, its
// nat/prerouting and filter chains, and every rule in plan order — redirects
// first, the residual drops last. The IPv4 table is asserted alongside it, so a
// change that deletes or reorders IPv4 rules fails here too.
func TestSetupInstallsIPv6InterceptionTable(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)
	rules.dnsServers = []net.IP{net.IPv4(8, 8, 8, 8).To4()}

	installed := testSetup(t, rules)

	// Both tables are applied in the same batch (the fail-closed contract: no
	// sandbox without the ip6 rules).
	require.Equal(t, uint8(nftables.TableFamilyIPv4), tableByName(t, installed, "matchlock_tap0").family)
	require.Equal(t, uint8(nftables.TableFamilyIPv6), tableByName(t, installed, "matchlock6_tap0").family)

	// The ip6 chains: a nat chain that redirects at prerouting/dstnat, a filter
	// prerouting chain that runs after it, and the forward/input/output chains.
	natChain := chainByName(t, installed, "matchlock6_tap0", chainPreNAT)
	require.Equal(t, string(nftables.ChainTypeNAT), natChain.typ)
	require.Equal(t, uint32(unix.NF_INET_PRE_ROUTING), natChain.hook)
	require.Equal(t, int32(*nftables.ChainPriorityNATDest), natChain.priority)

	preFilter := chainByName(t, installed, "matchlock6_tap0", chainPreFilterV6)
	require.Equal(t, string(nftables.ChainTypeFilter), preFilter.typ)
	require.Equal(t, uint32(unix.NF_INET_PRE_ROUTING), preFilter.hook)
	require.Equal(t, int32(*nftables.ChainPriorityFilter), preFilter.priority)
	require.Less(t, natChain.priority, preFilter.priority,
		"the redirect must run before the chain that accepts or drops what it redirected")

	for chain, hook := range map[string]uint32{
		chainFwd:    unix.NF_INET_FORWARD,
		chainInput:  unix.NF_INET_LOCAL_IN,
		chainOutput: unix.NF_INET_LOCAL_OUT,
	} {
		got := chainByName(t, installed, "matchlock6_tap0", chain)
		require.Equal(t, string(nftables.ChainTypeFilter), got.typ)
		require.Equal(t, hook, got.hook)
	}

	// Every ip6 rule, in the order the kernel receives it.
	require.Equal(t, []string{
		"prerouting/dnat",          // TCP 80 -> HTTP proxy port
		"prerouting/dnat",          // TCP 443 -> HTTPS proxy port
		"prerouting/dnat",          // UDP 53 -> DNS forwarder port
		"prerouting/dnat",          // TCP 53 -> DNS forwarder port
		"prerouting/dnat",          // catch-all TCP -> passthrough port
		"prerouting_filter/accept", // ICMPv6 neighbor solicitation
		"prerouting_filter/accept", // ICMPv6 neighbor advertisement
		"prerouting_filter/accept", // redirected traffic (destination = gateway)
		"prerouting_filter/drop",   // residual: guest IPv6 that was not redirected
		"forward/drop",             // defense in depth, both directions
		"forward/drop",
		"output/accept", // neighbor discovery toward the guest
		"output/accept",
		"output/accept", // host traffic sourced from the gateway
		"output/drop",   // residual: any other host-emitted IPv6 toward the guest
		"input/accept",  // DNS forwarder port on the gateway (UDP)
		"input/accept",  // DNS forwarder port on the gateway (TCP)
	}, summaries(installed.rules, "matchlock6_tap0"))

	// The DNAT rules carry the IPv6 family, the 16-byte gateway address and the
	// proxy ports as registers.
	gateway := net.ParseIP("fd00:200::1").To16()
	var dnatRules []installedRule
	for _, rule := range installed.rules {
		if rule.table == "matchlock6_tap0" && ruleSummary(rule) == "prerouting/dnat" {
			dnatRules = append(dnatRules, rule)
		}
	}
	require.Len(t, dnatRules, 5)

	var gotAddresses [][]byte
	var gotPorts [][]byte
	for _, rule := range dnatRules {
		nat := rule.exprs[len(rule.exprs)-1]
		require.Equal(t, "nat", nat.name)
		require.True(t, nat.hasNAT)
		require.Equal(t, uint32(unix.NFT_NAT_DNAT), nat.natType)
		require.Equal(t, uint32(unix.NFPROTO_IPV6), nat.natFamily)
		require.Equal(t, uint32(1), nat.natRegAddr)
		require.Equal(t, uint32(2), nat.natRegProto)

		var address, port []byte
		for _, e := range rule.exprs {
			if e.name != "immediate" {
				continue
			}
			switch len(e.value) {
			case 16:
				address = e.value
			case 2:
				port = e.value
			}
		}
		require.Len(t, address, 16, "every redirect must carry the 16-byte gateway address")
		require.Len(t, port, 2, "every redirect must carry a 2-byte port")
		gotAddresses = append(gotAddresses, address)
		gotPorts = append(gotPorts, port)
	}

	for _, address := range gotAddresses {
		require.Equal(t, []byte(gateway), address)
	}
	require.Equal(t, [][]byte{
		binaryutil.BigEndian.PutUint16(8080),
		binaryutil.BigEndian.PutUint16(8443),
		binaryutil.BigEndian.PutUint16(5353),
		binaryutil.BigEndian.PutUint16(5353),
		binaryutil.BigEndian.PutUint16(15000),
	}, gotPorts)

	// IPv4 rule building is unchanged: the same summary list the previous
	// implementation produced for this configuration.
	require.Equal(t, []string{
		"prerouting/dnat", // TCP 80
		"prerouting/dnat", // TCP 443
		"prerouting/dnat", // catch-all TCP
		"prerouting/dnat", // UDP 53
		"input/accept",    // DNS forwarder port
		"output/accept",   // upstream DNS server
		"forward/accept",  // upstream DNS server over UDP
		"forward/drop",    // all other UDP
		"forward/accept",  // TAP ingress
		"forward/accept",  // TAP egress
	}, summaries(installed.rules, "matchlock_tap0"))
}

// TestSetupWithoutIPv6GatewayInstallsOnlyDrops pins the fail-closed shape for a
// caller that did not configure an IPv6 gateway: the ip6 table still exists and
// still drops everything at the TAP boundary, it just redirects nothing.
func TestSetupWithoutIPv6GatewayInstallsOnlyDrops(t *testing.T) {
	rules := NewNFTablesRules("tap0", "192.168.100.1", 8080, 8443, 15000, nil)
	rules.SetDNSForwarderPort(5353)

	installed := testSetup(t, rules)

	require.Equal(t, uint8(nftables.TableFamilyIPv6), tableByName(t, installed, "matchlock6_tap0").family)
	require.Equal(t, []string{
		"prerouting_filter/drop",
		"forward/drop",
		"forward/drop",
		"output/drop",
	}, summaries(installed.rules, "matchlock6_tap0"))

	for _, chain := range []string{chainPreNAT, chainInput} {
		for _, got := range installed.chains {
			require.Falsef(t, got.table == "matchlock6_tap0" && got.name == chain,
				"no gateway means no %q chain (nothing to redirect)", chain)
		}
	}
}

// TestSetupFailsClosedWhenIPv6TableCannotBeInstalled pins the fail-closed
// contract: the ip6 table is applied in the same batch as the IPv4 table, so if
// the batch cannot be installed Setup fails with a wrapped ErrNFTablesApply and
// the caller does not create the sandbox — it must not fall back to an IPv4-only
// firewall that leaves IPv6 unenforced.
func TestSetupFailsClosedWhenIPv6TableCannotBeInstalled(t *testing.T) {
	applyErr := errors.New("simulated ip6 table install failure")

	rules, err := testSetupWithFailingApply(t, newIPv6TestRules(t, 8080, 8443, 15000, 5353), applyErr)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrNFTablesApply)
	require.ErrorIs(t, err, applyErr)
	require.NotErrorIs(t, err, ErrNFTablesConn)
	require.NotNil(t, rules.tableV6, "the ip6 table is built and part of the failed batch")
	require.NotNil(t, rules.table)
}

// TestChainV6HooksAndPriorities pins the chain definitions directly, so a wrong
// hook or priority is caught even if the install path is not exercised.
func TestChainV6HooksAndPriorities(t *testing.T) {
	rules := &NFTablesRules{tapInterface: "tap0"}
	table := &nftables.Table{Family: nftables.TableFamilyIPv6, Name: "matchlock6_tap0"}

	tests := []struct {
		chain    string
		typ      nftables.ChainType
		hook     *nftables.ChainHook
		priority *nftables.ChainPriority
	}{
		{chain: chainPreNAT, typ: nftables.ChainTypeNAT, hook: nftables.ChainHookPrerouting, priority: nftables.ChainPriorityNATDest},
		{chain: chainPreFilterV6, typ: nftables.ChainTypeFilter, hook: nftables.ChainHookPrerouting, priority: nftables.ChainPriorityFilter},
		{chain: chainFwd, typ: nftables.ChainTypeFilter, hook: nftables.ChainHookForward, priority: nftables.ChainPriorityFilter},
		{chain: chainInput, typ: nftables.ChainTypeFilter, hook: nftables.ChainHookInput, priority: nftables.ChainPriorityFilter},
		{chain: chainOutput, typ: nftables.ChainTypeFilter, hook: nftables.ChainHookOutput, priority: nftables.ChainPriorityFilter},
	}

	for _, tt := range tests {
		t.Run(tt.chain, func(t *testing.T) {
			chain := rules.chainV6(table, tt.chain)
			require.Equal(t, tt.chain, chain.Name)
			require.Equal(t, table, chain.Table)
			require.Equal(t, tt.typ, chain.Type)
			require.NotNil(t, chain.Hooknum)
			require.NotNil(t, chain.Priority)
			require.Equal(t, *tt.hook, *chain.Hooknum)
			require.Equal(t, *tt.priority, *chain.Priority)
		})
	}
}

// TestSetupIPv6InterceptionRecordsTable guards the bookkeeping Cleanup relies on:
// the table handle must be recorded so the per-VM ip6 table can be removed.
func TestSetupIPv6InterceptionRecordsTable(t *testing.T) {
	rules := newIPv6TestRules(t, 8080, 8443, 15000, 5353)

	installed := testSetup(t, rules)

	require.NotNil(t, rules.tableV6)
	require.Equal(t, "matchlock6_tap0", rules.tableV6.Name)
	require.Equal(t, nftables.TableFamilyIPv6, rules.tableV6.Family)
	require.Len(t, installed.tables, 2, "one IPv4 table and one IPv6 table")
}
