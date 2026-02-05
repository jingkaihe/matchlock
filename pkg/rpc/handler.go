package rpc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      *uint64         `json:"id,omitempty"`
}

type Response struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *Error      `json:"error,omitempty"`
	ID      *uint64     `json:"id,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	ErrCodeParse          = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
	ErrCodeVMFailed       = -32000
	ErrCodeExecFailed     = -32001
	ErrCodeFileFailed     = -32002
)

type VM interface {
	ID() string
	Config() *api.Config
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error)
	WriteFile(ctx context.Context, path string, content []byte, mode uint32) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	ListFiles(ctx context.Context, path string) ([]api.FileInfo, error)
	Events() <-chan api.Event
	Close() error
}

type VMFactory func(ctx context.Context, config *api.Config) (VM, error)

type Handler struct {
	factory VMFactory
	vm      VM
	events  chan api.Event
	stdin   io.Reader
	stdout  io.Writer
	mu      sync.Mutex
	closed  atomic.Bool
}

func NewHandler(factory VMFactory, stdin io.Reader, stdout io.Writer) *Handler {
	return &Handler{
		factory: factory,
		events:  make(chan api.Event, 100),
		stdin:   stdin,
		stdout:  stdout,
	}
}

func (h *Handler) Run(ctx context.Context) error {
	go h.eventLoop(ctx)

	scanner := bufio.NewScanner(h.stdin)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		if h.closed.Load() {
			break
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			h.sendError(nil, ErrCodeParse, "Parse error")
			continue
		}

		resp := h.handleRequest(ctx, &req)
		if resp != nil {
			h.sendResponse(resp)
		}
	}

	return scanner.Err()
}

func (h *Handler) handleRequest(ctx context.Context, req *Request) *Response {
	switch req.Method {
	case "create":
		return h.handleCreate(ctx, req)
	case "exec":
		return h.handleExec(ctx, req)
	case "exec_stream":
		return h.handleExecStream(ctx, req)
	case "write_file":
		return h.handleWriteFile(ctx, req)
	case "read_file":
		return h.handleReadFile(ctx, req)
	case "list_files":
		return h.handleListFiles(ctx, req)
	case "close":
		return h.handleClose(ctx, req)
	default:
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeMethodNotFound, Message: "Method not found"},
			ID:      req.ID,
		}
	}
}

func (h *Handler) handleCreate(ctx context.Context, req *Request) *Response {
	var params api.Config
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &Response{
				JSONRPC: "2.0",
				Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
				ID:      req.ID,
			}
		}
	}

	config := api.DefaultConfig().Merge(&params)

	vm, err := h.factory(ctx, config)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	if err := vm.Start(ctx); err != nil {
		vm.Close()
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	h.vm = vm

	go func() {
		for event := range vm.Events() {
			h.events <- event
		}
	}()

	result := map[string]interface{}{
		"id": vm.ID(),
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  result,
		ID:      req.ID,
	}
}

func (h *Handler) handleExec(ctx context.Context, req *Request) *Response {
	if h.vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Command    string `json:"command"`
		WorkingDir string `json:"working_dir,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
			ID:      req.ID,
		}
	}

	opts := &api.ExecOptions{
		WorkingDir: params.WorkingDir,
	}

	result, err := h.vm.Exec(ctx, params.Command, opts)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeExecFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	return &Response{
		JSONRPC: "2.0",
		Result: map[string]interface{}{
			"exit_code":   result.ExitCode,
			"stdout":      base64.StdEncoding.EncodeToString(result.Stdout),
			"stderr":      base64.StdEncoding.EncodeToString(result.Stderr),
			"duration_ms": result.DurationMS,
		},
		ID: req.ID,
	}
}

func (h *Handler) handleExecStream(ctx context.Context, req *Request) *Response {
	return h.handleExec(ctx, req)
}

func (h *Handler) handleWriteFile(ctx context.Context, req *Request) *Response {
	if h.vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Mode    uint32 `json:"mode,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
			ID:      req.ID,
		}
	}

	content, err := base64.StdEncoding.DecodeString(params.Content)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: "invalid base64 content"},
			ID:      req.ID,
		}
	}

	mode := params.Mode
	if mode == 0 {
		mode = 0644
	}

	if err := h.vm.WriteFile(ctx, params.Path, content, mode); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeFileFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  map[string]interface{}{},
		ID:      req.ID,
	}
}

func (h *Handler) handleReadFile(ctx context.Context, req *Request) *Response {
	if h.vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
			ID:      req.ID,
		}
	}

	content, err := h.vm.ReadFile(ctx, params.Path)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeFileFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	return &Response{
		JSONRPC: "2.0",
		Result: map[string]interface{}{
			"content": base64.StdEncoding.EncodeToString(content),
		},
		ID: req.ID,
	}
}

func (h *Handler) handleListFiles(ctx context.Context, req *Request) *Response {
	if h.vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
			ID:      req.ID,
		}
	}

	files, err := h.vm.ListFiles(ctx, params.Path)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeFileFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	return &Response{
		JSONRPC: "2.0",
		Result: map[string]interface{}{
			"files": files,
		},
		ID: req.ID,
	}
}

func (h *Handler) handleClose(ctx context.Context, req *Request) *Response {
	h.closed.Store(true)

	if h.vm != nil {
		h.vm.Stop(ctx)
		h.vm.Close()
		h.vm = nil
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  map[string]interface{}{},
		ID:      req.ID,
	}
}

func (h *Handler) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-h.events:
			if !ok {
				return
			}
			h.sendEvent(event)
		}
	}
}

func (h *Handler) sendResponse(resp *Response) {
	h.mu.Lock()
	defer h.mu.Unlock()

	data, _ := json.Marshal(resp)
	fmt.Fprintln(h.stdout, string(data))
}

func (h *Handler) sendError(id *uint64, code int, message string) {
	h.sendResponse(&Response{
		JSONRPC: "2.0",
		Error:   &Error{Code: code, Message: message},
		ID:      id,
	})
}

func (h *Handler) sendEvent(event api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "event",
		"params":  event,
	}
	data, _ := json.Marshal(notification)
	fmt.Fprintln(h.stdout, string(data))
}

func RunRPC(ctx context.Context, factory VMFactory) error {
	handler := NewHandler(factory, os.Stdin, os.Stdout)
	return handler.Run(ctx)
}
