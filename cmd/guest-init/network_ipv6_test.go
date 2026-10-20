//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseIPv6Cmdline writes content to a temporary cmdline file and parses it, so
// the boot-config tests exercise the real field-dispatch path.
func parseIPv6Cmdline(t *testing.T, content string) (*bootConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmdline")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return parseBootConfig(path)
}

func TestParseBootConfigIPv6Link(t *testing.T) {
	tests := []struct {
		name        string
		cmdline     string
		wantGuest   string
		wantPrefix  int
		wantGateway string
	}{
		{
			name:        "guest and gateway",
			cmdline:     "matchlock.dns=1.1.1.1 matchlock.ipv6=fd00:100::2/64,fd00:100::1",
			wantGuest:   "fd00:100::2",
			wantPrefix:  64,
			wantGateway: "fd00:100::1",
		},
		{
			name:        "alongside the IPv4 ip= argument and every other field",
			cmdline:     "console=hvc0 ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off matchlock.dns=1.1.1.1 matchlock.mtu=1500 matchlock.ipv6=fd00:137::2/64,fd00:137::1 matchlock.cpus=0.5 matchlock.no_network=0",
			wantGuest:   "fd00:137::2",
			wantPrefix:  64,
			wantGateway: "fd00:137::1",
		},
		{
			name:        "non default prefix length",
			cmdline:     "matchlock.dns=1.1.1.1 matchlock.ipv6=fd00:200:1::2/48,fd00:200:1::1",
			wantGuest:   "fd00:200:1::2",
			wantPrefix:  48,
			wantGateway: "fd00:200:1::1",
		},
		{
			name:        "full uncompressed addresses",
			cmdline:     "matchlock.dns=1.1.1.1 matchlock.ipv6=fd00:00ff:0000:0000:0000:0000:0000:0002/64,fd00:00ff:0000:0000:0000:0000:0000:0001",
			wantGuest:   "fd00:ff::2",
			wantPrefix:  64,
			wantGateway: "fd00:ff::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseIPv6Cmdline(t, tc.cmdline)
			require.NoError(t, err)
			require.NotNil(t, cfg.IPv6)
			assert.Equal(t, tc.wantGuest, cfg.IPv6.Guest.String())
			assert.Equal(t, tc.wantPrefix, cfg.IPv6.PrefixLen)
			assert.Equal(t, tc.wantGateway, cfg.IPv6.Gateway.String())
			assert.Equal(t, fmt.Sprintf("%s/%d,%s", tc.wantGuest, tc.wantPrefix, tc.wantGateway), cfg.IPv6.String())
		})
	}
}

func TestParseBootConfigIPv6LinkRejectsMalformedValues(t *testing.T) {
	// Every one of these would leave the guest without a usable IPv6 link, so
	// the boot must fail loudly rather than continue IPv4-only behind the
	// fail-closed ip6 interception table.
	tests := []struct {
		name  string
		value string
	}{
		{name: "missing gateway", value: "fd00:100::2/64"},
		{name: "empty gateway", value: "fd00:100::2/64,"},
		{name: "empty address", value: ",fd00:100::1"},
		{name: "only a comma", value: ","},
		{name: "gateway with a prefix length", value: "fd00:100::2/64,fd00:100::1/64"},
		{name: "prefix length out of range", value: "fd00:100::2/129,fd00:100::1"},
		{name: "negative prefix length", value: "fd00:100::2/-1,fd00:100::1"},
		{name: "missing prefix length", value: "fd00:100::2,fd00:100::1"},
		{name: "non numeric prefix length", value: "fd00:100::2/abc,fd00:100::1"},
		{name: "hostname instead of an address", value: "not-an-ip/64,fd00:100::1"},
		{name: "IPv4 guest address", value: "192.168.100.2/24,fd00:100::1"},
		{name: "IPv4 gateway", value: "fd00:100::2/64,192.168.100.1"},
		{name: "IPv4-mapped guest address", value: "::ffff:192.168.100.2/120,fd00:100::1"},
		{name: "IPv4-mapped gateway", value: "fd00:100::2/64,::ffff:192.168.100.1"},
		{name: "trailing garbage", value: "fd00:100::2/64,fd00:100::1,extra"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseIPv6Cmdline(t, "matchlock.dns=1.1.1.1 matchlock.ipv6="+tc.value)
			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.ErrorIs(t, err, ErrInvalidIPv6Link)
		})
	}
}

func TestParseBootConfigIPv6LinkDefaultsOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdline string
	}{
		{name: "field absent", cmdline: "matchlock.dns=1.1.1.1"},
		{name: "field empty", cmdline: "matchlock.dns=1.1.1.1 matchlock.ipv6="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseIPv6Cmdline(t, tc.cmdline)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			assert.Nil(t, cfg.IPv6, "an IPv6-less boot must configure nothing")
			assert.Nil(t, cfg.activeIPv6Link())
		})
	}
}

func TestActiveIPv6Link(t *testing.T) {
	link := ipv6Link{Guest: net.ParseIP("fd00:100::2"), PrefixLen: 64, Gateway: net.ParseIP("fd00:100::1")}

	tests := []struct {
		name string
		cfg  *bootConfig
		want bool
	}{
		{name: "link present", cfg: &bootConfig{IPv6: &link}, want: true},
		{name: "no link", cfg: &bootConfig{}, want: false},
		{name: "no network", cfg: &bootConfig{IPv6: &link, NoNetwork: true}, want: false},
		{name: "nil config", cfg: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.activeIPv6Link()
			if !tc.want {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, link, *got)
		})
	}
}

// decodeNetlinkAttrs walks the rtattr region of a built message and returns the
// attributes in wire order, requiring the region to end exactly at the message
// end (no slack, no missing bytes).
func decodeNetlinkAttrs(t *testing.T, msg []byte, off int) []struct {
	Type    uint16
	Payload []byte
} {
	t.Helper()
	var attrs []struct {
		Type    uint16
		Payload []byte
	}
	for off < len(msg) {
		require.GreaterOrEqual(t, len(msg)-off, 4, "truncated rtattr header at offset %d", off)
		length := int(netlinkByteOrder.Uint16(msg[off : off+2]))
		require.GreaterOrEqual(t, length, 4, "rtattr at offset %d has length %d", off, length)
		require.LessOrEqual(t, off+length, len(msg), "rtattr at offset %d overruns the message", off)
		attrs = append(attrs, struct {
			Type    uint16
			Payload []byte
		}{Type: netlinkByteOrder.Uint16(msg[off+2 : off+4]), Payload: msg[off+4 : off+length]})
		off += length
	}
	require.Equal(t, len(msg), off, "attribute region must consume the whole message")
	return attrs
}

func TestBuildGuestAddrMessage(t *testing.T) {
	tests := []struct {
		name      string
		ifindex   int
		ip        string
		prefixLen int
	}{
		{name: "guest address", ifindex: 2, ip: "fd00:100::2", prefixLen: 64},
		{name: "single interface index", ifindex: 1, ip: "fd00:200:1::2", prefixLen: 48},
		{name: "large interface index", ifindex: 65535, ip: "fd00:254::2", prefixLen: 128},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip)

			msg, err := buildGuestAddrMessage(tc.ifindex, ip, tc.prefixLen)
			require.NoError(t, err)

			// nlmsghdr (16) + ifaddrmsg (8) + IFA_LOCAL + IFA_ADDRESS (20 each).
			require.Len(t, msg, syscall.SizeofNlMsghdr+syscall.SizeofIfAddrmsg+2*netlinkAddrAttrLen)
			assert.Equal(t, uint32(len(msg)), netlinkByteOrder.Uint32(msg[0:4]), "nlmsg_len")
			assert.Equal(t, uint16(syscall.RTM_NEWADDR), netlinkByteOrder.Uint16(msg[4:6]), "nlmsg_type")
			assert.Equal(t, uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE),
				netlinkByteOrder.Uint16(msg[6:8]), "nlmsg_flags")
			assert.NotZero(t, netlinkByteOrder.Uint16(msg[6:8])&syscall.NLM_F_CREATE, "create is requested")
			assert.NotZero(t, netlinkByteOrder.Uint16(msg[6:8])&syscall.NLM_F_REPLACE, "replace is requested")
			assert.Zero(t, netlinkByteOrder.Uint16(msg[6:8])&syscall.NLM_F_EXCL, "exclusive add would break idempotency")
			assert.Equal(t, uint32(netlinkRequestSeq), netlinkByteOrder.Uint32(msg[8:12]), "nlmsg_seq")
			assert.Zero(t, netlinkByteOrder.Uint32(msg[12:16]), "nlmsg_pid is filled in by the kernel")

			off := syscall.SizeofNlMsghdr
			assert.Equal(t, uint8(syscall.AF_INET6), msg[off], "ifa_family")
			assert.Equal(t, uint8(tc.prefixLen), msg[off+1], "ifa_prefixlen")
			// IFA_F_NODAD: the guest link is point to point, so no duplicate can
			// exist and the address must be usable the moment it is installed -
			// a TENTATIVE address is refused as a source, which would break the
			// guest's first IPv6 connections right after boot.
			assert.Equal(t, uint8(ifaFlagNoDAD), msg[off+2], "ifa_flags must carry IFA_F_NODAD")
			assert.Equal(t, uint8(ipv6GlobalScope), msg[off+3], "ifa_scope")
			assert.Equal(t, uint32(tc.ifindex), netlinkByteOrder.Uint32(msg[off+4:off+8]), "ifa_index")

			attrs := decodeNetlinkAttrs(t, msg, off+syscall.SizeofIfAddrmsg)
			require.Len(t, attrs, 2)
			assert.Equal(t, uint16(syscall.IFA_LOCAL), attrs[0].Type)
			assert.Equal(t, uint16(syscall.IFA_ADDRESS), attrs[1].Type)
			for _, attr := range attrs {
				assert.Len(t, attr.Payload, net.IPv6len)
				assert.Equal(t, ip.To16(), net.IP(attr.Payload))
			}
		})
	}
}

func TestBuildGuestAddrMessageRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		ifindex   int
		ip        net.IP
		prefixLen int
	}{
		{name: "zero interface index", ifindex: 0, ip: net.ParseIP("fd00:100::2"), prefixLen: 64},
		{name: "negative interface index", ifindex: -1, ip: net.ParseIP("fd00:100::2"), prefixLen: 64},
		{name: "nil address", ifindex: 2, ip: nil, prefixLen: 64},
		{name: "IPv4 address", ifindex: 2, ip: net.ParseIP("192.168.100.2"), prefixLen: 24},
		{name: "prefix length above 128", ifindex: 2, ip: net.ParseIP("fd00:100::2"), prefixLen: 129},
		{name: "negative prefix length", ifindex: 2, ip: net.ParseIP("fd00:100::2"), prefixLen: -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := buildGuestAddrMessage(tc.ifindex, tc.ip, tc.prefixLen)
			require.Error(t, err)
			assert.Nil(t, msg)
			assert.ErrorIs(t, err, ErrInvalidIPv6Link)
		})
	}
}

func TestBuildGuestRouteMessage(t *testing.T) {
	tests := []struct {
		name    string
		ifindex int
		gateway string
	}{
		{name: "gateway on the tap", ifindex: 2, gateway: "fd00:100::1"},
		{name: "single interface index", ifindex: 1, gateway: "fd00:137::1"},
		{name: "uncompressed gateway", ifindex: 9, gateway: "fd00:00ff:0000:0000:0000:0000:0000:0001"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateway := net.ParseIP(tc.gateway)
			require.NotNil(t, gateway)

			msg, err := buildGuestRouteMessage(tc.ifindex, gateway)
			require.NoError(t, err)

			// nlmsghdr (16) + rtmsg (12) + RTA_DST + RTA_GATEWAY (20 each) +
			// RTA_OIF (8). This is the exact shape (and 76-byte size)
			// `ip -6 route add default via <gateway> dev <iface>` sends.
			require.Len(t, msg, syscall.SizeofNlMsghdr+syscall.SizeofRtMsg+2*netlinkAddrAttrLen+netlinkUint32AttrLen)
			assert.Equal(t, uint32(len(msg)), netlinkByteOrder.Uint32(msg[0:4]), "nlmsg_len")
			assert.Equal(t, uint16(syscall.RTM_NEWROUTE), netlinkByteOrder.Uint16(msg[4:6]), "nlmsg_type")
			assert.Equal(t, uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE),
				netlinkByteOrder.Uint16(msg[6:8]), "nlmsg_flags")
			assert.Zero(t, netlinkByteOrder.Uint16(msg[6:8])&syscall.NLM_F_EXCL, "exclusive add would break idempotency")
			assert.Equal(t, uint32(netlinkRequestSeq), netlinkByteOrder.Uint32(msg[8:12]), "nlmsg_seq")

			off := syscall.SizeofNlMsghdr
			assert.Equal(t, uint8(syscall.AF_INET6), msg[off], "rtm_family")
			assert.Zero(t, msg[off+1], "rtm_dst_len: ::/0")
			assert.Zero(t, msg[off+2], "rtm_src_len")
			assert.Zero(t, msg[off+3], "rtm_tos")
			assert.Equal(t, uint8(syscall.RT_TABLE_MAIN), msg[off+4], "rtm_table")
			assert.Equal(t, uint8(syscall.RTPROT_BOOT), msg[off+5], "rtm_protocol")
			assert.Equal(t, uint8(ipv6GlobalScope), msg[off+6], "rtm_scope")
			assert.Equal(t, uint8(syscall.RTN_UNICAST), msg[off+7], "rtm_type")
			assert.Zero(t, netlinkByteOrder.Uint32(msg[off+8:off+12]), "rtm_flags")

			attrs := decodeNetlinkAttrs(t, msg, off+syscall.SizeofRtMsg)
			require.Len(t, attrs, 3)

			assert.Equal(t, uint16(syscall.RTA_DST), attrs[0].Type, "the default route still carries RTA_DST")
			assert.Equal(t, net.IPv6zero, net.IP(attrs[0].Payload))

			assert.Equal(t, uint16(syscall.RTA_GATEWAY), attrs[1].Type)
			assert.Equal(t, gateway.To16(), net.IP(attrs[1].Payload))

			assert.Equal(t, uint16(syscall.RTA_OIF), attrs[2].Type)
			require.Len(t, attrs[2].Payload, 4)
			assert.Equal(t, uint32(tc.ifindex), netlinkByteOrder.Uint32(attrs[2].Payload))
		})
	}
}

func TestBuildGuestRouteMessageRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		ifindex int
		gateway net.IP
	}{
		{name: "zero interface index", ifindex: 0, gateway: net.ParseIP("fd00:100::1")},
		{name: "negative interface index", ifindex: -1, gateway: net.ParseIP("fd00:100::1")},
		{name: "nil gateway", ifindex: 2, gateway: nil},
		{name: "IPv4 gateway", ifindex: 2, gateway: net.ParseIP("192.168.100.1")},
		{name: "IPv4-mapped gateway", ifindex: 2, gateway: net.ParseIP("::ffff:192.168.100.1")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := buildGuestRouteMessage(tc.ifindex, tc.gateway)
			require.Error(t, err)
			assert.Nil(t, msg)
			assert.ErrorIs(t, err, ErrInvalidIPv6Link)
		})
	}
}

func TestConfigureGuestIPv6WithInstallsAddressThenRoute(t *testing.T) {
	device, err := net.InterfaceByName("lo")
	require.NoError(t, err)

	link := ipv6Link{Guest: net.ParseIP("fd00:100::2"), PrefixLen: 64, Gateway: net.ParseIP("fd00:100::1")}

	var sent [][]byte
	send := func(msg []byte) error {
		sent = append(sent, msg)
		return nil
	}

	require.NoError(t, configureGuestIPv6With("lo", link, send))
	require.Len(t, sent, 2, "the address and the default route are both installed")

	wantAddr, err := buildGuestAddrMessage(device.Index, link.Guest, link.PrefixLen)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(wantAddr, sent[0]), "first request installs the guest address")

	wantRoute, err := buildGuestRouteMessage(device.Index, link.Gateway)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(wantRoute, sent[1]), "second request installs the ::/0 route via the gateway")
}

func TestConfigureGuestIPv6WithErrors(t *testing.T) {
	link := ipv6Link{Guest: net.ParseIP("fd00:100::2"), PrefixLen: 64, Gateway: net.ParseIP("fd00:100::1")}
	sendErr := errors.New("netlink refused")

	t.Run("unknown interface sends nothing", func(t *testing.T) {
		var sends int
		err := configureGuestIPv6With("matchlock-no-such-iface", link, func([]byte) error {
			sends++
			return nil
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrConfigureGuestIPv6)
		assert.Zero(t, sends)
	})

	t.Run("unusable guest address sends nothing", func(t *testing.T) {
		// A link whose address cannot be encoded must fail before the kernel is
		// asked to install anything, so a half-configured link can never exist.
		var sends int
		bad := ipv6Link{Guest: net.ParseIP("192.168.100.2"), PrefixLen: 24, Gateway: link.Gateway}
		err := configureGuestIPv6With("lo", bad, func([]byte) error {
			sends++
			return nil
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidIPv6Link)
		assert.Zero(t, sends)
	})

	t.Run("address failure stops before the route", func(t *testing.T) {
		var sends int
		err := configureGuestIPv6With("lo", link, func([]byte) error {
			sends++
			return sendErr
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, sendErr)
		assert.Equal(t, 1, sends)
	})

	t.Run("route failure is reported", func(t *testing.T) {
		var sends int
		err := configureGuestIPv6With("lo", link, func([]byte) error {
			sends++
			if sends == 2 {
				return sendErr
			}
			return nil
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, sendErr)
		assert.Equal(t, 2, sends)
	})
}

// TestApplyNetlinkRequestReachesTheKernel drives the real socket/bind/send/ACK
// path against the running kernel. The request is deliberately for an interface
// that does not exist (and this process is unprivileged), so the kernel must
// answer with an error rather than accept it: what is under test is that the
// round trip completes and the reply is parsed, not the verdict.
func TestApplyNetlinkRequestReachesTheKernel(t *testing.T) {
	msg, err := buildGuestRouteMessage(999999, net.ParseIP("fd00:100::1"))
	require.NoError(t, err)

	err = applyNetlinkRequest(msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConfigureGuestIPv6)
	t.Logf("kernel verdict on an unusable request: %v", err)
}

// requireIPv6Privileges gates the tests that install addresses and routes on a
// real interface. They need CAP_NET_ADMIN, which this environment reaches by
// running the compiled test binary in an ephemeral privileged container (see
// AGENTS.md); a skip here is not a pass.
func requireIPv6Privileges(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("MATCHLOCK_PRIVILEGED_TESTS") != "1" {
		t.Skip("requires root (CAP_NET_ADMIN) and MATCHLOCK_PRIVILEGED_TESTS=1 to configure a real interface")
	}
}

// TestConfigureGuestIPv6InstallsAddressAndRoute is the real-kernel counterpart of
// the builder tests: it configures the guest address and the ::/0 route through
// the gateway on a scratch veth pair, checks both landed, proves re-application
// is idempotent, and removes everything again. The device is created by the test
// itself, so no host interface is ever touched; run it in an ephemeral
// privileged container (AGENTS.md recipe). Like the address and route it
// installs, cleanup is verified.
func TestConfigureGuestIPv6InstallsAddressAndRoute(t *testing.T) {
	requireIPv6Privileges(t)

	const (
		iface = "mlv6-test"
		peer  = "mlv6-peer"
	)
	// A veth pair is a plain ethernet-like device, which is what eth0 is in the
	// guest: unlike lo, the kernel accepts a default route via an on-link
	// gateway on it.
	runIP(t, "link", "add", iface, "type", "veth", "peer", "name", peer)
	require.NoError(t, setInterfaceUp(iface), "bring the scratch device up")
	t.Cleanup(func() {
		// Best effort: the assertions above already verify the netlink cleanup,
		// this only removes the device the test created.
		if path, err := exec.LookPath("ip"); err == nil {
			_ = exec.Command(path, "link", "del", iface).Run()
		}
	})

	link := ipv6Link{Guest: net.ParseIP("fd00:2f0::2"), PrefixLen: 64, Gateway: net.ParseIP("fd00:2f0::1")}
	device, err := net.InterfaceByName(iface)
	require.NoError(t, err)

	require.NoError(t, configureGuestIPv6(iface, link), "installing the guest link must succeed as root")
	require.NoError(t, configureGuestIPv6(iface, link), "re-applying the same link must be idempotent")

	assert.True(t, interfaceHasIPv6(iface, link.Guest), "guest address %s must be configured on %s", link.Guest, iface)
	assert.True(t, defaultRouteVia(iface, link.Gateway), "::/0 via %s must be installed on %s", link.Gateway, iface)

	// IFA_F_NODAD is what makes the address usable right away: the workload
	// starts as soon as the ready signal is served, which is before duplicate
	// address detection could finish on a link that has no carrier yet, and a
	// tentative address cannot be used as a source.
	ln, err := net.Listen("tcp", net.JoinHostPort(link.Guest.String(), "0"))
	require.NoError(t, err, "the guest address must be bindable immediately (IFA_F_NODAD)")
	require.NoError(t, ln.Close())

	// Remove what this test installed: first the route, then the address, using
	// the same netlink sender the production path uses (only nlmsg_type and the
	// create/replace flags differ for a delete).
	routeMsg, err := buildGuestRouteMessage(device.Index, link.Gateway)
	require.NoError(t, err)
	require.NoError(t, applyNetlinkRequest(withNetlinkType(routeMsg, syscall.RTM_DELROUTE)), "delete the default route")

	addrMsg, err := buildGuestAddrMessage(device.Index, link.Guest, link.PrefixLen)
	require.NoError(t, err)
	require.NoError(t, applyNetlinkRequest(withNetlinkType(addrMsg, syscall.RTM_DELADDR)), "delete the guest address")

	assert.False(t, interfaceHasIPv6(iface, link.Guest), "cleanup must remove the guest address")
	assert.False(t, defaultRouteVia(iface, link.Gateway), "cleanup must remove the default route")
}

// runIP provisions the scratch device for the privileged test. iproute2 is only
// used to create and destroy the test's own veth pair; the configuration under
// test always goes through the production netlink path.
func runIP(t *testing.T, args ...string) {
	t.Helper()
	path, err := exec.LookPath("ip")
	if err != nil {
		t.Skipf("iproute2 is required to provision the scratch veth pair: %v", err)
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	require.NoError(t, err, "ip %s: %s", strings.Join(args, " "), out)
}

// withNetlinkType rewrites a built create-or-replace request into its delete
// counterpart. The payload is byte-identical (the kernel matches a delete on the
// same attributes), only nlmsg_type and the create/replace flags change.
func withNetlinkType(msg []byte, msgType uint16) []byte {
	out := append([]byte(nil), msg...)
	netlinkByteOrder.PutUint16(out[4:6], msgType)
	netlinkByteOrder.PutUint16(out[6:8], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	return out
}

// interfaceHasIPv6 reports whether address is configured on iface.
func interfaceHasIPv6(iface string, address net.IP) bool {
	device, err := net.InterfaceByName(iface)
	if err != nil {
		return false
	}
	addrs, err := device.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.Equal(address) {
			return true
		}
	}
	return false
}

// defaultRouteVia reports whether /proc/net/ipv6_route holds a ::/0 entry on
// iface whose next hop is gateway. Each line is: destination (32 hex digits),
// destination prefix length, source, source prefix length, next hop, then the
// metric/refcount/use/flags words and finally the interface name.
func defaultRouteVia(iface string, gateway net.IP) bool {
	data, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 10 {
			continue
		}
		if fields[0] != strings.Repeat("0", 32) || fields[1] != "00" || fields[9] != iface {
			continue
		}
		nextHop := net.ParseIP(hexToIPv6(fields[4]))
		if nextHop != nil && nextHop.Equal(gateway) {
			return true
		}
	}
	return false
}

// hexToIPv6 renders the 32 hex digits /proc/net/ipv6_route uses as a colon
// separated IPv6 literal.
func hexToIPv6(hex string) string {
	if len(hex) != 32 {
		return ""
	}
	parts := make([]string, 0, 8)
	for i := 0; i < 32; i += 4 {
		parts = append(parts, hex[i:i+4])
	}
	return strings.Join(parts, ":")
}
