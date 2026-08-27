// Package executor runs work over a bounded pool with checkpointing.
package executor

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// Result is one item's terminal outcome.
//
// Exactly one of Value or Err is meaningful, mirroring the store's Outcome and
// the event stream's CaseScored/CaseErrored split. An item is either done or
// failed; it is never both, because a shape permitting both would let one item
// land on both sides of the denominator.
type Result[I proto.Message, T any] struct {
	// Item is the input this result belongs to.
	Item I

	// Value is the work's output. Nil when Err is set.
	Value *T

	// Err is the terminal failure. Nil when the work succeeded.
	Err error
}

// Done reports whether the item succeeded.
func (r Result[I, T]) Done() bool { return r.Err == nil }

// ModelCarrier is implemented by outcome types whose Value holds the model
// that answered, so a stage-agnostic hook can read it without knowing the
// stage's concrete outcome type.
type ModelCarrier interface{ Model() string }

// Model reports which model answered, or "" — the one piece of an outcome
// the mid-run model gate needs, abstracted over the generic T.
func (r Result[I, T]) Model() string {
	if r.Value == nil {
		return ""
	}
	if m, ok := any(r.Value).(ModelCarrier); ok {
		return m.Model()
	}
	return ""
}

// Options configures a Run.
type Options struct {
	// Concurrency bounds in-flight work. Zero means a conservative default.
	//
	// The constraint is provider rate limits, not CPU: a bigger pool buys
	// nothing but a faster path to a 429.
	Concurrency int

	// ID identifies an item, for Skip and for error messages. Required.
	//
	// The executor is generic over its item type — it has no business knowing
	// what a Case is — so the caller supplies the identity function.
	ID func(any) string

	// Skip reports whether an item is already complete and should not be
	// re-executed. Nil means execute everything.
	//
	// This is how resume avoids paying twice. It is consulted in the producer,
	// before any work is dispatched, so a skipped item costs nothing.
	Skip func(caseID string) bool

	// IsFatal reports whether a work error ends the whole run rather than just
	// that item. Nil means every work error is per-item.
	//
	// Without this there is no path from a WorkFunc to shutdown, and a
	// budget-exhausted run would dispatch every remaining item only to have
	// each one denied — completing with thousands of failures instead of
	// stopping as budget-stopped and resumable. The caller decides which
	// errors qualify, so this package needs no knowledge of the budget guard.
	IsFatal func(error) bool

	// RecordGrace bounds how long result recording may continue after the
	// caller's context is cancelled. Zero means a sensible default.
	//
	// Recording outlives cancellation on purpose — the money is already spent —
	// but not indefinitely, or a hung sink would make Ctrl-C unbounded.
	//
	// It is armed BY the cancellation, not at run start. It was a
	// context.WithTimeout built before the first item was dispatched, which
	// made it a bound on the whole run: every result after 30 seconds was
	// silently discarded, sinkBroken latched so the loss cascaded to every
	// result after it, and a resumed run paid for all of them again. The
	// godoc above always described this behavior; nothing implemented it until
	// docs/debt.md#54.
	RecordGrace time.Duration

	// PerRecordTimeout bounds a SINGLE SinkFunc call. Zero means a sensible
	// default.
	//
	// This is the bound that makes a hung sink survivable during a run, and it
	// is per call because the hazard is one call that never returns — not a
	// budget the whole run draws down. Bounding the sum instead is what
	// docs/debt.md#54 was.
	PerRecordTimeout time.Duration

	// AfterRecord runs on the sink goroutine once a result is DURABLY
	// recorded, and a non-nil return ends the run.
	//
	// It exists because IsFatal is consulted only on a work ERROR, so there is
	// no path from a SUCCESSFUL item to shutdown — and some conditions are
	// discovered in a successful response. The resolved model changing mid-run
	// is the case that forced this: the answer is paid for and scoreable, so
	// failing the item to stop the run would discard a result the caller
	// already owns, and returning an error from SinkFunc would latch
	// sinkBroken and discard every result after it.
	//
	// Ending the run here keeps the triggering result durable and counted,
	// which is the whole point of the seam.
	//
	// result is a Result[I, T] boxed as `any`, for the same reason ID's
	// argument is untyped: this package is generic over its item type and
	// Options is not. Type-assert it.
	//
	// Boxed WHOLE rather than splintered into (item, value, err): Result
	// documents that exactly one of Value and Err is meaningful, and three
	// loose parameters both discard that invariant and hand the caller a
	// non-nil `any` wrapping a nil *T on the failure path — a trap that needs
	// a godoc paragraph to survive rather than a shape that cannot spring it.
	// Done() comes along for free.
	//
	// The returned error is surfaced to the caller verbatim, so it must be a
	// code or an identifier, never prompt or response content.
	//
	// It runs on the sink goroutine with no context and no timeout of its own:
	// a blocking implementation stalls the run, exactly as a blocking SinkFunc
	// would but without PerRecordTimeout to bound it. A panic inside it is
	// recovered and ends the run rather than deadlocking it.
	//
	// It is not called again after returning an error, but IS called for
	// results already in flight during the drain that follows — the first
	// error is the one reported.
	AfterRecord func(result any) error
}

// id returns an item's identity, falling back to a placeholder rather than
// panicking if the caller omitted the function.
func (o Options) id(item any) string {
	if o.ID == nil {
		return "<unidentified>"
	}
	return o.ID(item)
}

func (o Options) recordGrace() time.Duration {
	if o.RecordGrace > 0 {
		return o.RecordGrace
	}
	return 30 * time.Second
}

func (o Options) perRecordTimeout() time.Duration {
	if o.PerRecordTimeout > 0 {
		return o.PerRecordTimeout
	}
	return 30 * time.Second
}

func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return DefaultConcurrency()
}

// DefaultConcurrency is what a zero Concurrency resolves to.
//
// Exported because a caller planning against a budget has to know the number it
// will actually run at. Treating zero as "unset, skip the check" let core's
// feasibility guard be bypassed on the CLI's default path — the one almost
// every user takes.
//
// Modest by default. The work is I/O against a rate-limited provider, so the
// useful ceiling is set by that provider, not by this machine.
func DefaultConcurrency() int {
	if n := runtime.NumCPU(); n < 8 {
		return n
	}
	return 8
}

// WorkFunc executes one item.
//
// It must honor ctx. A worker blocked past cancellation is what turns a
// graceful drain into a hung process.
type WorkFunc[I proto.Message, T any] func(ctx context.Context, item I) (*T, error)

// SinkFunc durably records one result.
//
// Called from a single goroutine, so implementations need no locking of their
// own and results are recorded in completion order. Returning an error stops
// the run: if results cannot be persisted, continuing would spend money whose
// outcome nothing can record, and resume would pay for it again.
//
// ctx carries a PerRecordTimeout deadline for THIS call. It does not inherit
// the caller's cancellation — recording outlives that on purpose — but it is
// cancelled once RecordGrace expires after the caller cancels, which is how a
// Ctrl-C eventually reaches an implementation that is still writing.
type SinkFunc[I proto.Message, T any] func(ctx context.Context, r Result[I, T]) error

// Stats reports what a run did.
type Stats struct {
	// Dispatched is the number of items handed to workers.
	Dispatched int

	// Skipped is the number recognized as already complete.
	Skipped int

	// Succeeded and Failed count DURABLY RECORDED outcomes, and partition
	// Dispatched once the run is over.
	//
	// Counted after the sink accepts them, not when work finishes, so they
	// describe outcomes that survive the process. A result the sink rejected
	// is neither, and the run ends with an error saying so — reporting it as
	// succeeded would tell a resumed run that an unrecorded item was done.
	Succeeded int
	Failed    int
}

// Recorded returns the number of outcomes durably persisted.
func (s Stats) Recorded() int { return s.Succeeded + s.Failed }

// Run executes work over items, recording each result through sink.
//
// The pipeline is producer, workers, sink:
//
//	producer                  workers (N)            sink (1)
//	────────────────          ───────────            ────────
//	range items          ──▶  recv case         ──▶  recv result
//	  clone each Case          work(ctx, case)        record durably
//	  select{ send, done }     send result
//
// Rules that make it correct, each answering a specific failure:
//
//   - The clone happens in the PRODUCER, before the send. Cloning after the
//     pointer crosses the channel leaves a window where the producer's next
//     iteration can reuse the backing memory while a worker still holds it.
//     The Ring-0 contract says yielded values are borrowed for one iteration,
//     and this is the boundary where that stops being documentation.
//
//   - Every send selects on the shutdown context. Without it a producer can
//     block forever handing off to workers that are all stuck, leaving no
//     goroutine alive to service a cancellation.
//
//   - Shutdown is bidirectional. The caller cancels top-down; a fatal iterator
//     error, a sink failure, or a panic cancels bottom-up. Both route through
//     one internal context, because a worker-side failure has to be able to
//     stop the producer's range loop.
//
//   - A fatal error from the item source stops iteration but DRAINS what is
//     already in flight. Those items' work is already paid for; discarding
//     their results would spend money and record nothing.
//
//   - Worker AND sink panics are recovered. A panic in one item must not take
//     down a run that has spent money on hundreds of others, and a panic on
//     the sink goroutine would be worse still: no caller's recover can reach
//     another goroutine, so it would kill the process mid-run.
//
//   - A completed result is ALWAYS delivered to the sink, even during
//     shutdown. The work is already paid for, and a result that never reaches
//     the sink is one a resumed run pays for again.
//
//   - Recording outlives the caller's cancellation, under a grace period the
//     cancellation itself arms. On Ctrl-C the caller's context is precisely
//     the one that died, so recording through it would drop the results it is
//     meant to preserve — and bounding the whole run instead of the drain
//     discards them just as thoroughly, which is docs/debt.md#54.
//
//   - AfterRecord is the only path from a SUCCESSFUL item to shutdown. IsFatal
//     answers for work errors; some conditions are only visible in an answer
//     the caller has already paid for and wants to keep.
//
// A sink that blocks forever stalls one result for PerRecordTimeout and is then
// reported as broken; a sink that blocks and ignores its context stalls the run
// indefinitely, which is the caller's responsibility to avoid. RecordGrace
// bounds the drain AFTER the caller cancels, not the run.
func Run[I proto.Message, T any](
	ctx context.Context,
	items iter.Seq2[I, error],
	work WorkFunc[I, T],
	sink SinkFunc[I, T],
	opts Options,
) (Stats, error) {
	if work == nil || sink == nil {
		return Stats{}, errors.New("executor: work and sink are required")
	}

	// The internal context is what lets a worker-side failure stop the
	// producer. Cancelling it never cancels the caller's.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		stats    Stats
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil && err != nil {
			firstErr = err
		}
		cancel()
	}

	n := opts.concurrency()
	cases := make(chan I)
	results := make(chan Result[I, T])

	// Sink: one goroutine, so implementations need no locking and results are
	// recorded in completion order.
	//
	// Recording outcomes must outlive the caller's cancellation: the money is
	// already spent either way. So recordCtx drops the cancellation and keeps
	// the values, and carries NO deadline of its own.
	//
	// It carried one, built here, before the first item was dispatched — which
	// made RecordGrace a bound on the whole run rather than on shutdown. On any
	// run longer than the grace the first write failed, sinkBroken latched, and
	// every result after it was silently discarded: no outcome row, absent from
	// the caller's completed set, and paid for again on resume. See
	// docs/debt.md#54.
	recordCtx, recordCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer recordCancel()

	// The grace is armed BY the caller's cancellation, which is what its godoc
	// has always described: "how long result recording may continue AFTER the
	// caller's context is cancelled".
	//
	// Not scoped to the results channel closing instead — that bounds nothing.
	// results is unbuffered and is closed only after every worker has returned,
	// so a send completes when the sink receives it and the post-close window
	// is at most one in-flight call, which perRecordTimeout already covers. The
	// window that needs a bound is the drain after Ctrl-C, where N workers each
	// finish their current item and send.
	//
	// The timer is stopped on the way out, because an unstopped 30s timer
	// holds recordCancel — and through it the whole context tree — live long
	// after the run is over.
	//
	// A flag under a mutex, not an atomic handle: stopArming() returning false
	// means the arming func has already STARTED, not that it has finished, so
	// a plain Load() can read nil in the window before it stores and leave a
	// live timer behind. That window opens on any caller whose context is
	// already cancelled when Run begins.
	var (
		graceMu      sync.Mutex
		graceTimer   *time.Timer
		graceStopped bool
	)
	stopArming := context.AfterFunc(ctx, func() {
		graceMu.Lock()
		defer graceMu.Unlock()
		if graceStopped {
			return
		}
		graceTimer = time.AfterFunc(opts.recordGrace(), recordCancel)
	})
	defer func() {
		stopArming()
		graceMu.Lock()
		defer graceMu.Unlock()
		graceStopped = true
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()

	sinkBroken := false
	gateFired := false
	sinkDone := make(chan struct{})
	go func() {
		// #5: the sink is caller-supplied code doing serialization and I/O,
		// exactly as fallible as the work function. An unrecovered panic in
		// THIS goroutine cannot be caught by the CLI's top-level recover — it
		// takes the process down mid-run, losing every in-flight result.
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("panic recording results: %T", r))
			}
		}()
		defer close(sinkDone)
		for r := range results {
			if sinkBroken {
				// A sink that has already failed is not asked again. Nothing
				// in SinkFunc's contract says it must tolerate being called
				// after returning an error, and the run is ending regardless.
				// Draining continues so workers are never left blocked.
				continue
			}

			// recordCtx deliberately does NOT inherit the caller's
			// cancellation. On Ctrl-C the caller's context is the thing that
			// died, so passing it here would ask a store to persist results
			// while handing it an already-dead context — losing exactly the
			// record resume depends on. Values are preserved; only the
			// cancellation is dropped.
			// Per CALL, not per run. A sink that hangs on one write must not
			// hang the run, and a run that takes longer than one write's
			// budget is not a failure.
			callCtx, callCancel := context.WithTimeout(recordCtx, opts.perRecordTimeout())
			err := recordOne(callCtx, sink, r)
			callCancel()
			if err != nil {
				sinkBroken = true
				recErr := fmt.Errorf("recording result for %s: %w", opts.id(r.Item), err)
				// When the grace has expired, this failure IS the
				// cancellation, and the run's classification must not depend
				// on whether the sink happened to return a wrapped ctx.Err().
				// Without the join, a store surfacing its own error text turns
				// a Ctrl-C into RUN_STATUS_FAILED and a generic exit code — so
				// a CI gate keying on the interrupted code flips the day a
				// driver rewords.
				if cause := ctx.Err(); cause != nil {
					recErr = errors.Join(recErr, cause)
				}
				fail(recErr)
				continue
			}

			// Counted only once durably recorded, so Succeeded and Failed
			// describe outcomes that actually survive the process.
			mu.Lock()
			if r.Done() {
				stats.Succeeded++
			} else {
				stats.Failed++
			}
			mu.Unlock()

			// AFTER the record and after the count: the result is durable and
			// the run is ending, so the caller's own view of what completed
			// must include it. Ending the run here rather than by failing the
			// item is the whole point — the item succeeded, and the store says
			// so.
			//
			// Draining continues; the loop is not broken out of. Workers still
			// in flight have results to deliver, and a result that never
			// reaches the sink is one a resumed run pays for again.
			// Guarded, like the sink and the work function beside it. A panic
			// here unwinds out of `for r := range results`, and nothing else
			// drains that channel — every worker's send is unconditional by
			// design, so they block forever and workers.Wait() never returns.
			// The run hangs permanently and only SIGKILL ends it, losing every
			// in-flight result and re-paying for them on resume. Reproduced
			// before this guard existed.
			if opts.AfterRecord != nil && !gateFired {
				if err := afterRecordOne(opts.AfterRecord, r, opts.id(r.Item)); err != nil {
					gateFired = true
					fail(err)
				}
			}
		}
	}()

	// Workers.
	var workers sync.WaitGroup
	for range n {
		workers.Go(func() {
			for c := range cases {
				value, err := runOne(runCtx, work, c, opts.id(c))

				// UNCONDITIONAL send. An earlier version selected on
				// runCtx.Done() here, which was the most damaging bug in this
				// package: once cancelled, that select raced a ready-forever
				// Done channel against a sink that is momentarily busy writing
				// to disk, so on Ctrl-C most in-flight results were thrown
				// away. Each one is work already performed and money already
				// spent, and a result that never reaches the sink is never in
				// CompletedCases — so a resumed run pays for it again.
				//
				// This cannot block indefinitely: the sink drains `results`
				// until it is closed, and it is closed only after every worker
				// has returned.
				results <- Result[I, T]{Item: c, Value: value, Err: err}

				if opts.IsFatal != nil && err != nil && opts.IsFatal(err) {
					// The work reported something that ends the run rather than
					// just this item — budget exhaustion being the case M1-5
					// needs. The result above is already on its way to the
					// sink; this stops new work from being dispatched.
					fail(err)
					return
				}
			}
		})
	}

	// Producer.
	producerErr := func() error {
		defer close(cases)

		for c, err := range items {
			if err != nil {
				// Fatal by the Ring-0 contract. Stop reading, but let what is
				// already in flight finish and be recorded.
				return fmt.Errorf("reading items: %w", err)
			}
			if err := runCtx.Err(); err != nil {
				return nil // shutting down; not this loop's error to report
			}

			// Skip runs outside the lock: resume's implementation consults a
			// store, and holding the sink's mutex across that would serialize
			// every result's accounting behind it.
			if opts.Skip != nil && opts.Skip(opts.id(c)) {
				mu.Lock()
				stats.Skipped++
				mu.Unlock()
				continue
			}

			// Clone BEFORE the send. The source may reuse its backing memory
			// on the next iteration, and the worker outlives this one.
			clone, ok := proto.Clone(c).(I)
			if !ok {
				return fmt.Errorf("cloning item %s produced the wrong type", opts.id(c))
			}

			select {
			case cases <- clone:
				// Counted AFTER the handoff. Counting before it meant an item
				// that lost the race to shutdown was reported as dispatched
				// despite never reaching a worker, breaking the
				// Dispatched == Succeeded + Failed invariant M1-5 uses to
				// compute the Run's case counts.
				mu.Lock()
				stats.Dispatched++
				mu.Unlock()
			case <-runCtx.Done():
				return nil
			}
		}
		return nil
	}()

	workers.Wait()
	close(results)
	<-sinkDone

	mu.Lock()
	defer mu.Unlock()

	// A producer error is reported only if nothing worse happened first: a
	// sink failure means results are being lost, which matters more than the
	// source running out.
	if firstErr != nil {
		return stats, firstErr
	}
	if producerErr != nil {
		return stats, producerErr
	}
	// A cancelled caller context is reported so the caller can distinguish a
	// completed run from an interrupted one — the difference between a result
	// and a checkpoint.
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

// recordOne calls the sink, converting a panic into an error.
//
// A panic here would otherwise escape on the sink goroutine, where no caller's
// recover can reach it, and take the process down mid-run.
func recordOne[I proto.Message, T any](ctx context.Context, sink SinkFunc[I, T], r Result[I, T]) (err error) {
	defer func() {
		if p := recover(); p != nil {
			// %T, not %v: a panic value is arbitrary and may embed prompt or
			// response content, and this error is persisted.
			err = fmt.Errorf("panic in sink: %T", p)
		}
	}()
	return sink(ctx, r)
}

// afterRecordOne runs the caller's hook, converting a panic into an error.
//
// Unguarded, a panic here is strictly worse than a panic in the sink: it
// unwinds out of the loop that drains `results`, and every worker's send into
// that channel is unconditional, so the whole run deadlocks with no path to
// recovery. The sink goroutine's own recover cannot help — it fires after the
// loop is already gone.
func afterRecordOne[I proto.Message, T any](
	hook func(result any) error, r Result[I, T], id string,
) (err error) {
	defer func() {
		if p := recover(); p != nil {
			// %T, not %v: a panic value is arbitrary and may embed prompt or
			// response content, and this error reaches the user.
			err = fmt.Errorf("panic in AfterRecord for %s: %T", id, p)
		}
	}()
	return hook(r)
}

// runOne executes a single item, converting a panic into an error.
//
// A panic in one item's work must not take down a run that has already spent
// money on hundreds of others. CLAUDE.md reserves panics for programmer error;
// this boundary makes one item's programmer error survivable and recorded
// rather than fatal to the whole run.
func runOne[I proto.Message, T any](ctx context.Context, work WorkFunc[I, T], item I, id string) (value *T, err error) {
	defer func() {
		if p := recover(); p != nil {
			value = nil
			// %T rather than %v. A panic value is arbitrary — it may embed a
			// prompt, a response, or a formatted assertion containing either —
			// and this error is persisted as Outcome.Err, which store.go
			// requires to be a code rather than verbatim content.
			err = fmt.Errorf("panic executing item %s: %T", id, p)
		}
	}()
	return work(ctx, item)
}
