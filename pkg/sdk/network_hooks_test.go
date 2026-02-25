package sdk

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServeNetworkHookConn_PreservesEmptyMutations(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
	})

	hooks := map[string]compiledNetworkHook{
		"cb_1": {
			id: "cb_1",
			callback: func(ctx context.Context, req NetworkHookRequest) (*NetworkHookResult, error) {
				return &NetworkHookResult{
					Action: NetworkHookActionMutate,
					Request: &NetworkHookRequestMutation{
						Headers: map[string][]string{},
						Query:   map[string]string{},
					},
					Response: &NetworkHookResponseMutation{
						Headers: map[string][]string{},
						SetBody: []byte{},
					},
				}, nil
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveNetworkHookConn(serverConn, hooks)
	}()

	enc := json.NewEncoder(clientConn)
	dec := json.NewDecoder(clientConn)
	err := enc.Encode(api.NetworkHookCallbackRequest{
		CallbackID: "cb_1",
		Phase:      "after",
	})
	require.NoError(t, err)

	var resp api.NetworkHookCallbackResponse
	err = dec.Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, "mutate", resp.Action)

	require.NotNil(t, resp.Request)
	assert.NotNil(t, resp.Request.Headers)
	assert.Empty(t, resp.Request.Headers)
	assert.NotNil(t, resp.Request.Query)
	assert.Empty(t, resp.Request.Query)

	require.NotNil(t, resp.Response)
	assert.NotNil(t, resp.Response.Headers)
	assert.Empty(t, resp.Response.Headers)
	require.NotNil(t, resp.Response.SetBodyBase64)
	assert.Equal(t, "", *resp.Response.SetBodyBase64)

	_ = clientConn.Close()
	<-done
}
