package bridge_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

func quotePortfolio() *knov1.Portfolio {
	return &knov1.Portfolio{Selected: []*knov1.PortfolioEntry{
		{AssetId: "a", Destination: knov1.Destination_DESTINATION_TUNING_SET},
		{AssetId: "b", Destination: knov1.Destination_DESTINATION_TUNING_SET},
	}}
}

func quoteAssets() map[string]*core.Asset {
	return map[string]*core.Asset{
		"a": {Id: "a", Content: []byte("refund demonstration one")},
		"b": {Id: "b", Content: []byte("billing demonstration one")},
	}
}

// TestQuoteGroupsSpendsNothing is the property Step 4 names directly: the
// un-armed plan makes zero network calls and spends zero — this test
// asserts it structurally, by construction: QuoteGroups takes no core.Tuner
// and no budget.Guard, so there is nothing here that COULD spend.
func TestQuoteGroupsSpendsNothing(t *testing.T) {
	t.Parallel()

	plan := &bridge.GroupsPlan{AllIn: []string{"a", "b"}, LeaveOneOut: map[string][]string{}}
	price := pricing.TrainPrice{PerMTokUSDMicros: 1_000_000}

	quotes, err := bridge.QuoteGroups(quotePortfolio(), plan, quoteAssets(), "together:meta-llama/Llama-3-8b", price, 1)
	if err != nil {
		t.Fatalf("QuoteGroups: %v", err)
	}
	if len(quotes) != 1 || quotes[0].Group != bridge.AllIn {
		t.Fatalf("got %+v, want exactly one all-in quote", quotes)
	}
	if quotes[0].EstimatedCostUSDMicros <= 0 {
		t.Error("estimate is not positive; a job is never submitted without one")
	}
	if quotes[0].TrainingFileSHA256 == "" {
		t.Error("TrainingFileSHA256 is empty")
	}
}

// TestQuoteGroupsMatchesGroupsPlanOrder pins that quotes come back in
// GroupsPlan.Groups()'s order: all-in first, then leave-one-out groups
// sorted by tag — the order the consent quote and job table print in.
func TestQuoteGroupsMatchesGroupsPlanOrder(t *testing.T) {
	t.Parallel()

	plan := &bridge.GroupsPlan{
		AllIn: []string{"a", "b"},
		LeaveOneOut: map[string][]string{
			"billing": {"a"},
			"refunds": {"b"},
		},
	}
	price := pricing.TrainPrice{PerMTokUSDMicros: 1_000_000}

	quotes, err := bridge.QuoteGroups(quotePortfolio(), plan, quoteAssets(), "together:meta-llama/Llama-3-8b", price, 1)
	if err != nil {
		t.Fatalf("QuoteGroups: %v", err)
	}
	want := []string{bridge.AllIn, "billing", "refunds"}
	if len(quotes) != len(want) {
		t.Fatalf("got %d quotes, want %d", len(quotes), len(want))
	}
	for i, w := range want {
		if quotes[i].Group != w {
			t.Errorf("quotes[%d].Group = %q, want %q", i, quotes[i].Group, w)
		}
	}
}

// TestTotalEstimatedCostUSDMicrosSumsEveryQuote pins the total the consent
// quote prints.
func TestTotalEstimatedCostUSDMicrosSumsEveryQuote(t *testing.T) {
	t.Parallel()

	quotes := []bridge.GroupQuote{
		{Group: bridge.AllIn, EstimatedCostUSDMicros: 6_000_000},
		{Group: "refunds", EstimatedCostUSDMicros: 5_500_000},
	}
	if got := bridge.TotalEstimatedCostUSDMicros(quotes); got != 11_500_000 {
		t.Errorf("total = %d, want 11500000", got)
	}
}

// TestQuoteGroupsRefusesAnEmptyTrainingFile pins the "a zero-example
// fine-tune is a paid no-op" refusal.
func TestQuoteGroupsRefusesAnEmptyTrainingFile(t *testing.T) {
	t.Parallel()

	plan := &bridge.GroupsPlan{AllIn: nil, LeaveOneOut: map[string][]string{}} // no assets at all
	price := pricing.TrainPrice{PerMTokUSDMicros: 1_000_000}

	_, err := bridge.QuoteGroups(quotePortfolio(), plan, quoteAssets(), "together:meta-llama/Llama-3-8b", price, 1)
	if err == nil {
		t.Fatal("want a refusal for an empty training file")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "zero examples") {
		t.Errorf("error does not explain why: %q", err.Error())
	}
}
