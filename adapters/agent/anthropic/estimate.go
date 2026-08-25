package anthropic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Estimate reports the most one Invoke of c could cost.
//
// Local arithmetic over the pricing table, with no network call of any kind.
// That is a contract rather than an optimization: this runs BEFORE the budget
// guard authorizes anything, so a request here would spend money outside the
// guard entirely.
//
// Pessimistic on every term — input charged at the fresh rate rather than the
// cached one, output charged at the full ceiling rather than at a typical
// answer — because it bounds a reservation, and a bound that can be too low is
// not a bound.
func (a *Agent) Estimate(ctx context.Context, c *core.Case) (budget.Estimate, error) {
	if err := ctx.Err(); err != nil {
		return budget.Estimate{}, err
	}
	if c == nil {
		return budget.Estimate{}, fmt.Errorf("anthropic: nil case")
	}
	return a.estimate(c)
}

// priceOf applies an explicit override when one was supplied, and the table
// otherwise.
//
// The override wins where it exists but does NOT replace a row that disagrees
// silently — it is consulted only because the caller stated a price, and a
// caller who states one is asserting knowledge of their own contract that a
// published rate card does not have.
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
		// pricing, after taking the user's consent for a figure that was never
		// going to apply. See docs/debt.md#46.
		//
		// Marked HERE rather than in core, which cannot tell this apart from
		// an Estimator that refuses one Case and prices the rest — a
		// distinction core has a test for and must keep.
		return budget.Estimate{}, agenterr.AsRunFatal(err)
	}
	return est, err
}

// prompt reports every byte the provider will bill as input for this Case.
//
// Deliberately built from the same pieces compose sends — the system prompt
// plus the run's, every history turn, and the Case's input. Counting only
// Case.input is the path of least resistance and it under-reserves by the whole
// conversation, systematically, in the direction that walks a run past its cap.
//
// It does NOT reuse compose's return value, because compose can refuse a Case
// whose history is malformed and an estimate must not. A Case this adapter will
// refuse costs nothing to refuse, and returning an error here would instead
// route it through core's unpriceable path, which leaves the Case unrecorded
// and re-attempted on every resume forever.
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

		// Context is the injected Asset, and M2 injects nothing. Named rather
		// than omitted so the day valuation lands, the missing term is a
		// compile-visible field rather than an invisible under-reservation.
		Context: "",
	}
}

// WorstCase reports the most any single Case could cost, before any Case is
// seen.
//
// Planning needs a number and per-Case estimates need a Case. Without this the
// engine plans against BaselineOptions.EstCostPerCallUSDMicros — a scalar an
// Estimator does not use — and the consent prompt quotes a figure for a run
// whose real exposure is something else entirely.
//
// Zero when the model is unpriced, which is core's sanctioned degradation:
// planningCostPerCall falls back to the scalar rather than planning against a
// number this adapter cannot stand behind.
func (a *Agent) WorstCase() budget.Estimate { return a.worst }

// computeWorstCase builds the answer once, at construction.
//
// The prompt term is a placeholder of MaxPromptBytes bytes rather than a token
// count, because pricing.Prompt is byte-denominated and inverting its token
// approximation here would put a second copy of that arithmetic in the tree —
// the two would drift, and the one that drifted low would be the reservation.
//
// The placeholder is never sent anywhere; only its length is read.
func (a *Agent) computeWorstCase() budget.Estimate {
	n := a.opts.MaxPromptBytes
	if n <= 0 {
		n = defaultWorstCasePromptBytes
	}
	if n > maxWorstCasePromptBytes {
		// A fat-fingered ceiling must not turn a planning call into a
		// multi-gigabyte allocation. Clamped rather than refused: WorstCase has
		// no error return, and the clamp is far above any real context window.
		n = maxWorstCasePromptBytes
	}

	// Through priceOf, so an explicit override reaches the figure
	// checkCostIsKnowable reads. Without it, supplying a price for an unpriced
	// model still left WorstCase at zero and the run was refused for having no
	// computable cost — naming a flag the user had just passed.
	est, err := a.priceOf(
		pricing.Prompt{System: a.opts.System, Input: strings.Repeat("x", int(n))})
	if err != nil {
		return budget.Estimate{}
	}
	return est
}
