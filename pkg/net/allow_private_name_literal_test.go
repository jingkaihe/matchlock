//go:build linux

package net

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are the proxy-level regression coverage for the FORK-NET-FIX-0923
// defect: an allow_private NAME entry must authorize the LITERAL destination the
// proxy actually observes, not only a request whose host string matches the
// entry. Both proxy paths see a literal when the guest connects by IP:
//
//   - pkg/net/proxy.go handlePassthrough is handed the pre-DNAT destination
//     IP:port (an IPv6 literal arrives bare, exactly like an IPv4 one), so a
//     name entry can only work there if it covers the address set it resolves
//     to. This is the coordinator's reproduction: '--allow-private
//     <name>:8888' failed by name ('Empty reply from server') and by literal
//     ('Connection reset by peer') while the literal and CIDR entry forms
//     worked.
//   - http.go HandleHTTP evaluates the policy on the request's Host header, so a
//     request carrying an IP literal is the same shape.
//
// The covered cases mirror the reproduction: an IPv6 literal in the ranges the
// repro used (fd00::/8 and the Yggdrasil 200::/7), an IPv4 literal, a
// port-scoped entry, an unrelated entry, and the no-entry control. The positive
// cases assert the relayed payload AND the dialed address (the verified literal
// is what gets dialed — never the name), the negative cases assert no dial, a
// closed guest connection and a blocked network event naming the literal
// destination.
//
// They resolve the allow_private NAME entries through the package's
// process-local synthetic DNS (dnssynth_test.go), which replaces the
// process-global net.DefaultResolver, so they must NOT call t.Parallel().

const (
	// v6PrivateLiteral is a private (fd00::/8) IPv6 literal, the shape of the
	// per-VM unique-local address the Linux guest link uses.
	v6PrivateLiteral = "fd00:7::5"
	// yggPrivateLiteral is the Yggdrasil (200::/7) private address of the
	// coordinator's reproduction; its allow_private NAME entry never matched on
	// the passthrough path.
	yggPrivateLiteral = "219:c447:5629:62b7:4e7:9db8:2a3f:8ca9"
	// otherPrivateLiteral is a second private IPv6 address, used by the
	// "unrelated entry" control.
	otherPrivateLiteral = "fc00:9::1"
	// v4PrivateLiteral is the private IPv4 literal the HTTP fixture is reached
	// at (the fixture listener is bound to loopback).
	v4PrivateLiteral = "127.0.0.1"

	// The synthetic-DNS names. The engine resolves an allow_private NAME entry
	// host-side; these tables are the only source of those answers.
	v6Name    = "matchlock-priv-v6.test"
	yggName   = "matchlock-priv-ygg.test"
	otherName = "matchlock-priv-other.test"
	v4Name    = "matchlock-priv-v4.test"
)

// relayPassthrough drives handlePassthrough over an in-process pipe with one
// payload and returns what the relay echoed back.
func relayPassthrough(t *testing.T, tp *TransparentProxy, dstIP string, dstPort int, payload string) string {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })

	go tp.handlePassthrough(server, dstIP, dstPort)

	require.NoError(t, client.SetWriteDeadline(time.Now().Add(2*time.Second)))
	_, err := client.Write([]byte(payload))
	require.NoError(t, err)

	buf := make([]byte, len(payload))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(client, buf)
	require.NoError(t, err)
	return string(buf)
}

// refusePassthrough asserts that handlePassthrough refuses dstIP:dstPort: it
// returns without dialing, the guest connection is closed without a payload, and
// the refusal is reported as a blocked network event naming the literal
// destination.
func refusePassthrough(t *testing.T, tp *TransparentProxy, dialed *atomic.Value, events chan api.Event, dstIP string, dstPort int) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, dstIP, dstPort)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough must return at once for a blocked literal")
	}

	_, loaded := dialed.Load().(string)
	assert.False(t, loaded, "a refused private destination must never be dialed")

	// A closed net.Pipe reports ErrClosedPipe from SetWriteDeadline, so the
	// deadline is best-effort here (same shape as the IPv6-literal tests).
	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write([]byte("must not be relayed"))
	assert.Error(t, err, "a refused passthrough must have closed the guest connection")

	ev := nextBlockedEvent(t, events)
	assert.Equal(t, net.JoinHostPort(dstIP, strconv.Itoa(dstPort)), ev.Network.Host)
}

// TestHandlePassthrough_AllowPrivateNameCoversLiteralDestination is the
// passthrough half of the regression: an allow_private NAME entry must cover the
// literal IP:port the proxy sees after the original-destination lookup.
func TestHandlePassthrough_AllowPrivateNameCoversLiteralDestination(t *testing.T) {
	upstream := startEchoServer(t)
	t.Cleanup(func() { upstream.Close() })

	_, portStr, err := net.SplitHostPort(upstream.Addr().String())
	require.NoError(t, err)
	port := mustAtoi(portStr)
	wrongPort := otherPort(port)

	// newProxy builds a TransparentProxy whose dial seam records the requested
	// (policy-verified) address and lands on the owned echo server, so a relayed
	// payload and the dial target are both observable.
	newProxy := func(t *testing.T, cfg *api.NetworkConfig) (*TransparentProxy, *atomic.Value, chan api.Event) {
		t.Helper()

		var dialed atomic.Value // string
		events := make(chan api.Event, 10)
		tp := &TransparentProxy{
			policy: policy.NewEngine(cfg),
			events: events,
			dial: func(network, addr string) (net.Conn, error) {
				dialed.Store(addr)
				return net.Dial(network, upstream.Addr().String())
			},
		}
		return tp, &dialed, events
	}

	t.Run("NAME entry covering the v6 literal relays and dials the literal", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			v6Name + ".": {{v6PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		tp, dialed, _ := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{fmt.Sprintf("%s:%d", v6Name, port)},
		})

		payload := relayPassthrough(t, tp, v6PrivateLiteral, port, "hello name-entry v6")
		assert.Equal(t, "hello name-entry v6", payload,
			"a NAME entry covering the v6 literal must relay")

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort(v6PrivateLiteral, portStr), got,
			"the v6 destination must be dialed as the bracketed verified literal")
		assert.GreaterOrEqual(t, dns.aAAAACount(v6Name+"."), 1,
			"the NAME entry must have been resolved host-side")
	})

	t.Run("NAME entry covering the Yggdrasil literal relays and dials the literal", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			yggName + ".": {{yggPrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		tp, dialed, _ := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{fmt.Sprintf("%s:%d", yggName, port)},
		})

		payload := relayPassthrough(t, tp, yggPrivateLiteral, port, "hello name-entry yggdrasil")
		assert.Equal(t, "hello name-entry yggdrasil", payload,
			"the coordinator's 200::/7 reproduction must relay with a NAME entry")

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort(yggPrivateLiteral, portStr), got)
	})

	t.Run("the same literal without a matching NAME entry is refused", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			v6Name + ".":    {{v6PrivateLiteral}},
			otherName + ".": {{otherPrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		// The entry is a NAME entry, but it resolves to a different private
		// address, so it must not cover this literal.
		tp, dialed, events := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{fmt.Sprintf("%s:%d", otherName, port)},
		})

		refusePassthrough(t, tp, dialed, events, v6PrivateLiteral, port)
	})

	t.Run("a port-scoped NAME entry does not cover another port", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			v6Name + ".": {{v6PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		tp, dialed, events := newProxy(t, &api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{fmt.Sprintf("%s:%d", v6Name, wrongPort)},
		})

		refusePassthrough(t, tp, dialed, events, v6PrivateLiteral, port)
	})

	t.Run("no allow_private entry at all is refused", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			v6Name + ".": {{v6PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		tp, dialed, events := newProxy(t, &api.NetworkConfig{BlockPrivateIPs: true})

		refusePassthrough(t, tp, dialed, events, v6PrivateLiteral, port)
	})
}

// TestHTTPInterceptor_AllowPrivateNameCoversLiteralHostHeader is the HTTP half
// of the regression: a request whose Host header is an IP literal (the shape a
// guest produces when it connects by address) must be proxied when an
// allow_private NAME entry covers that literal, with the dial bound to the
// verified literal rather than the name.
func TestHTTPInterceptor_AllowPrivateNameCoversLiteralHostHeader(t *testing.T) {
	// newInterceptor builds an interceptor over a private loopback fixture. The
	// fixture is started first so the port it landed on can be used in the
	// allow_private entry (a port-scoped entry must carry the real destination
	// port). Its dial seam records the requested upstream address and routes the
	// connection to the fixture, so a 200 and the dial target can both be
	// observed; the hit counter proves whether the fixture was reached at all.
	newInterceptor := func(t *testing.T, cfgFor func(dstPort int) *api.NetworkConfig) (*HTTPInterceptor, *atomic.Value, *atomic.Int64, int) {
		t.Helper()

		var hits atomic.Int64
		fixture := startOwnedHTTPFixture(t, &hits)
		port := localPort(t, fixture)
		interceptor := NewHTTPInterceptor(policy.NewEngine(cfgFor(port)), nil, nil)

		var dialed atomic.Value // string
		interceptor.dial = func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			return net.Dial("tcp", fixture.Addr().String())
		}
		return interceptor, &dialed, &hits, port
	}

	t.Run("IPv4 literal Host header with a covering NAME entry dials the literal", func(t *testing.T) {
		dns := newSyntheticDNS(t, syntheticDNSConfig{
			v4Name + ".": {{v4PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		interceptor, dialed, hits, port := newInterceptor(t, func(dstPort int) *api.NetworkConfig {
			return &api.NetworkConfig{
				BlockPrivateIPs: true,
				AllowPrivate:    []string{fmt.Sprintf("%s:%d", v4Name, dstPort)},
			}
		})

		resp := doInterceptedHTTP(t, interceptor, v4PrivateLiteral, port,
			fmt.Sprintf("%s:%d", v4PrivateLiteral, port))
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode,
			"an IP-literal Host header covered by a NAME entry must be proxied")
		assert.Equal(t, int64(1), hits.Load(), "the private fixture must have been reached")

		got, _ := dialed.Load().(string)
		assert.Equal(t, fmt.Sprintf("%s:%d", v4PrivateLiteral, port), got,
			"the dial must be bound to the verified literal, never the name")
		assert.NotEqual(t, fmt.Sprintf("%s:%d", v4Name, port), got)
	})

	t.Run("IPv6 literal Host header with a covering NAME entry dials the literal", func(t *testing.T) {
		dns := newSyntheticDNSWithAAAA(t, nil, syntheticDNSConfig{
			v6Name + ".": {{v6PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		interceptor, dialed, hits, port := newInterceptor(t, func(dstPort int) *api.NetworkConfig {
			return &api.NetworkConfig{
				BlockPrivateIPs: true,
				AllowPrivate:    []string{fmt.Sprintf("%s:%d", v6Name, dstPort)},
			}
		})

		resp := doInterceptedHTTP(t, interceptor, v6PrivateLiteral, port,
			fmt.Sprintf("[%s]:%d", v6PrivateLiteral, port))
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode,
			"a bracketed v6 literal Host header covered by a NAME entry must be proxied")
		assert.Equal(t, int64(1), hits.Load(), "the private fixture must have been reached")

		got, _ := dialed.Load().(string)
		assert.Equal(t, net.JoinHostPort(v6PrivateLiteral, strconv.Itoa(port)), got,
			"the v6 literal must be dialed bracketed and unchanged")
	})

	t.Run("IP-literal Host header without a covering entry is forbidden", func(t *testing.T) {
		dns := newSyntheticDNS(t, syntheticDNSConfig{
			v4Name + ".": {{v4PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		// No allow_private entry at all: the literal is private and uncovered.
		interceptor, dialed, hits, port := newInterceptor(t, func(int) *api.NetworkConfig {
			return &api.NetworkConfig{BlockPrivateIPs: true}
		})

		resp := doInterceptedHTTP(t, interceptor, v4PrivateLiteral, port,
			fmt.Sprintf("%s:%d", v4PrivateLiteral, port))
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"an uncovered private literal must be refused")
		assert.Nil(t, dialed.Load(), "must be refused before any upstream dial")
		assert.Equal(t, int64(0), hits.Load(), "the private fixture must not be reached")
	})

	t.Run("a port-scoped NAME entry does not cover another port", func(t *testing.T) {
		dns := newSyntheticDNS(t, syntheticDNSConfig{
			v4Name + ".": {{v4PrivateLiteral}},
		})
		dns.install()
		defer dns.uninstall()

		var hits atomic.Int64
		fixture := startOwnedHTTPFixture(t, &hits)
		port := localPort(t, fixture)

		interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{fmt.Sprintf("%s:%d", v4Name, otherPort(port))},
		}), nil, nil)
		var dialed atomic.Value // string
		interceptor.dial = func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			return net.Dial("tcp", fixture.Addr().String())
		}

		resp := doInterceptedHTTP(t, interceptor, v4PrivateLiteral, port,
			fmt.Sprintf("%s:%d", v4PrivateLiteral, port))
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"an entry scoped to another port must not cover this destination")
		assert.Nil(t, dialed.Load(), "must be refused before any upstream dial")
		assert.Equal(t, int64(0), hits.Load(), "the private fixture must not be reached")
	})
}
