// Package executor runs work over a bounded pool with checkpointing.
package executor

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"sync"

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

	// Succeeded and Failed partition Dispatched, once the run is over.
	Succeeded int
	Failed    int
}

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
//   - Worker panics are recovered at the worker boundary and recorded as
//     failures. A panic in one item must not take down a run that has spent
//     money on hundreds of others.
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
	sinkDone := make(chan struct{})
	go func() {
		defer close(sinkDone)
		for r := range results {
			mu.Lock()
			if r.Done() {
				stats.Succeeded++
			} else {
				stats.Failed++
			}
			mu.Unlock()

			// The sink records outcomes for work already performed, so it uses
			// the CALLER's context rather than runCtx. Using runCtx would mean
			// a cancellation raced the recording of results whose money is
			// already spent — losing exactly the record resume depends on.
			if err := sink(ctx, r); err != nil {
				fail(fmt.Errorf("recording result for %s: %w", r.Item.GetId(), err))
			}
		}
	}()

	// Workers.
	var workers sync.WaitGroup
	for range n {
		workers.Go(func() {
			for c := range cases {
				value, err := runOne(runCtx, work, c)
				select {
				case results <- Result[T]{Item: c, Value: value, Err: err}:
				case <-runCtx.Done():
					// Shutting down. The result is dropped deliberately: the
					// sink is closing, and a send here would block forever.
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

			mu.Lock()
			skip := opts.Skip != nil && opts.Skip(c.GetId())
			if skip {
				stats.Skipped++
			} else {
				stats.Dispatched++
			}
			mu.Unlock()
			if skip {
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

// runOne executes a single item, converting a panic into an error.
//
// A panic in one item's work must not take down a run that has already spent
// money on hundreds of others. CLAUDE.md reserves panics for programmer error;
// this boundary makes one item's programmer error survivable and recorded
// rather than fatal to the whole run.
func runOne[T any](ctx context.Context, work WorkFunc[T], c *core.Case) (value *T, err error) {
	defer func() {
		if r := recover(); r != nil {
			value = nil
			err = fmt.Errorf("panic executing case %s: %v", c.GetId(), r)
		}
	}()
	return work(ctx, c)
}
