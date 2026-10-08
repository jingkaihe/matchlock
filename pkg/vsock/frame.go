package vsock

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
)

// FrameWriter serializes framed vsock message writes onto one net.Conn.
//
// The interactive exec path sends MsgTypeStdin, MsgTypeResize and MsgTypeSignal
// frames from independent goroutines over the same connection. SendMessage
// issues the 5-byte header and the payload as two separate conn.Write calls, so
// concurrent senders can interleave one frame's header with another frame's
// payload and desync the guest frame parser (readMessage then reads a bogus
// length and blocks).
//
// FrameWriter builds each frame (header first, then payload) into a single
// buffer and performs one conn.Write under a per-connection mutex, so a frame
// reaches the wire as one contiguous unit. Create one per session/connection;
// it is safe for concurrent use.
type FrameWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

// NewFrameWriter returns a FrameWriter that serializes frames on conn.
func NewFrameWriter(conn net.Conn) *FrameWriter {
	return &FrameWriter{conn: conn}
}

// Send writes one vsock frame (1-byte type + 4-byte big-endian length +
// payload) as a single locked conn.Write. It returns io.ErrShortWrite when the
// write completes with fewer bytes than the frame, since a stream socket would
// silently drop the tail of a short write and corrupt the framing.
func (w *FrameWriter) Send(msgType uint8, data []byte) error {
	buf := make([]byte, 5+len(data))
	buf[0] = msgType
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(data)))
	copy(buf[5:], data)

	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.conn.Write(buf)
	if err == nil && n < len(buf) {
		err = io.ErrShortWrite
	}
	return err
}
