//go:build linux

package net

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
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
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if string(buf) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf)
	}
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
		t.Fatal("handlePassthrough should have returned quickly for blocked host")
	}

	select {
	case ev := <-tp.events:
		if !ev.Network.Blocked {
			t.Error("expected blocked event")
		}
		if ev.Network.Host != "93.184.216.34:8080" {
			t.Errorf("expected host 93.184.216.34:8080, got %s", ev.Network.Host)
		}
	default:
		t.Error("expected a blocked event to be emitted")
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
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if string(buf) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf)
	}
}

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo server: %v", err)
	}
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
