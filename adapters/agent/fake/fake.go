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
				fmt.Errorf("fake: throttled the first attempt at %s", c.GetId()))
		}
	}

	if a.opts.RateLimitEvery > 0 && n%int64(a.opts.RateLimitEvery) == 0 {
		a.rateLimited.Add(1)
		return nil, errs.ErrRateLimited.Wrap(
			fmt.Errorf("fake: rate limited on call %d", n))
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
	}, nil
}

// Capabilities reports what this adapter supports.
//
// The fake declares no injection capability. It answers Cases; it has no
// context to inject into and no knowledge index to write, and claiming
// otherwise would let a valuation run report a measurement mode it never used.
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		ContextInject:  false,
		KnowledgeWrite: false,
		Stream:         false,
		TokenCounts:    true,
	}
}

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
