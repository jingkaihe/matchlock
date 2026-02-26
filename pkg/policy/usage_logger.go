package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// UsageLoggerConfig is the typed config for the usage_logger plugin.
type UsageLoggerConfig struct {
	LogPath string `json:"log_path,omitempty"`
}

// UsageLogEntry represents a single LLM API call intercepted by the usage_logger.
// Written as one JSON line per entry to the JSONL log file.
type UsageLogEntry struct {
	Timestamp        string  `json:"ts"`
	GenerationID     string  `json:"generation_id"`
	Model            string  `json:"model"`
	Backend          string  `json:"backend"`
	Host             string  `json:"host"`
	Path             string  `json:"path"`
	StatusCode       int     `json:"status_code"`
	PromptTokens     *int    `json:"prompt_tokens"`
	CompletionTokens *int    `json:"completion_tokens"`
	TotalTokens      *int    `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CachedTokens     *int    `json:"cached_tokens"`
	ReasoningTokens  *int    `json:"reasoning_tokens"`
}

// usageLoggerPlugin implements ResponsePlugin.
// It intercepts OpenRouter API responses, extracts token/cost data,
// and writes JSONL log entries.
type usageLoggerPlugin struct {
	logPath      string
	totalCostUSD float64
	mu           sync.Mutex
	logger       *slog.Logger
}

// openRouterResponse is the minimal structure parsed from OpenRouter/Ollama
// chat completions response bodies.
type openRouterResponse struct {
	ID    string           `json:"id"`
	Model string           `json:"model"`
	Usage *openRouterUsage `json:"usage"`
}

type openRouterUsage struct {
	PromptTokens            int                          `json:"prompt_tokens"`
	CompletionTokens        int                          `json:"completion_tokens"`
	TotalTokens             int                          `json:"total_tokens"`
	Cost                    *float64                     `json:"cost"`
	PromptTokensDetails     *openRouterTokenDetails      `json:"prompt_tokens_details"`
	CompletionTokensDetails *openRouterCompletionDetails `json:"completion_tokens_details"`
}

type openRouterTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type openRouterCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

var _ Plugin = (*usageLoggerPlugin)(nil)
var _ ResponsePlugin = (*usageLoggerPlugin)(nil)

// NewUsageLoggerPlugin creates a usage_logger plugin from typed config.
func NewUsageLoggerPlugin(logPath string, logger *slog.Logger) *usageLoggerPlugin {
	if logger == nil {
		logger = slog.Default()
	}
	p := &usageLoggerPlugin{
		logPath: logPath,
		logger:  logger,
	}
	p.restoreTotal()
	return p
}

// NewUsageLoggerPluginFromConfig creates a usage_logger plugin from JSON config.
func NewUsageLoggerPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var cfg UsageLoggerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return NewUsageLoggerPlugin(cfg.LogPath, logger), nil
}

func (p *usageLoggerPlugin) Name() string {
	return "usage_logger"
}

func (p *usageLoggerPlugin) TransformResponse(resp *http.Response, req *http.Request, host string) (*ResponseDecision, error) {
	// Strip port from host
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
	// Reassign body so downstream can still read it
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

	// Build log entry
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
		// Ollama: null tokens, zero cost
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

// TotalCostUSD returns the current running total cost in USD. Thread-safe.
func (p *usageLoggerPlugin) TotalCostUSD() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totalCostUSD
}

func (p *usageLoggerPlugin) appendLogEntry(entry *UsageLogEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal usage entry: %w", err)
	}
	line = append(line, '\n')

	p.mu.Lock()
	defer p.mu.Unlock()

	f, err := os.OpenFile(p.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open usage log: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write usage entry: %w", err)
	}

	p.totalCostUSD += entry.CostUSD
	return nil
}

func (p *usageLoggerPlugin) restoreTotal() {
	if p.logPath == "" {
		return
	}
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		return
	}
	var total float64
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry struct {
			CostUSD float64 `json:"cost_usd"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		total += entry.CostUSD
	}
	p.totalCostUSD = total
	if total > 0 {
		p.logger.Info("restored usage total from existing log",
			"path", p.logPath,
			"total_cost_usd", fmt.Sprintf("%.6f", total))
	}
}

func intPtr(v int) *int {
	return &v
}
