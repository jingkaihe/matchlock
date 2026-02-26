package policy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempLogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "usage.jsonl")
}

func makeUsageResponse(statusCode int, body string, headers map[string]string) *http.Response {
	resp := &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	for k, v := range headers {
		resp.Header.Set(k, v)
	}
	return resp
}

func makeUsageRequest(method, path string) *http.Request {
	return &http.Request{
		Method: method,
		URL:    &url.URL{Path: path},
		Header: http.Header{},
	}
}

const openRouterResponseJSON = `{
	"id": "gen-abc123",
	"model": "anthropic/claude-sonnet-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello"}}],
	"usage": {
		"prompt_tokens": 1200,
		"completion_tokens": 450,
		"total_tokens": 1650,
		"cost": 0.00492,
		"prompt_tokens_details": {
			"cached_tokens": 100,
			"cache_write_tokens": 0
		},
		"completion_tokens_details": {
			"reasoning_tokens": 50
		}
	}
}`

const ollamaResponseJSON = `{
	"id": "chatcmpl-403d5a85-2631-4233-92cb-01e6dffc3c39",
	"object": "chat.completion",
	"created": 1696992706,
	"model": "llama3.2",
	"system_fingerprint": "fp_ollama",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hi"}, "finish_reason": "stop"}],
	"usage": {
		"prompt_tokens": 18,
		"completion_tokens": 25,
		"total_tokens": 43
	}
}`

const openRouterNoCostJSON = `{
	"id": "gen-xyz789",
	"model": "anthropic/claude-sonnet-4",
	"choices": [],
	"usage": {
		"prompt_tokens": 500,
		"completion_tokens": 200,
		"total_tokens": 700
	}
}`

const noUsageResponseJSON = `{
	"id": "gen-nousage",
	"model": "anthropic/claude-sonnet-4",
	"choices": []
}`

func readJSONLEntries(t *testing.T, path string) []UsageLogEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var entries []UsageLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry UsageLogEntry
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		entries = append(entries, entry)
	}
	return entries
}

// 1. Plugin Identity
func TestUsageLoggerPlugin_Name(t *testing.T) {
	p := NewUsageLoggerPlugin("", nil)
	assert.Equal(t, "usage_logger", p.Name())
}

// 2. OpenRouter Response -- Full Extraction
func TestUsageLoggerPlugin_OpenRouterResponse(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.NotEmpty(t, e.Timestamp)
	assert.Equal(t, "gen-abc123", e.GenerationID)
	assert.Equal(t, "anthropic/claude-sonnet-4", e.Model)
	assert.Equal(t, "openrouter", e.Backend)
	assert.Equal(t, "openrouter.ai", e.Host)
	assert.Equal(t, "/api/v1/chat/completions", e.Path)
	assert.Equal(t, 200, e.StatusCode)

	require.NotNil(t, e.PromptTokens)
	assert.Equal(t, 1200, *e.PromptTokens)
	require.NotNil(t, e.CompletionTokens)
	assert.Equal(t, 450, *e.CompletionTokens)
	require.NotNil(t, e.TotalTokens)
	assert.Equal(t, 1650, *e.TotalTokens)
	assert.InDelta(t, 0.00492, e.CostUSD, 0.000001)
	require.NotNil(t, e.CachedTokens)
	assert.Equal(t, 100, *e.CachedTokens)
	require.NotNil(t, e.ReasoningTokens)
	assert.Equal(t, 50, *e.ReasoningTokens)

	assert.InDelta(t, 0.00492, p.TotalCostUSD(), 0.000001)
}

// 3. Ollama Response -- Null Tokens and Zero Cost
func TestUsageLoggerPlugin_OllamaResponse(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, ollamaResponseJSON, map[string]string{
		"X-Routed-Via": "local-backend",
	})
	req := makeUsageRequest("POST", "/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, "ollama", e.Backend)
	assert.Equal(t, "llama3.2", e.Model)
	assert.Equal(t, "chatcmpl-403d5a85-2631-4233-92cb-01e6dffc3c39", e.GenerationID)
	assert.Equal(t, "/v1/chat/completions", e.Path)
	assert.Nil(t, e.PromptTokens)
	assert.Nil(t, e.CompletionTokens)
	assert.Nil(t, e.TotalTokens)
	assert.Nil(t, e.CachedTokens)
	assert.Nil(t, e.ReasoningTokens)
	assert.Equal(t, 0.0, e.CostUSD)
	assert.Equal(t, 0.0, p.TotalCostUSD())

	// Verify the raw JSON actually contains null (not 0)
	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"prompt_tokens":null`)
	assert.Contains(t, string(raw), `"completion_tokens":null`)
	assert.Contains(t, string(raw), `"total_tokens":null`)
}

// 4. Response Body Preserved
func TestUsageLoggerPlugin_BodyPreserved(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)

	// Read the body after plugin processed it
	body, err := io.ReadAll(decision.Response.Body)
	require.NoError(t, err)
	assert.Equal(t, openRouterResponseJSON, string(body))
}

// 5. Non-Matching Host -- No-Op
func TestUsageLoggerPlugin_NonMatchingHost(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "api.openai.com")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	_, statErr := os.Stat(logPath)
	assert.True(t, os.IsNotExist(statErr))
}

// 6. Non-Matching Path -- No-Op
func TestUsageLoggerPlugin_NonMatchingPath(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("GET", "/api/v1/models")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	_, statErr := os.Stat(logPath)
	assert.True(t, os.IsNotExist(statErr))
}

// 7. Host With Port
func TestUsageLoggerPlugin_HostWithPort(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	_, err := p.TransformResponse(resp, req, "openrouter.ai:443")
	assert.NoError(t, err)

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 1)
	assert.Equal(t, "openrouter.ai", entries[0].Host)
}

// 8. Non-200 Status Code -- Skip
func TestUsageLoggerPlugin_Non200StatusCode(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(400, `{"error": "bad request"}`, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	_, statErr := os.Stat(logPath)
	assert.True(t, os.IsNotExist(statErr))
}

// 9. Malformed JSON Body -- Graceful Skip
func TestUsageLoggerPlugin_MalformedJSON(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, "not json at all", nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	_, statErr := os.Stat(logPath)
	assert.True(t, os.IsNotExist(statErr))
}

// 10. Missing Usage Object -- Graceful Skip
func TestUsageLoggerPlugin_NoUsageObject(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, noUsageResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)

	_, statErr := os.Stat(logPath)
	assert.True(t, os.IsNotExist(statErr))
}

// 11. OpenRouter Response Without Cost Field
func TestUsageLoggerPlugin_NoCostField(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterNoCostJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	_, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 1)

	assert.Equal(t, 0.0, entries[0].CostUSD)
	require.NotNil(t, entries[0].PromptTokens)
	assert.Equal(t, 500, *entries[0].PromptTokens)
}

// 12. Cost Accumulator
func TestUsageLoggerPlugin_CostAccumulator(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	for i := 0; i < 3; i++ {
		resp := makeUsageResponse(200, openRouterResponseJSON, nil)
		req := makeUsageRequest("POST", "/api/v1/chat/completions")
		_, err := p.TransformResponse(resp, req, "openrouter.ai")
		require.NoError(t, err)
	}

	entries := readJSONLEntries(t, logPath)
	assert.Len(t, entries, 3)
	assert.InDelta(t, 0.00492*3, p.TotalCostUSD(), 0.000001)
}

// 13. Concurrent Safety
func TestUsageLoggerPlugin_ConcurrentWrites(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := makeUsageResponse(200, openRouterResponseJSON, nil)
			req := makeUsageRequest("POST", "/api/v1/chat/completions")
			_, err := p.TransformResponse(resp, req, "openrouter.ai")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	entries := readJSONLEntries(t, logPath)
	assert.Len(t, entries, 20)
	assert.InDelta(t, 0.00492*20, p.TotalCostUSD(), 0.000001)
}

// 14. Restore Total From Existing File
func TestUsageLoggerPlugin_RestoreTotal(t *testing.T) {
	logPath := tempLogPath(t)

	// Pre-populate the JSONL file
	existing := `{"ts":"2024-01-01T00:00:00Z","cost_usd":0.01}
{"ts":"2024-01-01T00:00:01Z","cost_usd":0.02}
{"ts":"2024-01-01T00:00:02Z","cost_usd":0.03}
`
	require.NoError(t, os.WriteFile(logPath, []byte(existing), 0644))

	p := NewUsageLoggerPlugin(logPath, nil)
	assert.InDelta(t, 0.06, p.TotalCostUSD(), 0.000001)

	// Add one more entry
	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")
	_, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)

	assert.InDelta(t, 0.06+0.00492, p.TotalCostUSD(), 0.000001)

	entries := readJSONLEntries(t, logPath)
	assert.Len(t, entries, 4)
}

// 15. Empty Log Path -- No File Write
func TestUsageLoggerPlugin_EmptyLogPath(t *testing.T) {
	p := NewUsageLoggerPlugin("", nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response)
	assert.Equal(t, 0.0, p.TotalCostUSD())
}

// 16. Factory -- Valid Config
func TestUsageLoggerPlugin_FromConfig(t *testing.T) {
	raw := json.RawMessage(`{"log_path": "/tmp/test-usage.jsonl"}`)
	plugin, err := NewUsageLoggerPluginFromConfig(raw, nil)
	assert.NoError(t, err)
	assert.NotNil(t, plugin)

	_, ok := plugin.(ResponsePlugin)
	assert.True(t, ok, "plugin should implement ResponsePlugin")
}

// 17. Factory -- Invalid Config
func TestUsageLoggerPlugin_FromConfig_Invalid(t *testing.T) {
	raw := json.RawMessage("{invalid}")
	_, err := NewUsageLoggerPluginFromConfig(raw, nil)
	assert.Error(t, err)
}

// 18. Factory -- Empty Config
func TestUsageLoggerPlugin_FromConfig_Empty(t *testing.T) {
	raw := json.RawMessage("{}")
	plugin, err := NewUsageLoggerPluginFromConfig(raw, nil)
	assert.NoError(t, err)
	assert.NotNil(t, plugin)
}

// 19. Both Paths Match
func TestUsageLoggerPlugin_BothPathsMatch(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	// OpenRouter direct path
	resp1 := makeUsageResponse(200, openRouterResponseJSON, nil)
	req1 := makeUsageRequest("POST", "/api/v1/chat/completions")
	_, err := p.TransformResponse(resp1, req1, "openrouter.ai")
	require.NoError(t, err)

	// Ollama routed path
	resp2 := makeUsageResponse(200, ollamaResponseJSON, map[string]string{
		"X-Routed-Via": "local-backend",
	})
	req2 := makeUsageRequest("POST", "/v1/chat/completions")
	_, err = p.TransformResponse(resp2, req2, "openrouter.ai")
	require.NoError(t, err)

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 2)
	assert.Equal(t, "/api/v1/chat/completions", entries[0].Path)
	assert.Equal(t, "/v1/chat/completions", entries[1].Path)
}

// 20. Response Not Modified
func TestUsageLoggerPlugin_ResponseNotModified(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, map[string]string{
		"X-Custom-Header": "test-value",
	})
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	assert.NoError(t, err)
	assert.Equal(t, resp, decision.Response) // same pointer
	assert.Equal(t, 200, decision.Response.StatusCode)
	assert.Equal(t, "test-value", decision.Response.Header.Get("X-Custom-Header"))
}

// --- New Decision Struct Tests ---

func TestUsageLoggerPlugin_Decision_LoggedUsage(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)
	assert.Equal(t, "logged_usage", decision.Action)
	assert.Contains(t, decision.Reason, "recorded $")
	assert.Contains(t, decision.Reason, "anthropic/claude-sonnet-4")
}

func TestUsageLoggerPlugin_Decision_NoOp_WrongHost(t *testing.T) {
	p := NewUsageLoggerPlugin("", nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "api.openai.com")
	require.NoError(t, err)
	assert.Equal(t, "no_op", decision.Action)
}

func TestUsageLoggerPlugin_Decision_NoOp_WrongPath(t *testing.T) {
	p := NewUsageLoggerPlugin("", nil)

	resp := makeUsageResponse(200, openRouterResponseJSON, nil)
	req := makeUsageRequest("GET", "/api/v1/models")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)
	assert.Equal(t, "no_op", decision.Action)
}

func TestUsageLoggerPlugin_Decision_NoOp_Non200(t *testing.T) {
	p := NewUsageLoggerPlugin("", nil)

	resp := makeUsageResponse(500, `{"error":"internal"}`, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)
	assert.Equal(t, "no_op", decision.Action)
}

// --- SSE Streaming Response Tests ---

const sseResponseWithUsage = `data: {"id":"gen-abc123","object":"chat.completion.chunk","model":"minimax/minimax-m2.5","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}

data: {"id":"gen-abc123","object":"chat.completion.chunk","model":"minimax/minimax-m2.5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}],"usage":null}

data: {"id":"gen-abc123","object":"chat.completion.chunk","model":"minimax/minimax-m2.5","choices":[{"index":0,"delta":{"content":" World"},"finish_reason":"stop"}],"usage":{"prompt_tokens":589,"completion_tokens":91,"total_tokens":680,"cost":0.00028,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}

data: [DONE]
`

const sseResponseNoUsage = `data: {"id":"gen-xyz","choices":[{"delta":{"content":"Hi"}}]}

data: {"id":"gen-xyz","choices":[{"delta":{"content":"!"}}]}

data: [DONE]
`

// 21. SSE Response -- Extracts Usage From Final Chunk
func TestUsageLoggerPlugin_SSEResponse(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, sseResponseWithUsage, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)
	assert.Equal(t, "logged_usage", decision.Action)
	assert.Contains(t, decision.Reason, "minimax/minimax-m2.5")

	entries := readJSONLEntries(t, logPath)
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, "gen-abc123", e.GenerationID)
	assert.Equal(t, "minimax/minimax-m2.5", e.Model)
	assert.Equal(t, "openrouter", e.Backend)
	require.NotNil(t, e.PromptTokens)
	assert.Equal(t, 589, *e.PromptTokens)
	require.NotNil(t, e.CompletionTokens)
	assert.Equal(t, 91, *e.CompletionTokens)
	require.NotNil(t, e.TotalTokens)
	assert.Equal(t, 680, *e.TotalTokens)
	assert.InDelta(t, 0.00028, e.CostUSD, 0.000001)

	assert.InDelta(t, 0.00028, p.TotalCostUSD(), 0.000001)
}

// 22. SSE Response Without Usage -- Graceful Skip
func TestUsageLoggerPlugin_SSEResponse_NoUsage(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, sseResponseNoUsage, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)
	// Falls through to "invalid JSON" since no SSE chunk has usage
	assert.Equal(t, "no_op", decision.Action)
}

// 23. SSE Response Body Preserved
func TestUsageLoggerPlugin_SSEResponse_BodyPreserved(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	resp := makeUsageResponse(200, sseResponseWithUsage, nil)
	req := makeUsageRequest("POST", "/api/v1/chat/completions")

	decision, err := p.TransformResponse(resp, req, "openrouter.ai")
	require.NoError(t, err)

	body, err := io.ReadAll(decision.Response.Body)
	require.NoError(t, err)
	assert.Equal(t, sseResponseWithUsage, string(body))
}

// 24. SSE Cost Accumulates
func TestUsageLoggerPlugin_SSEResponse_CostAccumulates(t *testing.T) {
	logPath := tempLogPath(t)
	p := NewUsageLoggerPlugin(logPath, nil)

	// First: normal JSON response
	resp1 := makeUsageResponse(200, openRouterResponseJSON, nil)
	req1 := makeUsageRequest("POST", "/api/v1/chat/completions")
	_, err := p.TransformResponse(resp1, req1, "openrouter.ai")
	require.NoError(t, err)

	// Second: SSE response
	resp2 := makeUsageResponse(200, sseResponseWithUsage, nil)
	req2 := makeUsageRequest("POST", "/api/v1/chat/completions")
	_, err = p.TransformResponse(resp2, req2, "openrouter.ai")
	require.NoError(t, err)

	entries := readJSONLEntries(t, logPath)
	assert.Len(t, entries, 2)
	assert.InDelta(t, 0.00492+0.00028, p.TotalCostUSD(), 0.000001)
}

// 25. parseSSEUsage unit test
func TestParseSSEUsage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantOK   bool
		wantID   string
		wantCost float64
	}{
		{
			name:     "valid SSE with usage in last data chunk",
			body:     sseResponseWithUsage,
			wantOK:   true,
			wantID:   "gen-abc123",
			wantCost: 0.00028,
		},
		{
			name:   "SSE without usage",
			body:   sseResponseNoUsage,
			wantOK: false,
		},
		{
			name:   "not SSE at all",
			body:   "just plain text",
			wantOK: false,
		},
		{
			name:   "empty body",
			body:   "",
			wantOK: false,
		},
		{
			name:   "single JSON object (not SSE)",
			body:   openRouterResponseJSON,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, ok := parseSSEUsage([]byte(tt.body))
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantID, parsed.ID)
				require.NotNil(t, parsed.Usage)
				require.NotNil(t, parsed.Usage.Cost)
				assert.InDelta(t, tt.wantCost, *parsed.Usage.Cost, 0.000001)
			}
		})
	}
}
