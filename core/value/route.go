package value

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// CaseRef is everything the router is allowed to know about a Case.
//
// An ID, its tags, and whether the baseline failed it. Deliberately NOT a Case
// and deliberately NOT a Score — the router takes no Store and no Score, so the
// routing path and the delta path cannot share a source by construction rather
// than by review.
//
// Failed is here on purpose and is not the same exposure. Routing to the Cases
// a baseline failed is what makes the stage affordable and is what DESIGN.md
// prescribes; the bias it would otherwise introduce is removed by measuring a
// fresh control arm on those Cases (see AssetRouting.FreshControlArm) and by
// drawing the harm test from a partition reserved before routing runs. What
// must never reach this package is the per-Case score VALUE, because that is
// the number the delta is computed from — and a router holding it could pair a
// Case against the very draw that selected it.
type CaseRef struct {
	// ID is the Case's identifier, as it appears in the Evals file.
	ID string

	// Tags are the Case's labels, used for failure clustering.
	Tags []string

	// Failed reports whether the baseline scored this Case as not passing.
	Failed bool
}

// AssetRef is everything the router is allowed to know about an Asset.
type AssetRef struct {
	// ID identifies the Asset.
	ID string

	// Tags are the Asset's labels, matched against a failure cluster's tags.
	Tags []string
}

// Mode records how routing was decided for a run, and is fixed before the
// consent quote rather than discovered per Asset.
type Mode int32

const (
	// ModeUnspecified is the zero value and never appears in a Plan.
	ModeUnspecified Mode = iota

	// ModeTagOverlap is the intended path: dev Cases the baseline failed are
	// clustered by tag, and an Asset routes to the clusters its own tags
	// overlap.
	ModeTagOverlap

	// ModeAllFailed means no Case in the dev split carries a tag, so there are
	// no clusters to overlap and tag routing is inapplicable. Every Asset is
	// measured against a sample of the failed Cases.
	//
	// This is the DEFAULT STATE OF A REAL EVAL FILE — Case.tags is optional and
	// nothing populates it — so it is a first-class path, decided and reported
	// before the quote. Treating it as an edge case yields a run that rejects
	// every Asset as irrelevant and appears to do nothing.
	ModeAllFailed

	// ModeAllDev means every Asset is measured against a sample of every
	// eligible Case. Reached two ways, and they are NOT equivalent for the
	// purpose that matters: the caller switched routing off, which is
	// outcome-blind, or the baseline failed nothing in the eligible partition
	// — which is a fact about the outcomes, discovered by reading them.
	//
	// Only the first may pair against the recorded baseline. See the
	// FreshControlArm assignment in Route.
	ModeAllDev
)

// minDetectableHarm is the half-width of a one-sided bound over m paired
// observations, which is the smallest regression a run of that size can
// separate from zero.
//
// The worst-case paired-binary standard deviation is sqrt(0.5) — differences
// live in {-1, 0, +1}, and the variance 2p(1-p) is maximised at 0.5 when the
// discordant pairs split evenly — so this is a bound rather than an estimate,
// and it does not need the data. Reporting an optimistic figure computed from
// the observed variance would make the number smaller exactly on the runs
// where it mattered most.
//
// The t quantile replaces z for small m: at m=20 the one-sided 95% t value is
// 1.729 against z's 1.645, and quoting z there under-states a bound the user
// is about to act on.
func minDetectableHarm(m int) float64 {
	if m < 1 {
		// No control sample: nothing is detectable, and the caller must not
		// read a small number here as a tight bound.
		return 0
	}
	z := 1.645 // z at 95% one-sided, used beyond df=30 where t reaches it.
	// The table is indexed by degrees of freedom 1..31, stored 0-based, so
	// df=1 (m=2) reads index 0.
	if df := m - 1; df >= 1 && df <= len(t95OneSided) {
		z = t95OneSided[df-1]
	}
	// sdMax is the STANDARD DEVIATION bound, sqrt(0.5) ~ 0.707, not the
	// variance bound 0.5 — the earlier slip that understated every reported
	// minimum by ~sqrt(2).
	const sdMax = 0.7071067811865476 // math.Sqrt(0.5)
	return z * sdMax / math.Sqrt(float64(m))
}

// t95OneSided is the one-sided 95% Student-t critical value by degrees of
// freedom, df 0..30. df = m-1 for a sample of m pairs. Kept as a table rather
// than a dependency: these are the small-m values where t differs from z
// enough to matter, and the bound is reported, not refined.
var t95OneSided = [...]float64{
	6.314, 2.920, 2.353, 2.132, 2.015, 1.943, 1.895, 1.860, 1.833, 1.812,
	1.796, 1.782, 1.771, 1.761, 1.753, 1.746, 1.740, 1.734, 1.729, 1.725,
	1.721, 1.717, 1.714, 1.711, 1.708, 1.706, 1.703, 1.701, 1.699, 1.697,
	1.695,
}

// String renders a Mode for events and error messages.
func (m Mode) String() string {
	switch m {
	case ModeTagOverlap:
		return "tag-overlap"
	case ModeAllFailed:
		return "all-failed"
	case ModeAllDev:
		return "all-dev"
	case ModeUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("Mode(%d)", int32(m))
	}
}

// Options configure routing and sampling.
//
// Rates are fractions of a candidate set with a floor, not counts: a rate
// scales with the eval set a user actually has, and DESIGN.md names
// `sample_rate` and `trials` as the budget vocabulary.
type Options struct {
	// SampleRate bounds the treatment draw from an Asset's routed candidates.
	// Zero uses DefaultSampleRate.
	SampleRate float64

	// ControlSampleRate bounds the draw from the reserved partition for the
	// harm test. Zero uses DefaultControlSampleRate.
	ControlSampleRate float64

	// ControlReserve is the fraction of eligible Cases held out of routing
	// entirely, from which the harm test is drawn. Zero uses
	// DefaultControlReserve.
	ControlReserve float64

	// Trials is how many times each measurement is repeated. Zero uses 1.
	Trials int32

	// Seed makes the partition and every sample reproducible. It is recorded on
	// the Run, so a reader can re-derive exactly which Cases were chosen.
	//
	// The stream is specified from v0.1.0 (PCG + the inlined draw and shuffle
	// in shuffle.go, docs/debt.md#75). Runs recorded by earlier releases
	// re-derive only under the earlier binary: the shuffle was replaced, not
	// preserved, and the field does not pretend otherwise.
	Seed int64

	// DisableRouting measures every Asset against a sample of every eligible
	// Case, ignoring tags and failures entirely. What --route none wires to.
	//
	// It makes the run MORE expensive per Asset and cheaper per Case, and the
	// second half is not obvious: a random sample is not conditioned on the
	// baseline outcome, so the recorded baseline is a valid control and the
	// treatment arm needs no fresh partner. Routing off halves the per-Case
	// cost while multiplying the Case count.
	//
	// The reserved partition is unaffected, which is what makes this safe. An
	// earlier design drew controls from the complement of the routed set, and
	// there --route none left no complement and the harm test silently
	// disappeared — the flag a user reaches for when they distrust their tags
	// would have quietly removed the regression check. Drawing controls from a
	// partition reserved before routing means routing can be switched off
	// without touching them.
	DisableRouting bool

	// MinSample is the floor under every rate-derived sample size, so a small
	// eval set does not silently produce a one-Case measurement with an
	// interval too wide to mean anything. Zero uses DefaultMinSample.
	MinSample int
}

// Defaults, each derived rather than picked.
const (
	// DefaultSampleRate draws most of an Asset's routed candidates. Routing has
	// already cut the candidate set to the failures an Asset's tags match, so
	// the sample is bounding a set that is small by construction; cutting it
	// hard again buys little and costs interval width, which is the thing the
	// stage exists to report.
	DefaultSampleRate = 0.8

	// DefaultControlSampleRate draws the harm test from the reserved partition.
	//
	// Higher than the treatment rate, not lower, which inverts the intuition
	// and is the point: the control question is "did this break something",
	// answered as a one-sided bound on harm, and an underpowered harm test
	// looks exactly like a passed one. At M=10 paired binary observations the
	// two-sided bound is roughly +/-0.44 (1.96 x sqrt(0.5)/sqrt(10)), so a
	// true -0.10 regression reads as no regression. The rate is set so a
	// default-sized run clears MinSample with
	// room, and the underpowered marker fires rather than being avoided by
	// hope.
	DefaultControlSampleRate = 1.0

	// DefaultControlReserve holds this fraction of eligible Cases out of
	// routing so the harm test is drawn from a set no routing decision touched.
	//
	// The reservation is what makes the control arm outcome-independent BY
	// CONSTRUCTION. Drawing controls from the complement of a failure-routed
	// set instead selects on "the baseline passed", and for a null Asset that
	// yields E[delta_control] = p - 1 — a systematic HARM signal aimed at inert
	// Assets, read through the instrument most sensitive to it. Simulated at
	// p=0.7 over 14,043 Cases: -0.2947 against -0.30 predicted.
	DefaultControlReserve = 0.3

	// DefaultMinSample is the floor under every sample.
	DefaultMinSample = 5
)

// HarmMargin is the regression this stage is calibrated to catch: epsilon, the
// harm size worth acting on.
//
// Named rather than left implicit, because every other number here derives from
// it. A control sample is "enough" only relative to a margin, and a design that
// picks a sample size first and describes its power afterwards has chosen the
// convenience number and written the justification backwards.
const HarmMargin = 0.10

// MinControlSample is the control-Case count below which a harm bound is
// marked underpowered.
//
// CASES, not measurements. Repeat trials of one Case are not independent
// observations, so counting them would inflate this exactly the way flattening
// trials inflates an interval.
//
// This threshold is honest about being weak. At 20 paired binary Cases the
// one-sided half-width is about 0.26 (1.729 x sqrt(0.5)/sqrt(20)) — larger
// than HarmMargin itself, so a run at this floor cannot clear anything at the
// margin it claims to care about. Detecting HarmMargin reliably needs roughly
// 135 Cases, which is more than most dev splits hold, so raising the constant
// would mark nearly every run underpowered and the marker would stop carrying
// information.
//
// The flag is therefore a floor, not a licence: above it a run is not "powered",
// it is merely not absurd. Plan.MinDetectableHarm is the number a reader should
// actually use, and it is reported rather than summarised into a bool.
const MinControlSample = 20

// AssetRouting is one Asset's routing decision, made before any spend.
type AssetRouting struct {
	// AssetID identifies the Asset.
	AssetID string

	// CaseIDs are the Cases the treatment arm will measure, already sampled.
	// Empty means the Asset routed to nothing.
	CaseIDs []string

	// FreshControlArm reports whether the control arm for these Cases must be
	// measured rather than read from the recorded baseline.
	//
	// True whenever the selection conditioned on the baseline outcome, which is
	// every mode except ModeAllDev. It doubles this Asset's measurement count,
	// and that is the price of the delta meaning what it says: pairing a Case
	// against the recorded draw that selected it manufactures the effect.
	FreshControlArm bool

	// NotMeasuredReason is set when CaseIDs is empty, and says why.
	NotMeasuredReason knov1.RejectionReason
}

// Plan is a whole run's routing and sampling decision.
//
// Produced before the consent quote and before any spend, so the quote is
// computed from the same object the run executes.
type Plan struct {
	// Mode is how routing was decided, fixed for the run.
	Mode Mode

	// Routed is one entry per Asset, in the order the Assets were supplied.
	Routed []AssetRouting

	// EligibleCases is the dev-split population every Asset was routed from —
	// the Cases left after the reservation and after dropping the unpairable.
	// This is Valuation.n_dev, which names that pool and not the control
	// partition: consumers scaling a delta by n_dev to estimate pool-wide
	// benefit would otherwise understate it by the reserve fraction.
	EligibleCases int

	// ControlCaseIDs are the reserved-partition Cases the harm test measures,
	// the same set for every Asset. Their control side is the recorded
	// baseline, which is valid here because the reservation is at random and
	// happened before routing.
	ControlCaseIDs []string

	// ControlUnderpowered reports whether the harm test is below
	// MinControlSample. A floor, not a certificate — see MinDetectableHarm.
	ControlUnderpowered bool

	// MinDetectableHarm is the smallest regression this run's control sample
	// could distinguish from zero, at the shipped confidence level.
	//
	// The honest form of the underpowered marker. A bool answers "is this
	// absurd", which a reader turns into "is this safe"; this answers the
	// question they were actually asking — "how big would the damage have to be
	// before this run noticed". A run reporting no regression with a minimum
	// detectable harm of 0.28 has not cleared an Asset that costs 0.10.
	//
	// Zero when the control sample is empty, which means the harm test is
	// ABSENT rather than wide: nothing was measured, so no harm size is
	// detectable at all and ControlUnderpowered is set.
	MinDetectableHarm float64

	// Trials is how many times each measurement is repeated.
	Trials int32

	// Seed is what the partition and every sample were drawn from.
	Seed int64
}

// Measurements is the ceiling this Plan implies.
//
// A ceiling on MEASUREMENTS, not on provider calls. Every attempt is billed and
// a transient failure is retried, so a run that hits one 429 makes more calls
// than this number — bounded by --max-cost-usd, which is the instrument for
// that, not by this. The quote says so in those words rather than asserting a
// bound it cannot hold.
//
// assets x (routed_sample x arms + control_sample) x trials, where arms is 2
// when the selection conditioned on the baseline outcome and 1 otherwise.
func (p *Plan) Measurements() int64 {
	var total int64
	for i := range p.Routed {
		n := int64(len(p.Routed[i].CaseIDs))
		if n == 0 {
			// Routed to nothing: no treatment arm, and no harm test either.
			// There is nothing to test for harm — the Asset is never put in
			// front of the agent at all.
			//
			// Charging it the full control arm made the cheapest answer the
			// stage gives away the most expensive line in the quote. On the
			// pool this package's own docs describe — 200 Assets, 199 matching
			// no cluster — it quoted 12,036 measurements for 96 of actual work,
			// and DESIGN.md budgets ~30% of Assets routing to zero, so this is
			// the designed-for case rather than a corner. A user shown the
			// dollar figure derived from a 125x over-quote aborts a run they
			// should have approved.
			continue
		}
		if p.Routed[i].FreshControlArm {
			n *= 2
		}
		total += n + int64(len(p.ControlCaseIDs))
	}
	trials := int64(p.Trials)
	if trials < 1 {
		trials = 1
	}
	return total * trials
}

// Route decides which Cases measure which Assets.
//
// Pure and deterministic given Options.Seed: the same inputs produce the same
// Plan, which is what makes the selection auditable after the fact from the
// seed recorded on the Run.
//
// Refuses rather than proceeding when there is nothing honest to measure, since
// every refusal here is free and the alternative is a full-price run whose
// numbers read as a result.
func Route(cases []CaseRef, assets []AssetRef, opts Options) (*Plan, error) {
	if len(cases) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("run `kno baseline` first, or widen the dev split; Value measures Assets against Cases and there are none").
			Wrap(fmt.Errorf("the dev split is empty"))
	}
	if len(assets) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("point --pool at a file with at least one Asset").
			Wrap(fmt.Errorf("the pool is empty"))
	}
	if err := checkDuplicateCases(cases); err != nil {
		return nil, err
	}
	if err := checkDuplicateAssets(assets); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()

	// The partition is drawn FIRST, from a shuffle that has seen no outcome,
	// so the reserved side is outcome-independent by construction rather than
	// by an argument about what routing happened to select.
	eligible, reserved := reserve(cases, opts)

	plan := &Plan{
		Trials: opts.Trials,
		Seed:   opts.Seed,
	}
	// "\x00control" rather than "control": the label namespace is shared
	// with Asset IDs, and an Asset named "control" would otherwise draw from
	// the identical PCG stream. Harmless while the candidate lists differ, and
	// a silent correlation the day they do not.
	plan.EligibleCases = len(eligible)
	plan.ControlCaseIDs = sampleIDs(reserved, opts.ControlSampleRate, opts.MinSample, opts.Seed, "\x00control")
	plan.MinDetectableHarm = minDetectableHarm(len(plan.ControlCaseIDs))
	// The flag is the floor OR the honest bound: below MinControlSample the
	// sample cannot see HarmMargin at all, and above it the bound itself says
	// whether the run could separate it from zero — which is the number a
	// reader should act on either way.
	plan.ControlUnderpowered = len(plan.ControlCaseIDs) < MinControlSample ||
		plan.MinDetectableHarm > HarmMargin

	clusters, mode := cluster(eligible)
	if opts.DisableRouting {
		mode = ModeAllDev
		clusters = nil
	}
	plan.Mode = mode

	plan.Routed = make([]AssetRouting, 0, len(assets))
	for _, a := range assets {
		candidates := candidatesFor(a, eligible, clusters, mode)
		r := AssetRouting{
			AssetID: a.ID,
			// DisableRouting is the ONLY outcome-blind path, so it is the only
			// one that may pair against the recorded baseline.
			//
			// `mode != ModeAllDev` looks equivalent and is not, which is worth
			// the sentence because the difference is a sign error on every null
			// Asset. ModeAllDev is reached two ways. --route none is genuinely
			// blind: nothing looked at an outcome. But "nothing failed" is
			// chosen by cluster() READING Failed over the eligible set — the
			// branch IS the conditioning. Conditional on entering it every
			// recorded score on the candidates is a pass, so a null Asset's
			// fresh draw averages p against a recorded 1 and measures p-1.
			//
			// That is the +0.70 this package exists to prevent, mirrored: a
			// systematic REGRESSION on Assets that do nothing, aimed at exactly
			// the small eval sets the stage advertises itself to. At 30 dev
			// Cases against a 95%-pass agent the branch is entered about a
			// third of the time and every Asset reports about -0.05 with a
			// tight interval.
			FreshControlArm: !opts.DisableRouting,
		}
		if len(candidates) == 0 {
			r.NotMeasuredReason = knov1.RejectionReason_REJECTION_REASON_IRRELEVANT
			plan.Routed = append(plan.Routed, r)
			continue
		}
		r.CaseIDs = sampleIDs(candidates, opts.SampleRate, opts.MinSample, opts.Seed, a.ID)
		plan.Routed = append(plan.Routed, r)
	}
	return plan, nil
}

// withDefaults fills the zero values, so a caller that supplies nothing gets a
// documented configuration rather than a silent zero sample.
func (o Options) withDefaults() Options {
	if o.SampleRate <= 0 {
		o.SampleRate = DefaultSampleRate
	}
	if o.ControlSampleRate <= 0 {
		o.ControlSampleRate = DefaultControlSampleRate
	}
	if o.ControlReserve <= 0 {
		o.ControlReserve = DefaultControlReserve
	}
	if o.Trials < 1 {
		o.Trials = 1
	}
	if o.MinSample < 1 {
		o.MinSample = DefaultMinSample
	}
	return o
}

// checkDuplicateCases refuses a repeated Case ID.
//
// A duplicate would be measured twice and counted twice, inflating the
// denominator behind an interval with a Case that contributes no independent
// information — the pseudo-replication stats/interval refuses to represent.
func checkDuplicateCases(cases []CaseRef) error {
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if c.ID == "" {
			return errs.ErrInvalidInput.
				WithFix("give every Case an id in the Evals file").
				Wrap(fmt.Errorf("a Case has no ID, so nothing can pair a measurement to it"))
		}
		if _, dup := seen[c.ID]; dup {
			return errs.ErrInvalidInput.
				WithFix("make every Case id unique in the Evals file").
				Wrap(fmt.Errorf("case %s appears twice; it would be measured twice and "+
					"counted twice, widening no interval while narrowing every one", c.ID))
		}
		seen[c.ID] = struct{}{}
	}
	return nil
}

// checkDuplicateAssets refuses a repeated or empty Asset ID.
//
// The same argument as checkDuplicateCases, one level up. Two Assets sharing an
// ID are quoted twice, draw byte-identical samples because the per-draw stream
// is keyed by the ID, and produce two Valuations that cannot be paired back to
// an Asset — so a portfolio would rank a thing it cannot name.
func checkDuplicateAssets(assets []AssetRef) error {
	seen := make(map[string]struct{}, len(assets))
	for _, a := range assets {
		if a.ID == "" {
			return errs.ErrInvalidInput.
				WithFix("give every Asset an id in the pool file").
				Wrap(fmt.Errorf("an Asset has no ID, so no Valuation could name it"))
		}
		if _, dup := seen[a.ID]; dup {
			return errs.ErrInvalidInput.
				WithFix("make every Asset id unique in the pool file").
				Wrap(fmt.Errorf("asset %s appears twice; both copies would be measured, "+
					"quoted, and ranked as if they were different Assets", a.ID))
		}
		seen[a.ID] = struct{}{}
	}
	return nil
}

// reserve splits the Cases into a routing-eligible set and a reserved set, at
// random, before any outcome is consulted.
//
// The shuffle reads only the seed. Nothing in this function looks at Failed,
// which is the property the reservation depends on and the one a later edit
// could quietly break — so it is asserted by a test over the reserved set's
// failure rate rather than left to the comment.
func reserve(cases []CaseRef, opts Options) (eligible, reserved []CaseRef) {
	order := make([]CaseRef, len(cases))
	copy(order, cases)
	// Sorted before shuffling so the caller's input order cannot change the
	// partition: two runs over the same Cases read from different adapters must
	// reserve the same set, or the seed recorded on the Run does not identify
	// the selection it claims to.
	sort.Slice(order, func(i, j int) bool { return order[i].ID < order[j].ID })

	//nolint:gosec // G404: sampling, not cryptography. Reproducibility from a
	// recorded seed is the requirement, and a CSPRNG cannot provide it. The
	// stream is PCG (specified) and the bounded draw is inlined — issue #110,
	// docs/debt.md#75, shuffle.go.
	rng := rand.New(rand.NewPCG(uint64(opts.Seed), uint64(opts.Seed)+0x9e3779b9))
	shuffle(rng, len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	n := int(float64(len(order)) * opts.ControlReserve)
	// At least one on each side whenever there are two Cases, so neither the
	// harm test nor the treatment arm silently disappears on a small eval set.
	if n < 1 && len(order) > 1 {
		n = 1
	}
	if n >= len(order) {
		n = len(order) - 1
	}
	return order[n:], order[:n]
}

// cluster groups the failed Cases by tag and reports which mode applies.
//
// The mode is a property of the RUN, decided here once, so the consent quote
// reflects the path the run will actually take.
func cluster(eligible []CaseRef) (map[string][]CaseRef, Mode) {
	var failed []CaseRef
	tagged := false
	for _, c := range eligible {
		if len(c.Tags) > 0 {
			tagged = true
		}
		if c.Failed {
			failed = append(failed, c)
		}
	}
	if len(failed) == 0 {
		// Nothing failed, so there is no failure signal to route on. Measuring
		// every Asset against everything is the honest fallback: the run is
		// more expensive and says so in the quote, rather than rejecting every
		// Asset as irrelevant and appearing to do nothing.
		return nil, ModeAllDev
	}
	if !tagged {
		return nil, ModeAllFailed
	}

	clusters := make(map[string][]CaseRef)
	for _, c := range failed {
		for _, t := range c.Tags {
			key := normalizeTag(t)
			if key == "" {
				continue
			}
			clusters[key] = append(clusters[key], c)
		}
	}
	if len(clusters) == 0 {
		// Tags exist in the dev split but every FAILED Case is untagged, so
		// there is nothing to overlap against.
		return nil, ModeAllFailed
	}
	return clusters, ModeTagOverlap
}

// candidatesFor returns the Cases an Asset may be measured against, before
// sampling.
func candidatesFor(a AssetRef, eligible []CaseRef, clusters map[string][]CaseRef, mode Mode) []CaseRef {
	switch mode {
	case ModeAllDev:
		return eligible
	case ModeAllFailed:
		return failedIn(eligible)
	case ModeTagOverlap:
		if len(a.Tags) == 0 {
			// An Asset with no tags is UNLABELLED, not irrelevant. Refusing to
			// measure it would make the modal Asset — one nobody has annotated
			// — silently unmeasurable.
			return failedIn(eligible)
		}
		seen := make(map[string]struct{})
		var out []CaseRef
		for _, t := range a.Tags {
			for _, c := range clusters[normalizeTag(t)] {
				if _, dup := seen[c.ID]; dup {
					// A Case carrying two of this Asset's tags appears in two
					// clusters and must still be measured once.
					continue
				}
				seen[c.ID] = struct{}{}
				out = append(out, c)
			}
		}
		return out
	case ModeUnspecified:
		return nil
	default:
		return nil
	}
}

// failedIn returns the failed Cases, preserving order.
func failedIn(cases []CaseRef) []CaseRef {
	var out []CaseRef
	for _, c := range cases {
		if c.Failed {
			out = append(out, c)
		}
	}
	return out
}

// normalizeTag makes tag matching case- and whitespace-insensitive.
//
// "Refunds" in a pool and "refunds" in an eval file are the same cluster to
// every human who wrote them, and an Asset that silently routes to nothing
// because of a capital letter reports REJECTION_REASON_IRRELEVANT — a
// confident, wrong answer that costs nothing and is indistinguishable from a
// correct one.
func normalizeTag(t string) string { return strings.ToLower(strings.TrimSpace(t)) }

// sampleIDs draws a reproducible sample of Case IDs.
//
// The stream is keyed by the seed AND by a per-draw label, so two draws in one
// run are independent rather than identical. Without the label every Asset with
// the same candidate count would receive the same Cases in the same order,
// which looks like a working sample and correlates every Asset's measurement
// with every other's.
func sampleIDs(candidates []CaseRef, rate float64, minSample int, seed int64, label string) []string {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	sort.Strings(ids)

	n := int(float64(len(ids))*rate + 0.5)
	if n < minSample {
		n = minSample
	}
	if n > len(ids) {
		n = len(ids)
	}
	if n == len(ids) {
		return ids
	}

	//nolint:gosec // G404: sampling, not cryptography. Reproducibility from a
	// recorded seed is the requirement, and a CSPRNG cannot provide it. The
	// stream is PCG (specified) and the bounded draw is inlined — issue #110,
	// docs/debt.md#75, shuffle.go.
	rng := rand.New(rand.NewPCG(uint64(seed), hashLabel(label)))
	shuffle(rng, len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	// Clipped: without it the returned slice keeps the full shuffle's capacity,
	// so a caller's append writes into the discarded tail rather than
	// allocating — handing them Cases that were deliberately not sampled.
	out := slices.Clip(ids[:n])
	slices.Sort(out)
	return out
}

// hashLabel derives a per-draw stream from a label. FNV-1a, inline, because the
// only requirement is that two different labels differ.
func hashLabel(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
