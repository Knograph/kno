package core

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The resolved-model gate: refusing a resume that is being served a different
// model than the one it was measured against.
//
// A ref like `openai:gpt-4.1` is a moving pointer. A run interrupted on Monday
// and resumed on Friday after the alias re-points passes every check in
// checkResumable and blends two models into one AggregateScore — the
// corrupted-reference failure prime directive 5 exists to prevent, arriving
// through the one input nothing could see.
//
// It runs at FIRST RESPONSE, not at run open. The check used to compare
// BaselineOptions.ResolvedModel, a caller-supplied field read before any
// request was made, so the only value it could hold was one a previous run had
// recorded — comparing a value to itself. It never fired once (docs/debt.md#42).
// First-response placement is also the only one that catches an alias
// re-pointing DURING a long run rather than only between two.

// modelGate compares what is answering now against what the run recorded.
//
// State lives here rather than on BaselineOptions because every method on that
// struct has a value receiver, so it is copied per call — a sync.Once field
// would be a govet copylocks failure, and a copied Once is a check that runs
// once per worker instead of once per run.
type modelGate struct {
	// recorded is the set of models the resumed Run observed. Empty on a fresh
	// run, which is why a fresh run is never gated.
	recorded []string

	once sync.Once
	err  error
}

// newModelGate arms the gate from a Run's record.
//
// A fresh run records nothing, so the gate stays inert: there is no prior
// measurement to be inconsistent with, and a run that legitimately observes two
// models mid-rollout is exactly why resolved_models is a set.
func newModelGate(run *knov1.Run) *modelGate {
	return &modelGate{recorded: run.GetCaseExecution().GetResolvedModels()}
}

// check reports whether this response's model contradicts the record.
//
// Runs at most once per run, under sync.Once: with concurrency there is no
// "first response", N workers can arrive together, and the answer cannot differ
// between them. The losers see the same stored result.
//
// Membership, not models[0]. The field is repeated because during a provider
// rollout two workers in one run legitimately see different builds, so a run
// that saw {A, B} and is now served by B has not changed — and comparing
// against whichever element sorted first would refuse it.
//
// Empty on either side means nothing to compare: a run whose Cases all errored
// records no model, and a response that reports none tells us nothing. Refusing
// on absence would make every run that stopped before its first answer
// unresumable.
func (g *modelGate) check(now string) error {
	if len(g.recorded) == 0 || now == "" {
		return nil
	}
	g.once.Do(func() {
		if slices.Contains(g.recorded, now) {
			return
		}
		g.err = errs.ErrCheckpointStale.
			WithFix("re-run without --resume, or pin the model in the agent ref " +
				"so a provider rollout cannot re-point it mid-run").
			Wrap(fmt.Errorf("this run was measured against %s and is now being "+
				"served %q; continuing would average two models into one score",
				strings.Join(g.recorded, ", "), now))
	})
	return g.err
}

// afterRecord is the executor hook.
//
// It runs once a result is DURABLY recorded, which is the whole reason the hook
// exists. Returning an error from the work would have discarded a paid,
// scoreable answer and filed the Case as an agent error — the mistake
// SettlementOvershoot already fixed once. Here the answer is kept, the store
// says so, and the run stops after it.
//
// The stop is not free and the plan says so plainly: IsFatal-driven shutdown
// reaches the other workers asynchronously, so up to concurrency-1 further
// Cases can be recorded under the new model before the run ends. The gate stops
// this getting worse; it cannot undo what is already mixed. incomplete_reason
// says which two models, and the fix line says to re-run without --resume.
func (g *modelGate) afterRecord(result any) error {
	r, ok := result.(executor.Result[*Case, caseOutcome])
	if !ok || r.Value == nil {
		return nil
	}
	return g.check(r.Value.Response.GetResolvedModel())
}
