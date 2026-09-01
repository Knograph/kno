package core_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// scoreGoal is the minimal Goal ScorePass's tests need: a declared
// direction/domain and a scripted score.
type scoreGoal struct {
	dir       core.Direction
	domain    core.ScoreDomain
	value     float64
	scoreErr  error
	scoreFunc func(c *core.Case, r *core.Response) (*core.Score, error)
}

func (g scoreGoal) Score(_ context.Context, c *core.Case, r *core.Response) (*core.Score, error) {
	if g.scoreFunc != nil {
		return g.scoreFunc(c, r)
	}
	if g.scoreErr != nil {
		return nil, g.scoreErr
	}
	return &knov1.Score{Value: g.value, Passed: g.value > 0}, nil
}

func (g scoreGoal) Direction() core.Direction { return g.dir }
func (g scoreGoal) Domain() core.ScoreDomain  { return g.domain }

func maximizeGoal(value float64) scoreGoal {
	return scoreGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL, value: value}
}

// scoreScriptedAgent answers per-Case, scripted by Case ID, and counts how
// many times each Case was invoked — the thing every ScorePass test needs to
// assert on: was a skipped Case ever invoked, did a retry actually retry.
type scoreScriptedAgent struct {
	mu       sync.Mutex
	calls    map[string]int
	behavior func(callNum int, c *core.Case) (*core.Response, error)
}

func (a *scoreScriptedAgent) Invoke(_ context.Context, c *core.Case) (*core.Response, error) {
	a.mu.Lock()
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	a.calls[c.GetId()]++
	n := a.calls[c.GetId()]
	a.mu.Unlock()
	if a.behavior != nil {
		return a.behavior(n, c)
	}
	return &knov1.Response{}, nil
}

func (a *scoreScriptedAgent) callCount(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[id]
}

// scoreEstimatorAgent is a scoreScriptedAgent that also implements
// core.Estimator, scripted to return a fixed cost (possibly zero).
type scoreEstimatorAgent struct {
	scoreScriptedAgent
	costUSDMicros int64
	estErr        error
}

func (a *scoreEstimatorAgent) Estimate(context.Context, *core.Case) (budget.Estimate, error) {
	if a.estErr != nil {
		return budget.Estimate{}, a.estErr
	}
	return budget.Estimate{Calls: 1, CostUSDMicros: a.costUSDMicros}, nil
}

func (a *scoreEstimatorAgent) WorstCase() budget.Estimate {
	return budget.Estimate{Calls: 1, CostUSDMicros: a.costUSDMicros}
}

func devCases(ids ...string) []*core.Case {
	out := make([]*core.Case, 0, len(ids))
	for _, id := range ids {
		out = append(out, &core.Case{Id: id, Split: knov1.Split_SPLIT_DEV})
	}
	return out
}

func sealedFromCases(cases []*core.Case) *core.SealedEvals {
	return core.Seal(&fakeEvals{cases: cases, yieldAt: -1})
}

// TestScorePassRecordsAllThreeSpendDimensions covers the plan's required
// coverage: Calls, CostUSDMicros and Tokens must all be recorded — dropping
// Tokens has been the same bug twice (#170, #172).
func TestScorePassRecordsAllThreeSpendDimensions(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{behavior: func(int, *core.Case) (*core.Response, error) {
		return &knov1.Response{CostUsdMicros: 1_000, PromptTokens: 10, CompletionTokens: 5}, nil
	}}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.8), Cases: sealedFromCases(devCases("c1", "c2")),
		Guard: guard,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if res.Spent.Calls != 2 {
		t.Errorf("Spent.Calls = %d, want 2", res.Spent.Calls)
	}
	if res.Spent.CostUSDMicros != 2_000 {
		t.Errorf("Spent.CostUSDMicros = %d, want 2000", res.Spent.CostUSDMicros)
	}
	if res.Spent.Tokens != 30 {
		t.Errorf("Spent.Tokens = %d, want 30 (dropping tokens has been this bug twice)", res.Spent.Tokens)
	}
	if len(res.Scores) != 2 {
		t.Errorf("Scores = %v, want 2 entries", res.Scores)
	}
}

// TestScorePassBudgetRefusalMidPass covers a cap that admits only some
// Cases: the pass must stop rather than silently spending past the cap, and
// must report what it settled before stopping.
func TestScorePassBudgetRefusalMidPass(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{behavior: func(int, *core.Case) (*core.Response, error) {
		return &knov1.Response{CostUsdMicros: 1_000_000}, nil
	}}
	// Room for exactly one Case's cost. EstCostPerCallUSDMicros must match
	// what the agent actually bills, or Authorize's estimate (not the real
	// charge) is what decides whether a reservation fits — a fake agent
	// that is not an Estimator is priced from this scalar alone.
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1", "c2", "c3")),
		Guard: guard, Concurrency: 1, EstCostPerCallUSDMicros: 1_000_000,
	})
	if err == nil {
		t.Fatal("want an error: the cap should stop the pass")
	}
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("err = %v, want errs.ErrBudgetExceeded", err)
	}
	if res.Spent.CostUSDMicros > 1_000_000 {
		t.Errorf("Spent.CostUSDMicros = %d, exceeded the cap of 1000000", res.Spent.CostUSDMicros)
	}
}

// TestScorePassPanickingAgent covers a panic in Invoke: it must not take the
// pass down, and the Case must be recorded as errored rather than silently
// dropped.
func TestScorePassPanickingAgent(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{behavior: func(int, *core.Case) (*core.Response, error) {
		panic("boom")
	}}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if _, ok := res.Scores["c1"]; ok {
		t.Error("a panicking Case must not produce a score")
	}
	if res.Errors["c1"] == "" {
		t.Error("a panicking Case must be recorded as errored")
	}
}

// TestScorePassRetryableErrorSucceedsOnRetry covers the shared invoker's
// retry loop reaching ScorePass: a rate-limited first attempt followed by a
// success must score, and must have actually retried (two calls, not one).
func TestScorePassRetryableErrorSucceedsOnRetry(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{behavior: func(n int, _ *core.Case) (*core.Response, error) {
		if n == 1 {
			return nil, errs.ErrRateLimited
		}
		return &knov1.Response{CostUsdMicros: 500}, nil
	}}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.9), Cases: sealedFromCases(devCases("c1")),
		Guard: guard, RetryBackoff: 1,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if agent.callCount("c1") != 2 {
		t.Errorf("Invoke called %d times, want 2 (one failure, one retry)", agent.callCount("c1"))
	}
	if got, ok := res.Scores["c1"]; !ok || got != 0.9 {
		t.Errorf("Scores[c1] = %v, %v, want 0.9, true", got, ok)
	}
}

// TestScorePassSkipNeverInvokesASkippedCase is the resume contract: a Case
// Skip reports as already-scored must never reach Invoke, or a resumed
// bridge group would re-pay for it.
func TestScorePassSkipNeverInvokesASkippedCase(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1", "c2")),
		Guard: guard,
		Skip:  func(id string) bool { return id == "c1" },
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if agent.callCount("c1") != 0 {
		t.Errorf("Invoke called %d times for a skipped case, want 0", agent.callCount("c1"))
	}
	if _, ok := res.Scores["c1"]; ok {
		t.Error("a skipped case must not appear in Scores")
	}
	if _, ok := res.Scores["c2"]; !ok {
		t.Error("an unskipped case must be scored")
	}
}

// TestScorePassOnScoredErrorStopsThePass pins that OnScored is the
// durability seam: an error from it must stop the pass, per ScoreParams.OnScored's doc.
func TestScorePassOnScoredErrorStopsThePass(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{}
	guard := budget.New(budget.Limits{}, nil, 0)
	boom := errors.New("store write failed")

	_, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard,
		OnScored: func(context.Context, string, float64, budget.Spend) error {
			return boom
		},
	})
	if err == nil {
		t.Fatal("want an error: OnScored failing must stop the pass")
	}
}

// TestScorePassDirectionNormalization pins ScoreResult.Scores's contract:
// higher always means better, regardless of the Goal's own polarity.
func TestScorePassDirectionNormalization(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{}
	guard := budget.New(budget.Limits{}, nil, 0)
	minimizeGoal := scoreGoal{
		dir: knov1.Direction_DIRECTION_MINIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED,
		value: 4.0,
	}

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: minimizeGoal, Cases: sealedFromCases(devCases("c1")),
		Guard: guard,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if got := res.Scores["c1"]; got != -4.0 {
		t.Errorf("Scores[c1] = %v, want -4 (a MINIMIZE goal's raw score sign-flipped)", got)
	}
}

// TestScorePassAcceptFreeCallsBypassesZeroCostRefusal is decision #4's
// executable form: without AcceptFreeCalls, a zero-cost Estimator under a
// cap is refused exactly like core/ring0.go's Estimator doc describes; with
// it, the SAME zero estimate is accepted because it is an assertion rather
// than a discovery.
func TestScorePassAcceptFreeCallsBypassesZeroCostRefusal(t *testing.T) {
	t.Parallel()
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	t.Run("without the assertion, a zero estimate under a cap is refused", func(t *testing.T) {
		t.Parallel()
		agent := &scoreEstimatorAgent{costUSDMicros: 0}
		// Per-Case, mirroring BaselineOptions.estimate: an unpriceable Case
		// fails that Case (errs.ErrInvalidInput, not errs.ErrBudgetExceeded
		// or a run-fatal sentinel), so ScorePass itself returns no error —
		// the refusal shows up as c1 never being scored.
		res, err := core.ScorePass(context.Background(), core.ScoreParams{
			Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
			Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
			Guard: guard,
		})
		if err != nil {
			t.Fatalf("ScorePass: %v", err)
		}
		if _, ok := res.Scores["c1"]; ok {
			t.Error("want c1 unscored: an unasserted zero estimate under a cap must not pass silently")
		}
		if res.Errors["c1"] == "" {
			t.Error("want c1 recorded as errored")
		}
		if agent.callCount("c1") != 0 {
			t.Error("want Invoke never called: the refusal must happen before any spend")
		}
	})

	t.Run("with the assertion, the same zero estimate is accepted", func(t *testing.T) {
		t.Parallel()
		agent := &scoreEstimatorAgent{costUSDMicros: 0}
		res, err := core.ScorePass(context.Background(), core.ScoreParams{
			Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
			Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
			Guard: guard, AcceptFreeCalls: true,
		})
		if err != nil {
			t.Fatalf("ScorePass: %v", err)
		}
		if _, ok := res.Scores["c1"]; !ok {
			t.Error("want c1 scored")
		}
		if res.Spent.CostUSDMicros != 0 {
			t.Errorf("Spent.CostUSDMicros = %d, want 0", res.Spent.CostUSDMicros)
		}
	})
}

// TestScorePassNeverSeesTheHoldout is the bridge equivalent of
// TestSelectHoldoutCanary and TestValueNeverTouchesTheHoldout: a sentinel
// holdout Case behind a *SealedEvals must never be scored or reach OnScored.
func TestScorePassNeverSeesTheHoldout(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{}
	guard := budget.New(budget.Limits{}, nil, 0)
	sealed := core.Seal(&fakeEvals{cases: mixedCases(), yieldAt: -1})

	var onScoredIDs []string
	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealed, Guard: guard,
		OnScored: func(_ context.Context, id string, _ float64, _ budget.Spend) error {
			onScoredIDs = append(onScoredIDs, id)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	for id := range res.Scores {
		if agent.callCount(id) == 0 {
			t.Errorf("scored case %s was never invoked", id)
		}
	}
	for _, id := range onScoredIDs {
		if id == "holdout-1" || id == "holdout-2" {
			t.Errorf("HOLDOUT LEAK: OnScored saw %s", id)
		}
	}
	if agent.callCount("holdout-1") != 0 || agent.callCount("holdout-2") != 0 {
		t.Error("HOLDOUT LEAK: a holdout case was invoked")
	}
	// dev-1, dev-2 only — unassigned split is filtered by Seal too.
	if len(res.Scores) != 2 {
		t.Errorf("Scores has %d entries, want 2 (dev-1, dev-2 only)", len(res.Scores))
	}
}

// TestScorePassValidateRequiredFields covers ScoreParams.validate's
// required-field branches.
func TestScorePassValidateRequiredFields(t *testing.T) {
	t.Parallel()
	guard := budget.New(budget.Limits{}, nil, 0)
	agent := &scoreScriptedAgent{}
	full := func() core.ScoreParams {
		return core.ScoreParams{
			Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
			Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")), Guard: guard,
		}
	}
	tests := []struct {
		name   string
		mutate func(p *core.ScoreParams)
	}{
		{"no agent", func(p *core.ScoreParams) { p.Agent = nil }},
		{"no agent ref", func(p *core.ScoreParams) { p.AgentRef = nil }},
		{"no goal", func(p *core.ScoreParams) { p.Goal = nil }},
		{"no cases", func(p *core.ScoreParams) { p.Cases = nil }},
		{"no guard", func(p *core.ScoreParams) { p.Guard = nil }},
		{"no direction", func(p *core.ScoreParams) {
			p.Goal = scoreGoal{dir: knov1.Direction_DIRECTION_UNSPECIFIED, domain: knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := full()
			tc.mutate(&p)
			if _, err := core.ScorePass(context.Background(), p); err == nil {
				t.Errorf("want an error for %s", tc.name)
			}
		})
	}
}

// TestScorePassErrorPropagatesCode covers that an errored Case's failure
// reaches ScoreResult.Errors with a machine-readable code, never the
// verbatim message alone, matching codeOf's contract elsewhere.
func TestScorePassErrorPropagatesCode(t *testing.T) {
	t.Parallel()
	agent := &scoreScriptedAgent{behavior: func(int, *core.Case) (*core.Response, error) {
		return nil, fmt.Errorf("boom")
	}}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if res.Errors["c1"] == "" {
		t.Error("want a non-empty error code for c1")
	}
}

// TestScorePassRespectsCustomRetryAndConcurrencyOptions covers the
// non-default branches of ScoreParams.maxAttempts/retryBudget/concurrency:
// each configured value must be the one actually used, not silently
// replaced by the default.
func TestScorePassRespectsCustomRetryAndConcurrencyOptions(t *testing.T) {
	t.Parallel()
	var attemptsSeen int
	agent := &scoreScriptedAgent{behavior: func(n int, _ *core.Case) (*core.Response, error) {
		if n < 2 {
			return nil, errs.ErrRateLimited
		}
		attemptsSeen = n
		return &knov1.Response{}, nil
	}}
	guard := budget.New(budget.Limits{}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard, Concurrency: 2, MaxAttempts: 5,
		RetryBudget: 500_000_000, RetryBackoff: 1,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if attemptsSeen != 2 {
		t.Errorf("attempts seen = %d, want 2 (one retry within the configured MaxAttempts)", attemptsSeen)
	}
	if _, ok := res.Scores["c1"]; !ok {
		t.Error("want c1 scored after its retry succeeded")
	}
}

// TestScorePassEstimateFallsBackToScalarWithoutACap covers estimate's
// "the Estimator errored and there is no cost cap" branch: the run
// proceeds on the scalar fallback rather than refusing, because an
// uncapped run has nothing for an unknown price to threaten.
func TestScorePassEstimateFallsBackToScalarWithoutACap(t *testing.T) {
	t.Parallel()
	agent := &scoreEstimatorAgent{estErr: fmt.Errorf("cannot price this model")}
	guard := budget.New(budget.Limits{}, nil, 0) // no cap

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard, EstCostPerCallUSDMicros: 42,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if _, ok := res.Scores["c1"]; !ok {
		t.Error("want c1 scored: an unknown price with no cap must not block the run")
	}
}

// TestScorePassRefusesAnEstimateReservingMoreThanOneCall covers
// estimate's Estimate.Calls != 1 refusal — one Invoke settles as exactly
// one call, so an Estimator claiming otherwise is an adapter bug this
// pass must catch rather than silently under-count against the call cap.
func TestScorePassRefusesAnEstimateReservingMoreThanOneCall(t *testing.T) {
	t.Parallel()
	agent := &scoreEstimatorAgentMultiCall{}
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 1_000_000}, nil, 0)

	res, err := core.ScorePass(context.Background(), core.ScoreParams{
		Agent: agent, AgentRef: &knov1.AgentRef{Scheme: "fake"},
		Goal: maximizeGoal(0.5), Cases: sealedFromCases(devCases("c1")),
		Guard: guard,
	})
	if err != nil {
		t.Fatalf("ScorePass: %v", err)
	}
	if _, ok := res.Scores["c1"]; ok {
		t.Error("want c1 unscored: an Estimate claiming more than one call must be refused")
	}
	if res.Errors["c1"] == "" {
		t.Error("want c1 recorded as errored")
	}
}

// scoreEstimatorAgentMultiCall is a fixed-shape Estimator whose Estimate
// always claims 2 calls — an adapter bug ScorePass must catch.
type scoreEstimatorAgentMultiCall struct{ scoreScriptedAgent }

func (a *scoreEstimatorAgentMultiCall) Estimate(context.Context, *core.Case) (budget.Estimate, error) {
	return budget.Estimate{Calls: 2, CostUSDMicros: 100}, nil
}

func (a *scoreEstimatorAgentMultiCall) WorstCase() budget.Estimate {
	return budget.Estimate{Calls: 1, CostUSDMicros: 100}
}
