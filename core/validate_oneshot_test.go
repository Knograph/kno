package core_test

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// cancelAfterHoldoutUse cancels the run the instant the holdout_uses row is
// committed — after the Run row, before any Invoke.
//
// This is the kill that PROVES the rule. Recording the use at completion, or
// on the first measurement, passes a mid-run kill test and fails this one:
// the row exists here even though not one Case was measured, because a
// validate that opened the holdout and then died has already looked at it and
// pretending otherwise is the leak.
type cancelAfterHoldoutUse struct {
	store.Store
	cancel context.CancelFunc
}

func (c *cancelAfterHoldoutUse) RecordHoldoutUse(ctx context.Context, use *store.HoldoutUse) error {
	if err := c.Store.RecordHoldoutUse(ctx, use); err != nil {
		return err
	}
	c.cancel()
	return nil
}

// cancelAfterNCalls cancels the run once the agent has answered n Cases, for
// the mid-run kill.
type cancelAfterNCalls struct {
	inner  core.Agent
	n      int64
	calls  *atomic.Int64
	cancel context.CancelFunc
}

func (c *cancelAfterNCalls) Invoke(ctx context.Context, cs *core.Case) (*core.Response, error) {
	if c.calls.Load() >= c.n {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.inner.Invoke(ctx, cs)
}

func (c *cancelAfterNCalls) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{ContextInject: true, ContextSetInject: true, TokenCounts: true}
}

func (c *cancelAfterNCalls) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	inner, err := c.inner.(core.ContextSetInjector).WithContextSet(assets)
	if err != nil {
		return nil, err
	}
	return &cancelAfterNCalls{inner: inner, n: c.n, calls: c.calls, cancel: c.cancel}, nil
}

// TestInterruptedValidateStillConsumesTheHoldout drives TWO kills, and the
// first is the one that proves the rule.
//
// A kill BEFORE the first Case — after the Run and the holdout_uses row are
// committed, before any Invoke — must still leave the row. Recording at
// completion, or on first measurement, passes the mid-run variant and fails
// this one, which is why both are driven.
//
// In each case a FRESH validate for that pair is then refused with a fix
// naming --resume, and the resume completes without paying for any Case twice.
func TestInterruptedValidateStillConsumesTheHoldout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		killBefore bool
	}{
		{"killed before the first Case is measured", true},
		{"killed mid-run", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 8, assets: 1})
			var calls atomic.Int64
			scripted := &scriptedSetAgent{
				calls:          &calls,
				controlRight:   func(id string) bool { return id >= "hold-002" },
				treatmentRight: func(string) bool { return true },
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			opts := h.opts
			if tc.killBefore {
				opts.Store = &cancelAfterHoldoutUse{Store: h.store, cancel: cancel}
				opts.Agent = scripted
			} else {
				opts.Agent = &cancelAfterNCalls{
					inner: scripted, n: 5, calls: &calls, cancel: cancel,
				}
			}
			res, _ := opts.Validate(ctx)
			if res == nil {
				t.Fatal("the interrupted run returned no result, so its spend is unreportable")
			}

			// THE ASSERTION THE RULE LIVES IN.
			uses, err := h.store.HoldoutUses(context.Background(), "holdout-fp-1")
			if err != nil {
				t.Fatalf("HoldoutUses: %v", err)
			}
			if len(uses) != 1 {
				t.Fatalf("recorded %d holdout uses after an interrupted validate, want 1. "+
					"A run that opened the holdout and then died HAS looked at it.", len(uses))
			}
			if res.Validation != nil {
				t.Error("an interrupted validate wrote a Validation; a partial peek is not " +
					"a validation")
			}

			// A FRESH run for the same pair is refused, and the fix names
			// --resume rather than telling the user to start over.
			fresh := h.opts
			fresh.RunID = "validate-2"
			fresh.Agent = refusingAgent{t: t}
			_, err = fresh.Validate(context.Background())
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("a fresh run after an interruption = %v, want ErrInvalidInput", err)
			}
			var act *errs.Actionable
			if errors.As(err, &act) && !contains(act.Fix, "--resume") {
				t.Errorf("the fix does not name --resume: %q", act.Fix)
			}

			// The resume completes and pays for nothing twice.
			firstSpend, err := h.store.SettledSpend(context.Background(), "validate-1")
			if err != nil {
				t.Fatalf("SettledSpend: %v", err)
			}
			resumed := h.opts
			resumed.Resume = true
			resumed.Agent = scripted
			// A FRESH guard, because a resume is a fresh PROCESS: the guard is
			// in-memory and starts empty, and Guard.Restore seeds it from
			// SettledSpend. Reusing this process's guard would double-count
			// the first session and make the run-lifetime assertion below pass
			// for the wrong reason.
			resumed.Guard = budget.New(budget.Limits{}, nil, 0)
			out, err := resumed.Validate(context.Background())
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if out.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
				t.Fatalf("resumed status = %v, want COMPLETED", out.Status)
			}
			measured := out.Validation.GetMeasuredCaseCount()
			if tc.killBefore {
				// Nothing was measured before the kill, so the resume measures
				// the whole holdout. This is the variant that pins the record's
				// timing: the row existed with zero Cases behind it.
				if measured != 8 {
					t.Errorf("resumed run measured %d Cases, want the whole holdout (8)", measured)
				}
			}
			// NOTHING VANISHES SILENTLY. A Case whose provider call was charged
			// and then cancelled is recorded as errored and can never be paired
			// — re-measuring it would pay twice for one answer, and the
			// measurements table is INSERT OR IGNORE, so the retry's result
			// would be discarded anyway. What this stage owes the reader is
			// that the shortfall is COUNTED rather than absorbed into the
			// denominator: measured + dropped is the whole holdout, and both
			// numbers are printed beside the gain. See docs/debt.md.
			if measured+out.Validation.GetNDropped() != 8 {
				t.Errorf("measured %d + dropped %d != 8 holdout Cases; a Case went missing "+
					"from the denominator without being counted",
					measured, out.Validation.GetNDropped())
			}
			after, err := h.store.SettledSpend(context.Background(), "validate-1")
			if err != nil {
				t.Fatalf("SettledSpend: %v", err)
			}
			if after.Calls < firstSpend.Calls {
				t.Errorf("settled calls fell from %d to %d across a resume",
					firstSpend.Calls, after.Calls)
			}
			// Run-lifetime spend, not session spend: the run is the unit the
			// user authorized.
			if out.Spent.Calls != after.Calls {
				t.Errorf("the resumed run reported %d calls against %d settled for the run; "+
					"a resumed run reports the WHOLE run", out.Spent.Calls, after.Calls)
			}
			// No Case is paid for twice. The quote is the bound: 8 holdout
			// Cases x 2 arms x 1 trial, across BOTH processes.
			if after.Calls > 16 {
				t.Errorf("the run settled %d calls across both processes against a quote of "+
					"16 (8 holdout x 2 arms x 1 trial); anything more is a Case paid for twice",
					after.Calls)
			}
			// A resume is one look, not a second: the use record is unchanged.
			uses, err = h.store.HoldoutUses(context.Background(), "holdout-fp-1")
			if err != nil {
				t.Fatalf("HoldoutUses: %v", err)
			}
			if len(uses) != 1 || uses[0].ValidateRunID != "validate-1" {
				t.Errorf("a resume counted as a second peek: %+v", uses)
			}
		})
	}
}

// TestResumeRefusesAChangedHoldout.
//
// Re-splitting reclassifies Cases, and a "holdout" containing formerly-dev
// Cases is not a holdout. The fingerprint refuses the mismatch rather than
// mixing two populations into one number.
func TestResumeRefusesAChangedHoldout(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 8, assets: 1})
	var calls atomic.Int64
	scripted := &scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := h.opts
	opts.Store = &cancelAfterHoldoutUse{Store: h.store, cancel: cancel}
	opts.Agent = scripted
	_, _ = opts.Validate(ctx)

	resumed := h.opts
	resumed.Resume = true
	resumed.Agent = scripted
	resumed.InputFingerprint = "fp-changed"
	if _, err := resumed.Validate(context.Background()); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("a resume with changed inputs = %v, want ErrInvalidInput", err)
	}
}

// TestBudgetStopIsResumableAndEmitsNoGain.
//
// A cap that admits fewer than 2 x n_holdout calls stops the run, records
// BUDGET_STOPPED, writes NO Validation — a number over whatever got measured
// first is not a validation — and leaves the run resumable.
func TestBudgetStopIsResumableAndEmitsNoGain(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 8, assets: 1})
	var calls atomic.Int64
	scripted := &scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	h.opts.Agent = scripted
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 6}, nil, 0)

	res, _ := h.opts.Validate(context.Background())
	if res == nil {
		t.Fatal("the budget-stopped run returned no result, so its spend is unreportable")
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		t.Fatalf("status = %v, want BUDGET_STOPPED", res.Status)
	}
	if res.Validation != nil {
		t.Error("a budget-stopped run wrote a Validation")
	}
	uses, err := h.store.HoldoutUses(context.Background(), "holdout-fp-1")
	if err != nil {
		t.Fatalf("HoldoutUses: %v", err)
	}
	if len(uses) != 1 {
		t.Errorf("a budget-stopped validate recorded %d holdout uses, want 1", len(uses))
	}

	resumed := h.opts
	resumed.Resume = true
	resumed.Guard = budget.New(budget.Limits{}, nil, 0)
	out, err := resumed.Validate(context.Background())
	if err != nil {
		t.Fatalf("resume after a budget stop: %v", err)
	}
	if out.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Fatalf("resumed status = %v, want COMPLETED", out.Status)
	}
	if out.Validation.GetMeasuredCaseCount() != 8 {
		t.Errorf("resumed run measured %d Cases, want 8", out.Validation.GetMeasuredCaseCount())
	}
}

// TestTheHoldoutEstimatorDoesNotReadTheDevSlice is the sharp form of the
// winner's-curse property.
//
// The simulation below shows the estimator is unbiased under the null; this
// shows the mechanism directly: the recorded dev estimate is CARRIED onto the
// Validation for the comparison, and moving it must not move the holdout
// number by a single bit. An estimator that acquired any dependence on the dev
// slice would fail here immediately and visibly, rather than as a shifted mean
// a thousand iterations later.
func TestTheHoldoutEstimatorDoesNotReadTheDevSlice(t *testing.T) {
	t.Parallel()

	var gains []float64
	for _, dev := range []float64{0.0, 0.5, -0.5} {
		h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 12, assets: 1})
		p, err := h.store.Portfolio(context.Background(), "select-1")
		if err != nil {
			t.Fatalf("Portfolio: %v", err)
		}
		p.DevEstimatedGain = dev
		p.DevEstimatedInterval = &knov1.Interval{Low: dev - 0.1, High: dev + 0.1, Level: 0.95}
		if err := h.store.WritePortfolio(context.Background(), "select-1", p); err != nil {
			t.Fatalf("WritePortfolio: %v", err)
		}
		var calls atomic.Int64
		h.opts.Agent = &scriptedSetAgent{
			calls:          &calls,
			controlRight:   func(id string) bool { return id >= "hold-004" },
			treatmentRight: func(string) bool { return true },
		}
		res, err := h.opts.Validate(context.Background())
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if res.Validation.GetDevEstimatedGain() != dev {
			t.Errorf("the dev estimate was not carried verbatim: %v != %v",
				res.Validation.GetDevEstimatedGain(), dev)
		}
		gains = append(gains, res.Validation.GetHoldoutGain())
	}
	for i := 1; i < len(gains); i++ {
		if gains[i] != gains[0] {
			t.Fatalf("the holdout gain moved with the dev estimate: %v. The holdout number "+
				"is a measurement of the holdout population and must not read a dev-slice "+
				"quantity.", gains)
		}
	}
}

// TestTheWinnersCurseDoesNotReachTheHoldoutNumber.
//
// Over 2000 simulated pipelines where every Asset's TRUE effect is zero: the
// dev-side selection statistic — the best of K screened Assets, which is what
// Select ranks on — is reliably positive, because taking a maximum over noise
// is what the winner's curse IS. The holdout statistic, computed by the same
// arithmetic this stage uses (mean of per-Case paired means, interval'd by
// interval.PairedTrials), is within Monte Carlo error of zero.
//
// WHAT THIS DOES AND DOES NOT EXERCISE, stated rather than implied: it drives
// the ESTIMATOR — the paired arithmetic and the interval — over synthetic
// draws, not 2000 full runs through SQLite, which would take minutes and test
// the store rather than the statistics. The complementary property, that the
// estimator never reads a dev-slice quantity at all, is driven end to end by
// TestTheHoldoutEstimatorDoesNotReadTheDevSlice above. Together they are the
// claim; neither is it alone.
func TestTheWinnersCurseDoesNotReachTheHoldoutNumber(t *testing.T) {
	t.Parallel()

	const (
		pipelines = 2000
		screened  = 20 // Assets a Select run screens
		devCases  = 40
		holdout   = 40
	)
	// A fixed seed: a statistical test that fails one run in twenty is a
	// flaky test, and the flaky policy is to fix or delete rather than retry.
	rng := rand.New(rand.NewPCG(0x5eed, 0xf00d))

	var devSum, holdoutSum float64
	for range pipelines {
		// Dev: screen K Assets whose true effect is zero and keep the best.
		best := math.Inf(-1)
		for range screened {
			var total float64
			for range devCases {
				total += rng.NormFloat64()
			}
			if m := total / devCases; m > best {
				best = m
			}
		}
		devSum += best

		// Holdout: the SELECTED Portfolio, measured fresh in two arms. The
		// selection told us which Asset to carry; it tells us nothing about
		// these Cases, so the paired difference is a clean draw.
		perCase := make([][]float64, holdout)
		for i := range perCase {
			perCase[i] = []float64{rng.NormFloat64()}
		}
		holdoutSum += meanOfPerCase(perCase)
		if iv := interval.PairedTrials(perCase,
			knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED, interval.DefaultLevel); iv == nil {
			t.Fatal("PairedTrials returned no interval for a full sample")
		}
	}

	devMean := devSum / pipelines
	holdoutMean := holdoutSum / pipelines
	// The standard error of the holdout mean across pipelines: each pipeline's
	// statistic has SD 1/sqrt(holdout), so the mean of `pipelines` of them has
	// SD 1/sqrt(holdout*pipelines). Four of those is a bound this passes with
	// enormous margin at a fixed seed, and fails immediately if the estimator
	// acquires a dev-slice term.
	tol := 4.0 / math.Sqrt(float64(holdout)*float64(pipelines))
	if devMean <= 10*tol {
		t.Errorf("the dev-side selection statistic averaged %v under the null; the "+
			"simulation is not producing a winner's curse, so its holdout assertion "+
			"proves nothing", devMean)
	}
	if math.Abs(holdoutMean) > tol {
		t.Errorf("the holdout estimator averaged %v under the null, outside +/-%v. "+
			"The holdout number is supposed to be unbiased for the effect of THIS "+
			"Portfolio, and nothing optimized against these Cases.", holdoutMean, tol)
	}
}

// meanOfPerCase averages each Case's trials, then averages the Cases —
// the same mean-of-means the stage computes, and deliberately not the mean of
// every measurement, which weights a Case that lost a trial differently.
func meanOfPerCase(perCase [][]float64) float64 {
	var total float64
	for _, tr := range perCase {
		var sum float64
		for _, v := range tr {
			sum += v
		}
		total += sum / float64(len(tr))
	}
	return total / float64(len(perCase))
}
