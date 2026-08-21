package core_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// fixedNow makes Run records stable across test runs.
func fixedNow() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) }

type harness struct {
	store  *store.SQLite
	guard  *budget.Guard
	agent  *fake.Agent
	goal   *exactmatch.Goal
	evals  *core.SealedEvals
	cases  []*core.Case
	opts   core.BaselineOptions
	holdIn []string // ids deliberately marked holdout
}

// splitCases yields Cases, marking a fixed slice of them holdout.
type splitCases struct {
	cases []*core.Case
	err   error
	at    int
}

func (s *splitCases) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		for i, c := range s.cases {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if s.err != nil && i == s.at {
				yield(nil, s.err)
				return
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

func newHarness(t *testing.T, devCount, holdoutCount int, agentOpts fake.Options) *harness {
	t.Helper()

	st, err := store.NewSQLite(context.Background(), filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var cases []*core.Case
	var holdIn []string
	for i := range devCount {
		cases = append(cases, &core.Case{
			Id: fmt.Sprintf("dev-%03d", i), Input: "q", Expected: "a",
			Split: knov1.Split_SPLIT_DEV,
		})
	}
	for i := range holdoutCount {
		id := fmt.Sprintf("holdout-%03d", i)
		holdIn = append(holdIn, id)
		cases = append(cases, &core.Case{
			Id: id, Input: "q", Expected: "a", Split: knov1.Split_SPLIT_HOLDOUT,
		})
	}

	h := &harness{
		store:  st,
		guard:  budget.New(budget.Limits{}, nil, 0),
		agent:  fake.New(agentOpts),
		goal:   &exactmatch.Goal{},
		cases:  cases,
		holdIn: holdIn,
	}
	h.evals = core.Seal(&splitCases{cases: cases})
	h.opts = core.BaselineOptions{
		RunID:            "run-1",
		Agent:            h.agent,
		AgentRef:         &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:             h.goal,
		GoalName:         h.goal.Name(),
		Guard:            h.guard,
		Store:            st,
		Concurrency:      4,
		InputFingerprint: "fp-1",
		EvalContentHash:  "hash-1",
		DevCases:         devCount,
		HoldoutCases:     holdoutCount,
		Now:              fixedNow,
	}
	return h
}

// TestBaselineRunsEndToEnd is the milestone's headline: an agent measured over
// an eval set, scored, persisted, with a run record that adds up.
func TestBaselineRunsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 40, 10, fake.Options{})
	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	if got := res.Run.GetStatus(); got != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Errorf("status = %v, want COMPLETED", got)
	}
	if res.AggregateScore == nil {
		t.Fatal("no aggregate score for a run that scored 40 Cases")
	}
	if *res.AggregateScore != 1 {
		t.Errorf("aggregate = %v, want 1: the fake echoes the expected answer", *res.AggregateScore)
	}
	if got := res.Run.GetScoredCaseCount(); got != 40 {
		t.Errorf("scored = %d, want 40", got)
	}
	if got := res.Run.GetAttemptedCaseCount(); got != res.Run.GetScoredCaseCount()+res.Run.GetErroredCaseCount() {
		t.Errorf("attempted (%d) != scored (%d) + errored (%d)",
			got, res.Run.GetScoredCaseCount(), res.Run.GetErroredCaseCount())
	}
}

// TestBaselineNeverTouchesTheHoldout is the canary — the executable form of
// prime directive 5.
//
// The type makes a bypass a compile error; this proves the filtering holds at
// runtime, that the agent was never invoked for a holdout Case, and that no
// holdout outcome was persisted.
func TestBaselineNeverTouchesTheHoldout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 30, 20, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// The agent saw exactly the dev Cases and no more.
	if got := h.agent.Calls(); got != 30 {
		t.Errorf("the agent was invoked %d times for a 30-Case dev split; "+
			"holdout Cases reached the agent", got)
	}

	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	for _, id := range h.holdIn {
		if _, leaked := done[id]; leaked {
			t.Errorf("HOLDOUT LEAK: %s was executed and persisted by Baseline", id)
		}
	}
	if len(done) != 30 {
		t.Errorf("recorded %d outcomes, want 30", len(done))
	}
}

// TestErroredCasesAreExcludedFromTheScore pins the denominator.
//
// An agent that returned an error did not answer badly, it did not answer.
// Counting infrastructure failure as task failure biases the baseline downward
// and makes every later Asset look better than it is.
func TestErroredCasesAreExcludedFromTheScore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Every 5th call fails: 8 of 40.
	h := newHarness(t, 40, 5, fake.Options{FailEvery: 5})
	h.opts.MaxErrorRate = 0.5 // high enough that the run is still "clean"

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	scored, errored := res.Run.GetScoredCaseCount(), res.Run.GetErroredCaseCount()
	if errored == 0 {
		t.Fatal("no Cases errored; the fixture is not exercising the path")
	}
	if scored+errored != res.Run.GetAttemptedCaseCount() {
		t.Errorf("attempted (%d) != scored (%d) + errored (%d)",
			res.Run.GetAttemptedCaseCount(), scored, errored)
	}
	// The successful Cases all matched, so the mean over SCORED Cases is 1.
	// Had errors been counted as zeros it would be below 1.
	if res.AggregateScore == nil || *res.AggregateScore != 1 {
		t.Errorf("aggregate = %v, want 1: errored Cases were folded into the score",
			res.AggregateScore)
	}
}

// TestHighErrorRateMarksTheRunUnusable.
//
// A baseline computed over a run where most Cases never got an answer is a
// partial sample dressed as a reference. Later stages must be able to refuse it.
func TestHighErrorRateMarksTheRunUnusable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{FailEvery: 2}) // half fail
	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	if !res.Run.GetErrorRateExceeded() {
		t.Error("a run with half its Cases errored was not marked unusable")
	}
	if res.Run.GetIncompleteReason() == "" {
		t.Error("no reason recorded for an unusable run")
	}
}

// TestBudgetExhaustionStopsTheRunResumably.
//
// A budget stop means the run did what it was told and can continue. Reporting
// it as FAILED would make a deploy gate treat a spending limit as a broken
// build.
func TestBudgetExhaustionStopsTheRunResumably(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 500, 50, fake.Options{})
	// Only 10 calls are affordable.
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if got := res.Run.GetStatus(); got != knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		t.Errorf("status = %v, want BUDGET_STOPPED — a resumable stop, not a failure", got)
	}
	if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d", got, errs.ExitBudgetStopped)
	}
	// The run stopped rather than failing every remaining Case one at a time.
	if got := h.agent.Calls(); got > 100 {
		t.Errorf("the agent was invoked %d times against a 10-call cap; the run "+
			"kept dispatching after exhaustion", got)
	}
}

// TestResumeSkipsCompletedWorkAndDoesNotDoubleSpend is the property the whole
// checkpointing design exists for.
func TestResumeSkipsCompletedWorkAndDoesNotDoubleSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 60, 10, fake.Options{CostPerCallUSDMicros: 1_000})

	// First run: stop after 20 Cases by capping calls.
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 20}, nil, 0)
	first, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}
	firstDone, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	spentAfterFirst, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	callsAfterFirst := h.agent.Calls()
	t.Logf("first run: %d recorded, %d spent, status %v",
		len(firstDone), spentAfterFirst.CostUSDMicros, first.Run.GetStatus())

	// Resume with a fresh guard and a generous cap.
	resumed := newHarness(t, 0, 0, fake.Options{CostPerCallUSDMicros: 1_000})
	resumed.store = h.store
	resumed.agent = fake.New(fake.Options{CostPerCallUSDMicros: 1_000})
	opts := h.opts
	opts.Store = h.store
	opts.Agent = resumed.agent
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// Already-completed Cases were not re-executed.
	if got := int(resumed.agent.Calls()); got != 60-len(firstDone) {
		t.Errorf("the resumed run invoked the agent %d times; want %d (the %d "+
			"already-recorded Cases must not be paid for again)",
			got, 60-len(firstDone), len(firstDone))
	}

	// Total spend is one call per Case, not two.
	finalSpend, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if want := int64(60) * 1_000; finalSpend.CostUSDMicros != want {
		t.Errorf("total spend %d, want %d (60 Cases at 1000 each); the run paid twice "+
			"for %d Cases", finalSpend.CostUSDMicros, want,
			(finalSpend.CostUSDMicros-want)/1_000)
	}
	_ = callsAfterFirst
}

// TestResumeRestoresSpendBeforeAuthorizing.
//
// The guard is in-memory. A resumed run that started at zero could authorize
// its whole cap a second time.
func TestResumeRestoresSpendBeforeAuthorizing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 30, 5, fake.Options{CostPerCallUSDMicros: 10_000})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 10_000
	if _, err := core.Baseline(ctx, h.evals, h.opts); err == nil {
		t.Log("first run completed within budget")
	}

	spent, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spent.CostUSDMicros == 0 {
		t.Fatal("the first run spent nothing; the fixture is not exercising this")
	}

	// Resume with the SAME cap. The guard must know the money is already gone.
	opts := h.opts
	opts.Resume = true
	opts.Agent = fake.New(fake.Options{CostPerCallUSDMicros: 10_000})
	opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000}, nil, 0)
	opts.EstCostPerCallUSDMicros = 10_000

	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Logf("resumed run ended with: %v", err)
	}

	final, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if final.CostUSDMicros > 100_000 {
		t.Errorf("total spend %d exceeded the %d cap across a resume; the guard "+
			"forgot what the first run spent", final.CostUSDMicros, 100_000)
	}
}

// TestStaleFingerprintRefusesResume, naming which input changed.
func TestStaleFingerprintRefusesResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 10, 3, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.InputFingerprint = "fp-2"
	opts.EvalContentHash = "hash-2"

	_, err := core.Baseline(ctx, h.evals, opts)
	if !errors.Is(err, errs.ErrCheckpointStale) {
		t.Fatalf("error = %v, want ErrCheckpointStale", err)
	}
	if got := err.Error(); !contains(got, "eval source changed") {
		t.Errorf("error = %q, want it to name WHICH input changed", got)
	}
}

// TestFatalSourceErrorRecordsWhatWasPaidFor.
func TestFatalSourceErrorRecordsWhatWasPaidFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 50, 5, fake.Options{})
	boom := errors.New("the eval file is corrupt")
	h.evals = core.Seal(&splitCases{cases: h.cases, err: boom, at: 20})

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the source error", err)
	}
	if got := res.Run.GetStatus(); got != knov1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", got)
	}

	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) == 0 {
		t.Error("nothing was recorded; work already paid for was discarded")
	}
	t.Logf("recorded %d outcomes before the source failed", len(done))
}

// TestEventsAreEmittedAndSequenced covers the spine the TUI and API consume.
func TestEventsAreEmittedAndSequenced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 15, 3, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	maxSeq, err := h.store.MaxEventSequence(ctx, "run-1")
	if err != nil {
		t.Fatalf("MaxEventSequence: %v", err)
	}
	// RunStarted + 15 outcomes + RunFinished.
	if want := int64(17); maxSeq != want {
		t.Errorf("max sequence = %d, want %d (started + 15 cases + finished)", maxSeq, want)
	}
}

// TestBaselineValidatesItsInputs rather than failing partway through a run
// that has already spent money.
func TestBaselineValidatesItsInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := newHarness(t, 5, 2, fake.Options{})

	tests := []struct {
		name   string
		mutate func(*core.BaselineOptions)
	}{
		{"no run id", func(o *core.BaselineOptions) { o.RunID = "" }},
		{"no agent", func(o *core.BaselineOptions) { o.Agent = nil }},
		{"no goal", func(o *core.BaselineOptions) { o.Goal = nil }},
		{"no guard", func(o *core.BaselineOptions) { o.Guard = nil }},
		{"no store", func(o *core.BaselineOptions) { o.Store = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base.opts
			tc.mutate(&opts)
			if _, err := core.Baseline(ctx, base.evals, opts); err == nil {
				t.Error("invalid options were accepted")
			}
		})
	}

	if _, err := core.Baseline(ctx, nil, base.opts); err == nil {
		t.Error("a nil sealed source was accepted")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestCancelledRunIsInterruptedNotFailed.
//
// CI branches on this: an interrupted run is resumable, a failed one means
// something is broken. Reporting a Ctrl-C as FAILED would make a deploy gate
// treat an operator stopping a run as a broken build.
func TestCancelledRunIsInterruptedNotFailed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	h := newHarness(t, 2_000, 100, fake.Options{})

	// Cancel once some work has genuinely happened.
	go func() {
		for h.agent.Calls() < 10 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := res.Run.GetStatus(); got != knov1.RunStatus_RUN_STATUS_INTERRUPTED {
		t.Errorf("status = %v, want INTERRUPTED — resumable, not broken", got)
	}
	if got := errs.ExitCodeOf(err); got == errs.ExitValidationFailed {
		t.Errorf("exit code = %d; an interruption must not read as a validation failure", got)
	}

	// The run record survived the cancellation that ended it: a Run left in
	// RUNNING would look like a crash rather than an interruption.
	stored, err := h.store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.GetStatus() == knov1.RunStatus_RUN_STATUS_RUNNING {
		t.Error("the run was left RUNNING; recording how it ended did not survive cancellation")
	}
}

// TestRunThatScoredNothingHasNoAggregate.
//
// A run where every Case errored has no mean. Reporting zero would be
// indistinguishable from a real mean of zero — the same absent-versus-zero
// distinction Interval exists to preserve elsewhere.
func TestRunThatScoredNothingHasNoAggregate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 10, 3, fake.Options{FailEvery: 1}) // every call fails
	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	if res.AggregateScore != nil {
		t.Errorf("aggregate = %v, want absent: nothing scored, so there is no mean",
			*res.AggregateScore)
	}
	if got := res.Run.GetScoredCaseCount(); got != 0 {
		t.Errorf("scored = %d, want 0", got)
	}
	if got := res.Run.GetErroredCaseCount(); got != 10 {
		t.Errorf("errored = %d, want 10", got)
	}
	if !res.Run.GetErrorRateExceeded() {
		t.Error("a run where everything errored was not marked unusable")
	}
}

// TestRateLimitedCallsAreRecordedAsErrors covers the sentinel the fake can
// schedule, so the path exists before a paid adapter first exercises it.
func TestRateLimitedCallsAreRecordedAsErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{RateLimitEvery: 4})
	h.opts.MaxErrorRate = 0.9

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if h.agent.RateLimited() == 0 {
		t.Fatal("no calls were rate limited; the fixture is not exercising this")
	}
	if res.Run.GetErroredCaseCount() == 0 {
		t.Error("rate-limited Cases were not recorded as errors")
	}
	// A rate limit is not a scoring outcome, so the aggregate is over the rest.
	if res.AggregateScore == nil || *res.AggregateScore != 1 {
		t.Errorf("aggregate = %v, want 1 over the Cases that did answer", res.AggregateScore)
	}
}

// TestGoalWithoutDirectionIsRefused: without a direction the sign of every
// number the run produces is uninterpretable.
func TestGoalWithoutDirectionIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 5, 2, fake.Options{})
	h.opts.Goal = directionlessGoal{}

	if _, err := core.Baseline(context.Background(), h.evals, h.opts); err == nil {
		t.Error("a Goal with no direction was accepted")
	}
}

type directionlessGoal struct{}

func (directionlessGoal) Score(context.Context, *core.Case, *core.Response) (*core.Score, error) {
	return &knov1.Score{}, nil
}

func (directionlessGoal) Direction() core.Direction {
	return knov1.Direction_DIRECTION_UNSPECIFIED
}

// failingStore fails a chosen operation, so the error paths that persistence
// failures take are exercised rather than assumed.
type failingStore struct {
	store.Store
	failCreate bool
	failGet    bool
	failFinish bool
	failRecord bool
	failEvent  bool
	failDone   bool
	failSpend  bool
	failMaxSeq bool
}

var errStore = errors.New("store is unavailable")

func (f *failingStore) CreateRun(ctx context.Context, r *knov1.Run) error {
	if f.failCreate {
		return errStore
	}
	return f.Store.CreateRun(ctx, r)
}

func (f *failingStore) GetRun(ctx context.Context, id string) (*knov1.Run, error) {
	if f.failGet {
		return nil, errStore
	}
	return f.Store.GetRun(ctx, id)
}

func (f *failingStore) FinishRun(ctx context.Context, r *knov1.Run) error {
	if f.failFinish {
		return errStore
	}
	return f.Store.FinishRun(ctx, r)
}

func (f *failingStore) RecordOutcome(ctx context.Context, id string, o *store.Outcome) error {
	if f.failRecord {
		return errStore
	}
	return f.Store.RecordOutcome(ctx, id, o)
}

func (f *failingStore) AppendEvent(ctx context.Context, e *knov1.Event) error {
	if f.failEvent {
		return errStore
	}
	return f.Store.AppendEvent(ctx, e)
}

func (f *failingStore) CompletedCases(ctx context.Context, id string) (map[string]struct{}, error) {
	if f.failDone {
		return nil, errStore
	}
	return f.Store.CompletedCases(ctx, id)
}

func (f *failingStore) SettledSpend(ctx context.Context, id string) (budget.Spend, error) {
	if f.failSpend {
		return budget.Spend{}, errStore
	}
	return f.Store.SettledSpend(ctx, id)
}

func (f *failingStore) MaxEventSequence(ctx context.Context, id string) (int64, error) {
	if f.failMaxSeq {
		return 0, errStore
	}
	return f.Store.MaxEventSequence(ctx, id)
}

// TestStoreFailuresSurfaceRatherThanCorrupting.
//
// Every one of these paths ends a run. None may silently continue: a run that
// cannot record what it did is a run whose results do not exist, and
// continuing would spend money nothing can attribute.
func TestStoreFailuresSurfaceRatherThanCorrupting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name   string
		broken func(*failingStore)
		resume bool
	}{
		{"cannot create the run", func(f *failingStore) { f.failCreate = true }, false},
		{"cannot record an outcome", func(f *failingStore) { f.failRecord = true }, false},
		{"cannot append an event", func(f *failingStore) { f.failEvent = true }, false},
		{"cannot load completed cases", func(f *failingStore) { f.failDone = true }, true},
		{"cannot load prior spend", func(f *failingStore) { f.failSpend = true }, true},
		{"cannot read the event sequence", func(f *failingStore) { f.failMaxSeq = true }, true},
		{"cannot load the run", func(f *failingStore) { f.failGet = true }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, 10, 3, fake.Options{})
			if tc.resume {
				// Seed a completed run so the resume path has something to load.
				if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			}

			broken := &failingStore{Store: h.store}
			tc.broken(broken)

			opts := h.opts
			opts.Store = broken
			opts.Resume = tc.resume
			opts.Agent = fake.New(fake.Options{})

			if _, err := core.Baseline(ctx, h.evals, opts); err == nil {
				t.Error("a store failure was swallowed; the run reported success it " +
					"cannot substantiate")
			}
		})
	}
}

// TestFinishRunFailureIsReported: how a run ended must be recorded, and a
// failure to record it must not be silently dropped.
func TestFinishRunFailureIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 5, 2, fake.Options{})
	opts := h.opts
	opts.Store = &failingStore{Store: h.store, failFinish: true}

	res, err := core.Baseline(ctx, h.evals, opts)
	if err == nil {
		t.Error("failing to record how the run ended was swallowed")
	}
	// The result is still returned, so a caller can report what happened.
	if res == nil {
		t.Error("no result returned; the caller cannot report the run at all")
	}
}

// TestStaleFixNamesTheGoalWhenTheEvalsAreUnchanged covers the other branch of
// the staleness message: the fix must name what actually changed.
func TestStaleFixNamesTheGoalWhenTheEvalsAreUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 8, 2, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.InputFingerprint = "fp-changed"
	// The eval content is the SAME; something else moved.
	opts.EvalContentHash = h.opts.EvalContentHash

	_, err := core.Baseline(ctx, h.evals, opts)
	if !errors.Is(err, errs.ErrCheckpointStale) {
		t.Fatalf("error = %v, want ErrCheckpointStale", err)
	}
	if got := err.Error(); !contains(got, "goal, agent, or split") {
		t.Errorf("error = %q, want it to name the goal/agent/split rather than the evals", got)
	}
}

// TestDefaultNowIsUsedWhenUnset keeps the injected clock from being the only
// tested path.
func TestDefaultNowIsUsedWhenUnset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 3, 1, fake.Options{})
	opts := h.opts
	opts.Now = nil

	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if res.Run.GetCreatedAt() == "" {
		t.Error("no creation timestamp recorded")
	}
	if res.Run.GetFinishedAt() == "" {
		t.Error("no finish timestamp recorded")
	}
}

// TestResumedRunReportsTheWholeRunNotJustTheResumedPart.
//
// The aggregator starts empty in each process, so a resumed run's counts
// covered only the work IT did. A run that scored 24 Cases, was interrupted,
// and then scored 36 more recorded 36 — not 60.
//
// Those counts are the denominator behind every delta later measured against
// this baseline. Reporting the resumed portion as the whole run understates
// the sample, and every Asset measured against it would look better than it is.
func TestResumedRunReportsTheWholeRunNotJustTheResumedPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const total = 60
	h := newHarness(t, total, 10, fake.Options{})

	// Stop partway by capping calls.
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 20}, nil, 0)
	first, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}
	firstScored := first.Run.GetScoredCaseCount()
	if firstScored == 0 || firstScored >= total {
		t.Fatalf("first run scored %d of %d; the fixture is not exercising a partial run",
			firstScored, total)
	}

	// Resume to completion.
	opts := h.opts
	opts.Resume = true
	opts.Agent = fake.New(fake.Options{})
	opts.Guard = budget.New(budget.Limits{}, nil, 0)

	second, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// Every Case in the eval set has now been executed exactly once, across
	// two processes. The Run must say so.
	if got := second.Run.GetScoredCaseCount(); got != total {
		t.Errorf("the completed run reports %d scored Cases, want %d; it counted only "+
			"the resumed portion and lost the %d Cases the first run paid for",
			got, total, firstScored)
	}
	if got := second.Run.GetAttemptedCaseCount(); got != total {
		t.Errorf("attempted = %d, want %d", got, total)
	}

	// And the persisted outcomes agree with the reported counts.
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != int(second.Run.GetAttemptedCaseCount()) {
		t.Errorf("the store holds %d outcomes but the Run claims %d attempted",
			len(done), second.Run.GetAttemptedCaseCount())
	}
}
