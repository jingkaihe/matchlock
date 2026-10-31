package sdk

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyParams(t *testing.T) {
	var got []map[string]interface{}
	client, cleanup := newScriptedClient(t, func(req request) response {
		params, _ := req.Params.(map[string]interface{})
		params["method"] = req.Method
		got = append(got, params)
		return response{JSONRPC: "2.0", Result: json.RawMessage(`{}`), ID: &req.ID}
	})
	defer cleanup()
	ctx := context.Background()
	wd, err := os.Getwd()
	require.NoError(t, err)

	require.NoError(t, client.CopyIn(ctx, "src/.", "/workspace", nil))
	require.NoError(t, client.CopyIn(ctx, "/abs/file", "/workspace/", &CopyInOptions{Owner: "app", FollowLink: true, Exclude: []string{"*.log"}}))
	require.NoError(t, client.CopyOut(ctx, "/workspace/dist", "out/", nil))
	require.NoError(t, client.CopyOut(ctx, "/workspace/dist", "/abs/out", &CopyOutOptions{MaxSize: -1, MaxFiles: 10}))

	assert.Equal(t, []map[string]interface{}{
		{"method": "copy_in", "src": wd + "/src/.", "dest": "/workspace"},
		{"method": "copy_in", "src": "/abs/file", "dest": "/workspace/", "owner": "app", "follow_link": true, "exclude": []interface{}{"*.log"}},
		{"method": "copy_out", "src": "/workspace/dist", "dest": wd + "/out/"},
		{"method": "copy_out", "src": "/workspace/dist", "dest": "/abs/out", "max_size": float64(0), "max_files": float64(10)},
	}, got)

	assert.ErrorIs(t, client.CopyIn(ctx, "src", "/w", &CopyInOptions{Owner: "app", Archive: true}), ErrInvalidCopy)
	client.useSudo = true
	assert.ErrorIs(t, client.CopyOut(ctx, "/w", "out", nil), ErrCopyOutWithSudo)
	assert.Len(t, got, 4, "rejected copies must not reach the server")
}
