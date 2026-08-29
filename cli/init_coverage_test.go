package cli

import (
	"context"
	"errors"
	"math"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
)

// TestSaturatingMulPinsTheConsentArithmetic covers the multiplication the
// consent quote builds on: an overflowed product goes negative and would sail
// past the cap clamp, skipping the confirmation a user is owed.
func TestSaturatingMulPinsTheConsentArithmetic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b int64
		want int64
	}{
		{0, 5, 0},
		{5, 0, 0},
		{-1, 5, 0},
		{5, -1, 0},
		{7, 6, 42},
		{math.MaxInt64, 2, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64},
	}
	for _, tc := range tests {
		if got := saturatingMul(tc.a, tc.b); got != tc.want {
			t.Errorf("saturatingMul(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestPromptFailureFailsClosed pins the exit-2 shape of a broken prompt: the
// refusal must carry the budget-stop classification and the --yes fix, never
// a silent proceed.
func TestPromptFailureFailsClosed(t *testing.T) {
	t.Parallel()
	err := promptFailure(errors.New("the terminal vanished"))
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("promptFailure = %v, want an ErrBudgetExceeded wrap so the exit code is the budget stop", err)
	}
	if code := errs.ExitCodeOf(err); code != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d", code, errs.ExitBudgetStopped)
	}
}

// TestValuePlanningCostPerCallPrefersTheEstimator pins the mirror of core's
// planning rule on the ValueOptions surface: an agent that prices itself wins
// over a caller's scalar.
func TestValuePlanningCostPerCallPrefersTheEstimator(t *testing.T) {
	t.Parallel()

	estimator := &stubEstimator{worst: 1234}
	opts := core.ValueOptions{Agent: estimator}
	if got := valuePlanningCostPerCall(opts); got != 1234 {
		t.Errorf("valuePlanningCostPerCall = %d, want the estimator's 1234", got)
	}

	if got := valuePlanningCostPerCall(core.ValueOptions{}); got != 0 {
		t.Errorf("valuePlanningCostPerCall with no agent = %d, want 0", got)
	}
}

// stubEstimator prices every Case at one worst-case figure. It embeds the
// Agent interface so the ValueOptions literal accepts it; Invoke is never
// reached by this test.
type stubEstimator struct {
	core.Agent
	worst int64
}

func (s *stubEstimator) WorstCase() budget.Estimate {
	return budget.Estimate{CostUSDMicros: s.worst, Calls: 1}
}

// Estimate satisfies the Estimator interface; this test only reads WorstCase.
func (s *stubEstimator) Estimate(_ context.Context, _ *core.Case) (budget.Estimate, error) {
	return s.WorstCase(), nil
}

// TestKindNameCoversEveryYAMLKind pins the error-name helper so a schema
// refusal can say "mapping" rather than a bare kind number.
func TestKindNameCoversEveryYAMLKind(t *testing.T) {
	t.Parallel()
	tests := map[yaml.Kind]string{
		yaml.MappingNode:  "mapping",
		yaml.SequenceNode: "list",
		yaml.ScalarNode:   "scalar",
		yaml.AliasNode:    "document",
	}
	for kind, want := range tests {
		if got := kindName(kind); got != want {
			t.Errorf("kindName(%d) = %q, want %q", kind, got, want)
		}
	}
}
