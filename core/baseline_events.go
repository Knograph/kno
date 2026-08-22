package core

import (
	"context"
	"errors"
	"fmt"
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

	// unrecoverable counts Cases that scored but whose number is gone, because
	// they were purged before the score lived in its own column.
	//
	// They cannot be averaged in as zero: that drags the mean toward zero and
	// presents the result as the run's actual aggregate, which is worse than
	// reporting nothing. The mean refuses instead.
	unrecoverable int
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

// mean returns the aggregate over scored Cases, or nil when nothing scored.
//
// Nil rather than zero: a run that scored nothing has no mean, and a zero here
// would be indistinguishable from a real mean of zero.
// mean reports the aggregate over every scored Case in the run, or nil.
//
// Nil for two different reasons, and conflating them would be dishonest in
// opposite directions. Nothing scored: there is no mean. Something scored but
// its number is unrecoverable: there is a mean and we cannot compute it, so
// reporting the partial one would present a number nobody can reproduce.
func (a *aggregator) mean() *float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.unrecoverable > 0 {
		return nil
	}
	total := a.scored + a.priorScored
	if total == 0 {
		return nil
	}
	m := (a.sum + a.priorSum) / float64(total)
	return &m
}

// aggregateUnavailable reports whether a mean exists but cannot be computed.
//
// Distinct from "nothing scored", so a caller can say which it is rather than
// showing the same blank for both.
func (a *aggregator) aggregateUnavailable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unrecoverable > 0
}

// next returns the next event sequence number.
func (a *aggregator) next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return a.seq
}

// seedCounts carries a prior run's outcome counts into this process.
//
// Only the counts, not the score sum: the individual Scores live in the store
// and are not re-read, so a resumed run's aggregate is the mean over the Cases
// IT scored. That is recorded as debt rather than silently presented as the
// whole run's mean — see docs/debt.md#27.
// seedCounts carries an interrupted run's totals into this process.
//
// sum and unrecoverable come from the store's score column, which is why
// docs/debt.md#25 requires a purge to null the trace blobs and never the row:
// the number survives a purge precisely so this can read it.
func (a *aggregator) seedCounts(scored, errored int, sum float64, unrecoverable int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.priorScored = scored
	a.priorErrored = errored
	a.priorSum = sum
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
