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

	// ErrPortfolioNotFound means no Portfolio is recorded for the given run.
	ErrPortfolioNotFound = errors.New("store: portfolio not found")

	// ErrGapsNotFound means no gaps record is recorded for the given run.
	// Absence is an answer: the run predates cluster data.
	ErrGapsNotFound = errors.New("store: gaps not found")
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

	// PurgeableCount reports how many outcomes still hold trace content, so a
	// confirmation prompt can state what it would remove rather than assert it.
	PurgeableCount(ctx context.Context, runID string) (int, error)

	// ScoreSum returns what a run's recorded scores add up to, and what could
	// not be added up.
	//
	// The counts of what could not be added up exist so a caller can refuse to
	// report an aggregate rather than report one biased toward zero, and they
	// are separate because "the user purged this" and "an older binary wrote
	// this" are different diagnoses. See ScoreSummary.
	ScoreSum(ctx context.Context, runID string) (ScoreSummary, error)

	// RecordMeasurement durably records one Case measured once, for one Asset,
	// in one arm — the Value stage's analogue of RecordOutcome, and subject to
	// the same atomicity contract: the recorded row IS the done-marker.
	//
	// A separate method and a separate table because outcomes is keyed
	// (run_id, case_id) and Value measures one Case against many Assets. Given
	// that key, all but the first measurement of a Case would be silently
	// discarded after being paid for.
	RecordMeasurement(ctx context.Context, runID string, m *Measurement) error

	// CompletedMeasurements returns the key of every measurement already
	// recorded, which is what a Value resume consults.
	//
	// It must be this rather than CompletedCases: that method reads outcomes,
	// which is empty for every Value run, so a resume driven by it would find
	// nothing done and re-pay for the entire run.
	CompletedMeasurements(ctx context.Context, runID string) (map[MeasurementKey]struct{}, error)

	// MeasurementCounts aggregates a run's measurements from the durable
	// rows: attempted, scored, errored. Value's close reads this rather than
	// in-memory counters, so a resumed run's CaseExecution describes the
	// WHOLE run, first process included.
	MeasurementCounts(ctx context.Context, runID string) (attempted, scored, errored int32, err error)

	// CaseScores returns the recorded score of every Case in a run that
	// produced one, distinguishing "no score" from "scored, number gone".
	//
	// Value pairs a fresh measurement against the BASELINE's recorded score, so
	// this is called with a baseline run's ID. A map[string]float64 would
	// collapse an unrecoverable score into an absent one, and pairing against
	// the resulting zero would manufacture a delta.
	CaseScores(ctx context.Context, runID string) (map[string]CaseScore, error)

	// Measurements returns everything recorded for one Asset in a run: each
	// measurement's key, what it scored, and whether that number survives.
	//
	// What makes the Valuation contract implementable across a resume. A run
	// stopped mid-Asset must recompute that Asset's Valuation over BOTH
	// processes' measurements; without this reader it could only recompute over
	// its own half — a delta over half a sample — or re-pay to recover the
	// numbers.
	Measurements(ctx context.Context, runID, assetID string) ([]RecordedMeasurement, error)

	// WriteValuation records one Asset's finished Valuation, written only once
	// every measurement behind it is durable.
	//
	// A run stopped by its cost cap part-way through an Asset therefore leaves
	// the paid measurements and no Valuation: resume finishes the Asset without
	// paying twice, and nothing downstream can read a delta over half a sample.
	WriteValuation(ctx context.Context, runID string, v *knov1.Valuation) error

	// Valuations returns every Valuation recorded for a run, ordered by Asset
	// ID.
	Valuations(ctx context.Context, runID string) ([]*knov1.Valuation, error)

	// WritePortfolio records the Portfolio one Select run produced. One row
	// per run; rewriting the same run replaces the row, so a resume that
	// reaches the end again records the current decision rather than the
	// first one.
	WritePortfolio(ctx context.Context, runID string, p *knov1.Portfolio) error

	// Portfolio loads a run's Portfolio. Returns ErrPortfolioNotFound when
	// the run recorded none.
	Portfolio(ctx context.Context, runID string) (*knov1.Portfolio, error)

	// WriteGaps records the gaps verdicts one Export run computed, keyed by
	// the Export run that produced them. One row per run; rewriting the same
	// run replaces the row.
	WriteGaps(ctx context.Context, runID string, g *knov1.Gaps) error

	// Gaps loads the gaps record an Export run computed. Returns
	// ErrGapsNotFound when the run recorded none — the report's "no cluster
	// data for this run", never guessed.
	Gaps(ctx context.Context, runID string) (*knov1.Gaps, error)

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

	// CaseObservations aggregates per-outcome facts for one Run.
	//
	// AGGREGABLE facts only. dev_case_count and holdout_case_count are
	// deliberately absent: they describe what was LOADED, not what executed,
	// and ADR-0004 records that aggregating them from outcomes reports a zero
	// holdout count — the number that sets every interval's width. The caller
	// composes CaseExecution from this plus what it loaded.
	//
	// Returns zeros for a Run with no outcomes, and no presence signal.
	// Presence on the wire means "this stage executes Cases", which is a
	// property of the stage rather than of the query, so it belongs to the
	// caller.
	//
	// ResolvedModels and ProviderBuilds are SETS, ordered, and exclude the
	// empty string: a Case that errored has no resolved model, and a set
	// carrying "" would make a resume comparison test against nothing.
	//
	// Purge-transparent: every field is a column, never a blob.
	CaseObservations(ctx context.Context, runID string) (Observations, error)

	// RecordOrphanSpend adds spend the guard settled for a Case that produced
	// no outcome — refused by the budget after earlier attempts were charged,
	// or cancelled mid-backoff.
	//
	// Additive, and separate from RecordOutcome, because the two answer
	// different questions. RecordOutcome says a Case is DONE; this says money
	// was spent on one that is not. A Case carrying orphan spend stays absent
	// from CompletedCases and is re-attempted on resume, while its earlier
	// charge is already inside SettledSpend and is not spent twice.
	//
	// An implementation must NOT record this as an outcome row. RecordOutcome
	// is idempotent on (run_id, case_id) by ignoring a second insert, so a
	// spend-only row would permanently block the real outcome for its Case.
	//
	// The amount is recorded against the RUN, so this method cannot say which
	// Case it belonged to. The engine emits an OrphanSpend event carrying that
	// attribution; an implementation of this interface is not responsible for
	// it.
	//
	// Refuses a run that does not exist rather than silently dropping the
	// spend, which is the failure it exists to prevent.
	RecordOrphanSpend(ctx context.Context, runID string, spend budget.Spend) error

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

// Observations are the per-outcome facts a Run can report about itself.
//
// Aggregated from the outcomes table rather than from in-memory counters, so
// they survive a crash and stay correct across a resume — the same reason
// ScoreSum exists.
type Observations struct {
	// Attempted is Cases with a terminal outcome; Scored and Errored partition
	// it. A Case still being retried is not counted.
	Attempted int32
	Scored    int32
	Errored   int32

	// Refused is Cases the provider declined on policy grounds. Scored, and
	// counted separately so a run that was refused rather than measured cannot
	// pass for a clean baseline.
	Refused int32

	// Truncated is Cases whose answer hit the output ceiling. Scored against an
	// incomplete answer, so a number here means our own max_output_tokens is
	// depressing the score.
	Truncated int32

	// UsageEstimated is Cases whose cost is the engine's prediction rather than
	// reported usage.
	UsageEstimated int32

	// ResolvedModels is the distinct models that actually answered, ordered and
	// excluding the empty string.
	ResolvedModels []string

	// ProviderBuilds is the distinct provider-side builds observed, same
	// treatment.
	ProviderBuilds []string
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
