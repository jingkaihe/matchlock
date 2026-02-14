package net

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDNSProxy_InvalidConfig(t *testing.T) {
	_, err := NewDNSProxy(nil)
	require.ErrorIs(t, err, ErrDNSProxyConfig)
}

func TestDNSProxy_ForwardsUDP(t *testing.T) {
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstream.Close()

	go func() {
		buf := make([]byte, 1024)
		n, addr, readErr := upstream.ReadFrom(buf)
		if readErr != nil {
			return
		}
		resp := append([]byte("ok:"), buf[:n]...)
		_, _ = upstream.WriteTo(resp, addr)
	}()

	proxy, err := NewDNSProxy(&DNSProxyConfig{
		BindAddr:       "127.0.0.1",
		Port:           0,
		UpstreamServer: []string{upstream.LocalAddr().String()},
	})
	require.NoError(t, err)
	proxy.Start()
	defer func() { require.NoError(t, proxy.Close()) }()

	client, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write([]byte("query"))
	require.NoError(t, err)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))

	resp := make([]byte, 1024)
	n, err := client.Read(resp)
	require.NoError(t, err)
	assert.Equal(t, "ok:query", string(resp[:n]))
}

func TestDNSProxy_ForwardsTCP(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstream.Close()

	go func() {
		conn, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	proxy, err := NewDNSProxy(&DNSProxyConfig{
		BindAddr:       "127.0.0.1",
		Port:           0,
		UpstreamServer: []string{upstream.Addr().String()},
	})
	require.NoError(t, err)
	proxy.Start()
	defer func() { require.NoError(t, proxy.Close()) }()

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	require.NoError(t, err)
	defer client.Close()

	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = client.Write([]byte("dns-tcp"))
	require.NoError(t, err)

	resp := make([]byte, 7)
	_, err = io.ReadFull(client, resp)
	require.NoError(t, err)
	assert.Equal(t, "dns-tcp", string(resp))
}

func TestDNSProxy_UDPFallbackOnServfail(t *testing.T) {
	upstreamServfail, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstreamServfail.Close()

	upstreamOK, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstreamOK.Close()

	go func() {
		buf := make([]byte, 1024)
		_, addr, readErr := upstreamServfail.ReadFrom(buf)
		if readErr != nil {
			return
		}
		// DNS SERVFAIL (rcode=2)
		_, _ = upstreamServfail.WriteTo([]byte{0, 0, 0, 2}, addr)
	}()

	go func() {
		buf := make([]byte, 1024)
		n, addr, readErr := upstreamOK.ReadFrom(buf)
		if readErr != nil {
			return
		}
		resp := append([]byte("ok:"), buf[:n]...)
		_, _ = upstreamOK.WriteTo(resp, addr)
	}()

	proxy, err := NewDNSProxy(&DNSProxyConfig{
		BindAddr:       "127.0.0.1",
		Port:           0,
		UpstreamServer: []string{upstreamServfail.LocalAddr().String(), upstreamOK.LocalAddr().String()},
	})
	require.NoError(t, err)
	proxy.Start()
	defer func() { require.NoError(t, proxy.Close()) }()

	client, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write([]byte("query"))
	require.NoError(t, err)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))

	resp := make([]byte, 1024)
	n, err := client.Read(resp)
	require.NoError(t, err)
	assert.Equal(t, "ok:query", string(resp[:n]))
}
