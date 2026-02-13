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
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/state"
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
	ErrCodeCancelled      = -32003
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
	Close(ctx context.Context) error
}

type VMFactory func(ctx context.Context, config *api.Config) (VM, error)

type Handler struct {
	factory   VMFactory
	vm        VM
	vmMu      sync.RWMutex // protects vm field
	events    chan api.Event
	stdin     io.Reader
	stdout    io.Writer
	mu        sync.Mutex // protects stdout writes
	closed    atomic.Bool
	wg        sync.WaitGroup // tracks in-flight requests
	cancelsMu sync.Mutex
	cancels   map[uint64]context.CancelFunc // per-request cancel funcs
}

func NewHandler(factory VMFactory, stdin io.Reader, stdout io.Writer) *Handler {
	return &Handler{
		factory: factory,
		events:  make(chan api.Event, 100),
		stdin:   stdin,
		stdout:  stdout,
		cancels: make(map[uint64]context.CancelFunc),
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

		// Handle cancel requests immediately (no goroutine, no wg)
		if req.Method == "cancel" {
			resp := h.handleCancel(&req)
			if resp != nil {
				h.sendResponse(resp)
			}
			continue
		}

		// Create and close run synchronously to avoid races
		if req.Method == "create" || req.Method == "close" {
			h.wg.Wait()
			resp := h.handleRequest(ctx, &req)
			if resp != nil {
				h.sendResponse(resp)
			}
			continue
		}

		h.wg.Add(1)
		go func(r Request) {
			defer h.wg.Done()

			reqCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			if r.ID != nil {
				h.cancelsMu.Lock()
				h.cancels[*r.ID] = cancel
				h.cancelsMu.Unlock()

				defer func() {
					h.cancelsMu.Lock()
					delete(h.cancels, *r.ID)
					h.cancelsMu.Unlock()
				}()
			}

			resp := h.handleRequest(reqCtx, &r)
			if resp != nil {
				h.sendResponse(resp)
			}
		}(req)
	}

	h.wg.Wait()
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

func (h *Handler) getVM() VM {
	h.vmMu.RLock()
	defer h.vmMu.RUnlock()
	return h.vm
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

	if params.Image == "" {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: "image is required (e.g., alpine:latest)"},
			ID:      req.ID,
		}
	}

	config := api.DefaultConfig().Merge(&params)
	if config.VFS != nil && len(config.VFS.Mounts) > 0 {
		if err := api.ValidateVFSMountsWithinWorkspace(config.VFS.Mounts, config.GetWorkspace()); err != nil {
			return &Response{
				JSONRPC: "2.0",
				Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
				ID:      req.ID,
			}
		}
	}

	vm, err := h.factory(ctx, config)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	if err := vm.Start(ctx); err != nil {
		vm.Close(ctx)
		state.NewManager().Remove(vm.ID())
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: err.Error()},
			ID:      req.ID,
		}
	}

	h.vmMu.Lock()
	h.vm = vm
	h.vmMu.Unlock()

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
	vm := h.getVM()
	if vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Command    string `json:"command"`
		WorkingDir string `json:"working_dir,omitempty"`
		User       string `json:"user,omitempty"`
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
		User:       params.User,
	}

	result, err := vm.Exec(ctx, params.Command, opts)
	if err != nil {
		code := ErrCodeExecFailed
		if ctx.Err() != nil {
			code = ErrCodeCancelled
		}
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: code, Message: err.Error()},
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

// handleExecStream executes a command and streams stdout/stderr as JSON-RPC
// notifications before sending the final response with the exit code.
//
// Notifications:
//
//	{"jsonrpc":"2.0","method":"exec_stream.stdout","params":{"id":<req_id>,"data":"<base64>"}}
//	{"jsonrpc":"2.0","method":"exec_stream.stderr","params":{"id":<req_id>,"data":"<base64>"}}
//
// Final response:
//
//	{"jsonrpc":"2.0","id":<req_id>,"result":{"exit_code":0,"duration_ms":123}}
func (h *Handler) handleExecStream(ctx context.Context, req *Request) *Response {
	vm := h.getVM()
	if vm == nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeVMFailed, Message: "VM not created"},
			ID:      req.ID,
		}
	}

	var params struct {
		Command    string `json:"command"`
		WorkingDir string `json:"working_dir,omitempty"`
		User       string `json:"user,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
			ID:      req.ID,
		}
	}

	reqID := req.ID
	stdoutWriter := &streamWriter{handler: h, reqID: reqID, method: "exec_stream.stdout"}
	stderrWriter := &streamWriter{handler: h, reqID: reqID, method: "exec_stream.stderr"}

	opts := &api.ExecOptions{
		WorkingDir: params.WorkingDir,
		User:       params.User,
		Stdout:     stdoutWriter,
		Stderr:     stderrWriter,
	}

	result, err := vm.Exec(ctx, params.Command, opts)
	if err != nil {
		code := ErrCodeExecFailed
		if ctx.Err() != nil {
			code = ErrCodeCancelled
		}
		return &Response{
			JSONRPC: "2.0",
			Error:   &Error{Code: code, Message: err.Error()},
			ID:      req.ID,
		}
	}

	// For streaming, stdout/stderr were already sent as notifications.
	// If the VM backend didn't use the writers (fell back to buffered), send them now.
	if !stdoutWriter.used && len(result.Stdout) > 0 {
		h.sendStreamData(reqID, "exec_stream.stdout", result.Stdout)
	}
	if !stderrWriter.used && len(result.Stderr) > 0 {
		h.sendStreamData(reqID, "exec_stream.stderr", result.Stderr)
	}

	return &Response{
		JSONRPC: "2.0",
		Result: map[string]interface{}{
			"exit_code":   result.ExitCode,
			"duration_ms": result.DurationMS,
		},
		ID: req.ID,
	}
}

// streamWriter implements io.Writer and sends each Write as a JSON-RPC notification.
type streamWriter struct {
	handler *Handler
	reqID   *uint64
	method  string
	used    bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.used = true
	w.handler.sendStreamData(w.reqID, w.method, p)
	return len(p), nil
}

func (h *Handler) sendStreamData(reqID *uint64, method string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params": map[string]interface{}{
			"id":   reqID,
			"data": base64.StdEncoding.EncodeToString(data),
		},
	}
	encoded, _ := json.Marshal(notification)
	fmt.Fprintln(h.stdout, string(encoded))
}

func (h *Handler) handleWriteFile(ctx context.Context, req *Request) *Response {
	vm := h.getVM()
	if vm == nil {
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

	if err := vm.WriteFile(ctx, params.Path, content, mode); err != nil {
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
	vm := h.getVM()
	if vm == nil {
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

	content, err := vm.ReadFile(ctx, params.Path)
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
	vm := h.getVM()
	if vm == nil {
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

	files, err := vm.ListFiles(ctx, params.Path)
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

	var params struct {
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds*float64(time.Second)))
	defer cancel()

	h.vmMu.Lock()
	vm := h.vm
	h.vm = nil
	h.vmMu.Unlock()

	if vm != nil {
		vm.Close(ctx)
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  map[string]interface{}{},
		ID:      req.ID,
	}
}

func (h *Handler) handleCancel(req *Request) *Response {
	var params struct {
		ID uint64 `json:"id"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &Response{
				JSONRPC: "2.0",
				Error:   &Error{Code: ErrCodeInvalidParams, Message: err.Error()},
				ID:      req.ID,
			}
		}
	}

	h.cancelsMu.Lock()
	cancel, ok := h.cancels[params.ID]
	h.cancelsMu.Unlock()

	if ok {
		cancel()
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  map[string]interface{}{"cancelled": ok},
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
