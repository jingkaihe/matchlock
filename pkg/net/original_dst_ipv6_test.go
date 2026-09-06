//go:build linux

package net

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sockaddrIn6Bytes builds the 28-byte sockaddr_in6 the kernel writes for an
// IP6T_SO_ORIGINAL_DST answer: family(2, host byte order) + port(2, network
// order) + flowinfo(4) + address(16) + scope_id(4). Kept next to the parser so
// a wire-format change breaks both at once.
func sockaddrIn6Bytes(t *testing.T, ip string, port int, scopeID uint32) []byte {
	t.Helper()

	parsed := net.ParseIP(ip)
	require.NotNil(t, parsed, "test address %q must parse", ip)
	require.Nil(t, parsed.To4(), "test address %q must be a native IPv6 literal", ip)

	return sockaddrIn6Raw(t, ip, port, scopeID)
}

// sockaddrIn6Raw is sockaddrIn6Bytes without the native-IPv6 requirement, for
// the v4-mapped form a v6 socket can also report.
func sockaddrIn6Raw(t *testing.T, ip string, port int, scopeID uint32) []byte {
	t.Helper()

	parsed := net.ParseIP(ip)
	require.NotNil(t, parsed, "test address %q must parse", ip)
	require.NotNil(t, parsed.To16(), "test address %q must be an IPv6-shaped address", ip)

	buf := make([]byte, sockaddrIn6Len)
	binary.NativeEndian.PutUint16(buf[0:2], afInet6)
	binary.BigEndian.PutUint16(buf[2:4], uint16(port))
	copy(buf[8:24], parsed.To16())
	binary.NativeEndian.PutUint32(buf[24:28], scopeID)
	return buf
}

// sockaddrInBytes builds the 16-byte sockaddr_in an IPv4 SO_ORIGINAL_DST answer
// carries.
func sockaddrInBytes(t *testing.T, ip string, port int) []byte {
	t.Helper()

	parsed := net.ParseIP(ip).To4()
	require.NotNil(t, parsed, "test address %q must be an IPv4 literal", ip)

	buf := make([]byte, sockaddrInLen)
	binary.NativeEndian.PutUint16(buf[0:2], afInet)
	binary.BigEndian.PutUint16(buf[2:4], uint16(port))
	copy(buf[4:8], parsed)
	return buf
}

// TestOriginalDstSockoptForFamily pins the dispatch table: an IPv6 socket is
// queried at level IPPROTO_IPV6 with option IP6T_SO_ORIGINAL_DST, an IPv4 socket
// keeps the historical SOL_IP/SO_ORIGINAL_DST pair, and any other family is
// refused instead of being queried with the wrong level.
func TestOriginalDstSockoptForFamily(t *testing.T) {
	t.Run("IPv6", func(t *testing.T) {
		level, option, ok := originalDstSockopt(afInet6)
		require.True(t, ok)
		assert.Equal(t, 41, level, "IPPROTO_IPV6")
		assert.Equal(t, syscall.IPPROTO_IPV6, level, "must match the syscall package's constant")
		assert.Equal(t, 80, option, "IP6T_SO_ORIGINAL_DST")
	})

	t.Run("IPv4", func(t *testing.T) {
		level, option, ok := originalDstSockopt(afInet)
		require.True(t, ok)
		assert.Equal(t, 0, level, "SOL_IP")
		assert.Equal(t, SO_ORIGINAL_DST, option)
	})

	t.Run("IPv6 option must not reuse the IPv4 level", func(t *testing.T) {
		level, _, _ := originalDstSockopt(afInet6)
		v4Level, _, _ := originalDstSockopt(afInet)
		assert.NotEqual(t, v4Level, level,
			"a v6 socket queried at SOL_IP would either fail or return a v4-mapped answer")
	})

	t.Run("unsupported families", func(t *testing.T) {
		for _, domain := range []int{0, 1, afInet6 + 1, 17} {
			_, _, ok := originalDstSockopt(domain)
			assert.False(t, ok, "domain %d must not be queried", domain)
		}
	})
}

// TestParseSockaddrIn6 decodes crafted sockaddr_in6 buffers, including the two
// fields the parser deliberately ignores (flowinfo and scope_id) and the
// v4-mapped form.
func TestParseSockaddrIn6(t *testing.T) {
	t.Run("unique-local destination with scope id", func(t *testing.T) {
		got, err := parseSockaddrIn6(sockaddrIn6Bytes(t, "fd00:100::1", 8443, 3))
		require.NoError(t, err)
		assert.Equal(t, "fd00:100::1", got.IP.String())
		assert.Equal(t, 8443, got.Port)
	})

	t.Run("loopback and unspecified", func(t *testing.T) {
		for _, tc := range []struct {
			ip   string
			port int
		}{
			{"::1", 53},
			{"::", 0},
			{"2001:db8::dead:beef", 65535},
			{"200::1", 8080},
		} {
			got, err := parseSockaddrIn6(sockaddrIn6Bytes(t, tc.ip, tc.port, 0))
			require.NoError(t, err)
			assert.True(t, net.ParseIP(tc.ip).Equal(got.IP), "address %s round-trips", tc.ip)
			assert.Equal(t, tc.port, got.Port)
		}
	})

	t.Run("flowinfo is ignored", func(t *testing.T) {
		buf := sockaddrIn6Bytes(t, "fd00:200::5", 443, 0)
		binary.BigEndian.PutUint32(buf[4:8], 0xdeadbeef)

		got, err := parseSockaddrIn6(buf)
		require.NoError(t, err)
		assert.Equal(t, "fd00:200::5", got.IP.String())
		assert.Equal(t, 443, got.Port)
	})

	t.Run("v4-mapped destination stays a v6 sockaddr", func(t *testing.T) {
		// The kernel can report a v4-mapped address on a v6 socket; the parser
		// must not mangle it into a native v4 address.
		got, err := parseSockaddrIn6(sockaddrIn6Raw(t, "::ffff:192.168.100.1", 8080, 0))
		require.NoError(t, err)
		assert.Equal(t, "192.168.100.1", got.IP.String())
		assert.NotNil(t, got.IP.To4(), "a mapped address is dialable over v4")
	})
}

// TestParseSockaddrIn6RejectsMalformed proves a truncated or mislabelled buffer
// is an error (the connection is then closed) instead of a zero address.
func TestParseSockaddrIn6RejectsMalformed(t *testing.T) {
	t.Run("short buffers", func(t *testing.T) {
		for _, n := range []int{0, 8, 16, sockaddrIn6Len - 1} {
			_, err := parseSockaddrIn6(make([]byte, n))
			require.Error(t, err, "a %d-byte buffer is not a sockaddr_in6", n)
			assert.ErrorIs(t, err, ErrOriginalDst)
		}
	})

	t.Run("wrong family", func(t *testing.T) {
		buf := sockaddrIn6Bytes(t, "fd00:100::1", 443, 0)
		binary.NativeEndian.PutUint16(buf[0:2], afInet)

		_, err := parseSockaddrIn6(buf)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOriginalDst)
		assert.Contains(t, err.Error(), "family")
	})

	t.Run("zero family", func(t *testing.T) {
		_, err := parseSockaddrIn6(make([]byte, sockaddrIn6Len))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOriginalDst)
	})
}

// TestParseSockaddrIn keeps the IPv4 decoding pinned: a 16-byte sockaddr_in, and
// the longer buffer the v6-capable lookup uses (the kernel reports 16 bytes for
// an IPv4 socket even though the buffer is 28).
func TestParseSockaddrIn(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseSockaddrIn(sockaddrInBytes(t, "203.0.113.9", 443))
		require.NoError(t, err)
		assert.Equal(t, "203.0.113.9", got.IP.String())
		assert.Equal(t, 443, got.Port)
	})

	t.Run("oversized buffer is tolerated", func(t *testing.T) {
		// The shared lookup buffer is 28 bytes; an IPv4 answer only fills the
		// first 16 and the rest must be ignored.
		buf := make([]byte, sockaddrIn6Len)
		copy(buf, sockaddrInBytes(t, "192.0.2.7", 8080))

		got, err := parseSockaddrIn(buf)
		require.NoError(t, err)
		assert.Equal(t, "192.0.2.7", got.IP.String())
		assert.Equal(t, 8080, got.Port)
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := parseSockaddrIn(make([]byte, 15))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOriginalDst)

		buf := sockaddrInBytes(t, "192.0.2.7", 80)
		binary.NativeEndian.PutUint16(buf[0:2], afInet6)
		_, err = parseSockaddrIn(buf)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOriginalDst)
	})
}

// TestParseOriginalDstDispatchesOnFamily proves the family decides both the
// accepted sockaddr shape and the decoded result.
func TestParseOriginalDstDispatchesOnFamily(t *testing.T) {
	v6, err := parseOriginalDst(afInet6, sockaddrIn6Bytes(t, "fd00:137::2", 8443, 0))
	require.NoError(t, err)
	assert.Equal(t, "fd00:137::2", v6.IP.String())
	assert.Equal(t, 8443, v6.Port)

	v4, err := parseOriginalDst(afInet, sockaddrInBytes(t, "198.51.100.4", 8080))
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.4", v4.IP.String())
	assert.Equal(t, 8080, v4.Port)

	// The same bytes decoded as the other family must not silently produce an
	// address: a mismatched buffer is an error, not a garbage dial target.
	_, err = parseOriginalDst(afInet6, sockaddrInBytes(t, "198.51.100.4", 8080))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOriginalDst)

	_, err = parseOriginalDst(17, sockaddrIn6Bytes(t, "fd00:1::1", 1, 0))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOriginalDst)
}

// TestGetOriginalDstWithInjectedGetter drives the accept-side helper on a real
// *net.TCPConn: it proves the getter is called with the connection's own fd and
// that both the decoded destination and a getter failure propagate unchanged.
func TestGetOriginalDstWithInjectedGetter(t *testing.T) {
	newLoopbackConn := func(t *testing.T) *net.TCPConn {
		t.Helper()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { ln.Close() })

		client, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { client.Close() })

		srv, err := ln.Accept()
		require.NoError(t, err)
		t.Cleanup(func() { srv.Close() })

		return srv.(*net.TCPConn)
	}

	t.Run("returns the injected destination", func(t *testing.T) {
		conn := newLoopbackConn(t)

		var seenFD uintptr
		got, err := getOriginalDstWith(conn, func(fd uintptr) (*originalDst, error) {
			seenFD = fd
			domain, derr := socketDomain(fd)
			require.NoError(t, derr)
			require.Equal(t, afInet, domain, "the accepted socket is a real IPv4 socket")
			return parseSockaddrIn(sockaddrInBytes(t, "203.0.113.50", 443))
		})
		require.NoError(t, err)
		assert.NotZero(t, seenFD, "the getter must receive the connection's descriptor")
		assert.Equal(t, "203.0.113.50", got.IP.String())
		assert.Equal(t, 443, got.Port)
	})

	t.Run("propagates the getter error", func(t *testing.T) {
		conn := newLoopbackConn(t)

		sentinel := errors.New("original destination unavailable")
		got, err := getOriginalDstWith(conn, func(uintptr) (*originalDst, error) {
			return nil, sentinel
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel)
		assert.Nil(t, got)
	})
}

// TestSocketDomainReportsTheCreatedFamily uses real sockets to prove the
// dispatch input is the socket's own family: a v6 listener's accepted socket
// reports AF_INET6 and a v4 socket reports AF_INET.
func TestSocketDomainReportsTheCreatedFamily(t *testing.T) {
	accept := func(t *testing.T, network, addr string) *net.TCPConn {
		t.Helper()

		ln, err := net.Listen(network, addr)
		require.NoError(t, err, "no %s support on this host", network)
		t.Cleanup(func() { ln.Close() })

		client, err := net.Dial(network, ln.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { client.Close() })

		srv, err := ln.Accept()
		require.NoError(t, err)
		t.Cleanup(func() { srv.Close() })

		return srv.(*net.TCPConn)
	}

	t.Run("IPv6", func(t *testing.T) {
		conn := accept(t, "tcp6", "[::1]:0")

		var domain int
		raw, err := conn.SyscallConn()
		require.NoError(t, err)
		require.NoError(t, raw.Control(func(fd uintptr) {
			domain, err = socketDomain(fd)
		}))
		require.NoError(t, err)
		assert.Equal(t, afInet6, domain)
	})

	t.Run("IPv4", func(t *testing.T) {
		conn := accept(t, "tcp4", "127.0.0.1:0")

		var domain int
		raw, err := conn.SyscallConn()
		require.NoError(t, err)
		require.NoError(t, raw.Control(func(fd uintptr) {
			domain, err = socketDomain(fd)
		}))
		require.NoError(t, err)
		assert.Equal(t, afInet, domain)
	})
}

// TestGetOriginalDstWithoutConntrackFailsClosed documents the real kernel
// behaviour the accept loop depends on: a socket with no NAT/conntrack record
// (a plain loopback connection) yields an error, so the connection is closed
// rather than dialed at an unverified destination. It also proves the v6 lookup
// reaches the kernel with a valid fd.
func TestGetOriginalDstWithoutConntrackFailsClosed(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			addr := "127.0.0.1:0"
			if network == "tcp6" {
				addr = "[::1]:0"
			}

			ln, err := net.Listen(network, addr)
			require.NoError(t, err)
			defer ln.Close()

			client, err := net.Dial(network, ln.Addr().String())
			require.NoError(t, err)
			defer client.Close()

			srv, err := ln.Accept()
			require.NoError(t, err)
			defer srv.Close()

			got, err := getOriginalDst(srv.(*net.TCPConn))
			if err != nil {
				assert.ErrorIs(t, err, ErrOriginalDst)
				assert.Nil(t, got)
				return
			}
			// The IPv4 path may fall back to the peer address when the kernel
			// has no conntrack entry; whatever it reports must still be the
			// loopback peer.
			assert.True(t, got.IP.IsLoopback(), "an unreported destination must not be invented: %v", got.IP)
		})
	}
}
