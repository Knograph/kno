package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
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
	var (
		changed      string
		modelChanged bool
	)
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
	case resolvedModelChanged(run, o.ResolvedModel):
		modelChanged = true
		// A ref like openai:gpt-4.1 is a moving pointer. A run interrupted on
		// Monday and resumed on Friday after the alias re-points passes every
		// check above and blends two models into one AggregateScore.
		//
		// Only the RESOLVED model. The provider's build identifier is recorded
		// and reported but never refused on: it changes whenever the backend
		// config changes, routinely and with no model change, and refusing on
		// it would cost the user a full re-run for a false positive — worse
		// than the blending it prevents.
		changed = fmt.Sprintf("a resolved model it never saw (recorded %s, now %q)",
			recordedModels(run), o.ResolvedModel)
	default:
		return nil
	}
	return errs.ErrCheckpointStale.WithFix(o.staleFix(run, modelChanged)).
		Wrap(fmt.Errorf("run %s was recorded against %s", o.RunID, changed))
}

// resolvedModelChanged reports whether the provider is now answering with a
// different model than the one this run recorded.
//
// Empty on either side means unknown — the first process may not have reached a
// response, or this one has not yet. Unknown is not a mismatch: refusing on
// absence would make every run that stopped before its first answer
// unresumable.
// resolvedModelChanged reports whether the model now answering is absent from
// the set the recorded Run observed.
//
// Set MEMBERSHIP, not models[0]. The field is repeated because with
// concurrency there is no "first response", and during a provider rollout two
// workers in one Run legitimately see different builds — so a run that saw
// {A, B} and is now served by B has not changed, and comparing against
// whichever element happened to sort first would refuse it.
//
// Empty on either side means nothing to compare: a run whose Cases all errored
// records no model, and a process that has not called the provider yet knows
// none.
func resolvedModelChanged(run *knov1.Run, now string) bool {
	recorded := run.GetCaseExecution().GetResolvedModels()
	if len(recorded) == 0 || now == "" {
		return false
	}
	return !slices.Contains(recorded, now)
}

// recordedModels renders the set for a refusal message, so the user can see
// what the run was measured against rather than only that it disagreed.
func recordedModels(run *knov1.Run) string {
	return strings.Join(run.GetCaseExecution().GetResolvedModels(), ", ")
}

// staleFix names what to DO about the change the caller already identified.
//
// It must not name causes checkResumable does not test. It previously offered
// "the goal, agent, or split configuration changed" for every non-eval
// mismatch, which named the split — never compared, since InputFingerprint
// covers the eval SOURCE only — and omitted the resolved model, which is. A
// user re-pointed by an alias got told to restore a setting they never
// touched.
func (o BaselineOptions) staleFix(run *knov1.Run, modelChanged bool) string {
	switch {
	case modelChanged:
		// Not a setting the user can restore: the ref is a moving pointer and
		// the provider moved it. Pinning is the only thing that makes the run
		// resumable, and re-running is the only thing that makes it comparable.
		return "the agent ref resolved to a different model than this run was " +
			"measured against; re-run without --resume, or pin the model in the " +
			"agent ref so a provider rollout cannot re-point it mid-run"
	case run.GetEvalContentHash() != o.EvalContentHash:
		return "the eval source changed since this run started; re-run without --resume, " +
			"or restore the original file"
	default:
		return "the goal or agent changed since this run started; re-run without " +
			"--resume, or restore the setting it was recorded against"
	}
}
