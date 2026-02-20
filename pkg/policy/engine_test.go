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

func TestEngine_IsHostAllowed_NoRestrictions(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{})

	assert.True(t, engine.IsHostAllowed("example.com"), "All hosts should be allowed when no restrictions")
}

func TestEngine_IsHostAllowed_Allowlist(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com", "*.anthropic.com"},
	})

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
			assert.Equal(t, tt.allowed, engine.IsHostAllowed(tt.host))
		})
	}
}

func TestEngine_IsHostAllowed_BlockPrivateIPs(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	})

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
			assert.Equal(t, tt.allowed, engine.IsHostAllowed(tt.host))
		})
	}
}

func TestEngine_IsHostAllowed_AllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100"},
	})

	assert.True(t, engine.IsHostAllowed("192.168.1.100"), "Explicitly allowed private IP should be allowed")
	assert.False(t, engine.IsHostAllowed("192.168.1.101"), "Non-allowed private IP should be blocked")
	assert.False(t, engine.IsHostAllowed("10.0.0.1"), "Other private IP should be blocked")
	assert.True(t, engine.IsHostAllowed("8.8.8.8"), "Public IP should still be allowed")
}

func TestEngine_IsHostAllowed_AllowedPrivateHostsGlob(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.64.*"},
	})

	assert.True(t, engine.IsHostAllowed("192.168.64.1"), "IP matching glob should be allowed")
	assert.True(t, engine.IsHostAllowed("192.168.64.255"), "IP matching glob should be allowed")
	assert.False(t, engine.IsHostAllowed("192.168.65.1"), "IP not matching glob should be blocked")
	assert.False(t, engine.IsHostAllowed("10.0.0.1"), "Other private IP should be blocked")
}

func TestEngine_IsHostAllowed_EmptyAllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{},
	})

	assert.False(t, engine.IsHostAllowed("192.168.1.1"), "Private IP should be blocked with empty AllowedPrivateHosts")
	assert.False(t, engine.IsHostAllowed("10.0.0.1"), "Private IP should be blocked with empty AllowedPrivateHosts")
	assert.True(t, engine.IsHostAllowed("8.8.8.8"), "Public IP should still be allowed")
}

func TestEngine_IsHostAllowed_AllowedPrivateHostsNoBlock(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     false,
		AllowedPrivateHosts: []string{"192.168.1.100"},
	})

	assert.True(t, engine.IsHostAllowed("192.168.1.100"), "Private IP should be allowed when BlockPrivateIPs is false")
	assert.True(t, engine.IsHostAllowed("192.168.1.101"), "Private IP should be allowed when BlockPrivateIPs is false")
	assert.True(t, engine.IsHostAllowed("10.0.0.1"), "Private IP should be allowed when BlockPrivateIPs is false")
}

func TestEngine_IsHostAllowed_PrivateHostNeedsAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100"},
		AllowedHosts:        []string{"example.com", "192.168.1.100"},
	})

	assert.True(t, engine.IsHostAllowed("192.168.1.100"), "Private IP in both AllowedPrivateHosts and AllowedHosts should be allowed")
	assert.False(t, engine.IsHostAllowed("192.168.1.101"), "Private IP not in AllowedPrivateHosts should be blocked")
	assert.True(t, engine.IsHostAllowed("example.com"), "Public host in AllowedHosts should be allowed")
	assert.False(t, engine.IsHostAllowed("other.com"), "Host not in AllowedHosts should be blocked")
}

func TestEngine_IsHostAllowed_MultipleAllowedPrivateHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs:     true,
		AllowedPrivateHosts: []string{"192.168.1.100", "10.0.0.5", "172.16.0.*"},
	})

	assert.True(t, engine.IsHostAllowed("192.168.1.100"), "First allowed private IP should pass")
	assert.True(t, engine.IsHostAllowed("10.0.0.5"), "Second allowed private IP should pass")
	assert.True(t, engine.IsHostAllowed("172.16.0.1"), "IP matching glob pattern should pass")
	assert.True(t, engine.IsHostAllowed("172.16.0.254"), "IP matching glob pattern should pass")
	assert.False(t, engine.IsHostAllowed("192.168.1.101"), "Non-allowed private IP should be blocked")
	assert.False(t, engine.IsHostAllowed("10.0.0.6"), "Non-allowed private IP should be blocked")
}

func TestEngine_IsHostAllowed_WithPort(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.example.com"},
	})

	assert.True(t, engine.IsHostAllowed("api.example.com:443"), "Should allow host with port")
}

func TestEngine_GetPlaceholder(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {Value: "sk-secret-123"},
		},
	})

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
	})

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
	})

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
	})

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
	})

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
	})

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
	})

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
	engine := NewEngine(&api.NetworkConfig{})

	resp := &http.Response{
		StatusCode: 200,
	}

	result, err := engine.OnResponse(resp, nil, "example.com")
	require.NoError(t, err)

	assert.Equal(t, resp, result, "Response should be unchanged")
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		match   bool
	}{
		{"*", "anything", true},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", true}, // deep subdomains
		{"*.example.com", "example.com", false},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "other.example.com", false},
		{"prefix.*", "prefix.com", true},
		{"prefix.*", "other.com", false},
		// Middle wildcard patterns
		{"api-*.example.com", "api-v1.example.com", true},
		{"api-*.example.com", "api-prod.example.com", true},
		{"api-*.example.com", "other.example.com", false},
		{"*-prod.example.com", "api-prod.example.com", true},
		{"*-prod.example.com", "api-dev.example.com", false},
		// Multiple wildcards
		{"*.*.example.com", "a.b.example.com", true},
		{"api-*-*.example.com", "api-v1-prod.example.com", true},
		// Edge cases
		{"", "", true},
		{"test", "test", true},
		{"test", "other", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			assert.Equal(t, tt.match, matchGlob(tt.pattern, tt.str))
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		host    string
		private bool
	}{
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.private, isPrivateIP(tt.host))
		})
	}
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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(config)

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(routingConfig())

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
	engine := NewEngine(&api.NetworkConfig{})

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
	})

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
	})

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
	})

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
	})

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
	})

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
	})

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
