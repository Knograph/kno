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
	"github.com/knograph/kno/stats/interval"
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
// The regression-to-the-mean canary §6 requires, driven THROUGH Route. An
// earlier version of this test hardcoded `recorded = 0.0` and compared two
// Bernoulli draws, which asserted two properties of math/rand and nothing about
// this package — no mutation to route.go could fail it, which is the debt-#70
// pattern wearing the name of the test the plan asked for.
//
// Ground truth here is an Asset that does NOTHING: its treatment draw is the
// same Bernoulli(p) as the control draw. The question is what the two available
// controls measure on the Cases ROUTING ACTUALLY CHOSE.
func TestPairingAgainstTheSelectingDrawManufacturesTheEffect(t *testing.T) {
	t.Parallel()

	const (
		p     = 0.7
		nDev  = 400
		runs  = 60
		level = 0.03
	)

	var freshTotal, recordedTotal, samples float64
	for seed := range int64(runs) {
		// Seeded per iteration so a failure reproduces exactly.
		rng := rand.New(rand.NewPCG(uint64(seed), 99))
		bern := func() float64 {
			if rng.Float64() < p {
				return 1
			}
			return 0
		}

		// A baseline with real per-Case draws, and the router sees only the
		// pass/fail bit — exactly what it is given in production.
		cases := make([]value.CaseRef, nDev)
		recorded := make(map[string]float64, nDev)
		for i := range nDev {
			id := fmt.Sprintf("case-%04d", i)
			score := bern()
			recorded[id] = score
			cases[i] = value.CaseRef{ID: id, Failed: score == 0}
		}

		plan, err := value.Route(cases, []value.AssetRef{{ID: "null-asset"}}, value.Options{Seed: seed})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		routed := plan.Routed[0].CaseIDs
		if len(routed) == 0 {
			t.Fatal("the fixture routed nothing")
		}

		for _, id := range routed {
			treatment := bern() // the null Asset: an independent draw of the same thing
			freshTotal += treatment - bern()
			recordedTotal += treatment - recorded[id]
			samples++
		}
	}

	fresh, stale := freshTotal/samples, recordedTotal/samples
	if math.Abs(fresh) > level {
		t.Errorf("a FRESH control arm measures %.4f for an Asset that does nothing, "+
			"want ~0", fresh)
	}
	if math.Abs(stale-p) > level {
		t.Errorf("pairing against the recorded draw that SELECTED these Cases measures "+
			"%.4f, want ~%.2f. This is why FreshControlArm exists; if it stops "+
			"holding, the reason for doubling the routed arm's cost has gone with it",
			stale, p)
	}
}

// TestEveryOutcomeConditionedSelectionGetsAFreshControlArm.
//
// The predicate that decides whether an Asset costs one measurement per Case or
// two, and getting it wrong is a sign error on every null Asset — in whichever
// direction the branch happens to condition.
//
// ModeAllDev is reached two ways and they are NOT equivalent. `--route none` is
// genuinely blind. "Nothing failed" is a fact about the outcomes, DISCOVERED BY
// READING THEM: conditional on that branch every recorded score on the
// candidates is a pass, so a null Asset's fresh draw averages p against a
// recorded 1 and measures p-1 — the mirror image of the +0.70 above, aimed at
// inert Assets, on exactly the small eval sets this stage advertises to.
func TestEveryOutcomeConditionedSelectionGetsAFreshControlArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cases []value.CaseRef
		opts  value.Options
		mode  value.Mode
		fresh bool
	}{
		{
			name:  "tag overlap selects the failures its tags match",
			cases: devCases(60, 3, "refunds", "billing"),
			mode:  value.ModeTagOverlap,
			fresh: true,
		},
		{
			// The modal eval file, and the mode with no assertion at all before
			// this test existed.
			name:  "no tags anywhere still selects the failures",
			cases: devCases(60, 3),
			mode:  value.ModeAllFailed,
			fresh: true,
		},
		{
			name:  "nothing failed is itself a fact about the outcomes",
			cases: devCases(60, 0, "refunds"),
			mode:  value.ModeAllDev,
			fresh: true,
		},
		{
			name:  "routing off reads no outcome at all",
			cases: devCases(60, 3, "refunds"),
			opts:  value.Options{DisableRouting: true},
			mode:  value.ModeAllDev,
			fresh: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := tc.opts
			opts.Seed = 7
			plan, err := value.Route(tc.cases,
				[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if plan.Mode != tc.mode {
				t.Fatalf("mode = %s, want %s", plan.Mode, tc.mode)
			}
			if got := plan.Routed[0].FreshControlArm; got != tc.fresh {
				t.Errorf("FreshControlArm = %v, want %v. Pairing an outcome-conditioned "+
					"selection against the recorded baseline manufactures a delta with "+
					"the sign of whatever the branch selected on", got, tc.fresh)
			}
		})
	}
}

// TestRoutingSelectsFailuresWhereItClaimsTo.
//
// ModeTagOverlap and ModeAllFailed both promise to route to the Cases the
// baseline FAILED — that promise is what makes the stage affordable, and it is
// what FreshControlArm is priced against. Nothing asserted it, so a router that
// quietly returned every eligible Case in either mode passed the whole suite
// while roughly tripling the run's cost.
func TestRoutingSelectsFailuresWhereItClaimsTo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		cases      []value.CaseRef
		wantFailed bool
	}{
		{"tag overlap", devCases(120, 3, "refunds"), true},
		{"no tags anywhere", devCases(120, 3), true},
		{"nothing to route on", devCases(120, 0, "refunds"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan, err := value.Route(tc.cases,
				[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 2})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			failed := make(map[string]bool, len(tc.cases))
			for _, c := range tc.cases {
				failed[c.ID] = c.Failed
			}
			routed := plan.Routed[0].CaseIDs
			if len(routed) == 0 {
				t.Fatal("routed nothing")
			}
			var passing int
			for _, id := range routed {
				if !failed[id] {
					passing++
				}
			}
			if tc.wantFailed && passing > 0 {
				t.Errorf("%d of %d routed Cases are ones the baseline PASSED; this mode "+
					"claims to route to failures, and measuring the rest is money spent "+
					"where there was nothing to improve", passing, len(routed))
			}
			if !tc.wantFailed && passing == 0 {
				t.Error("every routed Case failed in a mode where nothing failed")
			}
		})
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
//
// The flag fires when the sample is below the MinControlSample floor OR when
// the honest bound cannot separate HarmMargin from zero. The consequence is
// stated rather than hidden: against epsilon=0.10 the honest threshold sits
// near m=135, so the flag fires on nearly every real run. That is the design —
// the number is always reported and the flag is information, not an alarm —
// and the Q2 review's rejection was of a bool that REPLACED the number, not of
// a flag that travels beside it.
func TestAnUnderpoweredHarmTestSaysSo(t *testing.T) {
	t.Parallel()

	small, err := value.Route(devCases(12, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !small.ControlUnderpowered {
		t.Errorf("a %d-Case control sample is not marked underpowered; below %d "+
			"paired observations a real regression and a clean result are "+
			"indistinguishable", len(small.ControlCaseIDs), value.MinControlSample)
	}

	// 400 dev Cases reserve ~120 controls; the honest bound there is ~0.106,
	// still above HarmMargin=0.10, so the flag fires — barely — on the honest
	// bound alone even though the sample clears the floor.
	mid, err := value.Route(devCases(400, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !mid.ControlUnderpowered {
		t.Errorf("a %d-Case control sample clears the floor but its honest bound "+
			"(%.4f) is above HarmMargin, so the flag must fire on the bound; the "+
			"number is what the reader acts on",
			len(mid.ControlCaseIDs), mid.MinDetectableHarm)
	}

	// 2000 dev Cases reserve ~600 controls; the bound is ~0.047, well inside
	// HarmMargin, and the floor is cleared, so nothing fires.
	big, err := value.Route(devCases(2000, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if big.ControlUnderpowered {
		t.Errorf("a %d-Case control sample with honest bound %.4f inside HarmMargin "+
			"is marked underpowered", len(big.ControlCaseIDs), big.MinDetectableHarm)
	}
}

// TestAnAssetRoutedToNothingIsNotChargedForAHarmTest.
//
// An Asset that matches no cluster is never put in front of the agent, so there
// is nothing to test it for harm. Charging it the full control arm made the
// cheapest answer the stage gives away the most expensive line in the quote:
// on the pool the docs describe — 200 Assets, 199 matching nothing — it quoted
// 12,036 measurements for 96 of real work. DESIGN.md budgets ~30% of Assets
// routing to zero, so this is the designed-for case, and a user shown the
// dollar figure derived from a 125x over-quote aborts a run they should have
// approved.
func TestAnAssetRoutedToNothingIsNotChargedForAHarmTest(t *testing.T) {
	t.Parallel()

	cases := devCases(200, 2, "refunds", "billing")
	assets := []value.AssetRef{{ID: "matches", Tags: []string{"refunds"}}}
	for i := range 199 {
		assets = append(assets, value.AssetRef{
			ID: fmt.Sprintf("misses-%03d", i), Tags: []string{"astronomy"},
		})
	}

	plan, err := value.Route(cases, assets, value.Options{Seed: 3, Trials: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	var measured int
	for _, r := range plan.Routed {
		if len(r.CaseIDs) > 0 {
			measured++
		}
	}
	if measured != 1 {
		t.Fatalf("%d Assets routed to something, want 1", measured)
	}

	// The one measured Asset: its treatment arm doubled for the fresh control,
	// plus the harm test. Nothing else.
	only := plan.Routed[0]
	want := int64(len(only.CaseIDs)*2 + len(plan.ControlCaseIDs))
	if got := plan.Measurements(); got != want {
		t.Errorf("quote = %d measurements, want %d. The 199 Assets that route to "+
			"nothing are never sent to the agent, so a quote that charges each of "+
			"them a full harm test prices the answer the stage gives away for free",
			got, want)
	}
}

// TestTheDefaultsAreTheDocumentedOnes.
//
// Each of these is derived in its godoc and each was silently mutable: the
// suite stayed green with ControlReserve at 0.06, which guts the harm test's
// power, and with MinSample at 1, the floor whose whole stated purpose is that
// a small eval set must not produce a one-Case measurement.
func TestTheDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"sample rate", value.DefaultSampleRate, 0.8},
		{"control sample rate", value.DefaultControlSampleRate, 1.0},
		{"control reserve", value.DefaultControlReserve, 0.3},
		{"min sample", float64(value.DefaultMinSample), 5},
		{"min control sample", float64(value.MinControlSample), 20},
		{"harm margin", value.HarmMargin, 0.10},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v; the derivation in its godoc is the "+
				"justification for this number and moving one without the other "+
				"leaves the reasoning describing a value that is not shipping",
				tc.name, tc.got, tc.want)
		}
	}

	// And they are actually applied when the caller supplies nothing.
	plan, err := value.Route(devCases(200, 2, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if want := int(200 * value.DefaultControlReserve); len(plan.ControlCaseIDs) != want {
		t.Errorf("reserved %d of 200 Cases, want %d", len(plan.ControlCaseIDs), want)
	}
	if plan.Trials != 1 {
		t.Errorf("trials = %d, want 1; a default of 3 silently triples every run",
			plan.Trials)
	}
}

// TestASmallEvalSetKeepsBothSides.
//
// Nobody tests at n=2, and n=2 is where the reservation's floor earns its
// keep: without it the reserved side is empty and the harm test disappears
// entirely on the smallest runs, which is where a user is most likely to be
// trying the tool out.
//
// n=1 is the honest exception — there is no way to have both sides — and it is
// asserted rather than left to chance, because a harm test that is ABSENT must
// not read the same as one that is merely small.
func TestASmallEvalSetKeepsBothSides(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 2, 3, 5, 10} {
		plan, err := value.Route(devCases(n, 2), []value.AssetRef{{ID: "a"}},
			value.Options{Seed: 1, Trials: 1})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		control, routed := len(plan.ControlCaseIDs), len(plan.Routed[0].CaseIDs)

		if n == 1 {
			if control != 0 {
				t.Errorf("n=1 reserved %d Cases; there is only one and it cannot be "+
					"on both sides", control)
			}
			if plan.MinDetectableHarm != 0 {
				t.Errorf("n=1 reports a detectable harm of %v for a harm test that "+
					"does not exist; a small number here reads as a tight bound",
					plan.MinDetectableHarm)
			}
		} else if control < 1 {
			t.Errorf("n=%d reserved nothing, so the harm test disappeared on exactly "+
				"the run size where a user is trying the tool out", n)
		}
		if routed < 1 {
			t.Errorf("n=%d routed nothing", n)
		}
		if !plan.ControlUnderpowered {
			t.Errorf("n=%d is not marked underpowered with %d control Cases", n, control)
		}
	}
}

// TestTheReportedBoundIsTheOneTheRunCanActuallySee.
//
// The underpowered flag answers "is this absurd", which a reader turns into
// "is this safe". MinDetectableHarm answers the question they were asking. At
// the 20-Case threshold the honest bound is 1.729 x sqrt(0.5)/sqrt(20) = 0.261
// — more than twice HarmMargin — so a run just above the floor has NOT cleared
// an Asset that costs 0.10, and the number says so where the bool does not.
//
// The arithmetic is pinned against external constants, not against the code's
// own formula: the earlier version of this test asserted the code matched
// itself while the code used the VARIANCE bound 0.5 where the sd bound
// sqrt(0.5) belongs, understating every reported minimum by ~sqrt(2).
func TestTheReportedBoundIsTheOneTheRunCanActuallySee(t *testing.T) {
	t.Parallel()

	// The critical values are the EXACT one-sided 95% Student-t quantiles,
	// not the 3-decimal table this code used to carry and not z. The bound
	// now delegates to interval.MinDetectableEffect, which computes the t
	// quantile at every df — so df=59 gets t (1.6711), where the old table
	// ran out at df=31 and fell back to z (1.645), understating the bound by
	// 1.6%. Widening a bound that was too narrow is the conservative
	// direction, and pinning the exact quantiles here is what keeps this test
	// asserting against external arithmetic rather than against the code.
	const (
		sd  = 0.7071067811865476
		t20 = 1.729 // one-sided 95% at df=19
		t60 = 1.671 // one-sided 95% at df=59
		// The published constants carry three decimals, so the comparison
		// does too. A tighter tolerance would be pinning the table's rounding
		// rather than the statistic.
		tol = 5e-4
	)
	// dev=67 reserves floor(0.3 x 67) = 20 controls; dev=200 reserves 60.
	plan20, err := value.Route(devCases(67, 3), []value.AssetRef{{ID: "a"}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if m := len(plan20.ControlCaseIDs); m != 20 {
		t.Fatalf("fixture reserved %d controls, want 20", m)
	}
	if got := plan20.MinDetectableHarm; math.Abs(got-t20*sd/math.Sqrt(20)) > tol {
		t.Errorf("MinDetectableHarm over 20 Cases = %v, want %v", got, t20*sd/math.Sqrt(20))
	}
	plan60, err := value.Route(devCases(200, 3), []value.AssetRef{{ID: "a"}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// df=59, where the bound used to fall back to z=1.645 and come back
	// SMALLER than the truth. t is above z at every finite df, so the old
	// value flattered the run by ~1.6% here and by ~3% at m=33.
	if got := plan60.MinDetectableHarm; math.Abs(got-t60*sd/math.Sqrt(60)) > tol {
		t.Errorf("MinDetectableHarm over 60 Cases = %v, want %v", got, t60*sd/math.Sqrt(60))
	}
	if got := plan60.MinDetectableHarm; got <= 1.645*sd/math.Sqrt(60) {
		t.Errorf("MinDetectableHarm over 60 Cases = %v, which is at or below the z "+
			"bound %v — the bound must never be optimistic, and t exceeds z at "+
			"every finite df", got, 1.645*sd/math.Sqrt(60))
	}

	// The bound must shrink as the sample grows, or it is not a bound.
	plan, err := value.Route(devCases(400, 3), []value.AssetRef{{ID: "a"}},
		value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	small, err := value.Route(devCases(100, 3), []value.AssetRef{{ID: "a"}},
		value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if small.MinDetectableHarm <= plan.MinDetectableHarm {
		t.Errorf("a smaller control sample reports a TIGHTER bound (%v over %d vs %v "+
			"over %d)", small.MinDetectableHarm, len(small.ControlCaseIDs),
			plan.MinDetectableHarm, len(plan.ControlCaseIDs))
	}
}

// TestTheReportedBoundCoversTheWorstCase simulates the worst-case paired
// binary process — differences split evenly between -1 and +1, which is the
// variance maximum the sd bound is derived from — and asserts the harm
// bound's empirical coverage: under the null the lower bound must sit at or
// below the true mean at least 95% of the time. If the reported arithmetic
// under-stated the bound (the variance/sd slip), this test fails at the small
// m where the t correction matters.
func TestTheReportedBoundCoversTheWorstCase(t *testing.T) {
	t.Parallel()

	for _, m := range []int{10, 20, 60} {
		m := m
		t.Run(fmt.Sprintf("m=%d", m), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(uint64(m), uint64(m)))
			const reps = 20000
			covered := 0
			for range reps {
				deltas := make([]float64, m)
				for i := range deltas {
					if rng.IntN(2) == 0 {
						deltas[i] = -1
					} else {
						deltas[i] = 1
					}
				}
				iv := interval.HarmBound(deltas, knov1.ScoreDomain_SCORE_DOMAIN_BINARY, 1, 0.95)
				if iv.GetLow() <= 0 {
					covered++
				}
			}
			if rate := float64(covered) / reps; rate < 0.94 {
				t.Errorf("empirical coverage %.4f at m=%d, want >= 0.95", rate, m)
			}
		})
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

func TestPlanCarriesAClusterSnapshot(t *testing.T) {
	t.Parallel()
	plan, err := value.Route(devCases(60, 3, "refunds", "billing"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 2})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(plan.Clusters) != 2 {
		t.Fatalf("Clusters = %d entries, want 2", len(plan.Clusters))
	}
	// Deterministic snapshot order: the map order cluster() returns is not.
	if plan.Clusters[0].Tag != "billing" || plan.Clusters[1].Tag != "refunds" {
		t.Fatalf("cluster order = %q, want sorted tag order", []string{plan.Clusters[0].Tag, plan.Clusters[1].Tag})
	}
	// Every failed dev Case appears in exactly its tag's cluster.
	failed := make(map[string]bool)
	for _, c := range devCases(60, 3, "refunds", "billing") {
		failed[c.ID] = c.Failed
	}
	for _, s := range plan.Clusters {
		for _, id := range s.CaseIDs {
			if !failed[id] {
				t.Errorf("cluster %s contains %s, which did not fail", s.Tag, id)
			}
			if s.NDropped != 0 {
				t.Errorf("cluster %s NDropped = %d, want 0", s.Tag, s.NDropped)
			}
		}
	}
}

func TestSnapshotDedupsSameTagDuplicates(t *testing.T) {
	t.Parallel()
	// Every failed Case carries the SAME tag twice. Routing measures each of
	// them once (candidatesFor dedups); the snapshot records each once and
	// counts the dropped reference. Each Case belongs to its clusters exactly
	// once, and the duplicate count proves the snapshot saw the duplicates.
	var cases []value.CaseRef
	for i := range 90 {
		c := value.CaseRef{ID: fmt.Sprintf("f-%03d", i)}
		if i%3 == 0 {
			c.Failed = true
			c.Tags = []string{"refunds", "refunds"}
		} else {
			c.Tags = []string{"refunds"}
		}
		cases = append(cases, c)
	}
	plan, err := value.Route(cases,
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 1})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(plan.Clusters) != 1 {
		t.Fatalf("Clusters = %d, want 1", len(plan.Clusters))
	}
	s := plan.Clusters[0]
	if len(s.CaseIDs) != s.NDropped {
		t.Fatalf("CaseIDs = %d entries but NDropped = %d: every failed Case in this "+
			"fixture carries the tag twice, so the counts must match",
			len(s.CaseIDs), s.NDropped)
	}
	seen := make(map[string]bool)
	for _, id := range s.CaseIDs {
		if seen[id] {
			t.Errorf("Case %s appears twice in the snapshot", id)
		}
		seen[id] = true
	}
	if len(s.CaseIDs) == 0 {
		t.Fatal("snapshot is empty; the fixture's failed Cases must be eligible")
	}
}

func TestSnapshotIsEmptyWhenThereIsNoCluster(t *testing.T) {
	t.Parallel()
	// Nothing failed: ModeAllDev, and the snapshot must be empty — the
	// report's "no cluster data for this run" for a run with no failures.
	plan, err := value.Route(devCases(60, 0, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}}, value.Options{Seed: 2})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Clusters != nil {
		t.Fatalf("Clusters = %v, want nil when nothing failed", plan.Clusters)
	}
	// Routing disabled is the same answer.
	plan, err = value.Route(devCases(60, 3, "refunds"),
		[]value.AssetRef{{ID: "a", Tags: []string{"refunds"}}},
		value.Options{Seed: 2, DisableRouting: true})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Clusters != nil {
		t.Fatalf("Clusters = %v, want nil when routing is disabled", plan.Clusters)
	}
}

// TestTheHarmGateClearsWhereThePowerActuallyArrives pins the sample size at
// which a control arm stops being underpowered.
//
// ControlUnderpowered is MinDetectableHarm > HarmMargin, so any understatement
// of the bound clears the gate EARLY — and the bound was understated for every
// m past 33, because it fell back to z=1.645 where the true t is larger. The
// gate cleared at m=136; the honest crossing is m=138. Two sample sizes
// declared a powered control arm they did not have, which is exactly the
// "underpowered harm test that looks like a passed one" the regression rule
// refuses to act on.
//
// Pinned by arithmetic rather than by routing a fixture, because the crossing
// is a property of the bound and the margin, not of any particular eval set.
func TestTheHarmGateClearsWhereThePowerActuallyArrives(t *testing.T) {
	t.Parallel()

	underpowered := func(m int) bool { return value.MinDetectableHarmFor(m) > value.HarmMargin }

	if underpowered(138) {
		t.Errorf("a control arm of 138 is reported underpowered; the bound at 138 is %v, "+
			"which clears HarmMargin %v", value.MinDetectableHarmFor(138), value.HarmMargin)
	}
	for _, m := range []int{136, 137} {
		if !underpowered(m) {
			t.Errorf("a control arm of %d is reported POWERED; its bound is %v against "+
				"HarmMargin %v. This is the z-fallback bug: the gate must not clear "+
				"before the power actually arrives", m, value.MinDetectableHarmFor(m), value.HarmMargin)
		}
	}
}
