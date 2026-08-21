package executor_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

// TestMain installs the process-wide leak check.
//
// This package is where goroutines actually proliferate — producer, workers,
// sink — so per docs/debt.md#18 it gets the VerifyTestMain treatment rather
// than relying on coretest's opt-in per-call check.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type output struct{ id string }

// caseID identifies an item for the executor, which is generic over its item
// type and so cannot know what a Case is.
func caseID(item any) string {
	c, ok := item.(*core.Case)
	if !ok {
		return ""
	}
	return c.GetId()
}

// staticCases yields n Cases, allocating a fresh one each time.
func staticCases(n int) iter.Seq2[*core.Case, error] {
	return func(yield func(*core.Case, error) bool) {
		for i := range n {
			if !yield(&core.Case{Id: fmt.Sprintf("case-%03d", i), Split: knov1.Split_SPLIT_DEV}, nil) {
				return
			}
		}
	}
}

// reusingCases yields Cases from ONE buffer, mutating it in place.
//
// This is the adversarial producer the borrow contract exists for: legal under
// Ring-0, which says yielded values are borrowed for a single iteration, and
// fatal to any consumer that holds the pointer past its turn.
func reusingCases(n int) iter.Seq2[*core.Case, error] {
	return func(yield func(*core.Case, error) bool) {
		buf := &core.Case{Split: knov1.Split_SPLIT_DEV}
		for i := range n {
			buf.Id = fmt.Sprintf("case-%03d", i)
			buf.Input = fmt.Sprintf("input-%03d", i)
			if !yield(buf, nil) {
				return
			}
		}
	}
}

func echoWork(_ context.Context, c *core.Case) (*output, error) {
	return &output{id: c.GetId()}, nil
}

func collectSink(mu *sync.Mutex, got *[]executor.Result[*core.Case, output]) executor.SinkFunc[*core.Case, output] {
	return func(_ context.Context, r executor.Result[*core.Case, output]) error {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, r)
		return nil
	}
}

// TestEveryItemIsExecutedExactlyOnce is the baseline guarantee: nothing lost,
// nothing duplicated. A duplicate would inflate the denominator behind every
// later delta; a loss would shrink it.
func TestEveryItemIsExecutedExactlyOnce(t *testing.T) {
	t.Parallel()

	const n = 500
	var (
		mu  sync.Mutex
		got []executor.Result[*core.Case, output]
	)

	stats, err := executor.Run(context.Background(), staticCases(n),
		echoWork, collectSink(&mu, &got), executor.Options{Concurrency: 16, ID: caseID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Dispatched != n || stats.Succeeded != n {
		t.Errorf("stats = %+v, want %d dispatched and succeeded", stats, n)
	}

	seen := make(map[string]int, n)
	for _, r := range got {
		seen[r.Item.GetId()]++
	}
	if len(seen) != n {
		t.Errorf("recorded %d distinct items, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("item %s recorded %d times, want exactly 1", id, count)
		}
	}
}

// TestWorkersDoNotShareCaseMemory is debt 8 becoming a test.
//
// The Ring-0 contract says yielded values are borrowed for one iteration, so a
// producer is free to reuse its buffer. If the executor dispatched the yielded
// pointer instead of a clone, concurrent workers would read a Case that the
// producer is simultaneously rewriting — a data race, and silently wrong
// results even when it does not trip the detector.
//
// Run with -race, this fails loudly against a non-cloning executor.
func TestWorkersDoNotShareCaseMemory(t *testing.T) {
	t.Parallel()

	const n = 400
	var (
		mu  sync.Mutex
		got []executor.Result[*core.Case, output]
	)

	// The work reads the Case's fields and asserts they are self-consistent.
	// A shared buffer shows up as an id and input from different iterations.
	work := func(_ context.Context, c *core.Case) (*output, error) {
		id, input := c.GetId(), c.GetInput()
		// Give the producer a chance to advance while this worker holds the
		// pointer, which is precisely the window a clone closes.
		time.Sleep(time.Microsecond)
		if id != c.GetId() || input != c.GetInput() {
			return nil, fmt.Errorf("case mutated mid-work: %s/%s became %s/%s",
				id, input, c.GetId(), c.GetInput())
		}
		if want := "input-" + id[len("case-"):]; input != want {
			return nil, fmt.Errorf("case %s carries input %q, want %q", id, input, want)
		}
		return &output{id: id}, nil
	}

	stats, err := executor.Run(context.Background(), reusingCases(n),
		work, collectSink(&mu, &got), executor.Options{Concurrency: 16, ID: caseID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Failed != 0 {
		for _, r := range got {
			if r.Err != nil {
				t.Errorf("worker saw torn state: %v", r.Err)
			}
		}
	}
}

// TestSkipAvoidsRedispatch is how resume does not pay twice.
func TestSkipAvoidsRedispatch(t *testing.T) {
	t.Parallel()

	const n = 100
	var executed atomic.Int64
	work := func(ctx context.Context, c *core.Case) (*output, error) {
		executed.Add(1)
		return echoWork(ctx, c)
	}

	// Pretend the first 60 completed in an earlier run.
	done := make(map[string]struct{}, 60)
	for i := range 60 {
		done[fmt.Sprintf("case-%03d", i)] = struct{}{}
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), staticCases(n), work,
		collectSink(&mu, &got), executor.Options{
			Concurrency: 8,
			ID:          caseID,
			Skip:        func(id string) bool { _, ok := done[id]; return ok },
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Skipped != 60 || stats.Dispatched != 40 {
		t.Errorf("stats = %+v, want 60 skipped and 40 dispatched", stats)
	}
	if got := executed.Load(); got != 40 {
		t.Errorf("work ran %d times, want 40: a resumed run re-executed completed items", got)
	}
	for _, r := range got {
		if _, wasDone := done[r.Item.GetId()]; wasDone {
			t.Errorf("already-completed item %s was re-executed", r.Item.GetId())
		}
	}
}

// TestFatalSourceErrorDrainsInFlightWork covers the rule that keeps paid-for
// work from being thrown away.
//
// A yielded error is fatal, so iteration stops — but items already dispatched
// have had money spent on them. Their results must still be recorded, or a
// resumed run pays for them again.
func TestFatalSourceErrorDrainsInFlightWork(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("source exploded")
	const before = 50

	items := func(yield func(*core.Case, error) bool) {
		for i := range before {
			if !yield(&core.Case{Id: fmt.Sprintf("case-%03d", i), Split: knov1.Split_SPLIT_DEV}, nil) {
				return
			}
		}
		yield(nil, wantErr)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), items, echoWork,
		collectSink(&mu, &got), executor.Options{Concurrency: 8, ID: caseID})

	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the source's fatal error", err)
	}
	if stats.Succeeded != before {
		t.Errorf("recorded %d results, want all %d dispatched before the error; "+
			"work already paid for was discarded", stats.Succeeded, before)
	}
	if len(got) != before {
		t.Errorf("sink saw %d results, want %d", len(got), before)
	}
}

// TestWorkerErrorsAreRecordedNotFatal: one item failing must not end a run
// that has spent money on the rest.
func TestWorkerErrorsAreRecordedNotFatal(t *testing.T) {
	t.Parallel()

	const n = 100
	work := func(ctx context.Context, c *core.Case) (*output, error) {
		if c.GetId() == "case-042" {
			return nil, errors.New("provider returned 500")
		}
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), staticCases(n), work,
		collectSink(&mu, &got), executor.Options{Concurrency: 8, ID: caseID})
	if err != nil {
		t.Fatalf("one failing item ended the whole run: %v", err)
	}

	if stats.Failed != 1 || stats.Succeeded != n-1 {
		t.Errorf("stats = %+v, want 1 failed and %d succeeded", stats, n-1)
	}
	if len(got) != n {
		t.Errorf("sink saw %d results, want %d: a failed item was not recorded", len(got), n)
	}
}

// TestPanicInOneItemDoesNotKillTheRun.
//
// A panic in one item's work must not take down a run that has already spent
// money on hundreds of others. It is recorded as that item's failure.
func TestPanicInOneItemDoesNotKillTheRun(t *testing.T) {
	t.Parallel()

	const n = 50
	work := func(ctx context.Context, c *core.Case) (*output, error) {
		if c.GetId() == "case-007" {
			panic("nil map write, or some other programmer error")
		}
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), staticCases(n), work,
		collectSink(&mu, &got), executor.Options{Concurrency: 4, ID: caseID})
	if err != nil {
		t.Fatalf("a panic in one item ended the run: %v", err)
	}
	if stats.Failed != 1 || stats.Succeeded != n-1 {
		t.Errorf("stats = %+v, want 1 failed and %d succeeded", stats, n-1)
	}

	var panicked bool
	for _, r := range got {
		if r.Item.GetId() == "case-007" {
			panicked = r.Err != nil
		}
	}
	if !panicked {
		t.Error("the panicking item was not recorded as a failure")
	}
}

// TestCancellationStopsDispatchPromptly: after Ctrl-C, the run must stop
// handing out new work rather than draining a million-item source.
func TestCancellationStopsDispatchPromptly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var executed atomic.Int64

	work := func(ctx context.Context, c *core.Case) (*output, error) {
		if executed.Add(1) == 20 {
			cancel()
		}
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(ctx, staticCases(100_000), work,
		collectSink(&mu, &got), executor.Options{Concurrency: 4, ID: caseID})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled so the caller can tell an "+
			"interrupted run from a completed one", err)
	}
	if stats.Dispatched > 5_000 {
		t.Errorf("dispatched %d items after cancellation; the producer kept "+
			"handing out work with nobody to do it", stats.Dispatched)
	}
	t.Logf("dispatched %d of 100000 before stopping", stats.Dispatched)
}

// TestSinkFailureStopsTheRun.
//
// If results cannot be recorded, continuing spends money whose outcome nothing
// can persist — and a resumed run pays for it again. Stopping is the only
// honest response.
func TestSinkFailureStopsTheRun(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("disk full")
	var recorded atomic.Int64
	sink := func(_ context.Context, _ executor.Result[*core.Case, output]) error {
		if recorded.Add(1) == 10 {
			return wantErr
		}
		return nil
	}

	stats, err := executor.Run(context.Background(), staticCases(100_000),
		echoWork, sink, executor.Options{Concurrency: 4, ID: caseID})

	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the sink's failure", err)
	}
	if stats.Dispatched > 5_000 {
		t.Errorf("dispatched %d items after the sink failed; money was spent on "+
			"work that could not be recorded", stats.Dispatched)
	}
}

// TestConcurrencyIsBounded: the pool must not exceed its limit, since the real
// constraint is a provider's rate limit rather than this machine.
func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	const limit = 4
	var inFlight, peak atomic.Int64

	work := func(ctx context.Context, c *core.Case) (*output, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	if _, err := executor.Run(context.Background(), staticCases(200), work,
		collectSink(&mu, &got), executor.Options{Concurrency: limit}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrency %d exceeded the limit of %d", got, limit)
	}
}

// TestEmptySourceIsNotAnError: an eval set that yields nothing is a legitimate
// outcome, not a crash.
func TestEmptySourceIsNotAnError(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), staticCases(0), echoWork,
		collectSink(&mu, &got), executor.Options{ID: caseID})
	if err != nil {
		t.Fatalf("an empty source errored: %v", err)
	}
	if stats.Dispatched != 0 || len(got) != 0 {
		t.Errorf("stats = %+v with %d results, want nothing", stats, len(got))
	}
}

// TestMissingWorkOrSinkIsRefused rather than panicking mid-run.
func TestMissingWorkOrSinkIsRefused(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]

	if _, err := executor.Run(context.Background(), staticCases(1), executor.WorkFunc[*core.Case, output](nil),
		collectSink(&mu, &got), executor.Options{ID: caseID}); err == nil {
		t.Error("a nil work function was accepted")
	}
	if _, err := executor.Run(context.Background(), staticCases(1), echoWork,
		nil, executor.Options{ID: caseID}); err == nil {
		t.Error("a nil sink was accepted")
	}
}

// TestResultsRecordSuccessAndFailureDistinctly mirrors the split the store and
// the event stream keep: an item is done or failed, never both.
func TestResultsRecordSuccessAndFailureDistinctly(t *testing.T) {
	t.Parallel()

	ok := executor.Result[*core.Case, output]{Item: &core.Case{Id: "a"}, Value: &output{id: "a"}}
	bad := executor.Result[*core.Case, output]{Item: &core.Case{Id: "b"}, Err: errors.New("boom")}

	if !ok.Done() {
		t.Error("a result with no error does not report itself as done")
	}
	if bad.Done() {
		t.Error("a result carrying an error reports itself as done")
	}
}

// TestCancellationDoesNotDropPaidForResults is the regression test for the
// most damaging bug this package had.
//
// Workers used to send results through a select on the shutdown context. Once
// cancelled, that raced a ready-forever Done channel against a sink busy
// writing to disk, so on Ctrl-C most in-flight results were thrown away. Each
// one is work already performed and money already spent, and a result that
// never reaches the sink is never in CompletedCases — so a resumed run pays
// for it a second time.
//
// Every item a worker completes must reach the sink, cancelled or not.
func TestCancellationDoesNotDropPaidForResults(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var (
		mu        sync.Mutex
		recorded  []executor.Result[*core.Case, output]
		completed atomic.Int64
	)

	work := func(_ context.Context, c *core.Case) (*output, error) {
		// Every call here represents money spent, regardless of cancellation.
		n := completed.Add(1)
		if n == 25 {
			cancel()
		}
		return &output{id: c.GetId()}, nil
	}

	// A sink slow enough that a worker's send would lose a select race.
	sink := func(_ context.Context, r executor.Result[*core.Case, output]) error {
		time.Sleep(200 * time.Microsecond)
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, r)
		return nil
	}

	stats, err := executor.Run(ctx, staticCases(100_000), work, sink,
		executor.Options{Concurrency: 8, ID: caseID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	mu.Lock()
	got := len(recorded)
	mu.Unlock()

	if int64(got) != completed.Load() {
		t.Errorf("work completed %d items but only %d were recorded; %d paid-for "+
			"results were dropped and a resumed run would pay for them again",
			completed.Load(), got, completed.Load()-int64(got))
	}
	if stats.Recorded() != got {
		t.Errorf("stats report %d recorded, sink saw %d", stats.Recorded(), got)
	}
}

// TestStatsPartitionDispatched pins the invariant M1-5 uses to compute the
// Run's case counts.
//
// Dispatched was previously incremented before the handoff, so an item that
// lost the race to shutdown counted as dispatched without ever reaching a
// worker — and dropped results counted as dispatched without ever being
// recorded. Either way the denominator behind every later delta was wrong.
func TestStatsPartitionDispatched(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var completed atomic.Int64

	work := func(ctx context.Context, c *core.Case) (*output, error) {
		if completed.Add(1) == 30 {
			cancel()
		}
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, _ := executor.Run(ctx, staticCases(50_000), work,
		collectSink(&mu, &got), executor.Options{Concurrency: 8, ID: caseID})

	if stats.Dispatched != stats.Recorded() {
		t.Errorf("stats = %+v: Dispatched (%d) != Succeeded+Failed (%d); a caller "+
			"computing case counts from these would report a denominator that "+
			"never happened", stats, stats.Dispatched, stats.Recorded())
	}
}

// TestIsFatalStopsTheRun covers the path M1-5 needs for budget exhaustion.
//
// Without it, a budget-exhausted run dispatches every remaining item only to
// have each denied — completing with thousands of failures instead of stopping
// as budget-stopped and resumable.
func TestIsFatalStopsTheRun(t *testing.T) {
	t.Parallel()

	errBudget := errors.New("budget exceeded")
	var attempts atomic.Int64

	work := func(ctx context.Context, c *core.Case) (*output, error) {
		if attempts.Add(1) > 20 {
			return nil, errBudget
		}
		return echoWork(ctx, c)
	}

	var mu sync.Mutex
	var got []executor.Result[*core.Case, output]
	stats, err := executor.Run(context.Background(), staticCases(100_000), work,
		collectSink(&mu, &got), executor.Options{
			Concurrency: 4,
			ID:          caseID,
			IsFatal:     func(err error) bool { return errors.Is(err, errBudget) },
		})

	if !errors.Is(err, errBudget) {
		t.Errorf("error = %v, want the fatal work error so the caller can report "+
			"budget-stopped rather than failed", err)
	}
	if stats.Dispatched > 5_000 {
		t.Errorf("dispatched %d items after the budget was exhausted", stats.Dispatched)
	}
	// The item that triggered the stop is still recorded: it happened.
	if stats.Recorded() != stats.Dispatched {
		t.Errorf("stats = %+v: the fatal item's own result was not recorded", stats)
	}
}

// TestSinkPanicDoesNotKillTheProcess.
//
// The sink runs on its own goroutine, so an unrecovered panic there cannot be
// caught by any recover in the caller's goroutine — it would take the process
// down mid-run, losing every in-flight result. That the test binary survives
// to make this assertion is itself the assertion.
func TestSinkPanicDoesNotKillTheProcess(t *testing.T) {
	t.Parallel()

	var recorded atomic.Int64
	sink := func(_ context.Context, _ executor.Result[*core.Case, output]) error {
		if recorded.Add(1) == 5 {
			panic("nil map write in the sink")
		}
		return nil
	}

	_, err := executor.Run(context.Background(), staticCases(1_000), echoWork,
		sink, executor.Options{Concurrency: 4, ID: caseID})
	if err == nil {
		t.Error("a panicking sink completed without error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("error = %v, want it to name the panic", err)
	}
}

// TestBrokenSinkIsNotCalledAgain: nothing in SinkFunc's contract says it must
// tolerate being called after it fails, and the run is ending regardless.
func TestBrokenSinkIsNotCalledAgain(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("disk full")
	var calls atomic.Int64

	sink := func(_ context.Context, _ executor.Result[*core.Case, output]) error {
		if calls.Add(1) >= 5 {
			return wantErr
		}
		return nil
	}

	_, err := executor.Run(context.Background(), staticCases(10_000), echoWork,
		sink, executor.Options{Concurrency: 4, ID: caseID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want the sink failure", err)
	}
	if got := calls.Load(); got > 5 {
		t.Errorf("the sink was called %d times, want it abandoned after its first failure", got)
	}
}

// TestSinkRecordsAfterCallerCancellation: recording must survive the very
// cancellation it exists to preserve results through.
func TestSinkRecordsAfterCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var sawDeadContext atomic.Bool

	work := func(ctx context.Context, c *core.Case) (*output, error) {
		cancel()
		return echoWork(ctx, c)
	}
	// A context-respecting sink, as any store-backed one is.
	sink := func(ctx context.Context, _ executor.Result[*core.Case, output]) error {
		if ctx.Err() != nil {
			sawDeadContext.Store(true)
			return ctx.Err()
		}
		return nil
	}

	if _, err := executor.Run(ctx, staticCases(200), work, sink,
		executor.Options{Concurrency: 2, ID: caseID}); !errors.Is(err, context.Canceled) {
		t.Logf("run ended with: %v", err)
	}

	if sawDeadContext.Load() {
		t.Error("the sink was handed an already-cancelled context; on Ctrl-C it " +
			"would refuse to record the results it exists to preserve")
	}
}
