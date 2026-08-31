package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/knograph/kno/adapters/evals/split"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The human rendering of `kno eval inspect`.
//
// Four markers, and the legend line in the output says so:
//
//	!  a flagged check
//	✓  a check that passed
//	?  a check that cannot be answered from what was given
//	·  a number that is REPORTED AND NEVER FLAGGED
//
// The third exists because the multi-behavior share has no principled
// threshold in this tree, and a tool built to refuse invented cut-offs cannot
// flag on one. It is not a fourth status — three states are the discipline
// borrowed from GapStatus, and inventing a fourth to rescue a deleted check
// would cost more than the check was worth.

// standingConditional is the sentence every per-tag number in this command
// depends on.
//
// Printed ONCE, above the first per-tag number, in the place a reader cannot
// skip — rather than repeated per line until it reads as boilerplate. It is
// also notes[0] in --json, and the suggestions block carries it in four
// words, because that is where the directive advice actually lands.
//
// The reason it is non-optional: `cluster()` groups by tag, so a behavior IS
// a normalized tag as far as the engine is concerned — but tags are free-form
// user strings. `p0`, `flaky`, `regression-2024` and `source:zendesk` are
// reported here as distinct behaviors with specific separable-effect numbers
// and directive per-tag suggestions, and nothing in the schema lets this
// command tell them from a real behavior taxonomy.
const standingConditional = "every per-tag number and suggestion assumes your tags name " +
	"behaviors you would fix separately; kno cannot distinguish a behavior tag from a " +
	"priority, source or date tag"

// noteSeparableEffect and noteMultiBehavior are the other two fixed notes.
const (
	noteSeparableEffect = "separable_effect is a two-sided 95% bound using the worst-case " +
		"paired-binary standard deviation; it is a bound, not an estimate from your data"

	noteMultiBehavior = "multi_behavior_share is reported and never flagged: there is no " +
		"principled threshold for it"
)

// findingsLegend names the markers. Printed above the findings rather than
// left to be inferred, because `·` means something no other line means: a
// number this command reports and will never flag.
const findingsLegend = "  ! flagged   ✓ ok   ? unknown   · reported, never flagged"

// suggestionsHeader carries the standing conditional into the block where the
// directive advice lands.
const suggestionsHeader = "If these tags are behaviors you would fix separately:"

// renderEvalInspect writes the human page.
func renderEvalInspect(out io.Writer, i *inspection) error {
	var b strings.Builder

	renderInspectHeader(&b, i)
	renderInspectBehaviors(&b, i)
	renderInspectFindings(&b, i)
	renderInspectObserved(&b, i)
	renderInspectSuggestions(&b, i)

	b.WriteString("\nWhat each number claims: docs/what-the-numbers-mean.md\n")
	b.WriteString("Designing evals: docs/evaluation-design.md\n")

	_, err := io.WriteString(out, b.String())
	return err
}

// renderInspectHeader writes the source, the split, and the standing
// conditional.
func renderInspectHeader(b *strings.Builder, i *inspection) {
	fmt.Fprintf(b, "Evals  %s\n", i.Source)
	fmt.Fprintf(b, "  %d Cases — %d dev, %d held back\n",
		i.Counts.Total(), i.Counts.Dev, i.Counts.Holdout)

	fmt.Fprintf(b, "  %s\n", behaviorCountLine(i))
	if i.Unsplit > 0 {
		fmt.Fprintf(b, "  %d Cases carry no split assignment and are excluded from the "+
			"per-behavior analysis, matching core.Seal\n", i.Unsplit)
	}

	// The standing conditional, once, before any per-tag number.
	b.WriteString("\n  Everything below reads your tags as behaviors, because that is what routing\n")
	b.WriteString("  does. If these tags name something else — priority, source, a date — the\n")
	b.WriteString("  per-tag numbers and suggestions below do not apply to them. Kno cannot tell\n")
	b.WriteString("  the difference.\n")
}

// behaviorCountLine reports the behavior count and the spelling collapse.
//
// The collapse line matters more than any other for a user who believes they
// have eight behaviors while routing sees six.
func behaviorCountLine(i *inspection) string {
	line := plural(len(i.Behaviors), "distinct behavior", "distinct behaviors") + " (tags)"
	if i.CollapsedSpellings > 1 {
		line += fmt.Sprintf(", %d spellings collapsed into %q", i.CollapsedSpellings, i.CollapsedTag)
		if i.CollapsedBehaviors > 1 {
			line += fmt.Sprintf(" and %s collapsed spellings too",
				plural(i.CollapsedBehaviors-1, "other behavior", "other behaviors"))
		}
	}
	return line
}

// renderInspectBehaviors writes the behavior table.
func renderInspectBehaviors(b *strings.Builder, i *inspection) {
	if len(i.Behaviors) == 0 {
		b.WriteString("\nNo dev Case carries a behavior tag.\n")
		return
	}

	shown := i.Behaviors
	if len(shown) > behaviorTableLimit {
		shown = shown[:behaviorTableLimit]
	}

	// Laid out by hand rather than with a tabwriter: the numeric columns are
	// RIGHT-aligned and the tag and status columns are left-aligned, and a
	// tabwriter aligns every cell the same way. A column of right-aligned
	// numbers is the difference between a table you can scan for the small
	// ones and a table you have to read.
	const (
		devHeader    = "DEV CASES"
		effectHeader = "SEPARABLE EFFECT (two-sided 95%)"
	)
	tagWidth := len("BEHAVIOR")
	for _, bh := range shown {
		if w := utf8.RuneCountInString(bh.Tag); w > tagWidth {
			tagWidth = w
		}
	}

	b.WriteString("\n")
	fmt.Fprintf(b, "%-*s  %s  %s  %s\n", tagWidth, "BEHAVIOR", devHeader, effectHeader, "STATUS")
	for _, bh := range shown {
		fmt.Fprintf(b, "%-*s  %*d  %*.2f  %s\n",
			tagWidth, bh.Tag,
			len(devHeader), bh.DevCases,
			len(effectHeader), bh.SeparableEffect,
			bh.Status)
	}
	if len(i.Behaviors) > behaviorTableLimit {
		fmt.Fprintf(b, "…and %d more (see --json)\n", len(i.Behaviors)-behaviorTableLimit)
	}
}

// renderInspectFindings writes the reported-only lines and the check lines,
// in one fixed order.
func renderInspectFindings(b *strings.Builder, i *inspection) {
	b.WriteString("\n")
	b.WriteString(findingsLegend + "\n")

	// Reported, never flagged: the multi-behavior share, first, because it
	// qualifies every per-behavior number above it.
	if i.DevCases > 0 {
		wrapMarker(b, "·", fmt.Sprintf(
			"%.0f%% of dev Cases carry more than one behavior tag — a failure in those Cases "+
				"testifies about every tag it carries, so per-behavior attribution is shared. "+
				"(Heuristic: a tag is a label, not a claim about what a Case exercises. Reported, "+
				"not flagged: there is no principled threshold for this.)",
			i.share(i.MultiBehaviorDevCases)*100,
		))
	}
	if i.BlankTagRefs > 0 {
		wrapMarker(b, "·", fmt.Sprintf(
			"%s empty or whitespace-only and skipped, matching cluster().",
			plural(i.BlankTagRefs, "tag entry is", "tag entries are"),
		))
	}
	if i.DuplicateTagRefs > 0 {
		wrapMarker(b, "·", fmt.Sprintf(
			"%s on single Cases were counted once each, matching snapshotClusters' "+
				"NDropped accounting.", plural(i.DuplicateTagRefs, "repeated tag reference", "repeated tag references"),
		))
	}
	if i.UnscoreableDevCases > 0 {
		wrapMarker(b, "·", fmt.Sprintf(
			"%s neither expected nor rubric; exact-match scores those as failures by "+
				"construction.", plural(i.UnscoreableDevCases, "dev Case carries", "dev Cases carry"),
		))
	}

	for _, c := range i.Checks {
		switch c.Status {
		case statusFlagged:
			wrapMarker(b, "!", c.Detail)
		case statusOK:
			wrapMarker(b, "✓", c.Detail)
		default:
			// UNKNOWN is a real answer and says which question it belongs
			// to, because "we did not look" and "we looked and found
			// nothing" must not read alike.
			wrapMarker(b, "?", c.Name+" is unknown: "+c.Detail)
		}
	}
}

// renderInspectObserved writes what a recorded Value run's routing did.
func renderInspectObserved(b *strings.Builder, i *inspection) {
	obs := i.Observed
	if obs == nil {
		return
	}

	fmt.Fprintf(b, "\nObserved  value run %s", obs.ValueRunID)
	if obs.BaselineRunID != "" {
		fmt.Fprintf(b, ", against baseline %s", obs.BaselineRunID)
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "  routing mode %s, run %s\n", obs.RoutingMode, statusName(obs.RunStatus))
	// The one-sided label is not decoration. Every behavior's separable
	// effect above is TWO-sided, and the two answer near-identical-sounding
	// questions with different values.
	fmt.Fprintf(b, "  control arm %d Cases, minimum detectable harm %.2f (one-sided 95%%)",
		obs.ControlCases, obs.MinDetectableHarm)
	if obs.ControlUnderpowered {
		b.WriteString(" — underpowered")
	}
	b.WriteString("\n")

	if len(obs.Behaviors) == 0 {
		b.WriteString("  the run routed against no clusters\n")
		return
	}
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  BEHAVIOR\tCLUSTER CASES\tFAILED AT BASELINE\tVERDICT\tBEST ASSET\tBEST DELTA\tCOVERED") //nolint:errcheck // strings.Builder cannot fail
	for _, ob := range obs.Behaviors {
		best := ob.BestAssetID
		delta := fmt.Sprintf("%+.4f", ob.BestDelta)
		if best == "" {
			// No usable covering measurement. An em dash, never a 0.0000
			// that reads as "measured, no effect".
			best, delta = "—", "—"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\t%d\n", //nolint:errcheck // strings.Builder cannot fail
			ob.Tag, ob.ClusterCases, ob.FailedAtBaseline, gapStatusName(ob.GapStatus),
			best, delta, ob.CoveredCount)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(b, "(the observed table could not be rendered: %v)\n", err)
	}
}

// gapStatusName renders a cluster verdict in the vocabulary the report uses.
func gapStatusName(s knov1.GapStatus) string {
	switch s {
	case knov1.GapStatus_GAP_STATUS_IMPROVED:
		return "improved"
	case knov1.GapStatus_GAP_STATUS_GAP:
		return "gap"
	case knov1.GapStatus_GAP_STATUS_UNSPECIFIED, knov1.GapStatus_GAP_STATUS_UNKNOWN:
		return "unknown"
	default:
		return "unknown"
	}
}

// renderInspectSuggestions writes the headline count and the advice.
func renderInspectSuggestions(b *strings.Builder, i *inspection) {
	fmt.Fprintf(b, "\n%d of %d checks flagged.", i.flaggedCount(), checksTotal)

	sugg := inspectSuggestions(i)
	if len(sugg) == 0 {
		b.WriteString("\n")
		return
	}
	// The conditional header. The suggestions are directive and per-tag, so
	// the conditional belongs where they are, not only above the table.
	fmt.Fprintf(b, "  %s\n", suggestionsHeader)
	for _, s := range sugg {
		wrapMarker(b, "-", s)
	}
}

// inspectSuggestions is the advice, in fixed order, shared by both
// renderings. Nothing here is emitted outside the conditional framing.
func inspectSuggestions(i *inspection) []string {
	var out []string

	if d := i.dominant(); d != nil && i.share(d.DevCases) > concentrationFlagShare {
		out = append(out, fmt.Sprintf(
			"split %q into the behaviors you would act on separately", d.Tag,
		))
	}
	if i.DevCases > 0 && i.share(i.UntaggedDevCases) > concentrationFlagShare {
		out = append(out, fmt.Sprintf(
			"tag the %d dev Cases that carry no behavior tag, or accept that routing will "+
				"measure everything against everything", i.UntaggedDevCases,
		))
	}
	if under := i.underpoweredBehaviors(); len(under) > 0 {
		out = append(out, fmt.Sprintf(
			"add Cases to %s, or merge them into a behavior you would fix together",
			strings.Join(under, " and "),
		))
	}
	if i.Counts.Holdout > 0 && i.Counts.Underpowered() {
		out = append(out, fmt.Sprintf(
			"grow the eval set to roughly %d Cases so the holdout reaches %d at a holdout "+
				"fraction of %.2f, or validate will have no meaningful interval",
			minHoldoutCases(i), split.MinHoldout, i.Counts.HoldoutFrac,
		))
	}
	if i.Observed == nil {
		out = append(out, "re-run with --value-run-id <id> to see which behaviors a run "+
			"actually attributed")
	}
	return out
}

// minHoldoutCases is how many Cases the whole set needs for a holdout at
// split.MinHoldout, at the fraction the user actually configured.
//
// Computed against the recorded fraction rather than the package default: a
// user at 0.05 who is told they need 100 Cases when the true answer is 400
// has been given a fix that does not fix.
func minHoldoutCases(i *inspection) int {
	frac := i.Counts.HoldoutFrac
	if frac <= 0 {
		return 0
	}
	return int(float64(split.MinHoldout)/frac + 0.5)
}

// wrapMarker writes one marked, wrapped finding line.
//
// Wrapped at a fixed width rather than at the terminal's: the output is
// golden-pinned and must be byte-identical in a pipe, in CI, and on a
// terminal of any size.
func wrapMarker(b *strings.Builder, marker, text string) {
	const width = 78
	indent := "    "
	first := "  " + marker + " "

	words := strings.Fields(text)
	if len(words) == 0 {
		return
	}
	// Width is counted in RUNES, not bytes. The findings carry em dashes and
	// typographic quotes, and wrapping on byte length would break lines early
	// and differently depending on which punctuation a tag happened to
	// contain.
	line := first
	col := utf8.RuneCountInString(first)
	for n, w := range words {
		wide := utf8.RuneCountInString(w)
		if n > 0 && col+1+wide > width {
			b.WriteString(line + "\n")
			line = indent + w
			col = utf8.RuneCountInString(indent) + wide
			continue
		}
		if n > 0 {
			line += " "
			col++
		}
		line += w
		col += wide
	}
	b.WriteString(line + "\n")
}
