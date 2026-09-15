package net

import (
	"io"
	"net"
)

// halfCloser is implemented by *net.TCPConn and allows one direction of a
// connection to be shut down while the other keeps flowing, so a half-close can
// be relayed end-to-end.
type halfCloser interface {
	CloseWrite() error
}

// closeWrite propagates a half-close to c: it shuts down only the write side
// when the transport supports it (TCP), otherwise it falls back to closing the
// whole connection (e.g. net.Pipe in tests) so the peer's pending copy unblocks.
func closeWrite(c net.Conn) {
	if cw, ok := c.(halfCloser); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// relayHalfClose copies bytes in both directions between a and b, propagating
// each direction's EOF as a write-side half-close on its destination instead of
// tearing the whole relay down. It returns once BOTH directions have completed,
// so a peer that sends a request and immediately half-closes (FIN) — exactly
// what `echo PAYLOAD | nc HOST PORT` does — still receives the pending response
// from the other direction.
//
// It deliberately never sets a deadline and never cancels the pending
// direction: the first direction to see EOF must not abort the other.
//
// This is the single shared implementation used by the Linux transparent-proxy
// passthrough (proxy.go) and the darwin gVisor passthrough (stack_darwin.go) so
// the two paths cannot drift.
func relayHalfClose(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()

	<-done
	<-done
}
