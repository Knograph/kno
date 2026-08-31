package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// `kno eval inspect` answers one question: can this eval set support
// attribution at all?
//
// Kno's central promise is "this Asset moved this outcome by this much", and
// every mechanism that delivers it is bounded by the granularity of the eval
// set. Today the tool discovers that bound the expensive way — after a
// baseline and a value run. This command reports what the routing and power
// machinery will actually see, before anything is spent.
//
// Read-only and free, the `kno doctor` posture: it constructs no Agent,
// resolves no model credential, makes no LLM call, creates no Run, and writes
// nothing. There is no spend path, so there is no budget guard and no consent
// dialog.
//
// Two properties are held by tests rather than convention:
//
//   - Per-Case analysis goes through core.Seal, so the holdout is not
//     reachable. Totals come from CountSplits, which counts every Case and
//     retains nothing but counters — the same distinction ingestion already
//     relies on.
//   - No Case content ever reaches the output. Tags, counts and IDs only, in
//     both renderings.
//
// Exit code is 0 whether zero or five checks are flagged. It is a diagnostic,
// not a gate; a CI consumer that wants a gate reads checks_flagged from
// --json. Overloading exit 1 ("something is broken") with "your eval set is
// coarse" trains people to ignore 1.

// The check names. These are a CLI contract — people pin CI to them — so
// renaming one is a breaking change with a CHANGELOG note.
const (
	checkBehaviorsDeclared     = "behaviors_declared"
	checkBehaviorsPowered      = "behaviors_powered"
	checkBehaviorConcentration = "behavior_concentration"
	checkHoldoutPowered        = "holdout_powered"
	checkAttributionObserved   = "attribution_observed"
)

// checksTotal is how many checks can be flagged.
//
// FIVE, not six. behavior_separation — the multi-behavior share — was the
// sixth and is now reported and never flagged, because the only threshold
// ever proposed for it (25%) was anchored to nothing in the tree. A command
// whose thesis is "do not invent thresholds" cannot flag on an invented one.
// The share is still computed, still printed, still in --json; it simply
// produces no status.
const checksTotal = 5

// The three statuses, borrowed from knov1.GapStatus' discipline: UNKNOWN is a
// real answer, and a check that needs data it does not have reports UNKNOWN
// rather than passing by default.
const (
	statusOK      = "ok"
	statusFlagged = "flagged"
	statusUnknown = "unknown"
)

// concentrationFlagShare is the dominant-tag and untagged share above which
// behavior_concentration flags.
//
// Half. Not an anchor from the tree, and named here rather than buried: a tag
// carried by more than half the dev Cases is the "one giant score"
// anti-pattern (docs/evaluation-design.md section 8) as it manifests in an
// eval file, and half is the point past which the catch-all is the eval set
// rather than a part of it. Reported as a percentage either way, so a user at
// 49% sees the same number as one at 51%.
const concentrationFlagShare = 0.5

// inspectLevel is the confidence level every separable effect is reported
// at. interval.DefaultLevel, named here so the --json `level` key and the
// column header cannot drift from the arithmetic.
const inspectLevel = interval.DefaultLevel

// behaviorTableLimit is how many behaviors the human table prints.
//
// A single Case carrying 500 tags produces 500 behaviors, each with one dev
// Case. The table truncates and says so; --json carries all of them, because
// a machine consumer has no screen to overflow.
const behaviorTableLimit = 50

// evalInspectFlags are the options `kno eval inspect` accepts.
type evalInspectFlags struct {
	evals      evalsFlags
	dbPath     string
	valueRunID string
	jsonOut    bool
}

func newEvalInspectCmd() *cobra.Command {
	var f evalInspectFlags

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Report whether an eval set can support attribution",
		Long: `Read an eval set and report what routing and the power machinery will
actually see: how many distinct behaviors your tags describe, how small an
effect each behavior's dev Cases could separate from noise, how much of the
set sits under one catch-all tag, and whether the holdout is large enough for
validate.

Five checks, each anchored to a constant the engine already uses. The exit
code is 0 whether zero or five of them are flagged — this is a diagnostic,
not a gate. A CI job that wants a gate reads checks_flagged from --json.

It makes no LLM call, constructs no agent, creates no Run and writes nothing.
A remote eval source (langsmith:, langfuse:, braintrust:, hf:) does reach its
vendor's API with the vendor's credentials, because reading the dataset is the
job; "costs nothing" is a claim about LLM spend.

Everything it reports about a tag assumes your tags name behaviors you would
fix separately. Kno cannot tell a behavior tag from a priority, source or date
tag, and the output says so above every number that depends on it.

With --value-run-id it additionally reports what a recorded Value run's
routing actually did: the mode, each cluster's gap verdict, and the control
arm's one-sided minimum detectable harm. Without it, that check reports
unknown rather than passing.`,
		Example: `  # Before you spend anything
  kno eval inspect --evals cases.jsonl

  # What a run actually attributed
  kno eval inspect --evals cases.jsonl --value-run-id <id>

  # As a CI gate on your own threshold
  kno eval inspect --evals cases.jsonl --json | jq '.checks_flagged'`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvalInspect(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.evals.path, "evals", "",
		"eval set: a JSONL path, or langsmith:/langfuse:/braintrust:/hf: dataset (required)")
	flags.Float64Var(&f.evals.holdoutFrac, "holdout-frac", split.DefaultHoldoutFrac,
		"share of Cases held back; it decides which Cases are dev, so it changes every number here")
	flags.StringVar(&f.evals.splitSeed, "split-seed", "",
		"deliberate re-split of the eval set; empty means the stable split")
	flags.BoolVar(&f.evals.allowInsecureURL, "allow-insecure-base-url", false,
		"allow a plain-HTTP dataset endpoint (self-hosted deployments only)")
	flags.BoolVar(&f.evals.allowPrivateAddress, "allow-private-address", false,
		"allow a private or link-local dataset endpoint (self-hosted deployments only)")
	flags.StringVar(&f.valueRunID, "value-run-id", "",
		"run ID of a recorded Value run, to also report what its routing attributed")
	flags.StringVar(&f.dbPath, "db", "kno.db",
		"where runs are stored; read only when --value-run-id is given")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	if err := cmd.MarkFlagRequired("evals"); err != nil {
		panic(fmt.Sprintf("cli: marking --evals required: %v", err))
	}
	return cmd
}

// runEvalInspect resolves the source, analyses it, and renders.
func runEvalInspect(ctx context.Context, out io.Writer, f evalInspectFlags) error {
	src, err := resolveEvals(f.evals)
	if err != nil {
		return err
	}

	insp, err := inspectEvals(ctx, src, f.evals.path)
	if err != nil {
		return err
	}

	// The observed section is strictly additive: it adds the fifth check's
	// answer and never changes the other four. A static property of the eval
	// file must not depend on whether a run happens to exist.
	obs, detail, err := observeRun(ctx, src, f)
	if err != nil {
		return err
	}
	insp.Observed = obs
	insp.setObservedCheck(obs, detail)

	if f.jsonOut {
		return writeJSON(out, insp.jsonReport())
	}
	return renderEvalInspect(out, insp)
}

// behavior is one normalized tag and what the dev split says about it.
type behavior struct {
	// Tag is the normalized form — value.NormalizeTag's output, which is
	// routing's own normalizer and not a copy of it.
	Tag string

	// DevCases is how many dev Cases carry the tag, counted once per Case
	// however many times the Case spells it.
	DevCases int

	// Spellings is how many distinct raw spellings collapsed into Tag. One
	// for a tag written consistently.
	Spellings int

	// SeparableEffect is the smallest effect DevCases paired observations can
	// separate from zero, TWO-SIDED at 95%. Two-sided because the question is
	// symmetric — "is this behavior distinguishable from noise" — and a
	// one-sided bound at the same level is tighter, which would report more
	// power than the set has.
	SeparableEffect float64

	// Status is statusOK or statusUnderpowered.
	Status string
}

// statusUnderpowered is a behavior below core.MinClusterCases. Its own word
// rather than statusFlagged, because a behavior is not a check: the check
// behaviors_powered is what flags, and it flags once for all of them.
const statusUnderpowered = "underpowered"

// inspection is the whole analysis. Both renderings read this and nothing
// else, so they cannot disagree about content.
type inspection struct {
	// Source is the --evals value as the user wrote it.
	Source string

	// Counts is CountSplits' answer: every Case seen, holdout included. The
	// header line and the holdout check are the only things that read it.
	Counts split.Counts

	// DevCases is how many Cases core.Seal actually yielded — the population
	// every share below is over. Normally equal to Counts.Dev; a source
	// emitting SPLIT_UNSPECIFIED makes them differ, and Unsplit records that
	// rather than hiding it.
	DevCases int

	// Unsplit is Counts.Dev - DevCases: Cases that are not holdout and are
	// not marked dev either. Excluded from the analysis, matching core.Seal.
	Unsplit int

	// Behaviors is every distinct normalized tag on a dev Case, ordered by
	// DevCases descending then Tag ascending.
	Behaviors []behavior

	// CollapsedSpellings is the spelling count of the single most-collapsed
	// behavior, and CollapsedTag names it. Zero when every tag was written
	// one way. CollapsedBehaviors is how many behaviors collapsed at all.
	CollapsedSpellings int
	CollapsedTag       string
	CollapsedBehaviors int

	// UntaggedDevCases is how many dev Cases carry no usable tag. The most
	// consequential number here: an untagged Case joins no cluster, and if
	// NO dev Case is tagged, cluster() returns ModeAllFailed and per-behavior
	// attribution does not happen for the entire run.
	UntaggedDevCases int

	// MultiBehaviorDevCases is how many dev Cases carry two or more distinct
	// normalized tags. Reported, never flagged.
	MultiBehaviorDevCases int

	// BlankTagRefs counts tag entries that normalized to the empty string,
	// which cluster() skips. Reported so they are not silently dropped.
	BlankTagRefs int

	// DuplicateTagRefs counts repeated references to one tag on one Case —
	// the same accounting snapshotClusters records as NDropped.
	DuplicateTagRefs int

	// UnscoreableDevCases is how many dev Cases carry neither expected nor
	// rubric. exact-match scores those as failures by construction.
	UnscoreableDevCases int

	// Checks is the five, in fixed order.
	Checks []check

	// Observed is nil without --value-run-id, and nil when the run could not
	// be joined to this source.
	Observed *observed
}

// check is one flaggable question and its answer.
type check struct {
	Name   string
	Status string
	Detail string
}

// observed is what a recorded Value run's routing actually did.
type observed struct {
	ValueRunID    string
	BaselineRunID string
	RunStatus     knov1.RunStatus
	RoutingMode   string

	ControlCases        int
	ControlUnderpowered bool

	// MinDetectableHarm is Plan.MinDetectableHarm verbatim and therefore
	// ONE-SIDED, while every behavior's SeparableEffect above is two-sided.
	// The two answer near-identical-sounding questions with different values,
	// so both are labeled at every appearance.
	MinDetectableHarm float64

	Behaviors []observedBehavior
}

// observedBehavior is one plan cluster and its verdict.
type observedBehavior struct {
	Tag              string
	ClusterCases     int
	FailedAtBaseline int
	GapStatus        knov1.GapStatus
	BestAssetID      string
	BestDelta        float64
	CoveredCount     int
}

// tagAccumulator is the streaming state of the dev-split pass.
//
// Memory is O(distinct tags), not O(Cases): per-tag counters and a spelling
// set, and no Case IDs at all. A 1M-Case eval set must not load into RAM.
type tagAccumulator struct {
	devCases int

	// byTag maps normalized tag to the number of dev Cases carrying it.
	byTag map[string]int

	// spellings maps normalized tag to the distinct raw spellings seen.
	spellings map[string]map[string]struct{}

	untagged      int
	multiBehavior int
	blankRefs     int
	duplicateRefs int
	unscoreable   int
}

func newTagAccumulator() *tagAccumulator {
	return &tagAccumulator{
		byTag:     map[string]int{},
		spellings: map[string]map[string]struct{}{},
	}
}

// add folds one dev Case into the counters.
//
// The Case is BORROWED for this call, per Evals.Cases' contract: nothing here
// retains it, and the only strings kept are normalized tags.
func (a *tagAccumulator) add(c *core.Case) {
	a.devCases++
	if c.GetExpected() == "" && c.GetRubric() == "" {
		a.unscoreable++
	}

	// distinct is this Case's normalized tags. A Case tagged ["refunds",
	// "refunds"] counts once toward refunds and records one duplicate — the
	// same accounting snapshotClusters performs, where the count is a
	// measurement of the source data rather than a routing decision.
	distinct := map[string]struct{}{}
	for _, raw := range c.GetTags() {
		key := value.NormalizeTag(raw)
		if key == "" {
			// cluster() skips these. Counted, not silently dropped.
			a.blankRefs++
			continue
		}
		if _, dup := distinct[key]; dup {
			a.duplicateRefs++
			continue
		}
		distinct[key] = struct{}{}
		a.byTag[key]++
		if a.spellings[key] == nil {
			a.spellings[key] = map[string]struct{}{}
		}
		a.spellings[key][raw] = struct{}{}
	}

	switch len(distinct) {
	case 0:
		a.untagged++
	case 1:
	default:
		a.multiBehavior++
	}
}

// inspectEvals streams the dev split and computes everything that does not
// need a run.
func inspectEvals(ctx context.Context, src evalSource, sourceName string) (*inspection, error) {
	counts, err := src.CountSplits(ctx)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix(countsSplitFix(src)).Wrap(err)
	}
	if counts.Total() == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("point --evals at an eval set that has Cases in it").
			Wrap(fmt.Errorf("the eval set at %s has no Cases", sourceName))
	}

	// core.Seal, not the raw source. Forgetting it would be a compile error
	// only if this function took a *SealedEvals — it takes the raw source
	// because CountSplits and ContentHash live there — so the seal is applied
	// here, once, and the canary test in cli/evalinspect_test.go is what
	// proves no holdout tag reaches the behavior list.
	sealed := core.Seal(src)
	seq, err := sealed.Cases(ctx)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix(countsSplitFix(src)).Wrap(err)
	}

	acc := newTagAccumulator()
	for c, err := range seq {
		if err != nil {
			// Fatal by contract: stop ranging, surface it, print no partial
			// analysis.
			return nil, errs.ErrInvalidInput.WithFix(countsSplitFix(src)).Wrap(err)
		}
		acc.add(c)
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.ErrInterrupted.Wrap(
			fmt.Errorf("the inspection was interrupted while reading %s: %w", sourceName, err),
		)
	}

	insp := &inspection{
		Source:                sourceName,
		Counts:                counts,
		DevCases:              acc.devCases,
		Behaviors:             behaviorsFrom(acc),
		UntaggedDevCases:      acc.untagged,
		MultiBehaviorDevCases: acc.multiBehavior,
		BlankTagRefs:          acc.blankRefs,
		DuplicateTagRefs:      acc.duplicateRefs,
		UnscoreableDevCases:   acc.unscoreable,
	}
	if unsplit := counts.Dev - acc.devCases; unsplit > 0 {
		insp.Unsplit = unsplit
	}
	insp.CollapsedTag, insp.CollapsedSpellings, insp.CollapsedBehaviors = collapseReport(insp.Behaviors)
	insp.Checks = staticChecks(insp)
	return insp, nil
}

// behaviorsFrom turns the counters into the ordered behavior list.
//
// Deterministic: dev Case count descending, tag ascending on ties. Map
// iteration order must not reach the output — the same file inspected twice
// is byte-identical.
func behaviorsFrom(acc *tagAccumulator) []behavior {
	out := make([]behavior, 0, len(acc.byTag))
	for tag, n := range acc.byTag {
		out = append(out, behavior{
			Tag:       tag,
			DevCases:  n,
			Spellings: len(acc.spellings[tag]),
			SeparableEffect: interval.MinDetectableEffect(
				n, knov1.Sidedness_SIDEDNESS_TWO_SIDED, interval.DefaultLevel,
			),
			Status: powerStatus(n),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DevCases != out[j].DevCases {
			return out[i].DevCases > out[j].DevCases
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

// powerStatus reads core.MinClusterCases, the constant ComputeGaps already
// uses to decide whether a measurement may testify about a cluster at all. A
// behavior below it cannot produce a cluster verdict by construction.
//
// There is no second adjectival tier above it: the separable effect is the
// tier, and only the user knows whether 0.45 is the effect they would act on.
func powerStatus(devCases int) string {
	if devCases < core.MinClusterCases {
		return statusUnderpowered
	}
	return statusOK
}

// collapseReport names the most-collapsed behavior and how many collapsed.
//
// Behaviors arrive sorted, so ties break by dev-case count then tag — the
// same determinism the table has.
func collapseReport(behaviors []behavior) (tag string, spellings, collapsed int) {
	for _, b := range behaviors {
		if b.Spellings <= 1 {
			continue
		}
		collapsed++
		if b.Spellings > spellings {
			spellings, tag = b.Spellings, b.Tag
		}
	}
	return tag, spellings, collapsed
}

// dominant returns the behavior carrying the most dev Cases, or nil.
//
// Membership is NON-EXCLUSIVE: a Case tagged ["overall_quality", "billing"]
// counts toward both. That is how every other count in this command works and
// how routing itself works — cluster() puts a multi-tagged failed Case into
// every one of its clusters — so an exclusivity-based reading would describe
// a structure the engine does not use.
func (i *inspection) dominant() *behavior {
	if len(i.Behaviors) == 0 {
		return nil
	}
	return &i.Behaviors[0]
}

// share is a count over ALL dev Cases, untagged included.
//
// One denominator for concentration, the untagged share and the
// multi-behavior share, so the three lines can be read against each other.
func (i *inspection) share(n int) float64 {
	if i.DevCases == 0 {
		return 0
	}
	return float64(n) / float64(i.DevCases)
}

// underpoweredBehaviors counts the behaviors below core.MinClusterCases.
func (i *inspection) underpoweredBehaviors() []string {
	var out []string
	for _, b := range i.Behaviors {
		if b.Status == statusUnderpowered {
			out = append(out, b.Tag)
		}
	}
	return out
}

// staticChecks answers the four checks that need only the eval source, plus
// the fifth's placeholder. Order is fixed; the renderers walk it as given.
func staticChecks(i *inspection) []check {
	return []check{
		declaredCheck(i),
		poweredCheck(i),
		concentrationCheck(i),
		holdoutCheck(i),
		// attribution_observed, filled by setObservedCheck. UNKNOWN until
		// something proves otherwise — never ok by default.
		{Name: checkAttributionObserved, Status: statusUnknown, Detail: "no --value-run-id given"},
	}
}

// declaredCheck: do any dev Cases carry tags at all?
func declaredCheck(i *inspection) check {
	c := check{Name: checkBehaviorsDeclared}
	switch {
	case i.DevCases == 0:
		c.Status = statusFlagged
		c.Detail = "no dev Cases: every Case landed in the holdout, so there is nothing to route"
	case len(i.Behaviors) == 0:
		c.Status = statusFlagged
		c.Detail = "no dev Case carries a tag, so routing runs in " +
			value.ModeAllFailed.String() + " mode and attributes nothing per behavior"
	default:
		c.Status = statusOK
		c.Detail = fmt.Sprintf("%s on %d of %d dev Cases",
			plural(len(i.Behaviors), "distinct behavior", "distinct behaviors"), i.DevCases-i.UntaggedDevCases, i.DevCases)
	}
	return c
}

// poweredCheck: per behavior, enough dev Cases to separate an effect?
func poweredCheck(i *inspection) check {
	c := check{Name: checkBehaviorsPowered}
	if len(i.Behaviors) == 0 {
		// Nothing to assess. UNKNOWN rather than ok: a check with no data
		// must not pass by default.
		c.Status = statusUnknown
		c.Detail = "no behavior tags to assess"
		return c
	}
	under := i.underpoweredBehaviors()
	if len(under) == 0 {
		c.Status = statusOK
		c.Detail = fmt.Sprintf("every behavior has at least %d dev Cases (core.MinClusterCases)",
			core.MinClusterCases)
		return c
	}
	c.Status = statusFlagged
	c.Detail = fmt.Sprintf("%s below core.MinClusterCases (%d)",
		plural(len(under), "behavior", "behaviors"), core.MinClusterCases)
	return c
}

// concentrationCheck: how much of the dev split sits under one tag, and how
// much under none?
func concentrationCheck(i *inspection) check {
	c := check{Name: checkBehaviorConcentration}
	if i.DevCases == 0 {
		c.Status = statusUnknown
		c.Detail = "no dev Cases to measure a share over"
		return c
	}
	untaggedShare := i.share(i.UntaggedDevCases)
	if untaggedShare > concentrationFlagShare {
		c.Status = statusFlagged
		c.Detail = fmt.Sprintf("%.0f%% of dev Cases carry no behavior tag", untaggedShare*100)
		return c
	}
	if d := i.dominant(); d != nil {
		if s := i.share(d.DevCases); s > concentrationFlagShare {
			c.Status = statusFlagged
			c.Detail = fmt.Sprintf("%q is carried by %.0f%% of dev Cases", d.Tag, s*100)
			return c
		}
		c.Status = statusOK
		c.Detail = fmt.Sprintf("the most common behavior %q is carried by %.0f%% of dev Cases",
			d.Tag, i.share(d.DevCases)*100)
		return c
	}
	// No behaviors and no majority untagged is only reachable when the dev
	// split is at most half untagged and carries no tags at all, which is a
	// contradiction. Kept as an honest UNKNOWN rather than an unreachable
	// panic.
	c.Status = statusUnknown
	c.Detail = "no behavior tags to measure a share over"
	return c
}

// holdoutCheck: is the holdout large enough for validate?
//
// Agrees with split.Counts.Underpowered() wherever that has an opinion. It
// has none about a ZERO holdout — Counts' own godoc says a zero holdout "is
// not 'underpowered' — it is invalid, and Validate says so" — so zero is
// flagged with the refusal it will actually meet, not passed as ok.
func holdoutCheck(i *inspection) check {
	c := check{Name: checkHoldoutPowered}
	switch {
	case i.Counts.Holdout == 0:
		c.Status = statusFlagged
		if err := i.Counts.Validate(); err != nil {
			c.Detail = err.Error()
		} else {
			c.Detail = "the eval set has no holdout"
		}
	case i.Counts.Underpowered():
		c.Status = statusFlagged
		c.Detail = fmt.Sprintf("the holdout has %d Cases (%d is the minimum for a meaningful "+
			"interval at validate)", i.Counts.Holdout, split.MinHoldout)
	default:
		c.Status = statusOK
		c.Detail = fmt.Sprintf("the holdout has %d Cases (%d is the minimum for a meaningful "+
			"interval at validate)", i.Counts.Holdout, split.MinHoldout)
	}
	return c
}

// setObservedCheck fills the fifth check from what observeRun found.
func (i *inspection) setObservedCheck(obs *observed, detail string) {
	for n := range i.Checks {
		if i.Checks[n].Name != checkAttributionObserved {
			continue
		}
		if obs == nil {
			i.Checks[n].Status = statusUnknown
			if detail != "" {
				i.Checks[n].Detail = detail
			}
			return
		}
		i.Checks[n].Status = statusOK
		i.Checks[n].Detail = detail
		return
	}
}

// flaggedCount is the headline numerator.
func (i *inspection) flaggedCount() int {
	n := 0
	for _, c := range i.Checks {
		if c.Status == statusFlagged {
			n++
		}
	}
	return n
}

// observeRun reads a recorded Value run and joins it to this eval source.
//
// Returns (nil, detail, nil) for every honest "we cannot say": no run named,
// the source has changed since the run, the run recorded no plan, the plan
// does not decode. Returns an error only for a refusal — an unreadable
// database, an unknown run, a run of the wrong stage — because those are
// mistakes the user can fix, not absences of data.
func observeRun(ctx context.Context, src evalSource, f evalInspectFlags) (*observed, string, error) {
	if f.valueRunID == "" {
		return nil, "no --value-run-id given", nil
	}

	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return nil, "", errs.ErrInvalidInput.WithFix("check --db is readable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	run, err := db.GetRun(ctx, f.valueRunID)
	if err != nil {
		return nil, "", errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno value` run — `kno value` prints it when it finishes").
			Wrap(fmt.Errorf("loading value run %s: %w", f.valueRunID, err))
	}
	if got := run.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return nil, "", errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno value` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a value run", f.valueRunID, got))
	}

	// Fingerprint before anything is joined. Reporting a current tag
	// structure against a stale plan would be a page composed of two
	// different eval sets, and never silently.
	hash, err := src.ContentHash(ctx)
	if err != nil {
		return nil, "", errs.ErrInvalidInput.WithFix(countsSplitFix(src)).Wrap(err)
	}
	switch fingerprintMatch(run, hash) {
	case fingerprintAbsent:
		return nil, fmt.Sprintf(
			"run %s records no eval fingerprint, so it cannot be joined to this source",
			f.valueRunID,
		), nil
	case fingerprintDiffers:
		return nil, "the eval source has changed since this run", nil
	case fingerprintMatches:
	}

	if len(run.GetValuePlan()) == 0 {
		return nil, fmt.Sprintf("run %s recorded no routing plan", f.valueRunID), nil
	}
	var plan value.Plan
	if err := gob.NewDecoder(bytes.NewReader(run.GetValuePlan())).Decode(&plan); err != nil {
		// The treatment core/export.go already applies to an undecodable
		// plan: an absence, never a panic and never a guess.
		return nil, fmt.Sprintf("run %s recorded a routing plan this build cannot decode", f.valueRunID), nil
	}

	obs := &observed{
		ValueRunID:          f.valueRunID,
		BaselineRunID:       run.GetBaselineRunId(),
		RunStatus:           run.GetStatus(),
		RoutingMode:         plan.Mode.String(),
		ControlCases:        len(plan.ControlCaseIDs),
		ControlUnderpowered: plan.ControlUnderpowered,
		MinDetectableHarm:   plan.MinDetectableHarm,
	}

	valuations, err := db.Valuations(ctx, f.valueRunID)
	if err != nil {
		return nil, "", fmt.Errorf("loading the valuations of %s: %w", f.valueRunID, err)
	}
	gaps := core.ComputeGaps(&plan, valuations)

	// Per-behavior baseline failure counts, from the run's own reference.
	// Absent for a run with no baseline: the count is then zero and the
	// column says so, rather than a guess.
	var scores map[string]store.CaseScore
	if obs.BaselineRunID != "" {
		scores, err = db.CaseScores(ctx, obs.BaselineRunID)
		if err != nil && !errors.Is(err, store.ErrRunNotFound) {
			return nil, "", fmt.Errorf("loading the baseline case scores of %s: %w", obs.BaselineRunID, err)
		}
	}

	byTag := make(map[string]*knov1.GapCluster, len(gaps.GetClusters()))
	for _, gc := range gaps.GetClusters() {
		byTag[gc.GetTag()] = gc
	}
	for _, cs := range plan.Clusters {
		ob := observedBehavior{
			Tag:              cs.Tag,
			ClusterCases:     len(cs.CaseIDs),
			FailedAtBaseline: failedCount(cs.CaseIDs, scores),
		}
		if gc := byTag[cs.Tag]; gc != nil {
			ob.GapStatus = gc.GetStatus()
			ob.BestAssetID = gc.GetBestAssetId()
			ob.BestDelta = gc.GetBestDelta()
			ob.CoveredCount = int(gc.GetCoveredCount())
		}
		obs.Behaviors = append(obs.Behaviors, ob)
	}

	detail := fmt.Sprintf("routing ran in %s mode over %s",
		obs.RoutingMode, plural(len(obs.Behaviors), "cluster", "clusters"))
	if obs.RunStatus != knov1.RunStatus_RUN_STATUS_COMPLETED {
		// A budget-stopped or interrupted run measured part of its plan.
		// The verdicts are rendered WITH the status rather than withheld —
		// Select's source-run rule — because a partial verdict is data and a
		// hidden one is not.
		detail += fmt.Sprintf("; the run is %s, so the verdicts are partial", statusName(obs.RunStatus))
	}
	return obs, detail, nil
}

// fingerprintVerdict is whether a run can be joined to an eval source.
type fingerprintVerdict int

const (
	// fingerprintMatches: a recorded fingerprint equals the source's.
	fingerprintMatches fingerprintVerdict = iota
	// fingerprintDiffers: a recorded fingerprint disagrees.
	fingerprintDiffers
	// fingerprintAbsent: the run recorded none, so nothing can be checked.
	// A THIRD answer rather than an optimistic match, for the reason
	// GapStatus has three: "we did not look" must not read as "we looked and
	// it was fine".
	fingerprintAbsent
)

// fingerprintMatch compares a run's recorded eval fingerprint to the source's
// current content hash.
//
// Two fields, in order of authority, and the second is why this is a function
// rather than a field read. Run.eval_content_hash is where the eval source's
// hash belongs — but only `kno baseline` writes it (cli/baseline.go), and a
// VALUE run leaves it empty. What a Value run does record is
// Run.input_fingerprint, which cli/value.go sets to evals.ContentHash(ctx)
// and nothing else. So a Value run's input fingerprint IS its eval content
// hash today.
//
// That coupling is load-bearing and is stated here so it cannot rot silently:
// if `kno value` ever mixes the pool or the agent into its input fingerprint,
// this comparison starts reporting a stale-source mismatch on every run, and
// the fix is for the Value run to record eval_content_hash the way Baseline
// does — not for this function to loosen.
func fingerprintMatch(run *knov1.Run, hash string) fingerprintVerdict {
	for _, recorded := range []string{run.GetEvalContentHash(), run.GetInputFingerprint()} {
		if recorded == "" {
			continue
		}
		if recorded == hash {
			return fingerprintMatches
		}
		return fingerprintDiffers
	}
	return fingerprintAbsent
}

// plural renders a count with its noun, taking BOTH forms.
//
// Both spelled out rather than derived by adding an s: this command counts
// "tag entries" and "dev Cases carry", neither of which the naive rule gets
// right, and a findings line that reads "1 behaviors" undermines the
// carefulness the rest of the output is asking the reader to trust.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// failedCount counts how many of the cluster's Cases the baseline record
// confirms as not passing.
//
// A cluster is built from failed Cases, so this normally equals the cluster
// size. A shortfall means scores that are no longer readable — the
// store.CaseScore.Unrecoverable state — and reporting the smaller number is
// the honest answer, not the cluster size dressed up as a confirmation.
func failedCount(caseIDs []string, scores map[string]store.CaseScore) int {
	if scores == nil {
		return 0
	}
	n := 0
	for _, id := range caseIDs {
		s, ok := scores[id]
		if !ok || s.Unrecoverable || s.Passed {
			continue
		}
		n++
	}
	return n
}
