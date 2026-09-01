package judge_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/budget"
)

// The synthetic Goals every calibration test is driven with.
//
// They are deterministic by construction — a Goal whose verdict depended on a
// clock or an unseeded RNG would make the gate's own tests flap, which is the
// failure a gate is least able to survive.

// constantGoal answers the same way for every record. It is the degenerate
// judge the whole statistic exists to catch.
type constantGoal struct{ pass bool }

func (g constantGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	return &knov1.Score{CaseId: c.GetId(), Value: boolValue(g.pass), Passed: g.pass}, nil
}
func (constantGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (constantGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// truthGoal reads the human verdict off the record's own expected field, then
// flips it for the record ids in flip. That is how a judge with an EXACT,
// stated error rate is built: no sampling, so the test asserts an identity
// rather than a distribution.
type truthGoal struct {
	truth map[string]bool
	flip  map[string]bool
}

func (g truthGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	v := g.truth[c.GetId()]
	if g.flip[c.GetId()] {
		v = !v
	}
	return &knov1.Score{CaseId: c.GetId(), Value: boolValue(v), Passed: v}, nil
}
func (truthGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (truthGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// erroringGoal fails on a stated fraction of records.
type erroringGoal struct {
	truth map[string]bool
	fail  map[string]bool
}

func (g erroringGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	if g.fail[c.GetId()] {
		return nil, fmt.Errorf("judge failed on %s", c.GetId())
	}
	v := g.truth[c.GetId()]
	return &knov1.Score{CaseId: c.GetId(), Value: boolValue(v), Passed: v}, nil
}
func (erroringGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (erroringGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// gradedGoal declares a continuous domain.
type gradedGoal struct{ truth map[string]bool }

func (g gradedGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	v := 0.25
	if g.truth[c.GetId()] {
		v = 0.75
	}
	return &knov1.Score{CaseId: c.GetId(), Value: v, Passed: v > 0.5}, nil
}

func (gradedGoal) Domain() core.ScoreDomain {
	return knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL
}
func (gradedGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// outOfDomainGoal returns a value its declared Domain cannot take.
type outOfDomainGoal struct{}

func (outOfDomainGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	return &knov1.Score{CaseId: c.GetId(), Value: 0.5, Passed: true}, nil
}
func (outOfDomainGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (outOfDomainGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// promptedGoal has prompt text and a provider. It is the shape a real judge
// will have, and it is what the replay purity and prompt-sha tests need.
//
// Its provider calls t.Fatal. A replay that reaches it fails the test rather
// than quietly producing a number from a live call.
type promptedGoal struct {
	t      *testing.T
	prompt string
	truth  map[string]bool
	cost   int64
	calls  *int
}

func (g *promptedGoal) Score(_ context.Context, c *core.Case, _ *core.Response) (*core.Score, error) {
	if g.calls != nil {
		*g.calls++
	}
	if g.t != nil {
		g.t.Helper()
		g.t.Fatal("the judge called its provider during a replay. " +
			"PR CI must never contact a provider, and a replay that can is not a replay.")
	}
	v := g.truth[c.GetId()]
	return &knov1.Score{
		CaseId: c.GetId(), Value: boolValue(v), Passed: v, JudgeModel: "test-judge-1",
	}, nil
}
func (*promptedGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (*promptedGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }
func (g *promptedGoal) PromptSources() []judge.PromptSource {
	return []judge.PromptSource{{Name: "verdict.prompt", Text: []byte(g.prompt)}}
}

func (g *promptedGoal) EstimateScore(context.Context, *core.Case) (budget.Estimate, error) {
	return budget.Estimate{Calls: 1, CostUSDMicros: g.cost, Tokens: 100}, nil
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// truthOf reads the adjudicated verdicts out of a set, keyed by record id.
func truthOf(set *judge.Set) map[string]bool {
	out := map[string]bool{}
	for _, r := range set.Records {
		out[r.ID] = r.Adjudicated.Passed
	}
	return out
}

// everyNth selects record ids at a fixed stride, so an error rate is exact
// rather than sampled.
func everyNth(set *judge.Set, stride int) map[string]bool {
	out := map[string]bool{}
	for i, r := range set.Records {
		if i%stride == 0 {
			out[r.ID] = true
		}
	}
	return out
}
