package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"google.golang.org/protobuf/proto"
)

// Ending a Run: the verdict it records, the status it carries, and how a
// RUN-ending error is classified for the exit code.
//
// Per-CASE error classification is not here — codeOf lives in
// baseline_record.go beside the two places that call it.

// closeRun records how the run ended and computes its aggregate.
func (o BaselineOptions) closeRun(
	ctx context.Context,
	run *knov1.Run,
	agg *aggregator,
	stats executor.Stats,
	runErr error,
) (*BaselineResult, error) {
	scored, errored, mean, aggregateLost, lostCount := agg.snapshot()
	attempted := scored + errored

	// Classify before anything reads runErr. A bare context.Canceled leaving
	// this package exits 1, indistinguishable from a broken build — and a
	// scheduled run killed by a pod eviction is not a broken build. Wrapping
	// keeps the chain, so errors.Is(err, context.Canceled) still answers.
	runErr = classifyRunErr(runErr)

	run.Status = statusFor(runErr)
	run.FinishedAt = proto.String(o.now().Format(time.RFC3339))
	// Still written, and still without presence — CaseExecution below is the
	// one that has it, and these stay on the wire until a deprecation with a
	// reader ready. The lint bundle rejects a DEBT marker pointing at a repaid
	// row, and #26 is repaid.
	run.AttemptedCaseCount = int32(attempted) //nolint:gosec // bounded by the eval set
	run.ScoredCaseCount = int32(scored)       //nolint:gosec // bounded by the eval set
	run.ErroredCaseCount = int32(errored)     //nolint:gosec // bounded by the eval set

	// The weak-label count, written when nonzero. It describes the EVAL SET,
	// not the outcomes, and it cannot be recomputed from durable rows — the
	// store has no per-Case provenance — so the caller's ingestion pass is its
	// only source. Written only when nonzero for the same reason Concurrency
	// is: a resume whose caller passes zero must not erase the first process's
	// record, and a fingerprint-checked resume has the same eval content and
	// therefore the same count anyway. Zero stays absent rather than
	// over-written, and a hand-authored eval set keeps a zero exactly where a
	// reader expects it.
	if o.WeakLabelCases > 0 {
		run.WeakLabelCaseCount = int32(o.WeakLabelCases) //nolint:gosec // bounded by the eval set
	}

	// Both fields are derived entirely from state recomputed here over the
	// WHOLE run, so both are cleared before recomputing. A resumed run
	// otherwise inherits the verdict of the process that stopped: one that
	// errored 6 of 10 Cases and quit leaves ErrorRateExceeded set, and a
	// resume that goes on to score 200 cleanly still reports "not a usable
	// baseline" forever, because the branch that sets the flag has no branch
	// that unsets it.
	// The width this Run executed at, recorded whether or not it was reduced.
	// A Run that ran at 32 and one that ran at 8 are otherwise identical on
	// the record, which cannot then answer whether two Runs are comparable.
	//
	// Never overwritten with nothing. openRun reloads the stored Run on a
	// resume and FinishRun re-marshals the whole message, so an unconditional
	// assignment ERASES the first process's decision — and checkFeasible
	// returns before recording when a resume has no Cases left, which is
	// exactly the idempotent `--resume` in CI that its own comment advertises.
	// Measured: a completed run recording effective=10 resumed to nil.
	//
	// The field is per-process where the two disagree: a resume that ran at a
	// different width records its own, because that is the width its Cases
	// actually ran at and the alternative is asserting a number no process
	// used.
	if o.concurrency != nil {
		run.Concurrency = o.concurrency
	}

	// Written for EVERY Baseline run, with zeros where nothing ran.
	//
	// Presence means "this stage executes Cases" (ADR-0004), which is a
	// property of the STAGE, not of the query. Deriving it from whether the
	// aggregate found rows would report "this stage executes no Cases" for a
	// run whose Cases were all refused after being charged — a run that
	// executed Cases and spent money — which is the inverted ambiguity
	// run.proto forbids. A stage that invokes no agent leaves it absent by not
	// writing it.
	run.ErrorRateExceeded = false
	run.IncompleteReason = ""

	// Reasons accumulate. A run can be both unscoreable and too error-prone,
	// and overwriting left whichever ran second — losing the one the user
	// cannot infer from anything else on screen.
	var reasons []string

	// A run missing scores cannot produce an aggregate, and saying so is the
	// point. Reporting the partial mean would present a number nobody can
	// reproduce beside counts that describe a larger population. The count is
	// included because one lost Case in 10,000 and 10,000 lost out of 10,000
	// are the same sentence otherwise, and only one of them is worth paying to
	// re-run.
	if aggregateLost {
		reasons = append(reasons, fmt.Sprintf(
			"%d of %d scored Cases can no longer contribute a number — purged "+
				"before scores were stored separately, or holding a Score that "+
				"could not be read back — so this run has no reportable "+
				"aggregate; the Cases themselves are intact and resume normally",
			lostCount, scored,
		))
	}

	// A run whose error rate is too high is completed but not clean. Later
	// stages must refuse to treat it as a reference rather than computing
	// deltas against a partial sample.
	if attempted > 0 {
		rate := float64(errored) / float64(attempted)
		if rate > o.maxErrorRate() {
			run.ErrorRateExceeded = true
			reasons = append(reasons, fmt.Sprintf(
				"%d of %d Cases errored (%.1f%%), above the %.1f%% threshold; "+
					"this run is not a usable baseline",
				errored, attempted, rate*100, o.maxErrorRate()*100,
			))
		}
	}
	run.IncompleteReason = strings.Join(reasons, "; also: ")

	result := &BaselineResult{
		Run:                  run,
		Stats:                stats,
		AggregateScore:       mean,
		AggregateUnavailable: aggregateLost,
		Spent:                o.Guard.Spent(),
	}

	// Recording how the run ended must survive the cancellation that ended it:
	// on Ctrl-C the caller's context is precisely the one that died, and a Run
	// left in RUNNING would look like a crash rather than an interruption.
	closeCtx := context.WithoutCancel(ctx)

	// Written for EVERY Baseline run, with zeros where nothing ran, and on the
	// uncancellable context: an interrupted run still executed Cases, and the
	// caller's context is precisely the one that died.
	//
	// Presence means "this stage executes Cases" (ADR-0004) — a property of
	// the STAGE, not of the query. Deriving it from whether the aggregate found
	// rows would report "this stage executes no Cases" for a run whose Cases
	// were all refused after being charged, which is the inverted ambiguity
	// run.proto forbids. A stage that invokes no agent leaves it absent by not
	// writing it.
	// NOT fatal, and NOT sequenced ahead of the writes that end the run.
	//
	// This is a READ. Letting it gate FinishRun meant one transient store error
	// left the Run in RUNNING with no finished_at — indistinguishable from a
	// crash — suppressed RunFinished, which the schema promises is always the
	// last event and which an SSE consumer waits on forever, and replaced the
	// real run error, so a budget stop reported "reading case observations"
	// and exited with the generic failure code.
	//
	// The same argument recordOrphanSpend makes one level down: an
	// observability failure must never change the result it describes.
	obsErr := o.writeCaseExecution(closeCtx, run)

	if err := o.Store.FinishRun(closeCtx, run); err != nil {
		return result, fmt.Errorf("finishing run %s: %w", o.RunID, err)
	}
	if err := o.emitRunFinished(closeCtx, agg, run); err != nil {
		return result, err
	}
	// Surfaced only once the run is durably closed, and only if nothing worse
	// happened — a budget stop keeps its own classification and its own exit
	// code.
	if obsErr != nil && runErr == nil {
		runErr = obsErr
	}
	return result, runErr
}

// classifyRunErr gives a run-ending error the CLI grammar and an exit code
// chosen deliberately, rather than the unclassified default.
func classifyRunErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errs.ErrInterrupted):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errs.ErrInterrupted.Wrap(err)
	default:
		return err
	}
}

// writeCaseExecution composes the Run's per-Case record.
//
// The counts come from the STORE, not from the aggregator: aggregating what is
// durable is what makes them survive a crash and stay correct across a resume.
//
// dev and holdout come from the RUN RECORD, never from SQL and never from this
// process's options. They describe what was loaded, and ADR-0004 records that
// aggregating them from outcomes reports a zero holdout count — the number
// that sets every interval's width. openRun wrote them at creation; a resume
// must not overwrite them with its own.
func (o BaselineOptions) writeCaseExecution(ctx context.Context, run *knov1.Run) error {
	obs, err := o.Store.CaseObservations(ctx, o.RunID)
	if err != nil {
		// Plain wrapping, matching FinishRun's on the same path. This is a
		// store failure rather than something the user did, and the run has
		// already succeeded — the flat counters still carry every number the
		// report needs.
		return fmt.Errorf("reading case observations for %s: %w", o.RunID, err)
	}
	run.CaseExecution = &knov1.CaseExecution{
		// Carried forward from the Run, NOT re-read from the options. openRun
		// reloads the stored record on a resume and keeps the first process's
		// split; taking this process's would put two different splits on one
		// message, with the presence-carrying copy describing a split the run
		// was never measured under. checkResumable does not compare the split
		// — InputFingerprint is the eval SOURCE only — so a resume with a
		// different --holdout-frac passes every check and would have recorded
		// holdout=22 beside holdout=5 for the same run.
		//
		// ADR-0004 calls holdout_case_count the number that sets every
		// interval's width, which is why it is ingested rather than
		// aggregated — and why it must be ingested ONCE.
		DevCaseCount:            run.GetDevCaseCount(),
		HoldoutCaseCount:        run.GetHoldoutCaseCount(),
		AttemptedCaseCount:      obs.Attempted,
		ScoredCaseCount:         obs.Scored,
		ErroredCaseCount:        obs.Errored,
		RefusedCaseCount:        obs.Refused,
		TruncatedCaseCount:      obs.Truncated,
		UsageEstimatedCaseCount: obs.UsageEstimated,
		ResolvedModels:          obs.ResolvedModels,
		ObservedProviderBuilds:  obs.ProviderBuilds,
	}
	return nil
}

// statusFor maps a run error to how the run ended.
//
// The distinction matters to CI: a budget stop means the run did what it was
// told and can continue, while a failure means something is broken.
func statusFor(err error) knov1.RunStatus {
	switch {
	case err == nil:
		return knov1.RunStatus_RUN_STATUS_COMPLETED
	case errors.Is(err, errs.ErrBudgetExceeded):
		return knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return knov1.RunStatus_RUN_STATUS_INTERRUPTED
	default:
		return knov1.RunStatus_RUN_STATUS_FAILED
	}
}
