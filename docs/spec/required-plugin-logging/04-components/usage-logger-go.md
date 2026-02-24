# Component: pkg/policy/usage_logger.go

**File:** `pkg/policy/usage_logger.go`
**Agent:** 2 (Plugin Migration)
**Pattern Reference:** Current implementation in
[`pkg/policy/usage_logger.go`](../../../pkg/policy/usage_logger.go).

## Purpose

Migrate `usageLoggerPlugin` to return `*ResponseDecision` from
`TransformResponse()`. Update the factory signature. The usage logger's own
JSONL file output is independent and unchanged.

## Changes Required

### 1. Change NewUsageLoggerPluginFromConfig Signature

```go
// Before:
func NewUsageLoggerPluginFromConfig(raw json.RawMessage, logger *slog.Logger, _ *logging.Emitter) (Plugin, error) {

// After:
func NewUsageLoggerPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
```

### 2. Remove logging Import

Remove `"github.com/jingkaihe/matchlock/pkg/logging"` since it was only used
for the `emitter` parameter type (which was already ignored with `_`).

### 3. Change TransformResponse Return Type

The method currently returns `(*http.Response, error)`. Change to
`(*ResponseDecision, error)`.

The key challenge: the current method has many early returns (non-matching host,
wrong path, non-200, parse failure). Each must return a `ResponseDecision`.

```go
func (p *usageLoggerPlugin) TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error) {
	host = strings.Split(host, ":")[0]

	// Guard: only intercept openrouter.ai
	if host != "openrouter.ai" {
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   fmt.Sprintf("skipped: host %s is not openrouter.ai", host),
		}, nil
	}

	// Guard: only intercept chat completions paths
	path := req.URL.Path
	if path != "/api/v1/chat/completions" && path != "/v1/chat/completions" {
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   fmt.Sprintf("skipped: path %s is not a chat completions endpoint", path),
		}, nil
	}

	// Guard: only log successful responses
	if resp.StatusCode != 200 {
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   fmt.Sprintf("skipped: status %d is not 200", resp.StatusCode),
		}, nil
	}

	// Read the response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		p.logger.Warn("usage_logger: failed to read response body", "error", err)
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   "skipped: failed to read response body",
		}, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Parse the response
	var parsed openRouterResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		p.logger.Warn("usage_logger: failed to parse response JSON", "error", err)
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   "skipped: invalid JSON in response body",
		}, nil
	}

	if parsed.Usage == nil {
		p.logger.Warn("usage_logger: response missing usage object", "id", parsed.ID)
		return &ResponseDecision{
			Response: resp,
			Action:   "no_op",
			Reason:   "skipped: response missing usage object",
		}, nil
	}

	// Determine backend
	backend := "openrouter"
	if resp.Header.Get("X-Routed-Via") == "local-backend" {
		backend = "ollama"
	}

	// Build log entry (unchanged from current implementation)
	entry := &UsageLogEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		GenerationID: parsed.ID,
		Model:        parsed.Model,
		Backend:      backend,
		Host:         host,
		Path:         path,
		StatusCode:   resp.StatusCode,
	}

	if backend == "openrouter" {
		entry.PromptTokens = intPtr(parsed.Usage.PromptTokens)
		entry.CompletionTokens = intPtr(parsed.Usage.CompletionTokens)
		entry.TotalTokens = intPtr(parsed.Usage.TotalTokens)
		if parsed.Usage.Cost != nil {
			entry.CostUSD = *parsed.Usage.Cost
		}
		if parsed.Usage.PromptTokensDetails != nil {
			entry.CachedTokens = intPtr(parsed.Usage.PromptTokensDetails.CachedTokens)
		} else {
			entry.CachedTokens = intPtr(0)
		}
		if parsed.Usage.CompletionTokensDetails != nil {
			entry.ReasoningTokens = intPtr(parsed.Usage.CompletionTokensDetails.ReasoningTokens)
		} else {
			entry.ReasoningTokens = intPtr(0)
		}
	} else {
		entry.PromptTokens = nil
		entry.CompletionTokens = nil
		entry.TotalTokens = nil
		entry.CachedTokens = nil
		entry.ReasoningTokens = nil
		entry.CostUSD = 0.0
	}

	// Append to log file
	if p.logPath != "" {
		if err := p.appendLogEntry(entry); err != nil {
			p.logger.Warn("usage_logger: failed to write log entry", "error", err)
			return &ResponseDecision{
				Response: resp,
				Action:   "no_op",
				Reason:   "skipped: failed to write log entry",
			}, nil
		}
	}

	p.logger.Debug("usage logged",
		"model", entry.Model,
		"backend", entry.Backend,
		"cost_usd", entry.CostUSD,
	)

	return &ResponseDecision{
		Response: resp,
		Action:   "logged_usage",
		Reason:   fmt.Sprintf("recorded $%.4f cost for %s via %s", entry.CostUSD, entry.Model, backend),
	}, nil
}
```

## Key Behavior Notes

1. **The JSONL file output is independent.** The `appendLogEntry()` call and
   all JSONL file handling remain unchanged. The `ResponseDecision` is about
   the structured event log, not the usage JSONL file.

2. **Many no_op returns.** The usage logger has many guard conditions. Each
   returns a `ResponseDecision` with `Action: "no_op"` and a descriptive
   reason. This is correct -- the engine will emit a `response_transform`
   event for each, recording that the plugin was called and chose to skip.

3. **No error returns.** The usage logger never returns errors. All failures
   are logged and result in a no_op response. This matches current behavior.

4. **The response pointer is always returned.** Even on skip/no_op paths, the
   original `resp` is set in `ResponseDecision.Response`. The engine chains
   the response forward.

## Verification

- Plugin compiles against new `ResponsePlugin` interface
- No references to `logging.Emitter` remain
- JSONL file output is identical for all test cases
- `TotalCostUSD()` accumulation is unchanged
