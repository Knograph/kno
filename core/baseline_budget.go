package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Budget planning and spend arithmetic: what a Case is estimated to cost,
// whether the run fits under its caps, what the human is asked to consent to,
// and how a settled budget.Spend is derived from a Response.
//
// The authorization and the settlement themselves are NOT here — Guard.Authorize,
// Reservation.Settle and Reservation.Release appear only in invokeOnce, in
// baseline_invoke.go. An earlier version of this comment claimed every path
// here could spend money, which sent a reviewer auditing prime directive 4 to
// the one file that cannot spend anything: everything below either plans a
// number or computes one from a Response already received.

// estimateTimeout bounds an Estimator call.
//
// Estimating is arithmetic over a local pricing table — the Estimator godoc
// says so — and this exists for the adapter that does not honor that. Generous
// enough that no honest implementation notices, short enough that a hung one
// costs a single Case rather than the run.
const estimateTimeout = 5 * time.Second

// estimate reports what one invocation of c may cost.
//
// An adapter implementing Estimator answers per Case; anything else falls back
// to the run-scoped scalar, which is what the fake and every M1 caller use.
//
// A Estimator that cannot price a Case is a refusal when a dollar cap is set,
// never a zero: the guard cannot refuse what it was not told about, and a zero
// estimate is precisely what made a cap unenforceable in M1.
func (o BaselineOptions) estimate(ctx context.Context, c *Case) (budget.Estimate, error) {
	e, ok := o.Agent.(Estimator)
	if !ok {
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil
	}

	// Bounded, because Estimate runs BEFORE Authorize on the worker's own
	// context. An adapter that hangs here would pin a worker for the life of
	// the run, and the godoc's "must be local" is a contract, not a guarantee.
	ctx, cancel := context.WithTimeout(ctx, estimateTimeout)
	defer cancel()

	est, err := e.Estimate(ctx, c)
	capped := o.Guard.Limits().MaxCostUSDMicros > 0

	switch {
	case err != nil && capped:
		return budget.Estimate{}, o.unpriceable(c, err)
	case err != nil:
		// No dollar cap: the call cap still applies, and refusing the run
		// because a price is unknown would be worse than running uncapped when
		// the user asked for uncapped.
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil

	// A zero cost is not a cheap Case, it is an absent answer — the natural
	// output of a pricing-table miss coded as a zero row. Accepting it puts the
	// cap back to being discovered at settlement, which is the M1 failure this
	// whole interface exists to close, reached through the interface.
	case est.CostUSDMicros <= 0 && capped:
		return budget.Estimate{}, o.unpriceable(c,
			errors.New("the estimate is zero, which a cost cap cannot be enforced against"))

	// One Invoke settles as exactly one call (enforced in invokeOnce, in
	// baseline_invoke.go — spendOf only shapes the number), so an Estimate
	// reserving more would reserve N and settle 1, and the call cap would drift
	// by (N-1) per Case. Measured at 18 real calls against a cap of 10.
	// Rejected rather than coerced: silently rewriting one out-of-contract
	// field hides the adapter bug a reviewer needs to see.
	case est.Calls != 1:
		return budget.Estimate{}, o.unpriceable(c,
			fmt.Errorf("the estimate reserves %d calls, but one Invoke settles as "+
				"exactly one call", est.Calls))
	}
	return est, nil
}

// errUnpriceable marks a Case the guard was never asked to authorize.
//
// A sentinel rather than an inspection of the message, so the sink can tell
// "refused before any call" from "failed after a paid call" — the two have
// opposite correct handling, and only one of them is resumable work.
var errUnpriceable = errors.New("core: case cannot be priced")

// unpriceable renders a refusal to authorize a Case.
//
// The fix names things that actually work. An explicit --cost-per-call-usd does
// NOT override an Estimator, so advertising it would send the user down a dead
// end — which the first version of this message did.
func (o BaselineOptions) unpriceable(c *Case, cause error) error {
	return errs.ErrInvalidInput.WithFix(
		"drop --max-cost-usd to run without a dollar cap, or use an agent that " +
			"can price this model").
		Wrap(fmt.Errorf("cannot price case %s, and a cost cap cannot be enforced "+
			"against an unknown cost: %w: %w", c.GetId(), errUnpriceable, cause))
}

// confirmRun asks the human about the whole run before any of it is authorized.
//
// Computed HERE rather than in the CLI, and after CompletedCases, because this
// is the only place that knows how many Cases are actually left. The CLI has
// DevCases but not the completed set, so a run killed at 9,988 of 10,000 would
// have prompted for the full amount to finish twelve Cases.
//
// Bounded by what the guard will permit. With --max-cost-usd 5.00 on a fresh
// run the honest maximum exposure is $5.00, not DevCases x per-Case; on a
// resume with $0.10 left it is $0.10, not $5.00. Showing a larger number than
// the guard will stop at is false in the direction that teaches people to
// dismiss the prompt.
func (o BaselineOptions) confirmRun(ctx context.Context, alreadyDone int) error {
	remaining := int64(o.DevCases - alreadyDone)
	perCall := o.planningCostPerCall()
	if remaining <= 0 || perCall <= 0 {
		return nil
	}

	total := budget.Estimate{
		Calls:         remaining,
		CostUSDMicros: saturatingMul(remaining, perCall),
	}
	// Never quote more than the guard will actually permit — which on a resume
	// is the headroom LEFT, not the cap the run was originally given.
	//
	// Limits() is the static cap and was wrong here. Baseline calls
	// Guard.Restore before this, so a run resumed with $0.10 of a $5.00 cap
	// left was quoted against $5.00: measured at $5.00 shown with $0.10
	// remaining, 50x. The CLI prints both numbers in one sentence, so the
	// contradiction reaches the user intact.
	//
	// checkFeasible, twenty lines below, already used Remaining() for exactly
	// this reason. The two sat in one file answering the same question two
	// ways, which is what splitting the file surfaced.
	if ceiling := o.Guard.Remaining().CostUSDMicros; ceiling > 0 && total.CostUSDMicros > ceiling {
		total.CostUSDMicros = ceiling
	}

	ok, err := o.Guard.PreConfirm(ctx, total)
	if err != nil {
		return err
	}
	if !ok {
		return errs.ErrBudgetExceeded.WithFix(
			"re-run with --yes to proceed, or lower --max-cost-usd").
			Wrap(fmt.Errorf("the run was not confirmed; nothing was spent"))
	}
	return nil
}

// planningCostPerCall is what one Case may cost, for planning rather than for
// authorizing.
//
// An Estimator prices each Case from the Case, so a scalar supplied by the
// caller is not what the guard will reserve. Both the feasibility check and the
// consent prompt have to plan against the number that will actually be
// reserved, or they answer a different question than the one being asked — and
// the consent prompt in particular then shows a human a figure that is wrong by
// whatever the two differ by.
func (o BaselineOptions) planningCostPerCall() int64 {
	if e, ok := o.Agent.(Estimator); ok {
		if w := e.WorstCase(); w.CostUSDMicros > 0 {
			return w.CostUSDMicros
		}
	}
	return o.EstCostPerCallUSDMicros
}

// saturatingMul multiplies without wrapping.
//
// An overflowed product goes negative, sails past the cap clamp and under the
// confirmation threshold, so PreConfirm silently returns true and the run falls
// back to the per-call prompt — a consent path skipped by arithmetic.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// feasibleHeadroomFraction bounds how much of a cost cap may be held as
// un-spendable reservation at once.
//
// A pessimistic reservation holds concurrency x estimate of the cap in flight.
// If that exceeds the cap, the guard denies before anything settles and the run
// stops having done almost nothing: measured at a 32k output ceiling against
// --max-cost-usd 1.00 at concurrency 8, the FOURTH Case was denied with $0.00
// spent.
//
// A quarter, so a run never holds more than that back. Chosen against the
// measured forfeiture — 4.7% of a $5 cap at concurrency 8, 19.5% at 32 — and
// written down as a decision rather than left as a constant to reverse-engineer.
const feasibleHeadroomFraction = 0.25

// checkFeasible reduces concurrency, or refuses, when the caps cannot admit the
// run as configured.
//
// It runs AFTER Guard.Restore, against the headroom a resumed run actually has
// rather than against the cap it was originally given. Placing it before would
// be the same defect this file already fixed for the confirmation prompt:
// computing against a number the run is not operating under.
//
// A refusal exits 2, not 1. An exhausted cap is a resumable stop with nothing
// wrong with the data, and reporting it as a broken build is what trains people
// to ignore exit 1.
func (o *BaselineOptions) checkFeasible(alreadyDone int) error {
	// A resume of a finished run has nothing left to authorize, so an exhausted
	// cap is not a problem to report — it is the expected state. Refusing here
	// turned an idempotent `--resume` in CI from a clean exit 0 into exit 2,
	// with a fix line telling the user to re-run and pay for all of it again.
	if o.DevCases > 0 && o.DevCases-alreadyDone <= 0 {
		return nil
	}

	limits := o.Guard.Limits()
	perCall := o.planningCostPerCall()
	if limits.MaxCostUSDMicros <= 0 || perCall <= 0 {
		return nil // no dollar cap, or nothing to compute against
	}

	remaining := o.Guard.Remaining().CostUSDMicros
	if remaining <= 0 {
		return errs.ErrBudgetExceeded.WithFix(
			"raise --max-cost-usd, or start a fresh run without --resume").
			Wrap(fmt.Errorf("the cost cap is already spent, so no Case can be authorized"))
	}

	// Refusing outright only makes sense when every Case costs the same, which
	// is true of the scalar and false of an Estimator: it prices each Case from
	// the Case, so the worst one exceeding the cap says nothing about whether
	// the cheap ones fit. Refusing there would reject a run where most Cases
	// are affordable, and the per-Case guard already stops the dear ones.
	if _, perCase := o.Agent.(Estimator); !perCase && perCall > remaining {
		return errs.ErrBudgetExceeded.WithFix(fmt.Sprintf(
			"raise --max-cost-usd above %s", formatUSDMicros(perCall))).
			Wrap(fmt.Errorf("one Case is estimated at %s and only %s remains, so "+
				"not a single Case can be authorized",
				formatUSDMicros(perCall), formatUSDMicros(remaining)))
	}

	// How many in-flight reservations the headroom admits.
	affordable := int(float64(remaining) * feasibleHeadroomFraction / float64(perCall))
	if affordable < 1 {
		affordable = 1
	}
	// Zero is not "no concurrency", it is the CLI's default and the executor
	// turns it into min(NumCPU, 8) — the exact figure in the measurement this
	// check exists for. Treating it as a bypass meant the guard still denied
	// its way to a halt on the path almost every user takes: measured 0 of 60
	// Cases scored, $0.00 spent, on a $1.00 cap.
	requested := o.Concurrency
	if requested <= 0 {
		requested = executor.DefaultConcurrency()
	}
	if requested <= affordable {
		return nil
	}

	o.Concurrency = affordable
	return nil
}

// formatUSDMicros renders micro-USD for a human.
func formatUSDMicros(micros int64) string {
	return fmt.Sprintf("$%.4f", float64(micros)/1_000_000)
}

func spendOf(r *knov1.Response) budget.Spend { return spendOfN(r, 1) }

// attemptsOf reports how many provider calls an outcome took, floored at one.
//
// A nil outcome means the work never produced one, which is still one call's
// worth of exposure at most.
func attemptsOf(o *caseOutcome) int64 {
	if o == nil || o.Attempts < 1 {
		return 1
	}
	return int64(o.Attempts)
}

// spendOfN is spendOf for a Case that took n provider calls.
func spendOfN(r *knov1.Response, n int64) budget.Spend {
	if n < 1 {
		n = 1
	}
	return budget.Spend{
		Calls:         n,
		CostUSDMicros: r.GetCostUsdMicros(),
		Tokens:        r.GetPromptTokens() + r.GetCompletionTokens(),
	}
}
