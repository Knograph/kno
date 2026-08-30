package vertex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// estimateAgent builds an Agent the estimate tests can use without any
// network: estimate touches only the pricing table and the options.
func estimateAgent(opts Options) *Agent {
	a := &Agent{opts: opts}
	a.worst = a.computeWorstCase()
	return a
}

// TestEstimateRegional asserts the per-Case reservation carries the #41(d)
// multiplier: base arithmetic times 1.10, rounded up.
func TestEstimateRegional(t *testing.T) {
	t.Parallel()

	a := estimateAgent(Options{Model: testModel, MaxOutputTokens: 1024, System: "grade"})
	est, err := a.Estimate(context.Background(), &core.Case{
		Id:    "c1",
		Input: "input",
		History: []*knov1.Turn{
			{Role: knov1.Role_ROLE_USER, Content: "history"},
			{Role: knov1.Role_ROLE_ASSISTANT, Content: "answer"},
		},
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.CostUSDMicros <= 0 {
		t.Fatalf("cost = %d, want > 0", est.CostUSDMicros)
	}

	// The multiplier is exactly 10%: recompute without it and compare.
	base, err := pricing.Estimate(Scheme, testModel, pricing.Prompt{
		System: "grade", History: "history\n\nanswer", Input: "input", Context: "",
	}, 1024)
	if err != nil {
		t.Fatalf("pricing.Estimate: %v", err)
	}
	want := base.CostUSDMicros + ceilDiv(base.CostUSDMicros*10, 100)
	if est.CostUSDMicros != want {
		t.Errorf("cost = %d, want %d (base %d + 10%% regional)", est.CostUSDMicros, want, base.CostUSDMicros)
	}
}

// TestEstimateNilCase asserts the same refusal as the invoke path.
func TestEstimateNilCase(t *testing.T) {
	t.Parallel()

	a := estimateAgent(Options{Model: testModel, MaxOutputTokens: 1024})
	if _, err := a.Estimate(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "vertex: nil case") {
		t.Errorf("err = %v, want vertex: nil case", err)
	}
}

// TestEstimatePriceOverride asserts a caller's own price is used and still
// multiplied regionally.
func TestEstimatePriceOverride(t *testing.T) {
	t.Parallel()

	p := &knov1.Price{
		InputPerMtokUsdMicros:  ptr(usd(1)),
		OutputPerMtokUsdMicros: ptr(usd(2)),
	}
	a := estimateAgent(Options{Model: testModel, MaxOutputTokens: 100, Price: p})
	est, err := a.Estimate(context.Background(), &core.Case{Id: "c1", Input: "x"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	base, err := pricing.EstimateWithPrice(p, testModel, pricing.Prompt{Input: "x"}, 100)
	if err != nil {
		t.Fatalf("EstimateWithPrice: %v", err)
	}
	want := base.CostUSDMicros + ceilDiv(base.CostUSDMicros*10, 100)
	if est.CostUSDMicros != want {
		t.Errorf("cost = %d, want %d", est.CostUSDMicros, want)
	}
}

// TestEstimateUnpriced asserts the refusal is run-fatal and names the model.
func TestEstimateUnpriced(t *testing.T) {
	t.Parallel()

	a := estimateAgent(Options{Model: "claude-nowhere-1", MaxOutputTokens: 100})
	_, err := a.Estimate(context.Background(), &core.Case{Id: "c1", Input: "x"})
	if err == nil {
		t.Fatal("Estimate succeeded for an unpriced model")
	}
	var rf interface{ RunFatal() bool }
	if !errors.As(err, &rf) || !rf.RunFatal() {
		t.Errorf("err = %v, want run-fatal", err)
	}
	if !errors.Is(err, pricing.ErrUnpriced) {
		t.Errorf("err = %v, want ErrUnpriced", err)
	}
}

// TestEstimateProfileRefused asserts a us./eu.-prefixed cross-region
// inference profile is refused with the class named — no row claims the
// destination region's rate, and the refusal says exactly that.
func TestEstimateProfileRefused(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"us.claude-sonnet-4-5", "eu.claude-opus-4-7"} {
		a := estimateAgent(Options{Model: model, MaxOutputTokens: 100})
		_, err := a.Estimate(context.Background(), &core.Case{Id: "c1", Input: "x"})
		if err == nil {
			t.Fatalf("Estimate succeeded for %s", model)
		}
		var ae *errs.Actionable
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want Actionable", err)
		}
		for _, want := range []string{"cross-region inference profile", "destination region"} {
			if !strings.Contains(ae.Fix, want) {
				t.Errorf("fix for %s = %q, want it to contain %q", model, ae.Fix, want)
			}
		}
	}
}

// TestWorstCase asserts the consent quote is memoized and regional.
func TestWorstCase(t *testing.T) {
	t.Parallel()

	a := estimateAgent(Options{Model: testModel, MaxOutputTokens: 1024})
	first := a.WorstCase()
	second := a.WorstCase()
	if first != second {
		t.Errorf("WorstCase is not memoized: %v vs %v", first, second)
	}
	if first.CostUSDMicros <= 0 {
		t.Errorf("WorstCase cost = %d, want > 0", first.CostUSDMicros)
	}

	// The 10% multiplier is in the quote: strip it and compare to a
	// non-regional WorstCase construction.
	base, err := pricing.Estimate(Scheme, testModel, pricing.Prompt{
		System: "", Context: "", Input: strings.Repeat("x", int(defaultWorstCasePromptBytes)),
	}, 1024)
	if err != nil {
		t.Fatalf("pricing.Estimate: %v", err)
	}
	want := base.CostUSDMicros + ceilDiv(base.CostUSDMicros*10, 100)
	if first.CostUSDMicros != want {
		t.Errorf("WorstCase cost = %d, want %d", first.CostUSDMicros, want)
	}
}

// TestWorstCaseCeilingClamped asserts a fat-fingered ceiling cannot turn the
// quote into a multi-gigabyte allocation.
func TestWorstCaseCeilingClamped(t *testing.T) {
	t.Parallel()

	a := estimateAgent(Options{Model: testModel, MaxOutputTokens: 100, MaxPromptBytes: 1 << 40})
	if got := a.promptCeiling(); got != maxWorstCasePromptBytes {
		t.Errorf("promptCeiling = %d, want %d", got, maxWorstCasePromptBytes)
	}
	if a.WorstCase().CostUSDMicros <= 0 {
		t.Error("clamped WorstCase cost is zero")
	}
}

func ceilDiv(n, d int64) int64 {
	return (n + d - 1) / d
}

func usd(v float64) int64 { return int64(v * 1_000_000) }

func ptr(v int64) *int64 { return &v }
