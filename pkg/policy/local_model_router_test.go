package policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localModelRouterRoutes() []api.LocalModelRoute {
	return []api.LocalModelRoute{{
		SourceHost:  "openrouter.ai",
		BackendHost: "127.0.0.1",
		BackendPort: 11434,
		Models: map[string]api.ModelRoute{
			"meta-llama/llama-3.1-8b-instruct":  {Target: "llama3.1:8b"},
			"meta-llama/llama-3.1-70b-instruct": {Target: "llama3.1:70b"},
			"qwen/qwen-2.5-coder-32b-instruct":  {Target: "qwen2.5-coder:32b"},
		},
	}}
}

func TestLocalModelRouterPlugin_Name(t *testing.T) {
	p := NewLocalModelRouterPlugin(nil, nil)
	assert.Equal(t, "local_model_router", p.Name())
}

func TestLocalModelRouterPlugin_MatchingModel(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)

	assert.Equal(t, "127.0.0.1", decision.Directive.Host)
	assert.Equal(t, 11434, decision.Directive.Port)
	assert.False(t, decision.Directive.UseTLS)
}

func TestLocalModelRouterPlugin_NonMatchingModel(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"openai/gpt-4o","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
	assert.NotEmpty(t, decision.Reason)
}

func TestLocalModelRouterPlugin_WrongHost(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "other-api.com")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
	assert.NotEmpty(t, decision.Reason)
}

func TestLocalModelRouterPlugin_WrongPath(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct"}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/models"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_GETMethod(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	req := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_NilBody(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   nil,
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_MalformedJSON(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader("not json")),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_BodyRewritten(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}],"stream":true,"route":"fallback","transforms":["middle-out"],"provider":{"order":["Together"]}}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{
			"Authorization": []string{"Bearer sk-or-fake-key"},
			"Http-Referer":  []string{"https://myapp.com"},
			"X-Title":       []string{"MyApp"},
			"Content-Type":  []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)

	assert.Equal(t, "/v1/chat/completions", req.URL.Path)

	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("Http-Referer"))
	assert.Empty(t, req.Header.Get("X-Title"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))

	gotBody, _ := io.ReadAll(req.Body)
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(gotBody, &parsed))

	assert.Equal(t, "llama3.1:8b", parsed["model"])
	assert.NotNil(t, parsed["messages"])
	assert.Equal(t, true, parsed["stream"])
	assert.Nil(t, parsed["route"])
	assert.Nil(t, parsed["transforms"])
	assert.Nil(t, parsed["provider"])

	assert.Equal(t, int64(len(gotBody)), req.ContentLength)
}

func TestLocalModelRouterPlugin_HostWithPort(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai:443")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)
	assert.Equal(t, "127.0.0.1", decision.Directive.Host)
}

func TestLocalModelRouterPlugin_EmptyRoutes(t *testing.T) {
	p := NewLocalModelRouterPlugin(nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_PerModelBackendOverride(t *testing.T) {
	routes := []api.LocalModelRoute{{
		SourceHost:  "openrouter.ai",
		BackendHost: "127.0.0.1",
		BackendPort: 11434,
		Models: map[string]api.ModelRoute{
			"meta-llama/llama-3.1-8b-instruct": {Target: "llama3.1:8b"},
			"qwen/qwen-2.5-coder-32b-instruct": {
				Target:      "qwen2.5-coder:32b",
				BackendHost: "10.0.0.5",
				BackendPort: 8080,
			},
		},
	}}
	p := NewLocalModelRouterPlugin(routes, nil)

	body1 := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req1 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body1)),
	}
	d1, err := p.Route(req1, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d1.Directive)
	assert.Equal(t, "127.0.0.1", d1.Directive.Host)
	assert.Equal(t, 11434, d1.Directive.Port)

	body2 := `{"model":"qwen/qwen-2.5-coder-32b-instruct","messages":[]}`
	req2 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body2)),
	}
	d2, err := p.Route(req2, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d2.Directive)
	assert.Equal(t, "10.0.0.5", d2.Directive.Host)
	assert.Equal(t, 8080, d2.Directive.Port)
}

func TestLocalModelRouterPlugin_CustomPath(t *testing.T) {
	routes := []api.LocalModelRoute{{
		SourceHost: "api.openai.com",
		Path:       "/v1/chat/completions",
		Models: map[string]api.ModelRoute{
			"gpt-4o-mini": {Target: "llama3.1:8b"},
		},
	}}
	p := NewLocalModelRouterPlugin(routes, nil)

	body := `{"model":"gpt-4o-mini","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)
}

func TestLocalModelRouterPlugin_MultipleSourceHosts(t *testing.T) {
	routes := []api.LocalModelRoute{
		{
			SourceHost: "openrouter.ai",
			Models: map[string]api.ModelRoute{
				"meta-llama/llama-3.1-8b-instruct": {Target: "llama3.1:8b"},
			},
		},
		{
			SourceHost:  "api.openai.com",
			Path:        "/v1/chat/completions",
			BackendHost: "10.0.0.5",
			BackendPort: 8080,
			Models: map[string]api.ModelRoute{
				"gpt-4o-mini": {Target: "llama3.1:8b"},
			},
		},
	}
	p := NewLocalModelRouterPlugin(routes, nil)

	body1 := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req1 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body1)),
	}
	d1, err := p.Route(req1, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d1.Directive)
	assert.Equal(t, "127.0.0.1", d1.Directive.Host)
	assert.Equal(t, 11434, d1.Directive.Port)

	body2 := `{"model":"gpt-4o-mini","messages":[]}`
	req2 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body2)),
	}
	d2, err := p.Route(req2, "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, d2.Directive)
	assert.Equal(t, "10.0.0.5", d2.Directive.Host)
	assert.Equal(t, 8080, d2.Directive.Port)
}

func TestLocalModelRouterPlugin_HostHeaderRewritten(t *testing.T) {
	routes := []api.LocalModelRoute{{
		SourceHost:  "openrouter.ai",
		BackendHost: "192.168.1.50",
		BackendPort: 9090,
		Models: map[string]api.ModelRoute{
			"meta-llama/llama-3.1-8b-instruct": {Target: "llama3.1:8b"},
		},
	}}
	p := NewLocalModelRouterPlugin(routes, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)

	assert.Equal(t, "192.168.1.50:9090", req.Host)
}

func TestLocalModelRouterPlugin_TransformRequestNoop(t *testing.T) {
	p := NewLocalModelRouterPlugin(nil, nil)

	req := &http.Request{
		Header: http.Header{"X-Test": []string{"unchanged"}},
		URL:    &url.URL{},
	}

	decision, err := p.TransformRequest(req, "example.com")
	require.NoError(t, err)
	assert.Equal(t, req, decision.Request)
	assert.Equal(t, "no_op", decision.Action)
	assert.Equal(t, "unchanged", decision.Request.Header.Get("X-Test"))
}

func TestLocalModelRouterPlugin_FromConfig(t *testing.T) {
	raw := json.RawMessage(`{
		"routes": [{
			"source_host": "openrouter.ai",
			"backend_host": "127.0.0.1",
			"backend_port": 11434,
			"models": {
				"meta-llama/llama-3.1-8b-instruct": {"target": "llama3.1:8b"}
			}
		}]
	}`)

	plugin, err := NewLocalModelRouterPluginFromConfig(raw, nil)
	require.NoError(t, err)

	rp, ok := plugin.(RoutePlugin)
	require.True(t, ok)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := rp.Route(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)
	assert.Equal(t, "127.0.0.1", decision.Directive.Host)
}

func TestLocalModelRouterPlugin_FromConfig_Invalid(t *testing.T) {
	_, err := NewLocalModelRouterPluginFromConfig(json.RawMessage(`{invalid}`), nil)
	assert.Error(t, err)
}

func TestLocalModelRouterPlugin_BodyRestored_OnNoMatch(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	originalBody := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(originalBody)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)

	gotBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, originalBody, string(gotBody))
}

// --- New Decision Struct Tests ---

func TestLocalModelRouterPlugin_Decision_Redirected(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, decision.Directive)
	assert.Contains(t, decision.Reason, "matched model")
}

func TestLocalModelRouterPlugin_Decision_Passthrough_WrongHost(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "other-api.com")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
	assert.Contains(t, decision.Reason, "no route entry")
}

func TestLocalModelRouterPlugin_Decision_Passthrough_NoMatch(t *testing.T) {
	p := NewLocalModelRouterPlugin(localModelRouterRoutes(), nil)

	body := `{"model":"openai/gpt-4o","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.Route(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, decision.Directive)
	assert.Contains(t, decision.Reason, "no matching route")
}

func TestLocalModelRouterPlugin_Decision_TransformRequestNoOp(t *testing.T) {
	p := NewLocalModelRouterPlugin(nil, nil)

	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
	}

	decision, err := p.TransformRequest(req, "example.com")
	require.NoError(t, err)
	assert.Equal(t, "no_op", decision.Action)
	assert.Equal(t, req, decision.Request)
	assert.NotEmpty(t, decision.Reason)
}
