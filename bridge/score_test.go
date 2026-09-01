package bridge_test

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// scoreEvalRunnerCases is a minimal core.Evals over a fixed set, used to
// build the *core.SealedEvals ScoreEvalRunner.Cases requires.
type scoreEvalRunnerCases struct{ cases []*core.Case }

func (s scoreEvalRunnerCases) Cases(context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		for _, c := range s.cases {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

type scoreEvalRunnerGoal struct{ value float64 }

func (g scoreEvalRunnerGoal) Score(context.Context, *core.Case, *core.Response) (*core.Score, error) {
	return &knov1.Score{Value: g.value, Passed: g.value > 0}, nil
}
func (scoreEvalRunnerGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }
func (scoreEvalRunnerGoal) Domain() core.ScoreDomain {
	return knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL
}

// scoreEvalRunnerAgent records every Case it was invoked on. core.ScorePass
// dispatches Invoke calls concurrently (executor's default concurrency is
// greater than 1), so calls is guarded by a mutex — an unguarded append
// here is a real data race, not a test-only shortcut, and the race
// detector on -shuffle=on catches it precisely because two Cases can land
// on different worker goroutines.
type scoreEvalRunnerAgent struct {
	mu    sync.Mutex
	calls []string
}

func (a *scoreEvalRunnerAgent) Invoke(_ context.Context, c *core.Case) (*core.Response, error) {
	a.mu.Lock()
	a.calls = append(a.calls, c.GetId())
	a.mu.Unlock()
	return &knov1.Response{}, nil
}

func devCase(id string) *core.Case {
	return &core.Case{Id: id, Split: knov1.Split_SPLIT_DEV}
}

// TestScoreEvalRunnerMeasureScoresOnlyTheAskedForCases pins bridge's
// production EvalRunner: it invokes exactly the Case IDs it is handed,
// direction-normalises their scores, and asserts calls are free
// (AcceptFreeCalls) so a cost cap under the guard never trips core/ring0.go's
// zero-estimate refusal on inference the hosting ticker already meters.
func TestScoreEvalRunnerMeasureScoresOnlyTheAskedForCases(t *testing.T) {
	t.Parallel()

	agent := &scoreEvalRunnerAgent{}
	pool := scoreEvalRunnerCases{cases: []*core.Case{devCase("d1"), devCase("d2"), devCase("ctl1")}}
	// A cost cap, to prove AcceptFreeCalls is really asserted: without it,
	// an agent that is not an Estimator falls back to a zero scalar
	// estimate, which is ALSO accepted under a cap only because no
	// Estimator interface is implemented here — a weaker proof than
	// asserting free calls explicitly succeed under a cap that would
	// otherwise refuse an unpriced Estimator. See core/score_test.go's
	// TestScorePassAcceptFreeCallsBypassesZeroCostRefusal for the sharper
	// version of this proof at the ScorePass layer directly.
	guard := budget.New(budget.Limits{MaxCostUSDMicros: 1}, nil, 0)

	runner := &bridge.ScoreEvalRunner{
		Cases: core.Seal(pool), Goal: scoreEvalRunnerGoal{value: 0.75}, Guard: guard,
		NewAgent: func(context.Context, *knov1.AgentRef) (core.Agent, error) { return agent, nil },
	}

	scores, err := runner.Measure(context.Background(), "cluster-x",
		&knov1.AgentRef{Scheme: "openai", Target: "ft-model"}, []string{"d1", "d2"})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want 2", len(scores))
	}
	if scores["d1"] != 0.75 || scores["d2"] != 0.75 {
		t.Errorf("scores = %v, want d1=0.75 d2=0.75", scores)
	}
	if _, ok := scores["ctl1"]; ok {
		t.Error("Measure scored ctl1, which was not in the requested caseIDs")
	}
	if len(agent.calls) != 2 {
		t.Errorf("agent invoked %d times, want 2", len(agent.calls))
	}
	for _, id := range agent.calls {
		if id != "d1" && id != "d2" {
			t.Errorf("agent invoked on unexpected case %s", id)
		}
	}
}

// TestScoreEvalRunnerMeasurePropagatesAgentFactoryError pins that a
// failure to build an Agent for the deployed model is reported rather
// than silently scoring nothing.
func TestScoreEvalRunnerMeasurePropagatesAgentFactoryError(t *testing.T) {
	t.Parallel()

	runner := &bridge.ScoreEvalRunner{
		Cases: core.Seal(scoreEvalRunnerCases{}), Goal: scoreEvalRunnerGoal{value: 0.5},
		Guard: budget.New(budget.Limits{}, nil, 0),
		NewAgent: func(context.Context, *knov1.AgentRef) (core.Agent, error) {
			return nil, context.DeadlineExceeded
		},
	}
	_, err := runner.Measure(context.Background(), "cluster-x", &knov1.AgentRef{}, []string{"d1"})
	if err == nil {
		t.Fatal("want an error when the agent factory fails")
	}
}
