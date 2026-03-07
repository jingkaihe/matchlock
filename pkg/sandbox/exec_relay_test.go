package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/require"
)

type fakeMachine struct {
	execStarted chan struct{}
	ctxCanceled chan struct{}
	release     chan struct{}
}

var _ vm.Machine = (*fakeMachine)(nil)

func newFakeMachine() *fakeMachine {
	return &fakeMachine{
		execStarted: make(chan struct{}),
		ctxCanceled: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (m *fakeMachine) Start(ctx context.Context) error { return nil }
func (m *fakeMachine) Stop(ctx context.Context) error  { return nil }
func (m *fakeMachine) Wait(ctx context.Context) error  { return nil }

func (m *fakeMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	close(m.execStarted)
	select {
	case <-ctx.Done():
		close(m.ctxCanceled)
		return nil, ctx.Err()
	case <-m.release:
		return &api.ExecResult{ExitCode: 0}, nil
	}
}

func (m *fakeMachine) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (m *fakeMachine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return nil, nil
}
func (m *fakeMachine) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	return nil, nil
}
func (m *fakeMachine) NetworkFD() (int, error) { return 0, nil }
func (m *fakeMachine) VsockFD() (int, error)   { return 0, nil }
func (m *fakeMachine) PID() int                { return 0 }
func (m *fakeMachine) Close(ctx context.Context) error {
	return nil
}
func (m *fakeMachine) RootfsPath() string { return "" }

type fakeInteractiveMachine struct {
	execStarted chan struct{}
	stdinData   chan []byte
	stdinClosed chan struct{}
	resizes     chan [2]uint16
}

var _ vm.InteractiveMachine = (*fakeInteractiveMachine)(nil)

func newFakeInteractiveMachine() *fakeInteractiveMachine {
	return &fakeInteractiveMachine{
		execStarted: make(chan struct{}),
		stdinData:   make(chan []byte, 1),
		stdinClosed: make(chan struct{}),
		resizes:     make(chan [2]uint16, 4),
	}
}

func (m *fakeInteractiveMachine) Start(ctx context.Context) error { return nil }
func (m *fakeInteractiveMachine) Stop(ctx context.Context) error  { return nil }
func (m *fakeInteractiveMachine) Wait(ctx context.Context) error  { return nil }

func (m *fakeInteractiveMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	return &api.ExecResult{ExitCode: 0}, nil
}

func (m *fakeInteractiveMachine) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	close(m.execStarted)

	// Forward resize events so tests can observe them.
	go func() {
		for size := range resizeCh {
			m.resizes <- size
		}
	}()

	data, _ := io.ReadAll(stdin)
	m.stdinData <- data
	close(m.stdinClosed)
	return 0, nil
}

func (m *fakeInteractiveMachine) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (m *fakeInteractiveMachine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return nil, nil
}
func (m *fakeInteractiveMachine) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	return nil, nil
}
func (m *fakeInteractiveMachine) NetworkFD() (int, error) { return 0, nil }
func (m *fakeInteractiveMachine) VsockFD() (int, error)   { return 0, nil }
func (m *fakeInteractiveMachine) PID() int                { return 0 }
func (m *fakeInteractiveMachine) Close(ctx context.Context) error {
	return nil
}
func (m *fakeInteractiveMachine) RootfsPath() string { return "" }

func TestExecRelayPipeStdinEOFDoesNotCancel(t *testing.T) {
	machine := newFakeMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	reqData, err := json.Marshal(relayExecRequest{Command: "noop"})
	require.NoError(t, err, "marshal request")

	done := make(chan struct{})
	go func() {
		relay.handleExecPipe(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	require.NoError(t, sendRelayMsg(clientConn, relayMsgStdin, nil), "send stdin EOF")

	select {
	case <-machine.ctxCanceled:
		require.Fail(t, "context canceled on stdin EOF")
	case <-time.After(200 * time.Millisecond):
	}

	close(machine.release)

	exitErr := make(chan error, 1)
	go func() {
		msgType, data, err := readRelayMsg(clientConn)
		if err != nil {
			exitErr <- err
			return
		}
		require.Equal(t, relayMsgExit, msgType)
		require.Len(t, data, 4)
		exitErr <- nil
	}()

	select {
	case err := <-exitErr:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for exit")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for relay")
	}
}

func TestExecRelayPipeDisconnectCancels(t *testing.T) {
	machine := newFakeMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	reqData, err := json.Marshal(relayExecRequest{Command: "noop"})
	require.NoError(t, err, "marshal request")

	done := make(chan struct{})
	go func() {
		relay.handleExecPipe(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	require.NoError(t, clientConn.Close(), "close client conn")

	select {
	case <-machine.ctxCanceled:
	case <-time.After(1 * time.Second):
		require.Fail(t, "expected context cancellation on disconnect")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for relay")
	}
}

func TestExecRelayInteractiveDisconnectClosesStdin(t *testing.T) {
	machine := newFakeInteractiveMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	reqData, err := json.Marshal(relayExecInteractiveRequest{Command: "noop", Rows: 24, Cols: 80})
	require.NoError(t, err, "marshal request")

	done := make(chan struct{})
	go func() {
		relay.handleExecInteractive(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	require.NoError(t, sendRelayMsg(clientConn, relayMsgStdin, []byte("hello")), "send stdin")
	require.NoError(t, clientConn.Close(), "close client conn")

	select {
	case data := <-machine.stdinData:
		require.Equal(t, "hello", string(data), "unexpected stdin data")
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for stdin data")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for relay")
	}
}

func TestExecRelayInteractiveResizeForwardedToMachine(t *testing.T) {
	machine := newFakeInteractiveMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	reqData, err := json.Marshal(relayExecInteractiveRequest{Command: "noop", Rows: 24, Cols: 80})
	require.NoError(t, err, "marshal request")

	done := make(chan struct{})
	go func() {
		relay.handleExecInteractive(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	// Send a resize message: 40 rows, 120 cols.
	resizeData := make([]byte, 4)
	binary.BigEndian.PutUint16(resizeData[0:2], 40)
	binary.BigEndian.PutUint16(resizeData[2:4], 120)
	require.NoError(t, sendRelayMsg(clientConn, relayMsgResize, resizeData), "send resize")

	select {
	case size := <-machine.resizes:
		require.Equal(t, [2]uint16{40, 120}, size, "resize rows/cols")
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for resize event")
	}

	// Close client to let the session end.
	require.NoError(t, clientConn.Close(), "close client conn")

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for relay")
	}
}

func TestExecInteractiveViaRelayForwardsResize(t *testing.T) {
	machine := newFakeInteractiveMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	// Use a short path — macOS limits Unix socket paths to 104 bytes.
	dir, err := os.MkdirTemp("", "relay")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := dir + "/s.sock"
	require.NoError(t, relay.Start(socketPath), "start relay")
	defer relay.Stop()

	resizeCh := make(chan [2]uint16, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := ExecInteractiveViaRelay(ctx, socketPath, "noop", "", "", 24, 80,
			&blockedReader{unblock: ctx.Done()}, io.Discard, resizeCh)
		done <- err
	}()

	<-machine.execStarted

	// Send a resize event through the client channel.
	resizeCh <- [2]uint16{50, 200}

	select {
	case size := <-machine.resizes:
		require.Equal(t, [2]uint16{50, 200}, size, "resize rows/cols")
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for resize event")
	}

	// Cancel context to tear down the session.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for client")
	}
}

// blockedReader blocks on Read until the unblock channel is closed,
// then returns EOF. This prevents ExecInteractive from finishing
// before the test can send resize events.
type blockedReader struct {
	unblock <-chan struct{}
}

func (r *blockedReader) Read([]byte) (int, error) {
	<-r.unblock
	return 0, io.EOF
}
