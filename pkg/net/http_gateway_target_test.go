package net

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests qualify that the darwin gateway-to-loopback mapping is applied to
// the HTTP/HTTPS interceptor's upstream dial. On darwin the guest's default route
// points at the netstack's virtual gateway (Config.GatewayIP), which is assigned
// to the stack NIC but is NOT bound on the host (no TAP), so a raw dial to it can
// never connect. The Linux TransparentProxy leaves gatewayIP empty so its TAP
// gateway keeps being dialed literally. The tests are build-tag-free so they run
// in the Linux unit suite as well.

// dialObservation captures the upstream address the interceptor tried to dial.
type dialObservation struct {
	addr string
	ok   bool
}

// newObservingInterceptor returns an interceptor whose upstream dial records the
// requested address and fails, so the tests never need a real upstream server.
func newObservingInterceptor(t *testing.T, allowedHosts []string, gatewayIP string, caPool *CAPool) (*HTTPInterceptor, <-chan dialObservation) {
	t.Helper()

	pol := policy.NewEngine(&api.NetworkConfig{AllowedHosts: allowedHosts})
	interceptor := NewHTTPInterceptor(pol, nil, caPool)
	interceptor.gatewayIP = gatewayIP

	dialed := make(chan dialObservation, 1)
	interceptor.dial = func(network, addr string) (net.Conn, error) {
		dialed <- dialObservation{addr: addr, ok: true}
		return nil, fmt.Errorf("refused by test seam")
	}
	return interceptor, dialed
}

// observeHandleHTTP drives HandleHTTP over net.Pipe with a single request with
// the given Host header and returns what the interceptor dialed, if anything.
func observeHandleHTTP(t *testing.T, allowedHosts []string, gatewayIP, reqHost string, dstPort int) (string, bool) {
	t.Helper()

	interceptor, dialed := newObservingInterceptor(t, allowedHosts, gatewayIP, nil)

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTP(server, reqHost, dstPort)

	_, err := client.Write([]byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", reqHost)))
	require.NoError(t, err)

	// Drain the (502/403) response so HandleHTTP can finish and release the pipe.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if resp, err := http.ReadResponse(bufio.NewReader(client), nil); err == nil {
		_ = resp.Body.Close()
	}

	select {
	case obs := <-dialed:
		return obs.addr, obs.ok
	default:
		return "", false
	}
}

// observeHandleHTTPS performs the guest-facing TLS handshake with the
// interceptor's MITM CA, then returns the raw upstream address the interceptor
// attempted to dial after the policy check.
func observeHandleHTTPS(t *testing.T, allowedHosts []string, gatewayIP, serverName string, dstPort int) (string, bool) {
	t.Helper()

	caPool, err := NewCAPool()
	require.NoError(t, err)

	interceptor, dialed := newObservingInterceptor(t, allowedHosts, gatewayIP, caPool)

	client, server := net.Pipe()
	defer client.Close()
	go interceptor.HandleHTTPS(server, serverName, dstPort)

	tlsClient := tls.Client(client, &tls.Config{
		ServerName: serverName,
		// The upstream dial is a test seam, so verification is irrelevant here.
		InsecureSkipVerify: true,
	})
	// Deliberately close only the raw pipe: tls.Conn.Close waits up to 5s to
	// flush a close_notify onto a pipe the interceptor is no longer reading.
	require.NoError(t, tlsClient.Handshake())

	select {
	case obs := <-dialed:
		return obs.addr, obs.ok
	case <-time.After(5 * time.Second):
		return "", false
	}
}

// TestNewHTTPInterceptor_GatewayIPDefaultsEmpty locks the Linux behavior: the
// constructor leaves gatewayIP empty, and the Linux TransparentProxy path only
// calls the constructor, so its TAP gateway is never mapped.
func TestNewHTTPInterceptor_GatewayIPDefaultsEmpty(t *testing.T) {
	interceptor := NewHTTPInterceptor(policy.NewEngine(&api.NetworkConfig{}), nil, nil)
	assert.Empty(t, interceptor.gatewayIP,
		"NewHTTPInterceptor must default gatewayIP to empty (Linux dials literally)")
}

// TestHTTPInterceptorHTTP_GatewayDialMapsToLoopback is the darwin regression: an
// HTTP request whose Host is the (allowlisted) virtual gateway must dial host
// loopback, not the unbound gateway address.
func TestHTTPInterceptorHTTP_GatewayDialMapsToLoopback(t *testing.T) {
	const gateway = "192.168.100.1"

	addr, ok := observeHandleHTTP(t, []string{gateway}, gateway, gateway, 8080)
	require.True(t, ok, "an allowlisted gateway request must attempt a dial")
	assert.Equal(t, "127.0.0.1:8080", addr,
		"the virtual gateway must be mapped to host loopback on darwin")
}

// TestHTTPInterceptorHTTP_NoGatewayDialsLiterally locks the Linux path: with no
// gateway configured the verified destination is dialed unchanged.
func TestHTTPInterceptorHTTP_NoGatewayDialsLiterally(t *testing.T) {
	const gateway = "192.168.100.1"

	addr, ok := observeHandleHTTP(t, []string{gateway}, "", gateway, 8080)
	require.True(t, ok, "an allowlisted host must attempt a dial")
	assert.Equal(t, "192.168.100.1:8080", addr,
		"with no gateway configured the literal destination must be dialed")
}

// TestHTTPInterceptorHTTP_NonGatewayDialsVerifiedLiteral proves the mapping is
// narrow: a policy-verified address that is not the gateway keeps its literal
// dial target even when a gateway is configured.
func TestHTTPInterceptorHTTP_NonGatewayDialsVerifiedLiteral(t *testing.T) {
	const (
		gateway = "192.168.100.1"
		public  = "203.0.113.99"
	)

	addr, ok := observeHandleHTTP(t, []string{public}, gateway, public, 8080)
	require.True(t, ok, "an allowlisted non-gateway host must attempt a dial")
	assert.Equal(t, "203.0.113.99:8080", addr,
		"only the configured gateway may be rewritten")
}

// TestHTTPInterceptorHTTP_UnallowlistedPerformsNoDial proves the policy check is
// still on the original guest-visible host: an unallowlisted gateway performs no
// dial at all, even though it would be mapped to loopback.
func TestHTTPInterceptorHTTP_UnallowlistedPerformsNoDial(t *testing.T) {
	const (
		gateway   = "192.168.100.1"
		otherHost = "203.0.113.99"
	)

	addr, ok := observeHandleHTTP(t, []string{otherHost}, gateway, gateway, 8080)
	assert.False(t, ok, "an unallowlisted gateway host must be blocked before any dial")
	assert.Empty(t, addr)
}

// TestHTTPInterceptorHTTPS_GatewayDialMapsToLoopback qualifies the HTTPS
// interceptor's raw upstream TLS dial: it too maps the verified gateway address
// to host loopback while leaving the TLS SNI untouched.
func TestHTTPInterceptorHTTPS_GatewayDialMapsToLoopback(t *testing.T) {
	const gateway = "192.168.100.1"

	addr, ok := observeHandleHTTPS(t, []string{gateway}, gateway, gateway, 8443)
	require.True(t, ok, "an allowlisted gateway HTTPS session must attempt a raw dial")
	assert.Equal(t, "127.0.0.1:8443", addr,
		"the HTTPS raw dial to the virtual gateway must be mapped to host loopback")
}

// TestHTTPInterceptorHTTPS_NoGatewayDialsVerifiedLiteral is the HTTPS control:
// with no gateway configured the verified address is dialed unchanged.
func TestHTTPInterceptorHTTPS_NoGatewayDialsVerifiedLiteral(t *testing.T) {
	const gateway = "192.168.100.1"

	addr, ok := observeHandleHTTPS(t, []string{gateway}, "", gateway, 8443)
	require.True(t, ok, "an allowlisted gateway HTTPS session must attempt a raw dial")
	assert.Equal(t, "192.168.100.1:8443", addr,
		"with no gateway configured the HTTPS dial must stay literal")
}
