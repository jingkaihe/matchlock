//go:build linux

package qemu

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeNetConn records each Write call so we can assert single-wire frames and
// short-write handling. It is a plain net.Conn.
type fakeNetConn struct {
	mu     sync.Mutex
	writes [][]byte
	limit  int // >0: cap total bytes accepted (to force a short write)
	total  int
}

func (f *fakeNetConn) Read(p []byte) (int, error)         { return 0, io.EOF }
func (f *fakeNetConn) Close() error                       { return nil }
func (f *fakeNetConn) LocalAddr() net.Addr                { return &net.IPAddr{} }
func (f *fakeNetConn) RemoteAddr() net.Addr               { return &net.IPAddr{} }
func (f *fakeNetConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeNetConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeNetConn) SetWriteDeadline(t time.Time) error { return nil }

func (f *fakeNetConn) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(p))
	copy(c, p)
	if f.limit > 0 {
		avail := f.limit - f.total
		if avail < len(c) {
			c = c[:avail]
		}
	}
	f.writes = append(f.writes, c)
	f.total += len(c)
	return len(c), nil
}

// writeFrameOn writes a single frame on an arbitrary net.Conn under a mutex.
// This mirrors vsockConn.writeFrame's semantics so we can test it without the
// concrete *socket.Conn.
func writeFrameOn(conn net.Conn, mu *sync.Mutex, msgType uint8, data []byte) (int, error) {
	header := make([]byte, 5)
	header[0] = msgType
	header[1] = byte(len(data) >> 24)
	header[2] = byte(len(data) >> 16)
	header[3] = byte(len(data) >> 8)
	header[4] = byte(len(data))
	buf := append(header, data...)

	mu.Lock()
	defer mu.Unlock()
	n, err := conn.Write(buf)
	if err == nil && n < len(buf) {
		err = io.ErrShortWrite
	}
	return n, err
}

func TestWriteFrameSingleWire(t *testing.T) {
	fc := &fakeNetConn{}
	mu := &sync.Mutex{}
	_, err := writeFrameOn(fc, mu, 0x07, []byte("hello"))
	require.NoError(t, err)
	require.Len(t, fc.writes, 1, "must be a single wire write")
	require.Equal(t, byte(0x07), fc.writes[0][0])
	require.True(t, bytes.Equal(fc.writes[0][5:], []byte("hello")))
}

func TestWriteFrameShortWrite(t *testing.T) {
	fc := &fakeNetConn{limit: 3}
	mu := &sync.Mutex{}
	_, err := writeFrameOn(fc, mu, 0x08, []byte("hello"))
	require.ErrorIs(t, err, io.ErrShortWrite)
}

func TestWriteFrameConcurrentNoInterleave(t *testing.T) {
	fc := &fakeNetConn{}
	mu := &sync.Mutex{}
	done := make(chan struct{})
	const n = 200
	for i := 0; i < n; i++ {
		go func(i int) {
			payload := []byte{byte(i), byte(i + 1), byte(i + 2)}
			_, _ = writeFrameOn(fc, mu, 0x01, payload)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	require.Len(t, fc.writes, n, "each frame must be one wire write")
	for _, w := range fc.writes {
		require.Len(t, w, 8, "frame = 5-byte header + 3-byte payload")
		require.Equal(t, byte(0x01), w[0])
		require.Equal(t, uint32(3), uint32(w[1])<<24|uint32(w[2])<<16|uint32(w[3])<<8|uint32(w[4]))
	}
}
