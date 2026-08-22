package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"google.golang.org/protobuf/proto"
)

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
			// Retries are not implemented in this stage, so every errored
			// Case here is terminal and counts once toward errored.
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
