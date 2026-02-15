package sdk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
)

func newScriptedClient(t *testing.T, handle func(request) response) (*Client, func()) {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			var req request
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			resp := handle(req)
			data, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintln(stdoutW, string(data))
		}
		_ = stdoutW.Close()
	}()

	c := &Client{
		stdin:   stdinW,
		stdout:  bufio.NewReader(stdoutR),
		pending: make(map[uint64]*pendingRequest),
	}

	cleanup := func() {
		_ = stdinW.Close()
		_ = stdoutW.Close()
		<-done
	}
	return c, cleanup
}

func TestCreateReturnsVMIDWhenPostCreatePortForwardFails(t *testing.T) {
	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-created"}`),
				ID:      &req.ID,
			}
		case "port_forward":
			return response{
				JSONRPC: "2.0",
				Error: &rpcError{
					Code:    ErrCodeVMFailed,
					Message: "bind: address already in use",
				},
				ID: &req.ID,
			}
		default:
			return response{
				JSONRPC: "2.0",
				Error: &rpcError{
					Code:    ErrCodeMethodNotFound,
					Message: "Method not found",
				},
				ID: &req.ID,
			}
		}
	})
	defer cleanup()

	vmID, err := client.Create(CreateOptions{
		Image: "alpine:latest",
		PortForwards: []api.PortForward{
			{LocalPort: 18080, RemotePort: 8080},
		},
	})

	require.Error(t, err)
	assert.Equal(t, "vm-created", vmID)
	assert.Equal(t, "vm-created", client.VMID())

	var rpcErr *RPCError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, ErrCodeVMFailed, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "address already in use")
}
