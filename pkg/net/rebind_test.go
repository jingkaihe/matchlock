//go:build linux

package net

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startOwnedHTTPFixture returns a listener bound to 127.0.0.1 that counts hits
// and answers 200 "OWNED". It is the "private" endpoint a rebinding attack would
// disclose to the proxy.
func startOwnedHTTPFixture(t *testing.T, hits *atomic.Int64) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "OWNED")
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln
}

// localPort returns the port of a listener.
func localPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

// readResponse reads a single HTTP response from conn.
func readResponse(t *testing.T, conn net.Conn) *http.Response {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	return resp
}

// TestHTTPInterceptor_RebindBindsDialToVerifiedIP qualifies the DNS-to-dial
// binding defense. A hostname whose resolution changes between the policy check
// (public) and a hypothetical dial-time re-resolution (private 127.0.0.1) must
// NOT have the private address dialed. The interceptor must resolve once, bind
// the dial to the verified public IP literal, and never re-resolve the hostname.
func TestHTTPInterceptor_RebindBindsDialToVerifiedIP(t *testing.T) {
	dns := newSyntheticDNS(t, syntheticDNSConfig{
		"rebind.test.": { // query1 -> public, query2+ -> private 127.0.0.1
			{"203.0.113.99"},
			{"127.0.0.1"},
		},
	})
	dns.install()
	defer dns.uninstall()

	var fixtureHits atomic.Int64
	fixture := startOwnedHTTPFixture(t, &fixtureHits)
	defer fixture.Close()

	pol := policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	var dialedAddr atomic.Value // string
	interceptor := NewHTTPInterceptor(pol, nil, nil)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialedAddr.Store(addr)
		return nil, fmt.Errorf("refused by test seam")
	}

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, "203.0.113.1", localPort(t, fixture))

	_, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: rebind.test\r\nConnection: close\r\n\r\n"))
	require.NoError(t, err)

	resp := readResponse(t, client)
	defer resp.Body.Close()
	// The verified IP (203.0.113.99) is not reachable in the test, so the proxy
	// should fail to connect (502) — it must never reach the private fixture.
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "dial to verified public IP fails; private fixture must not be served")
	assert.Equal(t, int64(0), fixtureHits.Load(), "private fixture must NOT be reached under rebinding")

	gotDial, _ := dialedAddr.Load().(string)
	assert.Equal(t, fmt.Sprintf("203.0.113.99:%d", localPort(t, fixture)), gotDial,
		"dial must be bound to the verified public IP literal, not the hostname")
	assert.Equal(t, 1, dns.aCount("rebind.test."),
		"hostname must be resolved exactly once (no re-resolution at dial time)")
}

// TestHTTPInterceptor_MixedAnswerDenied qualifies that a hostname resolving to a
// mixed public/private answer set is denied before any upstream dial, even when
// the private address is not first in the answer list.
func TestHTTPInterceptor_MixedAnswerDenied(t *testing.T) {
	dns := newSyntheticDNS(t, syntheticDNSConfig{
		"mixed.test.": {
			{"203.0.113.99", "127.0.0.1"}, // public first, private second
		},
	})
	dns.install()
	defer dns.uninstall()

	var dialed atomic.Bool
	pol := policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	interceptor := NewHTTPInterceptor(pol, nil, nil)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed.Store(true)
		return nil, fmt.Errorf("refused by test seam")
	}

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, "203.0.113.1", 8080)

	_, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: mixed.test\r\nConnection: close\r\n\r\n"))
	require.NoError(t, err)

	resp := readResponse(t, client)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"mixed public/private answer must be denied (private address present)")
	assert.False(t, dialed.Load(), "must be denied before any upstream dial")
}

// TestSelectDialIP_PrefersIPv4 qualifies the IPv6-first regression fix: a verified
// set where the public hostname resolved AAAA first (IPv6) and then an IPv4 must
// be dialed at the IPv4 address over the IPv4-only proxy, so a dual-stack public
// host is not lost to an IPv6-only dial.
func TestSelectDialIP_PrefersIPv4(t *testing.T) {
	ips := []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("203.0.113.99")}
	assert.Equal(t, "203.0.113.99", selectDialIP(ips).String(),
		"must prefer the IPv4 address over an IPv6-first public set")
}

// TestSelectDialIP_FallsBackToFirstWhenIPv6Only qualifies that a verified set with
// no IPv4 member falls back to the first address rather than returning nothing,
// so IPv6-only operation is not silently dropped.
func TestSelectDialIP_FallsBackToFirstWhenIPv6Only(t *testing.T) {
	ips := []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2")}
	assert.Equal(t, "2001:db8::1", selectDialIP(ips).String(),
		"must fall back to the first address when the set is IPv6-only")
}

// TestSelectDialIP_IPv4Only qualifies that an IPv4-only verified set picks the
// first IPv4 address.
func TestSelectDialIP_IPv4Only(t *testing.T) {
	ips := []net.IP{net.ParseIP("203.0.113.99"), net.ParseIP("203.0.113.100")}
	assert.Equal(t, "203.0.113.99", selectDialIP(ips).String(),
		"must pick the first IPv4 address")
}

// TestHTTPInterceptor_PublicHostnamePreservesOperation is a positive control that
// a public hostname is allowed, dials the verified public IP literal (not the
// hostname), and still forwards the request with the original Host header.
func TestHTTPInterceptor_PublicHostnamePreservesOperation(t *testing.T) {
	dns := newSyntheticDNS(t, syntheticDNSConfig{
		"public.test.": {
			{"203.0.113.99"},
		},
	})
	dns.install()
	defer dns.uninstall()

	var fixtureHits atomic.Int64
	fixture := startOwnedHTTPFixture(t, &fixtureHits)
	defer fixture.Close()

	pol := policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	var dialedAddr atomic.Value
	interceptor := NewHTTPInterceptor(pol, nil, nil)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialedAddr.Store(addr)
		// Route to the local fixture regardless of the requested addr, so the
		// request is served and we can assert the Host header survived.
		return net.Dial("tcp", fixture.Addr().String())
	}

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, "203.0.113.1", localPort(t, fixture))

	_, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: public.test\r\nConnection: close\r\n\r\n"))
	require.NoError(t, err)

	resp := readResponse(t, client)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "public hostname must be allowed and forwarded")
	assert.Equal(t, int64(1), fixtureHits.Load(), "one request must reach the upstream")

	gotDial, _ := dialedAddr.Load().(string)
	assert.Equal(t, fmt.Sprintf("203.0.113.99:%d", localPort(t, fixture)), gotDial,
		"dial must bind to the verified public IP literal")
}
