package cli

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// The analysis unit tests. They drive inspectEvals over synthetic sources so
// that every threshold boundary is hit exactly, which a golden cannot do.
//
// Every threshold assertion reads the EXPORTED constant rather than a literal:
// changing core.MinClusterCases or split.MinHoldout must change what these
// tests expect, not break them.

// fixtureEvals is an in-memory evalSource with an exactly-specified split.
//
// The jsonl adapter assigns splits by hashing Case IDs and treats
// HoldoutFrac 0 as "use the default", so it cannot produce an all-dev set —
// which every per-behavior count below needs, because a hash-driven split
// would make the expected numbers a function of the Case IDs. This fixture
// states the split instead of deriving it.
type fixtureEvals struct {
	cases    []*core.Case
	hash     string
	openErr  error
	yieldErr error
}

func (f *fixtureEvals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return func(yield func(*core.Case, error) bool) {
		for _, c := range f.cases {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if !yield(c, nil) {
				return
			}
		}
		if f.yieldErr != nil {
			// Fatal by contract: the consumer must stop ranging.
			yield(nil, f.yieldErr)
		}
	}, nil
}

func (f *fixtureEvals) CountSplits(_ context.Context) (split.Counts, error) {
	c := split.Counts{HoldoutFrac: split.DefaultHoldoutFrac}
	for _, cs := range f.cases {
		if cs.GetSplit() == knov1.Split_SPLIT_HOLDOUT {
			c.Holdout++
			continue
		}
		c.Dev++
	}
	return c, nil
}

func (f *fixtureEvals) ContentHash(_ context.Context) (string, error) { return f.hash, nil }

// devCase and holdoutCase build one Case in the named split.
//
// Every Case carries a SENTINEL input, expected and rubric. Nothing in this
// command may print Case content, and building the sentinel into every
// fixture means the honesty test cannot pass by inspecting a fixture that
// happened to have empty fields.
const contentSentinel = "SENTINEL-CASE-CONTENT-MUST-NEVER-BE-PRINTED"

func devCase(id string, tags ...string) *core.Case {
	return &core.Case{
		Id:       id,
		Input:    contentSentinel + "-input-" + id,
		Expected: contentSentinel + "-expected-" + id,
		Rubric:   contentSentinel + "-rubric-" + id,
		Split:    knov1.Split_SPLIT_DEV,
		Tags:     tags,
		History: []*knov1.Turn{{
			Role:    knov1.Role_ROLE_USER,
			Content: contentSentinel + "-turn-" + id,
		}},
	}
}

func holdoutCase(id string, tags ...string) *core.Case {
	c := devCase(id, tags...)
	c.Split = knov1.Split_SPLIT_HOLDOUT
	return c
}

// inspectFixture analyses an in-memory eval set.
func inspectFixture(t *testing.T, cases ...*core.Case) *inspection {
	t.Helper()

	insp, err := inspectEvals(context.Background(),
		&fixtureEvals{cases: cases, hash: "fixture-hash"}, "fixture.jsonl")
	if err != nil {
		t.Fatalf("inspectEvals: %v", err)
	}
	return insp
}

// devCases builds n dev Cases carrying the given tags.
func devCases(n int, tags ...string) []*core.Case {
	out := make([]*core.Case, 0, n)
	for i := range n {
		out = append(out, devCase(fmt.Sprintf("case-%04d", i), tags...))
	}
	return out
}

// checkStatus returns one check's status, or "" when the check is absent.
func checkStatus(insp *inspection, name string) string {
	for _, c := range insp.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

// TestInspectCollapsesSpellingsTheWayRoutingDoes: Refunds, refunds and
// " refunds " are one behavior to cluster(), so they must be one behavior
// here, and the collapse must be REPORTED — a user who believes they have
// eight behaviors while routing sees six needs that line more than any other.
func TestInspectCollapsesSpellingsTheWayRoutingDoes(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(
		t,
		devCase("c1", "Refunds"),
		devCase("c2", "refunds"),
		devCase("c3", " refunds "),
	)

	if len(insp.Behaviors) != 1 {
		t.Fatalf("got %d behaviors, want 1: %+v", len(insp.Behaviors), insp.Behaviors)
	}
	b := insp.Behaviors[0]
	if b.Tag != value.NormalizeTag("Refunds") {
		t.Errorf("behavior tag %q, want routing's normalized form", b.Tag)
	}
	if b.DevCases != 3 {
		t.Errorf("dev Cases %d, want 3", b.DevCases)
	}
	if b.Spellings != 3 {
		t.Errorf("spellings %d, want 3", b.Spellings)
	}
	if insp.CollapsedSpellings != 3 || insp.CollapsedTag != "refunds" {
		t.Errorf("collapse report = %d into %q, want 3 into \"refunds\"",
			insp.CollapsedSpellings, insp.CollapsedTag)
	}
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), `3 spellings collapsed into "refunds"`) {
		t.Errorf("the human rendering does not report the collapse:\n%s", out.String())
	}
}

// TestInspectSkipsBlankTagsAndCountsThem: cluster() skips a tag whose
// normalized form is empty. inspect must skip it identically and must not
// drop it silently.
func TestInspectSkipsBlankTagsAndCountsThem(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(
		t,
		devCase("c1", "", "  ", "billing"),
		devCase("c2", "\t"),
	)

	if len(insp.Behaviors) != 1 || insp.Behaviors[0].Tag != "billing" {
		t.Fatalf("got %+v, want exactly the billing behavior", insp.Behaviors)
	}
	if insp.BlankTagRefs != 3 {
		t.Errorf("blank tag refs %d, want 3", insp.BlankTagRefs)
	}
	// c2 carries only blank tags, so it is UNTAGGED as far as routing is
	// concerned. Counting it as tagged would over-credit the taxonomy.
	if insp.UntaggedDevCases != 1 {
		t.Errorf("untagged dev Cases %d, want 1 (the all-blank Case)", insp.UntaggedDevCases)
	}
}

// TestInspectCountsARepeatedTagOnceAndReportsTheDuplicate: snapshotClusters'
// NDropped accounting, in the one place it is visible before a run.
func TestInspectCountsARepeatedTagOnceAndReportsTheDuplicate(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, devCase("c1", "refunds", "refunds", "Refunds"))

	if len(insp.Behaviors) != 1 {
		t.Fatalf("got %d behaviors, want 1", len(insp.Behaviors))
	}
	if got := insp.Behaviors[0].DevCases; got != 1 {
		t.Errorf("dev Cases %d, want 1: one Case is one member however often it spells the tag", got)
	}
	if insp.DuplicateTagRefs != 2 {
		t.Errorf("duplicate tag refs %d, want 2", insp.DuplicateTagRefs)
	}
	// A Case with one DISTINCT tag is not multi-behavior, however many times
	// it repeats that tag.
	if insp.MultiBehaviorDevCases != 0 {
		t.Errorf("multi-behavior dev Cases %d, want 0", insp.MultiBehaviorDevCases)
	}
}

// TestInspectSeparableEffectIsTheTwoSidedBound pins the number and its
// sidedness. The one-sided figure would be smaller, which is the direction
// that overstates power.
func TestInspectSeparableEffectIsTheTwoSidedBound(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, devCases(12, "billing")...)

	b := insp.Behaviors[0]
	want := interval.MinDetectableEffect(12,
		knov1.Sidedness_SIDEDNESS_TWO_SIDED, interval.DefaultLevel)
	// The reported figure is rounded to four places at the source, because
	// the bisection behind it runs through math.Exp and math.Log and its tail
	// digits differ by architecture. The tolerance is that rounding and
	// nothing more: the one-sided bound this test rules out sits far outside
	// it, so the assertion still distinguishes the two sidednesses.
	if math.Abs(b.SeparableEffect-want) > 5e-5 {
		t.Fatalf("separable effect %.10f, want the two-sided bound %.10f", b.SeparableEffect, want)
	}
	oneSided := interval.MinDetectableEffect(12,
		knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel)
	if b.SeparableEffect <= oneSided {
		t.Fatalf("separable effect %.10f is not wider than the one-sided %.10f; "+
			"reporting the one-sided figure for a symmetric question overstates power",
			b.SeparableEffect, oneSided)
	}

	// The label is part of the number.
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), "SEPARABLE EFFECT (two-sided 95%)") {
		t.Error("the human column header does not name the sidedness")
	}
	doc := insp.jsonReport()
	if doc.Behaviors[0].Sidedness != sidednessTwoSided {
		t.Errorf("--json sidedness %q, want %q", doc.Behaviors[0].Sidedness, sidednessTwoSided)
	}
	if doc.Behaviors[0].Level != interval.DefaultLevel {
		t.Errorf("--json level %v, want %v", doc.Behaviors[0].Level, interval.DefaultLevel)
	}
}

// TestInspectPowerThresholdReadsMinClusterCases walks the boundary. The
// expectations are computed from core.MinClusterCases, so changing the
// constant changes the verdicts rather than breaking the test.
func TestInspectPowerThresholdReadsMinClusterCases(t *testing.T) {
	t.Parallel()

	for _, n := range []int{core.MinClusterCases - 1, core.MinClusterCases, core.MinClusterCases + 1} {
		t.Run(fmt.Sprintf("%d dev Cases", n), func(t *testing.T) {
			t.Parallel()
			insp := inspectFixture(t, devCases(n, "billing")...)

			want := statusOK
			wantCheck := statusOK
			if n < core.MinClusterCases {
				want, wantCheck = statusUnderpowered, statusFlagged
			}
			if got := insp.Behaviors[0].Status; got != want {
				t.Errorf("behavior status %q, want %q", got, want)
			}
			if got := checkStatus(insp, checkBehaviorsPowered); got != wantCheck {
				t.Errorf("%s = %q, want %q", checkBehaviorsPowered, got, wantCheck)
			}
		})
	}
}

// TestInspectFlagsAnUntaggedEvalSetNamingTheRoutingMode: no dev Case carries a
// tag, so cluster() returns ModeAllFailed and per-behavior attribution does
// not happen for the whole run. Zero behaviors, not an error.
func TestInspectFlagsAnUntaggedEvalSetNamingTheRoutingMode(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, devCase("c1"), devCase("c2"), devCase("c3"))

	if len(insp.Behaviors) != 0 {
		t.Fatalf("got %d behaviors, want 0", len(insp.Behaviors))
	}
	var declared check
	for _, c := range insp.Checks {
		if c.Name == checkBehaviorsDeclared {
			declared = c
		}
	}
	if declared.Status != statusFlagged {
		t.Errorf("%s = %q, want %q", checkBehaviorsDeclared, declared.Status, statusFlagged)
	}
	if !strings.Contains(declared.Detail, value.ModeAllFailed.String()) {
		t.Errorf("detail %q does not name the routing mode %q",
			declared.Detail, value.ModeAllFailed.String())
	}
	// behaviors_powered has nothing to assess. UNKNOWN, never ok.
	if got := checkStatus(insp, checkBehaviorsPowered); got != statusUnknown {
		t.Errorf("%s = %q, want %q with no behaviors", checkBehaviorsPowered, got, statusUnknown)
	}
	// The other checks still run, and rendering must not panic on an empty
	// behavior table.
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), "No dev Case carries a behavior tag.") {
		t.Errorf("the human rendering does not say the table is empty:\n%s", out.String())
	}
}

// TestInspectConcentrationIsNonExclusiveOverAllDevCases pins the semantic the
// goldens depend on: numerator is dev Cases carrying the most common
// normalized tag, denominator is ALL dev Cases, membership non-exclusive.
func TestInspectConcentrationIsNonExclusiveOverAllDevCases(t *testing.T) {
	t.Parallel()

	t.Run("every Case carries the dominant tag plus a second", func(t *testing.T) {
		t.Parallel()
		insp := inspectFixture(
			t,
			devCase("c1", "overall_quality", "billing"),
			devCase("c2", "overall_quality", "refunds"),
			devCase("c3", "overall_quality", "shipping"),
			devCase("c4", "overall_quality", "account"),
		)
		d := insp.dominant()
		if d == nil || d.Tag != "overall_quality" {
			t.Fatalf("dominant behavior %+v, want overall_quality", d)
		}
		if got := insp.share(d.DevCases); got != 1 {
			t.Errorf("concentration %.4f, want 1.0: membership is non-exclusive", got)
		}
		// The multi-behavior share is 100% too, and is REPORTED, not flagged.
		if got := insp.share(insp.MultiBehaviorDevCases); got != 1 {
			t.Errorf("multi-behavior share %.4f, want 1.0", got)
		}
	})

	t.Run("half the dev Cases untagged", func(t *testing.T) {
		t.Parallel()
		insp := inspectFixture(
			t,
			devCase("c1", "billing"),
			devCase("c2", "billing"),
			devCase("c3"),
			devCase("c4"),
		)
		d := insp.dominant()
		if got := insp.share(d.DevCases); got != 0.5 {
			t.Errorf("concentration %.4f, want 0.5 over ALL dev Cases, not 1.0 over tagged ones", got)
		}
		if got := insp.share(insp.UntaggedDevCases); got != 0.5 {
			t.Errorf("untagged share %.4f, want 0.5 over the same denominator", got)
		}
		// Neither exceeds half, so nothing flags.
		if got := checkStatus(insp, checkBehaviorConcentration); got != statusOK {
			t.Errorf("%s = %q at exactly half, want %q", checkBehaviorConcentration, got, statusOK)
		}
	})

	t.Run("the rendered line never says only", func(t *testing.T) {
		t.Parallel()
		insp := inspectFixture(
			t,
			devCase("c1", "overall_quality"),
			devCase("c2", "overall_quality"),
			devCase("c3", "overall_quality"),
			devCase("c4", "billing"),
		)
		var out strings.Builder
		if err := renderEvalInspect(&out, insp); err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out.String(), `"overall_quality" is carried by 75% of dev Cases`) {
			t.Errorf("the concentration line is not the pinned wording:\n%s", out.String())
		}
		if strings.Contains(out.String(), "is the only behavior on") {
			t.Error("the concentration line implies exclusivity, which the definition does not")
		}
	})
}

// TestInspectConcentrationBoundary walks 49 / 50 / 51 percent.
func TestInspectConcentrationBoundary(t *testing.T) {
	t.Parallel()

	// 100 dev Cases so a percentage is exact.
	for _, n := range []int{49, 50, 51} {
		t.Run(fmt.Sprintf("%d of 100 dev Cases", n), func(t *testing.T) {
			t.Parallel()
			cases := make([]*core.Case, 0, 100)
			for i := range 100 {
				tag := "other-" + fmt.Sprint(i)
				if i < n {
					tag = "overall_quality"
				}
				cases = append(cases, devCase(fmt.Sprintf("c%03d", i), tag))
			}
			insp := inspectFixture(t, cases...)

			want := statusOK
			if float64(n)/100 > concentrationFlagShare {
				want = statusFlagged
			}
			if got := checkStatus(insp, checkBehaviorConcentration); got != want {
				t.Errorf("%s at %d%% = %q, want %q", checkBehaviorConcentration, n, got, want)
			}
		})
	}
}

// TestInspectHoldoutThresholdReadsMinHoldout walks 19 / 20 / 21 against
// split.MinHoldout and asserts agreement with Counts.Underpowered().
func TestInspectHoldoutThresholdReadsMinHoldout(t *testing.T) {
	t.Parallel()

	for _, holdout := range []int{split.MinHoldout - 1, split.MinHoldout, split.MinHoldout + 1} {
		t.Run(fmt.Sprintf("holdout %d", holdout), func(t *testing.T) {
			t.Parallel()
			insp := &inspection{
				Counts:   split.Counts{Dev: 100, Holdout: holdout, HoldoutFrac: 0.2},
				DevCases: 100,
			}
			got := holdoutCheck(insp)
			want := statusOK
			if insp.Counts.Underpowered() {
				want = statusFlagged
			}
			if got.Status != want {
				t.Errorf("%s = %q, want %q (Counts.Underpowered() = %v)",
					checkHoldoutPowered, got.Status, want, insp.Counts.Underpowered())
			}
			if holdout < split.MinHoldout && got.Status != statusFlagged {
				t.Errorf("a holdout of %d is below split.MinHoldout (%d) and must flag",
					holdout, split.MinHoldout)
			}
		})
	}

	// A ZERO holdout is not "underpowered" — Counts' own godoc says it is
	// invalid and Validate says so — but it must not read as ok either.
	t.Run("no holdout at all", func(t *testing.T) {
		t.Parallel()
		insp := &inspection{Counts: split.Counts{Dev: 100, HoldoutFrac: 0.2}, DevCases: 100}
		got := holdoutCheck(insp)
		if got.Status != statusFlagged {
			t.Errorf("a zero holdout = %q, want %q", got.Status, statusFlagged)
		}
		if !strings.Contains(got.Detail, "no holdout") {
			t.Errorf("detail %q does not name the refusal validate will make", got.Detail)
		}
	})
}

// TestInspectHasFiveFlaggableChecksAndNoBehaviorSeparation is the F3
// assertion: behavior_separation appears in no checks array, and sweeping the
// multi-behavior share from 0 to 1 moves checks_flagged by zero.
func TestInspectHasFiveFlaggableChecksAndNoBehaviorSeparation(t *testing.T) {
	t.Parallel()

	// Two fixtures identical except for the multi-behavior share: 0% and
	// 100%. Every other property is held constant — same dev count, same
	// dominant share, same behavior sizes above the power floor.
	const n = 10
	var noneMulti, allMulti []*core.Case
	for i := range n {
		id := fmt.Sprintf("c%02d", i)
		noneMulti = append(noneMulti, devCase(id, "billing"))
		allMulti = append(allMulti, devCase(id, "billing", "refunds"))
	}

	a := inspectFixture(t, noneMulti...)
	b := inspectFixture(t, allMulti...)

	if a.share(a.MultiBehaviorDevCases) != 0 {
		t.Fatalf("fixture A multi-behavior share %.2f, want 0", a.share(a.MultiBehaviorDevCases))
	}
	if b.share(b.MultiBehaviorDevCases) != 1 {
		t.Fatalf("fixture B multi-behavior share %.2f, want 1", b.share(b.MultiBehaviorDevCases))
	}
	if a.flaggedCount() != b.flaggedCount() {
		t.Errorf("checks_flagged moved from %d to %d when the multi-behavior share went "+
			"0%% to 100%%; the share is reported and must never flag",
			a.flaggedCount(), b.flaggedCount())
	}

	for _, insp := range []*inspection{a, b} {
		doc := insp.jsonReport()
		if doc.ChecksTotal != checksTotal {
			t.Errorf("checks_total = %d, want checksTotal (%d)", doc.ChecksTotal, checksTotal)
		}
		if checksTotal != 5 {
			t.Errorf("checksTotal = %d, want 5: behavior_separation is not a check", checksTotal)
		}
		if len(doc.Checks) != 5 {
			t.Errorf("the checks array has %d entries, want 5", len(doc.Checks))
		}
		for _, c := range doc.Checks {
			if strings.Contains(c.Name, "separation") {
				t.Errorf("check %q exists; behavior_separation is reported, never a check", c.Name)
			}
		}
		// The share is still emitted.
		if doc.MultiBehaviorDevCases != insp.MultiBehaviorDevCases {
			t.Error("multi_behavior_dev_cases is missing from --json")
		}
	}
}

// TestInspectAttributionObservedIsUnknownWithoutARun: a check that needs data
// it was not given reports UNKNOWN, never ok.
func TestInspectAttributionObservedIsUnknownWithoutARun(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, devCase("c1", "billing"))
	insp.setObservedCheck(nil, "no --value-run-id given")

	if got := checkStatus(insp, checkAttributionObserved); got != statusUnknown {
		t.Fatalf("%s = %q without a run, want %q", checkAttributionObserved, got, statusUnknown)
	}
	doc := insp.jsonReport()
	if doc.Observed != nil {
		t.Error("--json carries an observed object with no run named")
	}
	if doc.ChecksTotal != 5 {
		t.Errorf("checks_total = %d without a run, want 5", doc.ChecksTotal)
	}
}

// TestInspectStandingConditionalAppearsOnceAndFirst is the F1 assertion.
func TestInspectStandingConditionalAppearsOnceAndFirst(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(
		t,
		devCase("c1", "p0"),
		devCase("c2", "regression-2024"),
		devCase("c3", "source:zendesk"),
	)

	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	text := out.String()

	const conditionalMarker = "Everything below reads your tags as behaviors"
	if n := strings.Count(text, conditionalMarker); n != 1 {
		t.Errorf("the standing conditional appears %d times, want exactly 1", n)
	}
	// Before any per-tag number: the table header must come after it.
	condAt := strings.Index(text, conditionalMarker)
	tableAt := strings.Index(text, "BEHAVIOR")
	if condAt < 0 || tableAt < 0 || condAt > tableAt {
		t.Errorf("the conditional is at %d and the behavior table at %d; "+
			"it must come first", condAt, tableAt)
	}
	// The suggestions block carries it too, because that is where the
	// directive advice lands.
	if !strings.Contains(text, suggestionsHeader) {
		t.Error("the suggestions block does not carry the conditional header")
	}
	suggAt := strings.Index(text, suggestionsHeader)
	for _, s := range inspectSuggestions(insp) {
		// Every suggestion's first word must appear after the header.
		word := strings.Fields(s)[0]
		if at := strings.Index(text[suggAt:], word); at < 0 {
			t.Errorf("suggestion %q is emitted outside the conditional framing", s)
		}
	}

	doc := insp.jsonReport()
	if len(doc.Notes) == 0 || doc.Notes[0] != standingConditional {
		t.Errorf("--json notes[0] = %q, want the standing conditional", doc.Notes)
	}
}

// TestInspectRefusesAnEmptyEvalSet: "0 behaviors, 0 checks flagged" would be
// a confident report about nothing.
func TestInspectRefusesAnEmptyEvalSet(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	src, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("jsonl.New: %v", err)
	}
	if _, err := inspectEvals(context.Background(), src, path); err == nil {
		t.Fatal("an empty eval set was reported rather than refused")
	} else if !strings.Contains(err.Error(), "--evals") {
		t.Errorf("the refusal does not name --evals: %v", err)
	}
}

// TestInspectIsDeterministicAcrossMapIterationOrder: the same file inspected
// twice must be byte-identical. Behaviors come out of a map, so ordering is
// the risk.
func TestInspectIsDeterministicAcrossMapIterationOrder(t *testing.T) {
	t.Parallel()

	cases := make([]*core.Case, 0, 60)
	for i := range 60 {
		// Several tags sharing a dev-case count, so the tie-break is exercised.
		cases = append(cases, devCase(fmt.Sprintf("c%03d", i), fmt.Sprintf("tag-%d", i%12)))
	}

	var first string
	for range 8 {
		insp := inspectFixture(t, cases...)
		var out strings.Builder
		if err := renderEvalInspect(&out, insp); err != nil {
			t.Fatalf("render: %v", err)
		}
		if first == "" {
			first = out.String()
			continue
		}
		if out.String() != first {
			t.Fatal("two inspections of the same file rendered differently")
		}
	}
}

// TestInspectTruncatesAHugeBehaviorTableAndSaysSo: one Case with 500 tags.
func TestInspectTruncatesAHugeBehaviorTableAndSaysSo(t *testing.T) {
	t.Parallel()

	tags := make([]string, 0, 500)
	for i := range 500 {
		tags = append(tags, fmt.Sprintf("tag-%03d", i))
	}
	insp := inspectFixture(t, devCase("c1", tags...))

	if len(insp.Behaviors) != 500 {
		t.Fatalf("got %d behaviors, want 500", len(insp.Behaviors))
	}
	for _, b := range insp.Behaviors {
		if b.Status != statusUnderpowered {
			t.Fatalf("behavior %q with 1 dev Case is %q, want %q", b.Tag, b.Status, statusUnderpowered)
		}
	}
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	want := fmt.Sprintf("…and %d more (see --json)", 500-behaviorTableLimit)
	if !strings.Contains(out.String(), want) {
		t.Errorf("the truncated table does not say %q", want)
	}
	// --json carries all of them.
	if got := len(insp.jsonReport().Behaviors); got != 500 {
		t.Errorf("--json carries %d behaviors, want all 500", got)
	}
}

// TestInspectHandlesAnAllHoldoutEvalSet: dev count 0, empty behavior table,
// behaviors_declared flagged, holdout_powered still evaluated.
func TestInspectHandlesAnAllHoldoutEvalSet(t *testing.T) {
	t.Parallel()

	cases := make([]*core.Case, 0, 30)
	for i := range 30 {
		cases = append(cases, holdoutCase(fmt.Sprintf("c%03d", i), "billing"))
	}
	insp := inspectFixture(t, cases...)

	if insp.DevCases != 0 {
		t.Fatalf("the fixture left %d dev Cases; every Case is holdout", insp.DevCases)
	}
	if len(insp.Behaviors) != 0 {
		t.Errorf("got %d behaviors over zero dev Cases", len(insp.Behaviors))
	}
	if got := checkStatus(insp, checkBehaviorsDeclared); got != statusFlagged {
		t.Errorf("%s = %q, want %q", checkBehaviorsDeclared, got, statusFlagged)
	}
	if got := checkStatus(insp, checkBehaviorConcentration); got != statusUnknown {
		t.Errorf("%s = %q with no dev Cases, want %q", checkBehaviorConcentration, got, statusUnknown)
	}
	if got := checkStatus(insp, checkHoldoutPowered); got == "" {
		t.Error("holdout_powered was not evaluated")
	}
	// share must not divide by zero.
	if got := insp.share(0); got != 0 || math.IsNaN(got) {
		t.Errorf("share over zero dev Cases = %v, want 0", got)
	}
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
}

// TestInspectCountsUnscoreableDevCases: exact-match scores a Case with
// neither expected nor rubric as a failure by construction, so the count is
// worth printing.
func TestInspectCountsUnscoreableDevCases(t *testing.T) {
	t.Parallel()

	noExpectation := devCase("c1", "billing")
	noExpectation.Expected, noExpectation.Rubric = "", ""
	rubricOnly := devCase("c3", "billing")
	rubricOnly.Expected = ""

	insp := inspectFixture(t, noExpectation, devCase("c2", "billing"), rubricOnly)

	if insp.UnscoreableDevCases != 1 {
		t.Errorf("unscoreable dev Cases %d, want 1", insp.UnscoreableDevCases)
	}
	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), "neither expected nor rubric") {
		t.Errorf("the unscoreable count is not reported:\n%s", out.String())
	}
}

// TestFingerprintMatchPrefersTheRunsOwnRecordAndHasThreeAnswers pins the join
// rule, including the third answer: a run with no fingerprint at all cannot
// be joined and must not read as a match.
func TestFingerprintMatchPrefersTheRunsOwnRecordAndHasThreeAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  *knov1.Run
		hash string
		want fingerprintVerdict
	}{
		{
			name: "baseline-style eval_content_hash matches",
			run:  &knov1.Run{EvalContentHash: "h1", InputFingerprint: "other"},
			hash: "h1", want: fingerprintMatches,
		},
		{
			name: "baseline-style eval_content_hash differs and wins over the fallback",
			run:  &knov1.Run{EvalContentHash: "h1", InputFingerprint: "h2"},
			hash: "h2", want: fingerprintDiffers,
		},
		{
			name: "value-style input_fingerprint matches",
			run:  &knov1.Run{InputFingerprint: "h1"},
			hash: "h1", want: fingerprintMatches,
		},
		{
			name: "value-style input_fingerprint differs",
			run:  &knov1.Run{InputFingerprint: "h1"},
			hash: "h2", want: fingerprintDiffers,
		},
		{
			name: "no fingerprint recorded at all",
			run:  &knov1.Run{},
			hash: "h1", want: fingerprintAbsent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fingerprintMatch(tt.run, tt.hash); got != tt.want {
				t.Errorf("fingerprintMatch = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFailedCountCountsOnlyRecoverableFailures: a score that exists but
// cannot be read back is not a confirmed failure, and reporting the cluster
// size instead would dress an absence up as a confirmation.
func TestFailedCountCountsOnlyRecoverableFailures(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c", "d"}
	scores := map[string]store.CaseScore{
		"a": {Passed: false},                      // counts
		"b": {Passed: true},                       // passed
		"c": {Passed: false, Unrecoverable: true}, // number is gone
		// "d" is absent from the record entirely.
	}
	if got := failedCount(ids, scores); got != 1 {
		t.Errorf("failedCount = %d, want 1", got)
	}
	if got := failedCount(ids, nil); got != 0 {
		t.Errorf("failedCount with no baseline record = %d, want 0", got)
	}
}

// TestGapStatusNameCoversEveryVerdict: an unnamed verdict would render blank,
// and a blank verdict is one a reader fills in themselves.
func TestGapStatusNameCoversEveryVerdict(t *testing.T) {
	t.Parallel()

	tests := map[knov1.GapStatus]string{
		knov1.GapStatus_GAP_STATUS_IMPROVED:    "improved",
		knov1.GapStatus_GAP_STATUS_GAP:         "gap",
		knov1.GapStatus_GAP_STATUS_UNKNOWN:     "unknown",
		knov1.GapStatus_GAP_STATUS_UNSPECIFIED: "unknown",
		knov1.GapStatus(99):                    "unknown",
	}
	for s, want := range tests {
		if got := gapStatusName(s); got != want {
			t.Errorf("gapStatusName(%v) = %q, want %q", s, got, want)
		}
	}
}

// TestMinHoldoutCasesUsesTheConfiguredFraction: a user at 0.05 told they need
// 100 Cases when the true answer is 400 has been given a fix that does not fix.
func TestMinHoldoutCasesUsesTheConfiguredFraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		frac float64
		want int
	}{
		{frac: 0.2, want: split.MinHoldout * 5},
		{frac: 0.05, want: split.MinHoldout * 20},
		{frac: 0, want: 0},
	}
	for _, tt := range tests {
		insp := &inspection{Counts: split.Counts{HoldoutFrac: tt.frac}}
		if got := minHoldoutCases(insp); got != tt.want {
			t.Errorf("minHoldoutCases at frac %.2f = %d, want %d", tt.frac, got, tt.want)
		}
	}
}

// TestObservedRenderingLabelsBothSidednesses is the F2 assertion in the
// output: separable_effect is two-sided and min_detectable_harm is one-sided,
// and both say so wherever they appear.
func TestObservedRenderingLabelsBothSidednesses(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, devCases(5, "refunds")...)
	insp.Observed = &observed{
		ValueRunID:        "v1",
		BaselineRunID:     "b1",
		RunStatus:         knov1.RunStatus_RUN_STATUS_COMPLETED,
		RoutingMode:       value.ModeTagOverlap.String(),
		ControlCases:      20,
		MinDetectableHarm: interval.MinDetectableEffect(20, knov1.Sidedness_SIDEDNESS_UPPER, interval.DefaultLevel),
		Behaviors: []observedBehavior{{
			Tag: "refunds", ClusterCases: 5, FailedAtBaseline: 5,
			GapStatus: knov1.GapStatus_GAP_STATUS_GAP, CoveredCount: 5,
		}},
	}
	insp.setObservedCheck(insp.Observed, "routing ran in tag-overlap mode over 1 clusters")

	var out strings.Builder
	if err := renderEvalInspect(&out, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "(two-sided 95%)") {
		t.Error("the separable-effect column does not carry its sidedness")
	}
	if !strings.Contains(text, "(one-sided 95%)") {
		t.Error("the minimum-detectable-harm line does not carry its sidedness")
	}

	doc := insp.jsonReport()
	if doc.Observed.MinDetectableHarmSidedness != sidednessOneSided {
		t.Errorf("min_detectable_harm_sidedness = %q, want %q",
			doc.Observed.MinDetectableHarmSidedness, sidednessOneSided)
	}
	for _, b := range doc.Behaviors {
		if b.Sidedness != sidednessTwoSided {
			t.Errorf("behavior %q sidedness = %q, want %q", b.Tag, b.Sidedness, sidednessTwoSided)
		}
	}
	// The two numbers must not be equal at the same n, or the labels are
	// decorating one figure.
	if doc.Observed.MinDetectableHarm == interval.MinDetectableEffect(
		20, knov1.Sidedness_SIDEDNESS_TWO_SIDED, interval.DefaultLevel,
	) {
		t.Error("the one-sided and two-sided figures agree at n=20, which they cannot")
	}
}

// TestInspectRendersNoCaseContentIncludingTurns is criterion 14 over the one
// field the jsonl file format cannot express.
//
// Every fixture Case carries the sentinel in its input, expected, rubric AND
// its turn content, so this holds every rendering to "tags, counts and IDs
// only" rather than to whichever fields a fixture happened to fill.
func TestInspectRendersNoCaseContentIncludingTurns(t *testing.T) {
	t.Parallel()

	insp := inspectFixture(t, append(
		devCases(8, "billing"),
		devCase("c-multi", "refunds", "billing"),
		holdoutCase("c-held", "shipping"),
	)...)

	var human strings.Builder
	if err := renderEvalInspect(&human, insp); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(human.String(), contentSentinel) {
		t.Errorf("Case content reached the human rendering:\n%s", human.String())
	}

	var jsonOut strings.Builder
	if err := writeJSON(&jsonOut, insp.jsonReport()); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if strings.Contains(jsonOut.String(), contentSentinel) {
		t.Errorf("Case content reached --json:\n%s", jsonOut.String())
	}
	// The holdout Case's tag is not reachable either: the analysis reads
	// through core.Seal.
	if strings.Contains(human.String(), "shipping") || strings.Contains(jsonOut.String(), "shipping") {
		t.Error("a holdout Case's tag reached the output; the seal was dropped from the read path")
	}
}

// TestInspectStopsAtAFatalIteratorError: a yielded error is fatal by the
// Evals.Cases contract, and no partial analysis may be returned.
func TestInspectStopsAtAFatalIteratorError(t *testing.T) {
	t.Parallel()

	src := &fixtureEvals{
		cases:    devCases(3, "billing"),
		hash:     "h",
		yieldErr: errFixtureFatal,
	}
	if _, err := inspectEvals(context.Background(), src, "fixture.jsonl"); err == nil {
		t.Fatal("a fatal iterator error was swallowed")
	} else if !strings.Contains(err.Error(), errFixtureFatal.Error()) {
		t.Errorf("the adapter's error did not surface: %v", err)
	}
}

// TestInspectPropagatesAnOpenFailure: the outer error reports a failure to
// OPEN the source, and it must reach the user with a fix.
func TestInspectPropagatesAnOpenFailure(t *testing.T) {
	t.Parallel()

	src := &fixtureEvals{cases: devCases(3, "billing"), openErr: errFixtureFatal}
	if _, err := inspectEvals(context.Background(), src, "fixture.jsonl"); err == nil {
		t.Fatal("a source that would not open was reported on anyway")
	}
}

// TestInspectHonorsCancellation: ctx cancellation propagates through the
// iterator's pre-yield check and nothing partial is returned.
func TestInspectHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fixtureEvals{cases: devCases(50, "billing"), hash: "h"}
	if _, err := inspectEvals(ctx, src, "fixture.jsonl"); err == nil {
		t.Fatal("a cancelled inspection returned an analysis")
	}
}

// errFixtureFatal is the fixture's fatal iterator error.
var errFixtureFatal = errors.New("fixture: the source went away mid-read")
