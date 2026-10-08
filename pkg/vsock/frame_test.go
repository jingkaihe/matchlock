package vsock

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frame is a parsed vsock frame.
type frame struct {
	typ  uint8
	data []byte
}

// parseFrames decodes a concatenation of vsock frames (type + 4-byte big-endian
// length + payload) and returns them. It fails on a truncated header or a
// declared length that runs past the end of the stream, which is exactly how a
// header/payload interleave manifests.
func parseFrames(stream []byte) ([]frame, error) {
	var frames []frame
	for len(stream) > 0 {
		if len(stream) < 5 {
			return frames, fmt.Errorf("frame %d: truncated header (%d bytes left)", len(frames), len(stream))
		}
		length := binary.BigEndian.Uint32(stream[1:5])
		if int(length) > len(stream)-5 {
			return frames, fmt.Errorf("frame %d: length %d exceeds remaining %d", len(frames), length, len(stream)-5)
		}
		data := make([]byte, length)
		copy(data, stream[5:5+int(length)])
		frames = append(frames, frame{typ: stream[0], data: data})
		stream = stream[5+int(length):]
	}
	return frames, nil
}

func frameKey(f frame) string {
	return fmt.Sprintf("%d:%x", f.typ, f.data)
}

func frameMultiset(frames []frame) map[string]int {
	m := make(map[string]int, len(frames))
	for _, f := range frames {
		m[frameKey(f)]++
	}
	return m
}

func assertFrameMultiset(t *testing.T, expected map[string]int, frames []frame) {
	t.Helper()
	assert.Equal(t, expected, frameMultiset(frames))
}

// testFrameType returns a stable frame type for sequence index i.
func testFrameType(i int) uint8 {
	if i%3 == 1 {
		return MsgTypeResize
	}
	return MsgTypeStdin
}

func testFrameData(prefix string, g, i int) []byte {
	return []byte(fmt.Sprintf("%s-g%02d-i%03d", prefix, g, i))
}

// interleaveConn is a deterministic in-memory net.Conn that records every Write
// call.
//
// It exposes the header/payload interleave that a two-write sender
// (SendMessage) is vulnerable to: the first payload-only write blocks until a
// second header-only write has arrived, so both headers land before either
// payload. A single-write sender (FrameWriter) never emits a bare 5-byte header,
// so the gate never fires and every recorded write is a whole frame.
type interleaveConn struct {
	mu           sync.Mutex
	stream       []byte
	writes       [][]byte
	headerWrites int
	gate         chan struct{}
	gateOnce     sync.Once
}

func newInterleaveConn() *interleaveConn {
	return &interleaveConn{gate: make(chan struct{})}
}

func (c *interleaveConn) Write(p []byte) (int, error) {
	buf := append([]byte(nil), p...)

	c.mu.Lock()
	c.writes = append(c.writes, buf)
	isHeader := len(buf) == 5
	if isHeader {
		c.headerWrites++
		if c.headerWrites >= 2 {
			c.gateOnce.Do(func() { close(c.gate) })
		}
	}
	shouldWait := !isHeader && c.headerWrites == 1
	gate := c.gate
	c.mu.Unlock()

	if shouldWait {
		<-gate
	}

	c.mu.Lock()
	c.stream = append(c.stream, buf...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *interleaveConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *interleaveConn) Close() error                     { return nil }
func (c *interleaveConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *interleaveConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *interleaveConn) SetDeadline(time.Time) error      { return nil }
func (c *interleaveConn) SetReadDeadline(time.Time) error  { return nil }
func (c *interleaveConn) SetWriteDeadline(time.Time) error { return nil }

func (c *interleaveConn) snapshotStream() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.stream...)
}

func (c *interleaveConn) snapshotWrites() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

func (c *interleaveConn) headerWriteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headerWrites
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

var _ net.Conn = (*interleaveConn)(nil)

// shortWriteConn truncates every write by one byte to exercise the short-write
// detection in FrameWriter.
type shortWriteConn struct {
	interleaveConn
}

func (c *shortWriteConn) Write(p []byte) (int, error) {
	if _, err := c.interleaveConn.Write(p); err != nil {
		return 0, err
	}
	return len(p) - 1, nil
}

// assertOneFramePerWrite asserts each recorded Write is exactly one complete
// frame and returns the parsed frames.
func assertOneFramePerWrite(t *testing.T, writes [][]byte) []frame {
	t.Helper()
	frames := make([]frame, 0, len(writes))
	for i, w := range writes {
		require.GreaterOrEqual(t, len(w), 5, "write %d is shorter than a header", i)
		length := int(binary.BigEndian.Uint32(w[1:5]))
		require.Equal(t, 5+length, len(w), "write %d is not a whole frame", i)
		data := make([]byte, length)
		copy(data, w[5:])
		frames = append(frames, frame{typ: w[0], data: data})
	}
	return frames
}

func TestFrameWriter_SendWritesSingleCompleteFrame(t *testing.T) {
	conn := newInterleaveConn()
	fw := NewFrameWriter(conn)

	sent := []frame{
		{typ: MsgTypeStdin, data: []byte("hello")},
		{typ: MsgTypeResize, data: []byte{0, 40, 0, 100}},
		{typ: MsgTypeSignal, data: []byte{}},
	}
	for _, f := range sent {
		require.NoError(t, fw.Send(f.typ, f.data))
	}

	writes := conn.snapshotWrites()
	require.Len(t, writes, len(sent), "FrameWriter must issue exactly one Write per frame")

	got := assertOneFramePerWrite(t, writes)
	require.Equal(t, sent, got)

	parsed, err := parseFrames(conn.snapshotStream())
	require.NoError(t, err)
	require.Equal(t, got, parsed)
}

func TestFrameWriter_ConcurrentFramesDoNotInterleave(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50
	total := goroutines * perGoroutine

	conn := newInterleaveConn()
	fw := NewFrameWriter(conn)

	expected := make(map[string]int, total)
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			expected[frameKey(frame{typ: testFrameType(i), data: testFrameData("fake", g, i)})]++
		}
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				assert.NoError(t, fw.Send(testFrameType(i), testFrameData("fake", g, i)))
			}
		}(g)
	}
	wg.Wait()

	// FrameWriter never emits a bare header, so the interleave gate must not
	// have fired.
	assert.Equal(t, 0, conn.headerWriteCount(), "FrameWriter wrote a bare header")

	writes := conn.snapshotWrites()
	require.Len(t, writes, total, "concurrent FrameWriter sends must be exactly one Write per frame")
	assertOneFramePerWrite(t, writes)

	parsed, err := parseFrames(conn.snapshotStream())
	require.NoError(t, err)
	require.Len(t, parsed, total)
	assertFrameMultiset(t, expected, parsed)
}

// TestSendMessage_ConcurrentFramesInterleave demonstrates the regression that
// motivated FrameWriter: the legacy two-write SendMessage path lets a competing
// frame's header land between another frame's header and payload, so the stream
// no longer parses back to the frames that were sent.
func TestSendMessage_ConcurrentFramesInterleave(t *testing.T) {
	const goroutines = 2
	const perGoroutine = 20
	total := goroutines * perGoroutine

	conn := newInterleaveConn()

	expected := make(map[string]int, total)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				assert.NoError(t, SendMessage(conn, testFrameType(i), testFrameData("legacy", g, i)))
				mu.Lock()
				expected[frameKey(frame{typ: testFrameType(i), data: testFrameData("legacy", g, i)})]++
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	// The deterministic fake forces two header-only writes before the first
	// payload, i.e. the interleave really happened.
	require.GreaterOrEqual(t, conn.headerWriteCount(), 2)

	parsed, err := parseFrames(conn.snapshotStream())
	if err == nil {
		require.NotEqual(t, expected, frameMultiset(parsed),
			"SendMessage interleave unexpectedly parsed back to the exact frames")
	}
}

func TestFrameWriter_ConcurrentOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	const goroutines = 8
	const perGoroutine = 200
	total := goroutines * perGoroutine

	expected := make(map[string]int, total)
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			expected[frameKey(frame{typ: testFrameType(i), data: testFrameData("tcp", g, i)})]++
		}
	}

	type readResult struct {
		frames []frame
		err    error
	}
	resCh := make(chan readResult, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			resCh <- readResult{err: err}
			return
		}
		defer c.Close()
		frames := make([]frame, 0, total)
		for i := 0; i < total; i++ {
			hdr := make([]byte, 5)
			if _, err := io.ReadFull(c, hdr); err != nil {
				resCh <- readResult{frames: frames, err: err}
				return
			}
			length := binary.BigEndian.Uint32(hdr[1:])
			data := make([]byte, length)
			if _, err := io.ReadFull(c, data); err != nil {
				resCh <- readResult{frames: frames, err: err}
				return
			}
			frames = append(frames, frame{typ: hdr[0], data: data})
		}
		resCh <- readResult{frames: frames}
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer clientConn.Close()

	fw := NewFrameWriter(clientConn)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				assert.NoError(t, fw.Send(testFrameType(i), testFrameData("tcp", g, i)))
			}
		}(g)
	}
	wg.Wait()

	select {
	case res := <-resCh:
		require.NoError(t, res.err)
		require.Len(t, res.frames, total)
		assertFrameMultiset(t, expected, res.frames)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the TCP frame reader")
	}
}

func TestFrameWriter_ShortWriteReturnsErrShortWrite(t *testing.T) {
	conn := &shortWriteConn{}
	fw := NewFrameWriter(conn)

	err := fw.Send(MsgTypeStdin, []byte("payload"))
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrShortWrite)
}
