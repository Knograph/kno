package core

import (
	"context"
	"fmt"
	"time"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Opening a Run: creating the record for a fresh one, or reloading an
// interrupted one and refusing a resume whose measurement configuration no
// longer matches.
//
// Both, not just the resume half. openRun's fresh branch is the ONLY place a
// knov1.Run is constructed, holdout provenance included — split seed, holdout
// fraction, and the underpowered flag. Someone adding a field to Run comes
// here, so the file is named for opening rather than for resuming.
//
// A resume that silently accepts changed inputs produces a Run whose parts
// were measured against different things, which is what checkResumable exists
// to prevent.

// openRun creates or reloads the Run record.
func (o BaselineOptions) openRun(ctx context.Context) (*knov1.Run, error) {
	if o.Resume {
		run, err := o.Store.GetRun(ctx, o.RunID)
		if err != nil {
			return nil, fmt.Errorf("loading run %s: %w", o.RunID, err)
		}
		if err := o.checkResumable(run); err != nil {
			return nil, err
		}
		return run, nil
	}

	run := &knov1.Run{
		Id:                  o.RunID,
		Stage:               knov1.Stage_STAGE_BASELINE,
		CreatedAt:           o.now().Format(time.RFC3339),
		Agent:               o.AgentRef,
		GoalName:            o.GoalName,
		GoalDirection:       o.Goal.Direction(),
		Status:              knov1.RunStatus_RUN_STATUS_RUNNING,
		InputFingerprint:    o.InputFingerprint,
		EvalContentHash:     o.EvalContentHash,
		SplitSeed:           o.SplitSeed,
		HoldoutFrac:         o.HoldoutFrac,
		DevCaseCount:        int32(o.DevCases),     //nolint:gosec // bounded by the eval set
		HoldoutCaseCount:    int32(o.HoldoutCases), //nolint:gosec // bounded by the eval set
		HoldoutUnderpowered: o.HoldoutUnderpowered,
	}
	if err := o.Store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("creating run %s: %w", o.RunID, err)
	}
	return run, nil
}

// checkResumable refuses a resume whose measurement configuration differs from
// the one recorded.
//
// The RESOLVED MODEL is deliberately not checked here. It was, against
// BaselineOptions.ResolvedModel — a caller-supplied field read before any
// request is made, so the only value it could ever hold was one a previous run
// recorded, and the check compared a value to itself. It never fired. The gate
// now runs at first-response time (modelGate, baseline_gate.go), which is also
// the only placement catching an alias that re-points DURING a long run rather
// than only between two. See docs/debt.md#42.
//
// InputFingerprint is supplied by the caller and covers the caller's inputs;
// core cannot assume it covers the Goal or the Agent, and the one caller that
// exists today does not fold them in. So the recorded fields are compared
// directly rather than by a convention every future caller has to remember.
//
// Without this, `--run-id r --agent A` followed by `--run-id r --resume --agent
// B` silently blends Cases scored under two different agents into one
// AggregateScore and presents it as a single homogeneous number — the
// corrupted-reference failure prime directive 5 exists to prevent.
func (o BaselineOptions) checkResumable(run *knov1.Run) error {
	var changed string
	switch {
	case run.GetInputFingerprint() != o.InputFingerprint:
		changed = "different inputs"
	case run.GetGoalName() != o.GoalName:
		changed = fmt.Sprintf("a different goal (recorded %q, now %q)", run.GetGoalName(), o.GoalName)
	case run.GetGoalDirection() != o.Goal.Direction():
		changed = fmt.Sprintf("a different goal direction (recorded %v, now %v)",
			run.GetGoalDirection(), o.Goal.Direction())
	case run.GetAgent().GetRef() != o.AgentRef.GetRef():
		changed = fmt.Sprintf("a different agent (recorded %q, now %q)",
			run.GetAgent().GetRef(), o.AgentRef.GetRef())
	default:
		return nil
	}
	return errs.ErrCheckpointStale.WithFix(o.staleFix(run)).
		Wrap(fmt.Errorf("run %s was recorded against %s", o.RunID, changed))
}

// staleFix names what to DO about the change the caller already identified.
//
// It must not name causes checkResumable does not test. It once offered "the
// goal, agent, or split configuration changed" for every non-eval mismatch,
// which named the split — never compared, since InputFingerprint covers the
// eval SOURCE only — so a user was told to restore a setting they had never
// touched.
//
// The resolved-model branch left with the check itself. modelGate carries its
// own fix line, because that refusal happens mid-run and has a different remedy
// than anything reachable here.
func (o BaselineOptions) staleFix(run *knov1.Run) string {
	if run.GetEvalContentHash() != o.EvalContentHash {
		return "the eval source changed since this run started; re-run without --resume, " +
			"or restore the original file"
	}
	return "the goal or agent changed since this run started; re-run without " +
		"--resume, or restore the setting it was recorded against"
}
