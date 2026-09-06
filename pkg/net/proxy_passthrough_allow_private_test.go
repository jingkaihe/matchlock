//go:build linux

package net

import (
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// otherPort returns a port number distinct from port and within the valid
// range, for the "wrong port" cases.
func otherPort(port int) int {
	if port >= 65535 {
		return port - 1
	}
	return port + 1
}

// TestHandlePassthrough_AllowPrivate_PortMatchRelays proves that a private
// destination with BlockPrivateIPs enabled is relayed when the ORIGINAL
// destination IP:port matches a port-scoped allow_private entry.
func TestHandlePassthrough_AllowPrivate_PortMatchRelays(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())
	port := mustAtoi(portStr)

	var dialed atomic.Value // string
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{net.JoinHostPort("127.0.0.1", portStr)},
		}),
		events: make(chan api.Event, 10),
		dial: func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			return net.Dial(network, addr)
		},
	}

	client, server := net.Pipe()
	defer client.Close()

	go tp.handlePassthrough(server, "127.0.0.1", port)

	msg := []byte("hello allow_private passthrough")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(client, buf)
	require.NoError(t, err)

	assert.Equal(t, string(msg), string(buf), "listed private IP:port must relay")
	assert.Equal(t, net.JoinHostPort("127.0.0.1", portStr), dialed.Load().(string),
		"allow_private passthrough must dial the original destination")
}

// TestHandlePassthrough_AllowPrivate_NoEntryRefused proves the same private
// destination is still refused when allow_private carries no entry: the block
// wins, nothing is dialed and no payload is delivered.
func TestHandlePassthrough_AllowPrivate_NoEntryRefused(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())
	port := mustAtoi(portStr)

	var dials atomic.Int64
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true}),
		events: make(chan api.Event, 10),
		dial: func(network, addr string) (net.Conn, error) {
			dials.Add(1)
			return net.Dial(network, addr)
		},
	}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", port)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should return quickly for a blocked private destination")
	}

	assert.Zero(t, dials.Load(), "a refused private destination must never be dialed")

	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write([]byte("should not be relayed"))
	assert.Error(t, err, "refused passthrough must have closed the guest connection")

	select {
	case ev := <-tp.events:
		assert.True(t, ev.Network.Blocked, "expected blocked event")
		assert.Equal(t, net.JoinHostPort("127.0.0.1", portStr), ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}
}

// TestHandlePassthrough_AllowPrivate_WrongPortRefused proves the exception is
// port-scoped: an entry for a different port does not lift the block.
func TestHandlePassthrough_AllowPrivate_WrongPortRefused(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())
	port := mustAtoi(portStr)
	wrongPort := otherPort(port)

	var dials atomic.Int64
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(wrongPort))},
		}),
		events: make(chan api.Event, 10),
		dial: func(network, addr string) (net.Conn, error) {
			dials.Add(1)
			return net.Dial(network, addr)
		},
	}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", port)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should return quickly when only a different port is listed")
	}

	assert.Zero(t, dials.Load(), "a wrong-port private destination must never be dialed")

	select {
	case ev := <-tp.events:
		assert.True(t, ev.Network.Blocked, "expected blocked event")
		assert.Equal(t, net.JoinHostPort("127.0.0.1", portStr), ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}
}
