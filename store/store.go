// Package store persists runs, traces, scores, and events.
package store

import (
	"context"
	"errors"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Errors returned by every Store implementation.
var (
	// ErrRunNotFound means no run exists with the given ID.
	ErrRunNotFound = errors.New("store: run not found")

	// ErrRunExists means a run with that ID was already created.
	ErrRunExists = errors.New("store: run already exists")
)

// Store is durable state for a run.
//
// The interface is deliberately narrow and says nothing about how writes are
// serialized, so a Postgres backend inherits no constraint from SQLite having
// been written first. The SQLite implementation relies on WAL — one writer
// alongside many readers — rather than funnelling writes through a goroutine
// of its own.
//
// Traces are customer data. Response.output may contain end-user conversation
// content, so this package is the only one that handles it, no query result is
// logged above DEBUG, and retention is documented rather than assumed.
type Store interface {
	// CreateRun records a new run. Returns ErrRunExists if the ID is taken.
	CreateRun(ctx context.Context, run *knov1.Run) error

	// GetRun loads a run by ID. Returns ErrRunNotFound if absent.
	GetRun(ctx context.Context, runID string) (*knov1.Run, error)

	// FinishRun records how a run ended, along with its final counts.
	FinishRun(ctx context.Context, run *knov1.Run) error

	// RecordOutcome durably records one Case's terminal outcome — its
	// Response, its Score if it produced one, and the spend it incurred — in a
	// SINGLE transaction.
	//
	// Atomicity here is the entire point. If the outcome and a separate "this
	// Case is done" marker were written in two transactions, a crash between
	// them would leave a Case that cost real money looking un-run, and resume
	// would pay for it a second time. There is no separate marker: the
	// recorded outcome IS the marker.
	//
	// Idempotent on (run_id, case_id). Re-recording a Case that already has an
	// outcome is a no-op rather than a second row, because a duplicate would
	// change the denominator behind every delta computed against this run.
	RecordOutcome(ctx context.Context, runID string, out *Outcome) error

	// Purge removes trace content from a run without making it un-resumable,
	// returning how many outcomes were affected.
	//
	// Implementations MUST NOT delete outcome rows. The outcome row is the
	// done-marker; deleting it would make a purged run pay for every Case a
	// second time on resume. See docs/debt.md#25.
	Purge(ctx context.Context, runID string) (int64, error)

	// ScoreSum returns the sum of recorded scores, how many Cases contributed
	// one, and how many scored but can no longer contribute — their number
	// having been purged before it was stored in a column.
	//
	// The third value exists so a caller can refuse to report an aggregate
	// rather than report one biased toward zero.
	ScoreSum(ctx context.Context, runID string) (sum float64, counted, unrecoverable int, err error)

	// CompletedCases returns the IDs of every Case with a terminal outcome.
	//
	// Resume uses this to skip finished work. It loads the full set into
	// memory — see docs/debt.md#22 for the bound and why the alternative, a
	// query per Case, is worse.
	CompletedCases(ctx context.Context, runID string) (map[string]struct{}, error)

	// OutcomeCounts reports how many Cases a run has scored and how many have
	// terminally errored.
	//
	// Resume needs these for the same reason it needs SettledSpend: the
	// in-memory aggregate starts empty in each process, so without them a
	// resumed run reports counts covering only the work it did — losing the
	// Cases the interrupted run already paid for, and understating the
	// denominator behind every delta later measured against it.
	OutcomeCounts(ctx context.Context, runID string) (scored, errored int, err error)

	// SettledSpend sums what a run actually spent, for reseeding the budget
	// guard on resume.
	//
	// The guard is in-memory, so this is the only durable record of money
	// spent. Without it a resumed run starts at zero and can spend its cap a
	// second time.
	SettledSpend(ctx context.Context, runID string) (budget.Spend, error)

	// AppendEvent records one event. The caller owns ordering and must set
	// Sequence.
	AppendEvent(ctx context.Context, ev *knov1.Event) error

	// MaxEventSequence returns the highest sequence recorded for a run, or 0
	// if none.
	//
	// Resume continues from this plus one rather than restarting at 1, which
	// would collide with events from before the interruption and silently
	// defeat the gap detection Event.sequence exists for.
	MaxEventSequence(ctx context.Context, runID string) (int64, error)

	// Close releases resources. Safe to call more than once, and safe to call
	// while other calls are in flight — those return an error rather than
	// racing, which is what the executor's drain-then-close shutdown needs.
	Close() error
}

// Outcome is one Case's terminal result.
//
// Exactly one of Score or Err is set. That mirrors the event stream's split
// between CaseScored and CaseErrored, for the same reason: an errored Case is
// not a scored Case, and a shape permitting both would let one Case land on
// both sides of the denominator.
type Outcome struct {
	// CaseID identifies the Case.
	CaseID string

	// Response is what the agent returned. Nil when the call failed before
	// producing one.
	Response *knov1.Response

	// Score is the Goal's judgement. Nil when the Case errored.
	Score *knov1.Score

	// Err is the terminal failure's machine-readable code, empty when the Case
	// scored.
	//
	// The code, not the verbatim provider text. Full error text lives on the
	// Response, under the same handling as any other trace content.
	Err string

	// Spend is what this Case cost, including any failed attempts preceding a
	// successful retry. One Case yields one outcome but may incur several
	// charges.
	Spend budget.Spend
}

// Scored reports whether this outcome produced a Score.
func (o *Outcome) Scored() bool { return o.Score != nil && o.Err == "" }
