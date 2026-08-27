package core

import (
	"fmt"
	"slices"
	"strings"

	"github.com/knograph/kno/core/errs"
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
// struct has a value receiver, so it is copied per call.
//
// Stateless on purpose. The first version memoized the verdict under a
// sync.Once, which made the gate blind to exactly the case it was moved to
// first-response time to catch: if the FIRST response matched, the Once was
// spent and every later re-point returned nil. Measured — check("model-a")
// then check("model-b") against a run recording {model-a} returned nil twice.
//
// The Once was defending a race that cannot happen. AfterRecord runs only on
// the sink goroutine, which is single, and the executor latches gateFired so
// the hook stops firing after the first error. Its stated justification — "the
// answer cannot differ between workers" — also contradicted the membership
// rule below, which exists precisely because two workers CAN see different
// models.
type modelGate struct {
	// recorded is the set of models the resumed Run observed. Empty on a fresh
	// run, which is why a fresh run is never gated.
	recorded []string
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
// Evaluated on EVERY response, not memoized. A provider can re-point a moving
// alias at any point in a long run, and a gate that answers once answers about
// the wrong moment. The executor stops calling AfterRecord after the first
// error, so "once per run" is enforced where it belongs rather than here.
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
	if len(g.recorded) == 0 || now == "" || slices.Contains(g.recorded, now) {
		return nil
	}
	return errs.ErrCheckpointStale.
		WithFix("re-run without --resume, or pin the model in the agent ref " +
			"so a provider rollout cannot re-point it mid-run").
		Wrap(fmt.Errorf("this run was measured against %s and is now being "+
			"served %q; continuing would average two models into one score",
			strings.Join(g.recorded, ", "), now))
}

// afterRecord is the executor hook.
//
// It runs once a result is DURABLY recorded, which is the whole reason the hook
// exists. Returning an error from the work would have discarded a paid,
// scoreable answer and filed the Case as an agent error — the mistake
// SettlementOvershoot already fixed once. Here the answer is kept, the store
// says so, and the run stops after it.
//
// The stop is not free, and two limits are worth stating rather than
// discovering. IsFatal-driven shutdown reaches the other workers
// asynchronously, so up to concurrency-1 further Cases can be recorded under
// the new model before the run ends — the gate stops this getting worse, it
// cannot undo what is already mixed. And the tripping Case is kept, so its new
// model joins resolved_models at close: a SECOND --resume arms the gate from
// the enlarged set, matches, and completes a blended run. Both are
// docs/debt.md#55, and neither is annotated on the Run today.
func (g *modelGate) afterRecord(result any) error {
	// The executor hands the Result over as `any`; both stages' outcome types
	// implement Model() so the gate reads the response's model without
	// knowing which stage produced it. A type assert on Baseline's concrete
	// outcome type is how the Value stage wired this hook and the gate never
	// fired — the assert failed, returned nil, and the run blended two models
	// with no error.
	r, ok := result.(interface{ Model() string })
	if !ok {
		return nil
	}
	return g.check(r.Model())
}
