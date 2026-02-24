package policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockGatePlugin for AND-semantics tests ---

type mockGatePlugin struct {
	name    string
	verdict *GateVerdict
}

func (m *mockGatePlugin) Name() string           { return m.name }
func (m *mockGatePlugin) Gate(host string) *GateVerdict { return m.verdict }

func TestEngine_IsHostAllowed_NoRestrictions(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("example.com"), "All hosts should be allowed when no restrictions")
}

func TestEngine_IsHostAllowed_Allowlist(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com", "*.anthropic.com"},
	}, nil, nil)

	tests := []struct {
		host    string
		allowed bool
	}{
		{"api.openai.com", true},
		{"api.anthropic.com", true},
		{"console.anthropic.com", true},
		{"evil.com", false},
		{"openai.com.evil.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			verdict := engine.IsHostAllowed(tt.host)
			if tt.allowed {
				assert.Nil(t, verdict)
			} else {
				assert.NotNil(t, verdict)
			}
		})
	}
}

func TestEngine_IsHostAllowed_BlockPrivateIPs(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	}, nil, nil)

	tests := []struct {
		host    string
		allowed bool
	}{
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"8.8.8.8", true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			verdict := engine.IsHostAllowed(tt.host)
			if tt.allowed {
				assert.Nil(t, verdict)
			} else {
				assert.NotNil(t, verdict)
			}
		})
	}
}

func TestEngine_IsHostAllowed_AllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("192.168.1.100"), "Explicitly allowed private IP should be allowed")
	assert.NotNil(t, engine.IsHostAllowed("192.168.1.101"), "Non-allowed private IP should be blocked")
	assert.NotNil(t, engine.IsHostAllowed("10.0.0.1"), "Other private IP should be blocked")
	assert.Nil(t, engine.IsHostAllowed("8.8.8.8"), "Public IP should still be allowed")
}

func TestEngine_IsHostAllowed_AllowedPrivateHostsGlob(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.64.*"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("192.168.64.1"), "IP matching glob should be allowed")
	assert.Nil(t, engine.IsHostAllowed("192.168.64.255"), "IP matching glob should be allowed")
	assert.NotNil(t, engine.IsHostAllowed("192.168.65.1"), "IP not matching glob should be blocked")
	assert.NotNil(t, engine.IsHostAllowed("10.0.0.1"), "Other private IP should be blocked")
}

func TestEngine_IsHostAllowed_EmptyAllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{},
	}, nil, nil)

	assert.NotNil(t, engine.IsHostAllowed("192.168.1.1"), "Private IP should be blocked with empty AllowedPrivateHosts")
	assert.NotNil(t, engine.IsHostAllowed("10.0.0.1"), "Private IP should be blocked with empty AllowedPrivateHosts")
	assert.Nil(t, engine.IsHostAllowed("8.8.8.8"), "Public IP should still be allowed")
}

func TestEngine_IsHostAllowed_AllowedPrivateHostsNoBlock(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     false,
		AllowedPrivateHosts: []string{"192.168.1.100"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("192.168.1.100"), "Private IP should be allowed when BlockPrivateIPs is false")
	assert.Nil(t, engine.IsHostAllowed("192.168.1.101"), "Private IP should be allowed when BlockPrivateIPs is false")
	assert.Nil(t, engine.IsHostAllowed("10.0.0.1"), "Private IP should be allowed when BlockPrivateIPs is false")
}

func TestEngine_IsHostAllowed_PrivateHostNeedsAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100"},
		AllowedHosts:        []string{"example.com", "192.168.1.100"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("192.168.1.100"), "Private IP in both AllowedPrivateHosts and AllowedHosts should be allowed")
	assert.NotNil(t, engine.IsHostAllowed("192.168.1.101"), "Private IP not in AllowedPrivateHosts should be blocked")
	assert.Nil(t, engine.IsHostAllowed("example.com"), "Public host in AllowedHosts should be allowed")
	assert.NotNil(t, engine.IsHostAllowed("other.com"), "Host not in AllowedHosts should be blocked")
}

func TestEngine_IsHostAllowed_MultipleAllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100", "10.0.0.5", "172.16.0.*"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("192.168.1.100"), "First allowed private IP should pass")
	assert.Nil(t, engine.IsHostAllowed("10.0.0.5"), "Second allowed private IP should pass")
	assert.Nil(t, engine.IsHostAllowed("172.16.0.1"), "IP matching glob pattern should pass")
	assert.Nil(t, engine.IsHostAllowed("172.16.0.254"), "IP matching glob pattern should pass")
	assert.NotNil(t, engine.IsHostAllowed("192.168.1.101"), "Non-allowed private IP should be blocked")
	assert.NotNil(t, engine.IsHostAllowed("10.0.0.6"), "Non-allowed private IP should be blocked")
}

func TestEngine_IsHostAllowed_WithPort(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.example.com"},
	}, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("api.example.com:443"), "Should allow host with port")
}

func TestEngine_GetPlaceholder(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {Value: "sk-secret-123"},
		},
	}, nil, nil)

	placeholder := engine.GetPlaceholder("API_KEY")
	assert.NotEmpty(t, placeholder)
	assert.True(t, strings.HasPrefix(placeholder, "SANDBOX_SECRET_"), "Placeholder should have correct prefix")
}

func TestEngine_GetPlaceholders(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"KEY1": {Value: "val1"},
			"KEY2": {Value: "val2"},
		},
	}, nil, nil)

	placeholders := engine.GetPlaceholders()
	assert.Len(t, placeholders, 2)
}

func TestEngine_OnRequest_SecretReplacement(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}, nil, nil)

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestEngine_OnRequest_SecretLeak(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}, nil, nil)

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect secret leak to unauthorized host")
}

func TestEngine_OnRequest_NoSecretForHost(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}, nil, nil)

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "other.com")
	require.NoError(t, err)

	assert.Equal(t, "normal-value", result.Header.Get("X-Custom"), "Non-secret values should be unchanged")
}

func TestEngine_OnRequest_SecretInURL(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}, nil, nil)

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{},
		URL: &url.URL{
			RawQuery: "key=" + placeholder,
		},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	assert.Contains(t, result.URL.RawQuery, "real-secret", "Secret should be replaced in URL")
}

func TestEngine_OnRequest_NoBodyReplacement(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}, nil, nil)

	placeholder := engine.GetPlaceholder("API_KEY")
	body := `{"key":"` + placeholder + `"}`

	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	got, _ := io.ReadAll(result.Body)
	assert.NotContains(t, string(got), "real-secret", "Secret should NOT be replaced in request body")
	assert.Contains(t, string(got), placeholder, "Placeholder should remain in request body")
}

func TestEngine_OnResponse(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)

	resp := &http.Response{
		StatusCode: 200,
	}

	result, err := engine.OnResponse(resp, nil, "example.com")
	require.NoError(t, err)

	assert.Equal(t, resp, result, "Response should be unchanged")
}

// --- Routing tests ---

func routingConfig() *api.NetworkConfig {
	return &api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{{
			SourceHost:  "openrouter.ai",
			BackendHost: "127.0.0.1",
			BackendPort: 11434,
			Models: map[string]api.ModelRoute{
				"meta-llama/llama-3.1-8b-instruct":  {Target: "llama3.1:8b"},
				"meta-llama/llama-3.1-70b-instruct": {Target: "llama3.1:70b"},
				"qwen/qwen-2.5-coder-32b-instruct":  {Target: "qwen2.5-coder:32b"},
			},
		}},
	}
}

func TestEngine_RouteRequest_MatchingModel(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, directive)

	assert.Equal(t, "127.0.0.1", directive.Host)
	assert.Equal(t, 11434, directive.Port)
	assert.False(t, directive.UseTLS)
}

func TestEngine_RouteRequest_NonMatchingModel(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	body := `{"model":"openai/gpt-4o","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_WrongHost(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "other-api.com")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_WrongPath(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct"}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/models"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_GETMethod(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	req := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_BodyRewritten(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

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

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, directive)

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

func TestEngine_RouteRequest_SecretScopingIntegration(t *testing.T) {
	config := routingConfig()
	config.Secrets = map[string]api.Secret{
		"OPENROUTER_KEY": {
			Value: "sk-or-real-secret",
			Hosts: []string{"openrouter.ai"},
		},
	}
	engine := NewEngine(config, nil, nil)

	placeholder := engine.GetPlaceholder("OPENROUTER_KEY")
	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, directive)

	req2 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
	}
	_, err = engine.OnRequest(req2, directive.Host)
	require.ErrorIs(t, err, api.ErrSecretLeak)
}

func TestEngine_RouteRequest_BodyRestored_OnNoMatch(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	originalBody := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(originalBody)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)

	gotBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, originalBody, string(gotBody))
}

func TestEngine_RouteRequest_NilBody(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   nil,
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_MalformedJSON(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader("not json")),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)
}

func TestEngine_RouteRequest_HostWithPort(t *testing.T) {
	engine := NewEngine(routingConfig(), nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai:443")
	require.NoError(t, err)
	require.NotNil(t, directive)
	assert.Equal(t, "127.0.0.1", directive.Host)
}

// --- New configurable routing tests ---

func TestEngine_RouteRequest_NilRouting(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive, "Nil routing config should pass through all requests")
}

func TestEngine_RouteRequest_EmptyRouting(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{},
	}, nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive, "Empty routing config should pass through all requests")
}

func TestEngine_RouteRequest_MultipleSourceHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{
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
		},
	}, nil, nil)

	body1 := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req1 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body1)),
	}
	d1, err := engine.RouteRequest(req1, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d1)
	assert.Equal(t, "127.0.0.1", d1.Host)
	assert.Equal(t, 11434, d1.Port)

	body2 := `{"model":"gpt-4o-mini","messages":[]}`
	req2 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body2)),
	}
	d2, err := engine.RouteRequest(req2, "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, d2)
	assert.Equal(t, "10.0.0.5", d2.Host)
	assert.Equal(t, 8080, d2.Port)
}

func TestEngine_RouteRequest_PerModelBackendOverride(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{{
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
		}},
	}, nil, nil)

	body1 := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req1 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body1)),
	}
	d1, err := engine.RouteRequest(req1, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d1)
	assert.Equal(t, "127.0.0.1", d1.Host)
	assert.Equal(t, 11434, d1.Port)

	body2 := `{"model":"qwen/qwen-2.5-coder-32b-instruct","messages":[]}`
	req2 := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body2)),
	}
	d2, err := engine.RouteRequest(req2, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, d2)
	assert.Equal(t, "10.0.0.5", d2.Host)
	assert.Equal(t, 8080, d2.Port)
}

func TestEngine_RouteRequest_CustomPath(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{{
			SourceHost: "api.openai.com",
			Path:       "/v1/chat/completions",
			Models: map[string]api.ModelRoute{
				"gpt-4o-mini": {Target: "llama3.1:8b"},
			},
		}},
	}, nil, nil)

	body := `{"model":"gpt-4o-mini","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "api.openai.com")
	require.NoError(t, err)
	require.NotNil(t, directive)
}

func TestEngine_RouteRequest_CustomPath_NoMatchDefaultPath(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{{
			SourceHost: "api.openai.com",
			Path:       "/v1/chat/completions",
			Models: map[string]api.ModelRoute{
				"gpt-4o-mini": {Target: "llama3.1:8b"},
			},
		}},
	}, nil, nil)

	body := `{"model":"gpt-4o-mini","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "api.openai.com")
	require.NoError(t, err)
	assert.Nil(t, directive, "Default path should not match when custom path is configured")
}

func TestEngine_RouteRequest_HostHeaderRewritten(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		LocalModelRouting: []api.LocalModelRoute{{
			SourceHost:  "openrouter.ai",
			BackendHost: "192.168.1.50",
			BackendPort: 9090,
			Models: map[string]api.ModelRoute{
				"meta-llama/llama-3.1-8b-instruct": {Target: "llama3.1:8b"},
			},
		}},
	}, nil, nil)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, directive)

	assert.Equal(t, "192.168.1.50:9090", req.Host, "Host header should be rewritten to effective backend")
}

// --- Plugin orchestration tests ---

func TestEngine_PluginDisabled(t *testing.T) {
	enabled := false
	config := &api.NetworkConfig{
		Plugins: []api.PluginConfig{
			{
				Type:    "host_filter",
				Enabled: &enabled,
				Config:  json.RawMessage(`{"allowed_hosts":["example.com"]}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	// No gate plugins registered (the one plugin is disabled)
	assert.Nil(t, engine.IsHostAllowed("anything.com"),
		"All hosts should be allowed when only plugin is disabled")
}

func TestEngine_ExplicitPluginConfig(t *testing.T) {
	config := &api.NetworkConfig{
		Plugins: []api.PluginConfig{
			{
				Type:   "host_filter",
				Config: json.RawMessage(`{"allowed_hosts":["only-this.com"]}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("only-this.com"))
	assert.NotNil(t, engine.IsHostAllowed("other.com"))
}

// TestEngine_MixedFlatAndPlugin tests AND semantics with two host_filter gates.
// Under AND semantics, a host must be allowed by ALL gates to pass.
// Two host_filters with different allowlists means only hosts in BOTH lists pass.
func TestEngine_MixedFlatAndPlugin(t *testing.T) {
	config := &api.NetworkConfig{
		AllowedHosts: []string{"shared.com", "flat-only.com"},
		Plugins: []api.PluginConfig{
			{
				Type:   "host_filter",
				Config: json.RawMessage(`{"allowed_hosts":["shared.com","plugin-only.com"]}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("shared.com"),
		"Host in both gates should be allowed (AND satisfied)")
	assert.NotNil(t, engine.IsHostAllowed("flat-only.com"),
		"Host only in flat gate should be blocked (plugin gate denies)")
	assert.NotNil(t, engine.IsHostAllowed("plugin-only.com"),
		"Host only in plugin gate should be blocked (flat gate denies)")
	assert.NotNil(t, engine.IsHostAllowed("neither.com"),
		"Host in neither should be blocked")
}

// TestEngine_MixedFlatAndPlugin_OverlappingAllows verifies that overlapping
// allowlists work correctly under AND semantics.
func TestEngine_MixedFlatAndPlugin_OverlappingAllows(t *testing.T) {
	config := &api.NetworkConfig{
		AllowedHosts: []string{"*.example.com"},
		Plugins: []api.PluginConfig{
			{
				Type:   "host_filter",
				Config: json.RawMessage(`{"allowed_hosts":["api.example.com","other.com"]}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	assert.Nil(t, engine.IsHostAllowed("api.example.com"),
		"Host matching both gates should be allowed")
	assert.NotNil(t, engine.IsHostAllowed("web.example.com"),
		"Host matching only flat gate glob should be blocked")
	assert.NotNil(t, engine.IsHostAllowed("other.com"),
		"Host matching only plugin gate should be blocked")
}

func TestEngine_UnknownPluginType(t *testing.T) {
	config := &api.NetworkConfig{
		Plugins: []api.PluginConfig{
			{
				Type:   "nonexistent_plugin",
				Config: json.RawMessage(`{}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	// Engine should create successfully; unknown plugin is skipped
	assert.Nil(t, engine.IsHostAllowed("anything.com"))
}

func TestEngine_PluginSecretInjection(t *testing.T) {
	config := &api.NetworkConfig{
		Plugins: []api.PluginConfig{
			{
				Type: "secret_injector",
				Config: json.RawMessage(`{
					"secrets": {
						"MY_KEY": {
							"value": "real-value",
							"hosts": ["api.example.com"]
						}
					}
				}`),
			},
		},
	}
	engine := NewEngine(config, nil, nil)

	placeholders := engine.GetPlaceholders()
	assert.Contains(t, placeholders, "MY_KEY")
}

func TestEngine_PluginConfigIsEnabled(t *testing.T) {
	// nil Enabled = true (default)
	p1 := api.PluginConfig{Type: "host_filter"}
	assert.True(t, p1.IsEnabled())

	// Explicit true
	enabled := true
	p2 := api.PluginConfig{Type: "host_filter", Enabled: &enabled}
	assert.True(t, p2.IsEnabled())

	// Explicit false
	disabled := false
	p3 := api.PluginConfig{Type: "host_filter", Enabled: &disabled}
	assert.False(t, p3.IsEnabled())
}

// --- AND-semantics tests with mock gates ---

func TestEngine_IsHostAllowed_ANDSemantics_OneDenies(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)
	engine.gates = []GatePlugin{
		&mockGatePlugin{name: "allow-all", verdict: &GateVerdict{Allowed: true}},
		&mockGatePlugin{name: "deny-all", verdict: &GateVerdict{Allowed: false, Reason: "denied by mock"}},
	}

	verdict := engine.IsHostAllowed("example.com")
	require.NotNil(t, verdict, "Should be blocked when one gate denies")
	assert.Equal(t, "denied by mock", verdict.Reason)
}

func TestEngine_IsHostAllowed_ANDSemantics_BothAllow(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)
	engine.gates = []GatePlugin{
		&mockGatePlugin{name: "gate-a", verdict: &GateVerdict{Allowed: true}},
		&mockGatePlugin{name: "gate-b", verdict: &GateVerdict{Allowed: true}},
	}

	assert.Nil(t, engine.IsHostAllowed("example.com"),
		"Should be allowed when all gates allow")
}

func TestEngine_IsHostAllowed_ANDSemantics_BothDeny(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)
	engine.gates = []GatePlugin{
		&mockGatePlugin{name: "gate-a", verdict: &GateVerdict{Allowed: false, Reason: "reason-a"}},
		&mockGatePlugin{name: "gate-b", verdict: &GateVerdict{Allowed: false, Reason: "reason-b"}},
	}

	verdict := engine.IsHostAllowed("example.com")
	require.NotNil(t, verdict, "Should be blocked when both gates deny")
	assert.Equal(t, "reason-a", verdict.Reason,
		"Should return first denier's verdict")
}

func TestEngine_IsHostAllowed_ReturnsVerdictFromBlockingGate(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{}, nil, nil)
	engine.gates = []GatePlugin{
		&mockGatePlugin{name: "host-filter", verdict: &GateVerdict{Allowed: true}},
		&mockGatePlugin{name: "budget-gate", verdict: &GateVerdict{
			Allowed:     false,
			Reason:      "budget exceeded: $5.00 >= $5.00",
			StatusCode:  429,
			ContentType: "application/json",
			Body:        `{"error":{"message":"budget exceeded","type":"budget_limit"}}`,
		}},
	}

	verdict := engine.IsHostAllowed("api.openai.com")
	require.NotNil(t, verdict)
	assert.Equal(t, 429, verdict.StatusCode)
	assert.Equal(t, "application/json", verdict.ContentType)
	assert.Contains(t, verdict.Body, "budget exceeded")
	assert.Equal(t, "budget exceeded: $5.00 >= $5.00", verdict.Reason)
}

// --- Budget gate integration test ---

func TestEngine_BudgetGate_Integration(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "usage.jsonl")

	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts:   []string{"openrouter.ai"},
		UsageLogPath:   logPath,
		BudgetLimitUSD: 0.01, // very low limit
	}, nil, nil)

	// Before any usage: should be allowed
	assert.Nil(t, engine.IsHostAllowed("openrouter.ai"))

	// Simulate a response that records cost
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body: io.NopCloser(strings.NewReader(`{
			"id": "gen-test",
			"model": "anthropic/claude-sonnet-4",
			"usage": {
				"prompt_tokens": 100,
				"completion_tokens": 50,
				"total_tokens": 150,
				"cost": 0.02
			}
		}`)),
	}
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
	}
	_, err := engine.OnResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)

	// After exceeding budget: should be blocked with 429
	verdict := engine.IsHostAllowed("openrouter.ai")
	require.NotNil(t, verdict)
	assert.False(t, verdict.Allowed)
	assert.Equal(t, 429, verdict.StatusCode)
	assert.Equal(t, "application/json", verdict.ContentType)
	assert.Contains(t, verdict.Body, "budget_exceeded")
}

// --- Engine Event Emission Tests ---

func TestEngine_RouteRequest_EmitsRouteDecisionEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	engine := NewEngine(routingConfig(), nil, emitter)

	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	require.NotNil(t, directive)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	assert.Equal(t, "route_decision", event.EventType)
	assert.Equal(t, "local_model_router", event.Plugin)

	var data logging.RouteDecisionData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "redirected", data.Action)
	assert.NotEmpty(t, data.RoutedTo)
}

func TestEngine_RouteRequest_EmitsPassthroughEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	engine := NewEngine(routingConfig(), nil, emitter)

	body := `{"model":"openai/gpt-4o","messages":[]}`
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	directive, err := engine.RouteRequest(req, "openrouter.ai")
	require.NoError(t, err)
	assert.Nil(t, directive)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	assert.Equal(t, "route_decision", event.EventType)

	var data logging.RouteDecisionData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "passthrough", data.Action)
}

func TestEngine_OnRequest_EmitsRequestTransformEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	config := &api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	}
	engine := NewEngine(config, nil, emitter)

	placeholder := engine.GetPlaceholder("API_KEY")
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))

	// Find the request_transform event (there may also be gate events from host_filter)
	var requestTransformEvents []*logging.Event
	for _, ev := range capture.events {
		if ev.EventType == "request_transform" {
			requestTransformEvents = append(requestTransformEvents, ev)
		}
	}
	require.Len(t, requestTransformEvents, 1)
	event := requestTransformEvents[0]
	assert.Equal(t, "secret_injector", event.Plugin)

	var data logging.RequestTransformData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "injected", data.Action)
}

func TestEngine_OnResponse_EmitsResponseTransformEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	logPath := filepath.Join(t.TempDir(), "usage.jsonl")
	config := &api.NetworkConfig{
		UsageLogPath: logPath,
	}
	engine := NewEngine(config, nil, emitter)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body: io.NopCloser(strings.NewReader(`{
			"id": "gen-test",
			"model": "anthropic/claude-sonnet-4",
			"usage": {
				"prompt_tokens": 100,
				"completion_tokens": 50,
				"total_tokens": 150,
				"cost": 0.002
			}
		}`)),
	}
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
	}

	_, err := engine.OnResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)

	var responseTransformEvents []*logging.Event
	for _, ev := range capture.events {
		if ev.EventType == "response_transform" {
			responseTransformEvents = append(responseTransformEvents, ev)
		}
	}
	require.Len(t, responseTransformEvents, 1)
	event := responseTransformEvents[0]
	assert.Equal(t, "usage_logger", event.Plugin)

	var data logging.ResponseTransformData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "logged_usage", data.Action)
}

func TestEngine_NilEmitter_NoEventsPanic(t *testing.T) {
	config := &api.NetworkConfig{
		AllowedHosts: []string{"openrouter.ai"},
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"openrouter.ai"},
			},
		},
		LocalModelRouting: []api.LocalModelRoute{{
			SourceHost:  "openrouter.ai",
			BackendHost: "127.0.0.1",
			BackendPort: 11434,
			Models: map[string]api.ModelRoute{
				"meta-llama/llama-3.1-8b-instruct": {Target: "llama3.1:8b"},
			},
		}},
	}
	engine := NewEngine(config, nil, nil) // nil emitter

	// Gate phase
	assert.Nil(t, engine.IsHostAllowed("openrouter.ai"))

	// Route phase
	body := `{"model":"meta-llama/llama-3.1-8b-instruct","messages":[]}`
	routeReq := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
	_, err := engine.RouteRequest(routeReq, "openrouter.ai")
	require.NoError(t, err)

	// Request phase
	placeholder := engine.GetPlaceholder("API_KEY")
	onReq := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}
	_, err = engine.OnRequest(onReq, "openrouter.ai")
	require.NoError(t, err)

	// Response phase
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}
	respReq := &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/api/v1/chat/completions"},
		Header: http.Header{},
	}
	_, err = engine.OnResponse(resp, respReq, "openrouter.ai")
	require.NoError(t, err)
}
