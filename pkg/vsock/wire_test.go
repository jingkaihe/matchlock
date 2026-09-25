package vsock

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecPipeBinaryStream(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	payload := bytes.Repeat([]byte{0, 255, 1, '\n'}, 20000)
	serverDone := make(chan error, 1)
	go func() {
		_, _, err := readTestFrame(guest)
		if err != nil {
			serverDone <- err
			return
		}
		for {
			_, data, err := readTestFrame(guest)
			if err != nil {
				serverDone <- err
				return
			}
			if len(data) == 0 {
				serverDone <- SendMessage(guest, MsgTypeExit, make([]byte, 4))
				return
			}
			if err := SendMessage(guest, MsgTypeStdout, data); err != nil {
				serverDone <- err
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	result, err := ExecPipe(ctx, host, "cat", &api.ExecOptions{Stdin: bytes.NewReader(payload), Stdout: &output})
	require.NoError(t, err)
	assert.Zero(t, result.ExitCode)
	assert.Equal(t, payload, output.Bytes())
	require.NoError(t, <-serverDone)
}

func TestExecPipeOutputErrors(t *testing.T) {
	failure := errors.New("disk full")
	for _, tc := range []struct {
		name string
		kind uint8
		err  error
	}{
		{"stdout", MsgTypeStdout, failure},
		{"stderr", MsgTypeStderr, failure},
		{"short write", MsgTypeStdout, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			t.Cleanup(func() { host.Close(); guest.Close() })
			go func() {
				if _, _, err := readTestFrame(guest); err == nil {
					_ = SendMessage(guest, tc.kind, []byte("image data"))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			writer := failingStreamWriter{tc.err}
			_, err := ExecPipe(ctx, host, "cat image.tar", &api.ExecOptions{Stdout: writer, Stderr: writer})
			assert.ErrorIs(t, err, ErrWriteExecOutput)
			if tc.err == nil {
				assert.ErrorIs(t, err, io.ErrShortWrite)
			} else {
				assert.ErrorIs(t, err, failure)
			}
		})
	}
}

func TestExecPipeInputError(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	go func() { _, _, _ = readTestFrame(guest) }()
	failure := errors.New("archive read failed")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ExecPipe(ctx, host, "tar -xf -", &api.ExecOptions{Stdin: failingStreamReader{failure}})
	assert.ErrorIs(t, err, ErrReadStdin)
	assert.ErrorIs(t, err, failure)
}

func TestExecPipeCancelClosesConnection(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		_, _, err := readTestFrame(guest)
		cancel()
		if err == nil {
			_, _, err = readTestFrame(guest)
		}
		serverDone <- err
	}()
	_, err := ExecPipe(ctx, host, "sleep 30", nil)
	assert.ErrorIs(t, err, context.Canceled)
	select {
	case err := <-serverDone:
		assert.ErrorIs(t, err, io.EOF)
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection remained open after cancellation")
	}
}

func TestExecPipeEarlyExitWithPendingStdin(t *testing.T) {
	for i := 0; i < 20; i++ {
		host, guest := net.Pipe()
		go func() {
			defer guest.Close()
			if _, _, err := readTestFrame(guest); err == nil {
				_ = SendMessage(guest, MsgTypeExit, make([]byte, 4))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := ExecPipe(ctx, host, "true", &api.ExecOptions{
			Stdin: bytes.NewReader(bytes.Repeat([]byte("input"), 10000)),
		})
		cancel()
		require.NoError(t, err)
		assert.Zero(t, result.ExitCode)
	}
}

func TestExecPipeCancelDuringStdinWrite(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writing := make(chan struct{}, 1)
	go func() {
		if _, _, err := readTestFrame(guest); err == nil {
			<-writing
		}
		cancel()
	}()
	_, err := ExecPipe(ctx, &stdinNotifyConn{Conn: host, writing: writing}, "sleep 30", &api.ExecOptions{
		Stdin: bytes.NewReader([]byte("input")),
	})
	assert.ErrorIs(t, err, context.Canceled)
}

type stdinNotifyConn struct {
	net.Conn
	writing chan struct{}
}

func (c *stdinNotifyConn) Write(data []byte) (int, error) {
	if len(data) == 5 && data[0] == MsgTypeStdin {
		select {
		case c.writing <- struct{}{}:
		default:
		}
	}
	return c.Conn.Write(data)
}

type failingStreamWriter struct{ err error }

func (w failingStreamWriter) Write([]byte) (int, error) { return 0, w.err }

type failingStreamReader struct{ err error }

func (r failingStreamReader) Read([]byte) (int, error) { return 0, r.err }

func readTestFrame(conn net.Conn) (uint8, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[1:]))
	_, err := io.ReadFull(conn, data)
	return header[0], data, err
}
