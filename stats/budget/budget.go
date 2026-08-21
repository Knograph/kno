// Package budget is the spend guard.
package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/knograph/kno/core/errs"
)

// Limits caps what a run may consume.
//
// Deliberately NOT a mirror of knov1.Budget. That message caps the SELECTED
// PORTFOLIO — how many context tokens it may add per call, how many training
// examples it may contain — which is a Select-stage question. This caps the
// RUN: what the measurement itself is allowed to spend getting there.
//
// Money is int64 micro-USD (1e-6 USD) everywhere, never a float. Dollars in
// floating point accumulate representation error across thousands of calls,
// and refusing to spend is decided on these numbers.
//
// A zero limit means unlimited, so a zero-value Limits guards nothing. That is
// the honest default for a struct literal: a guard that silently capped
// everything at zero would be worse, because it would look like it was working.
type Limits struct {
	// MaxCostUSDMicros caps total spend. Zero means unlimited.
	MaxCostUSDMicros int64

	// MaxLLMCalls caps how many provider calls a run may make. A hard ceiling
	// independent of dollars, because rate limits and wall-clock are budgets
	// too. Zero means unlimited.
	MaxLLMCalls int64
}

// Estimate is what an operation is expected to consume.
//
// Callers construct these; producing them from a real pricing model belongs
// with the first adapter, where there is a provider to be accurate about.
type Estimate struct {
	// Calls is the number of provider calls.
	Calls int64

	// CostUSDMicros is the expected spend.
	CostUSDMicros int64

	// Tokens is the expected token consumption. Recorded and reported, never
	// capped: DESIGN.md's budget config caps dollars and calls.
	Tokens int64
}

// Spend is what an operation actually consumed.
type Spend struct {
	// Calls actually made.
	Calls int64

	// CostUSDMicros actually spent.
	CostUSDMicros int64

	// Tokens actually consumed.
	Tokens int64
}

// Remaining is the headroom left, after subtracting both settled spend and
// outstanding reservations.
type Remaining struct {
	// CostUSDMicros left before the cap. Negative is impossible.
	CostUSDMicros int64

	// LLMCalls left before the cap.
	LLMCalls int64

	// Unlimited is true when no cap is set, in which case the fields above are
	// meaningless rather than zero.
	Unlimited bool
}

// ConfirmFunc asks the human whether to proceed.
//
// It is called at most once per threshold crossing, NEVER while the guard's
// lock is held, and never concurrently with itself. Returning false refuses
// the operation.
//
// The CLI satisfies this with a huh prompt, the API with estimate_only, and
// --yes with a function that returns true.
type ConfirmFunc func(ctx context.Context, est Estimate, rem Remaining) (bool, error)

// Guard authorizes spend against a set of limits.
//
// Every code path that can call an LLM or a fine-tuning API goes through it.
// A path that bypasses the guard spends someone else's money without consent,
// which SECURITY.md treats as a vulnerability rather than a bug.
//
// Safe for concurrent use.
type Guard struct {
	limits    Limits
	confirm   ConfirmFunc
	threshold int64 // micro-USD; an estimate at or above this asks first

	mu       sync.Mutex
	spent    Spend
	reserved Estimate
	nextID   uint64
	open     map[uint64]Estimate

	// confirmMu serializes confirmation prompts WITHOUT holding mu, so a human
	// staring at a prompt cannot block every other worker's accounting.
	confirmMu sync.Mutex
	confirmed bool
}

// New returns a Guard.
//
// confirm may be nil, in which case no confirmation is ever requested and
// operations within the limits proceed. threshold is in micro-USD: an estimate
// at or above it triggers confirmation once.
func New(limits Limits, confirm ConfirmFunc, thresholdUSDMicros int64) *Guard {
	return &Guard{
		limits:    limits,
		confirm:   confirm,
		threshold: thresholdUSDMicros,
		open:      make(map[uint64]Estimate),
	}
}

// Reservation is authorized headroom that has not yet been settled.
//
// Every Reservation MUST be either settled or released. The sanctioned idiom
// puts the release immediately after authorization, so that no error path can
// skip it:
//
//	res, err := guard.Authorize(ctx, est)
//	if err != nil {
//	    return err
//	}
//	defer res.Release()
//	...
//	res.Settle(actual)
//
// Release after Settle is a no-op, so the deferred call is always correct.
type Reservation struct {
	id    uint64
	est   Estimate
	guard *Guard

	once sync.Once
}

// ErrInvalidEstimate means an Estimate could not be authorized because its
// values are not usable as a bound.
//
// Separate from a refusal to spend: nothing was denied on budget grounds, the
// caller handed the guard a number it cannot reason about.
var ErrInvalidEstimate = errors.New("budget: invalid estimate")

// validate rejects an Estimate the guard cannot treat as a ceiling.
//
// A negative value does not merely under-reserve, it CREDITS the budget:
// fitsLocked compares spent+reserved+est against the cap, and reserved is
// summed, so a -$5.00 reservation hands $5.00 of phantom headroom to every
// other concurrent worker. Measured: a $1.00 cap reporting $6.00 remaining, and
// a cap of 2 calls authorizing 60 more.
//
// This lives here rather than only in the caller because Authorize is the
// choke point every spend path shares. cli/baseline.go already refuses a
// negative --cost-per-call-usd for exactly this reason; when core.Estimator
// moved the number from a validated flag to adapter code, that defense had to
// move with it.
func (est Estimate) validate() error {
	switch {
	case est.CostUSDMicros < 0:
		return fmt.Errorf("%w: cost is %d micro-USD; a negative reservation credits "+
			"the budget rather than consuming it", ErrInvalidEstimate, est.CostUSDMicros)
	case est.Calls < 0:
		return fmt.Errorf("%w: calls is %d; a negative reservation credits the call "+
			"cap rather than consuming it", ErrInvalidEstimate, est.Calls)
	case est.Tokens < 0:
		return fmt.Errorf("%w: tokens is %d", ErrInvalidEstimate, est.Tokens)
	}
	return nil
}

// Authorize reserves headroom for est, asking for confirmation if the estimate
// crosses the threshold.
//
// It denies AT the boundary rather than one call past it: an operation whose
// estimate would exceed a cap is refused before it runs, because refusing
// afterwards is only an expensive log line.
//
// Reservations are counted against the caps while outstanding, so N concurrent
// workers cannot each pass a check that only one of them should.
func (g *Guard) Authorize(ctx context.Context, est Estimate) (*Reservation, error) {
	if err := est.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	res, needsConfirm, rem := g.tryReserve(est)
	if res != nil {
		return res, nil
	}
	if !needsConfirm {
		return nil, g.denied(est, rem)
	}

	// Confirmation happens with the lock RELEASED. Holding it across a human
	// answering a prompt would serialize every other worker's accounting
	// behind one keypress.
	ok, err := g.askOnce(ctx, est, rem)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.ErrBudgetExceeded.WithFix(
			"the run was declined at the confirmation prompt; re-run with --yes to skip it")
	}

	// Retry under the lock. Budget may have been consumed by other workers
	// while the human was deciding, so the estimate is re-validated rather
	// than assumed still affordable.
	res, _, rem = g.tryReserve(est)
	if res == nil {
		return nil, g.denied(est, rem)
	}
	return res, nil
}

// tryReserve checks headroom and takes the reservation under a SINGLE lock
// acquisition.
//
// Splitting the check and the reservation into two acquisitions reopens the
// exact race the guard exists to close: between them, other workers can
// reserve the headroom this caller just verified, and N workers each pass a
// check that only one of them should. That is not theoretical — the
// concurrency test caught it authorizing 14 operations against a cap of 10.
//
// Returns a reservation on success. On failure it reports whether the caller
// should ask for confirmation and retry, along with current headroom.
func (g *Guard) tryReserve(est Estimate) (res *Reservation, needsConfirm bool, rem Remaining) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rem = g.remainingLocked()

	if g.confirm != nil && g.threshold > 0 && est.CostUSDMicros >= g.threshold && !g.confirmed {
		return nil, true, rem
	}
	if !g.fitsLocked(est) {
		return nil, false, rem
	}

	g.nextID++
	id := g.nextID
	g.open[id] = est
	g.reserved.Calls += est.Calls
	g.reserved.CostUSDMicros += est.CostUSDMicros
	g.reserved.Tokens += est.Tokens

	return &Reservation{id: id, est: est, guard: g}, false, rem
}

// fitsLocked reports whether est fits within the caps, counting outstanding
// reservations as already consumed. Callers must hold g.mu.
func (g *Guard) fitsLocked(est Estimate) bool {
	if g.limits.MaxCostUSDMicros > 0 {
		if g.spent.CostUSDMicros+g.reserved.CostUSDMicros+est.CostUSDMicros > g.limits.MaxCostUSDMicros {
			return false
		}
	}
	if g.limits.MaxLLMCalls > 0 {
		if g.spent.Calls+g.reserved.Calls+est.Calls > g.limits.MaxLLMCalls {
			return false
		}
	}
	return true
}

// askOnce invokes ConfirmFunc at most once across all callers.
//
// Without the singleflight, 128 workers crossing the threshold in the same
// instant would race to prompt on one terminal.
func (g *Guard) askOnce(ctx context.Context, est Estimate, rem Remaining) (bool, error) {
	g.confirmMu.Lock()
	defer g.confirmMu.Unlock()

	g.mu.Lock()
	already := g.confirmed
	g.mu.Unlock()
	if already {
		return true, nil
	}

	ok, err := g.confirm(ctx, est, rem)
	if err != nil {
		// A run that cannot obtain consent must STOP, not fail each operation
		// in turn. Wrapping as ErrBudgetExceeded is what makes it stop: the
		// caller treats that as fatal, so a --json run with nobody to answer
		// halts once instead of erroring every Case with the same prompt it
		// cannot show.
		return false, errs.ErrBudgetExceeded.
			WithFix("pass --yes to proceed without confirmation").
			Wrap(fmt.Errorf("confirming spend: %w", err))
	}
	if ok {
		g.mu.Lock()
		g.confirmed = true
		g.mu.Unlock()
	}
	return ok, nil
}

// denied explains WHICH cap stopped the run.
//
// Naming both dimensions when only one binds reads as noise — an operation
// refused by a call cap reported "needs $0.000000 and 1 call(s)", which tells
// the user nothing about what to change. CLAUDE.md's grammar requires the why
// to be specific enough that the fix follows from it.
func (g *Guard) denied(est Estimate, rem Remaining) error {
	g.mu.Lock()
	costCapped := g.limits.MaxCostUSDMicros > 0
	callCapped := g.limits.MaxLLMCalls > 0
	g.mu.Unlock()

	costBinds := costCapped && est.CostUSDMicros > rem.CostUSDMicros
	callBinds := callCapped && est.Calls > rem.LLMCalls

	switch {
	case costBinds && callBinds:
		return errs.ErrBudgetExceeded.Wrap(fmt.Errorf(
			"the next operation needs %s and %d call(s), but only %s and %d call(s) remain",
			formatUSD(est.CostUSDMicros), est.Calls,
			formatUSD(rem.CostUSDMicros), rem.LLMCalls))
	case callBinds:
		return errs.ErrBudgetExceeded.
			WithFix("raise --max-calls, or re-run with --resume to continue where it stopped").
			Wrap(fmt.Errorf("the call limit is spent: %d of %d used",
				g.Spent().Calls, g.Limits().MaxLLMCalls))
	default:
		return errs.ErrBudgetExceeded.
			WithFix("raise --max-cost-usd, or re-run with --resume to continue where it stopped").
			Wrap(fmt.Errorf("the cost limit is spent: %s of %s used",
				formatUSD(g.Spent().CostUSDMicros), formatUSD(g.Limits().MaxCostUSDMicros)))
	}
}

// Settle converts the reservation into recorded spend.
//
// actual may differ from the estimate in either direction; the reservation is
// released and the real figure recorded. Calling Settle more than once, or
// after Release, is a no-op.
func (r *Reservation) Settle(actual Spend) {
	r.once.Do(func() {
		g := r.guard
		g.mu.Lock()
		defer g.mu.Unlock()

		g.releaseLocked(r.id)
		g.spent.Calls += actual.Calls
		g.spent.CostUSDMicros += actual.CostUSDMicros
		g.spent.Tokens += actual.Tokens
	})
}

// Release returns the reservation's headroom without recording spend.
//
// This is what makes `defer res.Release()` safe: an operation that fails after
// authorization, a context cancelled mid-flight, or a recovered panic all end
// up here. Without it, every such path would leak headroom permanently, and
// the guard would eventually deny operations that should have been allowed —
// a false refusal is a quieter bug than an overspend, but still a wrong one.
//
// Idempotent, and a no-op after Settle.
func (r *Reservation) Release() {
	r.once.Do(func() {
		g := r.guard
		g.mu.Lock()
		defer g.mu.Unlock()
		g.releaseLocked(r.id)
	})
}

// Estimate returns what this reservation was authorized for.
func (r *Reservation) Estimate() Estimate { return r.est }

func (g *Guard) releaseLocked(id uint64) {
	est, ok := g.open[id]
	if !ok {
		return
	}
	delete(g.open, id)
	g.reserved.Calls -= est.Calls
	g.reserved.CostUSDMicros -= est.CostUSDMicros
	g.reserved.Tokens -= est.Tokens
}

// Remaining reports headroom, counting outstanding reservations as already
// consumed. Reading it mid-run is safe but inherently a snapshot.
func (g *Guard) Remaining() Remaining {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.remainingLocked()
}

func (g *Guard) remainingLocked() Remaining {
	if g.limits.MaxCostUSDMicros == 0 && g.limits.MaxLLMCalls == 0 {
		return Remaining{Unlimited: true}
	}
	rem := Remaining{}
	if g.limits.MaxCostUSDMicros > 0 {
		rem.CostUSDMicros = max(0, g.limits.MaxCostUSDMicros-g.spent.CostUSDMicros-g.reserved.CostUSDMicros)
	}
	if g.limits.MaxLLMCalls > 0 {
		rem.LLMCalls = max(0, g.limits.MaxLLMCalls-g.spent.Calls-g.reserved.Calls)
	}
	return rem
}

// Restore reseeds settled spend from durable storage.
//
// The Guard is in-memory, so a process that dies takes its accounting with it.
// Without this, `--resume` would construct a fresh Guard reporting zero spent
// no matter how much the killed run actually spent, and a run near its cap
// could authorize nearly the whole cap a second time — up to twice the
// intended spend across one kill/resume cycle. That is the silent overspend
// CLAUDE.md's fourth prime directive exists to prevent.
//
// Callers reconstruct spent by summing what was PERSISTED for the run, because
// the store is the only thing that outlives the process. Restore is additive,
// so it composes with a partially-used Guard, and it must be called before any
// Authorize — restoring afterwards would let an already-authorized operation
// slip under a cap it should have been measured against.
//
// It deliberately does not restore reservations. An outstanding reservation
// belongs to an operation whose process is gone; its work either completed and
// was persisted (and is therefore counted in spent) or it did not happen. See
// docs/debt.md#20 for the window this cannot observe.
func (g *Guard) Restore(spent Spend) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.spent.Calls += spent.Calls
	g.spent.CostUSDMicros += spent.CostUSDMicros
	g.spent.Tokens += spent.Tokens
}

// Limits reports the caps this Guard enforces.
func (g *Guard) Limits() Limits { return g.limits }

// Overshoot reports how far settled spend has passed the cost cap, or zero
// while it is still within it.
//
// It exists because Remaining clamps at zero: a Guard that has blown its cap
// reports Remaining{CostUSDMicros: 0}, which is indistinguishable from one
// exactly consumed. The breach is real — a cap of C bounds spend at C plus the
// sum of under-estimations across the calls in flight when it binds — and
// without this it is not merely unenforced but unobservable.
//
// Two things can produce a non-zero reading, and a consumer that reports this
// as "we under-estimated by X" would be wrong about the second: settled spend
// really passing the cap, OR a resume under a LOWER cap than the run was
// started with. Restore is additive and does not check the cap, so a run
// resumed with --max-cost-usd reduced reads as over immediately, before this
// process spends anything. That is a supported stop, not an estimation error.
//
// This is observability, not enforcement. By settlement the money is spent, and
// fitsLocked already refuses every subsequent authorization once spend reaches
// the cap. Making Settle fail instead would turn a successful, paid, scored
// call into an errored Case and lose work that was paid for.
//
// Zero when no cost cap is set: without a cap there is nothing to overshoot.
func (g *Guard) Overshoot() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.limits.MaxCostUSDMicros <= 0 {
		return 0
	}
	return max(0, g.spent.CostUSDMicros-g.limits.MaxCostUSDMicros)
}

// Spent reports what has actually been settled, excluding outstanding
// reservations. This is the number a report shows.
func (g *Guard) Spent() Spend {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent
}

// formatUSD renders micro-USD as dollars for error messages only. Never use it
// for arithmetic; the whole point of micro-USD is to avoid float math.
func formatUSD(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	// Cents, not micro-dollars. Six decimal places in a user-facing message is
	// precision nobody reads and everybody has to parse past.
	return fmt.Sprintf("%s$%d.%02d", sign, micros/1_000_000, (micros%1_000_000)/10_000)
}
