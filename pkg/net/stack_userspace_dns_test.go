package net

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExchangeDNSReturnsUpstreamResponse(t *testing.T) {
	response := []byte{0xde, 0xad, 0xbe, 0xef}
	server := startDarwinStubDNSServer(t, response)

	got, err := exchangeDNS([]byte{0xca, 0xfe}, server, time.Second)

	require.NoError(t, err)
	assert.Equal(t, response, got)
}

func TestExchangeDNSTimeoutReleasesBlackholedQuery(t *testing.T) {
	seen := make(chan struct{}, 1)
	server := startDarwinBlackholeDNSServer(t, seen)

	result := make(chan error, 1)
	go func() {
		_, err := exchangeDNS([]byte{0xca, 0xfe}, server, 50*time.Millisecond)
		result <- err
	}()

	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("upstream server did not receive DNS query")
	}

	select {
	case err := <-result:
		require.Error(t, err)
		var netErr net.Error
		require.True(t, errors.As(err, &netErr), "expected net.Error, got %T: %v", err, err)
		assert.True(t, netErr.Timeout(), "expected timeout error, got %v", err)
	case <-time.After(time.Second):
		t.Fatal("DNS exchange did not return after upstream timeout")
	}
}

func startDarwinStubDNSServer(t *testing.T, response []byte) string {
	t.Helper()

	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		buf := make([]byte, 512)
		for {
			_, addr, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = ln.WriteToUDP(response, addr)
		}
	}()

	return ln.LocalAddr().String()
}

func startDarwinBlackholeDNSServer(t *testing.T, seen chan<- struct{}) string {
	t.Helper()

	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		buf := make([]byte, 512)
		for {
			_, _, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			select {
			case seen <- struct{}{}:
			default:
			}
		}
	}()

	return ln.LocalAddr().String()
}
