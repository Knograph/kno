package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// `kno eval inspect` answers one question before any money is spent: can this
// eval set attribute anything?
//
// Read-only and free, the `kno doctor` posture. It constructs no Agent,
// resolves no model credential, makes no LLM call, creates no Run, and writes
// nothing. There is no spend path, so there is no budget guard and no consent
// dialog. The one caveat is in the help text: a REMOTE eval source does make
// vendor API calls with the vendor's credentials, because reading the dataset
// is the job — "costs nothing" is a claim about LLM spend.
//
// What it deliberately does NOT claim: that your tags are behaviors (nothing in
// the schema says so, and the standing conditional above every number says the
// tool cannot tell); that a score decomposes ("X accounts for N% of total
// score" is not computable — no Goal in this build populates Score.components,
// docs/debt.md); and that any single word summarises the answer. There is no
// grade, because a command whose second finding condemns single coarse scores
// cannot credibly emit one.

// inspectSeparableLevel is the confidence level `inspect` quotes its separable
// effect at, matching the level core/value's harm bound uses.
const inspectSeparableLevel = 0.95

// inspectBehaviorTableLimit is how many behaviors the human table renders
// before it truncates. A Case carrying 500 tags produces 500 behaviors, and a
// 500-row table is not a diagnostic. --json carries all of them.
const inspectBehaviorTableLimit = 50

// inspectConcentrationFlag is the share above which one tag, or the untagged
// remainder, is reported as a concentration problem.
//
// Half the dev split under one label is the "one giant score" anti-pattern
// (docs/evaluation-design.md section 8) as it manifests in an eval file. Not
// anchored to a constant in the tree because none exists; anchored to the only
// share that needs no argument — more than half.
const inspectConcentrationFlag = 0.5

// Check names. These are a CLI contract: a CI consumer pins them from --json,
// so renaming one is a breaking change with a CHANGELOG note.
const (
	checkBehaviorsDeclared    = "behaviors_declared"
	checkBehaviorsPowered     = "behaviors_powered"
	checkBehaviorConcentraton = "behavior_concentration"
	checkHoldoutPowered       = "holdout_powered"
	checkAttributionObserved  = "attribution_observed"
)

// inspectChecksTotal is how many checks can be FLAGGED. Five, not six.
//
// behavior_separation was the sixth and is not a check: the multi-behavior
// share is still computed and still printed, but it produces no status,
// because the threshold it used to flag on was invented. A tool whose thesis is
// "do not invent thresholds" cannot flag on one of its own. See the `·` marker.
const inspectChecksTotal = 5

// inspectStatus is a check's answer, three-state on purpose.
//
// The knov1.GapStatus discipline: UNKNOWN is a real answer, not a soft pass. A
// check that needs a Value run reports UNKNOWN without one rather than passing
// by default, because "we did not look" and "we looked and found nothing" call
// for different responses.
type inspectStatus string

const (
	inspectOK      inspectStatus = "ok"
	inspectFlagged inspectStatus = "flagged"
	inspectUnknown inspectStatus = "unknown"
)

// evalInspectFlags are the options `kno eval inspect` accepts.
type evalInspectFlags struct {
	evalsPath   string
	dbPath      string
	valueRunID  string
	holdoutFrac float64
	splitSeed   string
	jsonOut     bool

	// The endpoint opt-outs a remote eval source needs, mirroring the agent
	// transport's. Deliberately flags-only, like every other security boolean:
	// a committed allow_insecure_base_url would be an ambient TLS downgrade
	// for every teammate.
	allowInsecureURL    bool
	allowPrivateAddress bool
}

// evalSource builds the resolver's spec from the flags.
func (f evalInspectFlags) evalSource() evalSourceSpec {
	return evalSourceSpec{
		path:                f.evalsPath,
		holdoutFrac:         f.holdoutFrac,
		splitSeed:           f.splitSeed,
		allowInsecureURL:    f.allowInsecureURL,
		allowPrivateAddress: f.allowPrivateAddress,
	}
}

// applyFileAndEnv applies the kno.yaml and KNO_* layers to the flags inspect
// shares with `kno baseline`.
//
// Routed through a baselineFlags so there is one precedence table
// (configSpecs) rather than two: a second implementation would drift the day a
// key was added, and it would drift silently. Only the four fields inspect
// actually reads are copied back; the rest of the run-shaping keys are not
// this command's business.
func (f *evalInspectFlags) applyFileAndEnv(cmd *cobra.Command, cfg *configFile) error {
	b := baselineFlags{
		evalsPath:   f.evalsPath,
		dbPath:      f.dbPath,
		holdoutFrac: f.holdoutFrac,
		splitSeed:   f.splitSeed,
	}
	if _, err := b.applyFileAndEnv(cmd, cfg); err != nil {
		return err
	}
	f.evalsPath, f.dbPath = b.evalsPath, b.dbPath
	f.holdoutFrac, f.splitSeed = b.holdoutFrac, b.splitSeed
	return nil
}

// newEvalInspectCmd builds `kno eval inspect`.
func newEvalInspectCmd() *cobra.Command {
	var f evalInspectFlags

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Report whether an eval set can attribute anything",
		Long: `Read an eval set and report what routing and the power arithmetic will
actually see: how many distinct behaviors your tags declare, how small an
effect each one could separate from noise, how much of the dev split sits
under a single catch-all tag, and whether the holdout can support validate.

Run it BEFORE you spend. It makes no LLM call, constructs no agent, creates
no run, and writes nothing. A remote eval source (langsmith:, langfuse:,
braintrust:, hf:) does call the vendor's API with the vendor's credentials,
because reading the dataset is the job; "costs nothing" is a claim about LLM
spend.

Every per-tag number reads your tags as behaviors, because that is what
routing does. Kno cannot tell a behavior tag from a priority, a source or a
date, and the output says so above every number it qualifies.

Exit code is 0 whether nothing or everything is flagged: this is a
diagnostic, not a gate. Read checks_flagged from --json if you want one.`,
		Example: `  # Before the first baseline
  kno eval inspect --evals cases.jsonl

  # With what a Value run actually attributed
  kno eval inspect --evals cases.jsonl --value-run-id <id>

  # As a CI check
  kno eval inspect --evals cases.jsonl --json | jq .checks_flagged`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigFile(cmd)
			if err != nil {
				return err
			}
			if err := f.applyFileAndEnv(cmd, cfg); err != nil {
				return err
			}
			return runEvalInspect(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.evalsPath, "evals", "",
		"eval cases: a JSONL file path, langsmith:<dataset-name>, langfuse:<dataset-name>, "+
			"braintrust:<dataset-name>, or hf:<org>/<name>/<config>/<split> (required)")
	flags.StringVar(&f.valueRunID, "value-run-id", "",
		"run ID of a recorded Value run, to add what routing actually attributed")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs are stored; read only with --value-run-id")
	flags.Float64Var(&f.holdoutFrac, "holdout-frac", jsonl.DefaultHoldoutFrac,
		"share of cases held back for validate")
	flags.StringVar(&f.splitSeed, "split-seed", "",
		"deliberately re-split the evals (changes which cases are held back)")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	flags.BoolVar(&f.allowInsecureURL, "allow-insecure-base-url", false,
		"permit a plain-http endpoint on a remote eval source")
	flags.BoolVar(&f.allowPrivateAddress, "allow-private-address", false,
		"permit loopback and private addresses on a remote eval source")
	addConfigFlag(cmd, defaultConfigPath)

	if err := cmd.MarkFlagRequired("evals"); err != nil {
		panic(fmt.Sprintf("cli: marking --evals required: %v", err))
	}
	return cmd
}

// runEvalInspect reads the source, analyses it, and renders.
func runEvalInspect(ctx context.Context, out io.Writer, f evalInspectFlags) error {
	src, err := resolveEvalSource(f.evalSource())
	if err != nil {
		return err
	}

	counts, err := src.CountSplits(ctx)
	if err != nil {
		return errs.ErrInvalidInput.WithFix(countsSplitFix(src)).Wrap(err)
	}
	if counts.Total() == 0 {
		return errs.ErrInvalidInput.
			WithFix("point --evals at a source with at least one Case").
			Wrap(errors.New("the eval set is empty"))
	}

	insp, err := inspectEvals(ctx, src, f.evalsPath, counts)
	if err != nil {
		return err
	}
	insp.ValueRunID = f.valueRunID

	if f.valueRunID != "" {
		if err := attachObserved(ctx, src, insp, f); err != nil {
			return err
		}
	}
	insp.Checks = insp.checks(f.valueRunID)

	return renderEvalInspect(out, f.jsonOut, insp)
}

// inspectBehavior is one normalized tag and what the dev split says about it.
type inspectBehavior struct {
	// Tag is the normalized tag, exactly as value.NormalizeTag produces it —
	// which is exactly what routing clusters by.
	Tag string
	// DevCases is how many dev Cases carry the tag. A Case tagged twice with
	// the same tag counts once.
	DevCases int
	// Spellings is how many distinct raw spellings collapsed into Tag.
	Spellings int
	// SeparableEffect is the smallest effect DevCases dev Cases can separate
	// from zero, TWO-SIDED at inspectSeparableLevel.
	SeparableEffect float64
	// Status is inspectOK or inspectFlagged, against core.MinClusterCases.
	Status inspectStatus

	// raw accumulates the distinct spellings seen, and is dropped once
	// Spellings is set. Bounded by the distinct tag strings in the file.
	raw map[string]struct{}
}

// inspection is everything the two renderers share. It holds counters, tags
// and IDs — never a Case input, expected, rubric, or Turn content.
type inspection struct {
	Evals string
	// ValueRunID is the run --value-run-id named, or empty. Held so both
	// renderers agree on whether the observed section was ASKED for, which is
	// a different question from whether it could be rendered.
	ValueRunID string
	Counts     split.Counts
	DevCases   int
	Behaviors  []inspectBehavior

	CollapsedSpellings    int
	BlankTagRefs          int
	DuplicateTagRefs      int
	UnscorableCases       int
	UntaggedDevCases      int
	MultiBehaviorDevCases int

	// Dominant is the behavior carrying the most dev Cases, nil when no dev
	// Case carries a tag.
	Dominant *inspectBehavior
	// Underpowered names the behaviors below core.MinClusterCases, in table
	// order.
	Underpowered []string

	Checks   []inspectCheck
	Observed *inspectObserved
	// observedUnknown is why Observed is nil when a Value run WAS named: an
	// absent plan, an undecodable one, or a source that has changed since the
	// run. Empty when no run was named at all.
	observedUnknown string
}

// inspectCheck is one flaggable check's answer.
type inspectCheck struct {
	Name   string
	Status inspectStatus
	Detail string
}

// DominantShare is the share of dev Cases carrying the most common normalized
// tag.
//
// Membership is NON-EXCLUSIVE: a Case tagged ["overall_quality", "billing"]
// counts toward overall_quality's numerator, because that is how cluster()
// works — a multi-tagged failed Case joins EVERY one of its clusters. The
// denominator is ALL dev Cases, untagged included, so this share and the
// untagged share are shares of the same population and can be read against
// each other.
func (i *inspection) DominantShare() float64 {
	if i.Dominant == nil || i.DevCases == 0 {
		return 0
	}
	return float64(i.Dominant.DevCases) / float64(i.DevCases)
}

// UntaggedShare is the share of dev Cases carrying no tag at all, over the same
// denominator as DominantShare.
func (i *inspection) UntaggedShare() float64 {
	if i.DevCases == 0 {
		return 0
	}
	return float64(i.UntaggedDevCases) / float64(i.DevCases)
}

// MultiBehaviorShare is the share of dev Cases carrying two or more distinct
// normalized tags, over the same denominator.
//
// Reported and NEVER flagged. It is a heuristic — a tag is a label, not a claim
// about what a Case exercises — and there is no principled threshold for it in
// this tree. What makes it worth printing anyway is that the count is exactly
// what routing does: cluster() appends a multi-tagged failed Case to every one
// of its tag's clusters, so it measures how much of the cluster structure is
// shared, which is the thing that makes per-behavior attribution ambiguous.
func (i *inspection) MultiBehaviorShare() float64 {
	if i.DevCases == 0 {
		return 0
	}
	return float64(i.MultiBehaviorDevCases) / float64(i.DevCases)
}

// inspectEvals streams the DEV split and accumulates the counters.
//
// Through core.Seal, which makes forgetting the holdout a compile error rather
// than a review lapse. Memory is O(distinct tags), not O(Cases): per-tag
// counters, a spelling set, and a handful of scalars. No Case ID is retained at
// all on this path, and no Case content is read.
func inspectEvals(
	ctx context.Context, src core.Evals, evalsPath string, counts split.Counts,
) (*inspection, error) {
	cases, err := core.Seal(src).Cases(ctx)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("check --evals names a readable source").Wrap(err)
	}

	insp := &inspection{Evals: evalsPath, Counts: counts}
	byTag := map[string]*inspectBehavior{}
	seen := map[string]struct{}{}

	for c, err := range cases {
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix("fix the reported Case, then re-run").Wrap(err)
		}
		insp.DevCases++
		if c.GetExpected() == "" && c.GetRubric() == "" {
			insp.UnscorableCases++
		}
		clear(seen)
		for _, raw := range c.GetTags() {
			key := value.NormalizeTag(raw)
			if key == "" {
				insp.BlankTagRefs++
				continue
			}
			if _, dup := seen[key]; dup {
				insp.DuplicateTagRefs++
				continue
			}
			seen[key] = struct{}{}
			b := byTag[key]
			if b == nil {
				b = &inspectBehavior{Tag: key, raw: map[string]struct{}{}}
				byTag[key] = b
			}
			b.DevCases++
			b.raw[raw] = struct{}{}
		}
		switch len(seen) {
		case 0:
			insp.UntaggedDevCases++
		case 1:
		default:
			insp.MultiBehaviorDevCases++
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.ErrInterrupted.Wrap(fmt.Errorf("reading %s: %w", evalsPath, err))
	}

	insp.finish(byTag)
	return insp, nil
}

// finish orders the behaviors and derives everything computed from them.
//
// Deterministic: dev Cases descending, tag ascending on ties. Map iteration
// order never reaches the output, so the same file inspected twice renders
// byte-identically.
func (i *inspection) finish(byTag map[string]*inspectBehavior) {
	i.Behaviors = make([]inspectBehavior, 0, len(byTag))
	for _, b := range byTag {
		b.Spellings = len(b.raw)
		if b.Spellings > 1 {
			i.CollapsedSpellings += b.Spellings
		}
		b.SeparableEffect = interval.MinDetectableEffect(
			b.DevCases, knov1.Sidedness_SIDEDNESS_TWO_SIDED, inspectSeparableLevel)
		b.Status = inspectOK
		if b.DevCases < core.MinClusterCases {
			b.Status = inspectFlagged
		}
		b.raw = nil
		i.Behaviors = append(i.Behaviors, *b)
	}
	sort.Slice(i.Behaviors, func(a, b int) bool {
		if i.Behaviors[a].DevCases != i.Behaviors[b].DevCases {
			return i.Behaviors[a].DevCases > i.Behaviors[b].DevCases
		}
		return i.Behaviors[a].Tag < i.Behaviors[b].Tag
	})
	for n := range i.Behaviors {
		if i.Behaviors[n].Status == inspectFlagged {
			i.Underpowered = append(i.Underpowered, i.Behaviors[n].Tag)
		}
	}
	if len(i.Behaviors) > 0 {
		i.Dominant = &i.Behaviors[0]
	}
}

// checks answers the five flaggable checks. behavior_separation is not among
// them: it reports and never flags.
func (i *inspection) checks(valueRunID string) []inspectCheck {
	return []inspectCheck{
		i.checkDeclared(),
		i.checkPowered(),
		i.checkConcentration(),
		i.checkHoldout(),
		i.checkObserved(valueRunID),
	}
}

// checkDeclared: do any dev Cases carry tags at all?
func (i *inspection) checkDeclared() inspectCheck {
	c := inspectCheck{Name: checkBehaviorsDeclared, Status: inspectOK}
	switch {
	case i.DevCases == 0:
		c.Status = inspectFlagged
		c.Detail = "no dev Cases: nothing can be measured against this eval set"
	case len(i.Behaviors) == 0:
		c.Status = inspectFlagged
		c.Detail = "no dev Case carries a tag, so routing falls back to all-failed " +
			"(value.ModeAllFailed) and there is no per-behavior attribution"
	default:
		c.Detail = fmt.Sprintf("%d behaviors across %d dev Cases", len(i.Behaviors), i.DevCases)
	}
	return c
}

// checkPowered: does every behavior have enough dev Cases to separate an
// effect? The line is core.MinClusterCases, read from the constant.
func (i *inspection) checkPowered() inspectCheck {
	c := inspectCheck{Name: checkBehaviorsPowered}
	switch {
	case len(i.Behaviors) == 0:
		// Not a pass. There is nothing to assess, and reporting "ok" for an
		// eval set with no behaviors would be the soft-pass GapStatus exists
		// to refuse.
		c.Status = inspectUnknown
		c.Detail = "no tagged dev Cases to assess"
	case len(i.Underpowered) > 0:
		c.Status = inspectFlagged
		c.Detail = fmt.Sprintf("%d behaviors below core.MinClusterCases (%d)",
			len(i.Underpowered), core.MinClusterCases)
	default:
		c.Status = inspectOK
		c.Detail = fmt.Sprintf("every behavior has at least core.MinClusterCases (%d) dev Cases",
			core.MinClusterCases)
	}
	return c
}

// checkConcentration: how much of the dev split sits under one tag, and how
// much under none. Both shares are over ALL dev Cases.
func (i *inspection) checkConcentration() inspectCheck {
	c := inspectCheck{Name: checkBehaviorConcentraton, Status: inspectOK}
	if i.DevCases == 0 {
		c.Status = inspectUnknown
		c.Detail = "no dev Cases to divide"
		return c
	}
	dom, unt := i.DominantShare(), i.UntaggedShare()
	c.Detail = fmt.Sprintf("the most common tag is carried by %s of dev Cases; %s carry no tag",
		pct(dom), pct(unt))
	switch {
	case dom > inspectConcentrationFlag && unt > inspectConcentrationFlag:
		c.Status = inspectFlagged
	case dom > inspectConcentrationFlag:
		c.Status = inspectFlagged
		c.Detail = fmt.Sprintf("%q is carried by %s of dev Cases", i.Dominant.Tag, pct(dom))
	case unt > inspectConcentrationFlag:
		c.Status = inspectFlagged
		c.Detail = fmt.Sprintf("%s of dev Cases carry no tag", pct(unt))
	}
	return c
}

// checkHoldout: can the holdout support a meaningful interval at validate?
//
// split.MinHoldout, via Counts.Underpowered — the same check `kno baseline`
// prints AFTER a run, surfaced before. A holdout of zero is flagged too, and
// that is deliberately stricter than Underpowered, which reports false there:
// zero is not a small holdout, it is the absence of one, and split.Counts.Validate
// refuses a run over it outright.
func (i *inspection) checkHoldout() inspectCheck {
	c := inspectCheck{Name: checkHoldoutPowered, Status: inspectOK}
	switch {
	case i.Counts.Holdout == 0:
		c.Status = inspectFlagged
		c.Detail = "no Case is held back: validate has nothing to confirm a gain against"
	case i.Counts.Underpowered():
		c.Status = inspectFlagged
		c.Detail = fmt.Sprintf("the holdout has %d Cases, below split.MinHoldout (%d)",
			i.Counts.Holdout, split.MinHoldout)
	default:
		c.Detail = fmt.Sprintf("the holdout has %d Cases, at or above split.MinHoldout (%d)",
			i.Counts.Holdout, split.MinHoldout)
	}
	return c
}

// pct renders a share the way every line of this command's output does.
func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
