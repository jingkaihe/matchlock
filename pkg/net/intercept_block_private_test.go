//go:build linux

package net

import (
	"bufio"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPInterceptorBlockPrivateIPs qualifies that, in the HTTP interception
// path, a request to a private IPv4 address is denied (403) before any upstream
// dial, and a blocked event is emitted.
func TestHTTPInterceptorBlockPrivateIPs(t *testing.T) {
	pol := policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	})
	events := make(chan api.Event, 10)
	interceptor := NewHTTPInterceptor(pol, events, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go interceptor.HandleHTTP(server, "127.0.0.1", 8080)

	req := "GET / HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nConnection: close\r\n\r\n"
	_, err := client.Write([]byte(req))
	require.NoError(t, err)

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "private IPv4 host must be denied in intercept mode")

	select {
	case ev := <-events:
		require.NotNil(t, ev.Network)
		assert.True(t, ev.Network.Blocked)
		assert.Equal(t, "127.0.0.1:8080", ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked network event")
	}
}

// TestHTTPInterceptorBlockPrivateIPv6 qualifies that a bracketed private IPv6
// host is denied via the HTTP interception path too.
func TestHTTPInterceptorBlockPrivateIPv6(t *testing.T) {
	pol := policy.NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	})
	events := make(chan api.Event, 10)
	interceptor := NewHTTPInterceptor(pol, events, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go interceptor.HandleHTTP(server, "fd00::1", 8080)

	req := "GET / HTTP/1.1\r\nHost: [fd00::1]:8080\r\nConnection: close\r\n\r\n"
	_, err := client.Write([]byte(req))
	require.NoError(t, err)

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "private IPv6 host must be denied in intercept mode")

	select {
	case ev := <-events:
		require.NotNil(t, ev.Network)
		assert.True(t, ev.Network.Blocked)
	default:
		assert.Fail(t, "expected a blocked network event for private IPv6")
	}
}
