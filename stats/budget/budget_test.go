package budget_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
)

// TestDeniesAtTheBoundaryNotPastIt pins the rule that makes the guard useful:
// an operation whose ESTIMATE would exceed a cap is refused before it runs.
// Refusing afterwards is only an expensive log line.
func TestDeniesAtTheBoundaryNotPastIt(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	// Exactly at the cap is allowed.
	res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 1_000_000})
	if err != nil {
		t.Fatalf("an estimate exactly at the cap was refused: %v", err)
	}
	res.Settle(budget.Spend{CostUSDMicros: 1_000_000})

	// One micro-dollar past it is not.
	if _, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 1}); err == nil {
		t.Fatal("an operation past the cap was authorized")
	} else if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("got %v, want ErrBudgetExceeded so the CLI can exit 2", err)
	}
}

// TestBudgetStopIsResumableNotAFailure checks the exit code, because a budget
// stop and a crash mean different things to CI: one is resumable, the other is
// broken.
func TestBudgetStopIsResumableNotAFailure(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxLLMCalls: 1}, nil, 0)
	if _, err := g.Authorize(context.Background(), budget.Estimate{Calls: 2}); err == nil {
		t.Fatal("want a denial")
	} else if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d (budget-stopped)", got, errs.ExitBudgetStopped)
	}
}

// TestMicroUSDAccumulatesExactly is why money is int64 rather than float64.
//
// Ten thousand sub-cent charges must sum exactly. In float64 this accumulates
// representation error, and the guard decides whether to spend on this number.
func TestMicroUSDAccumulatesExactly(t *testing.T) {
	t.Parallel()

	const (
		charge = int64(1_234) // $0.001234
		n      = 10_000
	)
	g := budget.New(budget.Limits{}, nil, 0)

	for range n {
		res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: charge})
		if err != nil {
			t.Fatalf("unlimited guard refused a charge: %v", err)
		}
		res.Settle(budget.Spend{CostUSDMicros: charge})
	}

	if got, want := g.Spent().CostUSDMicros, charge*n; got != want {
		t.Errorf("accumulated %d micro-USD, want exactly %d (drift of %d)", got, want, got-want)
	}
}

// TestConcurrentAuthorizeNeverExceedsTheCap is the test the guard exists for.
//
// Without reservations counting against the cap while outstanding, N workers
// each pass a check that only one of them should — every one of them sees
// headroom, and the run overspends by a factor of N.
func TestConcurrentAuthorizeNeverExceedsTheCap(t *testing.T) {
	t.Parallel()

	const (
		workers  = 128
		maxCalls = int64(10) // only 10 of the 128 may proceed
	)
	g := budget.New(budget.Limits{MaxLLMCalls: maxCalls}, nil, 0)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int64
		refused int64
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := g.Authorize(context.Background(), budget.Estimate{Calls: 1})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			granted++
			res.Settle(budget.Spend{Calls: 1})
		}()
	}
	wg.Wait()

	if granted > maxCalls {
		t.Errorf("%d workers were authorized against a cap of %d; the run overspent", granted, maxCalls)
	}
	if granted+refused != workers {
		t.Errorf("accounting lost operations: %d granted + %d refused != %d", granted, refused, workers)
	}
	if got := g.Spent().Calls; got != granted {
		t.Errorf("settled %d calls but granted %d", got, granted)
	}
}

// TestAbandonedReservationDoesNotLeakHeadroom covers the case the original
// plan omitted entirely, and which Phase-1 review flagged as blocking.
//
// Release happened only on deny and on settle — both success paths. An
// operation that errors after authorization, a context cancelled mid-flight,
// or a recovered panic each leaked headroom permanently, shrinking Remaining()
// below the true value until the guard started refusing operations it should
// have allowed. A false refusal is quieter than an overspend, and just as wrong.
func TestAbandonedReservationDoesNotLeakHeadroom(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxLLMCalls: 2}, nil, 0)

	// A worker that authorizes, then fails before settling. The deferred
	// Release is the whole point of the idiom.
	func() {
		res, err := g.Authorize(context.Background(), budget.Estimate{Calls: 1})
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		defer res.Release()
		_ = errors.New("the provider call failed")
	}()

	// A worker that panics after authorizing.
	func() {
		defer func() { _ = recover() }()
		res, err := g.Authorize(context.Background(), budget.Estimate{Calls: 1})
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		defer res.Release()
		panic("boom")
	}()

	if got := g.Remaining().LLMCalls; got != 2 {
		t.Errorf("remaining = %d, want 2: abandoned reservations leaked headroom", got)
	}
	if got := g.Spent().Calls; got != 0 {
		t.Errorf("spent = %d, want 0: nothing was actually spent", got)
	}
}

// TestReleaseIsIdempotentAndSettleWins makes `defer res.Release()` safe to
// write unconditionally, which is what keeps every error path correct.
func TestReleaseIsIdempotentAndSettleWins(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxLLMCalls: 10}, nil, 0)

	res, err := g.Authorize(context.Background(), budget.Estimate{Calls: 1})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Settle(budget.Spend{Calls: 1, CostUSDMicros: 500})
	res.Release() // the deferred call, after a successful settle
	res.Release()
	res.Settle(budget.Spend{Calls: 99}) // a double-settle must not double-count

	if got := g.Spent().Calls; got != 1 {
		t.Errorf("spent %d calls, want 1: settle/release is not idempotent", got)
	}
	if got := g.Remaining().LLMCalls; got != 9 {
		t.Errorf("remaining = %d, want 9", got)
	}
}

// TestSettleRecordsActualNotEstimate matters because actual spend almost never
// equals the estimate: output length is not knowable in advance.
func TestSettleRecordsActualNotEstimate(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxCostUSDMicros: 10_000}, nil, 0)

	res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 5_000})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Settle(budget.Spend{CostUSDMicros: 1_200}) // came in well under

	if got := g.Spent().CostUSDMicros; got != 1_200 {
		t.Errorf("spent = %d, want the actual 1200, not the estimate", got)
	}
	if got := g.Remaining().CostUSDMicros; got != 8_800 {
		t.Errorf("remaining = %d, want 8800: the unspent reservation was not returned", got)
	}
}

// TestConfirmAskedOnceUnderConcurrency guards the terminal.
//
// 128 workers crossing the threshold together must produce ONE prompt, not 128
// racing for one terminal.
func TestConfirmAskedOnceUnderConcurrency(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		asked int
	)
	confirm := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		return true, nil
	}

	g := budget.New(budget.Limits{}, confirm, 1_000)

	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 5_000})
			if err == nil {
				res.Settle(budget.Spend{CostUSDMicros: 5_000})
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Errorf("ConfirmFunc was called %d times; 128 concurrent prompts would fight over one terminal", asked)
	}
}

// TestConfirmDeclineRefusesTheOperation checks that "no" means no, and that it
// still maps to the resumable exit code.
func TestConfirmDeclineRefusesTheOperation(t *testing.T) {
	t.Parallel()

	confirm := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		return false, nil
	}
	g := budget.New(budget.Limits{}, confirm, 1_000)

	if _, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 5_000}); err == nil {
		t.Fatal("a declined confirmation still authorized the spend")
	} else if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("got %v, want ErrBudgetExceeded", err)
	}
}

// TestNoConfirmBelowThreshold keeps small operations from prompting.
func TestNoConfirmBelowThreshold(t *testing.T) {
	t.Parallel()

	var asked int
	confirm := func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
		asked++
		return true, nil
	}
	g := budget.New(budget.Limits{}, confirm, 1_000_000)

	res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 10})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Settle(budget.Spend{CostUSDMicros: 10})

	if asked != 0 {
		t.Errorf("ConfirmFunc was called %d times for a spend far below the threshold", asked)
	}
}

// TestCancelledContextIsRefused stops a guard from authorizing work the caller
// has already given up on — Ctrl-C should not be followed by another paid call.
func TestCancelledContextIsRefused(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g := budget.New(budget.Limits{}, nil, 0)
	if _, err := g.Authorize(ctx, budget.Estimate{Calls: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// TestZeroLimitsMeanUnlimited pins the zero-value behavior, which is the one a
// struct literal produces by accident.
func TestZeroLimitsMeanUnlimited(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{}, nil, 0)
	if !g.Remaining().Unlimited {
		t.Error("zero Limits should report Unlimited rather than a headroom of zero")
	}
	res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 1 << 40})
	if err != nil {
		t.Errorf("an unlimited guard refused a large operation: %v", err)
	}
	res.Release()
}

// TestReservationsCountAgainstRemaining confirms outstanding work is visible as
// consumed, so a second worker cannot be told there is headroom that is
// already promised.
func TestReservationsCountAgainstRemaining(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxCostUSDMicros: 1_000}, nil, 0)

	res, err := g.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 600})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if got := g.Remaining().CostUSDMicros; got != 400 {
		t.Errorf("remaining = %d, want 400 while a 600 reservation is outstanding", got)
	}
	if got := g.Spent().CostUSDMicros; got != 0 {
		t.Errorf("spent = %d, want 0: a reservation is not spend", got)
	}
	res.Release()
	if got := g.Remaining().CostUSDMicros; got != 1_000 {
		t.Errorf("remaining = %d, want 1000 after release", got)
	}
}

// TestResumeDoesNotForgetSpend is the test that would have caught a P0.
//
// The Guard is in-memory, so a process that dies takes its accounting with it.
// Before Restore existed, `kno baseline --resume` constructed a fresh Guard
// reporting zero spent no matter what the killed run had actually spent — so a
// run that had already consumed most of its cap could consume nearly all of it
// again. Up to twice the intended spend across one kill/resume cycle, silently,
// which is exactly the failure CLAUDE.md's fourth prime directive names.
//
// This simulates the cycle: spend against a cap, discard the Guard as a crash
// would, rebuild it, and assert the rebuilt Guard refuses what the original
// would have refused.
func TestResumeDoesNotForgetSpend(t *testing.T) {
	t.Parallel()

	const limit = int64(1_000_000) // $1.00

	// First process: spends $0.90 of a $1.00 cap.
	first := budget.New(budget.Limits{MaxCostUSDMicros: limit}, nil, 0)
	res, err := first.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 900_000})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	res.Settle(budget.Spend{CostUSDMicros: 900_000})

	persisted := first.Spent() // what the store would hold for this run

	// The process dies here. Everything in `first` is gone.

	// Second process: same limits, fresh Guard, reseeded from the store.
	second := budget.New(budget.Limits{MaxCostUSDMicros: limit}, nil, 0)
	second.Restore(persisted)

	if got := second.Spent().CostUSDMicros; got != 900_000 {
		t.Fatalf("restored spend = %d, want 900000", got)
	}
	if got := second.Remaining().CostUSDMicros; got != 100_000 {
		t.Errorf("remaining = %d, want 100000 after restoring prior spend", got)
	}

	// The whole point: an operation that fits within a FRESH guard's cap must
	// still be refused, because the money is already gone.
	if _, err := second.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 900_000}); err == nil {
		t.Error("the resumed run authorized a second $0.90 against a $1.00 cap; " +
			"total spend would be $1.80 for a run capped at $1.00")
	} else if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("got %v, want ErrBudgetExceeded", err)
	}

	// And what genuinely fits still proceeds.
	ok, err := second.Authorize(context.Background(), budget.Estimate{CostUSDMicros: 100_000})
	if err != nil {
		t.Errorf("an operation within the remaining budget was refused: %v", err)
	} else {
		ok.Release()
	}
}

// TestRestoreIsAdditive covers reseeding a Guard that has already been used,
// and the multi-dimension case.
func TestRestoreIsAdditive(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{}, nil, 0)
	g.Restore(budget.Spend{Calls: 3, CostUSDMicros: 500, Tokens: 90})
	g.Restore(budget.Spend{Calls: 2, CostUSDMicros: 250, Tokens: 10})

	got := g.Spent()
	if got.Calls != 5 || got.CostUSDMicros != 750 || got.Tokens != 100 {
		t.Errorf("Spent = %+v, want {Calls:5 CostUSDMicros:750 Tokens:100}", got)
	}
}

// TestRestoreIsSafeUnderConcurrency guards the accounting under -race, since
// Restore mutates the same state Authorize reads.
func TestRestoreIsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()

	g := budget.New(budget.Limits{MaxLLMCalls: 1_000}, nil, 0)

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Restore(budget.Spend{Calls: 1})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := g.Authorize(context.Background(), budget.Estimate{Calls: 1}); err == nil {
				res.Settle(budget.Spend{Calls: 1})
			}
		}()
	}
	wg.Wait()

	if got := g.Spent().Calls; got < 64 {
		t.Errorf("spent = %d, want at least the 64 restored calls", got)
	}
}
