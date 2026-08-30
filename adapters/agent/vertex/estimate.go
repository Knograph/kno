package vertex

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// This file is the docs/debt.md#41(d) repayment on the estimate side. The
// regional +10% multiplier applies in ALL THREE places the guard touches —
// the per-Case reservation (Estimate), the consent quote (WorstCase), and the
// settlement (Settle, before Guard.Settle). A 10% shortfall in the reservation
// alone lets a capped run overshoot by a tenth of its spend, which is the
// exact bleed the ledger entry exists to prevent.
//
// The multiplier keys off the MODEL ID, never the region environment: a
// `us.`- or `eu.`-prefixed cross-region inference profile bills at the
// DESTINATION region's price, which is not 1.10x of the base row — so a
// profile id gets no multiplier AND no row, and the refusal names the region
// class. The day a row exists for a profile class, that row carries the
// destination-region price and RegionalMultiplierPct returns 100 for it.

// Estimate reports the most one Invoke of c could cost.
//
// Local arithmetic over the pricing table, with no network call of any kind.
// That is a contract rather than an optimization: this runs BEFORE the budget
// guard authorizes anything, so a request here would spend money outside the
// guard entirely.
func (a *Agent) Estimate(ctx context.Context, c *core.Case) (budget.Estimate, error) {
	if err := ctx.Err(); err != nil {
		return budget.Estimate{}, err
	}
	if c == nil {
		return budget.Estimate{}, fmt.Errorf("vertex: nil case")
	}
	return a.estimate(c)
}

// priceOf applies an explicit override when one was supplied, and the table
// otherwise. The regional multiplier applies AFTER the price is resolved, on
// both paths — the endpoint is regional either way, so a caller's own price is
// no more exempt from the add-on than the table's is.
func (a *Agent) priceOf(prompt pricing.Prompt) (budget.Estimate, error) {
	if a.opts.Price != nil {
		return pricing.EstimateWithPrice(a.opts.Price, a.opts.Model, prompt,
			a.opts.MaxOutputTokens)
	}
	return pricing.Estimate(Scheme, a.opts.Model, prompt, a.opts.MaxOutputTokens)
}

// estimate is Estimate without the context, so a settlement path that already
// holds no context of its own does not have to invent one.
func (a *Agent) estimate(c *core.Case) (budget.Estimate, error) {
	est, err := a.priceOf(a.prompt(c))
	if errors.Is(err, pricing.ErrUnpriced) {
		// Run-fatal, because it is a property of the MODEL and the model does
		// not change mid-run. Under a dollar cap core refuses every Case it
		// cannot price, so without this the run made a refusal per Case and
		// ended as "too many cases errored" — a verdict naming nothing about
		// pricing. See docs/debt.md#46.
		//
		// A cross-region inference profile id (us./eu. prefix) is unpriced by
		// design — no row claims the destination region's rate — and the
		// refusal names the class so the user does not chase a typo.
		return budget.Estimate{}, agenterr.AsRunFatal(pricingRefusal(a.opts.Model, err))
	}
	if err != nil {
		return budget.Estimate{}, err
	}
	est.CostUSDMicros = a.regional(est.CostUSDMicros)
	return est, nil
}

// regional applies the #41(d) multiplier to a cost.
func (a *Agent) regional(cost int64) int64 {
	return pricing.Regional(cost,
		pricing.RegionalMultiplierPct(Scheme, a.opts.Model))
}

// pricingRefusal enriches an unpriced refusal with the reason the model has no
// row, when the shape of the model id makes the reason knowable.
func pricingRefusal(model string, err error) error {
	if class := pricing.RegionClass(model); class != "" {
		return errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"the %s.-prefixed id names a cross-region inference profile, which "+
				"bills at the destination region's price; no row claims that rate, "+
				"so it is refused until one exists — pass explicit prices with "+
				"--price-input-per-mtok and --price-output-per-mtok if you know the "+
				"destination rate", class,
		)).
			Wrap(fmt.Errorf("vertex: %s: %w", model, err))
	}
	return err
}

// prompt reports every byte the provider will bill as input for this Case.
//
// Built from the same pieces compose sends — see the anthropic adapter's
// prompt for the full reasoning. :rawPredict bills the same four parts:
// system string, injected Asset, history, input.
func (a *Agent) prompt(c *core.Case) pricing.Prompt {
	system := a.opts.System
	var history strings.Builder

	for _, t := range c.GetHistory() {
		if t.GetRole() == knov1.Role_ROLE_SYSTEM {
			system = join(system, t.GetContent())
			continue
		}
		if history.Len() > 0 {
			history.WriteString("\n\n")
		}
		history.WriteString(t.GetContent())
	}

	return pricing.Prompt{
		System:  system,
		History: history.String(),
		Input:   c.GetInput(),
		Context: a.asset,
	}
}

// WorstCase reports the most any single Case could cost, before any Case is
// seen. Zero when the model is unpriced, which is core's sanctioned
// degradation.
func (a *Agent) WorstCase() budget.Estimate { return a.worst }

// computeWorstCase builds the answer once, at construction.
//
// Same construction as the anthropic adapter's, plus the regional multiplier:
// the consent quote is one of the three guard touch-points #41(d) names, and
// quoting a number 10% below the exposure it authorizes is the same bleed at
// planning time.
func (a *Agent) computeWorstCase() budget.Estimate {
	n := a.promptCeiling()

	est, err := a.priceOf(pricing.Prompt{
		System:  a.opts.System,
		Context: a.asset,
		Input:   strings.Repeat("x", int(n)),
	})
	if err != nil {
		return budget.Estimate{}
	}
	est.CostUSDMicros = a.regional(est.CostUSDMicros)
	return est
}
