package bridge

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// AgentFactory builds an invokable Agent for a deployed model.
//
// bridge stays decoupled from any concrete adapters/agent/* package this
// way — the same reason core/ imports nothing above it (CLAUDE.md prime
// directive 3). The caller (cli) supplies the real implementation, which
// knows how to reach whatever HTTP endpoint the Tuner's Deploy actually
// stood up.
type AgentFactory func(ctx context.Context, model *knov1.AgentRef) (core.Agent, error)

// ScoreEvalRunner is the production EvalRunner: core.ScorePass driving an
// AgentFactory-built agent over a Cases source resolved once for the whole
// bridge run, filtered per Measure call to the Case IDs asked for.
//
// AcceptFreeCalls is the caller's decision, not this type's — see the field
// doc below. It used to be asserted unconditionally here (decision B2 of
// docs/plans/2026-09-01-bridge-eval-seam.md), true for every provider on
// the theory that inference on reserved capacity is always free. That is
// Together-specific: a Together dedicated endpoint bills per minute per
// replica regardless of how many Cases run through it, and
// bridge/hosting.go's ticker already meters that spend through the SAME
// budget guard, so charging a per-call estimate on top would double-count
// the same dollars. A provider with no hosting ticker and a real per-token
// rate has no such cover — see
// docs/plans/2026-09-02-openai-tuner.md §2 for the correction and
// docs/debt.md#159 for the ledger entry it repays.
type ScoreEvalRunner struct {
	// Cases is the whole bridge run's Case content, ALREADY resolved from
	// --evals and sealed to dev-only — the CLI choke point per the plan's
	// §9. Every Case any group's Measure call could ask for must be
	// reachable through it; NewScoreEvalRunner's caller is responsible for
	// the pre-flight completeness check (a caseID with no Case behind it
	// is a refusal before any spend, not something this type discovers
	// mid-run).
	Cases *core.SealedEvals

	// Goal scores each Response, exactly as it does for every other stage.
	Goal core.Goal

	// Guard authorizes and settles every call — the SAME Guard the
	// hosting ticker settles into, so Overshoot() and SettledSpend read
	// one number across both spend shapes.
	Guard *budget.Guard

	// NewAgent builds the Agent to invoke for one group's deployed model.
	NewAgent AgentFactory

	// AcceptFreeCalls asserts that every Invoke this pass makes is already
	// paid for through a channel other than the per-call budget guard —
	// core.ScoreParams.AcceptFreeCalls, passed straight through.
	//
	// The caller (cli) sets this from whether an eval-pass price resolved
	// for --tuner's base model: true when it did not (a Together dedicated
	// endpoint's hosting ticker already covers inference — see this type's
	// doc), false when it did (a per-token provider, whose Agent then
	// prices each Case through core.Estimator instead). Keyed on the
	// resolved price rather than on scheme, per
	// docs/plans/2026-09-02-openai-tuner.md §2: scheme-keying would bake in
	// an assumption that breaks the first time one provider offers both
	// reserved capacity and per-token serving.
	AcceptFreeCalls bool

	// Concurrency, MaxAttempts, RetryBudget and RetryBackoff pass through
	// to core.ScorePass unchanged. Zero means ScorePass's own defaults.
	Concurrency               int
	MaxAttempts               int
	RetryBudget, RetryBackoff time.Duration
}

// Measure implements EvalRunner.
func (r *ScoreEvalRunner) Measure(ctx context.Context, group string, model *knov1.AgentRef, caseIDs []string) (map[string]float64, error) {
	agent, err := r.NewAgent(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("building an agent for the %s group's deployed model: %w", group, err)
	}

	want := make(map[string]struct{}, len(caseIDs))
	for _, id := range caseIDs {
		want[id] = struct{}{}
	}
	filtered := core.Seal(idFilteredEvals{inner: r.Cases, want: want})

	res, err := core.ScorePass(ctx, core.ScoreParams{
		Agent: agent, AgentRef: model, Goal: r.Goal, Cases: filtered, Guard: r.Guard,
		// The caller's decision — see ScoreEvalRunner.AcceptFreeCalls's doc.
		AcceptFreeCalls: r.AcceptFreeCalls,
		Concurrency:     r.Concurrency,
		MaxAttempts:     r.MaxAttempts,
		RetryBudget:     r.RetryBudget,
		RetryBackoff:    r.RetryBackoff,
	})
	if res == nil {
		return nil, err
	}
	// A partial pass still returns what it scored: bridge.Run persists it
	// and pairs against exactly the Cases both sides cover — edge case 5,
	// "the all-in baseline model scores a Case the ablation model errors
	// on. That Case drops from the pair set."
	return res.Scores, err
}

// idFilteredEvals restricts an already-sealed Evals source to a wanted
// Case ID set — the per-Measure-call filter each group's ScorePass
// invocation needs. --evals itself is resolved and sealed ONCE for the
// whole bridge run, at the CLI choke point (the plan's §9); this is a
// second, narrower filter on top, not a second sealing.
type idFilteredEvals struct {
	inner core.Evals
	want  map[string]struct{}
}

func (f idFilteredEvals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	seq, err := f.inner.Cases(ctx)
	if err != nil {
		return nil, err
	}
	return func(yield func(*core.Case, error) bool) {
		for c, err := range seq {
			if err != nil {
				yield(nil, err)
				return
			}
			if _, ok := f.want[c.GetId()]; !ok {
				continue
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}
