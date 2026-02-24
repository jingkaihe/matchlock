package policy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostFilterPlugin_NoRestrictions(t *testing.T) {
	p := NewHostFilterPlugin(nil, false, nil, nil)

	verdict := p.Gate("example.com")
	assert.True(t, verdict.Allowed, "All hosts should be allowed when no restrictions")
}

func TestHostFilterPlugin_Allowlist(t *testing.T) {
	p := NewHostFilterPlugin([]string{"api.openai.com", "*.anthropic.com"}, false, nil, nil)

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
			verdict := p.Gate(tt.host)
			assert.Equal(t, tt.allowed, verdict.Allowed)
		})
	}
}

func TestHostFilterPlugin_BlockPrivateIPs(t *testing.T) {
	p := NewHostFilterPlugin(nil, true, nil, nil)

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
			verdict := p.Gate(tt.host)
			assert.Equal(t, tt.allowed, verdict.Allowed)
		})
	}
}

func TestHostFilterPlugin_AllowedPrivateHosts(t *testing.T) {
	p := NewHostFilterPlugin(nil, true, []string{"192.168.1.100"}, nil)

	verdict := p.Gate("192.168.1.100")
	assert.True(t, verdict.Allowed, "Explicitly allowed private IP should be allowed")

	verdict = p.Gate("192.168.1.101")
	assert.False(t, verdict.Allowed, "Non-allowed private IP should be blocked")

	verdict = p.Gate("10.0.0.1")
	assert.False(t, verdict.Allowed, "Other private IP should be blocked")

	verdict = p.Gate("8.8.8.8")
	assert.True(t, verdict.Allowed, "Public IP should still be allowed")
}

func TestHostFilterPlugin_AllowedPrivateHostsGlob(t *testing.T) {
	p := NewHostFilterPlugin(nil, true, []string{"192.168.64.*"}, nil)

	verdict := p.Gate("192.168.64.1")
	assert.True(t, verdict.Allowed)

	verdict = p.Gate("192.168.64.255")
	assert.True(t, verdict.Allowed)

	verdict = p.Gate("192.168.65.1")
	assert.False(t, verdict.Allowed)

	verdict = p.Gate("10.0.0.1")
	assert.False(t, verdict.Allowed)
}

func TestHostFilterPlugin_WithPort(t *testing.T) {
	p := NewHostFilterPlugin([]string{"api.example.com"}, false, nil, nil)

	verdict := p.Gate("api.example.com:443")
	assert.True(t, verdict.Allowed, "Should allow host with port")
}

func TestHostFilterPlugin_BlockedReason(t *testing.T) {
	p := NewHostFilterPlugin([]string{"allowed.com"}, false, nil, nil)

	verdict := p.Gate("blocked.com")
	assert.Equal(t, "host not in allowlist", verdict.Reason)
}

func TestHostFilterPlugin_PrivateBlockedReason(t *testing.T) {
	p := NewHostFilterPlugin(nil, true, nil, nil)

	verdict := p.Gate("192.168.1.1")
	assert.Equal(t, "private IP blocked", verdict.Reason)
}

func TestHostFilterPlugin_Name(t *testing.T) {
	p := NewHostFilterPlugin(nil, false, nil, nil)
	assert.Equal(t, "host_filter", p.Name())
}

func TestHostFilterPlugin_FromConfig(t *testing.T) {
	raw := json.RawMessage(`{
		"allowed_hosts": ["only-this.com", "192.168.1.1"],
		"block_private_ips": true,
		"allowed_private_hosts": ["192.168.1.1"]
	}`)

	plugin, err := NewHostFilterPluginFromConfig(raw, nil)
	require.NoError(t, err)

	gp, ok := plugin.(GatePlugin)
	require.True(t, ok)

	verdict := gp.Gate("only-this.com")
	assert.True(t, verdict.Allowed)

	verdict = gp.Gate("other.com")
	assert.False(t, verdict.Allowed)

	// Private IP in both AllowedPrivateHosts and AllowedHosts should be allowed
	verdict = gp.Gate("192.168.1.1")
	assert.True(t, verdict.Allowed)

	verdict = gp.Gate("10.0.0.1")
	assert.False(t, verdict.Allowed)
}

func TestHostFilterPlugin_FromConfig_Invalid(t *testing.T) {
	_, err := NewHostFilterPluginFromConfig(json.RawMessage(`{invalid}`), nil)
	assert.Error(t, err)
}

func TestHostFilterPlugin_BlockedVerdictDefaultFields(t *testing.T) {
	p := NewHostFilterPlugin([]string{"allowed.com"}, false, nil, nil)

	verdict := p.Gate("blocked.com")
	require.False(t, verdict.Allowed)
	assert.Equal(t, 0, verdict.StatusCode, "host_filter should leave StatusCode at zero")
	assert.Empty(t, verdict.ContentType, "host_filter should leave ContentType empty")
	assert.Empty(t, verdict.Body, "host_filter should leave Body empty")
}
