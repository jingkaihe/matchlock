package rpc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

type mockVM struct {
	id       string
	execFunc func(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error)
}

func (m *mockVM) ID() string                                                { return m.id }
func (m *mockVM) Config() *api.Config                                       { return api.DefaultConfig() }
func (m *mockVM) Start(context.Context) error                               { return nil }
func (m *mockVM) Stop(context.Context) error                                { return nil }
func (m *mockVM) WriteFile(context.Context, string, []byte, uint32) error   { return nil }
func (m *mockVM) ReadFile(context.Context, string) ([]byte, error)          { return nil, nil }
func (m *mockVM) ListFiles(context.Context, string) ([]api.FileInfo, error) { return nil, nil }
func (m *mockVM) Events() <-chan api.Event                                  { return make(chan api.Event) }
func (m *mockVM) Close(context.Context) error                               { return nil }

func (m *mockVM) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, command, opts)
	}
	return &api.ExecResult{Stdout: []byte("hello\n")}, nil
}

type mockPortForwardVM struct {
	mockVM
	called bool
	ctx    context.Context
}

func (m *mockPortForwardVM) StartPortForwards(ctx context.Context, addresses []string, forwards []api.PortForward) (*sandbox.PortForwardManager, error) {
	m.called = true
	m.ctx = ctx
	return &sandbox.PortForwardManager{}, nil
}

type blockingPortForwardVM struct {
	mockVM
	started chan struct{}
	release chan struct{}
}

func (m *blockingPortForwardVM) StartPortForwards(ctx context.Context, addresses []string, forwards []api.PortForward) (*sandbox.PortForwardManager, error) {
	m.started <- struct{}{}
	<-m.release
	return &sandbox.PortForwardManager{}, nil
}

// rpcMsg is a generic JSON-RPC message that can be either a response or notification
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      *uint64         `json:"id,omitempty"`
}

type testRPC struct {
	stdinW *io.PipeWriter
	stdout *bufio.Reader
	done   chan error
}

func newTestRPC(vm *mockVM) *testRPC {
	return newTestRPCWithFactory(func(ctx context.Context, config *api.Config) (VM, error) {
		return vm, nil
	})
}

func newTestRPCWithFactory(factory VMFactory) *testRPC {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	h := NewHandler(factory, stdinR, stdoutW)

	done := make(chan error, 1)
	go func() { done <- h.Run(context.Background()) }()

	return &testRPC{
		stdinW: stdinW,
		stdout: bufio.NewReader(stdoutR),
		done:   done,
	}
}

func (t *testRPC) send(method string, id uint64, params interface{}) {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		p, _ := json.Marshal(params)
		req["params"] = json.RawMessage(p)
	}
	data, _ := json.Marshal(req)
	fmt.Fprintln(t.stdinW, string(data))
}

func (t *testRPC) read() *rpcMsg {
	line, _ := t.stdout.ReadBytes('\n')
	var msg rpcMsg
	json.Unmarshal(line, &msg)
	return &msg
}

func (t *testRPC) close() {
	t.stdinW.Close()
	<-t.done
}

func TestHandlerConcurrentExec(t *testing.T) {
	var mu sync.Mutex
	running := 0
	maxRunning := 0

	vm := &mockVM{
		id: "vm-test",
		execFunc: func(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
			mu.Lock()
			running++
			if running > maxRunning {
				maxRunning = running
			}
			mu.Unlock()

			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			running--
			mu.Unlock()

			return &api.ExecResult{Stdout: []byte(command)}, nil
		},
	}

	rpc := newTestRPC(vm)
	defer rpc.close()

	rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
	msg := rpc.read()
	require.Nil(t, msg.Error, "create failed")

	rpc.send("exec", 10, map[string]string{"command": "cmd-a"})
	rpc.send("exec", 11, map[string]string{"command": "cmd-b"})
	rpc.send("exec", 12, map[string]string{"command": "cmd-c"})

	results := make(map[uint64]string)
	for i := 0; i < 3; i++ {
		msg := rpc.read()
		require.Nil(t, msg.Error, "exec failed")
		var r struct {
			Stdout string `json:"stdout"`
		}
		json.Unmarshal(msg.Result, &r)
		decoded, _ := base64.StdEncoding.DecodeString(r.Stdout)
		results[*msg.ID] = string(decoded)
	}

	require.Equal(t, "cmd-a", results[10])
	require.Equal(t, "cmd-b", results[11])
	require.Equal(t, "cmd-c", results[12])

	mu.Lock()
	peak := maxRunning
	mu.Unlock()
	require.GreaterOrEqual(t, peak, 2, "expected concurrent execution, but peak running was %d", peak)
}

func TestHandlerExecStream(t *testing.T) {
	vm := &mockVM{
		id: "vm-test",
		execFunc: func(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
			if opts.Stdout != nil {
				opts.Stdout.Write([]byte("chunk1"))
				opts.Stdout.Write([]byte("chunk2"))
			}
			if opts.Stderr != nil {
				opts.Stderr.Write([]byte("err1"))
			}
			return &api.ExecResult{ExitCode: 0, DurationMS: 42}, nil
		},
	}

	rpc := newTestRPC(vm)
	defer rpc.close()

	rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
	rpc.read()

	rpc.send("exec_stream", 2, map[string]string{"command": "test"})

	var notifications []*rpcMsg
	var final *rpcMsg

	for {
		msg := rpc.read()
		if msg.ID != nil {
			final = msg
			break
		}
		notifications = append(notifications, msg)
	}

	require.Len(t, notifications, 3)

	stdoutCount := 0
	stderrCount := 0
	for _, n := range notifications {
		if strings.HasSuffix(n.Method, "stdout") {
			stdoutCount++
		} else if strings.HasSuffix(n.Method, "stderr") {
			stderrCount++
		}
	}
	assert.Equal(t, 2, stdoutCount, "stdout notification count")
	assert.Equal(t, 1, stderrCount, "stderr notification count")

	require.NotNil(t, final, "expected final response")
	require.Nil(t, final.Error, "expected successful final response")
	var result struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	json.Unmarshal(final.Result, &result)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, int64(42), result.DurationMS)
}

func TestHandlerCreateRejectsMountOutsideWorkspace(t *testing.T) {
	vm := &mockVM{id: "vm-test"}
	factoryCalls := 0

	rpc := newTestRPCWithFactory(func(ctx context.Context, config *api.Config) (VM, error) {
		factoryCalls++
		return vm, nil
	})
	defer rpc.close()

	rpc.send("create", 1, map[string]interface{}{
		"image": "alpine:latest",
		"vfs": map[string]interface{}{
			"workspace": "/workspace/project",
			"mounts": map[string]interface{}{
				"/workspace": map[string]interface{}{
					"type": "memory",
				},
			},
		},
	})

	msg := rpc.read()
	require.NotNil(t, msg.Error, "expected create to fail for mount outside workspace")
	require.Equal(t, ErrCodeInvalidParams, msg.Error.Code)
	require.Contains(t, msg.Error.Message, "must be within workspace")
	require.Equal(t, 0, factoryCalls, "factory should not have been called")
}

func TestHandlerCreateRejectsUserProvidedID(t *testing.T) {
	factoryCalls := 0
	rpc := newTestRPCWithFactory(func(ctx context.Context, config *api.Config) (VM, error) {
		factoryCalls++
		return &mockVM{id: "vm-test"}, nil
	})
	defer rpc.close()

	rpc.send("create", 1, map[string]interface{}{
		"image": "alpine:latest",
		"id":    "dev-server",
	})

	msg := rpc.read()
	require.NotNil(t, msg.Error, "expected create to fail for user-provided id")
	require.Equal(t, ErrCodeInvalidParams, msg.Error.Code)
	require.Contains(t, msg.Error.Message, "id is internal-only")
	require.Equal(t, 0, factoryCalls, "factory should not have been called")
}

func TestHandlerPortForwardUnsupported(t *testing.T) {
	vm := &mockVM{id: "vm-test"}
	rpc := newTestRPC(vm)
	defer rpc.close()

	rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
	rpc.read()

	rpc.send("port_forward", 2, map[string]interface{}{
		"forwards": []map[string]int{{"local_port": 8080, "remote_port": 8080}},
	})
	msg := rpc.read()
	require.NotNil(t, msg.Error)
	assert.Equal(t, ErrCodeVMFailed, msg.Error.Code)
}

func TestHandlerPortForwardSuccess(t *testing.T) {
	vm := &mockPortForwardVM{
		mockVM: mockVM{id: "vm-test"},
	}

	rpc := newTestRPCWithFactory(func(ctx context.Context, config *api.Config) (VM, error) {
		return vm, nil
	})
	defer rpc.close()

	rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
	rpc.read()

	rpc.send("port_forward", 2, map[string]interface{}{
		"forwards": []map[string]int{{"local_port": 8080, "remote_port": 8080}},
		"addresses": []string{
			"127.0.0.1",
		},
	})
	msg := rpc.read()
	require.Nil(t, msg.Error)
	require.NotNil(t, msg.Result)
	assert.True(t, vm.called)
	require.NotNil(t, vm.ctx)
	select {
	case <-vm.ctx.Done():
		t.Fatalf("port-forward context should remain active after request returns")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandlerPortForwardSerializesReplacement(t *testing.T) {
	vm := &blockingPortForwardVM{
		mockVM:  mockVM{id: "vm-test"},
		started: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}

	rpc := newTestRPCWithFactory(func(ctx context.Context, config *api.Config) (VM, error) {
		return vm, nil
	})
	defer rpc.close()

	rpc.send("create", 1, map[string]string{"image": "alpine:latest"})
	msg := rpc.read()
	require.Nil(t, msg.Error)

	params := map[string]interface{}{
		"forwards": []map[string]int{{"local_port": 8080, "remote_port": 8080}},
	}

	rpc.send("port_forward", 2, params)
	select {
	case <-vm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first port_forward to start")
	}

	rpc.send("port_forward", 3, params)

	secondStartedEarly := false
	select {
	case <-vm.started:
		secondStartedEarly = true
	case <-time.After(500 * time.Millisecond):
	}

	vm.release <- struct{}{}

	if !secondStartedEarly {
		select {
		case <-vm.started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for second port_forward to start")
		}
	}

	vm.release <- struct{}{}

	msgA := rpc.read()
	msgB := rpc.read()
	require.NotNil(t, msgA.ID)
	require.NotNil(t, msgB.ID)
	require.Nil(t, msgA.Error)
	require.Nil(t, msgB.Error)
	assert.ElementsMatch(t, []uint64{2, 3}, []uint64{*msgA.ID, *msgB.ID})
	assert.False(t, secondStartedEarly, "second port_forward started before first replacement completed")
}
