//go:build linux

package net

import (
	"crypto/tls"
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

// These tests exercise the HTTP/HTTPS interceptor's use of the real destination
// port when evaluating allow_private. A port-scoped exception must be honored
// only for the exact destination port, and an unlisted private address (literal
// or DNS answer) must still be refused before any upstream dial.

// newAllowPrivateInterceptor builds an interceptor over a private loopback
// fixture. Its dial seam records the requested upstream address and routes the
// connection to the fixture so an allowed request can complete with 200. The
// fixture hit counter lets a test assert that a blocked destination never
// reached the upstream.
func newAllowPrivateInterceptor(t *testing.T, cfg *api.NetworkConfig) (*HTTPInterceptor, *atomic.Value, *atomic.Int64, net.Listener) {
	t.Helper()

	var hits atomic.Int64
	fixture := startOwnedHTTPFixture(t, &hits)
	interceptor := NewHTTPInterceptor(policy.NewEngine(cfg), nil, nil)

	var dialed atomic.Value // string
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed.Store(addr)
		return net.Dial("tcp", fixture.Addr().String())
	}
	return interceptor, &dialed, &hits, fixture
}

// doInterceptedHTTP drives HandleHTTP over net.Pipe with one request and returns
// the guest-visible response. dstPort is the real destination port the guest
// connected to; hostHeader is the request Host header.
func doInterceptedHTTP(t *testing.T, interceptor *HTTPInterceptor, dstIP string, dstPort int, hostHeader string) *http.Response {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })
	go interceptor.HandleHTTP(server, dstIP, dstPort)

	_, err := client.Write([]byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostHeader)))
	require.NoError(t, err)

	return readResponse(t, client)
}

// TestHTTPInterceptor_AllowPrivateLiteralPortProxied qualifies that a private IP
// literal whose exact host:port is listed in allow_private is proxied (200) and
// the dial is bound to that literal.
func TestHTTPInterceptor_AllowPrivateLiteralPortProxied(t *testing.T) {
	const privateIP = "127.0.0.1"

	// The allow_private entry must carry the fixture's port, so the fixture is
	// started before the policy engine is built.
	var hits atomic.Int64
	fixture := startOwnedHTTPFixture(t, &hits)
	port := localPort(t, fixture)

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{fmt.Sprintf("%s:%d", privateIP, port)},
	}), nil, nil)
	var dialed atomic.Value
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed.Store(addr)
		return net.Dial("tcp", fixture.Addr().String())
	}

	resp := doInterceptedHTTP(t, interceptor, privateIP, port, fmt.Sprintf("%s:%d", privateIP, port))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "listed private literal:port must be proxied")
	assert.Equal(t, int64(1), hits.Load(), "the private fixture must have been reached")

	got, _ := dialed.Load().(string)
	assert.Equal(t, fmt.Sprintf("%s:%d", privateIP, port), got,
		"the dial must be bound to the verified private literal")
}

// TestHTTPInterceptor_AllowPrivateLiteralWithoutEntryForbidden qualifies that the
// same private literal with no allow_private entry is refused with 403 before any
// upstream dial.
func TestHTTPInterceptor_AllowPrivateLiteralWithoutEntryForbidden(t *testing.T) {
	const privateIP = "127.0.0.1"

	interceptor, dialed, hits, fixture := newAllowPrivateInterceptor(t, &api.NetworkConfig{
		BlockPrivateIPs: true,
	})
	port := localPort(t, fixture)

	resp := doInterceptedHTTP(t, interceptor, privateIP, port, fmt.Sprintf("%s:%d", privateIP, port))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "unlisted private literal must be refused")
	assert.Nil(t, dialed.Load(), "must be refused before any upstream dial")
	assert.Equal(t, int64(0), hits.Load(), "the private fixture must not be reached")
}

// TestHTTPInterceptor_AllowPrivateNameWithAddressEntryProxied qualifies the
// positive hostname case: a name listed in allow_private whose resolution is
// covered by a listed address entry is proxied, and the dial is bound to the
// verified private literal (resolved exactly once).
func TestHTTPInterceptor_AllowPrivateNameWithAddressEntryProxied(t *testing.T) {
	dns := newSyntheticDNS(t, syntheticDNSConfig{
		"allowed-private.test.": {{"127.0.0.1"}},
	})
	dns.install()
	defer dns.uninstall()

	interceptor, dialed, hits, fixture := newAllowPrivateInterceptor(t, &api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"allowed-private.test", "127.0.0.1"},
	})
	port := localPort(t, fixture)

	resp := doInterceptedHTTP(t, interceptor, "127.0.0.1", port, "allowed-private.test")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "listed name with a covering address entry must be proxied")
	assert.Equal(t, int64(1), hits.Load(), "the private fixture must have been reached")

	got, _ := dialed.Load().(string)
	assert.Equal(t, fmt.Sprintf("127.0.0.1:%d", port), got,
		"the dial must be bound to the verified private literal, not the hostname")
	assert.Equal(t, 1, dns.aCount("allowed-private.test."),
		"the hostname must be resolved exactly once")
}

// TestHTTPInterceptor_AllowPrivateNameUnlistedAddressForbidden qualifies the
// rebinding guard: a listed name that resolves to a private address that is NOT
// covered by an address entry is refused with 403 before any upstream dial.
func TestHTTPInterceptor_AllowPrivateNameUnlistedAddressForbidden(t *testing.T) {
	tests := map[string][]string{
		"name entry only, no address entry":            {"evil.test"},
		"name entry plus an unrelated private address": {"evil.test", "192.168.1.50"},
		"name entry plus a public address only":        {"evil.test", "203.0.113.50"},
	}
	for name, allowPrivate := range tests {
		t.Run(name, func(t *testing.T) {
			dns := newSyntheticDNS(t, syntheticDNSConfig{
				"evil.test.": {{"127.0.0.1"}},
			})
			dns.install()
			defer dns.uninstall()

			interceptor, dialed, hits, fixture := newAllowPrivateInterceptor(t, &api.NetworkConfig{
				BlockPrivateIPs: true,
				AllowPrivate:    allowPrivate,
			})

			resp := doInterceptedHTTP(t, interceptor, "127.0.0.1", localPort(t, fixture), "evil.test")
			defer resp.Body.Close()

			assert.Equal(t, http.StatusForbidden, resp.StatusCode,
				"a name resolving to an unlisted private address must be refused")
			assert.Nil(t, dialed.Load(), "must be refused before any upstream dial")
			assert.Equal(t, int64(0), hits.Load(), "the private fixture must not be reached")
		})
	}
}

// TestHTTPInterceptor_AllowPrivatePortMismatchForbidden qualifies that an entry
// scoped to a different port does not lift the private block.
func TestHTTPInterceptor_AllowPrivatePortMismatchForbidden(t *testing.T) {
	const privateIP = "127.0.0.1"

	var hits atomic.Int64
	fixture := startOwnedHTTPFixture(t, &hits)
	port := localPort(t, fixture)

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{fmt.Sprintf("%s:%d", privateIP, port+1)},
	}), nil, nil)
	var dialed atomic.Value
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed.Store(addr)
		return net.Dial("tcp", fixture.Addr().String())
	}

	resp := doInterceptedHTTP(t, interceptor, privateIP, port, fmt.Sprintf("%s:%d", privateIP, port))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"an allow_private entry scoped to another port must not match")
	assert.Nil(t, dialed.Load(), "must be refused before any upstream dial")
	assert.Equal(t, int64(0), hits.Load(), "the private fixture must not be reached")
}

// TestHTTPInterceptorHTTPS_AllowPrivatePortProxied qualifies that the HTTPS path
// evaluates allow_private with the real destination port: a port-scoped entry
// for the private SNI literal attempts an upstream dial.
func TestHTTPInterceptorHTTPS_AllowPrivatePortProxied(t *testing.T) {
	caPool, err := NewCAPool()
	require.NoError(t, err)

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"127.0.0.1:8443"},
	}), nil, caPool)
	dialed := make(chan string, 1)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("refused by test seam")
	}

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTPS(server, "127.0.0.1", 8443)

	tlsClient := tls.Client(client, &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true})
	require.NoError(t, tlsClient.Handshake())

	select {
	case addr := <-dialed:
		assert.Equal(t, "127.0.0.1:8443", addr,
			"the HTTPS dial must be bound to the allow-private literal at the matching port")
	case <-time.After(5 * time.Second):
		t.Fatal("expected an upstream dial for the allow-private HTTPS destination")
	}
}

// TestHTTPInterceptorHTTPS_AllowPrivateWrongPortNoDial qualifies that the HTTPS
// path refuses when the destination port does not match the allow_private entry.
func TestHTTPInterceptorHTTPS_AllowPrivateWrongPortNoDial(t *testing.T) {
	caPool, err := NewCAPool()
	require.NoError(t, err)

	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"127.0.0.1:8444"},
	}), nil, caPool)
	dialed := make(chan string, 1)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("refused by test seam")
	}

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTPS(server, "127.0.0.1", 8443)

	tlsClient := tls.Client(client, &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true})
	require.NoError(t, tlsClient.Handshake())

	// The blocked path closes the guest TLS session without an upstream
	// response; reading to error proves HandleHTTPS finished the refusal.
	_ = tlsClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, readErr := tlsClient.Read(make([]byte, 1))
	require.Error(t, readErr, "blocked HTTPS session must be closed without an upstream response")

	select {
	case addr := <-dialed:
		t.Fatalf("wrong-port allow_private must not dial, got %q", addr)
	default:
	}
}
