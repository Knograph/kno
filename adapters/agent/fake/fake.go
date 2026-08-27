// Package fake is a deterministic Agent that costs nothing.
package fake

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Agent answers Cases without a network or a bill.
//
// It is a first-class adapter, not a test double. `kno baseline` must be
// runnable by someone evaluating the tool before they have configured a
// provider, and CI must be able to exercise the whole pipeline without a
// budget.
//
// What it can simulate is chosen to match what the executor needs proving
// against: latency that honors cancellation, scheduled failures, and rate
// limiting. What it cannot simulate — partial reads, connection reuse,
// mid-stream deadlines — is why docs/debt.md#23 stays open until a real
// adapter lands.
type Agent struct {
	opts Options

	calls       atomic.Int64
	rateLimited atomic.Int64

	// attempted records which Cases have been seen, so ThrottleFirstAttempt is
	// per Case rather than per global call.
	attempted sync.Map

	// injected counts how many measurements ran with each Asset in context,
	// so a Value test can assert the treatment arm carried the Asset and the
	// control arm did not — which is the entire measurement.
	injected sync.Map
}

// Options configures the fake.
type Options struct {
	// Answer produces the response for a Case. Nil means echo the expected
	// answer, which makes every Case pass — useful for exercising the happy
	// path end to end.
	Answer func(c *core.Case) string

	// Latency is how long each call takes. It is honored through a ctx-aware
	// wait, not a plain sleep: a sleep would only ever prove that cancellation
	// works BEFORE a call, never during one.
	Latency time.Duration

	// FailEvery makes every Nth call fail. Zero disables it.
	FailEvery int

	// ThrottleFirstAttempt makes the FIRST attempt at each Case return
	// ErrRateLimited, so a retry recovers it.
	//
	// Deterministic per Case, unlike RateLimitEvery, which counts global calls
	// — under concurrency that lets a Case's retries land on the same multiple
	// and be throttled repeatedly, which makes a retry test flaky rather than
	// meaningful.
	ThrottleFirstAttempt bool

	// RateLimitEvery makes every Nth call return ErrRateLimited. Zero disables
	// it.
	//
	// This exists so the executor's handling of a rate limit is built and
	// tested here rather than first exercised by a paid adapter, which is the
	// wrong place to discover it is wrong.
	RateLimitEvery int

	// CostPerCallUSDMicros is the reported cost. Zero by default: the fake
	// spends nothing, and a run against it must not consume a real budget.
	CostPerCallUSDMicros int64

	// ResolvedModel is what the fake claims actually answered, echoed onto
	// every Response.
	//
	// It exists so the resolved-model gate can be driven through a real run
	// rather than a synthesized Run record. A real provider reports this and
	// can change it mid-run when a moving alias re-points, which is the whole
	// hazard; nothing else in the tree can produce that.
	ResolvedModel string

	// ResolvedModelAfter re-points ResolvedModel from the Nth call onward,
	// simulating an alias moving DURING a run. Zero disables it.
	//
	// Counts calls rather than Cases because that is what a provider does.
	ResolvedModelAfter int

	// ResolvedModelThen is what the alias re-points TO.
	ResolvedModelThen string

	// Inject enables WithContext and declares ContextInject in Capabilities,
	// so a Value-stage test can run the real measurement path rather than a
	// synthesized one.
	Inject bool
}

// New returns a fake Agent.
func New(opts Options) *Agent { return &Agent{opts: opts} }

// Invoke answers one Case.
//
// Deterministic: the same Case always produces the same answer, so a run
// against the fake is reproducible and a golden test over its output is
// stable.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("fake: nil case")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	n := a.calls.Add(1)

	if a.opts.ThrottleFirstAttempt {
		if _, seen := a.attempted.LoadOrStore(c.GetId(), true); !seen {
			a.rateLimited.Add(1)
			return nil, errs.ErrRateLimited.Wrap(
				fmt.Errorf("fake: throttled the first attempt at %s", c.GetId()),
			)
		}
	}

	if a.opts.RateLimitEvery > 0 && n%int64(a.opts.RateLimitEvery) == 0 {
		a.rateLimited.Add(1)
		return nil, errs.ErrRateLimited.Wrap(
			fmt.Errorf("fake: rate limited on call %d", n),
		)
	}

	if a.opts.Latency > 0 {
		// A ctx-aware wait, so cancellation DURING a call is exercised. A
		// plain sleep would only ever prove cancellation before one.
		select {
		case <-time.After(a.opts.Latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if a.opts.FailEvery > 0 && n%int64(a.opts.FailEvery) == 0 {
		return nil, &errs.Actionable{
			Code:     "FAKE_AGENT_ERROR",
			Message:  fmt.Sprintf("the fake agent failed deliberately on call %d", n),
			Fix:      "lower fail_every, or use a real agent",
			ExitCode: errs.ExitError,
		}
	}

	answer := c.GetExpected()
	if a.opts.Answer != nil {
		answer = a.opts.Answer(c)
	}

	return &core.Response{
		CaseId:           c.GetId(),
		Output:           answer,
		PromptTokens:     int64(len(c.GetInput())),
		CompletionTokens: int64(len(answer)),
		CostUsdMicros:    a.opts.CostPerCallUSDMicros,
		LatencyMs:        a.opts.Latency.Milliseconds(),
		ResolvedModel:    a.resolvedModel(),
	}, nil
}

// resolvedModel reports which model answered, honoring a mid-run re-point.
//
// The counter is the same one FailEvery and RateLimitEvery use, so the
// re-point lands deterministically at a call number rather than at whichever
// Case a scheduler happened to run first.
func (a *Agent) resolvedModel() string {
	if a.opts.ResolvedModelAfter > 0 && a.opts.ResolvedModelThen != "" &&
		int(a.calls.Load()) >= a.opts.ResolvedModelAfter {
		return a.opts.ResolvedModelThen
	}
	return a.opts.ResolvedModel
}

// Capabilities reports what this adapter supports.
//
// The fake declares context injection: WithContext records which Asset is
// carried and delegates the call, which is what lets a Value run exercise the
// real measurement path — treatment arm carrying the Asset, control arm not —
// against an agent that costs nothing. There is no knowledge index to write,
// and claiming one would let a valuation run report a mode it never used.
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		ContextInject:  true,
		KnowledgeWrite: false,
		Stream:         false,
		TokenCounts:    true,
	}
}

// WithContext wraps the agent so the Asset travels with every Invoke, which is
// what makes the treatment arm the treatment arm.
func (a *Agent) WithContext(asset *core.Asset) (core.Agent, error) {
	return &contextAgent{inner: a, asset: asset}, nil
}

// contextAgent is the injected wrapper: it records the injection and
// delegates the call.
type contextAgent struct {
	inner *Agent
	asset *core.Asset
}

func (c *contextAgent) Invoke(ctx context.Context, cs *core.Case) (*core.Response, error) {
	n, _ := c.inner.injected.LoadOrStore(c.asset.GetId(), new(atomic.Int64))
	n.(*atomic.Int64).Add(1)
	return c.inner.Invoke(ctx, cs)
}

func (c *contextAgent) Capabilities() *core.Capabilities {
	return c.inner.Capabilities()
}

// Injected returns how many measurements ran with the named Asset in context.
func (a *Agent) Injected(assetID string) int64 {
	if n, ok := a.injected.Load(assetID); ok {
		return n.(*atomic.Int64).Load()
	}
	return 0
}

// Spends reports whether this agent can cost the user money.
//
// Always false. The fake makes no network call, so nothing it does can appear
// on anyone's invoice — CostPerCallUSDMicros is a SIMULATED figure that exists
// so budget-guard behavior can be tested against a real Guard, not a claim
// about real money.
//
// core defaults to "spends" for any adapter that stays silent, because
// treating a paid agent as free would skip the consent prime directive 4
// requires. This method is the one place that default is overridden, and it is
// what keeps the quickstart — and every test that measures spend arithmetic
// against a costed fake — from being asked to approve a bill that cannot
// arrive.
func (a *Agent) Spends() bool { return false }

// Calls returns how many times Invoke has been called.
func (a *Agent) Calls() int64 { return a.calls.Load() }

// RateLimited returns how many calls were rate limited.
func (a *Agent) RateLimited() int64 { return a.rateLimited.Load() }

// Wrong returns an Answer function that gets a deterministic share of Cases
// wrong, for exercising a Goal that does not score everything as a pass.
func Wrong(share float64) func(c *core.Case) string {
	return func(c *core.Case) string {
		h := fnv.New64a()
		_, _ = h.Write([]byte(c.GetId()))
		if float64(h.Sum64()%1000)/1000 < share {
			return "wrong: " + strings.ToUpper(c.GetExpected())
		}
		return c.GetExpected()
	}
}

var (
	_ core.Agent   = (*Agent)(nil)
	_ core.Capable = (*Agent)(nil)
)
