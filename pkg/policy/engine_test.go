package policy

import (
	"encoding/base64"
	"io"
	"net"
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

func TestEngine_AddAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com"},
	})

	added := engine.AddAllowedHosts(" api.openai.com ", "api.anthropic.com", "", "api.anthropic.com")
	assert.Equal(t, []string{"api.anthropic.com"}, added)
	assert.Equal(t, []string{"api.openai.com", "api.anthropic.com"}, engine.AllowedHosts())
}

func TestEngine_RemoveAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com", "api.anthropic.com", "api.openai.com"},
	})

	removed := engine.RemoveAllowedHosts(" api.openai.com ", "missing.example.com")
	assert.Equal(t, []string{"api.openai.com"}, removed)
	assert.Equal(t, []string{"api.anthropic.com"}, engine.AllowedHosts())
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
		{"169.254.1.1", false},
		// CGNAT / Tailscale-style range.
		{"100.64.0.1", false},
		// IPv4-mapped loopback / link-local must be denied.
		{"::1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		// Yggdrasil overlay.
		{"200:1234::1", false},
		{"300::1", false},
		// IPv4-mapped IPv6 is checked as the embedded IPv4.
		{"::ffff:192.168.1.1", false},
		{"::ffff:127.0.0.1", false},
		{"[::ffff:192.168.1.1]:8080", false},
		{"8.8.8.8", true},
		{"100.128.0.1", true},
		{"::ffff:8.8.8.8", true},
		{"2001:4860:4860::8888", true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.allowed, engine.IsHostAllowed(tt.host))
		})
	}
}

func TestEngine_IsHostAllowed_BlockPrivateIPsWithPortsAndIPv6(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	})

	// A port suffix must not defeat private-range denial (IPv4).
	assert.False(t, engine.IsHostAllowed("127.0.0.1:22"))
	assert.False(t, engine.IsHostAllowed("10.0.0.1:8080"))
	// IPv6 link-local/ULA with a port must be denied too (bracketed form).
	assert.False(t, engine.IsHostAllowed("[fd00::1]:443"))
	assert.False(t, engine.IsHostAllowed("[::1]:80"))

	// Disabling block_private_ips removes the denial (allowlist still applies).
	openEngine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: false,
		AllowedHosts:    []string{"127.0.0.1"},
	})
	assert.True(t, openEngine.IsHostAllowed("127.0.0.1"))
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

func TestEngine_OnRequest_SecretReplacementInBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	auth := result.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(auth, "Basic "))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	require.NoError(t, err)
	assert.Equal(t, "user:real-secret", string(decoded))
}

func TestEngine_OnRequest_SecretReplacementInUnpaddedBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.RawStdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	auth := result.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(auth, "Basic "))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	require.NoError(t, err)
	assert.Equal(t, "user:real-secret", string(decoded))
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

func TestEngine_OnRequest_SecretLeakInBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect basic auth secret leak to unauthorized host")
}

func TestEngine_OnRequest_SecretLeakInUnpaddedBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.RawStdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect basic auth secret leak to unauthorized host")
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
		{"::1", true},
		{"fc00::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		// Yggdrasil overlay.
		{"200:1234::1", true},
		{"300::1", true},
		// CGNAT / Tailscale-style.
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.128.0.1", false},
		// IPv4-mapped IPv6 is judged by the embedded IPv4.
		{"::ffff:192.168.1.1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:8.8.8.8", false},
		{"2001:4860:4860::8888", false},
		{"2606:4700:4700::1111", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.private, isPrivateIP(tt.host))
		})
	}
}

// TestPrivateRanges_RequiredEntries locks the default BlockPrivateIPs range
// list: adding the Yggdrasil (200::/7) and CGNAT (100.64.0.0/10) ranges must
// not drop any previously covered range.
func TestPrivateRanges_RequiredEntries(t *testing.T) {
	required := []string{
		"200::/7",
		"100.64.0.0/10",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
	}

	present := make(map[string]bool, len(privateRanges))
	for _, cidr := range privateRanges {
		present[cidr] = true
	}
	for _, want := range required {
		assert.Truef(t, present[want], "privateRanges must contain %s", want)
	}
}

// TestIsPrivateAddr_ExtendedRanges is the address-level matrix for the extended
// ranges, including the IPv4-mapped IPv6 handling that must not degrade to
// "every IPv4 address is private".
func TestIsPrivateAddr_ExtendedRanges(t *testing.T) {
	tests := []struct {
		addr    string
		private bool
	}{
		{"200:1234::1", true},
		{"300::1", true},
		{"201:ffff::1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"::ffff:192.168.1.1", true},
		{"::ffff:127.0.0.1", true},
		{"fc00::1", true},
		{"fe80::1", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"100.128.0.1", false},
		{"100.63.255.255", false},
		{"2001:4860:4860::8888", false},
		{"::ffff:8.8.8.8", false},
		{"400::1", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			ip := net.ParseIP(tt.addr)
			require.NotNil(t, ip, "test address must parse as IP")
			assert.Equal(t, tt.private, isPrivateAddr(ip))
		})
	}
}

func TestStripHostPort(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:80", "127.0.0.1"},
		{"10.0.0.1:8080", "10.0.0.1"},
		{"8.8.8.8", "8.8.8.8"},
		{"[::1]:80", "::1"},
		{"[fd00::1]:443", "fd00::1"},
		{"fd00::1", "fd00::1"},
		{"example.com:443", "example.com"},
		{"example.com", "example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, stripHostPort(tt.in))
		})
	}
}
