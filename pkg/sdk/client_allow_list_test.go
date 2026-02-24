package sdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllowListAdd(t *testing.T) {
	var capturedHosts []interface{}

	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "allow_list_add":
			if req.Params != nil {
				if params, ok := req.Params.(map[string]interface{}); ok {
					if hosts, ok := params["hosts"].([]interface{}); ok {
						capturedHosts = hosts
					}
				}
			}
			return response{
				JSONRPC: "2.0",
				Result:  []byte(`{"added":["api.anthropic.com"],"allowed_hosts":["api.openai.com","api.anthropic.com"]}`),
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

	update, err := client.AllowListAdd(context.Background(), "api.openai.com,api.anthropic.com", "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, []interface{}{"api.openai.com", "api.anthropic.com"}, capturedHosts)
	assert.Equal(t, []string{"api.anthropic.com"}, update.Added)
	assert.Equal(t, []string{"api.openai.com", "api.anthropic.com"}, update.AllowedHosts)
}

func TestAllowListDelete(t *testing.T) {
	client, cleanup := newScriptedClient(t, func(req request) response {
		switch req.Method {
		case "allow_list_delete":
			return response{
				JSONRPC: "2.0",
				Result:  []byte(`{"removed":["api.openai.com"],"allowed_hosts":["api.anthropic.com"]}`),
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

	update, err := client.AllowListDelete(context.Background(), "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, []string{"api.openai.com"}, update.Removed)
	assert.Equal(t, []string{"api.anthropic.com"}, update.AllowedHosts)
}

func TestAllowListAddRequiresHosts(t *testing.T) {
	client := &Client{}
	_, err := client.AllowListAdd(context.Background())
	require.ErrorIs(t, err, ErrAllowListHosts)
}
