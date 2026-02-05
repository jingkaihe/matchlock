package policy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
)

func TestEngine_IsHostAllowed_NoRestrictions(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{})

	if !engine.IsHostAllowed("example.com") {
		t.Error("All hosts should be allowed when no restrictions")
	}
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
			got := engine.IsHostAllowed(tt.host)
			if got != tt.allowed {
				t.Errorf("IsHostAllowed(%q) = %v, want %v", tt.host, got, tt.allowed)
			}
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
			got := engine.IsHostAllowed(tt.host)
			if got != tt.allowed {
				t.Errorf("IsHostAllowed(%q) = %v, want %v", tt.host, got, tt.allowed)
			}
		})
	}
}

func TestEngine_IsHostAllowed_WithPort(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.example.com"},
	})

	if !engine.IsHostAllowed("api.example.com:443") {
		t.Error("Should allow host with port")
	}
}

func TestEngine_GetPlaceholder(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {Value: "sk-secret-123"},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	if placeholder == "" {
		t.Error("Placeholder should not be empty")
	}
	if !strings.HasPrefix(placeholder, "SANDBOX_SECRET_") {
		t.Error("Placeholder should have correct prefix")
	}
}

func TestEngine_GetPlaceholders(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"KEY1": {Value: "val1"},
			"KEY2": {Value: "val2"},
		},
	})

	placeholders := engine.GetPlaceholders()
	if len(placeholders) != 2 {
		t.Errorf("Expected 2 placeholders, got %d", len(placeholders))
	}
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
	if err != nil {
		t.Fatalf("OnRequest failed: %v", err)
	}

	auth := result.Header.Get("Authorization")
	if auth != "Bearer real-secret" {
		t.Errorf("Expected secret replacement, got %q", auth)
	}
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
	if err != api.ErrSecretLeak {
		t.Error("Should detect secret leak to unauthorized host")
	}
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
	if err != nil {
		t.Fatalf("OnRequest failed: %v", err)
	}

	if result.Header.Get("X-Custom") != "normal-value" {
		t.Error("Non-secret values should be unchanged")
	}
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
	if err != nil {
		t.Fatalf("OnRequest failed: %v", err)
	}

	if !strings.Contains(result.URL.RawQuery, "real-secret") {
		t.Error("Secret should be replaced in URL")
	}
}

func TestEngine_OnResponse(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{})

	resp := &http.Response{
		StatusCode: 200,
	}

	result, err := engine.OnResponse(resp, nil, "example.com")
	if err != nil {
		t.Fatalf("OnResponse failed: %v", err)
	}

	if result != resp {
		t.Error("Response should be unchanged")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		match   bool
	}{
		{"*", "anything", true},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "example.com", false},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "other.example.com", false},
		{"prefix.*", "prefix.com", true},
		{"prefix.*", "other.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			got := matchGlob(tt.pattern, tt.str)
			if got != tt.match {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.match)
			}
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
			got := isPrivateIP(tt.host)
			if got != tt.private {
				t.Errorf("isPrivateIP(%q) = %v, want %v", tt.host, got, tt.private)
			}
		})
	}
}
