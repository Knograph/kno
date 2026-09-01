package core_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// validateHarness wires everything a Validate run needs around a fake agent
// that CAN carry a set, so the measurement path is the real one.
type validateHarness struct {
	store *store.SQLite
	guard *budget.Guard
	agent *fake.Agent
	opts  core.ValidateOptions

	holdout []*core.Case
	pool    loopPool
}

// validateFixture describes the pipeline a Validate run is measured against.
type validateFixture struct {
	devCases     int
	holdoutCases int
	assets       int

	// answer overrides the fake's answer, so a test can script a treatment arm
	// that beats the control by a known amount.
	answer func(injected bool, c *core.Case) string

	// destinations overrides the Portfolio entries' destinations, for the
	// mixed-Portfolio refusal.
	destinations []knov1.Destination

	// noContentHash writes the Portfolio without content hashes, as a Select
	// run with no Pool would.
	noContentHash bool
}

// newValidateHarness builds the store, the recorded pipeline, the Pool, the
// evals and the options.
func newValidateHarness(t *testing.T, fx validateFixture) *validateHarness {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "kno.db")
	st, err := store.NewSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	agent := newArmAwareAgent(fx.answer)

	var assets []*core.Asset
	for i := range fx.assets {
		assets = append(assets, &core.Asset{
			Id:      fmt.Sprintf("asset-%d", i),
			Content: []byte(fmt.Sprintf("asset %d content", i)),
			Cost:    &knov1.CostVector{ContextTokens: 10},
		})
	}

	var cases []*core.Case
	for i := range fx.devCases {
		cases = append(cases, &core.Case{
			Id: fmt.Sprintf("dev-%03d", i), Input: "q", Expected: "a",
			Split: knov1.Split_SPLIT_DEV,
		})
	}
	var holdout []*core.Case
	for i := range fx.holdoutCases {
		c := &core.Case{
			Id: fmt.Sprintf("hold-%03d", i), Input: "q", Expected: "a",
			Split: knov1.Split_SPLIT_HOLDOUT,
		}
		cases = append(cases, c)
		holdout = append(holdout, c)
	}

	h := &validateHarness{
		store:   st,
		guard:   budget.New(budget.Limits{}, nil, 0),
		agent:   agent,
		holdout: holdout,
		pool:    loopPool{assets: assets},
	}
	writeValidatePipeline(t, st, assets, fx)

	h.opts = core.ValidateOptions{
		RunID:            "validate-1",
		SelectRunID:      "select-1",
		Agent:            agent,
		AgentRef:         &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:             &exactmatch.Goal{},
		GoalName:         "exact-match",
		Guard:            h.guard,
		Store:            st,
		Evals:            &loopCases{cases: cases},
		Pool:             h.pool,
		Concurrency:      4,
		MinHoldout:       20,
		EvalFingerprint:  "holdout-fp-1",
		InputFingerprint: "fp-1",
	}
	return h
}

// writeValidatePipeline records the Baseline -> Value -> Select chain a
// Validate run walks before it spends anything.
func writeValidatePipeline(t *testing.T, st store.Store, assets []*core.Asset, fx validateFixture) {
	t.Helper()
	ctx := context.Background()

	for _, run := range []*knov1.Run{
		{Id: "base-1", Stage: knov1.Stage_STAGE_BASELINE, Status: knov1.RunStatus_RUN_STATUS_COMPLETED},
		{
			Id: "value-1", Stage: knov1.Stage_STAGE_VALUE, BaselineRunId: "base-1",
			Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
		},
		{Id: "select-1", Stage: knov1.Stage_STAGE_SELECT, Status: knov1.RunStatus_RUN_STATUS_COMPLETED},
	} {
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatalf("creating %s: %v", run.GetId(), err)
		}
	}

	p := &knov1.Portfolio{
		RunId:                "select-1",
		SourceRunId:          "value-1",
		SourceStatus:         knov1.RunStatus_RUN_STATUS_COMPLETED,
		DevEstimatedGain:     0.20,
		DevEstimatedInterval: &knov1.Interval{Low: 0.10, High: 0.30, Level: 0.95},
		Budget:               &knov1.Budget{MaxContextTokens: 100_000},
	}
	for i, a := range assets {
		dest := knov1.Destination_DESTINATION_CONTEXT
		if i < len(fx.destinations) {
			dest = fx.destinations[i]
		}
		e := &knov1.PortfolioEntry{
			AssetId:     a.GetId(),
			Destination: dest,
			Rank:        int32(i) + 1,
		}
		if !fx.noContentHash {
			sum := sha256.Sum256(a.GetContent())
			e.ContentHash = sum[:]
		}
		p.Selected = append(p.Selected, e)
	}
	if err := st.WritePortfolio(ctx, "select-1", p); err != nil {
		t.Fatalf("writing the portfolio: %v", err)
	}
}

// newArmAwareAgent builds the real fake adapter, so the injection bookkeeping,
// the prefix recording and the capability declaration exercised by these tests
// are the shipped ones rather than a double.
//
// A scripted answer rides on the CONTROL arm only; tests that need a known
// difference between the arms use scriptedSetAgent below, because the fake has
// one Answer hook and no notion of an arm.
func newArmAwareAgent(answer func(injected bool, c *core.Case) string) *fake.Agent {
	opts := fake.Options{Inject: true}
	if answer != nil {
		// The fake has one Answer hook and no notion of an arm, so the arm is
		// carried on the Case: the injected wrapper is a different *Agent
		// instance, and the harness scripts the difference through the
		// treatment agent below.
		opts.Answer = func(c *core.Case) string { return answer(false, c) }
	}
	return fake.New(opts)
}

// scriptedSetAgent is an Agent whose treatment arm scores a fixed amount
// better than its control arm, so a holdout gain of a KNOWN size can be
// asserted to float equality.
type scriptedSetAgent struct {
	injected bool

	// correctShareControl is the share of Cases the control arm answers
	// correctly, keyed on the Case's ordinal so the split is deterministic.
	controlRight   func(caseID string) bool
	treatmentRight func(caseID string) bool

	// calls counts every Invoke. Atomic because the executor runs several
	// workers per arm, and a plain counter here is a data race the -race gate
	// catches before it catches anything about the engine.
	calls *atomic.Int64
}

func (s *scriptedSetAgent) Invoke(_ context.Context, c *core.Case) (*core.Response, error) {
	s.calls.Add(1)
	right := s.controlRight
	if s.injected {
		right = s.treatmentRight
	}
	out := "wrong"
	if right(c.GetId()) {
		out = c.GetExpected()
	}
	// Token counts are emitted because Capabilities below declares
	// TokenCounts, and an agent that claims the capability and reports zero
	// makes every token assertion in this file vacuously true. Fixed values:
	// the counts are asserted against a total, not a model's behaviour.
	return &core.Response{
		CaseId:           c.GetId(),
		Output:           out,
		ResolvedModel:    "scripted",
		PromptTokens:     7,
		CompletionTokens: 3,
	}, nil
}

func (s *scriptedSetAgent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{ContextInject: true, ContextSetInject: true, TokenCounts: true}
}

func (s *scriptedSetAgent) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	if len(assets) == 0 {
		return nil, errs.ErrInvalidInput.Wrap(errors.New("scripted: no Portfolio to inject"))
	}
	injected := *s
	injected.injected = true
	return &injected, nil
}

// refusingAgent fails the test on Invoke. Every "no agent call is made"
// assertion is driven through it rather than through a call counter checked
// afterwards, so the failure names the refusal that did not happen.
type refusingAgent struct{ t *testing.T }

func (r refusingAgent) Invoke(context.Context, *core.Case) (*core.Response, error) {
	r.t.Error("the agent was invoked; this refusal is supposed to be free, before any spend")
	return nil, errors.New("refusingAgent must not be invoked")
}

func (refusingAgent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{ContextInject: true, ContextSetInject: true}
}

func (r refusingAgent) WithContextSet([]*core.Asset) (core.Agent, error) { return r, nil }

// noSetAgent injects one Asset but not a set, which is what every v0.1
// adapter looked like.
type noSetAgent struct{}

func (noSetAgent) Invoke(context.Context, *core.Case) (*core.Response, error) {
	return &core.Response{}, nil
}

// declinesSetAgent implements the interface and declares the capability
// false, which is the shape that would otherwise measure both arms without
// the Portfolio and report the noise as a verdict.
type declinesSetAgent struct{ noSetAgent }

func (d declinesSetAgent) WithContextSet([]*core.Asset) (core.Agent, error) { return d, nil }

func (declinesSetAgent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{ContextInject: true, ContextSetInject: false}
}

// ---------------------------------------------------------------------------

// TestQuoteCountsBothArms is the consent quote's arithmetic.
//
// A quote showing n x trials would understate the run by exactly the arm
// count, which is the failure core/ring0.go records having already happened
// once at a different multiple. The derivation string is asserted too: prime
// directive 4 is about disclosure, and a total without its shape tells the
// user the number but not the run.
func TestQuoteCountsBothArms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ holdout, trials int }{
		{20, 1}, {20, 3}, {1, 1}, {80, 3}, {19, 2},
	} {
		t.Run(fmt.Sprintf("holdout=%d trials=%d", tc.holdout, tc.trials), func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 2, holdoutCases: tc.holdout, assets: 2})
			h.opts.Trials = int32(tc.trials)
			q, err := h.opts.Quote(context.Background())
			if err != nil {
				t.Fatalf("Quote: %v", err)
			}
			want := int64(tc.holdout) * 2 * int64(tc.trials)
			if q.Calls != want {
				t.Errorf("quoted %d calls, want %d (%d holdout x 2 arms x %d trials)",
					q.Calls, want, tc.holdout, tc.trials)
			}
			if q.Arms != 2 {
				t.Errorf("quoted %d arms, want 2", q.Arms)
			}
			wantStr := fmt.Sprintf("%d calls (%d holdout Cases x 2 arms x %d trial(s))",
				want, tc.holdout, tc.trials)
			if q.Derivation() != wantStr {
				t.Errorf("derivation = %q, want %q", q.Derivation(), wantStr)
			}
		})
	}
}

// TestValidateMeasuresBothArmsOverTheHoldout is the happy path, and it pins
// the gain to float equality.
//
// The scripted agent gets exactly two more holdout Cases right in the
// treatment arm than in the control arm out of twenty, so the paired mean is
// exactly +0.1. The interval must be non-zero-width and finite, which are the
// two invariants stats/interval's build enforces — a zero-width interval reads
// as certainty and a NaN bound renders as blank.
func TestValidateMeasuresBothArmsOverTheHoldout(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 20, assets: 2})
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls:          &calls,
		controlRight:   func(id string) bool { return id >= "hold-002" },
		treatmentRight: func(string) bool { return true },
	}

	res, err := h.opts.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Fatalf("status = %v, want COMPLETED", res.Status)
	}
	if got := calls.Load(); got != 40 {
		t.Errorf("made %d agent calls, want 40 (20 holdout x 2 arms x 1 trial)", got)
	}
	v := res.Validation
	if v == nil {
		t.Fatal("no Validation was written")
	}
	if v.GetHoldoutInterval() == nil {
		t.Fatal("a gain was reported without its interval, or no gain at all")
	}
	if got := v.GetHoldoutGain(); math.Abs(got-0.1) > 1e-12 {
		t.Errorf("holdout gain = %v, want exactly +0.1", got)
	}
	iv := v.GetHoldoutInterval()
	if iv.GetLow() == iv.GetHigh() {
		t.Error("the interval is zero-width, which reads as certainty")
	}
	if math.IsNaN(iv.GetLow()) || math.IsNaN(iv.GetHigh()) ||
		math.IsInf(iv.GetLow(), 0) || math.IsInf(iv.GetHigh(), 0) {
		t.Errorf("the interval has a non-finite bound [%v, %v]; it would render as blank",
			iv.GetLow(), iv.GetHigh())
	}
	if v.GetMeasuredCaseCount() != 20 {
		t.Errorf("measured %d Cases in both arms, want 20", v.GetMeasuredCaseCount())
	}
	if v.GetHoldoutUnderpowered() {
		t.Error("a 20-Case holdout is exactly MinHoldout and must not be marked underpowered")
	}
	if v.GetHoldoutUseIndex() != 1 {
		t.Errorf("holdout use index = %d, want 1", v.GetHoldoutUseIndex())
	}
	// The verdict is keyed on the INTERVAL, never on the sign of the point
	// estimate, so this asserts agreement with the interval that was actually
	// produced rather than pinning a word. A +0.1 mean over 20 binary pairs is
	// genuinely inconclusive at 95%, and a test that demanded CONFIRMED here
	// would be demanding the stage overstate its own evidence.
	wantVerdict := knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE
	switch {
	case iv.GetLow() > 0:
		wantVerdict = knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED
	case iv.GetHigh() <= 0:
		wantVerdict = knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED
	}
	if v.GetVerdict() != wantVerdict {
		t.Errorf("verdict = %v, want %v for interval [%v, %v]",
			v.GetVerdict(), wantVerdict, iv.GetLow(), iv.GetHigh())
	}
	// The dev comparison travels with the number so a reader cannot see one
	// without the other.
	if v.GetDevEstimatedGain() != 0.20 || v.GetDevEstimatedInterval() == nil {
		t.Error("the dev estimate was not carried onto the Validation")
	}
	if v.GetSelectRunId() != "select-1" || v.GetValueRunId() != "value-1" ||
		v.GetBaselineRunId() != "base-1" {
		t.Error("the provenance chain was not recorded on the Validation")
	}
	// Spend is the guard's number and must equal what the store settled, on
	// every dimension a cap can be set on. Tokens are compared because
	// SettledSpend is what a resumed run restores the guard from: a dimension
	// the store drops is a dimension the resume stops enforcing, silently.
	// This assertion skipped tokens once, and the Validate loop was dropping
	// them (docs/debt.md#137's defect, in this stage).
	settled, err := h.store.SettledSpend(context.Background(), "validate-1")
	if err != nil {
		t.Fatalf("SettledSpend: %v", err)
	}
	if res.Spent.CostUSDMicros != settled.CostUSDMicros ||
		res.Spent.Calls != settled.Calls ||
		res.Spent.Tokens != settled.Tokens {
		t.Errorf("result spend %+v != settled spend %+v", res.Spent, settled)
	}
	if settled.Tokens == 0 {
		t.Error("the store settled zero tokens for a run that measured both arms; " +
			"a resume would restore a zero token total and stop enforcing --max-tokens")
	}
}

// TestUnderpoweredHoldoutStillProducesANumber pins split.MinHoldout's own
// judgement: below the threshold the Run still executes, and the caveat
// travels with the number rather than replacing it.
func TestUnderpoweredHoldoutStillProducesANumber(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		holdout int
		want    bool
	}{{19, true}, {20, false}} {
		t.Run(fmt.Sprintf("holdout=%d", tc.holdout), func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: tc.holdout, assets: 1})
			var calls atomic.Int64
			h.opts.Agent = &scriptedSetAgent{
				calls:          &calls,
				controlRight:   func(id string) bool { return id >= "hold-002" },
				treatmentRight: func(string) bool { return true },
			}
			res, err := h.opts.Validate(context.Background())
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := res.Validation.GetHoldoutUnderpowered(); got != tc.want {
				t.Errorf("holdout_underpowered = %v, want %v at %d Cases against MinHoldout 20",
					got, tc.want, tc.holdout)
			}
			if res.Validation.GetHoldoutInterval() == nil {
				t.Error("an underpowered holdout must still produce a number, with the caveat attached")
			}
		})
	}
}

// TestNoGainWithoutItsInterval drives a holdout of exactly one usable pair.
//
// One pair cannot support an interval, and the Validation must then report NO
// gain rather than a bare number — the shape prime directive 5 exists to ban,
// and the one a consumer reading `.holdout_gain // 0` cannot tell from a
// measured zero.
func TestNoGainWithoutItsInterval(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 2, holdoutCases: 1, assets: 1})
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls:          &calls,
		controlRight:   func(string) bool { return false },
		treatmentRight: func(string) bool { return true },
	}
	res, err := h.opts.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	v := res.Validation
	if v.GetHoldoutInterval() != nil {
		t.Error("an interval was formed from one pair")
	}
	if v.HoldoutGain != nil {
		t.Errorf("a gain of %v was reported with no interval", v.GetHoldoutGain())
	}
	if v.GetNotMeasured() != knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED {
		t.Errorf("not_measured = %v, want UNDERPOWERED", v.GetNotMeasured())
	}
	if v.GetVerdict() != knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED {
		t.Errorf("verdict = %v, want UNMEASURED", v.GetVerdict())
	}
}

// TestValidateRefusesASecondRunOnTheSamePortfolio is the one-shot rule's
// COMPLETED branch, and there is no flag for it.
//
// The refusal must be free: the agent fails the test on Invoke, so a refusal
// that happened after a call would be a test failure rather than a silent
// pass.
func TestValidateRefusesASecondRunOnTheSamePortfolio(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
	var calls atomic.Int64
	h.opts.Agent = &scriptedSetAgent{
		calls:          &calls,
		controlRight:   func(id string) bool { return id >= "hold-002" },
		treatmentRight: func(string) bool { return true },
	}
	if _, err := h.opts.Validate(context.Background()); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	second := h.opts
	second.RunID = "validate-2"
	second.Agent = refusingAgent{t: t}
	_, err := second.Validate(context.Background())
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("second Validate = %v, want ErrInvalidInput", err)
	}
	var act *errs.Actionable
	if !errors.As(err, &act) {
		t.Fatalf("the refusal is not Actionable: %v", err)
	}
	if !contains(act.Fix, "kno report --validate-run-id") {
		t.Errorf("the fix does not point at the recorded result: %q", act.Fix)
	}
	if !contains(err.Error(), "validate-1") {
		t.Errorf("the refusal does not name the prior validate run: %v", err)
	}
}

// TestRepeatHoldoutUseIsCountedAndDisclosed is the DIFFERENT-portfolio branch.
//
// Refused without the flag, allowed with it, and COUNTED — because the
// alternative to a counted peek is an uncounted one: the user deletes kno.db
// or re-splits with --split-seed, and the tool has traded an honest number for
// a comfortable rule.
func TestRepeatHoldoutUseIsCountedAndDisclosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
	var calls atomic.Int64
	agent := &scriptedSetAgent{
		calls:          &calls,
		controlRight:   func(id string) bool { return id >= "hold-002" },
		treatmentRight: func(string) bool { return true },
	}
	h.opts.Agent = agent
	if _, err := h.opts.Validate(ctx); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	// A second Portfolio, same holdout.
	secondPortfolio(t, h.store)

	refused := h.opts
	refused.RunID = "validate-2"
	refused.SelectRunID = "select-2"
	refused.Agent = refusingAgent{t: t}
	if _, err := refused.Validate(ctx); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("a second Portfolio without --allow-repeat-holdout = %v, want ErrInvalidInput", err)
	}

	allowed := h.opts
	allowed.RunID = "validate-2"
	allowed.SelectRunID = "select-2"
	allowed.AllowRepeatHoldout = true
	res, err := allowed.Validate(ctx)
	if err != nil {
		t.Fatalf("second Validate with --allow-repeat-holdout: %v", err)
	}
	if got := res.Validation.GetHoldoutUseIndex(); got != 2 {
		t.Errorf("holdout use index = %d, want 2", got)
	}
	uses, err := h.store.HoldoutUses(ctx, "holdout-fp-1")
	if err != nil {
		t.Fatalf("HoldoutUses: %v", err)
	}
	if len(uses) != 2 {
		t.Fatalf("recorded %d holdout uses, want 2", len(uses))
	}
}

// secondPortfolio records a second Select run over the same Value run.
func secondPortfolio(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateRun(ctx, &knov1.Run{
		Id: "select-2", Stage: knov1.Stage_STAGE_SELECT,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
	}); err != nil {
		t.Fatalf("creating select-2: %v", err)
	}
	sum := sha256.Sum256([]byte("asset 0 content"))
	if err := st.WritePortfolio(ctx, "select-2", &knov1.Portfolio{
		RunId:                "select-2",
		SourceRunId:          "value-1",
		DevEstimatedGain:     0.15,
		DevEstimatedInterval: &knov1.Interval{Low: 0.05, High: 0.25, Level: 0.95},
		Budget:               &knov1.Budget{MaxContextTokens: 100_000},
		Selected: []*knov1.PortfolioEntry{{
			AssetId: "asset-0", Destination: knov1.Destination_DESTINATION_CONTEXT,
			Rank: 1, ContentHash: sum[:],
		}},
	}); err != nil {
		t.Fatalf("writing the second portfolio: %v", err)
	}
}

// TestAPortfolioThatSelectedNothingConsumesNoHoldout.
//
// A Portfolio that selected nothing is a complete answer, not a failure —
// Select says so — and validate must not turn it into a consumed holdout. No
// agent call, no Run, no holdout_uses row.
func TestAPortfolioThatSelectedNothingConsumesNoHoldout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 0})
	h.opts.Agent = refusingAgent{t: t}

	res, err := h.opts.Validate(ctx)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.NothingToValidate {
		t.Error("a Portfolio that selected nothing did not report NothingToValidate")
	}
	if res.Status != knov1.RunStatus_RUN_STATUS_COMPLETED {
		t.Errorf("status = %v, want COMPLETED", res.Status)
	}
	uses, err := h.store.HoldoutUses(ctx, "holdout-fp-1")
	if err != nil {
		t.Fatalf("HoldoutUses: %v", err)
	}
	if len(uses) != 0 {
		t.Errorf("recorded %d holdout uses; a Portfolio with nothing to measure must leave "+
			"the holdout untouched for the one that earns it", len(uses))
	}
	if _, err := h.store.GetRun(ctx, "validate-1"); err == nil {
		t.Error("a Run was created for a Portfolio there was nothing to validate")
	}
}

// TestMixedDestinationPortfolioIsRefused, and the opt-in changes the LABEL.
//
// Measuring the context subset and calling the result "the Portfolio's holdout
// gain" would be a number about a different set than the one the user is about
// to export.
func TestMixedDestinationPortfolioIsRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mixed := validateFixture{
		devCases: 5, holdoutCases: 6, assets: 2,
		destinations: []knov1.Destination{
			knov1.Destination_DESTINATION_CONTEXT,
			knov1.Destination_DESTINATION_TUNING_SET,
		},
	}

	h := newValidateHarness(t, mixed)
	h.opts.Agent = refusingAgent{t: t}
	_, err := h.opts.Validate(ctx)
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("a mixed Portfolio = %v, want ErrInvalidInput", err)
	}
	var act *errs.Actionable
	if errors.As(err, &act) && !contains(act.Fix, "--context-only") {
		t.Errorf("the fix does not name --context-only: %q", act.Fix)
	}

	h2 := newValidateHarness(t, mixed)
	var calls atomic.Int64
	h2.opts.Agent = &scriptedSetAgent{
		calls:          &calls,
		controlRight:   func(id string) bool { return id >= "hold-002" },
		treatmentRight: func(string) bool { return true },
	}
	h2.opts.ContextOnly = true
	res, err := h2.opts.Validate(ctx)
	if err != nil {
		t.Fatalf("--context-only Validate: %v", err)
	}
	if !res.Validation.GetContextOnly() {
		t.Error("the Validation is not labelled a subset")
	}
	excluded := res.Validation.GetExcludedAssetIds()
	if len(excluded) != 1 || excluded[0] != "asset-1" {
		t.Errorf("excluded_asset_ids = %v, want [asset-1]", excluded)
	}
}

// TestAMissingOrEditedAssetIsRefusedBeforeSpend.
//
// Both refusals are free, and both prevent a headline number describing a set
// that is not the set the report names. The content-hash case mutates one byte
// of pool content between select and validate.
func TestAMissingOrEditedAssetIsRefusedBeforeSpend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("an Asset the pool no longer holds", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 2})
		h.opts.Agent = refusingAgent{t: t}
		h.opts.Pool = loopPool{assets: h.pool.assets[:1]}
		_, err := h.opts.Validate(ctx)
		if !errors.Is(err, errs.ErrInvalidInput) {
			t.Fatalf("= %v, want ErrInvalidInput", err)
		}
		if !contains(err.Error(), "asset-1") {
			t.Errorf("the refusal does not name the missing Asset: %v", err)
		}
	})

	t.Run("an Asset whose content changed since select", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 2})
		h.opts.Agent = refusingAgent{t: t}
		edited := []*core.Asset{
			{Id: "asset-0", Content: []byte("asset 0 content"), Cost: &knov1.CostVector{ContextTokens: 10}},
			{Id: "asset-1", Content: []byte("asset 1 contenX"), Cost: &knov1.CostVector{ContextTokens: 10}},
		}
		h.opts.Pool = loopPool{assets: edited}
		_, err := h.opts.Validate(ctx)
		if !errors.Is(err, errs.ErrInvalidInput) {
			t.Fatalf("= %v, want ErrInvalidInput", err)
		}
		if !contains(err.Error(), "asset-1") {
			t.Errorf("the refusal does not name the edited Asset: %v", err)
		}
	})

	t.Run("a Portfolio recorded without content hashes is not evidence of tampering", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{
			devCases: 5, holdoutCases: 6, assets: 1, noContentHash: true,
		})
		var calls atomic.Int64
		h.opts.Agent = &scriptedSetAgent{
			calls:          &calls,
			controlRight:   func(id string) bool { return id >= "hold-002" },
			treatmentRight: func(string) bool { return true },
		}
		h.opts.Pool = loopPool{assets: []*core.Asset{
			{Id: "asset-0", Content: []byte("something else entirely")},
		}}
		if _, err := h.opts.Validate(ctx); err != nil {
			t.Fatalf("an older Portfolio with no hash was refused: %v", err)
		}
	})
}

// TestABrokenChainIsRefusedBeforeSpend.
//
// A headline number attached to a provenance that cannot be followed is worse
// than no number, because the provenance is what a reader would check.
func TestABrokenChainIsRefusedBeforeSpend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("the select run is not a Select run", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
		h.opts.Agent = refusingAgent{t: t}
		h.opts.SelectRunID = "value-1"
		if _, err := h.opts.Validate(ctx); !errors.Is(err, errs.ErrInvalidInput) {
			t.Fatalf("= %v, want ErrInvalidInput", err)
		}
	})

	t.Run("the value run behind the Portfolio is gone", func(t *testing.T) {
		t.Parallel()
		h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
		h.opts.Agent = refusingAgent{t: t}
		p, err := h.store.Portfolio(ctx, "select-1")
		if err != nil {
			t.Fatalf("Portfolio: %v", err)
		}
		p.SourceRunId = "value-missing"
		if err := h.store.WritePortfolio(ctx, "select-1", p); err != nil {
			t.Fatalf("WritePortfolio: %v", err)
		}
		if _, err := h.opts.Validate(ctx); !errors.Is(err, errs.ErrInvalidInput) {
			t.Fatalf("= %v, want ErrInvalidInput", err)
		}
	})
}

// TestAnAgentThatCannotCarryTheSetIsRefusedBeforeSpend.
//
// An adapter that cannot carry the set still ANSWERS every request — it
// answers the Case without the Portfolio. Both arms then measure the same
// thing and the run produces a verdict at full price that is
// indistinguishable from a real finding that the Portfolio does not help.
func TestAnAgentThatCannotCarryTheSetIsRefusedBeforeSpend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		agent core.Agent
	}{
		{"does not implement ContextSetInjector", noSetAgent{}},
		{"implements it and declares the capability false", declinesSetAgent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
			h.opts.Agent = tc.agent
			_, err := h.opts.Validate(ctx)
			if !errors.Is(err, errs.ErrCapabilityUnsupported) {
				t.Fatalf("= %v, want ErrCapabilityUnsupported", err)
			}
			var act *errs.Actionable
			if errors.As(err, &act) && !contains(act.Fix, "kno doctor") {
				t.Errorf("the fix does not point at `kno doctor`: %q", act.Fix)
			}
		})
	}
}

// TestAnEmptyHoldoutIsRefusedBeforeSpend.
//
// split.Counts.Validate refuses this at Baseline, but the eval source can
// change between runs, so it is re-checked here rather than assumed. A
// validate over zero Cases would produce a Validation with no number AND a
// consumed holdout, which is the worst of both.
func TestAnEmptyHoldoutIsRefusedBeforeSpend(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 0, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	if _, err := h.opts.Validate(context.Background()); !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("= %v, want ErrInvalidInput", err)
	}
}

// TestValidateRefusesASealedEvalsSource is the stage-level face of
// errs.ErrHoldoutSealed.
func TestValidateRefusesASealedEvalsSource(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 6, assets: 1})
	h.opts.Agent = refusingAgent{t: t}
	// Deliberately the wrong shape: this is what a caller who copied the Value
	// stage's wiring would hand over.
	h.opts.Evals = core.Seal(h.opts.Evals)
	if _, err := h.opts.Validate(context.Background()); !errors.Is(err, errs.ErrHoldoutSealed) {
		t.Fatalf("= %v, want ErrHoldoutSealed", err)
	}
}

// TestThePortfolioPrefixIsByteIdenticalAcrossCases pins the prefix-cache
// property set injection exists to preserve.
//
// Providers cache on a PREFIX. One recorded prefix across the whole holdout is
// the difference between paying for the Portfolio's tokens once and paying for
// them once per Case; someone reordering the set per Case would multiply the
// bill by the holdout size and nothing else would notice.
func TestThePortfolioPrefixIsByteIdenticalAcrossCases(t *testing.T) {
	t.Parallel()

	h := newValidateHarness(t, validateFixture{devCases: 5, holdoutCases: 8, assets: 3})
	if _, err := h.opts.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	prefixes := h.agent.Prefixes()
	if len(prefixes) != 1 {
		t.Fatalf("the treatment arm used %d distinct prefixes (%v); one Portfolio over one "+
			"holdout must produce exactly one", len(prefixes), prefixes)
	}
	// Rank order, which is what makes the prefix stable and the cache hit.
	want := "asset 0 content\n\nasset 1 content\n\nasset 2 content"
	if prefixes[0] != want {
		t.Errorf("prefix = %q, want %q (rank order)", prefixes[0], want)
	}
}
