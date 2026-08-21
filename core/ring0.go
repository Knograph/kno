package core

import (
	"context"
	"iter"

	"github.com/knograph/kno/stats/budget"
)

// Agent is anything invokable on a Case.
//
// This is the narrowest useful contract: given a Case, produce a Response.
// Everything else an agent might support — context injection, a writable
// knowledge index, streaming — is a CAPABILITY declared through Capable, not a
// requirement. The engine degrades per-adapter rather than refusing to run, and
// the report labels every measurement with the mode actually used.
//
// Implementations must not retain c beyond the call. See the borrowing rule on
// Evals.Cases.
type Agent interface {
	// Invoke runs the agent on one Case.
	//
	// c must be non-nil. A failed invocation should be returned as an error;
	// the caller records it rather than dropping it, because silently
	// discarding failures biases every number downstream.
	Invoke(ctx context.Context, c *Case) (*Response, error)
}

// Capable is implemented by adapters that declare what they support.
//
// Capabilities are checked BEFORE work is scheduled, so an unsupported
// injection mode is reported as a capability gap at connect time rather than
// as a run of mysterious failures. An adapter that does not implement Capable
// is treated as supporting nothing beyond Invoke.
type Capable interface {
	// Capabilities reports what this adapter supports. It must not return nil.
	Capabilities() *Capabilities
}

// ContextInjector is an Agent that can carry an Asset in its prompt.
//
// This is the UPPER-BOUND measurement mode: it bypasses retrieval entirely, so
// it measures what an Asset could contribute if retrieval were perfect. Results
// are reported as bounds, never as deployment predictions.
type ContextInjector interface {
	// WithContext returns an Agent that carries a in its context. The receiver
	// is unmodified, so the original Agent remains usable as a control.
	WithContext(a *Asset) (Agent, error)
}

// Estimator is an Agent that can say what a Case will cost before it runs.
//
// Named for the value it produces — budget.Estimate — rather than introducing
// a second word for the same act. The plan called this a Predictor; "predict"
// and "estimate" would then be two names for one thing, which is the drift the
// vocabulary rule exists to stop.
//
// Optional, like Capable and the injectors: an adapter that does not implement
// it falls back to BaselineOptions.EstCostPerCallUSDMicros, so the fake agent
// and every existing caller are unaffected.
//
// It exists because a cost cap the guard checks only at settlement is a cap
// discovered after the money is gone — that failure already overshot a $0.10
// cap once. And the cost of a Case depends on the Case: its input tokens are
// the input term of the arithmetic. A single run-scoped scalar cannot express
// that, which is why this is an interface on the adapter rather than another
// field on the options.
//
// The estimate must be PESSIMISTIC — the most a call could cost, not the most
// likely. It bounds a reservation, and a bound that can be too low is not a
// bound. Under-predicting is how a run walks past its cap; over-predicting only
// makes the guard refuse early, which is the recoverable direction.
type Estimator interface {
	// Estimate reports the most one Invoke of c could cost.
	//
	// An error means the cost is unknown, which is NOT the same as zero: a zero
	// estimate makes a dollar cap unenforceable. A caller with a cost cap must
	// refuse the run rather than substitute one.
	Estimate(ctx context.Context, c *Case) (budget.Estimate, error)
}

// KnowledgeInjector is an Agent whose knowledge index Kno can write.
//
// This is the DEPLOYMENT-FAITHFUL mode: the Asset is reached through the
// agent's own retriever, so the measurement reflects what would actually
// happen in production. It is also the only mode that preserves the difference
// between "the data was missing" and "the data was there but retrieval missed
// it" — context injection collapses the two.
type KnowledgeInjector interface {
	// WithKnowledge writes a into the agent's index and returns an Agent that
	// can retrieve it, plus a rollback function.
	//
	// The rollback MUST be called when the caller is done, including on error
	// paths — otherwise a valuation run permanently mutates the user's
	// production knowledge base. `defer rollback()` is the sanctioned idiom.
	WithKnowledge(ctx context.Context, a *Asset) (agent Agent, rollback func() error, err error)
}

// Evals supplies the Cases to measure against — the exam.
//
// Deliberately separate from Pool: the exam and the study material are
// different things with different sources, and conflating them made --pool
// ambiguous.
type Evals interface {
	// Cases returns an iterator over the eval Cases.
	//
	// The outer error reports a failure to OPEN the source — an unreadable
	// file, an unreachable server. Once that succeeds, the contract on the
	// iterator is:
	//
	//   - A yielded error is FATAL. The consumer MUST stop ranging. Adapters
	//     that tolerate malformed records handle them internally and report
	//     counts via Provenance — never by yielding a skippable error. This is
	//     one rule rather than two on purpose: if one adapter skipped bad
	//     records and another halted, the denominator behind every confidence
	//     interval would silently vary by adapter, and nothing would show it.
	//
	//   - The producer MUST defer resource cleanup INSIDE the iterator
	//     closure. Early break is legal and expected, and cleanup registered
	//     outside the closure will not run.
	//
	//   - The producer MUST check ctx.Err() before each yield. iter.Seq2
	//     carries no cancellation channel of its own.
	//
	//   - Yielded values are BORROWED for one iteration. Consumers clone
	//     before retaining or mutating; producers may reuse the backing memory
	//     between yields.
	//
	// Adapters prove they satisfy this by passing coretest.ConformIterator.
	Cases(ctx context.Context) (iter.Seq2[*Case, error], error)
}

// Pool supplies the candidate Assets — the study material.
type Pool interface {
	// Assets returns an iterator over the candidate Assets.
	//
	// The contract is identical to Evals.Cases: a yielded error is fatal,
	// cleanup is deferred inside the closure, ctx is checked before each
	// yield, and values are borrowed for one iteration. See Evals.Cases.
	Assets(ctx context.Context) (iter.Seq2[*Asset, error], error)
}

// Goal scores an outcome. Composable and weighted, defined as YAML plus BAML
// prompt files — extending a Goal is a prompt edit, never a Go plugin.
type Goal interface {
	// Score judges one Response against its Case.
	Score(ctx context.Context, c *Case, r *Response) (*Score, error)

	// Direction reports which way is better for this Goal.
	//
	// Without it the sign of every delta is uninterpretable: a -0.03 is an
	// improvement for a latency Goal and a regression for an accuracy one.
	// Implementations must not return DIRECTION_UNSPECIFIED.
	Direction() Direction
}

// Tuner submits and tracks fine-tuning jobs on hosted APIs.
//
// Orchestration is HTTP calls — there is no torch in this binary. That
// constraint is what makes proxy fine-tuning affordable enough to be a
// measurement rather than an infrastructure commitment.
//
// Every Submit is a spend path, so it must pass the budget guard first.
type Tuner interface {
	// Submit sends a tuning job to the provider.
	Submit(ctx context.Context, job *TuningJob) (*JobRef, error)

	// Status reports where a submitted job stands.
	Status(ctx context.Context, ref *JobRef) (*JobState, error)

	// Model returns the tuned model as an agent ref, usable directly for
	// post-tune validation against the same untouched holdout.
	Model(ctx context.Context, ref *JobRef) (*AgentRef, error)
}
