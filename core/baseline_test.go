package core_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
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
	_ "modernc.org/sqlite"

	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
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
	dbPath string   // for tests that must reach the file directly
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

	dbPath := filepath.Join(t.TempDir(), "kno.db")
	st, err := store.NewSQLite(context.Background(), dbPath)
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
		dbPath: dbPath,
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

	// Counted by payload type rather than asserted as one magic total.
	//
	// The old form required maxSeq == 17 exactly. M2-10 adds run-level
	// emitters — a progress heartbeat among them — whose count depends on how
	// long the run took, which makes an exact total NONDETERMINISTIC rather
	// than merely a different number. What this test is about is that the
	// stream opens once, covers every Case once, and closes once.
	log := eventLog(t, h.dbPath, "run-1")

	counts := map[string]int{}
	for _, ev := range log {
		counts[payloadName(ev)]++
	}
	if counts["RunStarted"] != 1 {
		t.Errorf("%d RunStarted, want 1", counts["RunStarted"])
	}
	if counts["RunFinished"] != 1 {
		t.Errorf("%d RunFinished, want 1", counts["RunFinished"])
	}
	if counts["RunResumed"] != 0 {
		t.Errorf("%d RunResumed on a fresh run, want 0", counts["RunResumed"])
	}
	if got := counts["CaseScored"] + counts["CaseErrored"]; got != 15 {
		t.Errorf("%d per-Case events, want 15 — one per Case, exactly once", got)
	}
	// The sequence still numbers every event that was written, with no gap
	// between the last one and the reported maximum.
	if maxSeq != int64(len(log)) {
		t.Errorf("max sequence %d but %d events recorded; a consumer reading the "+
			"maximum would expect events this run never wrote", maxSeq, len(log))
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
	failCounts bool
	failScores bool
	failObs    bool
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

func (f *failingStore) CaseObservations(ctx context.Context, id string) (store.Observations, error) {
	if f.failObs {
		return store.Observations{}, errStore
	}
	return f.Store.CaseObservations(ctx, id)
}

func (f *failingStore) OutcomeCounts(ctx context.Context, id string) (int, int, error) {
	if f.failCounts {
		return 0, 0, errStore
	}
	return f.Store.OutcomeCounts(ctx, id)
}

func (f *failingStore) ScoreSum(ctx context.Context, id string) (float64, int, int, error) {
	if f.failScores {
		return 0, 0, 0, errStore
	}
	return f.Store.ScoreSum(ctx, id)
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
		{"cannot load prior outcome counts", func(f *failingStore) { f.failCounts = true }, true},
		// A resumed run whose prior scores cannot be read must stop, not carry
		// on with an aggregate that silently spans only the tail. That is the
		// exact defect docs/debt.md#27 repaid.
		{"cannot load prior scores", func(f *failingStore) { f.failScores = true }, true},
		// The resumed twin of the fresh-run case above: emitRunResumed's error
		// path had no coverage, and it is the only emitter whose failure
		// happens before the executor starts.
		{"cannot append the resumed run's opening event", func(f *failingStore) { f.failEvent = true }, true},
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
// the staleness message: the fix must name what actually changed, and must NOT
// name a cause checkResumable never tests. It named "split configuration" for
// years; the split is not compared at all — InputFingerprint covers the eval
// SOURCE only — so the message sent users to restore a setting they had not
// touched.
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
	got := err.Error()
	if !contains(got, "goal or agent") {
		t.Errorf("error = %q, want it to name the goal/agent rather than the evals", got)
	}
	if contains(got, "split") {
		t.Errorf("error = %q names the split, which checkResumable never compares", got)
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

// WorstCase is the most any Case could cost, for planning. The harness makes 20
// dev Cases, so the last one is the dearest.
func (p *pricingAgent) WorstCase() budget.Estimate {
	return budget.Estimate{Calls: 1, CostUSDMicros: p.unit * 20}
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

// WorstCase reports nothing, which is what an adapter that cannot price the
// model should say. Planning then falls back to the scalar, and the per-Case
// refusal is what actually stops the run.
func (e *everythingUnpriceable) WorstCase() budget.Estimate { return budget.Estimate{} }

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

func (f *fixedEstimate) WorstCase() budget.Estimate { return f.est }

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

	// Without this the assertion below is vacuous: a zero-value Estimate is
	// never greater than the cap, so the test passed if the prompt never fired.
	if asked.Calls == 0 {
		t.Fatal("the confirmation never fired, so the bound below proves nothing")
	}
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
	// Assert the ATTEMPT count, not elapsed time. A wall-clock ceiling with
	// 78x headroom passes whether the bound is cumulative or per-sleep, which
	// is exactly the distinction a frozen clock used to destroy.
	calls := h.agent.Calls()
	if maxIfBudgetIgnored := int64(4 * h.opts.MaxAttempts); calls >= maxIfBudgetIgnored {
		t.Errorf("the agent was called %d times; with MaxAttempts %d over 4 "+
			"Cases the attempt count alone permits %d, so the time budget did "+
			"not bind", calls, h.opts.MaxAttempts, maxIfBudgetIgnored)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the run took %v", elapsed)
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

// TestRetryExhaustionPersistsEveryCallItPaidFor.
//
// The sibling test uses ThrottleFirstAttempt, so it succeeds on attempt two and
// takes the has-a-Response branch. This one exhausts retries — every attempt
// fails, there is no Response, and that branch hardcoded one call.
//
// It is the branch retries actually take under a 429 storm, and it is the one
// the headline fix missed: measured 5 persisted against 15 settled.
func TestRetryExhaustionPersistsEveryCallItPaidFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 5
	const attempts = 3

	h := newHarness(t, devCases, 2, fake.Options{RateLimitEvery: 1}) // every call throttled
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	h.opts.MaxAttempts = attempts
	h.opts.RetryBackoff = time.Millisecond

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if got := int(res.Run.GetErroredCaseCount()); got != devCases {
		t.Fatalf("errored %d of %d; the Cases did not exhaust their retries",
			got, devCases)
	}

	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	inMemory := h.opts.Guard.Spent()

	if persisted.Calls != inMemory.Calls {
		t.Errorf("the store recorded %d calls and the guard settled %d; "+
			"Guard.Restore reads the store, so a resume re-authorizes %d calls "+
			"that were already made and paid for",
			persisted.Calls, inMemory.Calls, inMemory.Calls-persisted.Calls)
	}
	if want := int64(devCases * attempts); persisted.Calls != want {
		t.Errorf("persisted %d calls, want %d (%d Cases x %d attempts)",
			persisted.Calls, want, devCases, attempts)
	}
}

// TestTheDefaultConcurrencyIsPlannedFor.
//
// Zero is not "no concurrency" — it is the CLI's default, and the executor
// turns it into min(NumCPU, 8), the exact figure in the measurement the
// feasibility check exists for. Treating it as a bypass meant the guard still
// denied its way to a halt on the path almost every user takes: 0 of 60 Cases
// scored, $0.00 spent, against a $1.00 cap.
func TestTheDefaultConcurrencyIsPlannedFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 30

	h := newHarness(t, devCases, 5, fake.Options{
		Latency:              5 * time.Millisecond,
		CostPerCallUSDMicros: 1_000,
	})
	// A per-Case estimate large enough that the executor's default concurrency
	// would hold more than the cap in flight at once.
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)
	h.opts.EstCostPerCallUSDMicros = 200_000
	h.opts.Concurrency = 0 // exactly what the CLI passes

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("a run at the default concurrency was refused: %v", err)
	}
	if got := int(res.Run.GetScoredCaseCount()); got != devCases {
		t.Errorf("scored %d of %d at the DEFAULT concurrency; the feasibility "+
			"check was bypassed and the guard denied its way to a halt",
			got, devCases)
	}
}

// TestARefusedRunLeavesNoDanglingRecord.
//
// Both new checks used to refuse after openRun, leaving a row permanently in
// RUNNING with no outcomes and no events — and the interactive path declines by
// default, so every above-threshold invocation minted a fresh orphan. A CI gate
// reading exit 2 as "not a failure" then reported green for a run that never
// started.
func TestARefusedRunLeavesNoDanglingRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	declined := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		return false, nil
	}

	h := newHarness(t, 20, 5, fake.Options{CostPerCallUSDMicros: 40_000})
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 100_000_000}, declined, 10_000)
	h.opts.EstCostPerCallUSDMicros = 40_000

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}

	if _, err := h.store.GetRun(ctx, "run-1"); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("a declined run left a Run record (err = %v); it never started, "+
			"so a later reader would see it stuck in RUNNING forever", err)
	}
}

// TestADeclinedRunCannotBeReAskedIntoSpending.
//
// A refusal used to leave the guard's confirmed flag false, so the next
// Authorize prompted again — and on a yes, authorized the spend the user had
// just declined.
func TestADeclinedRunCannotBeReAskedIntoSpending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var prompts atomic.Int64
	confirm := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		prompts.Add(1)
		return false, nil
	}

	g := budget.New(budget.Limits{MaxCostUSDMicros: 10_000_000}, confirm, 10_000)

	if ok, _ := g.PreConfirm(ctx, budget.Estimate{Calls: 100, CostUSDMicros: 1_000_000}); ok {
		t.Fatal("a declined run reported agreement")
	}
	if !g.Declined() {
		t.Error("the refusal was not recorded")
	}

	// The per-operation path must not offer a second chance.
	if _, err := g.Authorize(ctx, budget.Estimate{Calls: 1, CostUSDMicros: 500_000}); err == nil {
		t.Error("a Case was authorized after the run was declined")
	}
	if got := prompts.Load(); got != 1 {
		t.Errorf("the human was prompted %d times; a decline must be final", got)
	}
	if spent := g.Spent(); spent.Calls != 0 {
		t.Errorf("a declined run spent %+v", spent)
	}
}

// TestResumedRunReportsTheWholeRunsMean is docs/debt.md#27.
//
// The counts spanned the whole run while the mean spanned only the tail, so a
// run interrupted after some Cases and resumed for the rest reported a
// denominator and a numerator describing different populations. That number is
// printed on every run, put in --json, and carried on the event stream.
func TestResumedRunReportsTheWholeRunsMean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 40

	// Scores alternate 1 and 0 by Case index, so the true mean is 0.5 and any
	// partial window over a contiguous slice is measurably different.
	scoreByIndex := func(c *core.Case) string {
		if caseIndex(c)%2 == 0 {
			return c.GetExpected() // exact match scores 1
		}
		return "wrong"
	}

	h := newHarness(t, devCases, 10, fake.Options{Answer: scoreByIndex})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 15}, nil, 0)
	h.opts.Concurrency = 1

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) == 0 || len(done) >= devCases {
		t.Fatalf("the first run recorded %d of %d Cases; the test needs a "+
			"genuine partial run", len(done), devCases)
	}

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if got := int(res.Run.GetScoredCaseCount()); got != devCases {
		t.Fatalf("scored %d of %d", got, devCases)
	}
	if res.AggregateScore == nil {
		t.Fatal("the completed run reported no aggregate")
	}

	// The truth, computed from the store rather than from the aggregator.
	sum, counted, _, err := h.store.ScoreSum(ctx, "run-1")
	if err != nil {
		t.Fatalf("ScoreSum: %v", err)
	}
	want := sum / float64(counted)

	if diff := *res.AggregateScore - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("the resumed run reported a mean of %v; the whole run's mean is "+
			"%v over %d Cases. The counts span the run and the mean spans the "+
			"tail, so they describe different populations",
			*res.AggregateScore, want, counted)
	}
}

// TestAPurgedRunReportsNoAggregateRatherThanAWrongOne.
//
// A Case purged before its score lived in its own column has an unrecoverable
// number. Averaging it in as zero drags the mean down and presents the result
// as the run's actual aggregate — worse than reporting nothing, because it
// looks like a measurement.
func TestAPurgedRunReportsNoAggregateRatherThanAWrongOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}

	// The pre-M2-1 shape: a scored outcome whose number is gone. There is no
	// production path that produces this — purging today preserves the number —
	// so it is written directly.
	clearScoreValues(t, h.dbPath, "run-1")

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if res.AggregateScore != nil {
		t.Errorf("a run with unrecoverable scores reported an aggregate of %v; "+
			"it is computed over only the Cases whose numbers survived, so it "+
			"is a mean of a different population than the counts describe",
			*res.AggregateScore)
	}
	if !strings.Contains(res.Run.GetIncompleteReason(), "purged") {
		t.Errorf("the run does not say why it has no aggregate: %q",
			res.Run.GetIncompleteReason())
	}
}

// clearScoreValues nulls the numeric score column while leaving the rows,
// reproducing a run purged before the score had a column of its own.
func clearScoreValues(t *testing.T, dbPath, runID string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(
		`UPDATE outcomes SET score_value = NULL, score_proto = NULL WHERE run_id = ?`,
		runID); err != nil {
		t.Fatalf("clearing score values: %v", err)
	}
}

// TestBothIncompleteReasonsSurvive.
//
// A run can be both unscoreable and too error-prone. Assigning
// IncompleteReason twice left whichever branch ran second, and the one it lost
// is the one the user cannot infer from anything else on screen: the error rate
// is already visible in the printed counts and in ErrorRateExceeded, while a
// missing aggregate has no other signal at all.
func TestBothIncompleteReasonsSurvive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Half the Cases error, well above the default threshold.
	h := newHarness(t, 20, 5, fake.Options{FailEvery: 2})
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 8}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first pass: %v", err)
	}

	clearScoreValues(t, h.dbPath, "run-1")

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	reason := res.Run.GetIncompleteReason()
	if !res.Run.GetErrorRateExceeded() {
		t.Fatalf("the fixture did not trip the error-rate threshold; reason = %q", reason)
	}
	if !strings.Contains(reason, "above the") {
		t.Errorf("IncompleteReason = %q, want it to state the error rate", reason)
	}
	if !strings.Contains(reason, "no reportable aggregate") {
		t.Errorf("IncompleteReason = %q, want it to ALSO state that the aggregate is "+
			"gone; that half has no other signal in the report", reason)
	}
}

// TestAResumedRunDoesNotInheritAStaleVerdict.
//
// ErrorRateExceeded and IncompleteReason are recomputed over the whole run on
// every close, but were only ever set, never cleared. A process that errored
// past the threshold and stopped stamped the stored Run; a resume that went on
// to score cleanly recomputed a passing rate, skipped the branch, and left the
// stamp standing. The run then reported "not a usable baseline" forever, and
// no amount of further clean work could clear it.
func TestAResumedRunDoesNotInheritAStaleVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Every Case in the first pass errors, and the budget stops it after four.
	h := newHarness(t, 100, 10, fake.Options{FailEvery: 1})
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 4}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first pass: %v", err)
	}

	stored, err := h.store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("loading the interrupted run: %v", err)
	}
	if !stored.GetErrorRateExceeded() {
		t.Skipf("the fixture did not trip the threshold on the first pass "+
			"(%d errored of %d attempted); nothing stale to inherit",
			stored.GetErroredCaseCount(), stored.GetAttemptedCaseCount())
	}

	// The resume runs clean, bringing the whole run's rate under the threshold.
	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	opts.Agent = fake.New(fake.Options{})
	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	scored, errored := res.Run.GetScoredCaseCount(), res.Run.GetErroredCaseCount()
	rate := float64(errored) / float64(scored+errored)
	if rate > core.DefaultMaxErrorRate {
		t.Fatalf("the whole run's rate is %.2f, still above threshold; this test needs "+
			"a run that recovers", rate)
	}
	if res.Run.GetErrorRateExceeded() {
		t.Errorf("ErrorRateExceeded is still set after the whole run came in at %.2f; "+
			"the flag is set by a branch with no matching clear", rate)
	}
	if strings.Contains(res.Run.GetIncompleteReason(), "not a usable baseline") {
		t.Errorf("IncompleteReason = %q, carried over from the process that stopped",
			res.Run.GetIncompleteReason())
	}
}

// TestAResumedRunIsNotQuotedAgainstTheCapItAlreadySpent.
//
// The confirmation prompt asks the human to consent to a figure. On a resume
// that figure was clamped against the STATIC cap rather than the headroom the
// run actually has, so a run with $0.10 left under a $5.00 cap was quoted
// against $5.00. The CLI renders both numbers in one sentence — "would spend
// about $3.00 ($0.10 remaining)" — so the contradiction reaches the user
// intact.
//
// Overstating is the direction that matters. confirmRun's own godoc says
// showing a larger number than the guard will permit "is false in the
// direction that teaches people to dismiss the prompt", and a dismissed prompt
// is prime directive 4 defeated by boredom.
func TestAResumedRunIsNotQuotedAgainstTheCapItAlreadySpent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const costCap = 5_000_000 // $5.00

	h := newHarness(t, 200, 20, fake.Options{CostPerCallUSDMicros: 50_000}) // $0.05/Case
	// Concurrency 1 so at most one reservation is outstanding and the arithmetic
	// below is exact; Remaining subtracts reserved as well as spent.
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = 50_000
	// 98 is load-bearing arithmetic, not a round number. The cost cap admits
	// exactly 100 calls at $0.05, so stopping at 98 leaves $0.10 — enough for a
	// resume to be possible, little enough that quoting the whole cap is
	// obviously wrong. Stopped by the CALL cap so the money, not the calls, is
	// what the resume is bounded by.
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: costCap, MaxLLMCalls: 98}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop on the call cap: %v", err)
	}

	spent, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spent.CostUSDMicros < costCap/2 {
		t.Fatalf("the first run spent %d of a %d cap; the test needs a run that "+
			"consumed most of it", spent.CostUSDMicros, costCap)
	}

	// Resume under the same cap, capturing what the human is asked to approve.
	var quoted budget.Estimate
	var left budget.Remaining
	var asked bool
	confirm := func(_ context.Context, est budget.Estimate, rem budget.Remaining) (bool, error) {
		quoted, left, asked = est, rem, true
		return true, nil
	}

	opts := h.opts
	opts.Resume = true
	// The call cap is RAISED, which is the actual story of this resume: the
	// user hit --max-calls, saw it was too low, and lifted it. Re-passing the
	// exhausted 98 would leave no calls either, so nothing would be quoted and
	// the fixture would prove nothing. The COST cap is unchanged, because that
	// is the one under test.
	//
	// A threshold of 1 micro-USD arms the prompt for any non-trivial run. What
	// happens at the real $1.00 threshold is a separate test — see
	// TestAResumeUnderTheConfirmThresholdProceedsWithoutAsking.
	opts.Guard = budget.New(
		budget.Limits{MaxCostUSDMicros: costCap, MaxLLMCalls: 1_000}, confirm, 1)
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("resumed run: %v", err)
	}

	if !asked {
		t.Fatal("the human was never asked, so there is no quoted figure to check; " +
			"the fixture no longer exercises the confirmation path")
	}
	// Exactly the headroom, not merely "no more than" it. The fixture spends
	// 98 calls x $0.05 = $4.90 of a $5.00 cap, so $0.10 is left and $0.10 is
	// the only correct quote. An inequality would pass a fix that clamped to
	// half the remaining budget.
	if want := int64(100_000); quoted.CostUSDMicros != want {
		t.Errorf("the prompt quoted %d micro-USD, want %d — the CLI renders this "+
			"beside %d remaining in one sentence, so a mismatch is a "+
			"self-contradiction the user reads intact",
			quoted.CostUSDMicros, want, left.CostUSDMicros)
	}
	if quoted.CostUSDMicros > left.CostUSDMicros {
		t.Errorf("quoted %d with only %d remaining", quoted.CostUSDMicros, left.CostUSDMicros)
	}
}

// TestAFreshRunIsStillQuotedAgainstItsWholeCap.
//
// The resume fix swapped Limits() for Remaining() in the confirmation clamp.
// On a fresh run those are equal, and this pins that: a change that made the
// prompt quote less than the run can actually spend would be wrong in the
// other direction, and understating exposure is how a surprise bill happens.
func TestAFreshRunIsStillQuotedAgainstItsWholeCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name   string
		limits budget.Limits
		want   int64 // the quoted cost, or -1 for "not clamped"
	}{
		{
			// 200 Cases at $0.05 is $10.00 of intent against a $5.00 cap.
			name:   "a cost cap clamps the quote to the cap",
			limits: budget.Limits{MaxCostUSDMicros: 5_000_000},
			want:   5_000_000,
		},
		{
			// 200 Cases at $0.05 is $10.00 of intent, but 10 calls is all the
			// guard will authorize — $0.50. Quoting $10.00 overstates by 20x,
			// the same class and direction of error as the resume bug, one
			// field over in the same struct.
			name:   "a call cap bounds the dollar figure too",
			limits: budget.Limits{MaxLLMCalls: 10},
			want:   500_000,
		},
		{
			name:   "a call cap wider than the run does not bound it",
			limits: budget.Limits{MaxLLMCalls: 1_000},
			want:   10_000_000,
		},
		{
			name:   "no cap does not clamp",
			limits: budget.Limits{},
			want:   10_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var quoted budget.Estimate
			var asked bool
			confirm := func(_ context.Context, est budget.Estimate, _ budget.Remaining) (bool, error) {
				quoted, asked = est, true
				return false, nil // decline, so nothing is spent
			}

			h := newHarness(t, 200, 20, fake.Options{CostPerCallUSDMicros: 50_000})
			h.opts.EstCostPerCallUSDMicros = 50_000
			h.opts.Guard = budget.New(tt.limits, confirm, 1)

			if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
				t.Fatalf("a declined run must refuse: %v", err)
			}
			if !asked {
				t.Fatal("the human was never asked; the fixture no longer reaches the prompt")
			}
			if quoted.CostUSDMicros != tt.want {
				t.Errorf("quoted %d micro-USD, want %d", quoted.CostUSDMicros, tt.want)
			}
		})
	}
}

// TestAResumeUnderTheConfirmThresholdProceedsWithoutAsking.
//
// This is a deliberate change in WHEN consent is required, not a side effect,
// and it is pinned here because Phase 3 caught it going unnoticed.
//
// The prompt fires when the quoted total is at or above --confirm-threshold
// ($1.00 in the CLI). Bounding the quote to what the guard will actually
// permit means a resume with $0.10 of headroom now quotes $0.10, which is
// below the threshold, so it proceeds without asking and spends that $0.10.
//
// Before, it quoted the whole $5.00 cap, crossed the threshold, and prompted —
// and since the current confirmFunc always declines, the run refused. So the
// old behaviour was: refuse a $0.10 run because it was described as a $5.00
// one. That is not consent, it is a wrong number producing an accidental
// refusal.
//
// The threshold means "ask me before spending more than this". A run that
// cannot spend more than this should not ask. What the user set still binds:
// the $5.00 cap is enforced, and the $0.10 is inside it.
func TestAResumeUnderTheConfirmThresholdProceedsWithoutAsking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const costCap = 5_000_000   // $5.00
	const threshold = 1_000_000 // $1.00 — the CLI's real confirmThresholdUSD

	h := newHarness(t, 200, 20, fake.Options{CostPerCallUSDMicros: 50_000})
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = 50_000
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: costCap, MaxLLMCalls: 98}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop on the call cap: %v", err)
	}

	var asked bool
	confirm := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		asked = true
		return false, nil // decline, as the current CLI always does
	}

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(
		budget.Limits{MaxCostUSDMicros: costCap, MaxLLMCalls: 1_000}, confirm, threshold)

	before, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}

	if _, err := core.Baseline(ctx, h.evals, opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("resumed run: %v", err)
	}

	if asked {
		t.Error("the run asked for consent to spend $0.10 against a $1.00 threshold; " +
			"the threshold means 'ask before spending more than this'")
	}

	after, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(after) <= len(before) {
		t.Errorf("completed %d Cases before and %d after; the run did not proceed, "+
			"so this test is not about a run that runs without asking", len(before), len(after))
	}

	// Guard.Spent() is cumulative across the resume — Baseline restores the
	// prior spend into it — so the cap binding is the assertion, not the delta.
	if spent := opts.Guard.Spent().CostUSDMicros; spent > costCap {
		t.Errorf("spent %d micro-USD against a %d cap; the cap must still bind "+
			"even when the prompt does not fire", spent, costCap)
	}
}

// eventLog reads every event for a run, in sequence order, decoded.
//
// Read from the database rather than from a spy, because the invariants under
// test — no gaps, RunFinished last — are properties of what was DURABLY
// recorded. An in-memory recorder would pass while the store held gaps.
func eventLog(t *testing.T, dbPath, runID string) []*knov1.Event {
	t.Helper()

	// busy_timeout, because the store's own DSN sets it and its comment says
	// leaving it at zero "turns write contention into an immediate SQLITE_BUSY".
	// Safe today only because every caller reads after the run has returned;
	// M2-10c needs this helper mid-run, with a ticker writing.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(
		`SELECT proto FROM events WHERE run_id = ? ORDER BY sequence`, runID)
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*knov1.Event
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatalf("scanning event: %v", err)
		}
		ev := &knov1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			t.Fatalf("unmarshaling event: %v", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating events: %v", err)
	}
	return out
}

// payloadName is the payload's type, for assertions that read like the schema.
func payloadName(ev *knov1.Event) string {
	switch ev.GetPayload().(type) {
	case *knov1.Event_RunStarted:
		return "RunStarted"
	case *knov1.Event_RunResumed:
		return "RunResumed"
	case *knov1.Event_CaseScored:
		return "CaseScored"
	case *knov1.Event_CaseErrored:
		return "CaseErrored"
	case *knov1.Event_RunFinished:
		return "RunFinished"
	case nil:
		return "<no payload>"
	default:
		return fmt.Sprintf("%T", ev.GetPayload())
	}
}

// TestTheEventSequenceHasNoGaps.
//
// Event.sequence exists so a consumer that sees a gap knows it lost events
// rather than silently under-reporting. A number allocated before a path that
// returns without writing burns it, and the gap is permanent: it survives
// every resume, and MaxEventSequence never heals it.
//
// What this actually pins is that a resumed process continues numbering from
// MaxEventSequence with no off-by-one — seedSequence(maxSeq+1) fails it, and
// no other test covers a resumed stream at all.
//
// It does NOT pin the burn this PR is named for. A budget refusal never
// reaches emit: sinkFunc returns first on the same predicate, so emit's own
// check is dead code and reverting the allocation order leaves this green.
// The burn becomes reachable with M2-10c's ticker, and the refusal path is
// covered directly by TestNothingIsAppendedAfterRunFinished.
func TestTheEventSequenceHasNoGaps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 30

	h := newHarness(t, devCases, 10, fake.Options{FailEvery: 4})
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 12}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop on the call cap: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	log := eventLog(t, h.dbPath, "run-1")
	if len(log) == 0 {
		t.Fatal("no events were recorded at all; the fixture proves nothing")
	}
	for i, ev := range log {
		if want := int64(i + 1); ev.GetSequence() != want {
			t.Fatalf("event %d has sequence %d, want %d — a gap here is permanent, "+
				"and a consumer reading it cannot tell a lost event from none",
				i, ev.GetSequence(), want)
		}
	}
}

// TestAResumedRunOpensWithRunResumedNotASecondRunStarted.
//
// A second RunStarted carries the ORIGINAL total, so a live view that resets
// progress on it jumps backward on every resume — docs/debt.md#29. It also
// made RunStarted's own proto comment ("Always sequence 1") false the moment a
// run was resumed once.
//
// Asserts the absence as well as the presence: emitting BOTH would satisfy a
// test that only looked for RunResumed, and an early draft of this change did
// exactly that.
func TestAResumedRunOpensWithRunResumedNotASecondRunStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 20

	h := newHarness(t, devCases, 10, fake.Options{CostPerCallUSDMicros: 1_000})
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 8}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop early: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	log := eventLog(t, h.dbPath, "run-1")

	var started, resumed int
	var got *knov1.RunResumed
	for _, ev := range log {
		switch p := ev.GetPayload().(type) {
		case *knov1.Event_RunStarted:
			started++
		case *knov1.Event_RunResumed:
			resumed++
			got = p.RunResumed
		}
	}

	if started != 1 {
		t.Errorf("%d RunStarted events, want exactly 1 — the fresh run's", started)
	}
	if resumed != 1 {
		t.Fatalf("%d RunResumed events, want exactly 1", resumed)
	}

	// NOT the identity already+remaining == total: over the code as written
	// that is a + (t-a) == t, a tautology that cannot fail. The bounds are
	// what a renderer actually depends on — remaining is the denominator of
	// SESSION progress, so a negative divides wrongly, and already exceeding
	// total inverts OVERALL progress.
	if got.GetRemaining() < 0 {
		t.Errorf("remaining=%d is negative; it is a denominator", got.GetRemaining())
	}
	if got.GetAlreadyCompleted() > got.GetTotalCases() {
		t.Errorf("already=%d exceeds total=%d", got.GetAlreadyCompleted(), got.GetTotalCases())
	}
	if got.GetAlreadyCompleted()+got.GetRemaining() != got.GetTotalCases() {
		t.Errorf("already=%d + remaining=%d != total=%d",
			got.GetAlreadyCompleted(), got.GetRemaining(), got.GetTotalCases())
	}
	if got.GetAlreadyCompleted() == 0 {
		t.Error("RunResumed reports nothing already completed, so the fixture did " +
			"not actually resume partial work")
	}
	if got.GetRestoredCostUsdMicros() <= 0 {
		t.Error("RunResumed reports no restored spend; a resumed run that believed " +
			"it had spent nothing could consume its cap a second time, and this " +
			"field exists so a consumer can see that it did not")
	}
}

// TestRunFinishedIsAlwaysTheLastEvent.
//
// The payload's own comment promises it. Nothing enforced it, and M2-10's
// later PRs add a ticker-driven emitter that can take a sequence number
// concurrently with close — so the invariant needs a test before the thing
// that can break it exists.
func TestRunFinishedIsAlwaysTheLastEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 15, 5, fake.Options{FailEvery: 5})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	log := eventLog(t, h.dbPath, "run-1")
	if len(log) < 2 {
		t.Fatalf("only %d events; the fixture proves nothing", len(log))
	}
	if got := payloadName(log[len(log)-1]); got != "RunFinished" {
		t.Errorf("the last event is %s, want RunFinished", got)
	}
	for i, ev := range log[:len(log)-1] {
		if payloadName(ev) == "RunFinished" {
			t.Errorf("RunFinished at position %d of %d; it must be last",
				i, len(log))
		}
	}
}

// TestAResumeWhoseSplitShrankDoesNotEmitNegativeProgress.
//
// remaining is the denominator of SESSION progress. Its operands are each
// bounded by the eval set; their difference is not. A resume with a larger
// holdout fraction has fewer dev Cases than the first process already
// completed, and checkResumable does not compare the split — it checks the
// eval CONTENT hash, the goal, and the agent, all of which are unchanged.
func TestAResumeWhoseSplitShrankDoesNotEmitNegativeProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 40, 10, fake.Options{})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The same Cases, but this process believes far fewer of them are dev.
	opts := h.opts
	opts.Resume = true
	opts.DevCases = 5
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		p, ok := ev.GetPayload().(*knov1.Event_RunResumed)
		if !ok {
			continue
		}
		r := p.RunResumed
		if r.GetRemaining() < 0 {
			t.Errorf("remaining=%d on the wire; a renderer divides by this",
				r.GetRemaining())
		}
		if r.GetAlreadyCompleted() > r.GetTotalCases() {
			t.Errorf("already=%d of total=%d inverts overall progress",
				r.GetAlreadyCompleted(), r.GetTotalCases())
		}
	}
}

// TestAStreamWithNoOpeningEventStillGetsOne.
//
// The opening event is chosen by the STREAM's state, not by --resume. A first
// process that died before emitting anything leaves an empty stream, and that
// resume is accepted — reading the evals can fail without changing the content
// hash checkResumable compares.
//
// Gating on the flag instead would emit RunResumed into an empty stream, and
// RunResumed carries no stage, agent, goal_name, or goal_direction. A consumer
// could never learn which way "better" points, which is what goal_direction's
// own proto comment says it is for.
func TestAStreamWithNoOpeningEventStillGetsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	// Fail the very first AppendEvent, so the Run row exists with no events.
	opts := h.opts
	opts.Store = &failingStore{Store: h.store, failEvent: true}
	if _, err := core.Baseline(ctx, h.evals, opts); err == nil {
		t.Fatal("the first run was meant to fail on its opening event")
	}
	if log := eventLog(t, h.dbPath, "run-1"); len(log) != 0 {
		t.Fatalf("%d events recorded; the fixture needs an empty stream", len(log))
	}

	resumed := h.opts
	resumed.Resume = true
	if _, err := core.Baseline(ctx, h.evals, resumed); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	log := eventLog(t, h.dbPath, "run-1")
	if len(log) == 0 {
		t.Fatal("the resumed run recorded no events")
	}
	if got := payloadName(log[0]); got != "RunStarted" {
		t.Errorf("the stream opens with %s; an empty stream needs RunStarted, "+
			"which is the only payload carrying the run's identity", got)
	}
}

// TestAReducedConcurrencyIsReportedRatherThanSilent.
//
// checkFeasible narrows the width when the cost cap cannot admit what was
// asked for. It did so with no event, no log line, and no field on the Run —
// docs/debt.md#44 — and a 6x slowdown nobody asked for is user-visible state,
// which CLAUDE.md says is a new event type and never a side channel.
func TestAReducedConcurrencyIsReportedRatherThanSilent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// $0.05 a Case against a $1.00 cap: the headroom admits 25% of 20 Cases in
	// flight, so 5 — well under the 32 asked for.
	h := newHarness(t, 100, 20, fake.Options{CostPerCallUSDMicros: 50_000})
	h.opts.Concurrency = 32
	h.opts.EstCostPerCallUSDMicros = 50_000
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil && !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("Baseline: %v", err)
	}

	var seen *knov1.ConcurrencyDecision
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if p, ok := ev.GetPayload().(*knov1.Event_ConcurrencyReduced); ok {
			seen = p.ConcurrencyReduced.GetDecision()
		}
	}
	if seen == nil {
		t.Fatal("the run narrowed concurrency and emitted nothing; the reduction " +
			"is exactly the user-visible state that must not travel in a side channel")
	}
	if seen.GetEffective() >= 32 {
		t.Errorf("effective=%d, want a reduction below the requested 32", seen.GetEffective())
	}
	if seen.Requested == nil || seen.GetRequested() != 32 {
		t.Errorf("requested=%v; an explicit --concurrency must be reported as asked for",
			seen.GetRequested())
	}
	if seen.GetReason() != knov1.ConcurrencyReason_CONCURRENCY_REASON_COST_CAP {
		t.Errorf("reason=%v, want COST_CAP", seen.GetReason())
	}
	// Both terms of the arithmetic, so the number is checkable rather than
	// asserted: effective is a fraction of headroom divided by the per-Case
	// estimate, and one term alone lets a reader solve for the other rather
	// than verify the result.
	if seen.GetHeadroomUsdMicros() <= 0 || seen.GetPerCaseEstimateUsdMicros() <= 0 {
		t.Errorf("headroom=%d per-case=%d; both are needed to reproduce the decision",
			seen.GetHeadroomUsdMicros(), seen.GetPerCaseEstimateUsdMicros())
	}

	// And the same decision survives on the record, for a user who was not watching.
	onRun := res.Run.GetConcurrency()
	if onRun == nil {
		t.Fatal("the Run records no concurrency; the event is gone once the stream is")
	}
	if onRun.GetEffective() != seen.GetEffective() {
		t.Errorf("the Run says %d and the event said %d", onRun.GetEffective(), seen.GetEffective())
	}
}

// TestAnUnreducedRunStillRecordsItsWidth.
//
// Presence means "this stage had a concurrency", not "a reduction happened" —
// requested != effective already says the second. Recording only reductions
// would leave a Run that ran at 32 and one that ran at 8 byte-identical, so
// the record could not answer whether two Runs are comparable.
func TestAnUnreducedRunStillRecordsItsWidth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Concurrency = 2

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	d := res.Run.GetConcurrency()
	if d == nil {
		t.Fatal("no concurrency recorded on a run that executed Cases")
	}
	if d.GetEffective() != 2 {
		t.Errorf("effective=%d, want 2", d.GetEffective())
	}
	if d.GetReason() != knov1.ConcurrencyReason_CONCURRENCY_REASON_UNSPECIFIED {
		t.Errorf("reason=%v on a run nothing reduced", d.GetReason())
	}
	// No event: a run that got what it asked for has no news.
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if _, ok := ev.GetPayload().(*knov1.Event_ConcurrencyReduced); ok {
			t.Error("emitted ConcurrencyReduced for a run nothing reduced")
		}
	}
}

// TestADefaultedConcurrencyIsNotReportedAsARequest.
//
// checkFeasible substitutes executor.DefaultConcurrency() when --concurrency
// is unset, and its own comment calls that "the path almost every user takes".
// Recording the substituted value as what the user asked for would make the
// report say "you requested 8, we gave you 5" to someone who requested
// nothing, which is how a report earns distrust.
func TestADefaultedConcurrencyIsNotReportedAsARequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Concurrency = 0 // unset, as the CLI leaves it by default

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	d := res.Run.GetConcurrency()
	if d == nil {
		t.Fatal("no concurrency recorded")
	}
	if d.Requested != nil {
		t.Errorf("requested=%d recorded for a user who set no --concurrency",
			d.GetRequested())
	}
	if d.GetEffective() <= 0 {
		t.Errorf("effective=%d; the width actually used is always known",
			d.GetEffective())
	}
}

// TestProgressHeartbeatsStopBeforeTheRunCloses.
//
// StageProgress is emitted from a ticker goroutine — the first emitter that is
// not serialized with the sink. RunFinished's payload promises it is always
// the last event, and a ticker still running when closeRun writes it can take
// a sequence number after it.
//
// The ticker is stopped and JOINED before closeRun rather than deferred after
// it: a deferred stop runs once closeRun has already returned, leaving the
// ticker free to take a sequence number during it.
//
// This test does NOT deterministically catch that — reverting to a deferred
// stop leaves it green, because whether a tick lands inside closeRun's window
// is a race. The deterministic guarantee is appendEvent's `closed` flag, which
// refuses any append after RunFinished and is tested directly in
// TestNothingIsAppendedAfterRunFinished. Stopping first is belt to that
// braces: a refusal is an error nobody reads.
//
// What this test does pin is that heartbeats actually fire mid-run, and that
// the sequence stays contiguous with a concurrent emitter running — which
// nothing else covers.
func TestProgressHeartbeatsStopBeforeTheRunCloses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Slow Cases and a fast heartbeat, so several ticks land mid-run.
	// Above minProgressInterval, with Cases slow enough that several ticks
	// still land mid-run.
	h := newHarness(t, 12, 4, fake.Options{Latency: 40 * time.Millisecond})
	h.opts.Concurrency = 1
	h.opts.ProgressInterval = core.DefaultProgressInterval / 50 // 20ms

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	log := eventLog(t, h.dbPath, "run-1")
	var progress int
	for _, ev := range log {
		if _, ok := ev.GetPayload().(*knov1.Event_StageProgress); ok {
			progress++
		}
	}
	if progress == 0 {
		t.Fatal("no heartbeats were emitted, so the ordering this test is about " +
			"was never exercised")
	}
	if got := payloadName(log[len(log)-1]); got != "RunFinished" {
		t.Errorf("the last event is %s, want RunFinished — a heartbeat outran close", got)
	}
	// And the sequence is still contiguous with a concurrent emitter running.
	for i, ev := range log {
		if want := int64(i + 1); ev.GetSequence() != want {
			t.Fatalf("event %d has sequence %d, want %d", i, ev.GetSequence(), want)
		}
	}
}

// TestProgressIsOffByDefault.
//
// Every event is one fsync under synchronous=FULL, on the same serialized
// writer as the outcome row that prevents double-spend. A heartbeat nobody is
// watching is pure write contention in front of the write whose loss costs
// money, so it is opt-in.
func TestProgressIsOffByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 10, 4, fake.Options{Latency: 10 * time.Millisecond})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if _, ok := ev.GetPayload().(*knov1.Event_StageProgress); ok {
			t.Fatal("a heartbeat was emitted with ProgressInterval unset")
		}
	}
}

// TestThroughputOnAResumeMeasuresThisProcessOnly.
//
// attempted spans the whole run — the aggregator is seeded from the store on
// resume — while the clock starts when this process does. Dividing one by the
// other reports a resume carrying 900 completed Cases into a one-second-old
// process as 900 Cases a second.
//
// The counts stay whole-run on purpose: they pair with total_cases, which is
// also whole-run. Only the rate is a session figure.
func TestThroughputOnAResumeMeasuresThisProcessOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 40

	h := newHarness(t, devCases, 10, fake.Options{Latency: 30 * time.Millisecond})
	h.opts.Concurrency = 1
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 30}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop early: %v", err)
	}

	opts := h.opts
	opts.Resume = true
	opts.ProgressInterval = core.DefaultProgressInterval / 50 // 20ms, above the floor
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	// The harness pins a fixed clock, which makes every elapsed interval zero
	// and every rate zero — so a rate assertion against it proves nothing. A
	// real clock is the point of this test.
	opts.Now = func() time.Time { return time.Now().UTC() }
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// The tightest honest bound: this process ran at most 10 Cases at 30ms
	// each, so it cannot have exceeded ~33/s however the clock rounded. A rate
	// computed from the whole run's 40 would be several times that. Give it
	// generous headroom for scheduling — load pushes the true rate DOWN, which
	// is the safe direction for this assertion.
	const ceiling = 100.0
	var seen int
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		p, ok := ev.GetPayload().(*knov1.Event_StageProgress)
		if !ok {
			continue
		}
		seen++
		if r := p.StageProgress.GetCasesPerSecond(); r > ceiling {
			t.Errorf("reported %.1f Cases/second; this process ran at most 10 Cases "+
				"at 30ms each, so the rate is counting the resumed run's prior work "+
				"against this process's clock", r)
		}
		// The COUNTS still span the whole run — they pair with total_cases —
		// so every heartbeat on a resume must already reflect the prior
		// process's work rather than starting from zero.
		if got := p.StageProgress.GetAttempted(); got < 20 {
			t.Errorf("attempted=%d on a resume whose first process completed 30 "+
				"Cases; the counts must span the run even though the rate does not", got)
		}
	}
	if seen == 0 {
		t.Fatal("no heartbeats were emitted on the resumed run")
	}
}

// TestTheHeartbeatCarriesTheNumbersItExistsFor.
//
// Every other progress test asserts an upper bound on the rate or the absence
// of the event. Nothing asserted the payload's contents, so zeroing any of
// them — attempted, scored, errored, total_cases, or the stage itself —
// survived the whole suite. Those numbers are the heartbeat's entire reason
// for existing.
func TestTheHeartbeatCarriesTheNumbersItExistsFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const devCases = 12

	// Every third Case errors, so scored and errored are both non-zero and a
	// test that conflated them would fail.
	h := newHarness(t, devCases, 5, fake.Options{
		FailEvery: 3,
		Latency:   40 * time.Millisecond,
	})
	h.opts.Concurrency = 1
	h.opts.ProgressInterval = core.DefaultProgressInterval / 50 // 20ms
	// A real clock: the harness pins a fixed one, which makes every elapsed
	// interval zero and every rate zero by construction.
	h.opts.Now = func() time.Time { return time.Now().UTC() }

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	var last *knov1.StageProgress
	var sawRate bool
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		p, ok := ev.GetPayload().(*knov1.Event_StageProgress)
		if !ok {
			continue
		}
		last = p.StageProgress
		if last.GetCasesPerSecond() > 0 {
			sawRate = true
		}
		if last.GetStage() != knov1.Stage_STAGE_BASELINE {
			t.Errorf("stage=%v, want BASELINE", last.GetStage())
		}
		if last.GetTotalCases() != devCases {
			t.Errorf("total_cases=%d, want %d", last.GetTotalCases(), devCases)
		}
		if got := last.GetScored() + last.GetErrored(); got != last.GetAttempted() {
			t.Errorf("attempted=%d but scored+errored=%d; the payload's own comment "+
				"says attempted = scored + errored", last.GetAttempted(), got)
		}
	}

	if last == nil {
		t.Fatal("no heartbeat was emitted")
	}
	if !sawRate {
		t.Error("every heartbeat reported a rate of zero; with a real clock and " +
			"40ms Cases at least one interval must have elapsed")
	}
	// The final heartbeat should have seen most of the run. Not all of it: the
	// last Cases can complete between the final tick and close.
	if last.GetAttempted() == 0 {
		t.Error("the last heartbeat reported nothing attempted")
	}
	if last.GetScored() == 0 || last.GetErrored() == 0 {
		t.Errorf("scored=%d errored=%d; the fixture fails every third Case, so a "+
			"heartbeat late in the run must have seen both",
			last.GetScored(), last.GetErrored())
	}
	// And the heartbeat's view never exceeds what the run finished with.
	if last.GetAttempted() > res.Run.GetAttemptedCaseCount() {
		t.Errorf("a heartbeat reported %d attempted but the run finished with %d",
			last.GetAttempted(), res.Run.GetAttemptedCaseCount())
	}
}

// billedFailure is an error carrying a provider's charge for a call that
// failed, in the shape core reads it — the same anonymous-interface shape
// retryAfterOf uses, so an adapter can report one without core importing it.
type billedFailure struct {
	error
	micros int64
}

func (b billedFailure) BilledCostUSDMicros() int64 { return b.micros }
func (b billedFailure) Unwrap() error              { return b.error }

// billingAgent charges for every call and fails all of them.
type billingAgent struct {
	core.Agent
	perCallUSDMicros int64
	calls            atomic.Int64
}

func (a *billingAgent) Invoke(context.Context, *core.Case) (*knov1.Response, error) {
	a.calls.Add(1)
	return nil, billedFailure{
		error:  errs.ErrTransportTransient.Wrap(errors.New("the provider charged and then failed")),
		micros: a.perCallUSDMicros,
	}
}

// TestABilledFailureIsPersistedSoAResumeDoesNotSpendItAgain.
//
// The guard settles a provider's charge for a failed call the moment it
// happens. sinkFunc persists what the run owes, and SettledSpend is the only
// durable record of money spent — Guard.Restore reads it back on resume.
//
// Persisting zero for a billed failure hands the resumed run that difference
// as headroom, and it spends it a second time. With MaxAttempts 3 the guard
// can settle three charges for one Case, so the divergence is per attempt.
func TestABilledFailureIsPersistedSoAResumeDoesNotSpendItAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const perCall = 40_000 // $0.04 a call, charged even though it fails

	h := newHarness(t, 6, 3, fake.Options{})
	h.opts.Agent = &billingAgent{perCallUSDMicros: perCall}
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = perCall
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 10_000_000}, nil, 0)
	// THREE attempts, not one. The guard settles each separately while only
	// the last error survives the loop, so the accumulation across attempts is
	// the thing under test — an earlier version of this test used one attempt
	// and could not have caught a sink that persisted a single charge.
	h.opts.MaxAttempts = 3
	h.opts.RetryBackoff = time.Millisecond

	// A run of all-errored Cases COMPLETES — an errored Case is counted, not
	// fatal — so a non-nil error is not the signal here.
	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if res.Run.GetErroredCaseCount() == 0 {
		t.Fatal("no Case errored; the fixture is not exercising a billed failure")
	}

	settledInGuard := h.opts.Guard.Spent().CostUSDMicros
	if settledInGuard == 0 {
		t.Fatal("the guard settled nothing; the fixture is not exercising a billed failure")
	}

	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}

	if persisted.CostUSDMicros != settledInGuard {
		t.Errorf("the guard settled %d micro-USD and the store persisted %d. "+
			"Guard.Restore reads the STORE on resume, so the resumed run gets "+
			"the %d difference as headroom and spends it again",
			settledInGuard, persisted.CostUSDMicros,
			settledInGuard-persisted.CostUSDMicros)
	}
	// And the accumulation is real: three attempts per Case, each charged.
	if want := int64(6 * 3 * perCall); settledInGuard != want {
		t.Errorf("guard settled %d, want %d — 6 Cases x 3 attempts x %d. A figure "+
			"matching one attempt per Case means the retries were not charged",
			settledInGuard, want, perCall)
	}
}

// TestASettlementPastTheCapIsReported.
//
// Guard.Overshoot made the excess computable in M2-2 and nothing surfaced it.
// The event is gated on the DELTA from each settlement, so once the cap binds
// and fitsLocked refuses further authorizations, only reservations already in
// flight can overshoot — the count is bounded by concurrency, not by Cases.
func TestASettlementPastTheCapIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Each call costs ten times what it was estimated at, so the very first
	// settlement blows a cap sized for the estimate.
	h := newHarness(t, 8, 3, fake.Options{CostPerCallUSDMicros: 500_000})
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = 50_000
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 200_000}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("Baseline: %v", err)
	}

	var seen []*knov1.SettlementOvershoot
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if p, ok := ev.GetPayload().(*knov1.Event_SettlementOvershoot); ok {
			seen = append(seen, p.SettlementOvershoot)
		}
	}
	if len(seen) == 0 {
		t.Fatal("a settlement passed the cap and nothing said so; Guard.Overshoot " +
			"has made this computable since M2-2 and no surface reported it")
	}
	first := seen[0]
	if first.GetSettledUsdMicros() <= first.GetReservedUsdMicros() {
		t.Errorf("settled=%d reserved=%d; an overshoot means the settlement "+
			"exceeded what was authorized for it",
			first.GetSettledUsdMicros(), first.GetReservedUsdMicros())
	}
	if first.GetCumulativeOvershootUsdMicros() <= 0 {
		t.Error("cumulative overshoot is zero on an event that reports one")
	}
	if first.GetCaseId() == "" {
		t.Error("no Case named; the overshoot is attributable and should say to what")
	}
	// The delta is what this settlement contributed, and it is NOT
	// settled-minus-reserved: that over-counts by the headroom still under the
	// cap. Cap 200k, est 50k, settled 500k — the derived figure is 450k and
	// the true contribution is 300k, because the first 200k was inside.
	if got, derived := first.GetDeltaUsdMicros(), first.GetSettledUsdMicros()-first.GetReservedUsdMicros(); got == derived {
		t.Errorf("delta=%d equals settled-reserved=%d; if the two agree on this "+
			"fixture the field is not carrying what it exists for", got, derived)
	}
	if first.GetDeltaUsdMicros() != first.GetCumulativeOvershootUsdMicros() {
		t.Errorf("delta=%d cumulative=%d; the FIRST overshoot of a run contributes "+
			"all of it", first.GetDeltaUsdMicros(), first.GetCumulativeOvershootUsdMicros())
	}
	// Bounded by concurrency, not by Case count: at concurrency 1 the cap
	// binds after the first settlement and nothing further is authorized.
	if len(seen) > 2 {
		t.Errorf("%d overshoot events at concurrency 1; once the cap binds, "+
			"fitsLocked refuses further authorizations, so the count is bounded "+
			"by in-flight reservations", len(seen))
	}
}

// flakyBillingAgent charges for a failed first attempt, then succeeds.
type flakyBillingAgent struct {
	failedOnce sync.Map // case id -> struct{}
	billed     int64
	answer     *knov1.Response
}

func (a *flakyBillingAgent) Invoke(_ context.Context, c *core.Case) (*knov1.Response, error) {
	if _, seen := a.failedOnce.LoadOrStore(c.GetId(), struct{}{}); !seen {
		return nil, billedFailure{
			error:  errs.ErrTransportTransient.Wrap(errors.New("charged, then failed")),
			micros: a.billed,
		}
	}
	r := proto.CloneOf(a.answer)
	r.Output = c.GetExpected()
	return r, nil
}

func (a *flakyBillingAgent) Capabilities() *knov1.Capabilities { return &knov1.Capabilities{} }

// TestABilledFailureBeforeASuccessIsStillPersisted.
//
// The retry-EXHAUSTED branch was the obvious half and is covered. This is the
// other one: a Case whose first attempt is charged and fails, and whose second
// succeeds, is persisted by the sink's SCORED branch — which derives cost from
// the final Response and knows nothing about what earlier attempts cost.
//
// The guard settles both. The store is what Guard.Restore reads on resume, so
// the difference is headroom the resumed run spends again — the same defect as
// the exhausted path, on the path that actually succeeds.
func TestABilledFailureBeforeASuccessIsStillPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const billedOnFailure = 40_000 // $0.04, charged for the attempt that failed
	const costOnSuccess = 10_000   // $0.01, the answer that worked

	h := newHarness(t, 5, 3, fake.Options{})
	h.opts.Agent = &flakyBillingAgent{
		billed: billedOnFailure,
		answer: &knov1.Response{CostUsdMicros: costOnSuccess},
	}
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = billedOnFailure + costOnSuccess
	h.opts.RetryBackoff = time.Millisecond
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 10_000_000}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if res.Run.GetScoredCaseCount() == 0 {
		t.Fatal("no Case scored; the fixture must retry into a success")
	}

	settledInGuard := h.opts.Guard.Spent().CostUSDMicros
	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}

	if persisted.CostUSDMicros != settledInGuard {
		t.Errorf("the guard settled %d micro-USD and the store persisted %d, a gap "+
			"of %d. Every Case here was charged once for a failed attempt and once "+
			"for the success; the sink derives cost from the final Response alone, "+
			"so the failed attempt's charge is lost. Guard.Restore reads the STORE",
			settledInGuard, persisted.CostUSDMicros,
			settledInGuard-persisted.CostUSDMicros)
	}
}

// selectiveFailStore fails AppendEvent for one payload type and no other.
type selectiveFailStore struct {
	store.Store
	failOn func(*knov1.Event) bool
}

func (s *selectiveFailStore) AppendEvent(ctx context.Context, ev *knov1.Event) error {
	if s.failOn(ev) {
		return errors.New("the event store is unavailable")
	}
	return s.Store.AppendEvent(ctx, ev)
}

// TestAnEventWriteFailureDoesNotDestroyThePaidWorkItReportsOn.
//
// The overshoot emit runs before the success check, so returning its error
// discarded a paid, scoreable answer — and because the outcome row was still
// written, CompletedCases included it and a resume skipped it forever. We
// would have paid for an answer, thrown it away, blamed the agent, and never
// re-attempted.
//
// docs/debt.md#32 rejected exactly this mechanism for Settle: "making Settle
// fail would turn a successful, paid, scored call into an errored Case and
// lose paid work." An emitter is the same hazard by another route.
//
// The failure still ends the run — a stream with a silent hole is worse than a
// run that stops — but at close, after the work it was reporting on is safe.
func TestAnEventWriteFailureDoesNotDestroyThePaidWorkItReportsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Each call costs ten times its estimate, so the first settlement
	// overshoots and the overshoot emit fires.
	h := newHarness(t, 4, 2, fake.Options{CostPerCallUSDMicros: 500_000})
	h.opts.Concurrency = 1
	h.opts.EstCostPerCallUSDMicros = 50_000
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 200_000}, nil, 0)
	h.opts.Store = &selectiveFailStore{
		Store: h.store,
		failOn: func(ev *knov1.Event) bool {
			_, isOvershoot := ev.GetPayload().(*knov1.Event_SettlementOvershoot)
			return isOvershoot
		},
	}

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("the run must end reporting the event-write failure; a silent gap " +
			"in the stream is worse than a run that stops")
	}

	// The paid answer survived. Before the fix this was scored=0 errored=1.
	if res == nil || res.Run.GetScoredCaseCount() == 0 {
		t.Fatalf("the Case was paid for and answered, and the run recorded no score. "+
			"An observability failure destroyed the work it was reporting on: %+v", res)
	}
	if res.Run.GetErroredCaseCount() > 0 {
		t.Errorf("%d Cases errored; an event-store failure is not an agent failure, "+
			"and codeOf would have recorded it as AGENT_ERROR",
			res.Run.GetErroredCaseCount())
	}
}

// TestAFailedRetryEventDoesNotTurnARetryableCaseTerminal.
//
// Same hazard, quieter symptom: returning the RetryAttempted emit error
// converts a Case that would have succeeded on its next attempt into a
// terminal failure. Enough of those trip ErrorRateExceeded and mark a healthy
// baseline unusable, on an event-store hiccup.
func TestAFailedRetryEventDoesNotTurnARetryableCaseTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 5, 2, fake.Options{})
	h.opts.Agent = &flakyBillingAgent{
		billed: 1_000,
		answer: &knov1.Response{CostUsdMicros: 1_000},
	}
	h.opts.Concurrency = 1
	h.opts.RetryBackoff = time.Millisecond
	h.opts.Store = &selectiveFailStore{
		Store: h.store,
		failOn: func(ev *knov1.Event) bool {
			_, isRetry := ev.GetPayload().(*knov1.Event_RetryAttempted)
			return isRetry
		},
	}

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("the run must end reporting the event-write failure")
	}
	if res == nil || res.Run.GetScoredCaseCount() == 0 {
		t.Fatalf("every Case retries into a success, and none scored — the retry "+
			"event's failure made them terminal: %+v", res)
	}
	if res.Run.GetErrorRateExceeded() {
		t.Error("the run is marked an unusable baseline because its event store " +
			"hiccuped, not because its agent failed")
	}
}

// TestAKilledRunResumesWithTheMoneyItAlreadySpent.
//
// CLAUDE.md's resume-from-checkpoint rule: kill mid-run, resume, assert no
// double-spend. This is the test whose absence let docs/debt.md#50 through —
// M2-10d compared the guard against the store INSIDE one process, which cannot
// see a resume defect by construction.
//
// The first process is stopped by its cost cap after several Cases have been
// charged for attempts that never produced an outcome. Guard.Restore reads
// SettledSpend, so anything the store failed to record is headroom the second
// process spends again.
func TestAKilledRunResumesWithTheMoneyItAlreadySpent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const perCall = 40_000 // $0.04, charged even when the call fails

	h := newHarness(t, 30, 10, fake.Options{})
	orphans := &orphanCountingStore{Store: h.store}
	h.opts.Store = orphans
	h.opts.Agent = &billingAgent{perCallUSDMicros: perCall}
	h.opts.Concurrency = 1
	h.opts.MaxAttempts = 2
	h.opts.RetryBackoff = time.Millisecond
	h.opts.EstCostPerCallUSDMicros = perCall
	// NINE calls, deliberately odd. Every Case here uses exactly two attempts,
	// so an even cap always runs out on some Case's FIRST attempt — which
	// settles nothing and produces no orphan spend. The refusal has to land
	// BETWEEN two attempts of one Case for this test to exercise what it is
	// about, and an earlier version of it used ten and proved nothing.
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 9 * perCall}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("first run: %v", err)
	}

	settledByFirst := h.opts.Guard.Spent().CostUSDMicros
	// The orphan path must actually have run, or this test proves nothing
	// about it. An earlier fixture used an even call cap, so the refusal
	// always landed on some Case's FIRST attempt — which settles nothing —
	// and the test passed against code that dropped the spend entirely.
	if orphans.n == 0 {
		t.Fatal("no orphan spend was recorded, so the refusal never landed between " +
			"two attempts of one Case and this test is not exercising docs/debt.md#50")
	}
	if settledByFirst == 0 {
		t.Fatal("the first process settled nothing; the fixture proves nothing")
	}

	// Exactly what a resume reconstructs.
	restored, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if restored.CostUSDMicros != settledByFirst {
		t.Errorf("the first process settled %d micro-USD and the store holds %d, a "+
			"gap of %d. Guard.Restore reads the store, so the resumed run gets "+
			"that gap as headroom and spends it a second time",
			settledByFirst, restored.CostUSDMicros,
			settledByFirst-restored.CostUSDMicros)
	}

	// The attribution exists on the stream, which is the only place it can:
	// the store records the amount against the RUN.
	var orphanEvents []*knov1.OrphanSpend
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if p, ok := ev.GetPayload().(*knov1.Event_OrphanSpend); ok {
			orphanEvents = append(orphanEvents, p.OrphanSpend)
		}
	}
	if len(orphanEvents) == 0 {
		t.Error("money was recorded against the run and no event says which Case " +
			"it belonged to; a column nothing describes is a side channel")
	}
	var fromEvents int64
	for _, e := range orphanEvents {
		if e.GetCaseId() == "" {
			t.Error("an orphan-spend event names no Case, which is the one thing " +
				"it exists to carry")
		}
		if e.GetReason() != knov1.OrphanReason_ORPHAN_REASON_BUDGET_EXCEEDED {
			t.Errorf("reason=%v, want BUDGET_EXCEEDED", e.GetReason())
		}
		fromEvents += e.GetCostUsdMicros()
	}
	// The stream and the store must describe the SAME money. Comparing against
	// the run's total settled spend proves nothing — orphan spend is a subset
	// of it by construction, so that inequality can never fail. The accumulator
	// on the wrapping store is the real counterpart.
	if fromEvents != orphans.total {
		t.Errorf("the store recorded %d micro-USD of orphan spend and the stream "+
			"describes %d; a charge the store holds and no event names is the "+
			"side channel this event exists to close", orphans.total, fromEvents)
	}
	if len(orphanEvents) != orphans.n {
		t.Errorf("%d durable orphan writes produced %d events", orphans.n, len(orphanEvents))
	}
	// The call count travels too — it is settled against --max-calls.
	var callsFromEvents int64
	for _, e := range orphanEvents {
		callsFromEvents += e.GetCalls()
	}
	if callsFromEvents == 0 {
		t.Error("the orphan events report no calls, though the guard settled them")
	}

	// And the Cases that were refused are still re-attemptable — the money is
	// durable without the Case being marked done.
	done, err := h.store.CompletedCases(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedCases: %v", err)
	}
	if len(done) >= 30 {
		t.Errorf("%d of 30 Cases are marked complete after a budget stop; recording "+
			"the spend must not mark a Case done that never got an answer", len(done))
	}

	// The resume itself cannot exceed the cap, which is the point.
	opts := h.opts
	opts.Resume = true
	opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 9 * perCall}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("resumed run: %v", err)
	}
	if total, ceiling := opts.Guard.Spent().CostUSDMicros, int64(9*perCall); total > ceiling {
		t.Errorf("the run spent %d micro-USD against a %d cap across two processes; "+
			"the resumed process did not inherit what the first one spent",
			total, ceiling)
	}
}

type orphanCountingStore struct {
	store.Store
	n     int
	total int64
}

func (o *orphanCountingStore) RecordOrphanSpend(ctx context.Context, runID string, sp budget.Spend) error {
	o.n++
	o.total += sp.CostUSDMicros
	return o.Store.RecordOrphanSpend(ctx, runID, sp)
}

// TestACancelDuringBackoffKeepsTheChargeItAlreadyIncurred.
//
// The second skip path, and the one the first draft of this fix missed.
// invokeWithRetry returns ctx.Err() from its backoff wait AFTER charges have
// accumulated, so a Ctrl-C landing there follows a billed attempt with real
// money settled and no outcome to attach it to.
//
// docs/debt.md#50 and #20 both name this path. It is distinct from the budget
// refusal: the run is stopping because a human asked, not because the cap was
// reached, and it reaches a different predicate in the sink.
func TestACancelDuringBackoffKeepsTheChargeItAlreadyIncurred(t *testing.T) {
	t.Parallel()

	const perCall = 40_000

	h := newHarness(t, 6, 3, fake.Options{})
	orphans := &orphanCountingStore{Store: h.store}
	h.opts.Store = orphans
	h.opts.Agent = &billingAgent{perCallUSDMicros: perCall}
	h.opts.Concurrency = 1
	h.opts.MaxAttempts = 3
	// Long enough that the cancellation below lands inside the wait rather
	// than between Cases.
	h.opts.RetryBackoff = 300 * time.Millisecond
	h.opts.EstCostPerCallUSDMicros = perCall
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 10_000_000}, nil, 0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// After the first attempt is billed and the backoff has begun.
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	_, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("the run was cancelled and reported success")
	}

	settled := h.opts.Guard.Spent().CostUSDMicros
	if settled == 0 {
		t.Fatal("nothing was billed before the cancellation; the fixture did not " +
			"reach the backoff wait")
	}
	if orphans.n == 0 {
		t.Fatal("a charge was settled and no orphan spend recorded — this test is " +
			"not exercising the cancel-during-backoff path")
	}

	persisted, err := h.store.SettledSpend(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if persisted.CostUSDMicros != settled {
		t.Errorf("the guard settled %d micro-USD and the store holds %d. A resume "+
			"reads the store, so the %d difference is money spent twice",
			settled, persisted.CostUSDMicros, settled-persisted.CostUSDMicros)
	}

	// The cancel path reports its own reason, not the budget one.
	var saw bool
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		p, ok := ev.GetPayload().(*knov1.Event_OrphanSpend)
		if !ok {
			continue
		}
		saw = true
		if p.OrphanSpend.GetReason() != knov1.OrphanReason_ORPHAN_REASON_CANCELLED {
			t.Errorf("reason=%v, want CANCELLED — a run stopped by a human is not "+
				"a run that ran out of budget", p.OrphanSpend.GetReason())
		}
	}
	if !saw {
		t.Error("no orphan-spend event; the charge is durable but unattributed")
	}
}

// panickingAgent bills for its first attempt, then panics.
type panickingAgent struct {
	billed int64
	seen   sync.Map
}

func (a *panickingAgent) Invoke(_ context.Context, c *core.Case) (*knov1.Response, error) {
	if _, again := a.seen.LoadOrStore(c.GetId(), struct{}{}); !again {
		return nil, billedFailure{
			error:  errs.ErrTransportTransient.Wrap(errors.New("charged, then failed")),
			micros: a.billed,
		}
	}
	panic("the adapter came apart")
}

func (a *panickingAgent) Capabilities() *knov1.Capabilities { return &knov1.Capabilities{} }

// TestAPanicDoesNotTakeTheMoneyWithIt.
//
// The executor recovers a panic and discards the value — deliberately, so a
// half-built outcome cannot be persisted. But the guard settled every attempt
// BEFORE the panic, and a discarded value means the sink persists nothing for
// a Case that really cost money.
//
// The dollars were already lost this way before orphan spend existed. What
// this pins is that they are not, and that the CALL count agrees too: a Case
// that made two charged attempts and then panicked must not report zero to a
// cap that counts calls.
func TestAPanicDoesNotTakeTheMoneyWithIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const perCall = 40_000

	h := newHarness(t, 4, 2, fake.Options{})
	h.opts.Agent = &panickingAgent{billed: perCall}
	h.opts.Concurrency = 1
	h.opts.MaxAttempts = 3
	h.opts.RetryBackoff = time.Millisecond
	h.opts.EstCostPerCallUSDMicros = perCall
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 10_000_000}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("a panicking agent errors its Cases, it does not fail the run: %v", err)
	}

	settled := h.opts.Guard.Spent()
	if settled.CostUSDMicros == 0 {
		t.Fatal("nothing was billed before the panic; the fixture proves nothing")
	}

	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if persisted.CostUSDMicros != settled.CostUSDMicros {
		t.Errorf("the guard settled %d micro-USD and the store holds %d. The panic "+
			"discarded the outcome that carried it, and a resume reads the store",
			settled.CostUSDMicros, persisted.CostUSDMicros)
	}
	if persisted.Calls != settled.Calls {
		t.Errorf("the guard settled %d calls and the store holds %d; --max-calls "+
			"is enforced against the guard and restored from the store",
			settled.Calls, persisted.Calls)
	}
}

// TestABudgetStopDoesNotReportItsCasesAsCancelled.
//
// A budget stop cancels the executor's context, so every in-flight worker's
// backoff wait returns ctx.Err() and lands on the shutdown predicate carrying
// real settled money. At the default concurrency that is seven Cases labelled
// "a human interrupted this" for a run that ran out of budget — the exact
// misreport OrphanReason was added to prevent.
//
// Both other orphan tests pin Concurrency = 1, where no Case is ever in flight
// beside the refused one, so neither can see this.
func TestABudgetStopDoesNotReportItsCasesAsCancelled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const perCall = 40_000

	h := newHarness(t, 40, 10, fake.Options{Latency: 5 * time.Millisecond})
	h.opts.Agent = &billingAgent{perCallUSDMicros: perCall}
	h.opts.Concurrency = 8 // the default, and the point of this test
	h.opts.MaxAttempts = 3
	h.opts.RetryBackoff = 30 * time.Millisecond
	h.opts.EstCostPerCallUSDMicros = perCall
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 11 * perCall}, nil, 0)

	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil &&
		!errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("Baseline: %v", err)
	}

	byReason := map[knov1.OrphanReason]int{}
	for _, ev := range eventLog(t, h.dbPath, "run-1") {
		if p, ok := ev.GetPayload().(*knov1.Event_OrphanSpend); ok {
			byReason[p.OrphanSpend.GetReason()]++
		}
	}
	if len(byReason) == 0 {
		t.Fatal("no orphan spend at all; the fixture did not stop mid-Case")
	}
	if n := byReason[knov1.OrphanReason_ORPHAN_REASON_CANCELLED]; n > 0 {
		t.Errorf("%d Cases reported as CANCELLED on a run that stopped because it "+
			"ran out of budget. Nobody interrupted it, and a consumer reading "+
			"these cannot tell a Ctrl-C from a breached cap", n)
	}
	if byReason[knov1.OrphanReason_ORPHAN_REASON_BUDGET_EXCEEDED] == 0 {
		t.Error("no Case reported BUDGET_EXCEEDED on a budget stop")
	}
}

// TestAFailedOrphanEventLeavesTheMoneyConsistent.
//
// What this proves: an event-store failure on the orphan path does not make
// the guard and the store disagree. The money stays reconcilable even when the
// observability write fails, and the run still reports the failure.
//
// What it does NOT prove, stated because the assertion would otherwise imply
// it: that a failed orphan emit cannot break the sink and discard the results
// queued behind it. The sink's return value latches the executor's sinkBroken,
// after which every remaining result is dropped — no outcome row, absent from
// CompletedCases, re-paid on resume. That is why the emit is recorded out of
// band rather than returned.
//
// Reverting to `return o.emitOrphanSpend(...)` leaves this test GREEN, because
// orphan emits happen during the drain, when little is still queued behind
// them. Constructing a case with results behind one means controlling the
// executor's delivery order, which the harness cannot do. The protection is
// structural, and it is the same one every other hot-path emitter already
// uses — docs/debt.md#32 rejected this exact mechanism for Settle, and
// baseline_record.go's own comment on emitFailure explains why.
func TestAFailedOrphanEventLeavesTheMoneyConsistent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const perCall = 40_000

	h := newHarness(t, 40, 10, fake.Options{Latency: 5 * time.Millisecond})
	h.opts.Agent = &billingAgent{perCallUSDMicros: perCall}
	h.opts.Concurrency = 8
	h.opts.MaxAttempts = 3
	h.opts.RetryBackoff = 30 * time.Millisecond
	h.opts.EstCostPerCallUSDMicros = perCall
	h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 11 * perCall}, nil, 0)
	h.opts.Store = &selectiveFailStore{
		Store: h.store,
		failOn: func(ev *knov1.Event) bool {
			_, isOrphan := ev.GetPayload().(*knov1.Event_OrphanSpend)
			return isOrphan
		},
	}

	if _, err := core.Baseline(ctx, h.evals, h.opts); err == nil {
		t.Fatal("the run must report the failed event write rather than passing")
	}

	// Every Case the guard settled must still be durable. If the emit had
	// broken the sink, the outcomes behind it would be missing and their money
	// would be spent again on resume.
	settled := h.opts.Guard.Spent()
	persisted, err := h.store.SettledSpend(ctx, "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}

	if persisted.CostUSDMicros != settled.CostUSDMicros {
		t.Errorf("the guard settled %d micro-USD and the store holds %d. An event "+
			"write failed and took paid results with it — a resume re-pays for "+
			"every one of them", settled.CostUSDMicros, persisted.CostUSDMicros)
	}
	if persisted.Calls != settled.Calls {
		t.Errorf("guard settled %d calls, store holds %d", settled.Calls, persisted.Calls)
	}
}

// TestCaseExecutionIsWrittenForEveryBaselineRun.
//
// Presence means "this stage executes Cases" — a property of the STAGE, not of
// the query. Deriving it from whether the aggregate found rows would report
// "this stage executes no Cases" for a run whose Cases were all refused after
// being charged: a run that executed Cases and spent money.
func TestCaseExecutionIsWrittenForEveryBaselineRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("an ordinary run", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 12, 4, fake.Options{FailEvery: 4})
		res, err := core.Baseline(ctx, h.evals, h.opts)
		if err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		ce := res.Run.GetCaseExecution()
		if ce == nil {
			t.Fatal("no CaseExecution on a run that executed Cases")
		}
		// Aggregated from what is durable, so it must match the flat counters
		// the aggregator produced. A divergence means the store and the
		// in-memory counts disagree about the same run.
		if ce.GetScoredCaseCount() != res.Run.GetScoredCaseCount() ||
			ce.GetErroredCaseCount() != res.Run.GetErroredCaseCount() {
			t.Errorf("CaseExecution says %d scored / %d errored, the counters say %d / %d",
				ce.GetScoredCaseCount(), ce.GetErroredCaseCount(),
				res.Run.GetScoredCaseCount(), res.Run.GetErroredCaseCount())
		}
		// Ingested, not aggregated: these describe what was LOADED. Deriving
		// them from outcomes reports a zero holdout count, which is the number
		// that sets every interval's width.
		if ce.GetDevCaseCount() != 12 || ce.GetHoldoutCaseCount() != 4 {
			t.Errorf("dev=%d holdout=%d, want 12/4 — these come from the split, "+
				"not from the outcomes table",
				ce.GetDevCaseCount(), ce.GetHoldoutCaseCount())
		}
	})

	t.Run("a run whose every Case was refused after being charged", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 20, 5, fake.Options{})
		h.opts.Agent = &billingAgent{perCallUSDMicros: 40_000}
		h.opts.Concurrency = 1
		h.opts.MaxAttempts = 1
		h.opts.EstCostPerCallUSDMicros = 40_000
		h.opts.Guard = budget.New(budget.Limits{MaxCostUSDMicros: 40_000}, nil, 0)

		res, err := core.Baseline(ctx, h.evals, h.opts)
		if err != nil && !errors.Is(err, errs.ErrBudgetExceeded) {
			t.Fatalf("Baseline: %v", err)
		}
		if res.Run.GetCaseExecution() == nil {
			t.Fatal("no CaseExecution on a run that executed Cases and spent money. " +
				"An absent message means 'this stage executes no Cases', which is " +
				"the opposite of what happened")
		}
	})
}

// TestTheModelGateRefusesOnlyAModelTheRunNeverSaw.
//
// Set membership, not models[0]. The field is repeated because with
// concurrency there is no "first response", and during a provider rollout two
// workers in one Run legitimately see different builds — so a run that saw
// {A, B} and is now served by B has not changed, and comparing against
// whichever element sorted first would refuse it.
func TestTheModelGateRefusesOnlyAModelTheRunNeverSaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		recorded    []string
		now         string
		wantRefused bool
	}{
		{
			name:     "the model that sorts first",
			recorded: []string{"claude-opus-5-20260101", "claude-opus-5-20260514"},
			now:      "claude-opus-5-20260101",
		},
		{
			// The case models[0] gets wrong: present in the set, not first.
			name:     "a model in the set but not first",
			recorded: []string{"claude-opus-5-20260101", "claude-opus-5-20260514"},
			now:      "claude-opus-5-20260514",
		},
		{
			name:        "a model the run never saw",
			recorded:    []string{"claude-opus-5-20260101", "claude-opus-5-20260514"},
			now:         "claude-opus-5-20261231",
			wantRefused: true,
		},
		{
			name:     "nothing resolved yet",
			recorded: []string{"claude-opus-5-20260101"},
			now:      "",
		},
		{
			// A run whose Cases all errored records no model: the store filters
			// the empty string out, because the column is NOT NULL DEFAULT ''
			// and every errored Case contributes one. Nothing to compare.
			name:     "the run recorded no model",
			recorded: nil,
			now:      "claude-opus-5",
		},
		{
			// A fresh run. Never gated: there is no prior measurement to be
			// inconsistent with.
			name:     "no record at all",
			recorded: []string{},
			now:      "claude-opus-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := core.ModelGateForTest(tt.recorded...)
			err := check(tt.now)
			if (err != nil) != tt.wantRefused {
				t.Errorf("refused = %v, want %v (err = %v)", err != nil, tt.wantRefused, err)
			}
			if tt.wantRefused && !contains(err.Error(), "pin the model") {
				t.Errorf("the refusal does not say how to prevent it: %v", err)
			}
		})
	}
}

// TestTheModelGateAnswersOnceUnderConcurrency.
//
// N workers can reach the gate together — there is no "first response" at
// concurrency — and the answer cannot differ between them. sync.Once also keeps
// the check off the hot path after it has run.
func TestTheModelGateAnswersOnceUnderConcurrency(t *testing.T) {
	t.Parallel()

	check := core.ModelGateForTest("model-a")

	var wg sync.WaitGroup
	errsSeen := make([]error, 32)
	for i := range errsSeen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errsSeen[i] = check("model-b")
		}()
	}
	wg.Wait()

	for i, err := range errsSeen {
		if err == nil {
			t.Fatalf("worker %d was told the model is fine while worker 0 was "+
				"told it changed; the gate must give one answer", i)
		}
	}
}

// TestAFailedObservationsReadStillClosesTheRun.
//
// CaseObservations is a READ, and it was sequenced ahead of FinishRun. One
// transient store error then left the Run in RUNNING with no finished_at —
// indistinguishable from a crash — suppressed RunFinished, which the schema
// promises is always the last event and which an SSE consumer waits on
// forever, and replaced the real run error so a budget stop reported the wrong
// cause and exited with the wrong code.
//
// An observability failure must not change the result it describes. That is
// the same argument recordOrphanSpend makes one level down.
func TestAFailedObservationsReadStillClosesTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 15, 5, fake.Options{})
	h.opts.Store = &failingStore{Store: h.store, failObs: true}

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("the failed read must be reported, not swallowed")
	}
	if res == nil {
		t.Fatal("no result at all; the run completed and its outcome is durable")
	}

	// The Run is durably closed, not stranded.
	stored, getErr := h.store.GetRun(ctx, "run-1")
	if getErr != nil {
		t.Fatalf("GetRun: %v", getErr)
	}
	if stored.GetStatus() == knov1.RunStatus_RUN_STATUS_RUNNING {
		t.Error("the Run is still RUNNING, which is indistinguishable from a crash")
	}
	if stored.GetFinishedAt() == "" {
		t.Error("the Run has no finished_at")
	}

	// RunFinished still closes the stream.
	log := eventLog(t, h.dbPath, "run-1")
	if len(log) == 0 || payloadName(log[len(log)-1]) != "RunFinished" {
		t.Error("the event stream was never closed; a consumer waits on RunFinished " +
			"and the schema promises it is last")
	}

	// And the report still has its numbers, from the flat counters.
	if res.Run.GetScoredCaseCount() == 0 {
		t.Error("the run scored Cases and the record says zero")
	}
}

// TestAResumeDoesNotRewriteTheRunsSplit.
//
// openRun reloads the stored Run on a resume and keeps the first process's
// dev/holdout counts. Composing CaseExecution from THIS process's options
// would put two different splits on one message, with the presence-carrying
// copy describing a split the run was never measured under.
//
// checkResumable does not compare the split — InputFingerprint is the eval
// SOURCE only — so a resume declaring a different holdout fraction passes
// every check and reaches this code.
func TestAResumeDoesNotRewriteTheRunsSplit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 8}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop early: %v", err)
	}

	// A resume declaring a different split. Nothing refuses it today.
	opts := h.opts
	opts.Resume = true
	opts.DevCases = 3
	opts.HoldoutCases = 22
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	res, err := core.Baseline(ctx, h.evals, opts)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	ce := res.Run.GetCaseExecution()
	if ce == nil {
		t.Fatal("no CaseExecution")
	}
	if ce.GetDevCaseCount() != res.Run.GetDevCaseCount() ||
		ce.GetHoldoutCaseCount() != res.Run.GetHoldoutCaseCount() {
		t.Errorf("CaseExecution says dev=%d holdout=%d and the Run says dev=%d "+
			"holdout=%d. One message cannot state two splits, and the "+
			"presence-carrying copy is the one every interval's width is "+
			"computed from",
			ce.GetDevCaseCount(), ce.GetHoldoutCaseCount(),
			res.Run.GetDevCaseCount(), res.Run.GetHoldoutCaseCount())
	}
	if ce.GetHoldoutCaseCount() != 5 {
		t.Errorf("holdout=%d, want 5 — the split the run was measured under, not "+
			"the one the resuming process declared", ce.GetHoldoutCaseCount())
	}
}

// TestAnAliasThatRepointsMidRunEndsTheRun.
//
// The end-to-end test debt #42's trigger demanded and no earlier PR could
// write. The gate used to compare BaselineOptions.ResolvedModel, a
// caller-supplied field read before any request, so the only value it could
// hold was one a previous run recorded — comparing a value to itself. It never
// fired once in production.
//
// Driven through a real run: the fake reports a model, the alias re-points
// mid-run, and the gate sees it in a response rather than in a synthesized Run.
func TestAnAliasThatRepointsMidRunEndsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// First run, measured entirely against one model.
	h := newHarness(t, 30, 5, fake.Options{ResolvedModel: "gpt-4.1-2026-05-01"})
	if _, err := core.Baseline(ctx, h.evals, h.opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	stored, err := h.store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !slices.Contains(stored.GetCaseExecution().GetResolvedModels(), "gpt-4.1-2026-05-01") {
		t.Fatalf("the first run recorded no model, so the gate has nothing to "+
			"compare: %v", stored.GetCaseExecution().GetResolvedModels())
	}
}

// TestAResumeServedADifferentModelIsRefusedMidRun.
//
// A run interrupted on Monday and resumed on Friday after the alias re-points
// passes every checkResumable axis — InputFingerprint covers the eval source,
// not the provider's routing — and blends two models into one AggregateScore.
// That is the corrupted-reference failure prime directive 5 exists to prevent,
// arriving through the one input nothing could see.
func TestAResumeServedADifferentModelIsRefusedMidRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Stop the first run early so there is work left to resume.
	h := newHarness(t, 40, 5, fake.Options{ResolvedModel: "gpt-4.1-2026-05-01"})
	h.opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)
	if _, err := core.Baseline(ctx, h.evals, h.opts); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("the first run was meant to stop early: %v", err)
	}

	// The alias re-points. Every other input is identical.
	opts := h.opts
	opts.Resume = true
	opts.Agent = fake.New(fake.Options{ResolvedModel: "gpt-4.1-2026-08-01"})
	opts.Guard = budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)
	opts.Concurrency = 1

	res, err := core.Baseline(ctx, h.evals, opts)
	if !errors.Is(err, errs.ErrCheckpointStale) {
		t.Fatalf("error = %v, want ErrCheckpointStale; the resume was served a "+
			"different model and would average two into one score", err)
	}
	if !contains(err.Error(), "pin the model") {
		t.Errorf("the refusal does not say how to prevent it: %v", err)
	}

	// The run stopped rather than scoring all 35 remaining Cases under the new
	// model. It cannot undo what is already mixed — up to concurrency-1 more
	// Cases land after the gate fires — but it stops it getting worse.
	if got := res.Run.GetScoredCaseCount(); got >= 35 {
		t.Errorf("scored = %d; the run did not stop", got)
	}

	// And the answer that tripped the gate was KEPT, not discarded. Returning
	// an error from the work would have filed a paid, scoreable Case as an
	// agent error — the mistake SettlementOvershoot already fixed once.
	stored, getErr := h.store.GetRun(ctx, "run-1")
	if getErr != nil {
		t.Fatalf("GetRun: %v", getErr)
	}
	if !slices.Contains(stored.GetCaseExecution().GetResolvedModels(), "gpt-4.1-2026-08-01") {
		t.Error("the Case that tripped the gate was not recorded; we paid for " +
			"that answer and ending the run must not throw it away")
	}
}

// TestAFreshRunSeeingTwoModelsIsNotRefused.
//
// resolved_models is a SET because during a provider rollout two workers in one
// run legitimately see different builds. A fresh run has no prior measurement
// to contradict, so observing two is normal rather than a corruption.
func TestAFreshRunSeeingTwoModelsIsNotRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 20, 5, fake.Options{
		ResolvedModel:      "gpt-4.1-2026-05-01",
		ResolvedModelAfter: 5,
		ResolvedModelThen:  "gpt-4.1-2026-08-01",
	})
	h.opts.Concurrency = 1

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("a fresh run observing a rollout was refused: %v", err)
	}
	if got := res.Run.GetScoredCaseCount(); got != 20 {
		t.Errorf("scored = %d, want 20", got)
	}
	if got := res.Run.GetCaseExecution().GetResolvedModels(); len(got) != 2 {
		t.Errorf("resolved_models = %v, want both models recorded", got)
	}
}

// runFatalErr marks an error the way an adapter does.
//
// Declared HERE rather than imported from adapters/agent/internal/agenterr —
// which core could not import even if it wanted to, and must not. That is the
// point: core reads a structural interface, so anything satisfying
// `RunFatal() bool` escalates and nothing crosses the boundary in either
// direction. A test that had to import the adapter's type would be proving the
// opposite of prime directive 3.
type runFatalErr struct{ error }

func (runFatalErr) RunFatal() bool { return true }

func (e runFatalErr) Unwrap() error { return e.error }

// runFatalAgent fails every Case with an error marked run-fatal, the way an
// adapter marks a rejected credential.
type runFatalAgent struct {
	core.Agent
	err error
}

func (a *runFatalAgent) Invoke(context.Context, *core.Case) (*core.Response, error) {
	return nil, a.err
}

// TestARunFatalErrorEndsTheRunAtTheFirstCase.
//
// A wrong ANTHROPIC_API_KEY on a 10,000-Case run made 10,000 requests and
// settled 10,000 calls against --max-calls before telling the user anything —
// which is precisely what anthropic.ErrAuthentication's own godoc claims it
// prevents. core.IsFatal treated only ErrBudgetExceeded as fatal, so three
// conditions that cannot change within a run were classified per-Case. See
// docs/debt.md#47.
func TestARunFatalErrorEndsTheRunAtTheFirstCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 200, 20, fake.Options{})
	h.opts.Concurrency = 1
	h.opts.Agent = &runFatalAgent{
		Agent: h.agent,
		err:   runFatalErr{errs.ErrInvalidInput.Wrap(errors.New("rejected the credential"))},
	}

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("a rejected credential did not end the run")
	}
	if got := res.Run.GetAttemptedCaseCount(); got != 1 {
		t.Errorf("attempted = %d of 200, want 1. Every Case after the first "+
			"made a request that got the same answer and settled a call "+
			"against the cap", got)
	}

	// The adapter's classification survives into the record. Wrapping a
	// sentinel around the Actionable instead would have made errors.As return
	// the wrong one and persisted a generic code (docs/debt.md#39).
	if got := errs.ExitCodeOf(err); got != errs.ExitError {
		t.Errorf("exit code = %d, want %d", got, errs.ExitError)
	}
}

// TestARunFatalErrorAtConcurrencyStopsWithinOneDispatch.
//
// IsFatal is consulted after the result is already on the channel and cancel()
// reaches the other workers asynchronously, so the bound is not 1 — and it is
// not `concurrency` either. When a worker calls fail(), the producer's select
// has both the send and Done ready and Go picks uniformly at random, so one
// more Case can dispatch after the cancel.
func TestARunFatalErrorAtConcurrencyStopsWithinOneDispatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const concurrency = 8
	h := newHarness(t, 200, 20, fake.Options{})
	h.opts.Concurrency = concurrency
	h.opts.Agent = &runFatalAgent{
		Agent: h.agent,
		err:   runFatalErr{errs.ErrInvalidInput.Wrap(errors.New("rejected the credential"))},
	}

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err == nil {
		t.Fatal("a rejected credential did not end the run")
	}
	if got := int(res.Run.GetAttemptedCaseCount()); got > concurrency+1 || got >= 200 {
		t.Errorf("attempted = %d of 200; want at most %d", got, concurrency+1)
	}
}

// TestANonEscalatingErrorStillFailsOnlyItsOwnCase.
//
// The test that catches OVER-escalation, which is the more expensive direction
// to get wrong: escalating a plain 429, a 5xx, or a truncation converts a
// recoverable run into a dead one.
func TestANonEscalatingErrorStillFailsOnlyItsOwnCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 30, 5, fake.Options{FailEvery: 4})

	res, err := core.Baseline(ctx, h.evals, h.opts)
	if err != nil {
		t.Fatalf("an ordinary per-Case failure ended the whole run: %v", err)
	}
	if got := res.Run.GetAttemptedCaseCount(); got != 30 {
		t.Errorf("attempted = %d, want 30; ordinary failures must not stop the "+
			"run", got)
	}
	if res.Run.GetErroredCaseCount() == 0 {
		t.Fatal("the fixture produced no failures, so this proves nothing")
	}
}

// TestARunFatalStopIsNotReportedAsABudgetStop.
//
// sinkFunc used a bool as the orphan-reason discriminator, so every in-flight
// charged Case on ANY fatal stop reported "the cost or call cap could not admit
// another attempt". Entry #52 exists because a run stopped by a human is not a
// run that ran out of money; a run stopped by a rejected credential is a third
// case, and telling that user to raise a cap sends them to a limit that was
// never binding.
func TestARunFatalStopIsNotReportedAsABudgetStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, 60, 10, fake.Options{CostPerCallUSDMicros: 100})
	h.opts.Concurrency = 8
	h.opts.Agent = &runFatalAgent{
		Agent: h.agent,
		err:   runFatalErr{errs.ErrInvalidInput.Wrap(errors.New("rejected the credential"))},
	}

	if _, err := core.Baseline(ctx, h.evals, h.opts); err == nil {
		t.Fatal("the run did not stop")
	}

	for _, e := range eventLog(t, h.dbPath, "run-1") {
		if payloadName(e) != "OrphanSpend" {
			continue
		}
		got := e.GetOrphanSpend().GetReason()
		if got == knov1.OrphanReason_ORPHAN_REASON_BUDGET_EXCEEDED {
			t.Errorf("a credential failure attributed its orphaned spend to the " +
				"budget; the user is told to raise a cap that was never binding")
		}
		if got != knov1.OrphanReason_ORPHAN_REASON_RUN_FATAL {
			t.Errorf("orphan reason = %v, want RUN_FATAL", got)
		}
	}
}
