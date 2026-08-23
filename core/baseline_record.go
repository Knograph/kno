package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// Recording what one Case did: persisting its outcome, classifying its error,
// counting it, and emitting its event.
//
// The sink and the emitter live together because they share an invariant that
// nothing else enforces. sinkFunc skips persisting a Case on three predicates
// and emit skips on one, and they agree only because sinkFunc is emit's sole
// caller and returns first in the other two. Adding a fourth skip to one and
// not the other makes the outcomes table and the reported counts disagree —
// the failure workFunc's own comment records as having cost real money to
// find. Separated across files, that pair is invisible.

// aggregator accumulates the run's score over SCORED Cases only.
//
// Errored Cases are excluded rather than counted as zero. An agent that
// returned a 500 did not answer badly, it did not answer — scoring
// infrastructure failure as task failure biases the baseline downward and makes
// every later Asset look better than it is. The counts are reported separately
// so the exclusion is visible rather than implied.
type aggregator struct {
	mu      sync.Mutex
	sum     float64
	scored  int
	errored int
	seq     int64

	// Counts carried over from an interrupted run, so the totals describe the
	// whole run rather than only the resumed portion.
	priorScored  int
	priorErrored int

	// priorSum is the score total from Cases earlier processes recorded.
	//
	// Seeded alongside the counts. Without it the counts spanned the whole run
	// while the mean spanned only the tail, so a run interrupted after 24 Cases
	// and resumed for 36 reported "60 scored" beside a mean of the last 36 —
	// a denominator and a numerator describing different populations.
	priorSum float64

	// priorCounted is how many prior Cases actually contributed to priorSum.
	//
	// NOT priorScored. The two come from different queries over different
	// predicates: priorScored counts rows marked scored, priorCounted counts
	// rows holding a number. They agree only while every scored row has a
	// score, which nothing in the schema enforces. Dividing priorSum by
	// priorScored would put a numerator and a denominator from two separate
	// reads into one number — the defect this whole change repays, one level
	// down. The store returns both from a single query for that reason.
	priorCounted int

	// unrecoverable counts Cases that scored but whose number is gone —
	// purged before the score lived in its own column, or holding a Score blob
	// that could not be read back.
	//
	// They cannot be averaged in as zero: that drags the mean toward zero and
	// presents the result as the run's actual aggregate, which is worse than
	// reporting nothing. The mean refuses instead.
	unrecoverable int
}

// snapshot reads every reported number under one lock.
//
// Taken together rather than through separate accessors: the counts, the mean
// and the refusal are rendered side by side, and reading them at three
// instants lets a live consumer print a mean over a denominator it never had.
// Today closeRun runs after the executor has drained, so nothing tears — this
// keeps that true when a progress view reads the aggregator mid-run.
func (a *aggregator) snapshot() (scored, errored int, mean *float64, unavailable bool, lost int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scored + a.priorScored,
		a.errored + a.priorErrored,
		a.meanLocked(),
		a.unrecoverable > 0,
		a.unrecoverable
}

func (a *aggregator) add(value float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sum += value
	a.scored++
}

func (a *aggregator) addError() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errored++
}

func (a *aggregator) counts() (scored, errored int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scored + a.priorScored, a.errored + a.priorErrored
}

// mean reports the aggregate over every scored Case in the run, or nil.
//
// Nil for three different reasons, and conflating them would be dishonest in
// different directions. Nothing scored: there is no mean. Something scored but
// its number is unrecoverable: there is a mean and we cannot compute it, so
// reporting the partial one would present a number nobody can reproduce. A
// score arrived as NaN: every later arithmetic result is NaN too, and a
// resumed run would carry it forward through the stored sum forever.
//
// Callers that also report the counts should use snapshot instead, so the two
// come from one read.
func (a *aggregator) mean() *float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meanLocked()
}

func (a *aggregator) meanLocked() *float64 {
	if a.unrecoverable > 0 {
		return nil
	}
	total := a.scored + a.priorCounted
	if total == 0 {
		return nil
	}
	m := (a.sum + a.priorSum) / float64(total)
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return nil
	}
	return &m
}

// next returns the next event sequence number.
func (a *aggregator) next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return a.seq
}

// seedCounts carries an interrupted run's totals into this process.
//
// sum and unrecoverable come from the store's score column, which is why
// docs/debt.md#25 requires a purge to null the trace blobs and never the row:
// the number survives a purge precisely so this can read it.
func (a *aggregator) seedCounts(scored, errored int, sum float64, counted, unrecoverable int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.priorScored = scored
	a.priorErrored = errored
	a.priorSum = sum
	a.priorCounted = counted
	a.unrecoverable = unrecoverable
}

// seedSequence continues numbering after a resume rather than restarting at 1,
// which would collide with events from before the interruption.
func (a *aggregator) seedSequence(from int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq = from
}

// emit records one event for a Case's outcome.
//
// Events carry identifiers and metrics, never content. The Case's input and the
// agent's output stay in the store, which is the only package that handles
// trace content.
func (o BaselineOptions) emit(ctx context.Context, r executor.Result[*Case, caseOutcome], agg *aggregator) error {
	ev := &knov1.Event{
		RunId:     o.RunID,
		EmittedAt: o.now().Format(time.RFC3339),
		Sequence:  agg.next(),
	}

	// A budget refusal is not an outcome for this Case — it was never
	// attempted. Emitting one would put it in the errored count and make the
	// three counts describe work that did not happen.
	if errors.Is(r.Err, errs.ErrBudgetExceeded) {
		return nil
	}

	if r.Done() {
		// Counted here, after the outcome is persisted, so the Run's counts can
		// never outrun the outcomes table.
		agg.add(r.Value.Score.GetValue())
		ev.Payload = &knov1.Event_CaseScored{CaseScored: &knov1.CaseScored{
			CaseId:        r.Item.GetId(),
			Score:         r.Value.Score.GetValue(),
			Passed:        r.Value.Score.GetPassed(),
			CostUsdMicros: r.Value.Response.GetCostUsdMicros(),
			LatencyMs:     r.Value.Response.GetLatencyMs(),
		}}
	} else {
		agg.addError()
		ev.Payload = &knov1.Event_CaseErrored{CaseErrored: &knov1.CaseErrored{
			CaseId: r.Item.GetId(),
			Error: &knov1.EventError{
				Code:    codeOf(r.Err),
				Message: "the agent did not return an answer",
			},
			// Retries are exhausted before an outcome reaches here —
			// invokeWithRetry returns only a terminal result — so every
			// errored Case counts once.
			WillRetry: false,
		}}
	}

	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending event for %s: %w", r.Item.GetId(), err)
	}
	return nil
}

// emitRunStarted opens the event stream.
func (o BaselineOptions) emitRunStarted(ctx context.Context, agg *aggregator, total int) error {
	ev := &knov1.Event{
		RunId:     o.RunID,
		EmittedAt: o.now().Format(time.RFC3339),
		Sequence:  agg.next(),
		Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
			Stage:         knov1.Stage_STAGE_BASELINE,
			Agent:         o.AgentRef,
			GoalName:      o.GoalName,
			GoalDirection: o.Goal.Direction(),
			TotalCases:    int32(total), //nolint:gosec // bounded by the eval set
		}},
	}
	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending run-started event: %w", err)
	}
	return nil
}

// emitRunFinished closes the event stream. Always the last event.
func (o BaselineOptions) emitRunFinished(ctx context.Context, agg *aggregator, run *knov1.Run) error {
	scored, errored := agg.counts()

	finished := &knov1.RunFinished{
		Status:           run.GetStatus(),
		IncompleteReason: run.GetIncompleteReason(),
		Attempted:        int32(scored + errored), //nolint:gosec // bounded by the eval set
		Scored:           int32(scored),           //nolint:gosec // bounded by the eval set
		Errored:          int32(errored),          //nolint:gosec // bounded by the eval set
	}
	if m := agg.mean(); m != nil {
		finished.AggregateScore = proto.Float64(*m)
	}

	ev := &knov1.Event{
		RunId:     o.RunID,
		EmittedAt: o.now().Format(time.RFC3339),
		Sequence:  agg.next(),
		Payload:   &knov1.Event_RunFinished{RunFinished: finished},
	}
	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending run-finished event: %w", err)
	}
	return nil
}

// sinkFunc persists each Case's outcome and emits its event.
//
// runCtx is the RUN's context, distinct from the ctx the sink is called with:
// the sink deliberately runs on a context that outlives cancellation so it can
// still write during shutdown, which means it cannot ask its own ctx whether
// the run is ending. draining covers the other way a run stops — a fatal error
// such as a budget stop, which never touches the caller's context.
func (o BaselineOptions) sinkFunc(runCtx context.Context, draining *atomic.Bool, agg *aggregator) executor.SinkFunc[*Case, caseOutcome] {
	return func(ctx context.Context, r executor.Result[*Case, caseOutcome]) error {
		// A Case refused by the budget guard was never attempted: no provider
		// call was made and nothing was spent. Recording it as a terminal
		// outcome would mark it complete, so a resumed run would SKIP it — the
		// Case would vanish from the run permanently, and the denominator
		// would shrink with nothing showing why.
		//
		// It is left unrecorded so the resume picks it up, which is the whole
		// point of stopping resumably rather than failing.
		//
		// A Case that could not be PRICED is the same shape: estimate() refuses
		// before Authorize is ever called, so no provider call was made and
		// nothing was spent. Recording it would charge a resumed run for a call
		// that never happened AND mark the Case done, so fixing the pricing
		// table and re-running with --resume would never re-attempt it.
		if errors.Is(r.Err, errs.ErrBudgetExceeded) || errors.Is(r.Err, errUnpriceable) {
			return nil
		}

		// A Case the shutdown cancelled before it produced anything is the same
		// shape again: the run is stopping resumably, and this Case has no
		// result to record.
		//
		// Recording it would mark it complete, so a resume would SKIP it — and
		// the run would report a smaller denominator than it measured, with
		// nothing saying why. Measured: a budget stop at concurrency 8 with a
		// 50ms agent recorded 2 errored Cases every single time, and the
		// resumed run scored 51 of 52 rather than 52. CI caught it as a flaky
		// test; it is not flaky, it is timing-dependent.
		//
		// The trade is deliberate. Not recording means the resumed run does not
		// restore whatever that attempt may have cost, so it gets slightly more
		// headroom than it should — bounded by concurrency, and already the
		// documented dark-spend window (docs/debt.md#20). Losing a Case from
		// the run permanently, silently, is the worse failure: prime directive
		// 5 is what makes the denominator behind every later delta mean
		// something.
		//
		// Two conditions, and both matter.
		//
		// The RUN's context must be done. A per-Case deadline against a healthy
		// run is a provider timeout: that Case genuinely failed, it is recorded,
		// and a run where enough of them time out is marked unusable. Skipping
		// those would hide a broken provider behind a shrinking denominator —
		// the opposite failure, and an existing test caught me making it.
		//
		// And there must be no Response. A Case that failed AFTER a paid call
		// produced one is a real terminal outcome, recorded below with the
		// spend that call incurred.
		shuttingDown := runCtx.Err() != nil || draining.Load()
		cancelled := errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded)
		noResult := r.Value == nil || r.Value.Response == nil

		if shuttingDown && cancelled && noResult {
			return nil
		}

		out := &store.Outcome{CaseID: r.Item.GetId()}

		switch {
		case r.Done():
			out.Response = r.Value.Response
			out.Score = r.Value.Score
			out.Spend = spendOfN(r.Value.Response, int64(r.Value.Attempts))
		default:
			out.Err = codeOf(r.Err)
			// A Case can fail AFTER a paid call — a Goal erroring on malformed
			// output, for instance. A flat one-call spend there understates
			// what was actually spent, and SettledSpend is what Guard.Restore
			// reads on resume: the resumed process would believe less was
			// spent than really was, reopening the amnesia M1-0 closed.
			if r.Value != nil && r.Value.Response != nil {
				out.Response = r.Value.Response
				out.Spend = spendOfN(r.Value.Response, int64(r.Value.Attempts))
			} else {
				// No Response, which is the retry-EXHAUSTED path — every
				// attempt failed. Hardcoding one call here is what made the
				// headline fix miss the branch it was written for: measured 5
				// persisted against 15 settled with MaxAttempts 3.
				out.Spend = budget.Spend{Calls: attemptsOf(r.Value)}
			}
		}

		if err := o.Store.RecordOutcome(ctx, o.RunID, out); err != nil {
			return fmt.Errorf("recording %s: %w", r.Item.GetId(), err)
		}
		return o.emit(ctx, r, agg)
	}
}

// caseOutcome is one Case's result inside the executor.
type caseOutcome struct {
	// Attempts is how many provider calls this Case took. Persisted so the
	// store's spend matches what the guard actually settled.
	Attempts int

	Response *Response
	Score    *Score
	Err      error
}

// codeOf extracts a machine-readable code, never verbatim provider text.
func codeOf(err error) string {
	var a *errs.Actionable
	if errors.As(err, &a) {
		return a.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "INTERRUPTED"
	}
	return "AGENT_ERROR"
}
