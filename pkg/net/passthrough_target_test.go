package net

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMapGatewayDialHost pins the pure address mapping behind the darwin
// alternate-port fix. The comparison must use net.IP.Equal semantics so an IPv4
// gateway matches an IPv4-mapped-IPv6 destination, and any input it does not
// positively recognize (empty or unparseable gateway, unparseable destination,
// non-gateway address) must be returned untouched.
func TestMapGatewayDialHost(t *testing.T) {
	tests := []struct {
		name      string
		dstIP     string
		gatewayIP string
		want      string
	}{
		{
			name:      "ipv4 gateway maps to ipv4 loopback",
			dstIP:     "192.168.100.1",
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1",
		},
		{
			name:      "ipv4-mapped-ipv6 destination matches ipv4 gateway",
			dstIP:     "::ffff:192.168.100.1",
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1",
		},
		{
			name:      "ipv4 destination matches ipv4-mapped-ipv6 gateway",
			dstIP:     "192.168.100.1",
			gatewayIP: "::ffff:192.168.100.1",
			want:      "127.0.0.1",
		},
		{
			name:      "ipv6 gateway maps to ipv6 loopback",
			dstIP:     "fd00::1",
			gatewayIP: "fd00::1",
			want:      "::1",
		},
		{
			name:      "non-gateway ipv4 unchanged",
			dstIP:     "10.0.0.5",
			gatewayIP: "192.168.100.1",
			want:      "10.0.0.5",
		},
		{
			name:      "non-gateway ipv6 unchanged",
			dstIP:     "2001:db8::5",
			gatewayIP: "fd00::1",
			want:      "2001:db8::5",
		},
		{
			name:      "ipv4 destination with ipv6 gateway unchanged",
			dstIP:     "192.168.100.1",
			gatewayIP: "fd00::1",
			want:      "192.168.100.1",
		},
		{
			name:      "empty gateway unchanged",
			dstIP:     "192.168.100.1",
			gatewayIP: "",
			want:      "192.168.100.1",
		},
		{
			name:      "unparseable destination unchanged",
			dstIP:     "not-an-ip",
			gatewayIP: "192.168.100.1",
			want:      "not-an-ip",
		},
		{
			name:      "unparseable gateway unchanged",
			dstIP:     "192.168.100.1",
			gatewayIP: "not-an-ip",
			want:      "192.168.100.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mapGatewayDialHost(tt.dstIP, tt.gatewayIP))
		})
	}
}

// TestResolvePassthroughTarget pins the host:port form the darwin passthrough
// dials. The port must be preserved exactly for both loopback-mapped and
// unchanged destinations, and IPv6 literals must be bracketed by JoinHostPort.
func TestResolvePassthroughTarget(t *testing.T) {
	tests := []struct {
		name      string
		dstIP     string
		dstPort   int
		gatewayIP string
		want      string
	}{
		{
			name:      "ipv4 gateway maps to loopback with same port",
			dstIP:     "192.168.100.1",
			dstPort:   8080,
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1:8080",
		},
		{
			name:      "ipv6 gateway maps to loopback with same port",
			dstIP:     "fd00::1",
			dstPort:   443,
			gatewayIP: "fd00::1",
			want:      "[::1]:443",
		},
		{
			name:      "ipv4-mapped-ipv6 destination maps to loopback",
			dstIP:     "::ffff:192.168.100.1",
			dstPort:   53,
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1:53",
		},
		{
			name:      "non-gateway ipv4 unchanged",
			dstIP:     "10.0.0.5",
			dstPort:   3128,
			gatewayIP: "192.168.100.1",
			want:      "10.0.0.5:3128",
		},
		{
			name:      "non-gateway ipv6 unchanged and bracketed",
			dstIP:     "2001:db8::5",
			dstPort:   8443,
			gatewayIP: "fd00::1",
			want:      "[2001:db8::5]:8443",
		},
		{
			name:      "empty gateway unchanged",
			dstIP:     "192.168.100.1",
			dstPort:   1234,
			gatewayIP: "",
			want:      "192.168.100.1:1234",
		},
		{
			name:      "unparseable destination unchanged",
			dstIP:     "bogus",
			dstPort:   80,
			gatewayIP: "192.168.100.1",
			want:      "bogus:80",
		},
		{
			name:      "unparseable gateway unchanged",
			dstIP:     "192.168.100.1",
			dstPort:   80,
			gatewayIP: "bogus",
			want:      "192.168.100.1:80",
		},
		{
			name:      "port zero preserved",
			dstIP:     "192.168.100.1",
			dstPort:   0,
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1:0",
		},
		{
			name:      "max port preserved",
			dstIP:     "192.168.100.1",
			dstPort:   65535,
			gatewayIP: "192.168.100.1",
			want:      "127.0.0.1:65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePassthroughTarget(tt.dstIP, tt.dstPort, tt.gatewayIP)
			require.Equal(t, tt.want, got)

			_, port, err := net.SplitHostPort(got)
			require.NoError(t, err, "resolved target must be a valid host:port")
			portNum, err := strconv.Atoi(port)
			require.NoError(t, err)
			assert.Equal(t, tt.dstPort, portNum, "port must be preserved exactly")
		})
	}
}

// TestResolvePassthroughTarget_LoopbackIsDialable checks that a mapped gateway
// really names loopback, so the darwin dial succeeds against a host listener.
func TestResolvePassthroughTarget_LoopbackIsDialable(t *testing.T) {
	got := resolvePassthroughTarget("192.168.100.1", 9, "192.168.100.1")
	host, _, err := net.SplitHostPort(got)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	assert.True(t, ip.IsLoopback(), "mapped gateway %q must be loopback", host)
}
