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
	// DEBT(docs/debt.md#26): these do not track presence, so a stage that does
	// not execute Cases writes a zero indistinguishable from a real one.
	run.AttemptedCaseCount = int32(attempted) //nolint:gosec // bounded by the eval set
	run.ScoredCaseCount = int32(scored)       //nolint:gosec // bounded by the eval set
	run.ErroredCaseCount = int32(errored)     //nolint:gosec // bounded by the eval set

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
	run.Concurrency = o.concurrency

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
			lostCount, scored))
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
				errored, attempted, rate*100, o.maxErrorRate()*100))
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
	if err := o.Store.FinishRun(closeCtx, run); err != nil {
		return result, fmt.Errorf("finishing run %s: %w", o.RunID, err)
	}
	if err := o.emitRunFinished(closeCtx, agg, run); err != nil {
		return result, err
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
