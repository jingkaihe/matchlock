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

func TestCreateSendsNetworkMTU(t *testing.T) {
	var capturedMTU float64
	var capturedBlockPrivateIPs bool
	var hasNetworkConfig bool

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						hasNetworkConfig = true
						if mtu, ok := network["mtu"].(float64); ok {
							capturedMTU = mtu
						}
						if blockPrivate, ok := network["block_private_ips"].(bool); ok {
							capturedBlockPrivateIPs = blockPrivate
						}
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-mtu"}`),
				ID:      &req.ID,
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
		Image:      "alpine:latest",
		NetworkMTU: 1200,
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-mtu", vmID)
	assert.True(t, hasNetworkConfig)
	assert.Equal(t, 1200.0, capturedMTU)
	assert.True(t, capturedBlockPrivateIPs)
}

func TestCreateNetworkDefaultsBlockPrivateIPsWhenAllowHostsSet(t *testing.T) {
	var capturedBlockPrivateIPs bool
	var hasNetworkConfig bool

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						hasNetworkConfig = true
						if blockPrivate, ok := network["block_private_ips"].(bool); ok {
							capturedBlockPrivateIPs = blockPrivate
						}
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-hosts"}`),
				ID:      &req.ID,
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
		Image:        "alpine:latest",
		AllowedHosts: []string{"api.openai.com"},
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-hosts", vmID)
	assert.True(t, hasNetworkConfig)
	assert.True(t, capturedBlockPrivateIPs)
}

func TestCreateRespectsExplicitDisableBlockPrivateIPs(t *testing.T) {
	var capturedBlockPrivateIPs bool
	var hasNetworkConfig bool

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						hasNetworkConfig = true
						if blockPrivate, ok := network["block_private_ips"].(bool); ok {
							capturedBlockPrivateIPs = blockPrivate
						}
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-private-off"}`),
				ID:      &req.ID,
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
		Image:              "alpine:latest",
		NetworkMTU:         1200,
		BlockPrivateIPs:    false,
		BlockPrivateIPsSet: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-private-off", vmID)
	assert.True(t, hasNetworkConfig)
	assert.False(t, capturedBlockPrivateIPs)
}

func TestCreateOmitsNetworkWhenNoNetworkOverrides(t *testing.T) {
	var hasNetworkConfig bool

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					_, hasNetworkConfig = params["network"].(map[string]interface{})
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-default-net"}`),
				ID:      &req.ID,
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
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-default-net", vmID)
	assert.False(t, hasNetworkConfig)
}

func TestCreateRejectsNegativeNetworkMTU(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image:      "alpine:latest",
		NetworkMTU: -1,
	})
	require.ErrorIs(t, err, ErrInvalidNetworkMTU)
	assert.Empty(t, vmID)
}
