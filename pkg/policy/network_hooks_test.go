package policy

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubNetworkHookInvoker struct {
	invoke func(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error)
}

func (s *stubNetworkHookInvoker) Invoke(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
	if s == nil || s.invoke == nil {
		return nil, nil
	}
	return s.invoke(ctx, req)
}

func TestEngine_OnRequest_NetworkHookBlock(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "before",
					Action: "block",
					Hosts:  []string{"api.example.com"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	_, err := engine.OnRequest(req, "api.example.com")
	require.ErrorIs(t, err, api.ErrBlocked)
}

func TestEngine_OnRequest_NetworkHookMutate(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:         "before",
					Action:        "mutate",
					Hosts:         []string{"api.example.com"},
					Methods:       []string{"POST"},
					Path:          "/v1/*",
					SetHeaders:    map[string]string{"X-Test": "yes"},
					DeleteHeaders: []string{"X-Remove"},
					SetQuery:      map[string]string{"trace": "123"},
					DeleteQuery:   []string{"drop"},
					RewritePath:   "/v2/messages",
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "http://api.example.com/v1/messages?drop=1", strings.NewReader(""))
	req.Header.Set("X-Remove", "1")

	got, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "yes", got.Header.Get("X-Test"))
	assert.Equal(t, "", got.Header.Get("X-Remove"))
	assert.Equal(t, "/v2/messages", got.URL.Path)
	assert.Equal(t, "123", got.URL.Query().Get("trace"))
	assert.Equal(t, "", got.URL.Query().Get("drop"))
}

func TestEngine_OnResponse_NetworkHookMutateHeadersAndBody(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:                 "after",
					Action:                "mutate",
					Hosts:                 []string{"api.example.com"},
					SetResponseHeaders:    map[string]string{"X-Filtered": "1"},
					DeleteResponseHeaders: []string{"Server"},
					BodyReplacements: []api.NetworkBodyTransform{
						{Find: "foo", Replace: "bar"},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}, "Server": []string{"upstream"}},
		Body:          io.NopCloser(strings.NewReader(`{"msg":"foo"}`)),
		ContentLength: int64(len(`{"msg":"foo"}`)),
	}

	got, err := engine.OnResponse(resp, req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "1", got.Header.Get("X-Filtered"))
	assert.Equal(t, "", got.Header.Get("Server"))
	body, readErr := io.ReadAll(got.Body)
	require.NoError(t, readErr)
	assert.Equal(t, `{"msg":"bar"}`, string(body))
}

func TestEngine_OnResponse_NetworkHookBlock(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "after",
					Action: "block",
					Hosts:  []string{"api.example.com"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}

	_, err := engine.OnResponse(resp, req, "api.example.com")
	require.ErrorIs(t, err, api.ErrBlocked)
}

func TestEngine_OnResponse_SSEBodyReplacement(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "after",
					Action: "mutate",
					Hosts:  []string{"api.example.com"},
					BodyReplacements: []api.NetworkBodyTransform{
						{Find: "foo", Replace: "bar"},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/stream", nil)
	sse := "id:1\n" +
		"data: foo first\n" +
		"event: message\n" +
		"data: second foo\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	got, err := engine.OnResponse(resp, req, "api.example.com")
	require.NoError(t, err)
	body, readErr := io.ReadAll(got.Body)
	require.NoError(t, readErr)
	assert.Equal(t, "id:1\n"+"data: bar first\n"+"event: message\n"+"data: second bar\n\n", string(body))
}

func TestEngine_OnRequest_NetworkHookCallbackMutate(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:      "before",
					Hosts:      []string{"api.example.com"},
					Path:       "/v1/*",
					CallbackID: "cb_before",
				},
			},
		},
	})
	engine.SetNetworkHookInvoker(&stubNetworkHookInvoker{
		invoke: func(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
			require.Equal(t, "cb_before", req.CallbackID)
			require.Equal(t, "before", req.Phase)
			require.Equal(t, "api.example.com", req.Host)
			return &api.NetworkHookCallbackResponse{
				Action: "mutate",
				Request: &api.NetworkHookRequestMutation{
					Headers: map[string][]string{
						"X-From-Callback": []string{"1"},
					},
					Query: map[string]string{"trace": "cb"},
					Path:  "/v2/messages",
				},
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1/messages?drop=1", strings.NewReader(""))
	req.Header.Set("X-Remove", "1")
	got, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "1", got.Header.Get("X-From-Callback"))
	assert.Equal(t, "", got.Header.Get("X-Remove"))
	assert.Equal(t, "/v2/messages", got.URL.Path)
	assert.Equal(t, "cb", got.URL.Query().Get("trace"))
	assert.Equal(t, "", got.URL.Query().Get("drop"))
}

func TestEngine_OnResponse_NetworkHookCallbackSetBody(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:      "after",
					Hosts:      []string{"api.example.com"},
					CallbackID: "cb_after",
				},
			},
		},
	})
	engine.SetNetworkHookInvoker(&stubNetworkHookInvoker{
		invoke: func(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
			require.Equal(t, "cb_after", req.CallbackID)
			require.Equal(t, "after", req.Phase)
			require.Equal(t, http.StatusOK, req.StatusCode)
			return &api.NetworkHookCallbackResponse{
				Action: "mutate",
				Response: &api.NetworkHookResponseMutation{
					Headers: map[string][]string{
						"Content-Type":  []string{"application/json"},
						"X-Intercepted": []string{"true"},
					},
					SetBodyBase64: func() *string {
						encoded := base64.StdEncoding.EncodeToString([]byte(`{"msg":"callback-body"}`))
						return &encoded
					}(),
				},
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}, "Server": []string{"upstream"}},
		Body:          io.NopCloser(strings.NewReader(`{"msg":"upstream"}`)),
		ContentLength: int64(len(`{"msg":"upstream"}`)),
	}

	got, err := engine.OnResponse(resp, req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "true", got.Header.Get("X-Intercepted"))
	assert.Equal(t, "", got.Header.Get("Server"))
	body, readErr := io.ReadAll(got.Body)
	require.NoError(t, readErr)
	assert.Equal(t, `{"msg":"callback-body"}`, string(body))
}

func TestEngine_OnResponse_NetworkHookCallbackSetEmptyBody(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:      "after",
					Hosts:      []string{"api.example.com"},
					CallbackID: "cb_after",
				},
			},
		},
	})
	engine.SetNetworkHookInvoker(&stubNetworkHookInvoker{
		invoke: func(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
			require.Equal(t, "cb_after", req.CallbackID)
			empty := ""
			return &api.NetworkHookCallbackResponse{
				Action: "mutate",
				Response: &api.NetworkHookResponseMutation{
					SetBodyBase64: &empty,
				},
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(`{"msg":"upstream"}`)),
		ContentLength: int64(len(`{"msg":"upstream"}`)),
	}

	got, err := engine.OnResponse(resp, req, "api.example.com")
	require.NoError(t, err)
	body, readErr := io.ReadAll(got.Body)
	require.NoError(t, readErr)
	assert.Equal(t, "", string(body))
	assert.Equal(t, int64(0), got.ContentLength)
	assert.Equal(t, "0", got.Header.Get("Content-Length"))
}

func TestEngine_OnRequest_NetworkHookCallbackUsesRuleTimeout(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:      "before",
					Hosts:      []string{"api.example.com"},
					CallbackID: "cb_before",
					TimeoutMS:  2000,
				},
			},
		},
	})

	called := false
	engine.SetNetworkHookInvoker(&stubNetworkHookInvoker{
		invoke: func(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
			called = true
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			remaining := time.Until(deadline)
			assert.GreaterOrEqual(t, remaining, 1500*time.Millisecond)
			assert.LessOrEqual(t, remaining, 2*time.Second)
			return &api.NetworkHookCallbackResponse{Action: "allow"}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/v1", nil)
	_, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.True(t, called)
}
