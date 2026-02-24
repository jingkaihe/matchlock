package logging

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvent_JSONFieldNames(t *testing.T) {
	event := &Event{
		Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 123000000, time.UTC),
		RunID:       "session-9f8e7d6c",
		AgentSystem: "openclaw",
		EventType:   EventHTTPRequest,
		Summary:     "POST api.anthropic.com/v1/messages",
	}
	b, err := json.Marshal(event)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))

	assert.Contains(t, m, "ts")
	assert.Contains(t, m, "run_id")
	assert.Contains(t, m, "agent_system")
	assert.Contains(t, m, "event_type")
	assert.Contains(t, m, "summary")
	// Omitempty fields absent
	assert.NotContains(t, m, "plugin")
	assert.NotContains(t, m, "tags")
	assert.NotContains(t, m, "data")
}

func TestEvent_OmitemptyPresent(t *testing.T) {
	event := &Event{
		Timestamp:   time.Now().UTC(),
		RunID:       "test",
		AgentSystem: "test",
		EventType:   EventKeyInjection,
		Summary:     "test",
		Plugin:      "secret_injector",
		Tags:        []string{"tls"},
		Data:        json.RawMessage(`{"action":"injected"}`),
	}
	b, err := json.Marshal(event)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))

	assert.Contains(t, m, "plugin")
	assert.Contains(t, m, "tags")
	assert.Contains(t, m, "data")
}

func TestEvent_TimestampFormat(t *testing.T) {
	ts := time.Date(2026, 2, 23, 14, 30, 0, 123456789, time.UTC)
	event := &Event{Timestamp: ts, RunID: "r", AgentSystem: "a", EventType: "t", Summary: "s"}

	b, err := json.Marshal(event)
	require.NoError(t, err)

	// Verify RFC 3339 with sub-second precision
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	tsStr := m["ts"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, tsStr)
	require.NoError(t, err)
	assert.True(t, parsed.Equal(ts))
}

func TestHTTPRequestData_RoutedNotOmitted(t *testing.T) {
	data := &HTTPRequestData{
		Method: "POST",
		Host:   "api.anthropic.com",
		Path:   "/v1/messages",
		Routed: false,
	}
	b, err := json.Marshal(data)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Contains(t, m, "routed", "routed field must be present even when false")
	assert.Equal(t, false, m["routed"])
}

func TestKeyInjectionData_ActionAlwaysPresent(t *testing.T) {
	data := &KeyInjectionData{
		SecretName: "API_KEY",
		Host:       "api.example.com",
		Action:     "injected",
	}
	b, err := json.Marshal(data)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Contains(t, m, "action")
}

func TestEventTypeConstants(t *testing.T) {
	assert.Equal(t, "http_request", EventHTTPRequest)
	assert.Equal(t, "http_response", EventHTTPResponse)
	assert.Equal(t, "key_injection", EventKeyInjection)
	assert.Equal(t, "gate_decision", EventGateDecision)
}
