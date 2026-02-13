//go:build linux

package net

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlePassthrough_Allowed(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	go tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))

	msg := []byte("hello passthrough")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(client, buf)
	require.NoError(t, err)

	assert.Equal(t, string(msg), string(buf))
}

func TestHandlePassthrough_Blocked(t *testing.T) {
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"allowed.example.com"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "93.184.216.34", 8080)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should have returned quickly for blocked host")
	}

	select {
	case ev := <-tp.events:
		assert.True(t, ev.Network.Blocked, "expected blocked event")
		assert.Equal(t, "93.184.216.34:8080", ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}
}

func TestHandlePassthrough_EmptyAllowlist(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	go tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))

	msg := []byte("open policy")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	client.Write(msg)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadFull(client, buf)
	require.NoError(t, err)

	assert.Equal(t, string(msg), string(buf))
}

func TestHandlePassthrough_UpstreamRefused(t *testing.T) {
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	// Use a port with nothing listening — connection refused
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", 1)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should return when upstream is unreachable")
	}
}

func TestHandlePassthrough_HalfClose(t *testing.T) {
	// Start a server that writes a response then closes its write side
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read one message, echo it, then close
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
		conn.Close()
	}()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))
		close(done)
	}()

	msg := []byte("ping")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	client.Write(msg)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.ReadFull(client, buf)

	assert.Equal(t, string(msg), string(buf))

	// handlePassthrough should exit cleanly after upstream closes
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should have exited after upstream closed")
	}
}

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

func mustAtoi(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
