package policy

import (
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
