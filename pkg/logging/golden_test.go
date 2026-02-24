package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvent_GoldenFull(t *testing.T) {
	event := &Event{
		Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 123000000, time.UTC),
		RunID:       "session-9f8e7d6c",
		AgentSystem: "openclaw",
		EventType:   EventHTTPRequest,
		Summary:     "POST api.anthropic.com/v1/messages",
		Plugin:      "secret_injector",
		Tags:        []string{"tls", "mitm"},
		Data:        json.RawMessage(`{"method":"POST","host":"api.anthropic.com","path":"/v1/messages","model":"claude-sonnet-4-20250514","routed":false}`),
	}

	got, err := json.Marshal(event)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "event_full.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.MkdirAll("testdata", 0755)
		os.WriteFile(goldenPath, append(got, '\n'), 0644)
		t.Skip("golden file updated")
	}

	expected, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; run with UPDATE_GOLDEN=1 to create")

	assert.JSONEq(t, string(expected), string(got))
}

func TestEvent_GoldenMinimal(t *testing.T) {
	event := &Event{
		Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 0, time.UTC),
		RunID:       "vm-a1b2c3d4",
		AgentSystem: "unknown",
		EventType:   EventHTTPResponse,
		Summary:     "GET example.com/ -> 200",
	}

	got, err := json.Marshal(event)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "event_minimal.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.MkdirAll("testdata", 0755)
		os.WriteFile(goldenPath, append(got, '\n'), 0644)
		t.Skip("golden file updated")
	}

	expected, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; run with UPDATE_GOLDEN=1 to create")

	assert.JSONEq(t, string(expected), string(got))
}
