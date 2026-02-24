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
	p := NewSecretInjectorPlugin(nil, nil, nil)
	assert.Equal(t, "secret_injector", p.Name())
}

func TestSecretInjectorPlugin_PlaceholderGeneration(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {Value: "sk-secret-123"},
	}, nil, nil)

	placeholders := p.GetPlaceholders()
	assert.Len(t, placeholders, 1)
	assert.Contains(t, placeholders, "API_KEY")
	assert.True(t, strings.HasPrefix(placeholders["API_KEY"], "SANDBOX_SECRET_"))
}

func TestSecretInjectorPlugin_PlaceholderPreserved(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {Value: "sk-secret-123", Placeholder: "SANDBOX_SECRET_custom"},
	}, nil, nil)

	placeholders := p.GetPlaceholders()
	assert.Equal(t, "SANDBOX_SECRET_custom", placeholders["API_KEY"])
}

func TestSecretInjectorPlugin_MultiplePlaceholders(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"KEY1": {Value: "val1"},
		"KEY2": {Value: "val2"},
	}, nil, nil)

	placeholders := p.GetPlaceholders()
	assert.Len(t, placeholders, 2)
}

func TestSecretInjectorPlugin_ReplacementInHeaders(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_ReplacementInURL(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{},
		URL: &url.URL{
			RawQuery: "key=" + placeholder,
		},
	}

	result, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Contains(t, result.URL.RawQuery, "real-secret")
}

func TestSecretInjectorPlugin_NoBodyReplacement(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	body := `{"key":"` + placeholder + `"}`
	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	result, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)

	got, _ := io.ReadAll(result.Body)
	assert.NotContains(t, string(got), "real-secret")
	assert.Contains(t, string(got), placeholder)
}

func TestSecretInjectorPlugin_LeakDetection(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

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
	}, nil, nil)

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	result, err := p.TransformRequest(req, "other.com")
	require.NoError(t, err)
	assert.Equal(t, "normal-value", result.Header.Get("X-Custom"))
}

func TestSecretInjectorPlugin_EmptyHostsMeansAll(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := p.TransformRequest(req, "any-host.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_GlobHostMatch(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"*.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_HostWithPort(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := p.TransformRequest(req, "api.example.com:443")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
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

	plugin, err := NewSecretInjectorPluginFromConfig(raw, nil, nil)
	require.NoError(t, err)

	pp, ok := plugin.(PlaceholderProvider)
	require.True(t, ok)

	placeholders := pp.GetPlaceholders()
	assert.Contains(t, placeholders, "MY_KEY")
}

func TestSecretInjectorPlugin_FromConfig_Invalid(t *testing.T) {
	_, err := NewSecretInjectorPluginFromConfig(json.RawMessage(`{invalid}`), nil, nil)
	assert.Error(t, err)
}

func TestSecretInjectorPlugin_EmitsInjectedEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, emitter)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	assert.Equal(t, "key_injection", event.EventType)
	assert.Equal(t, "secret_injector", event.Plugin)
	assert.Contains(t, event.Summary, "API_KEY")
	assert.NotContains(t, event.Summary, "real-secret")

	var data logging.KeyInjectionData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "API_KEY", data.SecretName)
	assert.Equal(t, "api.example.com", data.Host)
	assert.Equal(t, "injected", data.Action)
}

func TestSecretInjectorPlugin_EmitsSkippedEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, emitter)

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	_, err := p.TransformRequest(req, "other.com")
	require.NoError(t, err)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	assert.Equal(t, "key_injection", event.EventType)
	assert.Equal(t, "secret_injector", event.Plugin)

	var data logging.KeyInjectionData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "API_KEY", data.SecretName)
	assert.Equal(t, "other.com", data.Host)
	assert.Equal(t, "skipped", data.Action)
}

func TestSecretInjectorPlugin_EmitsLeakBlockedEvent(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, emitter)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := p.TransformRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	assert.Equal(t, "key_injection", event.EventType)
	assert.Equal(t, "secret_injector", event.Plugin)

	var data logging.KeyInjectionData
	require.NoError(t, json.Unmarshal(event.Data, &data))
	assert.Equal(t, "API_KEY", data.SecretName)
	assert.Equal(t, "evil.com", data.Host)
	assert.Equal(t, "leak_blocked", data.Action)
}

func TestSecretInjectorPlugin_NilEmitterNoEvents(t *testing.T) {
	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "real-secret",
			Hosts: []string{"api.example.com"},
		},
	}, nil, nil)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	// Should work without panicking when emitter is nil
	result, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestSecretInjectorPlugin_NoSecretValuesInEvents(t *testing.T) {
	capture := &captureSink{}
	emitter := logging.NewEmitter(logging.EmitterConfig{
		RunID: "test-run", AgentSystem: "test",
	}, capture)

	p := NewSecretInjectorPlugin(map[string]api.Secret{
		"API_KEY": {
			Value: "super-secret-value-12345",
			Hosts: []string{"api.example.com"},
		},
	}, nil, emitter)

	placeholder := p.GetPlaceholders()["API_KEY"]
	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := p.TransformRequest(req, "api.example.com")
	require.NoError(t, err)

	require.Len(t, capture.events, 1)
	event := capture.events[0]
	// Check summary does not contain secret value
	assert.NotContains(t, event.Summary, "super-secret-value-12345")
	// Check data does not contain secret value
	assert.NotContains(t, string(event.Data), "super-secret-value-12345")
}
