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

	"github.com/jingkaihe/matchlock/pkg/api"
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
	if msg.Error != nil {
		t.Fatalf("create failed: %s", msg.Error.Message)
	}

	rpc.send("exec", 10, map[string]string{"command": "cmd-a"})
	rpc.send("exec", 11, map[string]string{"command": "cmd-b"})
	rpc.send("exec", 12, map[string]string{"command": "cmd-c"})

	results := make(map[uint64]string)
	for i := 0; i < 3; i++ {
		msg := rpc.read()
		if msg.Error != nil {
			t.Fatalf("exec failed: %s", msg.Error.Message)
		}
		var r struct {
			Stdout string `json:"stdout"`
		}
		json.Unmarshal(msg.Result, &r)
		decoded, _ := base64.StdEncoding.DecodeString(r.Stdout)
		results[*msg.ID] = string(decoded)
	}

	if results[10] != "cmd-a" || results[11] != "cmd-b" || results[12] != "cmd-c" {
		t.Fatalf("unexpected results: %v", results)
	}

	mu.Lock()
	peak := maxRunning
	mu.Unlock()
	if peak < 2 {
		t.Fatalf("expected concurrent execution, but peak running was %d", peak)
	}
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

	if len(notifications) != 3 {
		t.Fatalf("expected 3 notifications, got %d", len(notifications))
	}

	stdoutCount := 0
	stderrCount := 0
	for _, n := range notifications {
		if strings.HasSuffix(n.Method, "stdout") {
			stdoutCount++
		} else if strings.HasSuffix(n.Method, "stderr") {
			stderrCount++
		}
	}
	if stdoutCount != 2 || stderrCount != 1 {
		t.Fatalf("expected 2 stdout + 1 stderr, got %d + %d", stdoutCount, stderrCount)
	}

	if final == nil || final.Error != nil {
		t.Fatal("expected successful final response")
	}
	var result struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	json.Unmarshal(final.Result, &result)
	if result.ExitCode != 0 || result.DurationMS != 42 {
		t.Fatalf("unexpected result: %+v", result)
	}
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
	if msg.Error == nil {
		t.Fatal("expected create to fail for mount outside workspace")
	}
	if msg.Error.Code != ErrCodeInvalidParams {
		t.Fatalf("error code = %d, want %d", msg.Error.Code, ErrCodeInvalidParams)
	}
	if !strings.Contains(msg.Error.Message, "must be within workspace") {
		t.Fatalf("error = %q, want to contain %q", msg.Error.Message, "must be within workspace")
	}
	if factoryCalls != 0 {
		t.Fatalf("factory called %d times, want 0", factoryCalls)
	}
}
