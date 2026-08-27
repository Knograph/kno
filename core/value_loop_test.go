package core_test

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// loopCases yields Cases from a slice, for the Value loop tests.
type loopCases struct {
	cases []*core.Case
}

func (l *loopCases) Cases(_ context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		for _, c := range l.cases {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// loopPool supplies a fixed list of Assets.
type loopPool struct{ assets []*core.Asset }

func (p loopPool) Assets(_ context.Context) (iter.Seq2[*core.Asset, error], error) {
	return func(yield func(*core.Asset, error) bool) {
		for _, a := range p.assets {
			if !yield(a, nil) {
				return
			}
		}
	}, nil
}

// valueHarness wires everything a Value run needs around a fake agent that
// CAN inject, so the measurement path is the real one.
type valueHarness struct {
	store *store.SQLite
	guard *budget.Guard
	agent *fake.Agent
	opts  core.ValueOptions
}

func newValueHarness(t *testing.T, opts fake.Options) *valueHarness {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "kno.db")
	st, err := store.NewSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	opts.Inject = true
	agent := fake.New(opts)
	h := &valueHarness{
		store: st,
		guard: budget.New(budget.Limits{}, nil, 0),
		agent: agent,
	}
	h.opts = core.ValueOptions{
		RunID:            "run-1",
		BaselineRunID:    "base-1",
		Agent:            agent,
		AgentRef:         &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:             &exactmatch.Goal{},
		GoalName:         "exact-match",
		Guard:            h.guard,
		Store:            st,
		Concurrency:      4,
		InputFingerprint: "fp-1",
		Routing:          value.Options{Seed: 1},
	}
	return h
}

// valueCases builds a dev split of n Cases and returns it sealed, plus the
// underlying source for tests that plant a holdout Case in it.
func valueCases(t *testing.T, h *valueHarness, devCount int) *loopCases {
	t.Helper()
	var cases []*core.Case
	for i := range devCount {
		cases = append(cases, &core.Case{
			Id:       loopCaseID(i),
			Input:    "q",
			Expected: "a",
			Split:    knov1.Split_SPLIT_DEV,
		})
	}
	src := &loopCases{cases: cases}
	h.opts.Evals = core.Seal(src)
	return src
}

func loopCaseID(i int) string { return "dev-" + string(rune('a'+i%26)) + string(rune('0'+i/26)) }

// writeBaselineFor records a clean baseline over the same Cases so the Value
// run has something to pair against. Every Case passes.
func writeBaselineFor(t *testing.T, h *valueHarness, cases []*core.Case) {
	t.Helper()
	createBaselineRun(t, h.store, "base-1", []string{"fake-model"})
	for _, c := range cases {
		writeBaselineOutcome(t, h.store, "base-1", c.GetId(), 1.0)
	}
}

// createBaselineRun writes a finished Baseline run with the given resolved
// models and no incomplete reason.
func createBaselineRun(t *testing.T, st store.Store, runID string, models []string) {
	t.Helper()
	run := &knov1.Run{
		Id:            runID,
		Stage:         knov1.Stage_STAGE_BASELINE,
		GoalName:      "exact-match",
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		CaseExecution: &knov1.CaseExecution{ResolvedModels: models},
	}
	if err := st.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("creating baseline run: %v", err)
	}
}

// writeBaselineOutcome records one scored outcome on a baseline run.
func writeBaselineOutcome(t *testing.T, st store.Store, runID, caseID string, score float64) {
	t.Helper()
	if err := st.RecordOutcome(context.Background(), runID, &store.Outcome{
		CaseID: caseID,
		Score:  &knov1.Score{CaseId: caseID, Value: score, Passed: true},
	}); err != nil {
		t.Fatalf("recording baseline outcome: %v", err)
	}
}

// writeBaselineWithFailures records a baseline where every failEvery-th Case
// failed, so routing has clusters to work with.
func writeBaselineWithFailures(t *testing.T, h *valueHarness, cases []*core.Case, failEvery int) {
	t.Helper()
	createBaselineRun(t, h.store, "base-1", []string{"fake-model"})
	for i, c := range cases {
		score := 1.0
		if failEvery > 0 && i%failEvery == 0 {
			score = 0.0
		}
		if err := h.store.RecordOutcome(context.Background(), "base-1", &store.Outcome{
			CaseID: c.GetId(),
			Score:  &knov1.Score{CaseId: c.GetId(), Value: score, Passed: score == 1.0},
		}); err != nil {
			t.Fatalf("recording baseline outcome: %v", err)
		}
	}
}

// poolOf is a Pool holding the named Assets.
func poolOf(ids ...string) core.Pool {
	assets := make([]*core.Asset, len(ids))
	for i, id := range ids {
		assets[i] = &core.Asset{Id: id}
	}
	return loopPool{assets: assets}
}

// TestValueRunsEndToEnd is the stage's headline: an Asset measured over its
// routed slice, the treatment arm carrying it and the control arm not, one
// Valuation per Asset, and the Run record closed with its plan.
func TestValueRunsEndToEnd(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	result, err := h.opts.Value(context.Background(), poolOf("a1", "a2"))
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if len(result.Valuations) != 2 {
		t.Fatalf("got %d Valuations, want 2", len(result.Valuations))
	}
	if result.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Errorf("Status = %v, want COMPLETED", result.Status)
	}

	// The treatment arm carried each Asset; the control arm carried nothing.
	for _, id := range []string{"a1", "a2"} {
		if got := h.agent.Injected(id); got == 0 {
			t.Errorf("the treatment arm never carried %s", id)
		}
	}

	// The run row closed with the plan and trials recorded — what a resume
	// fingerprints against.
	run, err := h.store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(run.GetValuePlan()) == 0 {
		t.Error("the closed run records no value_plan; a resume would have " +
			"nothing to compare its routing against")
	}
	if run.GetTrials() != 1 {
		t.Errorf("the closed run records trials %d, want 1", run.GetTrials())
	}

	// Every Valuation's measurements are durable and pairable: each recorded
	// a delta with its interval, or a named reason.
	for _, v := range result.Valuations {
		if v.GetNotMeasured() != knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED {
			t.Errorf("Valuation for %s: not_measured = %v on a healthy run",
				v.GetAssetId(), v.GetNotMeasured())
		}
		if v.GetDeltaInterval() == nil {
			t.Errorf("Valuation for %s: no delta interval", v.GetAssetId())
		}
		if v.GetNDropped() != 0 {
			t.Errorf("Valuation for %s: dropped %d pairs on a run with no failures",
				v.GetAssetId(), v.GetNDropped())
		}
	}
}

// TestValueOrderDoesNotChangeDeltas is the Q12 resolution pinned: the delta
// path reads only (run, asset) measurements, so two runs over the same pool
// in different orders must produce identical per-Asset deltas.
func TestValueOrderDoesNotChangeDeltas(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, order ...string) map[string]float64 {
		t.Helper()
		h := newValueHarness(t, fake.Options{})
		h.opts.RunID = "run-" + order[0]
		src := valueCases(t, h, 30)
		writeBaselineFor(t, h, src.cases)
		result, err := h.opts.Value(context.Background(), poolOf(order...))
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		out := make(map[string]float64, len(result.Valuations))
		for _, v := range result.Valuations {
			out[v.GetAssetId()] = v.GetDeltaGoal()
		}
		return out
	}

	first := run(t, "a1", "a2", "a3")
	second := run(t, "a3", "a1", "a2")
	for id, delta := range first {
		if second[id] != delta {
			t.Errorf("delta for %s differs across Asset orders: %v vs %v; the "+
				"delta path must read only recorded measurements, never execution order",
				id, delta, second[id])
		}
	}
}

// TestValueNeverTouchesTheHoldout is the plan's canary, made falsifiable: a
// holdout Case planted in the source reaches neither the routing, the
// measurements, nor the events.
func TestValueNeverTouchesTheHoldout(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	src := valueCases(t, h, 30)
	hold := &core.Case{Id: "holdout-00", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_HOLDOUT}
	src.cases = append(src.cases, hold)
	writeBaselineFor(t, h, src.cases)

	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("Value: %v", err)
	}

	recorded, err := h.store.Measurements(context.Background(), "run-1", "a1")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	for _, m := range recorded {
		if m.Key.CaseID == "holdout-00" {
			t.Fatal("a holdout Case ID landed in the measurements table")
		}
	}
}

// TestZeroRoutedAssetCostsNothing pins P1-4 end to end: an Asset matching no
// cluster is never put in front of the agent, pays zero calls, and its
// Valuation carries IRRELEVANT.
func TestZeroRoutedAssetCostsNothing(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	// Every third Case fails, tagged "billing" — so routing has a cluster to
	// match against, and an Asset tagged for another domain routes to nothing.
	cases := make([]*core.Case, 20)
	for i := range cases {
		cases[i] = &core.Case{
			Id: loopCaseID(i), Input: "q", Expected: "a",
			Split: knov1.Split_SPLIT_DEV,
			Tags:  []string{"billing"},
		}
	}
	src := &loopCases{cases: cases}
	h.opts.Evals = core.Seal(src)
	writeBaselineWithFailures(t, h, src.cases, 3)

	before := h.agent.Calls()
	// The Asset is tagged for a domain no Case belongs to, so it matches no
	// cluster and routes to nothing.
	tagged := &core.Asset{Id: "lonely", Tags: []string{"astronomy"}}
	result, err := h.opts.Value(context.Background(), loopPool{assets: []*core.Asset{tagged}})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got := h.agent.Calls() - before; got != 0 {
		t.Errorf("a zero-routed Asset cost %d provider calls; it was never put "+
			"in front of the agent, so the price of measuring it is zero", got)
	}
	if len(result.Valuations) != 1 ||
		result.Valuations[0].GetNotMeasured() != knov1.RejectionReason_REJECTION_REASON_IRRELEVANT {
		t.Errorf("Valuations = %+v, want one with IRRELEVANT", result.Valuations)
	}
	if got := h.agent.Injected("lonely"); got != 0 {
		t.Errorf("a zero-routed Asset was injected %d times; it was never put "+
			"in front of the agent", got)
	}
}

// TestBudgetStopMarksTheTruncatedPortfolio pins Q12's truncation marker: when
// the cap binds mid-Asset, the truncated Asset lands BUDGET_EXHAUSTED and the
// run ends BUDGET_STOPPED — never a partial portfolio presented as complete.
func TestBudgetStopMarksTheTruncatedPortfolio(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{CostPerCallUSDMicros: 1000})
	// A cap that admits a few measurements, not the run.
	h.guard = budget.New(budget.Limits{MaxCostUSDMicros: 3000, MaxLLMCalls: 1000}, nil, 0)
	h.opts.Guard = h.guard

	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	result, err := h.opts.Value(context.Background(), poolOf("a1", "a2"))
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if result.Status != knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		t.Errorf("Status = %v, want BUDGET_STOPPED", result.Status)
	}
	for _, v := range result.Valuations {
		if v.GetNotMeasured() == knov1.RejectionReason_REJECTION_REASON_BUDGET_EXHAUSTED {
			continue
		}
		if v.GetDeltaInterval() != nil {
			continue
		}
		t.Errorf("Valuation for %s carries neither a measurement nor a reason", v.GetAssetId())
	}
}

// TestResumeDoesNotRePay pins P1-10: a run stopped by its cap resumes, skips
// the measurements already durable — done-markers included — and the settled
// spend across both processes equals what the store recorded once.
func TestResumeDoesNotRePay(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{CostPerCallUSDMicros: 1000})
	h.guard = budget.New(budget.Limits{MaxCostUSDMicros: 2000, MaxLLMCalls: 1000}, nil, 0)
	h.opts.Guard = h.guard

	src := valueCases(t, h, 40)
	writeBaselineFor(t, h, src.cases)

	first, err := h.opts.Value(context.Background(), poolOf("a1"))
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if first.Status != knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		t.Fatalf("first process Status = %v, want BUDGET_STOPPED (fixture)", first.Status)
	}
	// Resume: same fingerprint, bigger cap, same routing configuration.
	h.guard = budget.New(budget.Limits{MaxCostUSDMicros: 1 << 30, MaxLLMCalls: 1 << 30}, nil, 0)
	h.opts.Guard = h.guard
	h.opts.Resume = true
	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("resumed Value: %v", err)
	}

	// The amendment's end-to-end assertions, shaped by the fixture: a fresh
	// control arm doubles the routed slice, so the schedule is
	// routed x arms + control.
	arms := int64(1)
	if first.Plan.Routed[0].FreshControlArm {
		arms = 2
	}
	unique := int64(len(first.Plan.Routed[0].CaseIDs))*arms + int64(len(first.Plan.ControlCaseIDs))
	totalCalls := h.agent.Calls()

	// 1. No measurement is paid twice: total calls across both processes stay
	// at or below the single-process schedule. A re-paid measurement would
	// push the total above it.
	if totalCalls > unique {
		t.Errorf("total calls across both processes = %d, above the scheduled %d; "+
			"a measurement was paid for twice", totalCalls, unique)
	}

	// 2. Every scheduled key is durable — scored or done-marked — so a third
	// resume would pay nothing at all.
	rows, err := h.store.Measurements(context.Background(), "run-1", "a1")
	if err != nil {
		t.Fatalf("Measurements: %v", err)
	}
	if int64(len(rows)) != unique {
		t.Errorf("durable rows = %d, want the scheduled %d", len(rows), unique)
	}

	// 3. The store's settled spend matches what the provider was charged for:
	// the first process's settled calls plus the resumed process's, with the
	// budget-refused measurements costing no calls at all.
	spent, err := h.store.SettledSpend(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if spent.Calls != totalCalls {
		t.Errorf("SettledSpend records %d calls against %d provider calls",
			spent.Calls, totalCalls)
	}
}

// TestResumeRefusesADriftedPlan pins P1-10's refusal: resuming with a
// different routing configuration must be refused, because continuing would
// pair new measurements against rows recorded under a different plan.
func TestResumeRefusesADriftedPlan(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("Value: %v", err)
	}

	h.opts.Resume = true
	h.opts.Routing.Seed = 99
	_, err := h.opts.Value(context.Background(), poolOf("a1"))
	if err == nil {
		t.Fatal("a resume with a drifted seed was accepted; the recorded plan " +
			"is the consent, and continuing would pair against rows from a different one")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

// captureStore records every event appended, so the money events can be
// asserted against the real run.
type captureStore struct {
	store.Store
	events []*knov1.Event
}

func (c *captureStore) AppendEvent(_ context.Context, ev *knov1.Event) error {
	c.events = append(c.events, ev)
	return c.Store.AppendEvent(context.Background(), ev)
}

// TestValueMoneyEventsCarryTheMeasurementKey drives a retry and an overshoot
// through the real run and asserts both events carry the (Asset, arm, trial)
// key — the P1-8 contract, without which the API cannot attribute spend to
// the Asset that caused it.
func TestValueMoneyEventsCarryTheMeasurementKey(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{
		CostPerCallUSDMicros: 1000,
		ThrottleFirstAttempt: true,
	})
	captured := &captureStore{Store: h.store}
	h.opts.Store = captured
	// A cap that binds while four measurements are in flight, so at least one
	// settlement overshoots its reservation.
	h.guard = budget.New(budget.Limits{MaxCostUSDMicros: 2000, MaxLLMCalls: 1000}, nil, 0)
	h.opts.Guard = h.guard

	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("Value: %v", err)
	}

	var retries, overshoots int
	for _, ev := range captured.events {
		switch p := ev.GetPayload().(type) {
		case *knov1.Event_RetryAttempted:
			retries++
			r := p.RetryAttempted
			if r.GetAssetId() != "a1" ||
				r.GetArm() == knov1.Arm_ARM_UNSPECIFIED || r.GetTrial() < 1 {
				t.Errorf("RetryAttempted carries %q/%v/%d, want the (Asset, arm, trial) key",
					r.GetAssetId(), r.GetArm(), r.GetTrial())
			}
		case *knov1.Event_SettlementOvershoot:
			overshoots++
			r := p.SettlementOvershoot
			if r.GetAssetId() != "a1" {
				t.Errorf("SettlementOvershoot carries asset %q, want a1", r.GetAssetId())
			}
		}
	}
	if retries == 0 {
		t.Error("the throttled fixture produced no RetryAttempted events")
	}
	if overshoots == 0 {
		t.Error("the binding cap produced no SettlementOvershoot events")
	}
}

// TestValueResumeRefusesFingerprintMismatch: the fingerprint pins the inputs,
// and a resume under different inputs must refuse before any spend.
func TestValueResumeRefusesFingerprintMismatch(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)
	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("Value: %v", err)
	}

	h.opts.Resume = true
	h.opts.InputFingerprint = "fp-changed"
	before := h.agent.Calls()
	_, err := h.opts.Value(context.Background(), poolOf("a1"))
	if err == nil {
		t.Fatal("a resume with a changed fingerprint was accepted")
	}
	if got := h.agent.Calls() - before; got != 0 {
		t.Errorf("the refused resume made %d provider calls", got)
	}
}

// TestValueResumeRefusesACorruptPlan: a recorded plan that cannot be decoded
// is refused, not guessed at.
func TestValueResumeRefusesACorruptPlan(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)
	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err != nil {
		t.Fatalf("Value: %v", err)
	}

	run, err := h.store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	run.ValuePlan = []byte{0xde, 0xad}
	if err := h.store.FinishRun(context.Background(), run); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	h.opts.Resume = true
	if _, err := h.opts.Value(context.Background(), poolOf("a1")); err == nil ||
		!strings.Contains(err.Error(), "cannot be decoded") {
		t.Errorf("err = %v, want the corrupt-plan refusal", err)
	}
}

// lyingInjector builds a treatment wrapper whose Capabilities declare
// context_inject false — the adapter that answers anyway — so the P2-11
// wrapper check can be driven through a real run.
type lyingInjector struct{ *fake.Agent }

func (l *lyingInjector) WithContext(*core.Asset) (core.Agent, error) {
	return &lyingWrapper{inner: l.Agent}, nil
}

type lyingWrapper struct{ inner core.Agent }

func (l *lyingWrapper) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	return l.inner.Invoke(ctx, c)
}

func (*lyingWrapper) Capabilities() *core.Capabilities { return &knov1.Capabilities{} }

// TestValueRefusesAWrapperThatLiesAboutInjection: the capability check runs
// on the WRAPPER — the agent that actually runs — not the receiver. The
// receiver passes validate; the wrapper must not.
func TestValueRefusesAWrapperThatLiesAboutInjection(t *testing.T) {
	t.Parallel()

	h := newValueHarness(t, fake.Options{})
	h.opts.Agent = &lyingInjector{Agent: h.agent}
	src := valueCases(t, h, 30)
	writeBaselineFor(t, h, src.cases)

	before := h.agent.Calls()
	_, err := h.opts.Value(context.Background(), poolOf("a1"))
	if err == nil || !strings.Contains(err.Error(), "context_inject false") {
		t.Fatalf("err = %v, want the wrapper capability refusal", err)
	}
	if got := h.agent.Calls() - before; got != 0 {
		t.Errorf("the refusal came after %d provider calls; it must be free", got)
	}
}
