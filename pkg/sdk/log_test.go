package sdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLog(t *testing.T) {
	client, cleanup := newExecRPCClient(t, func(req inboundRPC, write func(interface{})) {
		switch req.Method {
		case "log":
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte("hello from log\n")),
				},
			})
		}
	})
	defer cleanup()

	logOutput, err := client.Log(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "hello from log\n", logOutput)
}

func TestLogStream(t *testing.T) {
	client, cleanup := newExecRPCClient(t, func(req inboundRPC, write func(interface{})) {
		switch req.Method {
		case "log_stream":
			require.NotNil(t, req.ID)
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "log_stream.data",
				"params": map[string]interface{}{
					"id":   req.ID,
					"data": base64.StdEncoding.EncodeToString([]byte("first\n")),
				},
			})
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "log_stream.data",
				"params": map[string]interface{}{
					"id":   req.ID,
					"data": base64.StdEncoding.EncodeToString([]byte("second\n")),
				},
			})
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]interface{}{},
			})
		}
	})
	defer cleanup()

	var out bytes.Buffer
	err := client.LogStream(context.Background(), &out)
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond\n", out.String())
}

func TestLogParseError(t *testing.T) {
	client, cleanup := newExecRPCClient(t, func(req inboundRPC, write func(interface{})) {
		switch req.Method {
		case "log":
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  json.RawMessage(`{"content":123}`),
			})
		}
	})
	defer cleanup()

	_, err := client.Log(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrParseLogResult)
}
