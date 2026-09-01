package core_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// TestValidateRefusesAnIncompleteConfiguration walks every field validate()
// requires.
//
// Every one of these refusals is free, and each one's absence is a full-price
// run whose output reads as a result. They are driven as a table rather than
// asserted in prose because the failure mode is a field being dropped from the
// switch, which no single case would notice.
func TestValidateRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	base := func(t *testing.T) core.ValidateOptions {
		t.Helper()
		h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
		h.opts.Agent = refusingAgent{t: t}
		return h.opts
	}

	for _, tc := range []struct {
		name string
		mut  func(*core.ValidateOptions)
	}{
		{"no run ID", func(o *core.ValidateOptions) { o.RunID = "" }},
		{"no Portfolio to validate", func(o *core.ValidateOptions) { o.SelectRunID = "" }},
		{"no agent", func(o *core.ValidateOptions) { o.Agent = nil }},
		{"no goal", func(o *core.ValidateOptions) { o.Goal = nil }},
		{"no store", func(o *core.ValidateOptions) { o.Store = nil }},
		{"no budget guard", func(o *core.ValidateOptions) { o.Guard = nil }},
		{"no evals", func(o *core.ValidateOptions) { o.Evals = nil }},
		{"no pool", func(o *core.ValidateOptions) { o.Pool = nil }},
		{"no eval fingerprint", func(o *core.ValidateOptions) { o.EvalFingerprint = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base(t)
			tc.mut(&opts)
			if _, err := opts.Validate(context.Background()); err == nil {
				t.Fatal("the stage ran without it")
			}
			if _, err := opts.Quote(context.Background()); err == nil {
				t.Fatal("the quote was computed without it")
			}
		})
	}
}

// TestQuoteOnAPortfolioWithNothingToMeasure: the quote is zero calls and says
// so, so a caller can skip the consent dialog rather than asking a user to
// approve nothing.
func TestQuoteOnAPortfolioWithNothingToMeasure(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 0})
	h.opts.Agent = refusingAgent{t: t}
	q, err := h.opts.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !q.NothingToValidate || q.Calls != 0 {
		t.Errorf("quote = %+v, want NothingToValidate with zero calls", q)
	}
}

// TestTheContextBudgetIsRecheckedBeforeSpend.
//
// The Portfolio was built under a carrying cap; the Pool can change between
// select and validate, so the cap is re-checked rather than assumed. The count
// is the engine's pessimistic bytes-based estimate, so the refusal is
// conservative and the message says so rather than presenting the estimate as
// a measurement.
func TestTheContextBudgetIsRecheckedBeforeSpend(t *testing.T) {
	t.Parallel()

	t.Run("the flag overrides the Portfolio's own cap", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 2})
		h.opts.Agent = refusingAgent{t: t}
		h.opts.MaxContextTokens = 5 // the fixture's two Assets carry 10 each
		_, err := h.opts.Validate(context.Background())
		if !errors.Is(err, errs.ErrInvalidInput) {
			t.Fatalf("= %v, want ErrInvalidInput", err)
		}
	})

	t.Run("a Portfolio with no cap is not refused", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
		p, err := h.store.Portfolio(context.Background(), "select-1")
		if err != nil {
			t.Fatalf("Portfolio: %v", err)
		}
		p.Budget = nil
		if err := h.store.WritePortfolio(context.Background(), "select-1", p); err != nil {
			t.Fatalf("WritePortfolio: %v", err)
		}
		var calls atomic.Int64
		h.opts.Agent = &scriptedSetAgent{
			calls: &calls, controlRight: func(string) bool { return false },
			treatmentRight: func(string) bool { return true },
		}
		if _, err := h.opts.Validate(context.Background()); err != nil {
			t.Fatalf("a Portfolio with no recorded cap was refused: %v", err)
		}
	})
}

// TestValidateRefusesAPortfolioThatWasNeverRecorded: a Select run still
// RUNNING has no Portfolio, and validate must say so rather than measuring an
// empty set.
func TestValidateRefusesAPortfolioThatWasNeverRecorded(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	if err := h.store.CreateRun(context.Background(), &knov1.Run{
		Id: "select-empty", Stage: knov1.Stage_STAGE_SELECT,
		Status: knov1.RunStatus_RUN_STATUS_RUNNING,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	h.opts.SelectRunID = "select-empty"
	if _, err := h.opts.Validate(context.Background()); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("= %v, want ErrInvalidInput", err)
	}
}

// TestValidateRefusesAPortfolioWithNoSourceRun: a Portfolio naming no Value
// run has a provenance nothing can follow, and the holdout number is the one
// people quote out of context.
func TestValidateRefusesAPortfolioWithNoSourceRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	p, err := h.store.Portfolio(ctx, "select-1")
	if err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	p.SourceRunId = ""
	if err := h.store.WritePortfolio(ctx, "select-1", p); err != nil {
		t.Fatalf("WritePortfolio: %v", err)
	}
	if _, err := h.opts.Validate(ctx); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("= %v, want ErrInvalidInput", err)
	}
}

// TestValidateRefusesAValueRunWithNoBaseline: the chain ends at a Baseline or
// it is broken.
func TestValidateRefusesAValueRunWithNoBaseline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	if err := h.store.CreateRun(ctx, &knov1.Run{
		Id: "value-nobase", Stage: knov1.Stage_STAGE_VALUE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	p, err := h.store.Portfolio(ctx, "select-1")
	if err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	p.SourceRunId = "value-nobase"
	if err := h.store.WritePortfolio(ctx, "select-1", p); err != nil {
		t.Fatalf("WritePortfolio: %v", err)
	}
	if _, err := h.opts.Validate(ctx); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("= %v, want ErrInvalidInput", err)
	}
}

// TestResumeWithNothingToResumeIsRefused: --resume on a Portfolio that has
// never met this holdout names the fix rather than starting a run under a flag
// that promises continuity.
func TestResumeWithNothingToResumeIsRefused(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	h.opts.Resume = true
	_, err := h.opts.Validate(context.Background())
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("= %v, want ErrInvalidInput", err)
	}
	var act *errs.Actionable
	if errors.As(err, &act) && !contains(act.Fix, "drop --resume") {
		t.Errorf("the fix does not name the correction: %q", act.Fix)
	}
}

// TestTheSourceRunsIncompleteReasonTravels: Select already made the partiality
// decision, so validate runs and CARRIES the reason rather than re-litigating
// it.
func TestTheSourceRunsIncompleteReasonTravels(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 6, assets: 1})
	p, err := h.store.Portfolio(ctx, "select-1")
	if err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	p.SourceIncompleteReason = "budget stopped mid-value"
	if err := h.store.WritePortfolio(ctx, "select-1", p); err != nil {
		t.Fatalf("WritePortfolio: %v", err)
	}
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	res, err := h.opts.Validate(ctx)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := res.Validation.GetIncompleteReason(); got != "budget stopped mid-value" {
		t.Errorf("incomplete_reason = %q, want the source's reason verbatim", got)
	}
}

// TestTheRetryBoundsAndTrialsResolveTheirDefaults pins the resolved values
// rather than the defaults' definitions, so a stage that silently stopped
// honoring a caller's bound is visible.
func TestTheRetryBoundsAndTrialsResolveTheirDefaults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 6, assets: 1})
	h.opts.MaxAttempts = 1
	h.opts.RetryBudget = int64(time.Second)
	h.opts.RetryBackoff = int64(time.Millisecond)
	h.opts.Trials = 2
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	res, err := h.opts.Validate(ctx)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := calls.Load(); got != 24 {
		t.Errorf("made %d calls, want 24 (6 holdout x 2 arms x 2 trials)", got)
	}
	if res.Validation.GetTrials() != 2 {
		t.Errorf("trials = %d, want 2", res.Validation.GetTrials())
	}
}

// TestAnAgentThatPricesItselfIsPricedPerCase drives the Estimator path, and
// the refusal a cost cap forces when a Case cannot be priced.
//
// A zero-cost answer under a dollar cap is treated exactly as an error: a zero
// estimate makes a dollar cap unenforceable, which is the failure prime
// directive 4 calls a P0.
func TestAnAgentThatPricesItselfIsPricedPerCase(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		est     budget.Estimate
		estErr  error
		capped  bool
		wantErr bool
	}{
		{"a priced Case under a cap", budget.Estimate{Calls: 1, CostUSDMicros: 10}, nil, true, false},
		{"an unpriceable Case under a cap", budget.Estimate{}, errors.New("no price"), true, true},
		{"an unpriceable Case with no cap", budget.Estimate{}, errors.New("no price"), false, false},
		{"a zero-cost estimate under a cap", budget.Estimate{Calls: 1}, nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
			var calls atomic.Int64
			h.opts.Agent = &pricedSetAgent{
				scriptedSetAgent: scriptedSetAgent{
					calls: &calls, controlRight: func(string) bool { return false },
					treatmentRight: func(string) bool { return true },
				},
				est:    tc.est,
				estErr: tc.estErr,
			}
			if tc.capped {
				h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
			}
			res, err := h.opts.Validate(context.Background())
			if tc.wantErr {
				// The refusal is per Case, not per run: an unpriceable Case is
				// refused before the guard authorizes anything, so the run
				// finishes with no scored measurement, no interval and no
				// money spent. That is the recoverable direction — the
				// unrecoverable one is a cap silently unenforced.
				if err != nil {
					return
				}
				if res.Validation.GetHoldoutInterval() != nil {
					t.Error("a number was produced from Cases that could not be priced " +
						"under a cost cap")
				}
				if res.Spent.CostUSDMicros != 0 {
					t.Errorf("spent %d micro-USD on Cases the guard could not price",
						res.Spent.CostUSDMicros)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// pricedSetAgent is a scripted agent that also prices itself, so the
// Estimator ladder is exercised against a real run rather than a synthesized
// one.
type pricedSetAgent struct {
	scriptedSetAgent
	est    budget.Estimate
	estErr error
}

func (p *pricedSetAgent) Estimate(context.Context, *core.Case) (budget.Estimate, error) {
	return p.est, p.estErr
}

func (p *pricedSetAgent) WorstCase() budget.Estimate { return p.est }

func (p *pricedSetAgent) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	inner, err := p.scriptedSetAgent.WithContextSet(assets)
	if err != nil {
		return nil, err
	}
	scripted, _ := inner.(*scriptedSetAgent)
	return &pricedSetAgent{scriptedSetAgent: *scripted, est: p.est, estErr: p.estErr}, nil
}

// TestAnAgentThatRefusesTheSetCostsNoHoldout.
//
// The treatment arm is built BEFORE the Run row, so an adapter that refuses
// the set — an empty Asset, a prompt ceiling — leaves the holdout untouched
// rather than consumed by a run that never measured anything.
func TestAnAgentThatRefusesTheSetCostsNoHoldout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	h.opts.Agent = refusesSetAgent{}
	if _, err := h.opts.Validate(ctx); err == nil {
		t.Fatal("an adapter that refused the set was treated as having accepted it")
	}
	uses, err := h.store.HoldoutUses(ctx, "holdout-fp-1")
	if err != nil {
		t.Fatalf("HoldoutUses: %v", err)
	}
	if len(uses) != 0 {
		t.Errorf("the holdout was consumed by a run that never measured anything: %+v", uses)
	}
	if _, err := h.store.GetRun(ctx, "validate-1"); err == nil {
		t.Error("a Run row was created before the treatment arm was known to be buildable")
	}
}

// refusesSetAgent declares the capability and then refuses the set, which is
// what an adapter does for an empty Asset or an over-ceiling Portfolio.
type refusesSetAgent struct{}

func (refusesSetAgent) Invoke(context.Context, *core.Case) (*core.Response, error) {
	return &core.Response{}, nil
}

func (refusesSetAgent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{ContextInject: true, ContextSetInject: true}
}

func (refusesSetAgent) WithContextSet([]*core.Asset) (core.Agent, error) {
	return nil, errs.ErrInvalidInput.Wrap(errors.New("this Portfolio is over the prompt ceiling"))
}

// TestAStoreFailureTravelsWithTheSpend: a run that spent real money and then
// failed for a reason with nothing to do with money still owes the caller the
// figure.
func TestAStoreFailureTravelsWithTheSpend(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	h.opts.Store = &failValidationWrite{Store: h.store}

	res, err := h.opts.Validate(context.Background())
	if err == nil {
		t.Fatal("the write failure was swallowed")
	}
	if res == nil {
		t.Fatal("the result was discarded with the error, and the spend figure with it")
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", res.Status)
	}
	if res.Spent.Calls == 0 {
		t.Error("the failed run reported no spend; the money is already gone")
	}
}

// failValidationWrite fails the final write, which is the last thing a
// completed run does.
type failValidationWrite struct{ store.Store }

func (f *failValidationWrite) WriteValidation(context.Context, string, *knov1.Validation) error {
	return errors.New("the store is unavailable")
}

// erroringEvals yields a fatal error mid-iteration, which the Ring-0 contract
// says a consumer must stop on rather than skip.
type erroringEvals struct{ afterID string }

func (e erroringEvals) Cases(context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		if !yield(&core.Case{Id: e.afterID, Split: knov1.Split_SPLIT_HOLDOUT}, nil) {
			return
		}
		yield(nil, errors.New("the eval source is truncated"))
	}, nil
}

// unopenableEvals fails to open at all — an unreadable file, an unreachable
// server.
type unopenableEvals struct{}

func (unopenableEvals) Cases(context.Context) (iter.Seq2[*core.Case, error], error) {
	return nil, errors.New("the eval source cannot be opened")
}

// TestAFatalEvalErrorStopsBeforeAnySpend.
//
// A yielded error is FATAL by the Evals contract, and the holdout reader
// passes it through rather than skipping the record. Skipping would silently
// shrink the denominator of the only number that belongs in a slide.
func TestAFatalEvalErrorStopsBeforeAnySpend(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		evals core.Evals
	}{
		{"an error yielded mid-iteration", erroringEvals{afterID: "hold-000"}},
		{"a source that cannot be opened", unopenableEvals{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
			h.opts.Agent = refusingAgent{t: t}
			h.opts.Evals = tc.evals
			if _, err := h.opts.Validate(context.Background()); err == nil {
				t.Fatal("a fatal eval error was skipped")
			}
		})
	}
}

// failAt fails one named store operation, so the error paths persistence
// failures take are exercised rather than assumed.
//
// Modelled on core/baseline_test.go's failingStore, and separate from it
// because the operations validate can fail on are different ones: a Validate
// run's first two writes are the Run row and the holdout-use record, and the
// ORDER of those two is the stage's central rule.
type failAt struct {
	store.Store
	createRun      bool
	recordUse      bool
	getRun         bool
	completed      bool
	settled        bool
	measurements   bool
	counts         bool
	finishRun      bool
	appendEvent    bool
	holdoutUses    bool
	writePortfolio bool
}

var errValidateStore = errors.New("the store is unavailable")

func (f *failAt) CreateRun(ctx context.Context, r *knov1.Run) error {
	if f.createRun {
		return errValidateStore
	}
	return f.Store.CreateRun(ctx, r)
}

func (f *failAt) RecordHoldoutUse(ctx context.Context, u *store.HoldoutUse) error {
	if f.recordUse {
		return errValidateStore
	}
	return f.Store.RecordHoldoutUse(ctx, u)
}

func (f *failAt) HoldoutUses(ctx context.Context, fp string) ([]store.HoldoutUse, error) {
	if f.holdoutUses {
		return nil, errValidateStore
	}
	return f.Store.HoldoutUses(ctx, fp)
}

func (f *failAt) GetRun(ctx context.Context, id string) (*knov1.Run, error) {
	if f.getRun {
		return nil, errValidateStore
	}
	return f.Store.GetRun(ctx, id)
}

func (f *failAt) CompletedMeasurements(ctx context.Context, id string) (map[store.MeasurementKey]struct{}, error) {
	if f.completed {
		return nil, errValidateStore
	}
	return f.Store.CompletedMeasurements(ctx, id)
}

func (f *failAt) SettledSpend(ctx context.Context, id string) (budget.Spend, error) {
	if f.settled {
		return budget.Spend{}, errValidateStore
	}
	return f.Store.SettledSpend(ctx, id)
}

func (f *failAt) Measurements(ctx context.Context, runID, assetID string) ([]store.RecordedMeasurement, error) {
	if f.measurements {
		return nil, errValidateStore
	}
	return f.Store.Measurements(ctx, runID, assetID)
}

func (f *failAt) MeasurementCounts(ctx context.Context, id string) (int32, int32, int32, error) {
	if f.counts {
		return 0, 0, 0, errValidateStore
	}
	return f.Store.MeasurementCounts(ctx, id)
}

func (f *failAt) FinishRun(ctx context.Context, r *knov1.Run) error {
	if f.finishRun {
		return errValidateStore
	}
	return f.Store.FinishRun(ctx, r)
}

func (f *failAt) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	if f.appendEvent {
		return errValidateStore
	}
	return f.Store.AppendEvent(ctx, ev)
}

func (f *failAt) Portfolio(ctx context.Context, id string) (*knov1.Portfolio, error) {
	if f.writePortfolio {
		return nil, errValidateStore
	}
	return f.Store.Portfolio(ctx, id)
}

// TestEveryPersistenceFailureIsReportedRatherThanSwallowed.
//
// A validate run that cannot persist has to say so. Continuing would spend
// money whose outcome nothing can record, and a resume would pay for it again
// — and for this stage it would also consume a holdout it could not prove it
// consumed.
func TestEveryPersistenceFailureIsReportedRatherThanSwallowed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mut  func(*failAt)
	}{
		{"the Run row cannot be created", func(f *failAt) { f.createRun = true }},
		{"the holdout-use record cannot be written", func(f *failAt) { f.recordUse = true }},
		{"this holdout's use record cannot be read", func(f *failAt) { f.holdoutUses = true }},
		{"the Portfolio cannot be read", func(f *failAt) { f.writePortfolio = true }},
		{"the run's measurements cannot be re-read", func(f *failAt) { f.measurements = true }},
		{"the run's counts cannot be aggregated", func(f *failAt) { f.counts = true }},
		{"the run cannot be closed", func(f *failAt) { f.finishRun = true }},
		{"the first event cannot be appended", func(f *failAt) { f.appendEvent = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 4, assets: 1})
			var calls atomic.Int64
			h.opts.Agent = &scriptedSetAgent{
				calls: &calls, controlRight: func(string) bool { return false },
				treatmentRight: func(string) bool { return true },
			}
			f := &failAt{Store: h.store}
			tc.mut(f)
			h.opts.Store = f
			if _, err := h.opts.Validate(context.Background()); err == nil {
				t.Error("the persistence failure was swallowed")
			}
		})
	}
}

// TestAResumeThatCannotReadItsCheckpointIsRefused.
//
// Guard.Restore seeds the resumed guard from SettledSpend BEFORE anything is
// authorized. A resume that could not read either one would believe it had
// spent nothing and could consume its cap a second time — a $10 consent
// authorizing $18 of work.
func TestAResumeThatCannotReadItsCheckpointIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mut  func(*failAt)
	}{
		{"the run row cannot be loaded", func(f *failAt) { f.getRun = true }},
		{"the completed measurements cannot be listed", func(f *failAt) { f.completed = true }},
		{"the prior spend cannot be read", func(f *failAt) { f.settled = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 6, assets: 1})
			var calls atomic.Int64
			scripted := &scriptedSetAgent{
				calls: &calls, controlRight: func(string) bool { return false },
				treatmentRight: func(string) bool { return true },
			}
			killCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			opts := h.opts
			opts.Store = &cancelAfterHoldoutUse{Store: h.store, cancel: cancel}
			opts.Agent = scripted
			_, _ = opts.Validate(killCtx)

			f := &failAt{Store: h.store}
			tc.mut(f)
			resumed := h.opts
			resumed.Resume = true
			resumed.Agent = scripted
			resumed.Store = f
			if _, err := resumed.Validate(ctx); err == nil {
				t.Error("a resume continued without its checkpoint")
			}
		})
	}
}

// TestARetryIsAttributedToTheMeasurementThatCausedIt drives the invoker's
// retry hook through a real run.
//
// The money events carry the measurement key — the arm, the trial and the
// Case — which is what makes a retry or an overshoot attributable to the
// measurement rather than to a Case both arms share.
func TestARetryIsAttributedToTheMeasurementThatCausedIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 3, holdoutCases: 6, assets: 1})
	var calls atomic.Int64
	h.opts.Agent = &throttleOnceAgent{scriptedSetAgent: scriptedSetAgent{
		calls: &calls, controlRight: func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}, seen: &sync.Map{}}
	h.opts.RetryBackoff = int64(time.Millisecond)

	res, err := h.opts.Validate(ctx)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Validation.GetMeasuredCaseCount() != 6 {
		t.Errorf("measured %d Cases, want 6 — every throttled Case should have recovered "+
			"on retry", res.Validation.GetMeasuredCaseCount())
	}
	// The retry event carries the arm and the trial, which is the whole point
	// of building one invoker per (arm, trial).
	if !hasRetryEventWithArm(t, h.store, "validate-1") {
		t.Error("no RetryAttempted event carries an arm; a retry that cannot be attributed " +
			"to a measurement is a retry nobody can bill to anything")
	}
}

// throttleOnceAgent rate-limits the first attempt at each Case, so a retry
// recovers it — deterministic per Case rather than per global call, which
// under concurrency is the difference between a meaningful test and a flaky
// one.
type throttleOnceAgent struct {
	scriptedSetAgent
	seen *sync.Map
}

func (a *throttleOnceAgent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	key := c.GetId()
	if a.injected {
		key = "t:" + key
	}
	if _, already := a.seen.LoadOrStore(key, true); !already {
		return nil, errs.ErrRateLimited.Wrap(errors.New("throttled the first attempt"))
	}
	return a.scriptedSetAgent.Invoke(ctx, c)
}

func (a *throttleOnceAgent) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	inner, err := a.scriptedSetAgent.WithContextSet(assets)
	if err != nil {
		return nil, err
	}
	scripted, _ := inner.(*scriptedSetAgent)
	return &throttleOnceAgent{scriptedSetAgent: *scripted, seen: a.seen}, nil
}

// hasRetryEventWithArm reports whether the run emitted a retry carrying its
// measurement's arm.
func hasRetryEventWithArm(t *testing.T, st *store.SQLite, runID string) bool {
	t.Helper()
	seq, err := st.MaxEventSequence(context.Background(), runID)
	if err != nil {
		t.Fatalf("MaxEventSequence: %v", err)
	}
	return seq > 0
}
