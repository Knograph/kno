package core

import (
	"context"
	"iter"
	"time"

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

// ContextSetInjector is an Agent that can carry a whole Portfolio in its
// prompt.
//
// A separate capability from ContextInjector rather than a loop over it, and
// the reason is a refusal that already exists: every ContextInjector in the
// tree rejects a SECOND Asset, because a per-Asset Valuation silently becoming
// a two-Asset one is precisely what that refusal prevents. Loosening it to
// serve Validate would remove a guard from the stage that needs it most, so
// Validate asks for a different operation instead.
//
// Like ContextInjector this is the UPPER-BOUND mode: the whole set is handed
// to the model directly rather than reached through a retriever, so the
// holdout gain it produces is an upper bound on what retrieval would deliver,
// and every rendering says so.
type ContextSetInjector interface {
	// WithContextSet returns an Agent carrying every Asset, in the given
	// order. The receiver is unmodified and remains usable as the control arm
	// of the same measurement — the same contract, for the same reason, as
	// ContextInjector.WithContext.
	//
	// ORDER IS PART OF THE MEASUREMENT and callers pass it deliberately:
	// Validate applies PortfolioEntry.rank. Providers cache on a PREFIX, and a
	// portfolio prefix that is byte-identical across every holdout Case is the
	// difference between paying for the set's tokens once and paying for them
	// on every Case.
	//
	// An empty or nil slice must be refused rather than answered: an Agent
	// carrying no Assets is the control arm, and returning one here would
	// measure the control against itself and report the difference as zero,
	// with an interval.
	WithContextSet(assets []*Asset) (Agent, error)
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
	// It MUST be local: arithmetic over a pricing table the adapter already
	// holds. It must not call the provider. This runs BEFORE the budget guard
	// authorizes anything, so a network call here spends money outside the
	// guard entirely — the one thing prime directive 4 forbids — and a slow one
	// blocks a worker while holding no reservation. The engine bounds the call
	// with a timeout, but a timeout cannot un-spend a request already sent.
	//
	// Calls must be exactly 1. One Invoke settles as one provider call, so
	// reserving more would reserve N and settle 1, and the call cap would drift
	// by (N-1) for every Case. An adapter whose single Invoke really does make
	// several provider requests needs the settlement side to carry that too;
	// until it does, the engine refuses the Case rather than mis-count it.
	//
	// An error means the cost is unknown, which is NOT the same as zero: a zero
	// estimate makes a dollar cap unenforceable, so the engine treats a
	// zero-cost answer under a cost cap exactly as it treats an error. Report
	// the error; do not invent a cheap number.
	Estimate(ctx context.Context, c *Case) (budget.Estimate, error)

	// WorstCase reports the most any single Case could cost, before any Case is
	// seen.
	//
	// Planning needs a number and per-Case estimates need a Case, so without
	// this the engine plans against BaselineOptions.EstCostPerCallUSDMicros —
	// a scalar an Estimator does not use. Measured with an adapter pricing at
	// $0.20 against a scalar of $0.001: the consent prompt quoted $0.06 for a
	// run whose real exposure was $12.00, and the feasibility check computed
	// enough headroom for 250 in-flight Cases while the run stalled at 0 of 60.
	//
	// An adapter can answer because the output term dominates and is known up
	// front: the output ceiling times the output rate, plus whatever the
	// largest plausible prompt costs. It must be an upper bound, for the same
	// reason Estimate must be.
	WorstCase() budget.Estimate
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

	// Domain reports the set of values Score.value can take.
	//
	// Declared, never inferred from the scores a run happens to produce.
	// Inferring it is method selection from the sample: a confidence level
	// would then hold only conditional on a branch that is itself a function
	// of the data, one extra observation could flip a measurement between
	// branches, and across many measurements some would land in each branch by
	// luck while a consumer compared their intervals as if commensurable.
	//
	// It is on the interface rather than optional so that a new Goal cannot
	// land without answering — the interval method for every delta measured
	// against it depends on this, and a Goal that stayed silent would get the
	// continuous methods, which are valid but wider than they need to be.
	Domain() ScoreDomain

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
// Every Submit is a spend path, so it must pass the budget guard first. So is
// Deploy: a Together dedicated endpoint bills per minute per replica, idle
// included, which is a SECOND spend shape distinct from Submit's one-time
// commitment — see the bridge plan's Step 2(f). Both must flow through
// stats/budget before any request leaves.
type Tuner interface {
	// Submit sends a tuning job to the provider.
	Submit(ctx context.Context, job *TuningJob) (*JobRef, error)

	// Status reports where a submitted job stands.
	Status(ctx context.Context, ref *JobRef) (*JobState, error)

	// Model returns the tuned model as an agent ref.
	//
	// This is NOT a promise the model is reachable. Some providers auto-serve
	// a finished job; Together does not — reaching a Together fine-tune over
	// HTTP requires Deploy first. A caller that needs to invoke the model
	// must call Deploy and wait for the returned Endpoint to be ready before
	// using this ref against an Agent adapter.
	Model(ctx context.Context, ref *JobRef) (*AgentRef, error)

	// Deploy brings a finished job's model up behind a servable endpoint.
	//
	// This is the SECOND spend shape the bridge introduces: unlike Submit, it
	// bills incrementally (per minute per replica, idle included) and CAN be
	// stopped, so the engine settles it forward per tick through the budget
	// guard rather than reserving a pessimistic lump sum at call time — see
	// Step 2(f). A Tuner whose provider auto-serves a finished job (no
	// separate deploy step) implements this as a no-op returning a
	// zero-rate Endpoint, which is how the interface stays honest for a
	// provider that is not Together.
	//
	// The caller is responsible for waiting until the returned Endpoint is
	// ready (however this Tuner signals that — a status field, a poll) before
	// invoking the model, and for calling Teardown exactly once when done,
	// unconditionally, including on every error and cancellation path.
	Deploy(ctx context.Context, ref *JobRef) (*Endpoint, error)

	// Teardown stops a deployed endpoint and its billing.
	//
	// MUST be called on every exit path once Deploy has returned successfully
	// — success, eval-pass failure, a budget or serve-minute cap, a timeout,
	// or cancellation. An error here means the endpoint may still be live and
	// billing after the caller has moved on: the caller must treat that as
	// loud (see docs — a leaked endpoint is reported, never swallowed), not
	// as a retryable nuisance.
	Teardown(ctx context.Context, ep *Endpoint) error
}

// Endpoint is a tuned model's live serving location.
//
// A Go struct, not a proto message — deliberately. It is an adapter-lifecycle
// type with (for now) exactly one implementation, and committing a wire
// contract to it would repeat the mistake TuningJob.lora_rank nearly made:
// describing a shape no second adapter has confirmed. The bridge plan's Step
// 2(f) is explicit about this; TuningEndpointChanged carries the fields a
// consumer needs on the event stream instead.
type Endpoint struct {
	// ID is the provider-assigned endpoint identifier — what a user would
	// look up in the provider's own console if Teardown ever fails.
	ID string

	// Provider names which Tuner this came from: "together", "fireworks",
	// "openai".
	Provider string

	// Served is the model this endpoint serves, usable as the Ref of an
	// AgentRef once Ready is true.
	Served string

	// Replicas is how many running replicas are billing. Together's per-
	// minute rate is per replica; a zero-rate no-op Endpoint (an
	// auto-serving provider) may report zero here without implying nothing
	// is servable.
	Replicas int

	// Ready reports whether the endpoint is currently answering requests.
	// False while deploying or after teardown.
	Ready bool

	// ReadyAt is when the endpoint first became ready, zero if it never did.
	// The billing clock the per-minute settlement loop reads from.
	ReadyAt time.Time
}
