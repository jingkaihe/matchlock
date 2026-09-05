//go:build linux

package qemu

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

// vsockConn adapts a *socket.Conn (mdlayher/socket) which satisfies net.Conn's
// Read/Write/Close/SetDeadline but not LocalAddr/RemoteAddr. We supply those
// from the sockaddr_vm.
type vsockConn struct {
	*socket.Conn
	local, remote net.Addr
	// writeMu serializes message writes on this connection. It is per-connection
	// (session-local), so it never serializes unrelated sandboxes.
	writeMu sync.Mutex
}

// writeFrame writes one vsock message frame (type + 4-byte big-endian length +
// payload) as a SINGLE wire write, protected by the connection's writeMu so the
// concurrent stdin/resize/signal goroutines of an interactive exec cannot split
// or interleave a frame. It returns the number of bytes written and an error.
func (c *vsockConn) writeFrame(msgType uint8, data []byte) (int, error) {
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	buf := make([]byte, 0, len(header)+len(data))
	buf = append(buf, header...)
	buf = append(buf, data...)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.Conn.Write(buf)
	if err == nil && n < len(buf) {
		// A short write on a stream socket would silently drop the tail of the
		// frame, corrupting the framing. Treat it as an error.
		err = io.ErrShortWrite
	}
	return n, err
}

// LocalAddr returns the local AF_VSOCK address.
func (c *vsockConn) LocalAddr() net.Addr { return c.local }

// RemoteAddr returns the remote AF_VSOCK address.
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

var _ net.Conn = (*vsockConn)(nil)

// vsockAddr implements net.Addr for a SockaddrVM.
type vsockAddr struct {
	cid, port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

func toVsockAddr(sa unix.Sockaddr) net.Addr {
	vm, ok := sa.(*unix.SockaddrVM)
	if !ok || vm == nil {
		return vsockAddr{}
	}
	return vsockAddr{cid: vm.CID, port: vm.Port}
}

// DialVsock connects to a guest CID+port over AF_VSOCK using mdlayher/socket
// (which registers the fd with the runtime poller so deadlines and cancellation
// work). It returns a net.Conn with working local/remote addresses.
func DialVsock(ctx context.Context, cid, port uint32) (net.Conn, error) {
	conn, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}

	sa := &unix.SockaddrVM{CID: cid, Port: port}
	remote, err := conn.Connect(ctx, sa)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("connect(%d:%d): %w", cid, port, err)
	}

	local, err := conn.Getsockname()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("getsockname(%d:%d): %w", cid, port, err)
	}

	return &vsockConn{
		Conn:   conn,
		local:  toVsockAddr(local),
		remote: toVsockAddr(remote),
	}, nil
}

// ListenVsock binds an AF_VSOCK listener on the given CID+port (used for
// guest→host services) and returns the listener plus the actually-bound port.
// When port is VMADDR_PORT_ANY the kernel auto-assigns a distinct 32-bit port,
// which is how concurrent QEMU sandboxes get isolated listeners without
// collision. The caller must use the returned boundPort to tell its guest which
// port to dial. Returns a fail-closed error if the kernel does not return a
// concrete assigned port.
func ListenVsock(ctx context.Context, cid, port uint32) (net.Listener, uint32, error) {
	conn, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
	if err != nil {
		return nil, 0, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	sa := &unix.SockaddrVM{CID: cid, Port: port}
	if err := conn.Bind(sa); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("bind(%d:%d): %w", cid, port, err)
	}
	// Read back the kernel-assigned port (when port was VMADDR_PORT_ANY).
	bound, err := conn.Getsockname()
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("getsockname: %w", err)
	}
	bvm, ok := bound.(*unix.SockaddrVM)
	if !ok || bvm == nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("bind: expected SockaddrVM, got %T", bound)
	}
	boundPort := bvm.Port
	if boundPort == VMADDR_PORT_ANY || boundPort == 0 {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("bind: kernel returned no concrete vsock port (%d)", boundPort)
	}
	if err := conn.Listen(8); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("listen(%d:%d): %w", cid, boundPort, err)
	}
	return &vsockListener{Conn: conn, addr: vsockAddr{cid: cid, port: boundPort}}, boundPort, nil
}

// VMADDR_CID_ANY is the wildcard CID used to bind a listener that accepts
// connections from any guest. On the QEMU vhost-vsock path the sandbox binds
// VMADDR_CID_ANY and uses peer-CID filtering plus a kernel-assigned distinct
// port for per-sandbox isolation.
const VMADDR_CID_ANY = 0xffffffff

// VMADDR_PORT_ANY requests that the kernel assign a distinct port automatically
// on bind. This is the AF_VSOCK equivalent of an ephemeral TCP port.
const VMADDR_PORT_ANY = 0xffffffff

// peerFilteredListener wraps a net.Listener and only returns accepted
// connections whose remote CID matches expectedCID. It is fail-closed: a
// connection whose CID can't be determined, or that doesn't match, is closed
// and never surfaced to the VFS server.
type peerFilteredListener struct {
	net.Listener
	expectedCID uint32
}

func (p *peerFilteredListener) Accept() (net.Conn, error) {
	for {
		conn, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if peerMatches(conn, p.expectedCID) {
			return conn, nil
		}
		// Not this sandbox's guest (or CID unknown): drop and keep listening.
		_ = conn.Close()
		continue
	}
}

// peerMatches reports whether conn's remote CID equals expectedCID. It is a
// pure predicate over the connection's address so the cross-guest isolation
// rule can be unit-tested without a real AF_VSOCK socket. Any connection that is
// not a usable *vsockConn, has no remote address, or whose remote is not a
// vsockAddr is treated as non-matching (fail-closed).
func peerMatches(conn net.Conn, expectedCID uint32) bool {
	vc, ok := conn.(*vsockConn)
	if !ok || vc == nil || vc.remote == nil {
		return false
	}
	addr, ok := vc.remote.(vsockAddr)
	return ok && addr.cid == expectedCID
}

// vsockListener implements net.Listener over an AF_VSOCK socket.Conn.
type vsockListener struct {
	*socket.Conn
	addr net.Addr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	c, sa, err := l.Conn.Accept(context.Background(), 0)
	if err != nil {
		return nil, err
	}
	return &vsockConn{Conn: c, local: l.addr, remote: toVsockAddr(sa)}, nil
}

func (l *vsockListener) Addr() net.Addr { return l.addr }
