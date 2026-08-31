package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// inspectCase is one line of a test eval file.
type inspectCase struct {
	ID       string
	Tags     []string
	Input    string
	Expected string
	Rubric   string
}

// writeInspectCases renders cases as JSONL and returns the path.
//
// Rendered with strconv.Quote rather than encoding/json: ADR-0001's exemption
// is scoped to cli/jsonreport.go, and a test that reached for its own encoder
// would be widening a boundary the lint bundle exists to hold.
func writeInspectCases(t *testing.T, dir, name string, cases []inspectCase) string {
	t.Helper()
	var b strings.Builder
	for _, c := range cases {
		tags := make([]string, len(c.Tags))
		for n, tag := range c.Tags {
			tags[n] = jsonQuote(tag)
		}
		input := c.Input
		if input == "" {
			input = "q " + c.ID
		}
		fmt.Fprintf(&b, `{"id":%s,"input":%s,"expected":%s,"rubric":%s,"tags":[%s]}`+"\n",
			jsonQuote(c.ID), jsonQuote(input),
			jsonQuote(c.Expected), jsonQuote(c.Rubric), strings.Join(tags, ","))
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// jsonQuote renders a Go string as a JSON string literal.
//
// strconv.Quote is not a substitute: it emits \x1b for an escape character,
// which is a Go literal and not valid JSON, so a fixture carrying one would
// exercise the malformed-line path rather than the tag path.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// devHoldoutIDs returns Case IDs that land where the test needs them.
//
// Through split.AssignSplit, the same function the adapters use, so a test
// asking for "exactly 20 holdout Cases" gets exactly 20 rather than a number
// the hash happened to produce.
func devHoldoutIDs(t *testing.T, frac float64, wantDev, wantHoldout int) (dev, hold []string) {
	t.Helper()
	for n := 0; len(dev) < wantDev || len(hold) < wantHoldout; n++ {
		if n > 1_000_000 {
			t.Fatalf("could not find %d dev and %d holdout IDs at frac %v", wantDev, wantHoldout, frac)
		}
		id := fmt.Sprintf("case-%06d", n)
		if split.AssignSplit(id, "", frac) == knov1.Split_SPLIT_DEV {
			if len(dev) < wantDev {
				dev = append(dev, id)
			}
			continue
		}
		if len(hold) < wantHoldout {
			hold = append(hold, id)
		}
	}
	return dev, hold
}

// tagged builds one Case per ID, all carrying the same tags.
func tagged(ids []string, tags ...string) []inspectCase {
	out := make([]inspectCase, 0, len(ids))
	for _, id := range ids {
		out = append(out, inspectCase{ID: id, Tags: tags, Expected: "a"})
	}
	return out
}

// inspectJSON runs `kno eval inspect --json` and decodes the document.
func inspectJSON(t *testing.T, args ...string) cli.EvalInspectReport {
	t.Helper()
	stdout, stderr, code := run(t, append([]string{"eval", "inspect", "--json"}, args...)...)
	if code != errs.ExitOK {
		t.Fatalf("eval inspect --json exit = %d\nstderr: %s", code, stderr)
	}
	doc, err := cli.DecodeEvalInspect([]byte(stdout))
	if err != nil {
		t.Fatalf("stdout is not one inspect document: %v\n%s", err, stdout)
	}
	return doc
}

// checkStatus returns one check's status from a decoded document.
func checkStatus(t *testing.T, doc cli.EvalInspectReport, name string) string {
	t.Helper()
	for _, c := range doc.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	t.Fatalf("no check named %q in %v", name, doc.Checks)
	return ""
}

// TestInspectRunsWithoutARunAndExitsZero is criteria 1, 9 and 17: four checks
// answered from the eval file alone, the fifth UNKNOWN, and exit 0 regardless.
func TestInspectRunsWithoutARunAndExitsZero(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 30, 25)
	var cases []inspectCase
	for n, id := range dev {
		cases = append(cases, inspectCase{
			ID: id, Tags: []string{[]string{"billing", "refunds", "shipping"}[n%3]}, Expected: "a",
		})
	}
	cases = append(cases, tagged(hold, "billing")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	stdout, stderr, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"0 of 5 checks flagged.",
		"SEPARABLE EFFECT (two-sided 95%)",
		"Everything below reads your tags as behaviors",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the page does not say %q:\n%s", want, stdout)
		}
	}

	doc := inspectJSON(t, "--evals", path)
	if doc.ChecksTotal != 5 {
		t.Errorf("checks_total = %d, want 5 — behavior_separation is not a check", doc.ChecksTotal)
	}
	if got := checkStatus(t, doc, "attribution_observed"); got != "unknown" {
		t.Errorf("attribution_observed = %q without --value-run-id, want unknown", got)
	}
}

// TestInspectExitsZeroWithEverythingFlagged is criterion 17. A diagnostic that
// exits non-zero on a finding is unrunnable in the pre-commit position where it
// is most useful, and it would collide with ExitError's meaning.
func TestInspectExitsZeroWithEverythingFlagged(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 8, 2)
	cases := append(tagged(dev[:6], "everything"), tagged(dev[6:], "rare")...)
	cases = append(cases, tagged(hold, "everything")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if doc.ChecksFlagged == 0 {
		t.Fatalf("the fixture was meant to flag; it flagged nothing: %v", doc.Checks)
	}
	_, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Errorf("exit = %d with %d checks flagged, want 0", code, doc.ChecksFlagged)
	}
}

// TestInspectCollapsesSpellingsAndReportsIt is criterion 2. The collapse must be
// routing's own, and it must be VISIBLE: a user who believes they have eight
// behaviors while routing sees six needs that line more than any other.
func TestInspectCollapsesSpellingsAndReportsIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dev, hold := devHoldoutIDs(t, 0.2, 9, 25)
	var cases []inspectCase
	for n, id := range dev {
		spelling := []string{"Refunds", "refunds", " refunds "}[n%3]
		cases = append(cases, inspectCase{ID: id, Tags: []string{spelling}, Expected: "a"})
	}
	cases = append(cases, tagged(hold, "refunds")...)
	path := writeInspectCases(t, dir, "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if len(doc.Behaviors) != 1 {
		t.Fatalf("three spellings produced %d behaviors, want 1: %v", len(doc.Behaviors), doc.Behaviors)
	}
	if doc.Behaviors[0].Tag != "refunds" {
		t.Errorf("the behavior is %q, want the normalized %q", doc.Behaviors[0].Tag, "refunds")
	}
	if doc.Behaviors[0].Spellings != 3 {
		t.Errorf("spellings = %d, want 3", doc.Behaviors[0].Spellings)
	}
	stdout, _, _ := run(t, "eval", "inspect", "--evals", path)
	if !strings.Contains(stdout, `3 spellings collapsed into "refunds"`) {
		t.Errorf("the collapse is not reported:\n%s", stdout)
	}
}

// TestInspectSkipsBlankTagsAndCountsDuplicatesOnce is criteria 3 and 4,
// matching cluster()'s key == "" skip and snapshotClusters' NDropped.
func TestInspectSkipsBlankTagsAndCountsDuplicatesOnce(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 6, 25)
	var cases []inspectCase
	for _, id := range dev {
		cases = append(cases, inspectCase{ID: id, Tags: []string{"billing", "", "  ", "billing"}, Expected: "a"})
	}
	cases = append(cases, tagged(hold, "billing")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if len(doc.Behaviors) != 1 || doc.Behaviors[0].DevCases != 6 {
		t.Fatalf("blank and duplicate tags leaked into the behaviors: %v", doc.Behaviors)
	}
	if doc.BlankTagRefs != 12 {
		t.Errorf("blank_tag_refs = %d, want 12 — a skipped tag is reported, not dropped", doc.BlankTagRefs)
	}
	if doc.DuplicateTagRefs != 6 {
		t.Errorf("duplicate_tag_refs = %d, want 6", doc.DuplicateTagRefs)
	}
	if doc.MultiBehaviorDevCases != 0 {
		t.Errorf("a Case tagged [billing, billing] is not multi-behavior; got %d",
			doc.MultiBehaviorDevCases)
	}
}

// TestInspectPowerThresholdReadsTheConstant is criterion 6: the verdict turns on
// core.MinClusterCases, read from the constant, so changing the constant changes
// the expectation rather than breaking the test.
func TestInspectPowerThresholdReadsTheConstant(t *testing.T) {
	t.Parallel()

	for _, delta := range []int{-1, 0, 1} {
		size := core.MinClusterCases + delta
		t.Run(fmt.Sprintf("dev_cases_%d", size), func(t *testing.T) {
			t.Parallel()
			dev, hold := devHoldoutIDs(t, 0.2, size, 25)
			path := writeInspectCases(t, t.TempDir(), "cases.jsonl",
				append(tagged(dev, "billing"), tagged(hold, "billing")...))

			doc := inspectJSON(t, "--evals", path)
			want := "ok"
			if size < core.MinClusterCases {
				want = "flagged"
			}
			if got := doc.Behaviors[0].Status; got != want {
				t.Errorf("a behavior of %d dev Cases is %q, want %q (core.MinClusterCases = %d)",
					size, got, want, core.MinClusterCases)
			}
			if got := checkStatus(t, doc, "behaviors_powered"); got != want {
				t.Errorf("behaviors_powered = %q, want %q", got, want)
			}
		})
	}
}

// TestInspectHoldoutThresholdReadsTheConstant is criterion 8: split.MinHoldout,
// and agreement with Counts.Underpowered at every size above zero.
func TestInspectHoldoutThresholdReadsTheConstant(t *testing.T) {
	t.Parallel()

	for _, delta := range []int{-1, 0, 1} {
		size := split.MinHoldout + delta
		t.Run(fmt.Sprintf("holdout_%d", size), func(t *testing.T) {
			t.Parallel()
			dev, hold := devHoldoutIDs(t, 0.2, 20, size)
			path := writeInspectCases(t, t.TempDir(), "cases.jsonl",
				append(tagged(dev, "billing"), tagged(hold, "billing")...))

			doc := inspectJSON(t, "--evals", path)
			if doc.Cases.Holdout != size {
				t.Fatalf("the fixture produced %d holdout Cases, not %d", doc.Cases.Holdout, size)
			}
			want := "ok"
			if (split.Counts{Dev: 20, Holdout: size}).Underpowered() {
				want = "flagged"
			}
			if got := checkStatus(t, doc, "holdout_powered"); got != want {
				t.Errorf("holdout_powered at %d Cases = %q, want %q (split.MinHoldout = %d)",
					size, got, want, split.MinHoldout)
			}
		})
	}
}

// TestInspectFlagsNoTagsAtAll is criterion 7: the DEFAULT STATE of a real eval
// file. Zero behaviors, not an error, and the detail names the mode routing
// would actually fall back to.
func TestInspectFlagsNoTagsAtAll(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 30, 25)
	var cases []inspectCase
	for _, id := range append(append([]string{}, dev...), hold...) {
		cases = append(cases, inspectCase{ID: id, Expected: "a"})
	}
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if len(doc.Behaviors) != 0 {
		t.Errorf("an untagged eval file reports %d behaviors, want 0", len(doc.Behaviors))
	}
	if got := checkStatus(t, doc, "behaviors_declared"); got != "flagged" {
		t.Errorf("behaviors_declared = %q, want flagged", got)
	}
	if got := checkStatus(t, doc, "behaviors_powered"); got != "unknown" {
		t.Errorf("behaviors_powered = %q with no behaviors, want unknown — not a soft pass", got)
	}
	if doc.DominantBehavior != nil {
		t.Errorf("dominant_behavior is present with no tags: %+v", doc.DominantBehavior)
	}
	var detail string
	for _, c := range doc.Checks {
		if c.Name == "behaviors_declared" {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "all-failed") && !strings.Contains(detail, "ModeAllFailed") {
		t.Errorf("behaviors_declared's detail does not name the routing fallback: %q", detail)
	}
	stdout, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("an untagged eval file must not be an error; exit = %d", code)
	}
	if !strings.Contains(stdout, "No dev Case carries a tag") {
		t.Errorf("the page does not say the file is untagged:\n%s", stdout)
	}
}

// TestMultiBehaviorShareNeverMovesTheFlagCount is criterion 25 (F3).
//
// Two fixtures with IDENTICAL tag membership — three behaviors, ten dev Cases
// each — differing only in how those memberships are distributed across Cases:
// one where no Case carries two tags, one where half of them carry two. The
// share moves from 0 to 0.5 and checks_flagged does not move at all, because
// behavior_separation is not a check. It is also absent from the checks array.
func TestMultiBehaviorShareNeverMovesTheFlagCount(t *testing.T) {
	t.Parallel()

	tags := []string{"billing", "refunds", "shipping"}
	dev, hold := devHoldoutIDs(t, 0.2, 30, 25)

	// separate: Cases 0-9 carry billing, 10-19 refunds, 20-29 shipping.
	separate := make([]inspectCase, 0, 30)
	for n, id := range dev {
		separate = append(separate, inspectCase{ID: id, Tags: []string{tags[n/10]}, Expected: "a"})
	}
	// shared: Cases 0-14 carry two tags each, 15-29 carry none. Every tag still
	// has exactly ten dev Cases, so every other check sees the same eval set.
	pairs := [][]string{{tags[0], tags[1]}, {tags[1], tags[2]}, {tags[2], tags[0]}}
	shared := make([]inspectCase, 0, 30)
	for n, id := range dev {
		c := inspectCase{ID: id, Expected: "a"}
		if n < 15 {
			c.Tags = pairs[n/5]
		}
		shared = append(shared, c)
	}

	var flagged []int
	for name, cases := range map[string][]inspectCase{"separate": separate, "shared": shared} {
		path := writeInspectCases(t, t.TempDir(), "cases.jsonl", append(cases, tagged(hold, tags[0])...))
		doc := inspectJSON(t, "--evals", path)
		for _, c := range doc.Checks {
			if c.Name == "behavior_separation" {
				t.Fatalf("behavior_separation is in the checks array; it must report and never flag")
			}
		}
		if len(doc.Behaviors) != 3 {
			t.Fatalf("%s: %d behaviors, want 3 — the two fixtures must differ only in sharing",
				name, len(doc.Behaviors))
		}
		for _, b := range doc.Behaviors {
			if b.DevCases != 10 {
				t.Fatalf("%s: %q has %d dev Cases, want 10", name, b.Tag, b.DevCases)
			}
		}
		want := 0.0
		if name == "shared" {
			want = 0.5
		}
		if doc.MultiBehaviorShare != want {
			t.Errorf("%s: multi_behavior_share = %v, want %v", name, doc.MultiBehaviorShare, want)
		}
		flagged = append(flagged, doc.ChecksFlagged)
	}
	for _, n := range flagged[1:] {
		if n != flagged[0] {
			t.Errorf("checks_flagged moved with the multi-behavior share: %v", flagged)
		}
	}
}

// TestConcentrationIsNonExclusiveOverAllDevCases is criterion 26 (F4).
//
// Every Case carries the dominant tag AND a second one, so an exclusivity-based
// concentration would report 0% and a non-exclusive one reports 100%. The
// engine is non-exclusive — cluster() puts a multi-tagged failed Case in EVERY
// one of its clusters — so this must be too.
func TestConcentrationIsNonExclusiveOverAllDevCases(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 10, 25)
	var cases []inspectCase
	for n, id := range dev {
		cases = append(cases, inspectCase{
			ID: id, Tags: []string{"overall_quality", fmt.Sprintf("area-%d", n%2)}, Expected: "a",
		})
	}
	cases = append(cases, tagged(hold, "overall_quality")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if doc.DominantBehavior == nil || doc.DominantBehavior.Share != 1 {
		t.Fatalf("concentration is %+v, want a share of 1 — membership is non-exclusive",
			doc.DominantBehavior)
	}
	stdout, _, _ := run(t, "eval", "inspect", "--evals", path)
	if strings.Contains(stdout, "is the only behavior on") {
		t.Errorf("the concentration line claims exclusivity it does not measure:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"overall_quality" is carried by 100% of dev Cases`) {
		t.Errorf("the concentration line does not read \"carried by\":\n%s", stdout)
	}
}

// TestConcentrationDenominatorIsAllDevCases is the other half of F4: half the
// dev Cases untagged, the rest all carrying one tag. Concentration is 50% over
// ALL dev Cases, not 100% over the tagged ones, and the untagged share is the
// same 50% — so the two lines can be read against each other.
func TestConcentrationDenominatorIsAllDevCases(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 10, 25)
	var cases []inspectCase
	for n, id := range dev {
		c := inspectCase{ID: id, Expected: "a"}
		if n%2 == 0 {
			c.Tags = []string{"billing"}
		}
		cases = append(cases, c)
	}
	cases = append(cases, tagged(hold, "billing")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	doc := inspectJSON(t, "--evals", path)
	if doc.DominantBehavior.Share != 0.5 {
		t.Errorf("concentration = %v, want 0.5 over all dev Cases", doc.DominantBehavior.Share)
	}
	if doc.UntaggedDevCases != 5 {
		t.Errorf("untagged_dev_cases = %d, want 5", doc.UntaggedDevCases)
	}
	if got := checkStatus(t, doc, "behavior_concentration"); got != "ok" {
		t.Errorf("behavior_concentration = %q at exactly half, want ok — the flag is ABOVE half", got)
	}
}

// TestInspectIsDeterministic is criterion 19: the same file inspected twice is
// byte-identical, in both renderings. Map iteration order must never reach the
// output.
func TestInspectIsDeterministic(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 40, 25)
	var cases []inspectCase
	for n, id := range dev {
		cases = append(cases, inspectCase{
			ID:       id,
			Tags:     []string{fmt.Sprintf("tag-%02d", n%7), fmt.Sprintf("area-%02d", n%3)},
			Expected: "a",
		})
	}
	cases = append(cases, tagged(hold, "tag-00")...)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	for _, args := range [][]string{
		{"eval", "inspect", "--evals", path},
		{"eval", "inspect", "--evals", path, "--json"},
	} {
		first, _, _ := run(t, args...)
		for range 4 {
			again, _, _ := run(t, args...)
			if again != first {
				t.Fatalf("%v is not deterministic", args)
			}
		}
	}
}

// TestInspectPrintsNoCaseContent is criterion 14.
//
// Every Case's input, expected and rubric is a unique sentinel; none may appear
// anywhere in stdout, stderr, or the JSON document. inspect prints tags, counts
// and IDs — never content.
func TestInspectPrintsNoCaseContent(t *testing.T) {
	t.Parallel()

	dev, hold := devHoldoutIDs(t, 0.2, 12, 6)
	var cases []inspectCase
	var sentinels []string
	for n, id := range append(append([]string{}, dev...), hold...) {
		in := fmt.Sprintf("SENTINELINPUT%03d", n)
		exp := fmt.Sprintf("SENTINELEXPECTED%03d", n)
		rub := fmt.Sprintf("SENTINELRUBRIC%03d", n)
		sentinels = append(sentinels, in, exp, rub)
		cases = append(cases, inspectCase{ID: id, Tags: []string{"billing"}, Input: in, Expected: exp, Rubric: rub})
	}
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", cases)

	for _, args := range [][]string{
		{"eval", "inspect", "--evals", path},
		{"eval", "inspect", "--evals", path, "--json"},
	} {
		stdout, stderr, _ := run(t, args...)
		for _, s := range sentinels {
			if strings.Contains(stdout, s) || strings.Contains(stderr, s) {
				t.Fatalf("%v leaked Case content (%s)", args, s)
			}
		}
	}
}

// TestInspectNeverReadsTheHoldout is criterion 15, the canary.
//
// A holdout Case carries a tag that appears nowhere else. If the seal is ever
// dropped from the read path, that tag appears in the behavior list — and the
// number beside it would be a holdout Case counted as evidence.
func TestInspectNeverReadsTheHoldout(t *testing.T) {
	t.Parallel()

	const canary = "canary-holdout-only"
	dev, hold := devHoldoutIDs(t, 0.2, 20, 20)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl",
		append(tagged(dev, "billing"), tagged(hold, canary)...))

	doc := inspectJSON(t, "--evals", path)
	for _, b := range doc.Behaviors {
		if b.Tag == canary {
			t.Fatalf("a holdout Case's tag reached the behavior list: %+v", doc.Behaviors)
		}
	}
	if doc.Cases.Holdout != 20 {
		t.Errorf("the canary fixture has %d holdout Cases, not 20", doc.Cases.Holdout)
	}
	stdout, stderr, _ := run(t, "eval", "inspect", "--evals", path)
	if strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
		t.Fatalf("the holdout canary tag reached the page:\n%s", stdout)
	}
	for _, id := range hold {
		if strings.Contains(stdout, id) {
			t.Fatalf("a holdout Case ID reached the page: %s", id)
		}
	}
}

// TestInspectRefusesAnEmptyEvalSet is criterion 21: refused, not reported as
// "0 behaviors, 0 checks flagged".
func TestInspectRefusesAnEmptyEvalSet(t *testing.T) {
	t.Parallel()

	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", nil)
	_, stderr, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitError {
		t.Fatalf("exit = %d for an empty eval set, want %d", code, errs.ExitError)
	}
	if !strings.Contains(stderr, "--evals") {
		t.Errorf("the refusal does not name --evals: %s", stderr)
	}
}

// TestInspectSurfacesAMalformedLine is criterion 22: the adapter's fatal error,
// with its context, and no partial analysis.
func TestInspectSurfacesAMalformedLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cases.jsonl")
	body := `{"id":"a","input":"x","expected":"y","tags":["billing"]}` + "\n" + "{not json\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	stdout, stderr, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitError {
		t.Fatalf("exit = %d for a malformed line, want %d\nstdout: %s", code, errs.ExitError, stdout)
	}
	if strings.Contains(stdout, "checks flagged") {
		t.Errorf("a partial analysis was printed before the refusal:\n%s", stdout)
	}
	if !strings.Contains(stderr, "line 2") {
		t.Errorf("the refusal does not name the line: %s", stderr)
	}
}

// TestEvalHelpNamesInspect is criterion 20.
func TestEvalHelpNamesInspect(t *testing.T) {
	t.Parallel()

	parent, _, code := run(t, "eval", "--help")
	if code != errs.ExitOK {
		t.Fatalf("kno eval --help exit = %d", code)
	}
	if !strings.Contains(parent, "inspect") {
		t.Errorf("`kno eval --help` does not list inspect:\n%s", parent)
	}

	child, _, code := run(t, "eval", "inspect", "--help")
	if code != errs.ExitOK {
		t.Fatalf("kno eval inspect --help exit = %d", code)
	}
	for _, want := range []string{
		"--evals", "--value-run-id", "makes no LLM call", "vendor's API",
	} {
		if !strings.Contains(child, want) {
			t.Errorf("`kno eval inspect --help` does not mention %q:\n%s", want, child)
		}
	}
}

// evalInspectGoldenPath is the pinned page of `kno eval inspect`.
const evalInspectGoldenPath = "testdata/eval_inspect.golden"

// goldenFixture writes the eval file the golden is recorded against.
//
// Everything the amended plan pins is present in one page: a dominant tag over
// half the dev split, two underpowered behaviors, a three-spelling collapse, a
// blank tag, a duplicate tag reference, a multi-behavior share, and a holdout
// below split.MinHoldout.
func goldenFixture(t *testing.T, dir string) {
	t.Helper()

	dev, hold := devHoldoutIDs(t, 0.2, 48, 12)
	var cases []inspectCase
	for n, id := range dev {
		c := inspectCase{ID: id, Expected: "a"}
		switch {
		case n < 30:
			c.Tags = []string{"overall_quality"}
		case n < 36:
			c.Tags = []string{"overall_quality", "billing"}
		case n < 42:
			c.Tags = []string{[]string{"Refunds", "refunds", " refunds "}[n%3], "overall_quality"}
		case n < 45:
			c.Tags = []string{"shipping", ""}
		default:
			c.Tags = []string{"tool_use", "tool_use"}
		}
		cases = append(cases, c)
	}
	cases = append(cases, tagged(hold, "overall_quality")...)
	writeInspectCases(t, dir, "cases.jsonl", cases)
}

// TestInspectPageIsPinned is the golden, and criteria 18, 23 and 26's rendered
// halves.
//
// Not parallel: t.Chdir is process-global, and running from the fixture's own
// directory is what makes the `Evals cases.jsonl` line stable.
func TestInspectPageIsPinned(t *testing.T) {
	golden := filepath.Join(packageDir(t), evalInspectGoldenPath)
	work := t.TempDir()
	goldenFixture(t, work)
	t.Chdir(work)

	got, stderr, code := run(t, "eval", "inspect", "--evals", "cases.jsonl")
	if code != errs.ExitOK {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatalf("writing the golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("the inspect page drifted. Re-run with -update and review the diff.\n"+
			"got:\n%s\nwant:\n%s", got, string(want))
	}

	// Criterion 23: the standing conditional appears exactly ONCE, and before
	// any per-tag number.
	const conditional = "Everything below reads your tags as behaviors"
	if n := strings.Count(got, conditional); n != 1 {
		t.Errorf("the standing conditional appears %d times, want exactly 1", n)
	}
	if strings.Index(got, conditional) > strings.Index(got, "BEHAVIOR ") {
		t.Errorf("the standing conditional appears below the behavior table")
	}
	// Every per-tag suggestion lands under the conditional header.
	if i := strings.Index(got, inspectSuggestionHeaderText); i < 0 {
		t.Errorf("the suggestions block carries no conditional header:\n%s", got)
	} else if strings.Index(got, "  - split ") < i {
		t.Errorf("a per-tag suggestion was emitted above its conditional header")
	}
	// F4: the concentration line says "carried by", never "the only behavior on".
	if strings.Contains(got, "only behavior") {
		t.Errorf("the concentration line claims exclusivity:\n%s", got)
	}
}

// inspectSuggestionHeaderText mirrors the header the renderer prints. Spelled
// out here rather than imported: the assertion is about the STRING a user
// reads, and a test that referenced the same constant would pass over a
// rewrite of it.
const inspectSuggestionHeaderText = "If these tags are behaviors you would fix separately:"

// TestInspectHumanAndJSONAgree is criterion 18's equivalence: the checks a
// human reads are the checks a jq pipeline reads, in the same order, with the
// same statuses — and the caveats survive in both renderings.
func TestInspectHumanAndJSONAgree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	goldenFixture(t, dir)
	path := filepath.Join(dir, "cases.jsonl")

	human, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	doc := inspectJSON(t, "--evals", path)

	if !strings.Contains(human, fmt.Sprintf("%d of %d checks flagged.",
		doc.ChecksFlagged, doc.ChecksTotal)) {
		t.Errorf("the page's count disagrees with checks_flagged=%d/%d:\n%s",
			doc.ChecksFlagged, doc.ChecksTotal, human)
	}
	// The behaviors are in the same order in both renderings, and the table
	// carries every one the document does (this fixture is under the limit).
	var order []string
	for _, line := range strings.Split(human, "\n") {
		for _, b := range doc.Behaviors {
			if strings.HasPrefix(line, b.Tag+" ") || line == b.Tag {
				order = append(order, b.Tag)
			}
		}
	}
	for n, b := range doc.Behaviors {
		if n >= len(order) || order[n] != b.Tag {
			t.Fatalf("behavior order differs: page %v, document %v", order, doc.Behaviors)
		}
	}
	// Every suggestion the document carries is on the page, and vice versa.
	for _, s := range doc.Suggestions {
		for _, word := range strings.Fields(s)[:3] {
			if !strings.Contains(human, word) {
				t.Errorf("the page does not carry the suggestion %q", s)
				break
			}
		}
	}
	// notes[0] is the standing conditional, always.
	if len(doc.Notes) == 0 || !strings.Contains(doc.Notes[0], "kno cannot distinguish a behavior tag") {
		t.Errorf("notes[0] is not the standing conditional: %v", doc.Notes)
	}
	if !strings.Contains(doc.Notes[2], "reported and never flagged") {
		t.Errorf("the document does not say the multi-behavior share never flags: %v", doc.Notes)
	}
}

// TestInspectTruncatesAHugeBehaviorTable is the "a single Case with 500 tags"
// edge case: 500 behaviors of one dev Case each is not a diagnostic, so the
// page shows the top 50 and says how many it withheld. --json carries all of
// them, because a truncation that lost data would be a different bug.
func TestInspectTruncatesAHugeBehaviorTable(t *testing.T) {
	t.Parallel()

	const tags = 500
	dev, hold := devHoldoutIDs(t, 0.2, 1, 25)
	all := make([]string, 0, tags)
	for n := range tags {
		all = append(all, fmt.Sprintf("tag-%03d", n))
	}
	cases := []inspectCase{{ID: dev[0], Tags: all, Expected: "a"}}
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl", append(cases, tagged(hold, all[0])...))

	doc := inspectJSON(t, "--evals", path)
	if len(doc.Behaviors) != tags {
		t.Fatalf("--json carries %d behaviors, want all %d", len(doc.Behaviors), tags)
	}

	stdout, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "…and 450 more (see --json)") {
		t.Errorf("the page does not report the truncation:\n%s", stdout)
	}
	if n := strings.Count(stdout, "underpowered"); n != 50 {
		t.Errorf("the table renders %d rows, want the top 50", n)
	}
}

// TestInspectNeutralisesControlCharactersInTags: a tag is arbitrary text from
// the user's eval file, and this command's whole output is tags. An ANSI escape
// in one would be interpreted by the terminal rather than displayed, which
// turns a diagnostic into a rendering primitive somebody else controls.
func TestInspectNeutralisesControlCharactersInTags(t *testing.T) {
	t.Parallel()

	const nasty = "bill\x1b[31ming"
	dev, hold := devHoldoutIDs(t, 0.2, 6, 25)
	path := writeInspectCases(t, t.TempDir(), "cases.jsonl",
		append(tagged(dev, nasty), tagged(hold, nasty)...))

	stdout, _, code := run(t, "eval", "inspect", "--evals", path)
	if code != errs.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("an escape sequence in a tag reached the terminal:\n%q", stdout)
	}
	// --json escapes it rather than replacing it: a jq consumer wants the tag
	// the file actually carries, and JSON string escaping is already safe.
	doc := inspectJSON(t, "--evals", path)
	if doc.Behaviors[0].Tag != nasty {
		t.Errorf("--json reports %q, want the tag verbatim", doc.Behaviors[0].Tag)
	}
}
