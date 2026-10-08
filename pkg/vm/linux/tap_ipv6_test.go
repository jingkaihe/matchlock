//go:build linux

package linux

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// addrAttr is one decoded rtattr (rtnetlink attribute) of an address request.
type addrAttr struct {
	typ  uint16
	len  int
	addr net.IP
}

// decodedAddrRequest is the decoded shape of a serialized RTM_NEWADDR message.
type decodedAddrRequest struct {
	length    int
	msgType   uint16
	flags     uint16
	seq       uint32
	family    uint8
	prefixLen uint8
	ifaFlags  uint8
	scope     uint8
	index     uint32
	attrs     []addrAttr
}

// decodeAddrMessage decodes the netlink message buildIPv6AddrMessage produces so
// tests can assert on every field without a kernel round trip. It is
// deliberately strict: a message whose length or attribute walk does not line up
// fails the test instead of decoding a truncated view.
func decodeAddrMessage(t *testing.T, msg []byte) decodedAddrRequest {
	t.Helper()
	require.GreaterOrEqual(t, len(msg), syscall.SizeofNlMsghdr+syscall.SizeofIfAddrmsg, "message shorter than its headers")

	out := decodedAddrRequest{
		length:  int(netlinkByteOrder.Uint32(msg[0:4])),
		msgType: netlinkByteOrder.Uint16(msg[4:6]),
		flags:   netlinkByteOrder.Uint16(msg[6:8]),
		seq:     netlinkByteOrder.Uint32(msg[8:12]),
	}
	require.Equal(t, int(netlinkByteOrder.Uint32(msg[0:4])), len(msg), "nlmsg_len must describe the whole message")

	off := syscall.SizeofNlMsghdr
	out.family = msg[off]
	out.prefixLen = msg[off+1]
	out.ifaFlags = msg[off+2]
	out.scope = msg[off+3]
	out.index = netlinkByteOrder.Uint32(msg[off+4 : off+8])
	off += syscall.SizeofIfAddrmsg

	for off < len(msg) {
		require.GreaterOrEqual(t, len(msg)-off, 4, "truncated rtattr header at offset %d", off)
		attrLen := int(netlinkByteOrder.Uint16(msg[off : off+2]))
		attrType := netlinkByteOrder.Uint16(msg[off+2 : off+4])
		require.GreaterOrEqual(t, attrLen, 4, "rtattr length %d too small at offset %d", attrLen, off)
		require.LessOrEqual(t, off+attrLen, len(msg), "rtattr at offset %d overruns the message", off)

		attr := addrAttr{typ: attrType, len: attrLen}
		if payload := msg[off+4 : off+attrLen]; len(payload) == net.IPv6len {
			attr.addr = net.IP(append([]byte(nil), payload...))
		}
		out.attrs = append(out.attrs, attr)
		off += attrLen
	}
	require.Equal(t, len(msg), off, "attribute walk must consume the whole message")
	return out
}

func TestBuildIPv6AddrMessage(t *testing.T) {
	cases := []struct {
		name      string
		ifindex   int
		addr      string
		prefixLen int
	}{
		{name: "per-VM ULA gateway", ifindex: 7, addr: "fd00:100::1", prefixLen: 64},
		{name: "another ULA octet", ifindex: 42, addr: "fd00:137::1", prefixLen: 64},
		{name: "host route prefix", ifindex: 3, addr: "fd00:254::2", prefixLen: 128},
		{name: "public prefix", ifindex: 9, addr: "2001:db8::1", prefixLen: 48},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantIP := net.ParseIP(tc.addr)
			require.NotNil(t, wantIP)

			msg, err := buildIPv6AddrMessage(tc.ifindex, wantIP, tc.prefixLen)
			require.NoError(t, err)

			got := decodeAddrMessage(t, msg)
			assert.Equal(t, uint16(syscall.RTM_NEWADDR), got.msgType, "message type")
			assert.Equal(t, uint32(ipv6AddrRequestSeq), got.seq, "sequence number")

			// NLM_F_REQUEST|NLM_F_ACK make it a netlink request that is
			// answered; NLM_F_CREATE|NLM_F_REPLACE are what `ip -6 addr
			// replace` sends, so the address is installed whether or not it is
			// already present. NLM_F_EXCL must stay clear: Start re-applies the
			// TAP configuration, and an exclusive request would fail with
			// EEXIST on the second pass.
			assert.NotZero(t, got.flags&syscall.NLM_F_REQUEST, "must be a netlink request")
			assert.NotZero(t, got.flags&syscall.NLM_F_ACK, "must ask for an ACK")
			assert.NotZero(t, got.flags&syscall.NLM_F_CREATE, "must be able to create the address")
			assert.NotZero(t, got.flags&syscall.NLM_F_REPLACE, "must replace an address that is already present")
			assert.Zero(t, got.flags&syscall.NLM_F_EXCL, "must not be exclusive: the TAP is configured twice")

			assert.Equal(t, uint8(syscall.AF_INET6), got.family, "address family")
			assert.Equal(t, uint8(tc.prefixLen), got.prefixLen, "prefix length")
			assert.Equal(t, uint32(tc.ifindex), got.index, "interface index")
			assert.Zero(t, got.scope, "a ULA is a global-scope address")

			// IFA_F_NODAD is required, not cosmetic: the TAP has no carrier
			// until the VMM attaches, so DAD cannot complete and the address
			// would stay TENTATIVE - and a tentative address cannot be bound,
			// which is exactly what the interception proxy does with the TAP's
			// IPv6 gateway before the VM starts (bind: cannot assign requested
			// address).
			assert.Equal(t, uint8(ifaFlagNoDAD), got.ifaFlags, "IFA_F_NODAD must be set")

			require.Len(t, got.attrs, 2, "IFA_LOCAL and IFA_ADDRESS")
			assert.Equal(t, uint16(syscall.IFA_LOCAL), got.attrs[0].typ)
			assert.Equal(t, uint16(syscall.IFA_ADDRESS), got.attrs[1].typ)
			for i, attr := range got.attrs {
				assert.Equal(t, ifaddrAttrLen, attr.len, "attribute %d length", i)
				assert.True(t, wantIP.To16().Equal(attr.addr), "attribute %d address: got %v want %v", i, attr.addr, wantIP)
			}
		})
	}
}

func TestBuildIPv6AddrMessageRejectsBadInput(t *testing.T) {
	v6 := net.ParseIP("fd00:100::1")
	cases := []struct {
		name      string
		ifindex   int
		addr      net.IP
		prefixLen int
		want      error
	}{
		{name: "zero ifindex", ifindex: 0, addr: v6, prefixLen: 64, want: ErrInvalidInterfaceIndex},
		{name: "negative ifindex", ifindex: -1, addr: v6, prefixLen: 64, want: ErrInvalidInterfaceIndex},
		{name: "nil address", ifindex: 5, addr: nil, prefixLen: 64, want: ErrInvalidIPv6Address},
		{name: "IPv4 address", ifindex: 5, addr: net.ParseIP("192.168.100.1"), prefixLen: 24, want: ErrInvalidIPv6Address},
		{name: "prefix above 128", ifindex: 5, addr: v6, prefixLen: 129, want: ErrInvalidIPv6PrefixLength},
		{name: "negative prefix", ifindex: 5, addr: v6, prefixLen: -1, want: ErrInvalidIPv6PrefixLength},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := buildIPv6AddrMessage(tc.ifindex, tc.addr, tc.prefixLen)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Nil(t, msg, "no message may be built from invalid input")
		})
	}
}

// TestConfigureInterfaceIPv6SendsBuiltMessage covers the whole configuration
// path (interface lookup, CIDR parsing, message building, send, link-up) with
// the two kernel-facing dependencies injected, so it runs without CAP_NET_ADMIN.
// "lo" always exists on Linux, but its addresses/flags are never touched.
func TestConfigureInterfaceIPv6SendsBuiltMessage(t *testing.T) {
	lo, err := net.InterfaceByName("lo")
	require.NoError(t, err, "the loopback interface must exist to exercise the lookup path")

	var sent []byte
	var linkUpName string
	send := func(msg []byte) error {
		sent = msg
		return nil
	}
	linkUp := func(name string) error {
		linkUpName = name
		return nil
	}

	require.NoError(t, configureInterfaceIPv6("lo", "fd00:9::1/64", send, linkUp))

	want, err := buildIPv6AddrMessage(lo.Index, net.ParseIP("fd00:9::1"), 64)
	require.NoError(t, err)
	require.Equal(t, want, sent, "the configured address must be the one parsed from the CIDR")
	require.Equal(t, "lo", linkUpName, "the link must be brought up on the named interface")
}

func TestConfigureInterfaceIPv6Errors(t *testing.T) {
	sendOK := func([]byte) error { return nil }
	linkUpOK := func(string) error { return nil }

	t.Run("unknown interface", func(t *testing.T) {
		err := configureInterfaceIPv6("ml-no-such-iface", "fd00:9::1/64", sendOK, linkUpOK)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInterfaceNotFound)
	})

	t.Run("malformed CIDR", func(t *testing.T) {
		err := configureInterfaceIPv6("lo", "fd00:9::1/64/extra", sendOK, linkUpOK)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidCIDR)
	})

	t.Run("IPv4 CIDR", func(t *testing.T) {
		err := configureInterfaceIPv6("lo", "192.168.100.1/24", sendOK, linkUpOK)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidCIDR)
	})

	t.Run("netlink failure stops before link-up", func(t *testing.T) {
		linkUpCalled := false
		err := configureInterfaceIPv6("lo", "fd00:9::1/64",
			func([]byte) error { return errx.Wrap(ErrNetlinkAddrAdd, syscall.EPERM) },
			func(string) error { linkUpCalled = true; return nil })
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNetlinkAddrAdd)
		assert.False(t, linkUpCalled, "the link must not be brought up when the address could not be installed")
	})

	t.Run("netlink failure under a Create sentinel", func(t *testing.T) {
		err := configureInterfaceIPv6("lo", "fd00:9::1/64",
			func([]byte) error { return errx.Wrap(ErrNetlinkAddrAdd, syscall.EPERM) },
			linkUpOK)
		// This is the wrapping Create adds, so the failure carries both
		// sentinels: the TAP-configuration operation and the netlink op.
		wrapped := errx.Wrap(ErrTAPConfigureIPv6, err)
		assert.ErrorIs(t, wrapped, ErrTAPConfigureIPv6)
		assert.ErrorIs(t, wrapped, ErrNetlinkAddrAdd)
	})

	t.Run("link-up failure is reported", func(t *testing.T) {
		err := configureInterfaceIPv6("lo", "fd00:9::1/64", sendOK,
			func(string) error { return errx.Wrap(ErrSIOCSIFFLAGS, syscall.EPERM) })
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSIOCSIFFLAGS)
	})
}

// TestApplyIPv6AddrReportsKernelRejection drives the real netlink round trip
// (socket, send, ACK parse) against an interface index that cannot exist. The
// kernel's rejection must surface as ErrNetlinkAddrAdd; the exact errno differs
// between a privileged (ENODEV) and an unprivileged (EPERM) caller, so only the
// sentinel is asserted.
func TestApplyIPv6AddrReportsKernelRejection(t *testing.T) {
	msg, err := buildIPv6AddrMessage(0x7fffffff, net.ParseIP("fd00:9::1"), 64)
	require.NoError(t, err)

	err = applyIPv6Addr(msg)
	require.Error(t, err, "a nonexistent interface index must be rejected by the kernel")
	assert.ErrorIs(t, err, ErrNetlinkAddrAdd)
	assert.False(t, errors.Is(err, syscall.EEXIST), "EEXIST must not be the reported failure")
}
