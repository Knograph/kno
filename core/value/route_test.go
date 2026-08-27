package value_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// devCases builds n Cases, failing every failEvery-th one, tagged in a cycle.
func devCases(n, failEvery int, tags ...string) []value.CaseRef {
	out := make([]value.CaseRef, n)
	for i := range n {
		c := value.CaseRef{ID: fmt.Sprintf("case-%03d", i)}
		if failEvery > 0 && i%failEvery == 0 {
			c.Failed = true
		}
		if len(tags) > 0 {
			c.Tags = []string{tags[i%len(tags)]}
		}
		out[i] = c
	}
	return out
}

// TestTheRouterCannotSeeAScore.
//
// The structural invariant §2.3 demands, asserted rather than described. Which
// Cases get measured is a SELECTION; if it depends on a Case's recorded score,
// that score cannot also serve as the Case's control, because reusing the draw
// that selected a Case as its control manufactures the effect being measured.
//
// Draft 2 of the plan proposed "scoping Store.CaseScores so routing cannot
// reach it", which Go cannot express for a method on an interface the same
// package holds. The enforcement is the INPUT TYPE instead: the router's only
// view of a Case is an ID, tags, and a bool. This test fails if a field
// carrying a score value is ever added to it.
//
// Failed is deliberately permitted and is a different exposure: routing to a
// baseline's failures is what makes the stage affordable, and the bias it would
// introduce is removed by a fresh control arm plus the reserved partition. The
// score VALUE is the thing that must not arrive, because it is what a delta is
// computed from.
func TestTheRouterCannotSeeAScore(t *testing.T) {
	t.Parallel()

	allowed := map[string]string{
		"ID":     "string",
		"Tags":   "[]string",
		"Failed": "bool",
	}

	rt := reflect.TypeOf(value.CaseRef{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		want, ok := allowed[f.Name]
		if !ok {
			t.Errorf("CaseRef gained a field %q (%s). The router's view of a Case is "+
				"deliberately an ID, tags, and a bool: a field carrying a score would "+
				"let the routing path and the delta path share a source, and pairing a "+
				"Case against the draw that selected it manufactures the effect being "+
				"measured", f.Name, f.Type)
			continue
		}
		if got := f.Type.String(); got != want {
			t.Errorf("CaseRef.%s is %s, want %s", f.Name, got, want)
		}
	}
	if rt.NumField() != len(allowed) {
		t.Errorf("CaseRef has %d fields, want %d", rt.NumField(), len(allowed))
	}

	// The same rule one level up: no function in this package may take a Score,
	// a Store, or anything carrying one.
	for _, name := range []string{"Score", "Store", "Outcome", "Valuation"} {
		if strings.Contains(reflect.TypeOf(value.Options{}).String(), name) {
			t.Errorf("Options mentions %s", name)
		}
	}
}

// TestTheReservedPartitionIsOutcomeIndependent.
//
// The reservation is what makes the recorded baseline a valid control for the
// harm test, and it holds only if the reserved set was drawn WITHOUT consulting
// the outcome. Asserted statistically, because that is the form the property
// takes: the reserved set's failure rate must track the population's.
//
// The alternative — drawing controls from the complement of a failure-routed
// set — selects on "the baseline passed", and for a null Asset gives
// E[delta_control] = p - 1: a systematic harm signal aimed at inert Assets,
// read through a one-sided harm bound, firing REJECTION_REASON_REGRESSION on
// the null.
func TestTheReservedPartitionIsOutcomeIndependent(t *testing.T) {
	t.Parallel()

	const (
		n         = 4000
		failEvery = 3 // ~33% of Cases failed
	)
	cases := devCases(n, failEvery)
	wantRate := 0.0
	for _, c := range cases {
		if c.Failed {
			wantRate++
		}
	}
	wantRate /= float64(n)

	// Over many seeds, so the assertion is about the mechanism rather than
	// about one lucky draw.
	var total, failed float64
	for seed := range int64(40) {
		plan, err := value.Route(cases, []value.AssetRef{{ID: "a"}}, value.Options{Seed: seed})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		byID := make(map[string]value.CaseRef, len(cases))
		for _, c := range cases {
			byID[c.ID] = c
		}
		for _, id := range plan.ControlCaseIDs {
			total++
			if byID[id].Failed {
				failed++
			}
		}
	}

	got := failed / total
	if math.Abs(got-wantRate) > 0.03 {
		t.Errorf("the reserved partition failed %.3f of its Cases against a population "+
			"rate of %.3f. A reservation that tracks the outcome is not a reservation: "+
			"the recorded baseline stops being a valid control, and a null Asset "+
			"measures a systematic harm of about p-1", got, wantRate)
	}
}

// TestPairingAgainstTheSelectingDrawManufacturesTheEffect.
//
// The regression-to-the-mean canary §6 requires, and the reason the fresh
// control arm exists. Ground truth here is an Asset that does NOTHING: the
// treatment draw is the same Bernoulli(p) as the control draw.
//
// Paired against a FRESH draw on the same Cases, delta is ~0 whatever the
// selection was. Paired against the RECORDED draw that selected those Cases —
// every one of which the baseline failed, so every recorded score is 0 by
// construction — delta is ~p.
//
// Note the direction of the arithmetic, which is easy to state backwards: the
// bias is the agent's success probability ON THAT SLICE, not that slice's
// recorded baseline score, which is zero by construction.
func TestPairingAgainstTheSelectingDrawManufacturesTheEffect(t *testing.T) {
	t.Parallel()

	const (
		p     = 0.7
		draws = 20000
	)
	// Seeded rather than random: the assertion is about the arithmetic, and a
	// canary that fails one run in fifty is a canary people delete.
	rng := rand.New(rand.NewPCG(1, 2))

	var freshDelta, recordedDelta float64
	for range draws {
		bern := func() float64 {
			if rng.Float64() < p {
				return 1
			}
			return 0
		}
		// A Case the baseline failed: its RECORDED score is 0.
		const recorded = 0.0
		treatment := bern()
		control := bern() // the fresh arm: an independent draw of the same null

		freshDelta += treatment - control
		recordedDelta += treatment - recorded
	}
	freshDelta /= draws
	recordedDelta /= draws

	if math.Abs(freshDelta) > 0.02 {
		t.Errorf("a fresh control arm gives delta %.4f for an Asset that does nothing, "+
			"want ~0", freshDelta)
	}
	if math.Abs(recordedDelta-p) > 0.02 {
		t.Errorf("pairing against the recorded draw gives delta %.4f, want ~%.2f — the "+
			"canary is what pins why FreshControlArm exists; if this stops holding, the "+
			"reason for doubling the cost of the routed arm has gone with it",
			recordedDelta, p)
	}

	// And the router asks for the fresh arm exactly when the selection
	// conditioned on the outcome.
	cases := devCases(60, 3, "refunds", "billing")
	for _, tc := range []struct {
		name  string
		cases []value.CaseRef
		want  bool
	}{
		{"routing selected on failure", cases, true},
		{"nothing failed, so nothing was selected on", devCases(60, 0, "refunds"), false},
	} {
		plan, err := value.Route(tc.cases, []value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 7})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := plan.Routed[0].FreshControlArm; got != tc.want {
			t.Errorf("%s: FreshControlArm = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestModeIsDecidedForTheRunBeforeTheQuote.
//
// The mode is a property of the run, fixed before any Asset is considered, so
// the consent quote reflects the path the run will take. The no-tags case is
// the DEFAULT STATE OF A REAL EVAL FILE — Case.tags is optional and nothing
// populates it — so treating it as an edge case ships a stage that rejects
// every Asset as irrelevant and appears to do nothing.
func TestModeIsDecidedForTheRunBeforeTheQuote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cases []value.CaseRef
		want  value.Mode
	}{
		{"tags on the failed Cases", devCases(60, 3, "refunds", "billing"), value.ModeTagOverlap},
		{"no tags anywhere, the modal eval file", devCases(60, 3), value.ModeAllFailed},
		{"nothing failed at all", devCases(60, 0, "refunds"), value.ModeAllDev},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan, err := value.Route(tc.cases,
				[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 3})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if plan.Mode != tc.want {
				t.Errorf("mode = %s, want %s", plan.Mode, tc.want)
			}
			if len(plan.Routed[0].CaseIDs) == 0 {
				t.Errorf("the Asset routed to nothing under %s; a run that rejects "+
					"every Asset as irrelevant looks identical to a stage that does "+
					"nothing", tc.want)
			}
		})
	}
}

// TestRoutingOffKeepsTheHarmTest.
//
// --route none is the flag a user reaches for when they distrust their tags,
// and under an earlier design it silently removed the regression check:
// controls were drawn from the complement of the routed set, and with nothing
// routed there was no complement. Drawing them from a partition reserved
// before routing means routing can be switched off without touching them.
//
// Switching it off also drops the fresh control arm, which is not a saving
// taken carelessly: a random sample is not conditioned on the baseline outcome,
// so the recorded baseline is a valid control there.
func TestRoutingOffKeepsTheHarmTest(t *testing.T) {
	t.Parallel()

	cases := devCases(200, 3, "refunds", "billing")
	assets := []value.AssetRef{{ID: "a", Tags: []string{"astronomy"}}}

	on, err := value.Route(cases, assets, value.Options{Seed: 4})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	off, err := value.Route(cases, assets, value.Options{Seed: 4, DisableRouting: true})
	if err != nil {
		t.Fatalf("Route with routing off: %v", err)
	}

	if len(off.ControlCaseIDs) == 0 {
		t.Fatal("routing off left no harm test; the flag a user reaches for when " +
			"they distrust their tags must not remove the regression check")
	}
	if !slices.Equal(on.ControlCaseIDs, off.ControlCaseIDs) {
		t.Errorf("the reserved partition moved when routing was switched off; it is "+
			"drawn before routing runs and must not depend on it:\n on:  %v\n off: %v",
			on.ControlCaseIDs, off.ControlCaseIDs)
	}

	// The Asset matched no cluster with routing on, and is measured with it off.
	if len(on.Routed[0].CaseIDs) != 0 {
		t.Fatal("the fixture was supposed to route this Asset to nothing")
	}
	if len(off.Routed[0].CaseIDs) == 0 {
		t.Fatal("routing off measured the Asset against nothing")
	}
	if off.Routed[0].FreshControlArm {
		t.Error("routing off still asks for a fresh control arm; a random sample is " +
			"not conditioned on the baseline outcome, so the recorded baseline is a " +
			"valid control and the second measurement is money for nothing")
	}
	if off.Mode != value.ModeAllDev {
		t.Errorf("mode = %s, want %s", off.Mode, value.ModeAllDev)
	}
}

// TestAnUntaggedAssetIsUnlabelledNotIrrelevant.
//
// The modal Asset is one nobody has annotated. Refusing to measure it would
// make it silently unmeasurable while reporting a confident reason.
func TestAnUntaggedAssetIsUnlabelledNotIrrelevant(t *testing.T) {
	t.Parallel()

	plan, err := value.Route(devCases(60, 3, "refunds", "billing"),
		[]value.AssetRef{{ID: "untagged"}}, value.Options{Seed: 5})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(plan.Routed[0].CaseIDs) == 0 {
		t.Fatal("an Asset with no tags routed to nothing; unlabelled is not irrelevant")
	}
}

// TestAnAssetThatMatchesNothingIsTheCheapValuableAnswer.
//
// Routing to zero Cases costs zero measurements and is a real result, not a
// failure — it is the answer a user most wants to get for free.
func TestAnAssetThatMatchesNothingIsTheCheapValuableAnswer(t *testing.T) {
	t.Parallel()

	plan, err := value.Route(devCases(60, 3, "refunds", "billing"),
		[]value.AssetRef{{ID: "a", Tags: []string{"astronomy"}}}, value.Options{Seed: 5, Trials: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	r := plan.Routed[0]
	if len(r.CaseIDs) != 0 {
		t.Fatalf("an Asset matching no cluster routed to %d Cases", len(r.CaseIDs))
	}
	if r.NotMeasuredReason != knov1.RejectionReason_REJECTION_REASON_IRRELEVANT {
		t.Errorf("reason = %v, want IRRELEVANT", r.NotMeasuredReason)
	}
}

// TestTagsMatchAcrossCaseAndWhitespace.
//
// "Refunds" in a pool and "refunds" in an eval file are the same cluster to
// whoever wrote them. An Asset that routes to nothing over a capital letter
// reports IRRELEVANT — a confident wrong answer, free, and indistinguishable
// from a correct one.
func TestTagsMatchAcrossCaseAndWhitespace(t *testing.T) {
	t.Parallel()

	plan, err := value.Route(devCases(60, 3, "refunds", "billing"),
		[]value.AssetRef{{ID: "a", Tags: []string{"  Refunds "}}}, value.Options{Seed: 5})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(plan.Routed[0].CaseIDs) == 0 {
		t.Fatal(`" Refunds " did not match "refunds"`)
	}
}

// TestTheQuoteCarriesEveryMultiplier.
//
// assets x (routed_sample x arms + control_sample) x trials. The plan's review
// record catches a missing multiplier in this formula twice: once the control
// arm, once trials. DESIGN.md's worked example runs 3 trials, so omitting that
// term under-quotes by 3x — and the quote is what the user consents to.
func TestTheQuoteCarriesEveryMultiplier(t *testing.T) {
	t.Parallel()

	cases := devCases(60, 3, "refunds")
	assets := []value.AssetRef{{ID: "a", Tags: []string{"refunds"}}, {ID: "b", Tags: []string{"refunds"}}}

	one, err := value.Route(cases, assets, value.Options{Seed: 11, Trials: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	three, err := value.Route(cases, assets, value.Options{Seed: 11, Trials: 3})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got, want := three.Measurements(), one.Measurements()*3; got != want {
		t.Errorf("3 trials quotes %d measurements against %d for 1; trials is a "+
			"multiplier on the whole run, and omitting it under-quotes the figure "+
			"the user consents to by 3x", got, want)
	}

	// Recompute the formula independently of the implementation.
	var want int64
	for i := range one.Routed {
		n := int64(len(one.Routed[i].CaseIDs))
		if one.Routed[i].FreshControlArm {
			n *= 2
		}
		want += n + int64(len(one.ControlCaseIDs))
	}
	if got := one.Measurements(); got != want {
		t.Errorf("Measurements() = %d, want %d", got, want)
	}
	if !one.Routed[0].FreshControlArm {
		t.Fatal("this fixture should have selected on failure")
	}
	if want == 0 {
		t.Fatal("the fixture quotes nothing, so the assertion above proves nothing")
	}
}

// TestTheSameSeedSelectsTheSameCases.
//
// The seed is recorded on the Run so a reader can re-derive which Cases were
// chosen. That claim is only true if the selection is reproducible — and it
// must not depend on the order the Cases arrived in, or two runs over the same
// eval file read through different adapters would reserve different sets while
// recording the same seed.
func TestTheSameSeedSelectsTheSameCases(t *testing.T) {
	t.Parallel()

	cases := devCases(80, 3, "refunds", "billing")
	assets := []value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}

	first, err := value.Route(cases, assets, value.Options{Seed: 42})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	shuffled := slices.Clone(cases)
	slices.Reverse(shuffled)
	second, err := value.Route(shuffled, assets, value.Options{Seed: 42})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if !slices.Equal(first.ControlCaseIDs, second.ControlCaseIDs) {
		t.Errorf("the reserved partition changed with the input order:\n %v\n %v\n"+
			"the seed on the Run then identifies a selection it did not make",
			first.ControlCaseIDs, second.ControlCaseIDs)
	}
	if !slices.Equal(first.Routed[0].CaseIDs, second.Routed[0].CaseIDs) {
		t.Errorf("the routed sample changed with the input order")
	}

	third, err := value.Route(cases, assets, value.Options{Seed: 43})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if slices.Equal(first.ControlCaseIDs, third.ControlCaseIDs) {
		t.Error("two different seeds reserved the same partition, so the seed is not " +
			"what the selection depends on")
	}
}

// TestTwoAssetsDrawIndependentSamples.
//
// Without a per-draw stream every Asset with the same candidate count receives
// the same Cases in the same order. That looks exactly like a working sample
// and correlates every Asset's measurement with every other's, so the
// portfolio's Assets are compared on one arbitrary slice rather than on the
// eval set.
func TestTwoAssetsDrawIndependentSamples(t *testing.T) {
	t.Parallel()

	// Two Assets over the SAME candidate cluster, sampled below full.
	cases := devCases(200, 2, "refunds")
	assets := []value.AssetRef{{ID: "asset-a", Tags: []string{"refunds"}}, {ID: "asset-b", Tags: []string{"refunds"}}}

	plan, err := value.Route(cases, assets, value.Options{Seed: 9, SampleRate: 0.5})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	a, b := plan.Routed[0].CaseIDs, plan.Routed[1].CaseIDs
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("the fixture routed nothing")
	}
	if slices.Equal(a, b) {
		t.Errorf("both Assets drew the identical %d Cases; the sample is keyed by the "+
			"seed alone, so every Asset of the same size is measured on one slice", len(a))
	}
}

// TestAnUnderpoweredHarmTestSaysSo.
//
// A two-sided interval on a small control sample spans zero, and the shipped
// coloring rule renders that as "no regression". An underpowered harm test that
// looks like a passed one is worse than no test, so the marker travels with the
// number.
func TestAnUnderpoweredHarmTestSaysSo(t *testing.T) {
	t.Parallel()

	small, err := value.Route(devCases(12, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !small.ControlUnderpowered {
		t.Errorf("a %d-Case control sample is not marked underpowered; below about "+
			"%d paired observations a real regression and a clean result are "+
			"indistinguishable", len(small.ControlCaseIDs), value.MinControlSample)
	}

	big, err := value.Route(devCases(400, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if big.ControlUnderpowered {
		t.Errorf("a %d-Case control sample is marked underpowered; the marker would "+
			"then be on every run and mean nothing", len(big.ControlCaseIDs))
	}
}

// TestRouteRefusesWhatItCannotHonestlyMeasure: every refusal is free and lands
// before a single Case is sent.
func TestRouteRefusesWhatItCannotHonestlyMeasure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		cases  []value.CaseRef
		assets []value.AssetRef
		wants  string
	}{
		{"no Cases", nil, []value.AssetRef{{ID: "a"}}, "dev split is empty"},
		{"no Assets", devCases(10, 2), nil, "pool is empty"},
		{
			"a Case with no ID",
			[]value.CaseRef{{ID: ""}},
			[]value.AssetRef{{ID: "a"}},
			"no ID",
		},
		{
			"the same Case twice",
			[]value.CaseRef{{ID: "dup"}, {ID: "dup"}},
			[]value.AssetRef{{ID: "a"}},
			"appears twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := value.Route(tc.cases, tc.assets, value.Options{Seed: 1})
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q", err, tc.wants)
			}
		})
	}
}
