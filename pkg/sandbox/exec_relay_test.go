package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
}

var _ vm.InteractiveMachine = (*fakeInteractiveMachine)(nil)

func newFakeInteractiveMachine() *fakeInteractiveMachine {
	return &fakeInteractiveMachine{
		execStarted: make(chan struct{}),
		stdinData:   make(chan []byte, 1),
		stdinClosed: make(chan struct{}),
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
	data, _ := io.ReadAll(stdin)
	m.stdinData <- data
	close(m.stdinClosed)
	return 0, nil
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
