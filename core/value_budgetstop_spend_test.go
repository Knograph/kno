package core_test

import (
	"context"
	"testing"

	"github.com/knograph/kno/adapters/agent/fake"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// TestBudgetStoppedRunStillReportsWhatItSpent is the plan's AC15.
//
// A run stopped by its own cost cap is the case where the reported figure
// matters most and is easiest to lose: the stage ended on the money path, so
// a naive implementation returns the budget error and drops the result. The
// figure must survive, and it must equal what the store recorded — the
// disagreement between the guard and the store is the failure
// docs/debt.md#50 already cost this project once.
func TestBudgetStoppedRunStillReportsWhatItSpent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newValueHarness(t, fake.Options{CostPerCallUSDMicros: 1000})
	// A cap that admits a few measurements, not the run.
	h.guard = budget.New(budget.Limits{MaxCostUSDMicros: 3000, MaxLLMCalls: 1000}, nil, 0)
	h.opts.Guard = h.guard

	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	result, err := h.opts.Value(ctx, poolOf("a1", "a2"))
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if result.Status != knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		t.Fatalf("Status = %v, want BUDGET_STOPPED — the fixture is not binding the cap",
			result.Status)
	}
	if result.Spent.CostUSDMicros == 0 || result.Spent.Calls == 0 {
		t.Fatalf("a budget-stopped run reports %+v; it stopped BECAUSE it spent",
			result.Spent)
	}
	settled, err := h.store.SettledSpend(ctx, h.opts.RunID)
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if result.Spent.Calls != settled.Calls ||
		result.Spent.CostUSDMicros != settled.CostUSDMicros {
		t.Errorf("the guard settled %d call(s) / %d micros, the store recorded %d / %d",
			result.Spent.Calls, result.Spent.CostUSDMicros,
			settled.Calls, settled.CostUSDMicros)
	}
}
