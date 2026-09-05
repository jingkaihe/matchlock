//go:build linux

package qemu

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPeerMatches is a hardware-independent unit test of the cross-guest VFS
// isolation predicate. peerMatches(conn, expectedCID) must accept ONLY a
// connection whose remote CID equals expectedCID; everything else is rejected
// fail-closed. This is the security boundary that keeps sandbox B's guest from
// reaching sandbox A's VFS server, and it must hold regardless of AF_VSOCK
// availability on the host.
func TestPeerMatches(t *testing.T) {
	// Positive: the exact guest CID is accepted.
	assert.True(t, peerMatches(&vsockConn{remote: vsockAddr{cid: 3, port: 100}}, 3),
		"the sandbox's own guest CID must be accepted")

	// Foreign CID (a different sandbox's guest) is rejected.
	assert.False(t, peerMatches(&vsockConn{remote: vsockAddr{cid: 9, port: 100}}, 3),
		"a different guest CID must be rejected")

	// Missing remote address: fail-closed.
	assert.False(t, peerMatches(&vsockConn{remote: nil}, 3),
		"no remote addr must be rejected (fail-closed)")

	// Host CID (2) dialing a guest-CID filter (3) is a foreign peer: rejected.
	assert.False(t, peerMatches(&vsockConn{remote: vsockAddr{cid: 2, port: 100}}, 3),
		"host-CID source must be rejected")

	// A *non-vsockConn* (e.g. an arbitrary net.Conn from another backend) cannot
	// be classified as a vsock peer; it must be rejected fail-closed.
	var nilConn net.Conn
	assert.False(t, peerMatches(nilConn, 3), "nil conn must be rejected")

	// A typed-nil *vsockConn is NOT a usable peer; it must not panic and must be
	// rejected (fail-closed).
	var typedNil *vsockConn
	assert.False(t, peerMatches(typedNil, 3), "typed-nil vsockConn must be rejected")
}
