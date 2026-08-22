package core_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	opts.EstCostPerCallUSDMicros = 1_000

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
	// Asserting "not 3" was too weak: it passed while an interruption exited 1,
	// indistinguishable from a broken build. An interrupted run has its own code.
	if got := errs.ExitCodeOf(err); got != errs.ExitInterrupted {
		t.Errorf("exit code = %d, want %d; exiting %d would report a Ctrl-C the same "+
			"way as a broken build", got, errs.ExitInterrupted, errs.ExitError)
	}
	if !errors.Is(err, errs.ErrInterrupted) {
		t.Errorf("a cancelled run does not carry ErrInterrupted, so nothing "+
			"downstream can tell it apart from a failure: %v", err)
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

// TestRateLimitsAreRetriedNotRecordedAsFailures.
//
// A 429 is the provider asking us to slow down, not the Case failing. An
// earlier version recorded it as a terminal error, which threw away capacity
// already paid for — and worse, ordinary throttling would push a run past
// max_error_rate and mark a perfectly good baseline unusable for no
// statistical reason.
func TestRateLimitsAreRetriedNotRecordedAsFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Every Case is throttled once, so every Case needs a retry and every
	// retry succeeds. Deterministic under concurrency, unlike a global
	// every-Nth-call counter, whose retries can land on the same multiple.
	h := newHarness(t, 20, 5, fake.Options{ThrottleFirstAttempt: true})
	h.opts.RetryBackoff = time.Millisecond // keep the test fast

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if h.agent.RateLimited() == 0 {
		t.Fatal("no calls were throttled; the fixture is not exercising retry")
	}

	if got := res.Run.GetErroredCaseCount(); got != 0 {
		t.Errorf("%d Cases recorded as errors; a throttled call must be retried, "+
			"not counted as a failure", got)
	}
	if got := res.Run.GetScoredCaseCount(); got != 20 {
		t.Errorf("scored = %d, want 20: retries did not recover the throttled Cases", got)
	}
	if res.Run.GetErrorRateExceeded() {
		t.Error("ordinary throttling marked the baseline unusable")
	}

	// Retries are real provider calls and must consume call budget: 20 Cases,
	// each throttled once, is 40 calls.
	if h.agent.Calls() != 40 {
		t.Errorf("the agent was called %d times for 20 Cases; retries were not "+
			"counted as calls", h.agent.Calls())
	}
}

// TestPersistentRateLimitEventuallyGivesUp: retry is bounded, or a wedged
// provider would hang a run forever.
func TestPersistentRateLimitEventuallyGivesUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 5, 2, fake.Options{RateLimitEvery: 1}) // always throttled
	h.opts.RetryBackoff = time.Millisecond
	h.opts.MaxAttempts = 2
	h.opts.MaxErrorRate = 1

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := res.Run.GetErroredCaseCount(); got != 5 {
		t.Errorf("errored = %d, want 5: a permanently throttled Case must eventually "+
			"be recorded rather than retried forever", got)
	}
	// Two attempts per Case, not more.
	if got := h.agent.Calls(); got != 10 {
		t.Errorf("the agent was called %d times, want 10 (5 Cases x 2 attempts)", got)
	}
}

// TestAgentErrorsAreNotRetried: retrying a 500 or malformed output burns
// budget on a Case that will fail again.
func TestAgentErrorsAreNotRetried(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 6, 2, fake.Options{FailEvery: 1}) // every call fails
	h.opts.RetryBackoff = time.Millisecond
	h.opts.MaxAttempts = 3
	h.opts.MaxErrorRate = 1

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := h.agent.Calls(); got != 6 {
		t.Errorf("the agent was called %d times for 6 Cases; a non-throttle error "+
			"was retried, burning budget on Cases that fail again", got)
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

// TestCostCapWithoutAnEstimateIsRefused.
//
// The guard cannot refuse what it was not told about: a dollar cap with a zero
// per-call estimate is only discovered at settlement, after the money is
// spent. That already caused a real overshoot, so the run is refused up front
// rather than silently overshooting again.
func TestCostCapWithoutAnEstimateIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 5, 2, fake.Options{})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 0

	_, err := core.Baseline(context.Background(), h.evals, h.opts)
	if err == nil {
		t.Fatal("a cost cap with no estimate was accepted; the cap would only be " +
			"enforced after the money was spent")
	}
	// Named in the user's terms, not the field's: this is reachable from the
	// command line, so it carries the grammar rather than a Go identifier.
	if !contains(err.Error(), "per-call cost estimate") {
		t.Errorf("error = %q, want it to name the missing estimate", err)
	}
	if !contains(err.Error(), "--cost-per-call-usd") {
		t.Errorf("error = %q, want the fix to name the flag that resolves it", err)
	}

	// A call cap alone needs no cost estimate.
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 100}, nil, 0)
	if _, err := core.Baseline(context.Background(), h.evals, h.opts); err != nil {
		t.Errorf("a call-capped run without a cost estimate was refused: %v", err)
	}
}

// erroringGoal fails after the agent has already answered — and been paid for.
type erroringGoal struct{}

func (erroringGoal) Score(context.Context, *core.Case, *core.Response) (*core.Score, error) {
	return nil, errors.New("the judge returned malformed output")
}
func (erroringGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// TestScoreFailureAfterAPaidCallPersistsTheRealCost.
//
// A Case can fail after the money is spent: the agent answers, then the Goal
// errors. An earlier version persisted a flat one-call spend for that path,
// understating what was actually spent. SettledSpend is what Guard.Restore
// reads on resume, so the resumed process would believe less was spent than
// really was — reopening the amnesia M1-0 exists to close.
//
// Unreachable with exact-match, which cannot fail; live the moment a judge
// Goal is paired with a paying agent, which is M2.
func TestScoreFailureAfterAPaidCallPersistsTheRealCost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const costPerCall = 7_000
	h := newHarness(t, 10, 3, fake.Options{CostPerCallUSDMicros: costPerCall})
	h.opts.Goal = erroringGoal{}
	h.opts.MaxErrorRate = 1

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := res.Run.GetErroredCaseCount(); got != 10 {
		t.Fatalf("errored = %d, want 10", got)
	}

	// The agent was paid for all ten calls, so the persisted total must say so.
	spend, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if want := int64(10) * costPerCall; spend.CostUSDMicros != want {
		t.Errorf("persisted spend = %d, want %d; a resumed run would reseed the "+
			"guard with %d less than was actually spent",
			spend.CostUSDMicros, want, want-spend.CostUSDMicros)
	}
}

// TestCountsNeverOutrunPersistedOutcomes.
//
// The scored count used to be incremented on the worker goroutine the moment
// scoring succeeded — before anything was persisted. A store failure mid-run
// then left the Run claiming more scored Cases than the outcomes table held,
// and that inflated count was the last thing durably recorded.
func TestCountsNeverOutrunPersistedOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 30, 5, fake.Options{})
	opts := h.opts
	opts.Store = &failingStore{Store: h.store, failRecord: true}

	res, err := core.Baseline(ctx, h.evals, opts)
	if err == nil {
		t.Fatal("a store failure was swallowed")
	}
	if res == nil {
		t.Fatal("no result returned")
	}

	done, dErr := h.store.CompletedCases(ctx, "run-1")
	if dErr != nil {
		t.Fatalf("CompletedCases: %v", dErr)
	}
	if got := int(res.Run.GetScoredCaseCount()); got > len(done) {
		t.Errorf("the Run claims %d scored Cases but only %d outcomes were "+
			"persisted; the count outran the store", got, len(done))
	}
}

// deadlineAgent always reports a deadline, deterministically.
//
// An earlier version of this test raced a real 30ms deadline against 20ms of
// simulated latency. It passed alone and failed under the full parallel suite —
// a timing-dependent flake, which CLAUDE.md says to fix rather than retry. The
// property under test is how a deadline error is CLASSIFIED, which needs no
// real clock.
type deadlineAgent struct{}

func (deadlineAgent) Invoke(context.Context, *core.Case) (*core.Response, error) {
	return nil, fmt.Errorf("calling provider: %w", context.DeadlineExceeded)
}

// TestDeadlineIsClassifiedAsAnInterruption, not as a generic agent error — the
// same distinction statusFor draws for the run as a whole. A run stopped by a
// deadline is resumable; one that broke is not.
func TestDeadlineIsClassifiedAsAnInterruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 8, 2, fake.Options{})
	h.opts.Agent = deadlineAgent{}
	// The default error-rate threshold applies: a run where everything timed
	// out is not a usable baseline.

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := res.Run.GetErroredCaseCount(); got != 8 {
		t.Fatalf("errored = %d, want 8", got)
	}

	// The run itself completed — the deadline was per-Case, not the run's.
	if got := res.Run.GetStatus(); got != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Errorf("status = %v, want COMPLETED: the run finished, the Cases did not", got)
	}
	if !res.Run.GetErrorRateExceeded() {
		t.Error("a run where every Case timed out was not marked unusable")
	}
}

// TestResumeRefusesAChangedGoalOrAgent.
//
// A Phase-3 review found that only InputFingerprint was compared, and the
// fingerprint the CLI builds covers the eval file and the split — not the Goal
// and not the Agent. So resuming with a different agent was accepted, and Cases
// scored under two different agents blended into one AggregateScore presented
// as a single homogeneous number. That is the corrupted-reference failure
// prime directive 5 exists to prevent, and it is silent.
func TestResumeRefusesAChangedGoalOrAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(o *core.BaselineOptions)
		names  string
	}{
		{
			name:   "a different agent",
			mutate: func(o *core.BaselineOptions) { o.AgentRef = &knov1.AgentRef{Ref: "other:model", Scheme: "other"} },
			names:  "agent",
		},
		{
			name:   "a different goal",
			mutate: func(o *core.BaselineOptions) { o.GoalName = "some-other-goal" },
			names:  "goal",
		},
		{
			name:   "a different eval source",
			mutate: func(o *core.BaselineOptions) { o.InputFingerprint = "fp-2" },
			names:  "inputs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, 20, 5, fake.Options{})
			if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
				t.Fatalf("first run: %v", err)
			}

			opts := h.opts
			opts.Resume = true
			tc.mutate(&opts)

			_, err := core.Baseline(ctx, h.evals, opts)
			if !errors.Is(err, errs.ErrCheckpointStale) {
				t.Fatalf("resuming with %s was accepted (err = %v); results measured "+
					"under two configurations would be averaged into one number", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not name what changed (%q):\n%s", tc.names, err)
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("the refusal does not name a fix:\n%s", err)
			}
		})
	}
}

// TestResumeAcceptsAnUnchangedConfiguration guards the test above from being
// satisfied by a check that refuses everything.
func TestResumeAcceptsAnUnchangedConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resuming an unchanged run was refused: %v", err)
	}
}

// TestTheEngineRefusesARunThatCanNeverBeValidated.
//
// docs/mental-model.md says Kno enforces the dev/holdout rule rather than
// documenting it. It was enforced only in cli/, so any other caller — the API,
// the TUI, a plugin — could run against an empty holdout and get no refusal at
// all. The guarantee has to live in the stage.
func TestTheEngineRefusesARunThatCanNeverBeValidated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name    string
		dev     int
		holdout int
		frag    string
	}{
		{name: "no holdout", dev: 20, holdout: 0, frag: "holdout"},
		{name: "no dev cases", dev: 0, holdout: 20, frag: "dev"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, 20, 5, fake.Options{})
			opts := h.opts
			opts.DevCases = tc.dev
			opts.HoldoutCases = tc.holdout

			_, err := core.Baseline(ctx, h.evals, opts)
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("the engine accepted a run with dev=%d holdout=%d (err = %v)",
					tc.dev, tc.holdout, err)
			}
			if !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("the refusal does not say which side was empty:\n%s", err)
			}
		})
	}
}

// pricingAgent is an Agent that also implements core.Estimator, so a Case's
// cost depends on the Case rather than on one run-scoped scalar.
type pricingAgent struct {
	core.Agent

	// unit is the per-index price step, so Case N costs unit*(N+1) and the
	// fixture exhibits genuine per-Case variance.
	unit int64

	// failOn makes Estimate refuse for a Case whose ID matches, standing in for
	// a model absent from the pricing table.
	failOn string
}

func (p *pricingAgent) Estimate(_ context.Context, c *core.Case) (budget.Estimate, error) {
	if p.failOn != "" && c.GetId() == p.failOn {
		return budget.Estimate{}, errors.New("no price for this model")
	}
	// Price varies per Case, keyed on its index. The harness gives every Case
	// the same one-byte input, so pricing on input length would produce a
	// constant — and a test whose fixture cannot exhibit per-Case variance
	// cannot detect an implementation that ignores the Case entirely.
	return budget.Estimate{Calls: 1, CostUSDMicros: p.unit * (caseIndex(c) + 1)}, nil
}

// caseIndex extracts the trailing number from a harness Case ID ("dev-007").
func caseIndex(c *core.Case) int64 {
	id := c.GetId()
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// TestEstimatorPricesEachCaseIndividually.
//
// The M1 design authorized every Case against one run-scoped number, which
// cannot express that a long Case costs more than a short one. A cap enforced
// against a flat guess is a cap that binds at the wrong time.
func TestEstimatorPricesEachCaseIndividually(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Case N costs 1000*(N+1). With a cap that admits the cheap ones and not
	// the dear ones, a correct implementation produces a SPLIT within one run.
	// An implementation that ignored the Case and returned a constant would
	// either score everything or refuse everything, and both fail here.
	const (
		unit         = 1_000
		capUSDMicros = 5_500 // admits Cases 0-4, refuses 5 and up
		devCases     = 20
	)

	h := newHarness(t, devCases, 5, fake.Options{})
	h.opts.Agent = &pricingAgent{Agent: h.agent, unit: unit}
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: capUSDMicros}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 1
	h.opts.Concurrency = 1 // so "which Cases fit" is about price, not scheduling

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded once the dear Cases arrive", err)
	}

	scored := int(res.Run.GetScoredCaseCount())
	if scored == 0 {
		t.Fatal("no Case was affordable; the estimate is not per-Case, or every " +
			"Case priced above the cap")
	}
	if scored == devCases {
		t.Fatal("every Case was affordable; the estimate is not per-Case, or every " +
			"Case priced below the cap")
	}
	t.Logf("%d of %d Cases fit under a %d cap before the price rose past it",
		scored, devCases, capUSDMicros)

	// The same run with no Estimator prices everything at the scalar, so the
	// cheap-then-dear split cannot occur. Without this the assertions above
	// would pass against a guard that simply ran out of money.
	flat := newHarness(t, devCases, 5, fake.Options{})
	flat.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: capUSDMicros}, nil, 0)
	flat.opts.EstCostPerCallUSDMicros = 1
	flat.opts.Concurrency = 1

	if _, err := core.Baseline(ctx, flat.evals, flat.opts); err != nil {
		t.Fatalf("the same run without an Estimator was refused: %v", err)
	}
}

// TestEstimatorFailureRefusesWhenACostCapIsSet.
//
// an Estimator that cannot price a Case must not fall back to a cheap guess when
// dollars are capped: the guard cannot refuse what it was not told about, and a
// too-low estimate is how a run walks past its cap.
func TestEstimatorFailureRefusesWhenACostCapIsSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// One unpriceable Case is refused; the priced ones still run. Aborting the
	// whole run would be harsher than the problem — no money can be spent on a
	// Case that was never authorized, and the rest are priced correctly.
	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Agent = &pricingAgent{Agent: h.agent, unit: 1, failOn: "dev-000"}
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 1

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("one unpriceable Case ended the whole run: %v", err)
	}
	if got := res.Run.GetScoredCaseCount(); got != 19 {
		t.Errorf("scored = %d, want 19; the Cases that COULD be priced should "+
			"still run", got)
	}

	// The unpriceable Case is left unrecorded rather than counted as errored:
	// it was never attempted, so a resume with a fixed pricing table must pick
	// it up again.
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if _, ok := done["dev-000"]; ok {
		t.Error("the unpriceable Case was marked complete; a resume would skip " +
			"it forever even after the pricing table is fixed")
	}

	// And when nothing can be priced, no work is recorded at all — rather than
	// a run that looks cheap because it never ran.
	all := newHarness(t, 20, 5, fake.Options{})
	unpriceable := &everythingUnpriceable{Agent: all.agent}
	all.opts.Agent = unpriceable
	all.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	all.opts.EstCostPerCallUSDMicros = 1

	if _, err := core.Baseline(ctx, all.evals, all.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if unpriceable.invoked.Load() != 0 {
		t.Errorf("the agent was invoked %d time(s) for Cases that could not be "+
			"priced; that is money spent against a cap the guard was never told "+
			"about", unpriceable.invoked.Load())
	}
}

// everythingUnpriceable stands in for a model absent from the pricing table.
type everythingUnpriceable struct {
	core.Agent
	invoked atomic.Int64
}

func (e *everythingUnpriceable) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	e.invoked.Add(1)
	return e.Agent.Invoke(ctx, c)
}

func (e *everythingUnpriceable) Estimate(context.Context, *core.Case) (budget.Estimate, error) {
	return budget.Estimate{}, errors.New("model not in the pricing table")
}

// TestEstimatorFailureRunsWhenNoCostCapIsSet: refusing the whole run because a
// price is unknown, when the user asked for no dollar cap, would be worse than
// running uncapped. The call cap still applies.
func TestEstimatorFailureRunsWhenNoCostCapIsSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Agent = &pricingAgent{Agent: h.agent, unit: 1, failOn: "dev-000"}
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("a run with no cost cap was refused over an unknown price: %v", err)
	}
}

// TestAgentWithoutEstimatorUsesTheScalar keeps the optional interface optional:
// the fake and every M1 caller must be unaffected.
func TestAgentWithoutEstimatorUsesTheScalar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 1_000

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("an agent that does not implement Estimator was refused: %v", err)
	}
}

// TestUnpriceableCaseIsNotRecordedAsDoneOrCharged.
//
// estimate() refuses before Authorize is reached, so no provider call was made
// and nothing was spent. Recording the Case would do two wrong things at once:
// charge a resumed run for a call that never happened, and mark the Case
// complete so that fixing the pricing table and re-running with --resume never
// re-attempts it. The denominator would shrink with nothing showing why.
func TestUnpriceableCaseIsNotRecordedAsDoneOrCharged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 5, 2, fake.Options{})
	unpriceable := &everythingUnpriceable{Agent: h.agent}
	h.opts.Agent = unpriceable
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 1

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	spent, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spent.Calls != 0 {
		t.Errorf("SettledSpend recorded %d call(s) for Cases the agent was never "+
			"asked to run; a resumed run would be charged for calls that did not "+
			"happen", spent.Calls)
	}

	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("%d Case(s) were marked complete without being attempted; "+
			"fixing the pricing table and resuming would never re-try them", len(done))
	}
}

// TestZeroAndMultiCallEstimatesAreRefusedUnderACostCap.
//
// A zero cost is not a cheap Case, it is an absent answer — the natural output
// of a pricing-table miss coded as a zero row — and accepting it puts the cap
// back to being discovered at settlement, which is the M1 failure this
// interface exists to close.
//
// A multi-call estimate reserves N and settles 1, because spendOf records one
// call per Response. Measured before the check: 18 real provider calls against
// MaxLLMCalls of 10. Rejected rather than coerced: silently rewriting one
// out-of-contract field hides the adapter bug a reviewer needs to see.
func TestZeroAndMultiCallEstimatesAreRefusedUnderACostCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name string
		est  budget.Estimate
	}{
		{"zero cost", budget.Estimate{Calls: 1, CostUSDMicros: 0}},
		{"two calls", budget.Estimate{Calls: 2, CostUSDMicros: 100}},
		{"zero calls", budget.Estimate{Calls: 0, CostUSDMicros: 100}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, 5, 2, fake.Options{})
			agent := &fixedEstimate{Agent: h.agent, est: tc.est}
			h.opts.Agent = agent
			h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
			h.opts.EstCostPerCallUSDMicros = 1

			if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
				t.Fatalf("Baseline: %v", err)
			}
			if got := agent.invoked.Load(); got != 0 {
				t.Errorf("the agent was invoked %d time(s) against an estimate the "+
					"guard cannot enforce a cap with", got)
			}
		})
	}
}

// fixedEstimate returns one Estimate for every Case.
type fixedEstimate struct {
	core.Agent
	est     budget.Estimate
	invoked atomic.Int64
}

func (f *fixedEstimate) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	f.invoked.Add(1)
	return f.Agent.Invoke(ctx, c)
}

func (f *fixedEstimate) Estimate(context.Context, *core.Case) (budget.Estimate, error) {
	return f.est, nil
}

// TestABudgetStopDoesNotLoseInFlightCases.
//
// A budget stop drains the workers, and a Case cancelled mid-call has no result
// to record. Recording it as terminally errored marks it complete, so a resume
// SKIPS it — and the run reports a smaller denominator than it measured, with
// nothing saying why.
//
// Measured before the fix, at concurrency 8 with a 50ms agent: two errored
// Cases on every single run, and a resumed run scoring 51 of 52. CI caught it
// as an intermittent CLI failure. It was not intermittent — the CLI's fake has
// no latency, so the window is narrow locally and wide on a loaded runner.
func TestABudgetStopDoesNotLoseInFlightCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 40

	h := newHarness(t, devCases, 10, fake.Options{
		// Latency is what puts calls in flight when the stop lands. Without it
		// the drain finds nothing running and the bug cannot appear.
		Latency:              50 * time.Millisecond,
		CostPerCallUSDMicros: 1_000,
	})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)
	h.opts.Concurrency = 8

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}

	if got := res.Run.GetErroredCaseCount(); got != 0 {
		t.Errorf("%d Case(s) were recorded as errored by the shutdown that "+
			"stopped them; each one is skipped forever on resume", got)
	}

	// Every recorded Case really produced a Score. Anything else is a Case the
	// resume will skip without having measured it.
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	scored, errored, err := h.store.OutcomeCounts(ctx, "run-1")
	if err != nil {
		t.Fatalf("OutcomeCounts: %v", err)
	}
	if errored != 0 {
		t.Errorf("the store holds %d errored outcome(s) from a resumable stop", errored)
	}
	if len(done) != scored {
		t.Errorf("%d Cases are marked complete but only %d scored", len(done), scored)
	}

	// And the resume finishes the job: every dev Case ends up scored.
	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	resumed, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if got := int(resumed.Run.GetScoredCaseCount()); got != devCases {
		t.Errorf("the completed run scored %d of %d dev Cases; the ones the "+
			"budget stop cancelled were never re-attempted", got, devCases)
	}
}

// TestAnUnaffordableRunIsRefusedRatherThanStarted.
//
// A pessimistic reservation holds concurrency x estimate of the cap in flight.
// When that exceeds the cap the guard denies before anything settles, and the
// run stops having done almost nothing — measured at a 32k output ceiling
// against a $1.00 cap at concurrency 8: the fourth Case denied, $0.00 spent.
//
// Exit 2, not 1. An exhausted cap is a resumable stop with nothing wrong with
// the data, and reporting it as a broken build is what trains people to ignore
// exit 1.
func TestAnUnaffordableRunIsRefusedRatherThanStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	// One Case costs more than the whole cap.
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 256_000
	h.opts.Concurrency = 8

	_, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d; an unaffordable run is a resumable "+
			"stop, not a broken build", got, errs.ExitBudgetStopped)
	}
	if !strings.Contains(err.Error(), "not a single Case") {
		t.Errorf("the refusal does not say what is wrong:\n%s", err)
	}
	if !strings.Contains(err.Error(), "fix:") {
		t.Errorf("no fix line:\n%s", err)
	}
}

// TestConcurrencyIsReducedRatherThanStalling.
//
// When the cap admits some Cases but not `concurrency` of them at once, the run
// should proceed at a lower concurrency rather than deny its way to a halt.
func TestConcurrencyIsReducedRatherThanStalling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 20

	h := newHarness(t, devCases, 5, fake.Options{CostPerCallUSDMicros: 1_000})
	// $1.00 cap, $0.05 per Case: a quarter of the cap admits 5 in flight, so a
	// requested concurrency of 32 is reduced rather than refused.
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 50_000
	h.opts.Concurrency = 32

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("a run that could proceed at lower concurrency was refused: %v", err)
	}
	if got := int(res.Run.GetScoredCaseCount()); got != devCases {
		t.Errorf("scored %d of %d; the run stalled instead of proceeding more "+
			"slowly", got, devCases)
	}
}

// TestFeasibilityIsCheckedAgainstWhatARESUMEHasLeft.
//
// The check runs after Guard.Restore, so it sees the headroom the resumed run
// actually has rather than the cap it was originally given. Checking before
// would be the same defect this file already fixed for the confirmation prompt.
func TestFeasibilityIsCheckedAgainstWhatARESUMEHasLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{CostPerCallUSDMicros: 40_000})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 500_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 40_000
	h.opts.Concurrency = 2

	// Burn most of the cap.
	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}

	// Resume with the SAME cap. Restore reseeds the spend, so almost nothing
	// remains — and the check must see that rather than the full cap.
	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 500_000}, nil, 0)

	_, err := core.Baseline(ctx, h.evals, opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("error = %v; the resume was admitted against the ORIGINAL cap "+
			"rather than what Restore left", err)
	}
}

// TestTheHumanIsAskedAboutTheWholeRun.
//
// The per-operation prompt was the wrong instrument. ConfirmFunc receives one
// call's estimate and the guard records agreement for the life of the run, so a
// user shown "$0.04" for the first Case that crossed the threshold consented to
// all of it — 10,000 Cases at that price is $400, asked once, at four cents.
func TestTheHumanIsAskedAboutTheWholeRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 40
	const perCall = 40_000 // $0.04

	var asked budget.Estimate
	var times atomic.Int64
	confirm := func(_ context.Context, est budget.Estimate, _ budget.Remaining) (bool, error) {
		times.Add(1)
		asked = est
		return true, nil
	}

	h := newHarness(t, devCases, 5, fake.Options{CostPerCallUSDMicros: perCall})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000_000}, confirm, 10_000)
	h.opts.EstCostPerCallUSDMicros = perCall

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	if times.Load() != 1 {
		t.Errorf("the human was asked %d times, want exactly 1", times.Load())
	}
	if want := int64(devCases) * perCall; asked.CostUSDMicros != want {
		t.Errorf("the prompt quoted %d micro-USD, want %d (%d Cases x %d); "+
			"quoting one Case's price for a whole run is how a user approves "+
			"four cents and pays for forty",
			asked.CostUSDMicros, want, devCases, perCall)
	}
	if asked.Calls != devCases {
		t.Errorf("the prompt quoted %d calls, want %d", asked.Calls, devCases)
	}
}

// TestTheQuoteIsBoundedByTheCap.
//
// With a cap set, the honest maximum exposure is the cap. Quoting DevCases x
// per-Case when the guard will stop far earlier is false in the direction that
// teaches people to dismiss the prompt.
func TestTheQuoteIsBoundedByTheCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const capUSDMicros = 500_000 // $0.50, far below 40 x $0.04

	var asked budget.Estimate
	confirm := func(_ context.Context, est budget.Estimate, _ budget.Remaining) (bool, error) {
		asked = est
		return true, nil
	}

	h := newHarness(t, 40, 5, fake.Options{CostPerCallUSDMicros: 40_000})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: capUSDMicros}, confirm, 10_000)
	h.opts.EstCostPerCallUSDMicros = 40_000
	h.opts.Concurrency = 2

	_, _ = core.Baseline(ctx, h.evals, h.opts)

	if asked.CostUSDMicros > capUSDMicros {
		t.Errorf("the prompt quoted %d micro-USD against a cap of %d; the guard "+
			"cannot spend more than the cap, so quoting more is false",
			asked.CostUSDMicros, capUSDMicros)
	}
}

// TestAResumeQuotesOnlyWhatIsLeft.
//
// The CLI has DevCases but not the completed set, so computing the quote there
// meant a run killed at 9,988 of 10,000 prompted for the full amount to finish
// twelve Cases. This is why the quote is computed in core, after
// CompletedCases.
func TestAResumeQuotesOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 40
	const perCall = 40_000

	h := newHarness(t, devCases, 5, fake.Options{CostPerCallUSDMicros: perCall})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = perCall

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) == 0 {
		t.Fatal("the first run recorded nothing, so the resume has nothing to skip")
	}

	var asked budget.Estimate
	confirm := func(_ context.Context, est budget.Estimate, _ budget.Remaining) (bool, error) {
		asked = est
		return true, nil
	}
	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000_000}, confirm, 10_000)

	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	if want := int64(devCases - len(done)); asked.Calls != want {
		t.Errorf("the resume quoted %d calls, want %d (%d dev Cases minus %d "+
			"already done); quoting the full run asks a user to approve work "+
			"they already paid for", asked.Calls, want, devCases, len(done))
	}
}

// TestARetriedCasePersistsEveryCallItPaidFor.
//
// store.Outcome.Spend is documented as "including any failed attempts preceding
// a successful retry", and it did not: the guard settled each attempt while the
// store persisted one call. Guard.Restore reads the store, so a resumed run
// under-restored the call cap by (attempts-1) for every retried Case — and a
// real provider makes retries routine where a fake never did.
func TestARetriedCasePersistsEveryCallItPaidFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 10

	// ThrottleFirstAttempt makes each Case's FIRST attempt fail with a rate
	// limit, so every Case takes exactly two provider calls.
	h := newHarness(t, devCases, 2, fake.Options{
		ThrottleFirstAttempt: true,
		CostPerCallUSDMicros: 1_000,
	})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	h.opts.RetryBackoff = time.Millisecond

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := int(res.Run.GetScoredCaseCount()); got != devCases {
		t.Fatalf("scored %d of %d; the retries did not recover", got, devCases)
	}

	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	inMemory := h.opts.Guard.Spent()

	if persisted.Calls != inMemory.Calls {
		t.Errorf("the store recorded %d calls and the guard settled %d; "+
			"Guard.Restore reads the store, so a resume would believe %d fewer "+
			"calls were made than really were",
			persisted.Calls, inMemory.Calls, inMemory.Calls-persisted.Calls)
	}
	if want := int64(devCases * 2); persisted.Calls != want {
		t.Errorf("persisted %d calls, want %d (two per Case)", persisted.Calls, want)
	}
}

// TestATransientTransportFailureIsRetried.
//
// A stale pooled connection is not the agent failing. At concurrency, any pause
// in a long run produces a handful of them, and treating them as terminal marks
// a healthy baseline unusable over an idle timeout. The transport classifies;
// core is what decides to try again.
func TestATransientTransportFailureIsRetried(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 5, 2, fake.Options{})
	agent := &transientOnce{Agent: h.agent}
	h.opts.Agent = agent
	h.opts.RetryBackoff = time.Millisecond

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := int(res.Run.GetErroredCaseCount()); got != 0 {
		t.Errorf("%d Case(s) errored on a transient transport failure that a "+
			"retry recovers; a dropped connection would mark a healthy "+
			"baseline unusable", got)
	}
	if agent.calls.Load() <= int64(res.Run.GetScoredCaseCount()) {
		t.Error("no Case was actually retried, so this test proves nothing")
	}
}

// transientOnce fails each Case's first call the way a dropped connection does.
type transientOnce struct {
	core.Agent
	calls atomic.Int64
	seen  sync.Map
}

func (a *transientOnce) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	a.calls.Add(1)
	if _, been := a.seen.LoadOrStore(c.GetId(), true); !been {
		return nil, errs.ErrTransportTransient.Wrap(errors.New("connection reset by peer"))
	}
	return a.Agent.Invoke(ctx, c)
}

// TestAResumeAfterTheAliasMovesIsRefused.
//
// A ref like openai:gpt-4.1 is a moving pointer. A run interrupted on Monday
// and resumed on Friday after the alias re-points passes every other resume
// check and blends two models into one AggregateScore — the corrupted-reference
// failure checkResumable exists to prevent, arriving through the one input it
// could not see.
func TestAResumeAfterTheAliasMovesIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 10, 3, fake.Options{})
	h.opts.ResolvedModel = "gpt-4.1-2026-05-01"
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Record what the first run resolved to, the way an adapter will.
	run, err := h.store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	run.CaseExecution = &knov1.CaseExecution{ResolvedModels: []string{"gpt-4.1-2026-05-01"}}
	if err := h.store.FinishRun(ctx, run); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.ResolvedModel = "gpt-4.1-2026-08-01" // the alias moved

	_, err = core.Baseline(ctx, h.evals, opts)
	if !errors.Is(err, errs.ErrCheckpointStale) {
		t.Fatalf("error = %v, want ErrCheckpointStale; the run resumed against a "+
			"different model and would average two of them into one score", err)
	}
	if !strings.Contains(err.Error(), "resolved model") {
		t.Errorf("the refusal does not name what changed:\n%s", err)
	}
}

// TestAnUnknownResolvedModelDoesNotBlockAResume.
//
// Empty on either side means unknown — the first process may not have reached a
// response. Refusing on absence would make every run that stopped before its
// first answer unresumable.
func TestAnUnknownResolvedModelDoesNotBlockAResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 10, 3, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.ResolvedModel = "gpt-4.1-2026-08-01" // known now, unknown then

	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Errorf("a resume was refused because the FIRST run never recorded a "+
			"resolved model: %v", err)
	}
}

// TestTheRetryBudgetBoundsTimeAsWellAsAttempts.
//
// Three attempts at 500ms doubling is a 1.5-second window, and a real
// provider's sustained 429 window is minutes — so a rate-limited account marked
// a good baseline unusable. Time alone is also wrong: each attempt takes its own
// reservation, so a long window lets one Case consume dozens of calls. Both
// bounds, whichever binds first.
func TestTheRetryBudgetBoundsTimeAsWellAsAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 4, 2, fake.Options{RateLimitEvery: 1}) // every call throttled
	h.opts.MaxAttempts = 100                                  // attempts would never bind
	h.opts.RetryBudget = 50 * time.Millisecond
	h.opts.RetryBackoff = 20 * time.Millisecond

	start := time.Now()
	res, err := core.Baseline(ctx, h.evals, h.opts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := int(res.Run.GetErroredCaseCount()); got == 0 {
		t.Fatal("every call was throttled but nothing errored, so the budget was never reached")
	}
	// 4 Cases, each bounded at ~50ms, with concurrency. Generous ceiling: the
	// point is that 100 attempts did not run.
	if elapsed > 5*time.Second {
		t.Errorf("the run took %v; the retry budget did not bound it and the "+
			"attempt count was doing the work", elapsed)
	}
}

// TestAProviderRequestedWaitOverridesOurGuess.
//
// A provider that says how long to wait is the authority on its own limits; the
// doubling backoff is only a guess for when it does not say. The transport
// parses Retry-After in both RFC 9110 forms and clamps it — core only reads
// what it decided.
func TestAProviderRequestedWaitOverridesOurGuess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 3, 1, fake.Options{})
	agent := &retryAfterAgent{Agent: h.agent, wait: 5 * time.Millisecond}
	h.opts.Agent = agent
	// A backoff far longer than the provider's ask. If ours won, the run would
	// take seconds rather than milliseconds.
	h.opts.RetryBackoff = 2 * time.Second
	h.opts.RetryBudget = time.Second

	start := time.Now()
	res, err := core.Baseline(ctx, h.evals, h.opts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := int(res.Run.GetScoredCaseCount()); got != 3 {
		t.Fatalf("scored %d of 3; the retries did not recover", got)
	}
	if elapsed > time.Second {
		t.Errorf("the run took %v; the provider asked for %v and our own "+
			"backoff was used instead", elapsed, agent.wait)
	}
}

// retryAfterAgent throttles each Case once, carrying a requested wait.
type retryAfterAgent struct {
	core.Agent
	wait time.Duration
	seen sync.Map
}

func (a *retryAfterAgent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if _, been := a.seen.LoadOrStore(c.GetId(), true); !been {
		return nil, &waitErr{wait: a.wait}
	}
	return a.Agent.Invoke(ctx, c)
}

// waitErr is a rate-limit error carrying the provider's requested delay, the
// shape an adapter produces from a Retry-After header.
type waitErr struct{ wait time.Duration }

func (e *waitErr) Error() string             { return "rate limited" }
func (e *waitErr) Unwrap() error             { return errs.ErrRateLimited }
func (e *waitErr) RetryAfter() time.Duration { return e.wait }

// TestAnUnconfirmedRunSpendsNothing.
//
// Declining the prompt must stop before any Case is authorized, and say so
// without implying something broke.
func TestAnUnconfirmedRunSpendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	declined := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		return false, nil
	}

	h := newHarness(t, 20, 5, fake.Options{CostPerCallUSDMicros: 40_000})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000_000}, declined, 10_000)
	h.opts.EstCostPerCallUSDMicros = 40_000

	_, err := core.Baseline(ctx, h.evals, h.opts)
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "nothing was spent") {
		t.Errorf("the refusal does not say the run cost nothing:\n%s", err)
	}
	if spent := h.opts.Guard.Spent(); spent.Calls != 0 || spent.CostUSDMicros != 0 {
		t.Errorf("a declined run spent %+v", spent)
	}
}
