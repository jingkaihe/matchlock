package policy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
