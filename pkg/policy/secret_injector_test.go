package policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSink records events in memory for test assertions.
type captureSink struct {
	mu     sync.Mutex
	events []*logging.Event
	closed bool
}

func (s *captureSink) Write(event *logging.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *event
	s.events = append(s.events, &cp)
	return nil
}

func (s *captureSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestSecretInjectorPlugin_Name(t *testing.T) {
	p := NewSecretInjectorPlugin(nil, nil)
	assert.Equal(t, "secret_injector", p.Name())
}

func TestSecretInjectorPlugin_PlaceholderGeneration(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {Value: "sk-secret-123"},
	}, nil)

	placeholders := p.GetPlaceholders()
	assert.Len(t, placeholders, 1)
	assert.Contains(t, placeholders, "API_KEY")
	assert.True(t, strings.HasPrefix(placeholders["API_KEY"], "SANDBOX_SECRET_"))
}

func TestSecretInjectorPlugin_PlaceholderPreserved(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {Value: "sk-secret-123", Placeholder: "SANDBOX_SECRET_custom"},
	}, nil)

	placeholders := p.GetPlaceholders()
	assert.Equal(t, "SANDBOX_SECRET_custom", placeholders["API_KEY"])
}

func TestSecretInjectorPlugin_MultiplePlaceholders(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"KEY1": {Value: "val1"},
		"KEY2": {Value: "val2"},
	}, nil)

	placeholders := p.GetPlaceholders()
	assert.Len(t, placeholders, 2)
}

func TestSecretInjectorPlugin_ReplacementInHeaders(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_ReplacementInURL(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{},
		URL: &url.URL{
			RawQuery: "key=" + placeholder,
		},
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Contains(t, decision.Request.URL.RawQuery, "real-secret")
}

func TestSecretInjectorPlugin_NoBodyReplacement(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	body := `{"key":"` + placeholder + `"}`
	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)

	got, _ := io.ReadAll(decision.Request.Body)
	assert.NotContains(t, string(got), "real-secret")
	assert.Contains(t, string(got), placeholder)
}

func TestSecretInjectorPlugin_LeakDetection(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := p.TransformRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak)
}

func TestSecretInjectorPlugin_NoSecretForHost(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "other.com")
	require.NoError(t, err)
	assert.Equal(t, "normal-value", decision.Request.Header.Get("X-Custom"))
}

func TestSecretInjectorPlugin_EmptyHostsMeansAll(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "any-host.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_GlobHostMatch(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"*.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_HostWithPort(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "api.example.com:443")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_FromConfig(t *testing.T) {
	raw := json.RawMessage(`{
		"secrets": {
			"MY_KEY": {
				"value": "real-value",
				"hosts": ["api.example.com"]
			}
		}
	}`)

	plugin, err := NewSecretInjectorPluginFromConfig(raw, nil)
	require.NoError(t, err)

	pp, ok := plugin.(PlaceholderProvider)
	require.True(t, ok)

	placeholders := pp.GetPlaceholders()
	assert.Contains(t, placeholders, "MY_KEY")
}

func TestSecretInjectorPlugin_FromConfig_Invalid(t *testing.T) {
	_, err := NewSecretInjectorPluginFromConfig(json.RawMessage(`{invalid}`), nil)
	assert.Error(t, err)
}

// --- New Decision Struct Tests ---

func TestSecretInjectorPlugin_Decision_Injected(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "injected", decision.Action)
	assert.Contains(t, decision.Reason, "1 secret(s) injected")
	assert.NotNil(t, decision.Request)
	assert.Equal(t, "Bearer real-secret", decision.Request.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_Decision_Skipped(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "other.com")
	require.NoError(t, err)
	assert.Equal(t, "skipped", decision.Action)
	assert.Contains(t, decision.Reason, "1 secret(s) skipped")
	assert.NotNil(t, decision.Request)
}

func TestSecretInjectorPlugin_Decision_NoOp(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{}, nil)

	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
	}

	decision, err := p.TransformRequest(req, "any-host.com")
	require.NoError(t, err)
	assert.Equal(t, "no_op", decision.Action)
	assert.NotNil(t, decision.Request)
}

func TestSecretInjectorPlugin_Decision_LeakReturnsError(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak)
	assert.Nil(t, decision)
}

func TestSecretInjectorPlugin_Decision_NoSecretValues(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "super-secret-value-12345",
			Hosts: []string{"api.example.com"},
		},
	}, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	decision, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.NotContains(t, decision.Reason, "super-secret-value-12345")
	assert.NotContains(t, decision.Reason, placeholder)
}
