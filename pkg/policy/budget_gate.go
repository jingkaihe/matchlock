package policy

import (
	"fmt"
	"log/slog"
)

// CostProvider exposes the cumulative cost total for budget enforcement.
// The usage_logger plugin satisfies this interface via TotalCostUSD().
type CostProvider interface {
	TotalCostUSD() float64
}

// budgetGatePlugin implements GatePlugin. It blocks requests when cumulative
// API costs exceed the configured limit.
type budgetGatePlugin struct {
	limitUSD     float64
	costProvider CostProvider
	logger       *slog.Logger
}

var _ Plugin = (*budgetGatePlugin)(nil)
var _ GatePlugin = (*budgetGatePlugin)(nil)

// NewBudgetGatePlugin creates a budget_gate plugin.
// limitUSD is the maximum allowed cumulative cost in USD.
// costProvider must be non-nil and thread-safe.
func NewBudgetGatePlugin(limitUSD float64, costProvider CostProvider, logger *slog.Logger) *budgetGatePlugin {
	if logger == nil {
		logger = slog.Default()
	}
	return &budgetGatePlugin{
		limitUSD:     limitUSD,
		costProvider: costProvider,
		logger:       logger,
	}
}

func (p *budgetGatePlugin) Name() string {
	return "budget_gate"
}

// Gate implements GatePlugin. It checks the current cumulative cost against
// the configured limit.
//
// Returns Allowed=true if the current cost is below the limit.
// Returns Allowed=false with HTTP 429 and OpenAI-format JSON body if the
// budget is exceeded.
//
// The >= comparison means: if the limit is $5.00 and current spend is exactly
// $5.00, the gate blocks. The last request that pushed the total to $5.00
// was already sent (cost is recorded in the response phase). The gate
// prevents the NEXT request.
func (p *budgetGatePlugin) Gate(host string) *GateVerdict {
	currentCost := p.costProvider.TotalCostUSD()

	if currentCost >= p.limitUSD {
		reason := fmt.Sprintf(
			"budget exceeded: $%.4f spent of $%.2f limit",
			currentCost, p.limitUSD,
		)
		p.logger.Warn("budget gate blocking request",
			"host", host,
			"current_cost_usd", fmt.Sprintf("%.4f", currentCost),
			"limit_usd", fmt.Sprintf("%.2f", p.limitUSD),
		)
		return &GateVerdict{
			Allowed:     false,
			Reason:      reason,
			StatusCode:  429,
			ContentType: "application/json",
			Body: fmt.Sprintf(
				`{"error":{"message":"Budget limit exceeded. Spent $%.4f of $%.2f limit.","type":"budget_exceeded","code":429}}`,
				currentCost, p.limitUSD,
			),
		}
	}

	return &GateVerdict{Allowed: true}
}
