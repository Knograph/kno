package core

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// ValueOptions configure the Value stage.
//
// Shaped like BaselineOptions on purpose: the two stages share a Store, a
// Guard, and an executor, and a field that means one thing here and another
// there is how a resume check ends up comparing the wrong pair of values.
type ValueOptions struct {
	// RunID identifies this Value run.
	RunID string

	// BaselineRunID is the run whose recorded scores this one pairs against.
	//
	// A separate run, and separate on purpose. The baseline is what the Asset
	// is being measured AGAINST, and reading its scores through the Store
	// rather than re-running it is what makes the stage affordable — and what
	// makes ADR-0005's reasoning about which draw selected which Case
	// checkable.
	BaselineRunID string

	// Agent is the control arm: the agent with no Asset injected. It must also
	// implement ContextInjector, which validate refuses without.
	Agent Agent

	// AgentRef identifies the agent, for the Run record and the event stream.
	AgentRef *AgentRef

	// Goal scores every measurement, and its Domain decides the interval
	// method for every delta this run reports.
	Goal Goal

	// GoalName is the Goal's registered name.
	GoalName string

	// Guard authorizes every measurement before it is made.
	Guard *budget.Guard

	// Evals is the SEALED dev split: SealedEvals never yields a non-dev Case,
	// which is the holdout canary's enforcement rather than a scan. The stage
	// reads Cases only through it.
	Evals *SealedEvals

	// EstCostPerCallUSDMicros is the fallback estimate for agents that do not
	// implement Estimator, exactly as Baseline uses it.
	EstCostPerCallUSDMicros int64

	// Store is where measurements and Valuations become durable.
	Store store.Store

	// Concurrency bounds workers within one arm of one Asset. See the plan's
	// note on the loop shape for why the bound is per-arm.
	Concurrency int

	// Routing configures which Cases measure which Asset.
	Routing value.Options

	// Resume continues a run whose measurements are already partly recorded.
	Resume bool

	// UnsafeBaseline accepts a baseline whose resolved models form a blend.
	//
	// Off by default, and deliberately awkward to reach: pairing against a
	// control that mixes models is not an estimator of anything, because each
	// Case's control draw came from a different agent. The flag exists for
	// operators who know their blend is irrelevant to the Goal — the refusal
	// cannot read that from the record.
	UnsafeBaseline bool

	// InputFingerprint pins the inputs, so a resume cannot silently continue a
	// different run.
	InputFingerprint string

	// MaxAttempts, RetryBudget and RetryBackoff bound retries per measurement,
	// exactly as they do for Baseline.
	MaxAttempts  int
	RetryBudget  int64
	RetryBackoff int64
}

// ValueResult is what a Value run produced.
type ValueResult struct {
	// RunID identifies the run.
	RunID string

	// Valuations is one entry per Asset, including the Assets that routed to
	// nothing — an Asset measured against zero Cases is a result, not an
	// omission, and it is the one this stage can give away for free.
	Valuations []*Valuation

	// Plan is the routing decision every number here was produced under.
	// Recorded so a reader can re-derive the selection from its seed.
	Plan *value.Plan

	// Status is how the run ended: COMPLETED, or BUDGET_STOPPED /
	// INTERRUPTED with the truncated portfolio marked in the Valuations.
	Status knov1.RunStatus

	// GoalDirection is which way is better, so the report can un-negate
	// MINIMIZE deltas for display: the stored delta is sign-corrected
	// (positive is better), and the display wants the goal's own units.
	GoalDirection knov1.Direction
}

// errNoInjection is returned when the agent cannot carry an Asset.
var errNoInjection = errors.New("this agent cannot carry an Asset in its context")

// validate refuses everything that can be refused before any spend.
//
// Every refusal here is free. The alternative to each of them is a full-price
// run whose output reads as a result — an interval, a delta, a rank in a
// portfolio — computed over something other than what the user asked for.
func (o ValueOptions) validate(pool Pool) error {
	switch {
	case o.RunID == "":
		return errs.ErrInvalidInput.Wrap(errors.New("value: a run ID is required"))
	case o.BaselineRunID == "":
		return errs.ErrInvalidInput.
			WithFix("pass --baseline-run-id, or run `kno baseline` first").
			Wrap(errors.New("value: a baseline run to pair against is required"))
	case o.Agent == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("value: an agent is required"))
	case o.Goal == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("value: a goal is required"))
	case o.Store == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("value: a store is required"))
	case o.Guard == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("value: a budget guard is required"))
	case o.Evals == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("value: a sealed evals source is required"))
	case pool == nil:
		return errs.ErrInvalidInput.
			WithFix("point --pool at a file of Assets").
			Wrap(errors.New("value: a pool is required"))
	}

	// The capability check, before any spend, and it is not a formality.
	//
	// An adapter that does not inject context still ANSWERS every request — it
	// simply answers the Case without the Asset. Both arms then measure the
	// same thing, every delta is noise around zero, and the run produces a
	// full set of intervals at full price that are indistinguishable from a
	// real result reporting that nothing helped.
	if _, ok := o.Agent.(ContextInjector); !ok {
		return errs.ErrCapabilityUnsupported.
			WithFix("measure with an adapter that supports context injection (openai: and anthropic: both do), or value this pool against a different agent").
			Wrap(fmt.Errorf("value: %w", errNoInjection))
	}
	if c, ok := o.Agent.(Capable); ok && !c.Capabilities().GetContextInject() {
		return errs.ErrCapabilityUnsupported.
			WithFix("measure with an adapter that supports context injection, or value this pool against a different agent").
			Wrap(fmt.Errorf("value: the agent implements ContextInjector but declares "+
				"context_inject false, so %w — an adapter that answered anyway would "+
				"measure the Case without the Asset in both arms, and report the "+
				"resulting noise as a full set of intervals", errNoInjection))
	}
	return nil
}

// baselineCases reads the recorded scores this run pairs against, and refuses a
// baseline that cannot serve as one.
//
// Recorded scores, not a re-run. Re-running the control would pay twice for a
// number already on disk, and would compare the Asset against a DIFFERENT draw
// than the one every other Asset in the pool was compared against — so two
// Assets' deltas would not be commensurable, which is the whole point of
// ranking them.
func (o ValueOptions) baselineCases(ctx context.Context) (map[string]store.CaseScore, []string, error) {
	run, err := o.Store.GetRun(ctx, o.BaselineRunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading baseline run %s: %w", o.BaselineRunID, err)
	}
	if got := run.GetStage(); got != knov1.Stage_STAGE_BASELINE {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno baseline` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a baseline", o.BaselineRunID, got))
	}
	// A baseline that ended because too many Cases errored is not a baseline.
	// Its recorded scores cover whichever Cases happened to succeed, and
	// pairing against them measures the Asset on a slice selected by transport
	// failures — a selection nothing downstream can see or correct for.
	if r := run.GetIncompleteReason(); r != "" {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("re-run the baseline until it completes, then value against that run").
			Wrap(fmt.Errorf("baseline run %s is marked incomplete (%s); its recorded "+
				"scores cover only the Cases that happened to succeed, so a delta "+
				"against them is measured on a slice selected by failures",
				o.BaselineRunID, r))
	}
	// A blended-model baseline is refused, not averaged with. Pairing against
	// it would compare the Asset against a control that was a different agent
	// on different Cases — a mix of estimators, not one estimator — and the
	// resulting "delta" would claim a single reference that never existed.
	// This is debt #55's marker, read here: the first stage consuming a
	// Baseline as a reference refuses what would make the reference lie.
	if models := run.GetCaseExecution().GetResolvedModels(); len(models) > 1 && !o.UnsafeBaseline {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("re-run the baseline pinned to one model, or pass --unsafe-baseline if " +
				"the blend is known not to matter for this Goal").
			Wrap(fmt.Errorf("baseline run %s resolved %d models (%v); a delta against "+
				"a blended control is not an estimator of anything",
				o.BaselineRunID, len(models), models))
	}

	scores, err := o.Store.CaseScores(ctx, o.BaselineRunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading baseline scores for %s: %w", o.BaselineRunID, err)
	}
	if len(scores) == 0 {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("run `kno baseline` to completion first").
			Wrap(fmt.Errorf("baseline run %s recorded no scores", o.BaselineRunID))
	}
	// The baseline's resolved models arm the FIRST process's mid-run gate:
	// the reference was measured against them, so a Value run whose agent is
	// answered by anything else is blending two models into one delta. The
	// resumed run's own record takes precedence when it exists.
	return scores, run.GetCaseExecution().GetResolvedModels(), nil
}

// caseRefs builds the router's view of the dev split.
//
// This is the ONLY place a baseline score is turned into a routing input, and
// it deliberately turns it into a bool. value.CaseRef carries an ID, tags, and
// Failed — no Store and no Score — so the routing path and the delta path
// cannot share a source. See value.CaseRef's godoc and ADR-0005.
//
// A Case whose baseline score is unrecoverable is DROPPED rather than treated
// as a failure. Its number is gone, so it can never be paired; routing to it
// would buy a measurement that has to be discarded at the pairing step, after
// it has been paid for.
func caseRefs(
	cases iter.Seq2[*Case, error],
	scores map[string]store.CaseScore,
) (refs []value.CaseRef, unpairable int, err error) {
	for c, err := range cases {
		if err != nil {
			return nil, 0, fmt.Errorf("reading the dev split: %w", err)
		}
		s, ok := scores[c.GetId()]
		if !ok {
			// Never scored in the baseline: it errored there, or was never
			// attempted. Nothing to pair against.
			unpairable++
			continue
		}
		if s.Unrecoverable {
			unpairable++
			continue
		}
		refs = append(refs, value.CaseRef{
			ID: c.GetId(),
			// Cloned: the iterator borrows its slice for one iteration, and
			// this outlives the loop.
			Tags:   append([]string(nil), c.GetTags()...),
			Failed: !s.Passed,
		})
	}
	return refs, unpairable, nil
}
