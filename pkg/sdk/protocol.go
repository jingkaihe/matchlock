package sdk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

// JSON-RPC request/response types
type request struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	ID      uint64      `json:"id"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      *uint64         `json:"id,omitempty"`
}

// notification is a JSON-RPC notification (no ID) with a method and params
type notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error codes
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

// RPCError represents an error from the Matchlock RPC
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("matchlock RPC error [%d]: %s", e.Code, e.Message)
}

// IsVMError returns true if the error is a VM-related error
func (e *RPCError) IsVMError() bool {
	return e.Code == ErrCodeVMFailed
}

// IsExecError returns true if the error is an execution error
func (e *RPCError) IsExecError() bool {
	return e.Code == ErrCodeExecFailed
}

// IsFileError returns true if the error is a file operation error
func (e *RPCError) IsFileError() bool {
	return e.Code == ErrCodeFileFailed
}

// pendingRequest tracks an in-flight request awaiting its response.
type pendingRequest struct {
	ch chan pendingResult
	// onNotification is called for streaming notifications matching this request ID.
	// It is only set for exec_stream requests.
	onNotification func(method string, params json.RawMessage)
}

type pendingResult struct {
	result json.RawMessage
	err    error
}

// sendRequest sends a JSON-RPC request without a cancellation context.
// It is safe for concurrent use.
func (c *Client) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	return c.sendRequestCtx(context.Background(), method, params, nil)
}

// sendRequestCtx sends a JSON-RPC request with context support.
// If the context is cancelled while waiting, a "cancel" RPC is fired to
// abort the server-side operation and ctx.Err() is returned.
// If onNotification is non-nil, it is called for each streaming notification
// matching this request's ID before the final response arrives.
func (c *Client) sendRequestCtx(ctx context.Context, method string, params interface{}, onNotification func(string, json.RawMessage)) (json.RawMessage, error) {
	c.readerOnce.Do(c.startReader)

	id := c.requestID.Add(1)

	pending := &pendingRequest{
		ch:             make(chan pendingResult, 1),
		onNotification: onNotification,
	}

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pendingMu.Unlock()
		return nil, ErrClientClosed
	}
	c.pending[id] = pending
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	req := request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, errx.Wrap(ErrMarshalRequest, err)
	}

	c.writeMu.Lock()
	_, writeErr := fmt.Fprintln(c.stdin, string(data))
	c.writeMu.Unlock()
	if writeErr != nil {
		return nil, errx.Wrap(ErrWriteRequest, writeErr)
	}

	select {
	case result := <-pending.ch:
		return result.result, result.err
	case <-ctx.Done():
		c.sendCancelRequest(id)
		return nil, ctx.Err()
	}
}

// sendCancelRequest sends a fire-and-forget "cancel" RPC to abort an in-flight request.
func (c *Client) sendCancelRequest(targetID uint64) {
	cancelID := c.requestID.Add(1)
	req := request{
		JSONRPC: "2.0",
		Method:  "cancel",
		Params:  map[string]uint64{"id": targetID},
		ID:      cancelID,
	}
	data, _ := json.Marshal(req)

	c.writeMu.Lock()
	fmt.Fprintln(c.stdin, string(data))
	c.writeMu.Unlock()
}

// startReader launches the background goroutine that reads JSON-RPC responses
// from stdout and dispatches them to the appropriate pending request.
func (c *Client) startReader() {
	go func() {
		for {
			line, err := c.stdout.ReadBytes('\n')
			if err != nil {
				c.pendingMu.Lock()
				for _, p := range c.pending {
					p.ch <- pendingResult{err: errx.Wrap(ErrConnectionClose, err)}
				}
				c.pending = nil
				c.pendingMu.Unlock()
				return
			}

			var resp response
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}

			// Notification (no ID) — may be a stream chunk or an event
			if resp.ID == nil {
				var notif notification
				if err := json.Unmarshal(line, &notif); err != nil {
					continue
				}
				c.handleNotification(notif)
				continue
			}

			c.pendingMu.Lock()
			p, ok := c.pending[*resp.ID]
			c.pendingMu.Unlock()

			if !ok {
				continue
			}

			if resp.Error != nil {
				p.ch <- pendingResult{err: &RPCError{
					Code:    resp.Error.Code,
					Message: resp.Error.Message,
				}}
			} else {
				p.ch <- pendingResult{result: resp.Result}
			}
		}
	}()
}

// handleNotification routes JSON-RPC notifications. Stream notifications
// (exec_stream.stdout, exec_stream.stderr) include a request ID in params
// and are forwarded to the matching pending request's callback.
func (c *Client) handleNotification(notif notification) {
	switch notif.Method {
	case "exec_stream.stdout", "exec_stream.stderr":
		var p struct {
			ID *uint64 `json:"id"`
		}
		if err := json.Unmarshal(notif.Params, &p); err != nil || p.ID == nil {
			return
		}
		c.pendingMu.Lock()
		pending, ok := c.pending[*p.ID]
		c.pendingMu.Unlock()
		if ok && pending.onNotification != nil {
			pending.onNotification(notif.Method, notif.Params)
		}
	case "event":
		var event api.Event
		if err := json.Unmarshal(notif.Params, &event); err != nil {
			return
		}
		if event.File == nil {
			return
		}
		c.handleVFSFileEvent(event.File.Op, event.File.Path, event.File.Size, event.File.Mode, event.File.UID, event.File.GID)
	}
}
