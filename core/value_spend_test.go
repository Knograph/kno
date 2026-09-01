package core

import (
	"context"
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// pricedAgent bills a fixed amount per call, so a test can pin the reported
// figure against arithmetic rather than against whatever the fixture happened
// to do.
type pricedAgent struct{ micros int64 }

func (p pricedAgent) Invoke(context.Context, *Case) (*Response, error) {
	return &Response{CostUsdMicros: p.micros, PromptTokens: 3, CompletionTokens: 2}, nil
}

// WithContext keeps the pricing on the treatment arm: returning a stubAgent
// here would make the two arms cost different amounts, which is a different
// test than the one this file is running.
func (p pricedAgent) WithContext(*Asset) (Agent, error) { return pricedAgent{micros: p.micros}, nil }

// spendFixture sets up a baseline with one scored Case and returns the
// ValueOptions a spend test runs.
func spendFixture(t *testing.T, st store.Store, runID string, micros int64) (ValueOptions, Pool) {
	t.Helper()
	ctx := context.Background()

	run := &knov1.Run{
		Id: "base-spend", Stage: knov1.Stage_STAGE_BASELINE, GoalName: "g",
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		CaseExecution: &knov1.CaseExecution{ResolvedModels: []string{"m"}},
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, id := range []string{"c1", "c2"} {
		if err := st.RecordOutcome(ctx, "base-spend", &store.Outcome{
			CaseID: id, Score: &knov1.Score{CaseId: id, Value: 0.9, Passed: true},
		}); err != nil {
			t.Fatalf("RecordOutcome: %v", err)
		}
	}
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: runID, BaselineRunID: "base-spend",
		Agent:    pricedAgent{micros: micros},
		AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal: fixedDirectionGoal{
			dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
		},
		GoalName:    "g",
		Guard:       budget.New(budget.Limits{}, nil, 0),
		Store:       st,
		Evals:       Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	return opts, stubPool{assets: []*Asset{{Id: "a1"}}}
}

// TestValueResultSpendEqualsSettledSpend is the plan's AC1 and the reason
// ValueResult.Spent exists at all.
//
// The two readers of money must agree to the micro-dollar: Guard.Spent is
// what a stage that ran the guard reports, Store.SettledSpend is what a
// surface reporting on somebody else's run reads, and a disagreement between
// them is the failure docs/debt.md#50 already cost this project once.
func TestValueResultSpendEqualsSettledSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openTestStore(t)
	opts, pool := spendFixture(t, st, "run-spend", 1_000)

	res, err := opts.Value(ctx, pool)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if res.Spent.Calls == 0 {
		t.Fatal("the run reports zero calls; the fixture is not exercising the meter")
	}
	if res.Spent.CostUSDMicros != res.Spent.Calls*1_000 {
		t.Errorf("spend %d over %d calls at 1000 micros each does not multiply out",
			res.Spent.CostUSDMicros, res.Spent.Calls)
	}
	settled, err := st.SettledSpend(ctx, "run-spend")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	assertMoneyAgrees(t, res.Spent, settled)
}

// assertMoneyAgrees pins the two readers of money to each other on all three
// dimensions a cap can be set on.
//
// Tokens were exempted here once, and the exemption carried an instruction:
// the Value sink recorded budget.Spend{Calls, CostUSDMicros} and dropped the
// token count, so Guard.Spent and Store.SettledSpend genuinely disagreed and
// a resumed Value run restored zero tokens. docs/debt.md#137 tracked it and
// the allowance said to delete itself once the sink recorded tokens. It does
// now, so this compares all three — a stale allowance is a test that has
// stopped asking its question.
func assertMoneyAgrees(t *testing.T, guard, settled budget.Spend) {
	t.Helper()
	if guard.Calls != settled.Calls || guard.CostUSDMicros != settled.CostUSDMicros ||
		guard.Tokens != settled.Tokens {
		t.Errorf("the guard settled %d call(s) / %d micros / %d tokens, the store "+
			"recorded %d / %d / %d. The two readers of money must agree on every "+
			"dimension a cap can be set on: SettledSpend is what a resume restores "+
			"the guard from, so a dimension the store drops is a cap the resume "+
			"stops enforcing",
			guard.Calls, guard.CostUSDMicros, guard.Tokens,
			settled.Calls, settled.CostUSDMicros, settled.Tokens)
	}
}

// TestValueReportsSpendAfterANonBudgetFailure is the plan's AC21.
//
// The failure that loses money most confusingly is not a budget stop and not
// a run that never spent: it is real charges settled, followed by a failure
// that has nothing to do with money. Returning only an error there throws the
// figure away while the money stays gone.
func TestValueReportsSpendAfterANonBudgetFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := openTestStore(t)
	failing := &failingValueStore{Store: inner, failValuation: true}
	opts, pool := spendFixture(t, failing, "run-fail", 2_000)

	res, err := opts.Value(ctx, pool)
	if err == nil {
		t.Fatal("Value over a store that cannot write a valuation returned no error")
	}
	if res == nil {
		t.Fatal("Value returned no result alongside the error, so the caller cannot " +
			"report what the failed run spent")
	}
	if res.Spent.CostUSDMicros == 0 || res.Spent.Calls == 0 {
		t.Fatalf("the failed run reports %+v; the measurements before the failure "+
			"were paid for", res.Spent)
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED — an unreported status renders as "+
			"UNSPECIFIED, which reads as a bug rather than as a failure", res.Status)
	}
	settled, err := inner.SettledSpend(ctx, "run-fail")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	assertMoneyAgrees(t, res.Spent, settled)
}

// TestResumedValueReportsLifetimeSpend is the plan's AC14 and §6's decision:
// the run, not the session, is the unit the user authorized, so the run is
// what gets reported.
//
// A second process over the same run resumes from the durable record.
// Guard.Restore is additive and openRun seeds it before anything is
// authorized, so the resumed run's figure covers both processes — and nothing
// is paid for twice, which the settled total proves.
func TestResumedValueReportsLifetimeSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := openTestStore(t)
	first, pool := spendFixture(t, st, "run-resume", 5_000)
	firstRes, err := first.Value(ctx, pool)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if firstRes.Spent.Calls == 0 {
		t.Fatal("the first pass settled nothing")
	}

	// A fresh guard, exactly as a second process would have: in-memory state
	// does not survive, and the store is the only thing that does.
	second, _ := spendFixture2(t, st, "run-resume", 5_000)
	second.Resume = true
	secondRes, err := second.Value(ctx, pool)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	settled, err := st.SettledSpend(ctx, "run-resume")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	assertMoneyAgrees(t, secondRes.Spent, settled)
	if secondRes.Spent.Calls < firstRes.Spent.Calls {
		t.Errorf("the resumed run reports %d calls, fewer than the first pass's %d — "+
			"a session figure, not the run's", secondRes.Spent.Calls, firstRes.Spent.Calls)
	}
	// Every measurement was already recorded, so the resume pays for nothing
	// and the lifetime figure is exactly the first pass's.
	if secondRes.Spent.CostUSDMicros != firstRes.Spent.CostUSDMicros ||
		secondRes.Spent.Calls != firstRes.Spent.Calls {
		t.Errorf("resumed spend %+v differs from the first pass's %+v; nothing was "+
			"left to measure, so nothing should have been paid for twice",
			secondRes.Spent, firstRes.Spent)
	}
}

// spendFixture2 builds a second process's options over an existing baseline,
// with a fresh guard and no baseline re-creation.
func spendFixture2(t *testing.T, st store.Store, runID string, micros int64) (ValueOptions, Pool) {
	t.Helper()
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: runID, BaselineRunID: "base-spend",
		Agent:    pricedAgent{micros: micros},
		AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal: fixedDirectionGoal{
			dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
		},
		GoalName:    "g",
		Guard:       budget.New(budget.Limits{}, nil, 0),
		Store:       st,
		Evals:       Seal(cases),
		Concurrency: 2,
		Routing:     value.Options{Seed: 1},
	}
	return opts, stubPool{assets: []*Asset{{Id: "a1"}}}
}
