package executor_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
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

// discardSink records nothing, for tests whose subject is not the sink.
func discardSink(_ context.Context, _ executor.Result[*core.Case, output]) error { return nil }

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

// TestRecordGraceDoesNotBoundTheRun is the regression test for docs/debt.md#54.
//
// RecordGrace was a context.WithTimeout built before the first item was
// dispatched, which made it a deadline on the WHOLE run rather than on the
// drain after cancellation. On any run longer than the grace the first write
// failed, sinkBroken latched so every result after it was discarded without
// being asked, and the caller's completed set was missing work it had already
// paid for — which a resumed run pays for again.
//
// Timings are the ledger's own reproduction, scaled to belong in `make test`:
// a grace far shorter than the run, with a sink slow enough that the run
// crosses it several times over.
func TestRecordGraceDoesNotBoundTheRun(t *testing.T) {
	t.Parallel()

	const items = 6
	var mu sync.Mutex
	var recorded []string

	sink := func(ctx context.Context, r executor.Result[*core.Case, output]) error {
		// Deliberately longer than the grace. Under the old code the second
		// call is already past the deadline.
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, r.Item.GetId())
		return nil
	}

	stats, err := executor.Run(context.Background(), staticCases(items), echoWork, sink, executor.Options{
		Concurrency: 1,
		ID:          caseID,
		RecordGrace: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v — a run longer than the grace is not a failure; the "+
			"grace bounds the drain after the CALLER cancels", err)
	}

	mu.Lock()
	got := len(recorded)
	mu.Unlock()
	if got != items {
		t.Errorf("recorded %d of %d results. The rest were silently discarded — "+
			"no outcome row, absent from the caller's completed set, and paid "+
			"for again on resume (docs/debt.md#54)", got, items)
	}
	if stats.Recorded() != items {
		t.Errorf("Recorded() = %d, want %d", stats.Recorded(), items)
	}
}

// TestRecordGraceStillBoundsTheDrainAfterCancel is the other half, and the one
// whose absence would let the Ctrl-C bound vanish with the suite green.
//
// The godoc promises recording does not continue indefinitely, because a hung
// sink would otherwise make Ctrl-C unbounded. Fixing the run-length bug by
// deleting the deadline entirely would satisfy the test above and quietly
// retire that promise.
func TestRecordGraceStillBoundsTheDrainAfterCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	// A sink that HONORS its context but never finishes on its own. That is
	// the bound the grace actually offers: a sink that ignores its context is
	// documented as the caller's problem, and Run's godoc says so — asserting
	// otherwise would be testing a promise this package deliberately declines
	// to make.
	var once sync.Once
	sink := func(sinkCtx context.Context, _ executor.Result[*core.Case, output]) error {
		once.Do(cancel) // the user hits Ctrl-C mid-run
		<-sinkCtx.Done()
		return sinkCtx.Err()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = executor.Run(ctx, staticCases(4), echoWork, sink, executor.Options{
			Concurrency:      1,
			ID:               caseID,
			RecordGrace:      40 * time.Millisecond,
			PerRecordTimeout: time.Hour, // the grace must be what stops this
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned. recordCtx is never cancelled, so the grace " +
			"is not armed by the caller's cancellation and Ctrl-C against a " +
			"slow sink hangs — the bound RecordGrace's godoc promises")
	}
}

// TestAHungSinkIsBoundedPerCallNotPerRun pins the other half of the split:
// PerRecordTimeout is what makes one hung write survivable, and it must not
// accumulate across calls.
func TestAHungSinkIsBoundedPerCallNotPerRun(t *testing.T) {
	t.Parallel()

	// Each call sleeps most of its own budget, and the SUM is several times
	// the budget. That gap is the whole assertion: under a per-run reading the
	// third call is already past the deadline, under a per-call reading every
	// call has room. A budget larger than the sum would pass either way, which
	// is what the first version of this test did.
	const (
		perCall = 500 * time.Millisecond
		perSink = 100 * time.Millisecond
		items   = 6 // 600ms of sink work against a 500ms budget
	)
	sink := func(ctx context.Context, _ executor.Result[*core.Case, output]) error {
		select {
		case <-time.After(perSink):
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}

	stats, err := executor.Run(context.Background(), staticCases(items), echoWork, sink, executor.Options{
		Concurrency:      1,
		ID:               caseID,
		PerRecordTimeout: perCall,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Recorded() != items {
		t.Errorf("Recorded() = %d, want %d — the per-call budget was consumed as "+
			"if it were a per-run one", stats.Recorded(), items)
	}
}

// TestAfterRecordEndsTheRunAndKeepsTheResult.
//
// IsFatal is consulted only on a work ERROR (see the send in Run), so there is
// no path from a SUCCESSFUL item to shutdown. Some conditions are discovered in
// an answer the caller has already paid for — a provider resolving a moving
// alias to a different model mid-run being the case this was built for.
//
// The two rejected alternatives are what the assertions here pin. Failing the
// item would discard a paid, scoreable answer and record it as an error;
// returning an error from SinkFunc would latch sinkBroken and discard every
// result after it, which is docs/debt.md#54's failure mode by another route.
func TestAfterRecordEndsTheRunAndKeepsTheResult(t *testing.T) {
	t.Parallel()

	errGate := errors.New("the resolved model changed")

	var mu sync.Mutex
	var recorded []string
	sink := func(_ context.Context, r executor.Result[*core.Case, output]) error {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, r.Item.GetId())
		return nil
	}

	var seen atomic.Int64
	stats, err := executor.Run(context.Background(), staticCases(50), echoWork, sink, executor.Options{
		Concurrency: 1,
		ID:          caseID,
		AfterRecord: func(result any) error {
			seen.Add(1)
			r, ok := result.(executor.Result[*core.Case, output])
			if !ok {
				t.Fatalf("AfterRecord got %T, not a Result", result)
			}
			if !r.Done() {
				t.Errorf("a successful item reported an error: %v", r.Err)
			}
			if r.Value == nil {
				t.Error("no value; AfterRecord runs on the recorded result")
			}
			if r.Item.GetId() == "case-002" {
				return errGate
			}
			return nil
		},
	})

	if !errors.Is(err, errGate) {
		t.Fatalf("Run error = %v, want the gate's error — AfterRecord must end "+
			"the run", err)
	}
	if stats.Dispatched >= 50 {
		t.Errorf("dispatched %d of 50; the run did not stop", stats.Dispatched)
	}

	// The triggering result is durable and counted. This is the whole point:
	// the item succeeded and the store says so.
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(recorded, "case-002") {
		t.Error("the triggering result was not recorded. Ending the run must " +
			"not discard the answer that ended it — it is paid for")
	}
	if stats.Failed != 0 {
		t.Errorf("Failed = %d, want 0. The item succeeded; counting it as a "+
			"failure is what returning an error from the work would have done",
			stats.Failed)
	}
	if stats.Succeeded < 3 {
		t.Errorf("Succeeded = %d, want at least 3 (case-000..002)", stats.Succeeded)
	}
}

// TestAfterRecordSeesAFailedItemWithoutBeingAskedToJudgeIt keeps the seam
// honest about its argument: a failed item still reaches AfterRecord, and
// Result's invariant survives the boxing — Done() answers, and a failed item
// carries no value.
func TestAfterRecordSeesAFailedItemWithoutBeingAskedToJudgeIt(t *testing.T) {
	t.Parallel()

	failing := func(_ context.Context, c *core.Case) (*output, error) {
		if c.GetId() == "case-001" {
			return nil, errors.New("boom")
		}
		return &output{id: c.GetId()}, nil
	}

	var sawErr atomic.Bool
	_, err := executor.Run(context.Background(), staticCases(3), failing, discardSink, executor.Options{
		Concurrency: 1,
		ID:          caseID,
		AfterRecord: func(result any) error {
			r, ok := result.(executor.Result[*core.Case, output])
			if !ok {
				t.Fatalf("AfterRecord got %T, not a Result", result)
			}
			if !r.Done() {
				sawErr.Store(true)
				// The invariant, which the boxed-Result shape makes checkable
				// rather than merely documented: a failed item carries no
				// value, and the caller reads that off Done() instead of
				// second-guessing a pointer.
				if r.Value != nil {
					t.Errorf("a failed item carried a value: %+v", r.Value)
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v — a per-item failure is not a run failure", err)
	}
	if !sawErr.Load() {
		t.Error("AfterRecord never saw the failed item")
	}
}

// TestAPanicInAfterRecordEndsTheRunRatherThanHangingIt.
//
// AfterRecord is caller-supplied code running inline in the loop that drains
// `results`. Unguarded, a panic unwinds out of that loop — and nothing else
// drains the channel, while every worker's send into it is unconditional by
// design (that unconditional send is itself a deliberate fix: selecting on
// cancellation there threw away paid-for results on Ctrl-C).
//
// So the workers block forever, workers.Wait() never returns, and Run hangs
// permanently with no path to recovery. Only SIGKILL ends it, which loses every
// in-flight result and re-pays for them on resume. Reproduced on this branch
// before the guard existed: 200 items at concurrency 8, still hung at 3s.
//
// The sink and the work function are both guarded for a weaker version of this
// reason. This one is the strongest version.
func TestAPanicInAfterRecordEndsTheRunRatherThanHangingIt(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = executor.Run(context.Background(), staticCases(200), echoWork, discardSink,
			executor.Options{
				Concurrency: 8,
				ID:          caseID,
				AfterRecord: func(result any) error {
					r, _ := result.(executor.Result[*core.Case, output])
					if r.Item.GetId() == "case-002" {
						// A panic value that CARRIES content, which is the
						// whole reason the recover formats with %T. A real one
						// is a failed assertion holding a prompt or a response.
						panic(errors.New("caller bug: sk-live-SECRET-do-not-log"))
					}
					return nil
				},
			})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: Run never returned. A panic in AfterRecord unwound " +
			"out of the sink loop, so nothing drains results and every worker " +
			"is blocked on an unconditional send")
	}

	if err == nil {
		t.Fatal("the panic was swallowed; the run must end")
	}
	if !strings.Contains(err.Error(), "panic in AfterRecord") {
		t.Errorf("error = %q, want it to name the panicking hook", err)
	}
	// %T, not %v — the same rule the sink and work recovers state, because a
	// panic value is arbitrary and may embed prompt or response content.
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("the panic value's CONTENT reached the error, which is "+
			"persisted and shown to the user: %q", err)
	}
}

// TestAfterRecordDoesNotDiscardResultsAlreadyInFlight.
//
// Ending the run must not break out of the drain loop. Every worker that is
// mid-item still has a result to deliver, and a result that never reaches the
// sink is one a resumed run pays for again — the same money-losing shape as
// docs/debt.md#54.
//
// At Concurrency 1 this is unfalsifiable: at most one result is ever in flight,
// so a drain that stops early is indistinguishable from one that does not. This
// runs wide on purpose.
//
// Two mutations are in scope and they fail differently. Breaking out of the
// loop hangs the run — nothing drains `results` and the workers block — so the
// timeout catches it. Skipping the RECORD for post-gate results while still
// draining keeps the run alive and silently loses paid-for work, and only the
// count assertion below catches that one.
func TestAfterRecordDoesNotDiscardResultsAlreadyInFlight(t *testing.T) {
	t.Parallel()

	const concurrency = 8

	var mu sync.Mutex
	recorded := map[string]bool{}
	sink := func(_ context.Context, r executor.Result[*core.Case, output]) error {
		mu.Lock()
		defer mu.Unlock()
		recorded[r.Item.GetId()] = true
		return nil
	}

	work := func(_ context.Context, c *core.Case) (*output, error) {
		return &output{id: c.GetId()}, nil
	}

	stats, err := executor.Run(context.Background(), staticCases(500), work, sink,
		executor.Options{
			Concurrency: concurrency,
			ID:          caseID,
			AfterRecord: func(result any) error {
				r, _ := result.(executor.Result[*core.Case, output])
				if r.Item.GetId() == "case-010" {
					return errors.New("gate")
				}
				return nil
			},
		})
	if err == nil {
		t.Fatal("the gate did not end the run")
	}

	// Every item a worker EXECUTED reached the sink.
	//
	// Dispatched is the independent baseline, and it has to be: comparing the
	// sink's count against Stats.Recorded() proves nothing, because a drain
	// that skips results decrements both together and they stay equal. Work
	// performed is the thing that cost money, so work performed is what must
	// equal work recorded.
	//
	// Run returns only after workers.Wait(), so every dispatched item has
	// completed and sent by the time this reads.
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != stats.Dispatched {
		t.Errorf("%d items were executed and %d reached the sink. The drain "+
			"stopped early, so paid-for work went unrecorded and a resumed run "+
			"pays for it again", stats.Dispatched, len(recorded))
	}
	if stats.Recorded() != stats.Dispatched {
		t.Errorf("Dispatched = %d, Recorded = %d", stats.Dispatched, stats.Recorded())
	}
	if !recorded["case-010"] {
		t.Error("the triggering result was discarded")
	}
	if stats.Recorded() < 11 {
		t.Errorf("Recorded() = %d, want at least 11 (case-000..010)", stats.Recorded())
	}
}
