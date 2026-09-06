//go:build linux

package net

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireIPv6Loopback skips a test when the host has no usable IPv6 loopback
// (the whole dual-stack path is then untestable here). A skip is NOT a pass:
// the IPv6 suite is expected to run on the Linux gate host.
func requireIPv6Loopback(t *testing.T) {
	t.Helper()

	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no usable IPv6 loopback in this environment: %v", err)
	}
	ln.Close()
}

// ipv6OrigDstGetter returns a getter that reports the socket's own family (via
// the real SO_DOMAIN syscall) and answers with crafted sockaddr_in6 bytes. It
// exists because a loopback socket has no conntrack record, so the kernel
// answers a real IP6T_SO_ORIGINAL_DST with ENOENT; the privileged test
// (original_dst_ipv6_privileged_test.go) covers the real DNAT path.
func ipv6OrigDstGetter(crafted []byte, observed chan<- int) originalDstGetter {
	return func(fd uintptr) (*originalDst, error) {
		domain, err := socketDomain(fd)
		if err != nil {
			return nil, err
		}
		if observed != nil {
			observed <- domain
		}
		if domain != afInet6 {
			return nil, fmt.Errorf("accepted socket family = %d, want AF_INET6", domain)
		}
		return parseSockaddrIn6(crafted)
	}
}

// TestSelectDialIPForFamily pins the family-aware dial choice and, with it, that
// an IPv6-intercepted connection never lands on an unrelated IPv4 literal.
func TestSelectDialIPForFamily(t *testing.T) {
	v6 := net.ParseIP("2001:db8::1")
	v4 := net.ParseIP("203.0.113.5")
	mapped := net.ParseIP("::ffff:203.0.113.5")

	t.Run("IPv4 preference is unchanged", func(t *testing.T) {
		assert.Equal(t, v4, selectDialIPForFamily([]net.IP{v6, v4}, false))
		assert.Equal(t, v4, selectDialIP([]net.IP{v6, v4}), "selectDialIP keeps preferring IPv4")
		assert.Equal(t, mapped, selectDialIPForFamily([]net.IP{mapped, v4}, false),
			"an IPv4-mapped address counts as IPv4")
	})

	t.Run("IPv6 destinations dial IPv6", func(t *testing.T) {
		assert.Equal(t, v6, selectDialIPForFamily([]net.IP{v4, v6}, true),
			"an IPv6 original destination must dial the verified IPv6 literal")
	})

	t.Run("falls back when the preferred family is absent", func(t *testing.T) {
		assert.Equal(t, v4, selectDialIPForFamily([]net.IP{v4}, true), "IPv4-only set")
		assert.Equal(t, v6, selectDialIPForFamily([]net.IP{v6}, false), "IPv6-only set")
		assert.Equal(t, v6, selectDialIP([]net.IP{v6}), "selectDialIP's IPv6-only fallback is preserved")
	})

	t.Run("single address always wins", func(t *testing.T) {
		assert.Equal(t, v6, selectDialIPForFamily([]net.IP{v6}, true))
		assert.Equal(t, v4, selectDialIPForFamily([]net.IP{v4}, false))
	})
}

// TestDstPrefersV6 pins which intercepted destinations switch the dial to IPv6:
// only a literal IPv6 original destination does; a hostname, an IPv4 literal or
// an unparseable value keeps the previous IPv4 preference.
func TestDstPrefersV6(t *testing.T) {
	for _, tc := range []struct {
		dst  string
		want bool
	}{
		{"2001:db8::1", true},
		{"fd00:100::2", true},
		{"::1", true},
		{"127.0.0.1", false},
		{"203.0.113.5", false},
		{"example.com", false},
		{"[2001:db8::1]", false},
		{"", false},
		// A bare v6 literal can never carry a port suffix (the port travels
		// separately, like the policy engine's hostAndPort), so every parseable
		// v6 string counts as v6.
		{"2001:db8::1:8080", true},
	} {
		assert.Equal(t, tc.want, dstPrefersV6(tc.dst), "destination %q", tc.dst)
	}
}

// TestAcceptLoopReadsIPv6OriginalDst drives the real accept path on a real IPv6
// socket: the accepted connection's fd must be an AF_INET6 socket and the
// original destination recovered for it must reach the handler as an IPv6
// literal, which is what the policy check sees.
func TestAcceptLoopReadsIPv6OriginalDst(t *testing.T) {
	requireIPv6Loopback(t)

	ln, err := net.Listen("tcp6", "[::1]:0")
	require.NoError(t, err)
	defer ln.Close()

	crafted := sockaddrIn6Bytes(t, "fd00:100::7", 8080, 0)
	domains := make(chan int, 1)

	tp := &TransparentProxy{originalDst: ipv6OrigDstGetter(crafted, domains)}

	type observed struct {
		ip   string
		port int
	}
	got := make(chan observed, 1)

	tp.wg.Add(1) // acceptLoop owns one WaitGroup slot (as Start arranges)
	go tp.acceptLoop(ln, func(conn net.Conn, dstIP string, dstPort int) {
		got <- observed{dstIP, dstPort}
		conn.Close()
	})

	client, err := net.Dial("tcp6", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	select {
	case o := <-got:
		assert.Equal(t, "fd00:100::7", o.ip, "the handler must receive the v6 original destination")
		assert.Equal(t, 8080, o.port)
	case <-time.After(5 * time.Second):
		require.Fail(t, "the IPv6 accept loop never dispatched the connection")
	}

	select {
	case d := <-domains:
		assert.Equal(t, afInet6, d, "the accepted socket must be a real AF_INET6 socket")
	case <-time.After(time.Second):
		assert.Fail(t, "the original-destination getter was never called")
	}
}

// TestAcceptLoopReadsIPv4OriginalDst is the IPv4 counterpart: an accepted IPv4
// connection must be dispatched through SOL_IP/SO_ORIGINAL_DST (level 0,
// option 80) and never through the IPv6 pair. The getter below observes that
// dispatch on the REAL socket (SO_DOMAIN is a real syscall on the accepted fd)
// and then performs the real lookup; a kernel with no conntrack record either
// answers with the socket's LOCAL address — for an undiverted connection
// exactly where it arrived — or, in a namespace without that fallback (a
// container), refuses to answer, in which case the crafted sentinel stands in
// so the dispatch assertion stays deterministic on every kernel.
func TestAcceptLoopReadsIPv4OriginalDst(t *testing.T) {
	const sentinelIP = "198.51.100.7"

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, lnPortStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	lnPort := mustAtoi(lnPortStr)

	crafted := sockaddrInBytes(t, sentinelIP, 8080)
	dispatches := make(chan string, 1)

	tp := &TransparentProxy{originalDst: func(fd uintptr) (*originalDst, error) {
		domain, err := socketDomain(fd)
		if err != nil {
			return nil, err
		}
		level, option, ok := originalDstSockopt(domain)
		if !ok {
			return nil, fmt.Errorf("unexpected socket family %d", domain)
		}
		dispatches <- fmt.Sprintf("domain=%d level=%d option=%d", domain, level, option)

		if got, err := getsockoptOriginalDst(fd); err == nil {
			return got, nil
		}
		return parseSockaddrIn(crafted)
	}}

	type observed struct {
		ip   string
		port int
	}
	got := make(chan observed, 1)

	tp.wg.Add(1)
	go tp.acceptLoop(ln, func(conn net.Conn, dstIP string, dstPort int) {
		got <- observed{dstIP, dstPort}
		conn.Close()
	})

	client, err := net.Dial("tcp4", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	select {
	case o := <-got:
		switch {
		case o.ip == "127.0.0.1":
			assert.Equal(t, lnPort, o.port, "the kernel's fallback reports the arrival address")
		case o.ip == sentinelIP:
			assert.Equal(t, 8080, o.port)
		default:
			assert.Fail(t, "unexpected IPv4 destination", "%s:%d", o.ip, o.port)
		}
	case <-time.After(5 * time.Second):
		require.Fail(t, "the IPv4 accept loop never dispatched the connection")
	}

	select {
	case d := <-dispatches:
		assert.Equal(t, fmt.Sprintf("domain=%d level=%d option=%d", afInet, syscall.SOL_IP, SO_ORIGINAL_DST), d,
			"an IPv4 socket must be looked up at SOL_IP/SO_ORIGINAL_DST")
	case <-time.After(time.Second):
		assert.Fail(t, "the original-destination getter was never called")
	}
}

// TestTransparentProxyBindsIPv6Listeners proves the constructor binds the same
// ports on the IPv6 gateway, ignores the feature when no IPv6 address is
// configured, and releases everything it opened when the IPv6 bind fails (no
// partial dual-stack proxy).
func TestTransparentProxyBindsIPv6Listeners(t *testing.T) {
	requireIPv6Loopback(t)

	t.Run("IPv4-only when no v6 address is configured", func(t *testing.T) {
		tp, err := NewTransparentProxy(&ProxyConfig{
			BindAddr:        "127.0.0.1",
			HTTPPort:        0,
			HTTPSPort:       0,
			PassthroughPort: 0,
			Policy:          policy.NewEngine(&api.NetworkConfig{}),
		})
		require.NoError(t, err)
		defer tp.Close()

		assert.False(t, tp.IPv6Enabled())
		assert.Empty(t, tp.BindAddrV6())
		assert.Nil(t, tp.httpListenerV6)
		require.Len(t, tp.acceptLoops(), 3, "IPv4-only: HTTP, HTTPS and passthrough")
	})

	t.Run("bind failure leaves no IPv4 listener behind", func(t *testing.T) {
		// Ask for fixed IPv4 ports so the released listeners can be re-bound
		// after the failed construction.
		ports := make([]int, 3)
		for i := range ports {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			ports[i] = ln.Addr().(*net.TCPAddr).Port
			require.NoError(t, ln.Close())
		}

		_, err := NewTransparentProxy(&ProxyConfig{
			BindAddr:        "127.0.0.1",
			BindAddrV6:      "::2", // not assigned to any interface: EADDRNOTAVAIL
			HTTPPort:        ports[0],
			HTTPSPort:       ports[1],
			PassthroughPort: ports[2],
			Policy:          policy.NewEngine(&api.NetworkConfig{}),
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrListen)

		for i, port := range ports {
			ln, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			require.NoError(t, lerr, "IPv4 listener %d (%d) was leaked by the failed dual-stack bind", i, port)
			ln.Close()
		}
	})
}

// TestTransparentProxyAcceptsIPv6OnEveryPort is the end-to-end dual-stack check:
// with an IPv6 bind address configured, the HTTP, HTTPS and passthrough ports
// all accept IPv6 connections on the same ports as IPv4 and run the shared
// policy path.
func TestTransparentProxyAcceptsIPv6OnEveryPort(t *testing.T) {
	requireIPv6Loopback(t)

	caPool, err := NewCAPool()
	require.NoError(t, err)

	events := make(chan api.Event, 16)
	tp, err := NewTransparentProxy(&ProxyConfig{
		BindAddr:        "127.0.0.1",
		BindAddrV6:      "::1",
		HTTPPort:        0,
		HTTPSPort:       0,
		PassthroughPort: 0,
		Policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"allowed.example.com"},
		}),
		Events: events,
		CAPool: caPool,
	})
	require.NoError(t, err)
	defer tp.Close()

	require.True(t, tp.IPv6Enabled())
	assert.Equal(t, "::1", tp.BindAddrV6())
	require.NotZero(t, tp.HTTPPort())
	require.NotZero(t, tp.HTTPSPort())
	require.NotZero(t, tp.PassthroughPort())
	require.Len(t, tp.acceptLoops(), 6, "both families on HTTP, HTTPS and passthrough")

	// An intercepted IPv6 connection is policed on its original v6 destination.
	crafted := sockaddrIn6Bytes(t, "2001:db8::42", 8080, 0)
	tp.originalDst = ipv6OrigDstGetter(crafted, nil)

	tp.Start()

	dial6 := func(t *testing.T, port int) net.Conn {
		t.Helper()

		conn, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)), 3*time.Second)
		require.NoError(t, err, "the IPv6 listener on port %d must accept", port)
		t.Cleanup(func() { conn.Close() })
		return conn
	}

	t.Run("HTTP", func(t *testing.T) {
		conn := dial6(t, tp.HTTPPort())

		// A destination literal that is not in allowed_hosts is refused by the
		// HTTP path (no DNS involved), proving the v6 listener runs the shared
		// interceptor.
		_, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 198.51.100.9\r\nConnection: close\r\n\r\n")
		require.NoError(t, err)

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		ev := nextBlockedEvent(t, events)
		assert.Equal(t, "198.51.100.9", ev.Network.Host)
	})

	t.Run("HTTPS", func(t *testing.T) {
		conn := dial6(t, tp.HTTPSPort())

		// No SNI: the interceptor then falls back to the ORIGINAL destination
		// (the v6 literal), which is not allowlisted, so it blocks before any
		// upstream dial. The client still completes the TLS handshake (the MITM
		// certificate is presented first) and then sees the connection close.
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		require.NoError(t, tlsConn.Handshake())

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err := tlsConn.Read(make([]byte, 1))
		assert.Error(t, err, "a blocked HTTPS destination is closed after the handshake")

		ev := nextBlockedEvent(t, events)
		assert.Equal(t, "2001:db8::42", ev.Network.Host,
			"the SNI-less HTTPS path is policed on the v6 original destination")
	})

	t.Run("passthrough", func(t *testing.T) {
		conn := dial6(t, tp.PassthroughPort())

		_, err := conn.Write([]byte("must not be relayed"))
		require.NoError(t, err)

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		assert.Error(t, err, "an unallowlisted v6 passthrough destination is refused")
		assert.Zero(t, n, "no payload may be relayed")

		ev := nextBlockedEvent(t, events)
		assert.Equal(t, "[2001:db8::42]:8080", ev.Network.Host,
			"the blocked event names the bracketed v6 original destination")
	})

	// Close must cover the IPv6 listeners: once they are shut down the v6 ports
	// stop accepting.
	httpPort := tp.HTTPPort()
	require.NoError(t, tp.Close())

	_, err = net.DialTimeout("tcp6", net.JoinHostPort("::1", strconv.Itoa(httpPort)), time.Second)
	assert.Error(t, err, "Close must close the IPv6 listeners too")
}

// nextBlockedEvent waits for the next network event and asserts it is a block.
func nextBlockedEvent(t *testing.T, events <-chan api.Event) api.Event {
	t.Helper()

	select {
	case ev := <-events:
		require.NotNil(t, ev.Network, "network event payload")
		require.True(t, ev.Network.Blocked, "expected a blocked event, got %+v", ev.Network)
		return ev
	case <-time.After(5 * time.Second):
		require.Fail(t, "expected a blocked network event")
		return api.Event{}
	}
}

// TestHandleHTTP_DualStackDialsTheDestinationFamily is the interceptor-level
// proof that the dial family follows the ORIGINAL destination: a dual-stack name
// reached over IPv6 must be dialed at its verified IPv6 literal, and the same
// name reached over IPv4 must keep dialing IPv4. Falls back to an unrelated
// address of the other family would be a silent cross-family substitution.
func TestHandleHTTP_DualStackDialsTheDestinationFamily(t *testing.T) {
	const (
		v4Answer = "203.0.113.9"
		v6Answer = "2001:db8::9"
	)

	dns := newSyntheticDNSWithAAAA(t,
		syntheticDNSConfig{"dual.test.": {{v4Answer}}},
		syntheticDNSConfig{"dual.test.": {{v6Answer}}},
	)
	dns.install()
	defer dns.uninstall()

	newInterceptor := func(t *testing.T) (*HTTPInterceptor, *atomic.Value, *atomic.Int64, net.Listener) {
		t.Helper()

		var hits atomic.Int64
		fixture := startOwnedHTTPFixture(t, &hits)
		interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"dual.test"},
		}), nil, nil)

		var dialed atomic.Value // string
		interceptor.dial = func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			return net.Dial("tcp", fixture.Addr().String())
		}
		return interceptor, &dialed, &hits, fixture
	}

	t.Run("IPv6 original destination dials the v6 answer", func(t *testing.T) {
		interceptor, dialed, hits, fixture := newInterceptor(t)
		port := localPort(t, fixture)

		resp := doInterceptedHTTP(t, interceptor, v6Answer, port, "dual.test")
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, int64(1), hits.Load())

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort(v6Answer, strconv.Itoa(port)), got,
			"an IPv6-intercepted request must dial the verified v6 literal")
		assert.GreaterOrEqual(t, dns.aAAAACount("dual.test."), 1,
			"the dual-stack name must have been resolved over AAAA")
	})

	t.Run("IPv4 original destination keeps dialing the v4 answer", func(t *testing.T) {
		interceptor, dialed, hits, fixture := newInterceptor(t)
		port := localPort(t, fixture)

		resp := doInterceptedHTTP(t, interceptor, v4Answer, port, "dual.test")
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, int64(1), hits.Load())

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort(v4Answer, strconv.Itoa(port)), got,
			"the IPv4 preference for a v4/v4-mapped destination is unchanged")
	})
}

// TestHandlePassthrough_IPv6Literals covers the passthrough policy path for IPv6
// destinations: the bare literal reaches the policy engine in the same shape as
// an IPv4 one, a listed private v6 literal is dialed literally and bracketed,
// and an unlisted private v6 literal is refused without a dial or a byte of
// relayed payload.
func TestHandlePassthrough_IPv6Literals(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	_, portStr, err := net.SplitHostPort(upstream.Addr().String())
	require.NoError(t, err)
	port := mustAtoi(portStr)

	newProxy := func(t *testing.T, cfg *api.NetworkConfig) (*TransparentProxy, *atomic.Value, chan api.Event) {
		t.Helper()

		var dialed atomic.Value // string
		events := make(chan api.Event, 10)
		tp := &TransparentProxy{
			policy: policy.NewEngine(cfg),
			events: events,
			dial: func(network, addr string) (net.Conn, error) {
				dialed.Store(addr)
				// Land on the owned echo server regardless of the requested
				// literal so the relay completes and the dial target can be
				// observed.
				return net.Dial(network, upstream.Addr().String())
			},
		}
		return tp, &dialed, events
	}

	t.Run("listed private v6 literal relays and dials the literal", func(t *testing.T) {
		tp, dialed, _ := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"fd00:7::/64"},
		})

		client, server := net.Pipe()
		defer client.Close()

		go tp.handlePassthrough(server, "fd00:7::5", port)

		msg := []byte("hello v6 passthrough")
		require.NoError(t, client.SetWriteDeadline(time.Now().Add(2*time.Second)))
		_, err := client.Write(msg)
		require.NoError(t, err)

		buf := make([]byte, len(msg))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = io.ReadFull(client, buf)
		require.NoError(t, err)
		assert.Equal(t, string(msg), string(buf), "an allow_private v6 destination must relay")

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort("fd00:7::5", portStr), got,
			"the v6 destination must be dialed as a bracketed v6 literal")
	})

	t.Run("Yggdrasil 200::/7 literal relays when listed", func(t *testing.T) {
		tp, dialed, _ := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"200::/7"},
		})

		client, server := net.Pipe()
		defer client.Close()

		go tp.handlePassthrough(server, "200::1", port)

		msg := []byte("hello yggdrasil")
		require.NoError(t, client.SetWriteDeadline(time.Now().Add(2*time.Second)))
		_, err := client.Write(msg)
		require.NoError(t, err)

		buf := make([]byte, len(msg))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = io.ReadFull(client, buf)
		require.NoError(t, err)
		assert.Equal(t, string(msg), string(buf))

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort("200::1", portStr), got)
	})

	t.Run("unlisted private v6 literal is refused without a dial", func(t *testing.T) {
		tp, dialed, events := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"10.0.0.0/8"}, // v4-only exception
		})

		client, server := net.Pipe()
		defer client.Close()

		done := make(chan struct{})
		go func() {
			tp.handlePassthrough(server, "fc00::1", 8080)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			require.Fail(t, "handlePassthrough must return at once for a blocked v6 literal")
		}

		_, loaded := dialed.Load().(string)
		assert.False(t, loaded, "a refused private v6 destination must never be dialed")

		client.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, werr := client.Write([]byte("must not be relayed"))
		assert.Error(t, werr, "a refused v6 passthrough must have closed the guest connection")

		ev := nextBlockedEvent(t, events)
		assert.Equal(t, "[fc00::1]:8080", ev.Network.Host)
	})

	t.Run("unlisted Yggdrasil literal is refused", func(t *testing.T) {
		tp, dialed, events := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
		})

		client, server := net.Pipe()
		defer client.Close()

		done := make(chan struct{})
		go func() {
			tp.handlePassthrough(server, "201:dead::1", 443)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			require.Fail(t, "handlePassthrough must return at once for a blocked 200::/7 literal")
		}

		_, loaded := dialed.Load().(string)
		assert.False(t, loaded, "an unlisted 200::/7 destination must never be dialed")

		ev := nextBlockedEvent(t, events)
		assert.Equal(t, "[201:dead::1]:443", ev.Network.Host)
	})

	t.Run("public v6 literal relays with an open policy", func(t *testing.T) {
		tp, dialed, _ := newProxy(t, &api.NetworkConfig{})

		client, server := net.Pipe()
		defer client.Close()

		go tp.handlePassthrough(server, "2001:db8::9", 8443)

		msg := []byte("open policy v6")
		require.NoError(t, client.SetWriteDeadline(time.Now().Add(2*time.Second)))
		_, err := client.Write(msg)
		require.NoError(t, err)

		buf := make([]byte, len(msg))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = io.ReadFull(client, buf)
		require.NoError(t, err)
		assert.Equal(t, string(msg), string(buf))

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort("2001:db8::9", "8443"), got)
	})
}
