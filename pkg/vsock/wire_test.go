package vsock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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

func TestExecStreamsWithoutRetainingWriterOutput(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	payload := bytes.Repeat([]byte{0, 255, 1, '\n'}, 20000)
	serverDone := make(chan error, 1)
	go func() {
		msgType, _, err := readTestFrame(guest)
		if err == nil && msgType != MsgTypeExecStream {
			err = errors.New("expected streaming exec request")
		}
		for _, frame := range []struct {
			kind uint8
			data []byte
		}{{MsgTypeStdout, payload[:4096]}, {MsgTypeStderr, []byte("warn\n")}, {MsgTypeStdout, payload[4096:]}} {
			if err == nil {
				err = SendMessage(guest, frame.kind, frame.data)
			}
		}
		if err == nil {
			err = sendTestExecResult(guest, ExecResponse{ExitCode: 3})
		}
		serverDone <- err
	}()
	var stdout bytes.Buffer
	result, err := Exec(context.Background(), host, "cat image.tar", &api.ExecOptions{Stdout: &stdout})
	require.NoError(t, err)
	require.NoError(t, <-serverDone)
	assert.Equal(t, 3, result.ExitCode)
	assert.Equal(t, payload, stdout.Bytes())
	assert.Empty(t, result.Stdout, "streamed output must not also be retained in memory")
	assert.Equal(t, "warn\n", string(result.Stderr), "streams without a writer stay buffered")
}

func TestExecBuffersWithoutWriters(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	go func() {
		msgType, _, err := readTestFrame(guest)
		if err != nil || msgType != MsgTypeExec {
			return
		}
		_ = sendTestExecResult(guest, ExecResponse{Stdout: []byte("batch out"), Stderr: []byte("batch err"), Error: "start failed", ExitCode: 1})
	}()
	result, err := Exec(context.Background(), host, "missing", nil)
	assert.ErrorIs(t, err, ErrExecRemote)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.ExitCode)
	assert.Equal(t, "batch out", string(result.Stdout))
	assert.Equal(t, "batch err", string(result.Stderr))
}

func TestExecOutputWriteErrorClosesConnection(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	serverDone := make(chan error, 1)
	go func() {
		_, _, err := readTestFrame(guest)
		if err == nil {
			err = SendMessage(guest, MsgTypeStdout, []byte("image data"))
		}
		if err == nil {
			_, _, err = readTestFrame(guest)
		}
		serverDone <- err
	}()
	failure := errors.New("disk full")
	_, err := Exec(context.Background(), host, "cat image.tar", &api.ExecOptions{Stdout: failingStreamWriter{failure}})
	assert.ErrorIs(t, err, ErrWriteExecOutput)
	assert.ErrorIs(t, err, failure)
	select {
	case err := <-serverDone:
		assert.ErrorIs(t, err, io.EOF, "guest must observe the closed connection")
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection remained open after output failure")
	}
}

func TestExecCancelClosesConnection(t *testing.T) {
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
	_, err := Exec(ctx, host, "sleep 30", &api.ExecOptions{Stdout: io.Discard})
	assert.ErrorIs(t, err, context.Canceled)
	select {
	case err := <-serverDone:
		assert.ErrorIs(t, err, io.EOF)
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection remained open after cancellation")
	}
}

func sendTestExecResult(conn net.Conn, resp ExecResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return SendMessage(conn, MsgTypeExecResult, data)
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
