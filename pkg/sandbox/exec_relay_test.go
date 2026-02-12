package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
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
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	done := make(chan struct{})
	go func() {
		relay.handleExecPipe(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	if err := sendRelayMsg(clientConn, relayMsgStdin, nil); err != nil {
		t.Fatalf("send stdin EOF: %v", err)
	}

	select {
	case <-machine.ctxCanceled:
		t.Fatal("context canceled on stdin EOF")
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
		if msgType != relayMsgExit {
			exitErr <- fmt.Errorf("expected exit message, got %d", msgType)
			return
		}
		if len(data) != 4 {
			exitErr <- fmt.Errorf("expected 4-byte exit payload, got %d", len(data))
			return
		}
		exitErr <- nil
	}()

	select {
	case err := <-exitErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for exit")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for relay")
	}
}

func TestExecRelayPipeDisconnectCancels(t *testing.T) {
	machine := newFakeMachine()
	sb := &Sandbox{config: &api.Config{}, machine: machine}
	relay := NewExecRelay(sb)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	reqData, err := json.Marshal(relayExecRequest{Command: "noop"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	done := make(chan struct{})
	go func() {
		relay.handleExecPipe(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client conn: %v", err)
	}

	select {
	case <-machine.ctxCanceled:
	case <-time.After(1 * time.Second):
		t.Fatal("expected context cancellation on disconnect")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for relay")
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
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	done := make(chan struct{})
	go func() {
		relay.handleExecInteractive(serverConn, reqData)
		close(done)
	}()

	<-machine.execStarted

	if err := sendRelayMsg(clientConn, relayMsgStdin, []byte("hello")); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client conn: %v", err)
	}

	select {
	case data := <-machine.stdinData:
		if string(data) != "hello" {
			t.Fatalf("unexpected stdin data: %q", string(data))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for stdin data")
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for relay")
	}
}
