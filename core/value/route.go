package value

import (
	"fmt"
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

	// ModeAllDev means no selection was made on the baseline outcome: either
	// the baseline failed nothing in the routing-eligible partition, so there
	// is no failure signal to route on, or the caller switched routing off.
	// Every Asset is measured against a sample of every eligible Case.
	//
	// It is the one mode whose selection is outcome-INDEPENDENT, which is why
	// it is also the one mode that needs no fresh control arm.
	ModeAllDev
)

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
	// bound is roughly +/-0.3, so a true -0.10 regression reads as no
	// regression. The rate is set so a default-sized run clears MinSample with
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

// MinControlSample is the number of control measurements below which a harm
// bound is marked underpowered.
//
// Below this the one-sided bound is wide enough that a real regression and a
// clean result are indistinguishable, and Valuation.control_underpowered exists
// so that shows up as "untested" rather than "safe".
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

	// ControlCaseIDs are the reserved-partition Cases the harm test measures,
	// the same set for every Asset. Their control side is the recorded
	// baseline, which is valid here because the reservation is at random and
	// happened before routing.
	ControlCaseIDs []string

	// ControlUnderpowered reports whether the harm test is too small to
	// distinguish a real regression from a clean result.
	ControlUnderpowered bool

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
	opts = opts.withDefaults()

	// The partition is drawn FIRST, from a shuffle that has seen no outcome,
	// so the reserved side is outcome-independent by construction rather than
	// by an argument about what routing happened to select.
	eligible, reserved := reserve(cases, opts)

	plan := &Plan{
		Trials: opts.Trials,
		Seed:   opts.Seed,
	}
	plan.ControlCaseIDs = sampleIDs(reserved, opts.ControlSampleRate, opts.MinSample, opts.Seed, "control")
	plan.ControlUnderpowered = len(plan.ControlCaseIDs) < MinControlSample

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
			// Every mode but ModeAllDev selected on the baseline outcome.
			FreshControlArm: mode != ModeAllDev,
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

	//nolint:gosec // sampling, not cryptography; reproducibility is the requirement
	rng := rand.New(rand.NewPCG(uint64(opts.Seed), uint64(opts.Seed)+0x9e3779b9))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

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

	//nolint:gosec // sampling, not cryptography; reproducibility is the requirement
	rng := rand.New(rand.NewPCG(uint64(seed), hashLabel(label)))
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	out := ids[:n]
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
