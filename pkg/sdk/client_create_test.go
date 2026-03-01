package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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

func TestLaunchSetsLaunchEntrypoint(t *testing.T) {
	var launchEntrypoint bool

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if v, ok := params["launch_entrypoint"].(bool); ok {
						launchEntrypoint = v
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-launch"}`),
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

	vmID, err := client.Launch(New("alpine:latest"))
	require.NoError(t, err)
	assert.Equal(t, "vm-launch", vmID)
	assert.True(t, launchEntrypoint)
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

func TestCreateSendsAddHosts(t *testing.T) {
	var capturedAddHosts []map[string]interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						if addHosts, ok := network["add_hosts"].([]interface{}); ok {
							capturedAddHosts = make([]map[string]interface{}, 0, len(addHosts))
							for _, item := range addHosts {
								if hostMap, ok := item.(map[string]interface{}); ok {
									capturedAddHosts = append(capturedAddHosts, hostMap)
								}
							}
						}
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-add-hosts"}`),
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
		AddHosts: []api.HostIPMapping{
			{Host: "api.internal", IP: "10.0.0.10"},
			{Host: "db.internal", IP: "10.0.0.11"},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-add-hosts", vmID)
	require.Len(t, capturedAddHosts, 2)
	assert.Equal(t, "api.internal", capturedAddHosts[0]["host"])
	assert.Equal(t, "10.0.0.10", capturedAddHosts[0]["ip"])
	assert.Equal(t, "db.internal", capturedAddHosts[1]["host"])
	assert.Equal(t, "10.0.0.11", capturedAddHosts[1]["ip"])
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

func TestCreateRejectsInvalidCPUCount(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image: "alpine:latest",
		CPUs:  -0.5,
	})
	require.ErrorIs(t, err, ErrInvalidCPUCount)
	assert.Empty(t, vmID)
}

func TestCreateSendsFractionalCPUs(t *testing.T) {
	var capturedCPUs float64

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if resources, ok := params["resources"].(map[string]interface{}); ok {
						if cpus, ok := resources["cpus"].(float64); ok {
							capturedCPUs = cpus
						}
					}
				}
			}
			return response{JSONRPC: "2.0", Result: json.RawMessage(`{"id":"vm-cpu"}`), ID: &req.ID}
		default:
			return response{JSONRPC: "2.0", Error: &rpcError{Code: ErrCodeMethodNotFound, Message: "Method not found"}, ID: &req.ID}
		}
	})
	defer cleanup()

	vmID, err := client.Create(CreateOptions{Image: "alpine:latest", CPUs: 0.5})
	require.NoError(t, err)
	assert.Equal(t, "vm-cpu", vmID)
	assert.Equal(t, 0.5, capturedCPUs)
}

func TestCreateSendsNoNetwork(t *testing.T) {
	var capturedNetwork map[string]interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						capturedNetwork = network
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-no-network"}`),
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
		Image:     "alpine:latest",
		NoNetwork: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-no-network", vmID)
	require.NotNil(t, capturedNetwork)
	assert.Equal(t, true, capturedNetwork["no_network"])
	_, hasBlockPrivate := capturedNetwork["block_private_ips"]
	assert.False(t, hasBlockPrivate)
}

func TestCreateSendsForceInterception(t *testing.T) {
	var capturedNetwork map[string]interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						capturedNetwork = network
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-intercept"}`),
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
		Image:             "alpine:latest",
		ForceInterception: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-intercept", vmID)
	require.NotNil(t, capturedNetwork)
	assert.Equal(t, true, capturedNetwork["intercept"])
	assert.Equal(t, true, capturedNetwork["block_private_ips"])
}

func TestCreateSendsNetworkInterceptionRules(t *testing.T) {
	var capturedNetwork map[string]interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						capturedNetwork = network
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-intercept-rules"}`),
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
		NetworkInterception: &NetworkInterceptionConfig{
			Rules: []NetworkHookRule{
				{
					Phase:                 NetworkHookPhaseAfter,
					Action:                NetworkHookActionMutate,
					Hosts:                 []string{"api.openai.com"},
					SetResponseHeaders:    map[string]string{"X-Test": "ok"},
					BodyReplacements:      []NetworkBodyTransform{{Find: "foo", Replace: "bar"}},
					DeleteResponseHeaders: []string{"Server"},
				},
			},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-intercept-rules", vmID)
	require.NotNil(t, capturedNetwork)
	assert.Equal(t, true, capturedNetwork["intercept"])
	interception, ok := capturedNetwork["interception"].(map[string]interface{})
	require.True(t, ok)
	rules, ok := interception["rules"].([]interface{})
	require.True(t, ok)
	require.Len(t, rules, 1)
}

func TestCreateSendsNetworkInterceptionCallbackRule(t *testing.T) {
	var capturedNetwork map[string]interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "create":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if network, ok := params["network"].(map[string]interface{}); ok {
						capturedNetwork = network
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"id":"vm-intercept-callback"}`),
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
		NetworkInterception: &NetworkInterceptionConfig{
			Rules: []NetworkHookRule{
				{
					Name:      "callback-after",
					Phase:     NetworkHookPhaseAfter,
					Hosts:     []string{"api.openai.com"},
					Path:      "/v1/*",
					Hook:      func(ctx context.Context, req NetworkHookRequest) (*NetworkHookResult, error) { return nil, nil },
					TimeoutMS: 1500,
				},
			},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "vm-intercept-callback", vmID)
	require.NotNil(t, capturedNetwork)
	assert.Equal(t, true, capturedNetwork["intercept"])

	interception, ok := capturedNetwork["interception"].(map[string]interface{})
	require.True(t, ok)
	callbackSocket, ok := interception["callback_socket"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, strings.TrimSpace(callbackSocket))

	rules, ok := interception["rules"].([]interface{})
	require.True(t, ok)
	require.Len(t, rules, 1)
	rule, ok := rules[0].(map[string]interface{})
	require.True(t, ok)
	callbackID, ok := rule["callback_id"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, strings.TrimSpace(callbackID))
}

func TestCreateRejectsNetworkCallbackWithActionBlock(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image: "alpine:latest",
		NetworkInterception: &NetworkInterceptionConfig{
			Rules: []NetworkHookRule{
				{
					Name:   "bad-callback",
					Phase:  NetworkHookPhaseAfter,
					Action: NetworkHookActionBlock,
					Hook:   func(ctx context.Context, req NetworkHookRequest) (*NetworkHookResult, error) { return nil, nil },
				},
			},
		},
	})
	require.ErrorIs(t, err, ErrInvalidNetworkHook)
	assert.Empty(t, vmID)
}

func TestCreateRejectsNoNetworkWithAllowedHosts(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image:        "alpine:latest",
		NoNetwork:    true,
		AllowedHosts: []string{"api.openai.com"},
	})
	require.ErrorIs(t, err, ErrNoNetworkConflict)
	assert.Empty(t, vmID)
}

func TestCreateRejectsNoNetworkWithForceInterception(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image:             "alpine:latest",
		NoNetwork:         true,
		ForceInterception: true,
	})
	require.ErrorIs(t, err, ErrNoNetworkConflict)
	assert.Empty(t, vmID)
}

func TestCreateRejectsNoNetworkWithNetworkInterceptionRules(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image:     "alpine:latest",
		NoNetwork: true,
		NetworkInterception: &NetworkInterceptionConfig{
			Rules: []NetworkHookRule{
				{
					Phase:  NetworkHookPhaseBefore,
					Action: NetworkHookActionBlock,
				},
			},
		},
	})
	require.ErrorIs(t, err, ErrNoNetworkConflict)
	assert.Empty(t, vmID)
}

func TestCreateRejectsInvalidAddHost(t *testing.T) {
	client := &Client{}
	vmID, err := client.Create(CreateOptions{
		Image: "alpine:latest",
		AddHosts: []api.HostIPMapping{
			{Host: "bad host", IP: "10.0.0.10"},
		},
	})
	require.ErrorIs(t, err, ErrInvalidAddHost)
	assert.Empty(t, vmID)
}
