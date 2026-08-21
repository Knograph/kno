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

	"github.com/knograph/kno/core"
)

// Result is one item's terminal outcome.
//
// Exactly one of Value or Err is meaningful, mirroring the store's Outcome and
// the event stream's CaseScored/CaseErrored split. An item is either done or
// failed; it is never both, because a shape permitting both would let one item
// land on both sides of the denominator.
type Result[T any] struct {
	// Item is the input this result belongs to.
	Item *core.Case

	// Value is the work's output. Nil when Err is set.
	Value *T

	// Err is the terminal failure. Nil when the work succeeded.
	Err error
}

// Done reports whether the item succeeded.
func (r Result[T]) Done() bool { return r.Err == nil }

// Options configures a Run.
type Options struct {
	// Concurrency bounds in-flight work. Zero means a conservative default.
	//
	// The constraint is provider rate limits, not CPU: a bigger pool buys
	// nothing but a faster path to a 429.
	Concurrency int

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
	RecordGrace time.Duration
}

func (o Options) recordGrace() time.Duration {
	if o.RecordGrace > 0 {
		return o.RecordGrace
	}
	return 30 * time.Second
}

func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	// Modest by default. The work is I/O against a rate-limited provider, so
	// the useful ceiling is set by that provider, not by this machine.
	if n := runtime.NumCPU(); n < 8 {
		return n
	}
	return 8
}

// WorkFunc executes one item.
//
// It must honor ctx. A worker blocked past cancellation is what turns a
// graceful drain into a hung process.
type WorkFunc[T any] func(ctx context.Context, c *core.Case) (*T, error)

// SinkFunc durably records one result.
//
// Called from a single goroutine, so implementations need no locking of their
// own and results are recorded in completion order. Returning an error stops
// the run: if results cannot be persisted, continuing would spend money whose
// outcome nothing can record, and resume would pay for it again.
type SinkFunc[T any] func(ctx context.Context, r Result[T]) error

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
//   - Recording outlives the caller's cancellation, under a bounded grace
//     period. On Ctrl-C the caller's context is precisely the one that died,
//     so recording through it would drop the results it is meant to preserve.
//
// A sink that blocks forever will stall a run for RecordGrace and then be
// abandoned; a sink that blocks and ignores its context will stall it
// indefinitely, which is the caller's responsibility to avoid.
func Run[T any](
	ctx context.Context,
	items iter.Seq2[*core.Case, error],
	work WorkFunc[T],
	sink SinkFunc[T],
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
	cases := make(chan *core.Case)
	results := make(chan Result[T])

	// Sink: one goroutine, so implementations need no locking and results are
	// recorded in completion order.
	// Recording outcomes must outlive the caller's cancellation: the money is
	// already spent either way. The grace period bounds how long a hung sink
	// can delay shutdown.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), opts.recordGrace())
	defer recordCancel()

	sinkBroken := false
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
			// cancellation is dropped, under its own bounded deadline so a
			// hung sink cannot make shutdown unbounded.
			if err := recordOne(recordCtx, sink, r); err != nil {
				sinkBroken = true
				fail(fmt.Errorf("recording result for %s: %w", r.Item.GetId(), err))
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
		}
	}()

	// Workers.
	var workers sync.WaitGroup
	for range n {
		workers.Go(func() {
			for c := range cases {
				value, err := runOne(runCtx, work, c)

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
				results <- Result[T]{Item: c, Value: value, Err: err}

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
			if opts.Skip != nil && opts.Skip(c.GetId()) {
				mu.Lock()
				stats.Skipped++
				mu.Unlock()
				continue
			}

			// Clone BEFORE the send. The source may reuse its backing memory
			// on the next iteration, and the worker outlives this one.
			clone, ok := proto.Clone(c).(*core.Case)
			if !ok {
				return fmt.Errorf("cloning case %s produced the wrong type", c.GetId())
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
func recordOne[T any](ctx context.Context, sink SinkFunc[T], r Result[T]) (err error) {
	defer func() {
		if p := recover(); p != nil {
			// %T, not %v: a panic value is arbitrary and may embed prompt or
			// response content, and this error is persisted.
			err = fmt.Errorf("panic in sink: %T", p)
		}
	}()
	return sink(ctx, r)
}

// runOne executes a single item, converting a panic into an error.
//
// A panic in one item's work must not take down a run that has already spent
// money on hundreds of others. CLAUDE.md reserves panics for programmer error;
// this boundary makes one item's programmer error survivable and recorded
// rather than fatal to the whole run.
func runOne[T any](ctx context.Context, work WorkFunc[T], c *core.Case) (value *T, err error) {
	defer func() {
		if p := recover(); p != nil {
			value = nil
			// %T rather than %v. A panic value is arbitrary — it may embed a
			// prompt, a response, or a formatted assertion containing either —
			// and this error is persisted as Outcome.Err, which store.go
			// requires to be a code rather than verbatim content.
			err = fmt.Errorf("panic executing case %s: %T", c.GetId(), p)
		}
	}()
	return work(ctx, c)
}
