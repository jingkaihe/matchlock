package policy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockCostProvider is a test helper that returns a fixed cost.
type mockCostProvider struct {
	cost float64
}

func (m *mockCostProvider) TotalCostUSD() float64 {
	return m.cost
}

func TestBudgetGate_Name(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 0}, nil)
	assert.Equal(t, "budget_gate", p.Name())
}

func TestBudgetGate_AllowsWhenUnderLimit(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 2.0}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.True(t, verdict.Allowed)
}

func TestBudgetGate_BlocksWhenAtLimit(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.0}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.False(t, verdict.Allowed, "should block when cost equals limit (>= comparison)")
}

func TestBudgetGate_BlocksWhenOverLimit(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.50}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.False(t, verdict.Allowed)
}

func TestBudgetGate_ZeroCostAllows(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 0.0}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.True(t, verdict.Allowed)
}

func TestBudgetGate_VerdictStatusCode429(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.0}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.Equal(t, 429, verdict.StatusCode)
}

func TestBudgetGate_VerdictContentTypeJSON(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.0}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.Equal(t, "application/json", verdict.ContentType)
}

func TestBudgetGate_VerdictJSONBody(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.50}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.False(t, verdict.Allowed)

	// Body should be valid JSON
	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(verdict.Body), &parsed)
	require.NoError(t, err, "verdict body should be valid JSON")

	// Should contain error object with message, type, and code
	errorObj, ok := parsed["error"].(map[string]interface{})
	require.True(t, ok, "should have error object")

	msg, ok := errorObj["message"].(string)
	require.True(t, ok)
	assert.Contains(t, msg, "5.5000", "message should contain current cost")
	assert.Contains(t, msg, "5.00", "message should contain limit")

	assert.Equal(t, "budget_exceeded", errorObj["type"])
	assert.Equal(t, float64(429), errorObj["code"])
}

func TestBudgetGate_VerdictReasonContainsCosts(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 5.50}, nil)

	verdict := p.Gate("openrouter.ai")
	assert.Contains(t, verdict.Reason, "5.5000", "reason should contain current cost")
	assert.Contains(t, verdict.Reason, "5.00", "reason should contain limit")
}

func TestBudgetGate_AllowsAnyHost(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 2.0}, nil)

	assert.True(t, p.Gate("openrouter.ai").Allowed)
	assert.True(t, p.Gate("api.openai.com").Allowed)
	assert.True(t, p.Gate("example.com").Allowed)
}

func TestBudgetGate_BlocksAnyHost(t *testing.T) {
	p := NewBudgetGatePlugin(5.0, &mockCostProvider{cost: 10.0}, nil)

	assert.False(t, p.Gate("openrouter.ai").Allowed)
	assert.False(t, p.Gate("api.openai.com").Allowed)
	assert.False(t, p.Gate("example.com").Allowed)
}

func TestBudgetGate_InterfaceCompliance(t *testing.T) {
	var _ Plugin = (*budgetGatePlugin)(nil)
	var _ GatePlugin = (*budgetGatePlugin)(nil)
}
