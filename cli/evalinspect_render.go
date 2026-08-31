package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
)

// The two renderings of `kno eval inspect`. They are pinned to identical
// CONTENT by the equivalence test: a caveat that survives in one renderer and
// not the other is the bug that test exists to catch.

// Finding markers. Three of them carry a check's status; the fourth marks a
// number that is REPORTED AND NEVER FLAGGED, and the legend says so. A reader
// who cannot tell "we measured this and it is fine" from "we measured this and
// have no principled threshold for it" has been told something false.
const (
	markFlagged  = "!"
	markOK       = "✓"
	markUnknown  = "?"
	markReported = "·"
)

// inspectWrapWidth is the column the prose wraps at. Fixed rather than read
// from the terminal: the output is golden-pinned, and a width that depended on
// the window would make the golden a fact about the machine that ran it.
const inspectWrapWidth = 78

// inspectStandingConditional is the sentence every per-tag number in this
// command is conditional on.
//
// Stated ONCE, prominently, above the behavior table — not repeated per line
// until it reads as boilerplate. Tags routinely encode priority, provenance or
// a date (`p0`, `flaky`, `regression-2024`), and this command reports those as
// distinct behaviors with a specific separable-effect number each and then
// advises splitting or merging them. It cannot tell them from a real behavior
// taxonomy, and there is nothing in the schema to tell it from. So it says so,
// before any number, in the place a reader cannot skip.
const inspectStandingConditional = "Everything below reads your tags as behaviors, because that is what " +
	"routing does. If these tags name something else — priority, source, a date — the per-tag numbers " +
	"and suggestions below do not apply to them. Kno cannot tell the difference."

// inspectStandingConditionalNote is the same sentence for a jq pipeline, and it
// is always notes[0].
const inspectStandingConditionalNote = "every per-tag number and suggestion assumes your tags name " +
	"behaviors you would fix separately; kno cannot distinguish a behavior tag from a priority, " +
	"source or date tag"

// inspectSuggestionHeader carries the standing conditional into the block where
// the directive advice actually lands.
const inspectSuggestionHeader = "If these tags are behaviors you would fix separately:"

// inspectSeparableNote states what separable_effect is, and what it is not.
const inspectSeparableNote = "separable_effect is a two-sided 95% bound using the worst-case " +
	"paired-binary standard deviation; it is a bound, not an estimate from your data"

// inspectMultiBehaviorNote states why the multi-behavior share has no status.
const inspectMultiBehaviorNote = "multi_behavior_share is reported and never flagged: there is no " +
	"principled threshold for it"

// inspectHarmSidednessNote separates the two bounds a run-backed report shows.
const inspectHarmSidednessNote = "min_detectable_harm is Plan.MinDetectableHarm verbatim and is " +
	"ONE-sided (it answers \"did this get worse\"); every behavior's separable_effect is TWO-sided " +
	"(it answers \"is this distinguishable from noise\"). They are not comparable"

// renderEvalInspect writes whichever rendering was asked for.
func renderEvalInspect(out io.Writer, jsonOut bool, i *inspection) error {
	if jsonOut {
		return writeJSON(out, evalInspectJSON(i))
	}
	return renderEvalInspectHuman(out, i)
}

// renderEvalInspectHuman writes the page.
func renderEvalInspectHuman(out io.Writer, i *inspection) error {
	var b strings.Builder

	fmt.Fprintf(&b, "Evals  %s\n", i.Evals)
	fmt.Fprintf(&b, "  %d Cases — %d dev, %d held back\n",
		i.Counts.Total(), i.Counts.Dev, i.Counts.Holdout)
	b.WriteString(wrapHanging(i.headline(), inspectWrapWidth, "  ", "  ") + "\n")
	b.WriteString(wrapHanging(inspectStandingConditional, inspectWrapWidth, "  ", "  ") + "\n")

	renderInspectBehaviorTable(&b, i)
	b.WriteString("\n")
	for _, f := range i.findings() {
		b.WriteString(wrapMarker(f.Marker, f.Text, inspectWrapWidth))
	}
	b.WriteString("\n")
	renderInspectObserved(&b, i)
	renderInspectVerdict(&b, i)

	_, err := io.WriteString(out, b.String())
	return err
}

// headline is the behavior count and the spelling collapse.
func (i *inspection) headline() string {
	s := fmt.Sprintf("%d distinct behaviors (tags)", len(i.Behaviors))
	var collapsed []string
	for _, b := range i.Behaviors {
		if b.Spellings > 1 {
			collapsed = append(collapsed,
				fmt.Sprintf("%d spellings collapsed into %q", b.Spellings, safeTag(b.Tag)))
		}
	}
	if len(collapsed) > 0 {
		s += ", " + strings.Join(collapsed, ", ")
	}
	if i.BlankTagRefs > 0 {
		s += fmt.Sprintf(", %d blank tags skipped", i.BlankTagRefs)
	}
	if i.DuplicateTagRefs > 0 {
		s += fmt.Sprintf(", %d duplicate tag references counted once", i.DuplicateTagRefs)
	}
	return s
}

// The behavior table's columns. Their headers are also their minimum widths.
var inspectBehaviorCols = []inspectColumn{
	{Head: "BEHAVIOR"},
	{Head: "DEV CASES", Right: true},
	{Head: "SEPARABLE EFFECT (two-sided 95%)", Right: true},
	{Head: "STATUS"},
}

// renderInspectBehaviorTable writes the per-behavior table, truncated.
func renderInspectBehaviorTable(b *strings.Builder, i *inspection) {
	if len(i.Behaviors) == 0 {
		b.WriteString("No dev Case carries a tag, so there are no behaviors to report.\n")
		return
	}
	shown := i.Behaviors
	if len(shown) > inspectBehaviorTableLimit {
		shown = shown[:inspectBehaviorTableLimit]
	}
	rows := make([][]string, 0, len(shown))
	for _, bh := range shown {
		rows = append(rows, []string{
			safeTag(bh.Tag),
			strconv.Itoa(bh.DevCases),
			fmt.Sprintf("%.2f", bh.SeparableEffect),
			inspectBehaviorWord(bh.Status),
		})
	}
	renderInspectTable(b, inspectBehaviorCols, rows)
	if n := len(i.Behaviors) - len(shown); n > 0 {
		fmt.Fprintf(b, "\u2026and %d more (see --json)\n", n)
	}
}

// inspectColumn is one column of a rendered table.
type inspectColumn struct {
	// Head is the header text, and the column's minimum width.
	Head string
	// Right right-aligns the cells. Numbers are right-aligned so a reader can
	// compare a column down; words are not.
	Right bool
}

// renderInspectTable lays out a table by hand rather than with text/tabwriter,
// because tabwriter cannot right-align one column and left-align another — and
// a column of left-aligned decimals is a column nobody can compare down.
//
// Widths are measured in RUNES. The %-*s verb counts bytes, so a non-ASCII tag
// would shorten its own column and skew every row below it.
func renderInspectTable(b *strings.Builder, cols []inspectColumn, rows [][]string) {
	width := make([]int, len(cols))
	for n, c := range cols {
		width[n] = utf8.RuneCountInString(c.Head)
	}
	for _, r := range rows {
		for n, cell := range r {
			if w := utf8.RuneCountInString(cell); w > width[n] {
				width[n] = w
			}
		}
	}
	heads := make([]string, len(cols))
	for n, c := range cols {
		heads[n] = c.Head
	}
	for _, r := range append([][]string{heads}, rows...) {
		b.WriteString(inspectTableRow(cols, width, r))
	}
}

// inspectTableRow renders one row. The last column carries no trailing
// padding: invisible whitespace at the end of a line is whitespace in a golden
// file, and it is the kind of diff nobody can read.
func inspectTableRow(cols []inspectColumn, width []int, cells []string) string {
	var b strings.Builder
	for n, cell := range cells {
		if n > 0 {
			b.WriteString("  ")
		}
		pad := strings.Repeat(" ", width[n]-utf8.RuneCountInString(cell))
		switch {
		case cols[n].Right:
			b.WriteString(pad + cell)
		case n == len(cells)-1:
			b.WriteString(cell)
		default:
			b.WriteString(cell + pad)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// inspectBehaviorWord is a behavior's power verdict in words. There is no
// second adjectival tier above the floor: the NUMBER is the tier, because only
// the user knows whether 0.45 is an effect they would act on.
func inspectBehaviorWord(s inspectStatus) string {
	if s == inspectFlagged {
		return "underpowered"
	}
	return "ok"
}

// inspectFinding is one marked line of the findings block.
type inspectFinding struct {
	Marker string
	Text   string
}

// findings is the findings block, in fixed order, deterministic end to end.
func (i *inspection) findings() []inspectFinding {
	var out []inspectFinding
	if i.DevCases > 0 {
		out = append(out, inspectFinding{markReported, fmt.Sprintf(
			"%s of dev Cases carry more than one behavior tag — a failure in those Cases "+
				"testifies about every tag it carries, so per-behavior attribution is shared. "+
				"(Heuristic: a tag is a label, not a claim about what a Case exercises. "+
				"Reported, not flagged: there is no principled threshold for this.)",
			pct(i.MultiBehaviorShare()))})
	}
	if c := i.check(checkBehaviorsDeclared); c.Status == inspectFlagged {
		out = append(out, inspectFinding{markFlagged, c.Detail + "."})
	}
	out = append(out, i.concentrationFindings()...)
	out = append(out, i.poweredFinding()...)
	out = append(out, inspectFinding{
		inspectMarker(i.check(checkHoldoutPowered).Status),
		fmt.Sprintf("the holdout has %d Cases (%d is the minimum for a meaningful interval "+
			"at validate, split.MinHoldout)", i.Counts.Holdout, split.MinHoldout),
	})
	if i.ValueRunID != "" {
		c := i.check(checkAttributionObserved)
		out = append(out, inspectFinding{
			inspectMarker(c.Status),
			"what the run did: " + c.Detail + ".",
		})
	}
	if i.UnscorableCases > 0 {
		out = append(out, inspectFinding{markReported, fmt.Sprintf(
			"%d dev Cases carry neither an expected answer nor a rubric — exact-match scores "+
				"those as failures by construction.", i.UnscorableCases)})
	}
	return out
}

// concentrationFindings is the concentration line and, when anything is
// untagged, the untagged line. Both shares are over ALL dev Cases, so the two
// can be read against each other.
func (i *inspection) concentrationFindings() []inspectFinding {
	if i.Dominant == nil {
		return nil
	}
	dom := i.DominantShare()
	line := fmt.Sprintf("%q is carried by %s of dev Cases", safeTag(i.Dominant.Tag), pct(dom))
	marker := markOK
	if dom > inspectConcentrationFlag {
		marker = markFlagged
		line += " — a catch-all tag under which nothing can be attributed. If it is a behavior " +
			"you would fix in one place, that is fine; if it is several, split it"
	}
	out := []inspectFinding{{marker, line + "."}}
	if i.UntaggedDevCases > 0 {
		unt := i.UntaggedShare()
		umark := markOK
		utext := fmt.Sprintf("%s of dev Cases carry no tag", pct(unt))
		if unt > inspectConcentrationFlag {
			umark = markFlagged
			utext += " — an untagged Case cannot join any cluster, so nothing it fails " +
				"testifies about any behavior"
		}
		out = append(out, inspectFinding{umark, utext + "."})
	}
	return out
}

// poweredFinding is the behaviors_powered line, in the shape its status calls
// for. The threshold is read from core.MinClusterCases, never spelled inline.
func (i *inspection) poweredFinding() []inspectFinding {
	c := i.check(checkBehaviorsPowered)
	switch c.Status {
	case inspectFlagged:
		return []inspectFinding{{markFlagged, fmt.Sprintf(
			"%d behaviors have fewer than %d dev Cases, the minimum a measurement needs before "+
				"it may testify about a behavior at all (core.MinClusterCases): %s.",
			len(i.Underpowered), core.MinClusterCases, strings.Join(safeTags(i.Underpowered), ", "))}}
	case inspectOK:
		return []inspectFinding{{markOK, fmt.Sprintf(
			"every behavior has at least %d dev Cases (core.MinClusterCases).",
			core.MinClusterCases)}}
	case inspectUnknown:
		return nil
	default:
		return nil
	}
}

// renderInspectObserved writes what a recorded Value run actually did.
func renderInspectObserved(b *strings.Builder, i *inspection) {
	o := i.Observed
	if o == nil {
		return
	}
	fmt.Fprintf(b, "Observed  value run %s (%s), against baseline %s\n",
		o.ValueRunID, o.ValueRunStatus, o.BaselineRunID)
	fmt.Fprintf(b, "  routing mode %s over %d eligible dev Cases\n", o.RoutingMode, o.EligibleCases)
	harm := "underpowered"
	if !o.ControlUnderpowered {
		harm = "powered"
	}
	fmt.Fprintf(b, "  control arm %d Cases — minimum detectable harm %.2f (ONE-sided 95%%), %s\n\n",
		o.ControlCases, o.MinDetectableHarm, harm)

	if len(o.Behaviors) == 0 {
		b.WriteString("  the run routed against no failure cluster, so no behavior got a verdict.\n\n")
		return
	}
	rows := make([][]string, 0, len(o.Behaviors))
	for _, bh := range o.Behaviors {
		asset := bh.BestAssetID
		if asset == "" {
			asset = "\u2014"
		}
		rows = append(rows, []string{
			safeTag(bh.Tag),
			strconv.Itoa(bh.ClusterCases),
			strconv.Itoa(bh.DevCases),
			bh.GapStatus,
			safeTag(asset),
		})
	}
	renderInspectTable(b, []inspectColumn{
		{Head: "BEHAVIOR"},
		{Head: "CLUSTERED FAILURES", Right: true},
		{Head: "DEV CASES", Right: true},
		{Head: "VERDICT"},
		{Head: "BEST ASSET"},
	}, rows)
	b.WriteString("\n")
}

// renderInspectVerdict writes the count, the suggestions and the legend.
//
// A COUNT, never a word. An adjectival grade blending five checks with five
// different fixes into one term is the "one giant score" anti-pattern this
// command's own findings condemn, and a tool cannot credibly criticise a number
// it is simultaneously emitting.
func renderInspectVerdict(b *strings.Builder, i *inspection) {
	fmt.Fprintf(b, "%d of %d checks flagged.", i.Flagged(), inspectChecksTotal)
	sugg := i.suggestions()
	if len(sugg) == 0 {
		b.WriteString("\n")
	} else {
		b.WriteString("  " + inspectSuggestionHeader + "\n")
		for _, s := range sugg {
			b.WriteString(wrapMarker("-", s, inspectWrapWidth))
		}
	}
	b.WriteString("\nMarkers: ! flagged, ✓ ok, ? unknown, · reported and never flagged.\n\n")
	b.WriteString("What each number claims: docs/what-the-numbers-mean.md\n")
	b.WriteString("Designing evals: docs/evaluation-design.md\n")
}

// suggestions are the actions the findings imply. Every per-tag one lands under
// inspectSuggestionHeader, which carries the standing conditional.
func (i *inspection) suggestions() []string {
	var out []string
	if i.Dominant != nil && i.DominantShare() > inspectConcentrationFlag {
		out = append(out, fmt.Sprintf(
			"split %q into the behaviors you would act on separately", safeTag(i.Dominant.Tag)))
	}
	if i.UntaggedShare() > inspectConcentrationFlag {
		out = append(out, "tag the dev Cases that carry no tag: an untagged Case joins no cluster")
	}
	if len(i.Underpowered) > 0 {
		out = append(out, fmt.Sprintf(
			"add Cases to %s, or merge them into a behavior you would fix together",
			joinAnd(safeTags(i.Underpowered))))
	}
	if i.check(checkHoldoutPowered).Status == inspectFlagged {
		out = append(out, fmt.Sprintf(
			"add Cases so the holdout reaches %d, or validate cannot confirm a gain",
			split.MinHoldout))
	}
	if i.ValueRunID == "" {
		out = append(out,
			"re-run with --value-run-id <id> to see which behaviors a run actually attributed")
	}
	return out
}

// Flagged is how many of the five checks are flagged. Unknown is not flagged
// and is not ok; it is counted as neither.
func (i *inspection) Flagged() int {
	n := 0
	for _, c := range i.Checks {
		if c.Status == inspectFlagged {
			n++
		}
	}
	return n
}

// check returns one check by name, or the zero value when the checks have not
// been computed.
func (i *inspection) check(name string) inspectCheck {
	for _, c := range i.Checks {
		if c.Name == name {
			return c
		}
	}
	return inspectCheck{Name: name, Status: inspectUnknown}
}

// inspectMarker maps a status to its marker.
func inspectMarker(s inspectStatus) string {
	switch s {
	case inspectFlagged:
		return markFlagged
	case inspectOK:
		return markOK
	case inspectUnknown:
		return markUnknown
	default:
		return markUnknown
	}
}

// safeTag makes a user-supplied tag safe to write to a terminal.
//
// Tags are arbitrary strings from the user's eval file, and this command's
// whole output is tags. A control character or an ANSI escape in one would be
// interpreted by the terminal rather than displayed, which turns a diagnostic
// into a rendering primitive somebody else controls.
func safeTag(t string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || unicode.IsControl(r) {
			return '�'
		}
		return r
	}, t)
}

// safeTags maps safeTag over a list.
func safeTags(tags []string) []string {
	out := make([]string, len(tags))
	for n, t := range tags {
		out[n] = quoteTag(safeTag(t))
	}
	return out
}

// quoteTag quotes a tag for prose, where an unquoted one would be ambiguous
// against the surrounding words.
func quoteTag(t string) string { return "\"" + t + "\"" }

// wrapMarker renders one marked line: two spaces, the marker, then the text,
// with continuation lines aligned under the text rather than under the marker.
func wrapMarker(marker, text string, width int) string {
	return wrapHanging(marker+" "+text, width, "  ", "    ")
}

// joinAnd joins a list the way a sentence does: commas, and "and" before the
// last.
func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}
