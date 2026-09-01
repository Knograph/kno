// Redundancy detection for Select: is a candidate Asset's measured effect
// equivalent to, and co-located with, an already-selected Asset's?
//
// The 2026-08-31 redundancy-detection plan is the spec. Two independent
// evidence paths, scoped differently on purpose (finding F3):
//
//   - MEASUREMENT evidence compares per-Case delta vectors reconstructed from
//     store.Measurements and store.CaseScores, within the SAME destination,
//     for any Kind — the mechanism argument: two Assets are substitutes when
//     they compete for the same vehicle and the same cap.
//   - CONTENT evidence is the shipped shingle rule, unchanged: KIND_KNOWLEDGE
//     only, destination-blind, at the existing 0.6 threshold. Kept exactly as
//     `main` computes it so criterion 7's compatibility golden means
//     something.
//
// When measurement evidence exists (the shared routed slice reaches
// MinOverlapCases and both delta vectors are recoverable) it decides, and
// content is recorded only as corroboration. Content decides only when
// measurement evidence does not exist.

package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/stats/portfolio"
	"github.com/knograph/kno/store"
)

// MinOverlapCases is the minimum shared routed slice two Assets need before a
// redundancy verdict may be REDUNDANT or DISTINCT rather than UNKNOWN.
//
// Reuses core.MinClusterCases rather than inventing a second number for the
// same "did we look?" question — CLAUDE.md's vocabulary rule extends to
// numbers doing the same job under two names. If 5 is wrong here, it is wrong
// in gaps.go too, and both move together. See docs/debt.md#160.
const MinOverlapCases = MinClusterCases

// defaultRedundancyMaxMargin is the ceiling on the equivalence margin delta
// (--redundancy-max-margin). An effect a tenth of the Goal's range apart is
// not the same effect for any Goal this tool has shipped.
const defaultRedundancyMaxMargin = 0.10

// redundancyBootstrapIterations is the resample count for the co-improvement
// Jaccard's bootstrap interval. Fixed rather than configurable: it is an
// implementation precision knob, not a statistical claim a user should tune.
const redundancyBootstrapIterations = 2000

// redundancyCostBiasBand is the factor #68 names: two Assets whose
// context_tokens differ by less than this cannot be distinguished by that
// estimate, so the cost tie-break refuses to decide between them (finding
// F2). See docs/debt.md#68 and docs/debt.md#161.
const redundancyCostBiasBand = 2.4

// maxRedundancyMultiplicityPasses bounds the fixed-point search for
// n_redundancy_tests (see decideWithRedundancy's doc). Bounded rather than
// exact: the count is a function of which pairs are found REDUNDANT, which is
// a function of the corrected interval, which is a function of the count.
// Each pass either reaches a fixed point or the loop takes the last pass's
// count — conservative in the sense that matters (docs/debt.md#157), not
// exact in the sense a pre-registered correction would be.
const maxRedundancyMultiplicityPasses = 4

// redundancyConfig is the resolved --redundancy-* flag set.
type redundancyConfig struct {
	// margin is the user-supplied floor for Condition 1's equivalence margin
	// (--redundancy-margin). Zero means "the sample's own resolution
	// decides".
	margin float64

	// maxMargin is the ceiling (--redundancy-max-margin). Zero means
	// defaultRedundancyMaxMargin.
	maxMargin float64

	// minCoImprovement is the user-supplied floor for Condition 2's
	// co-improvement Jaccard (--redundancy-min-coimprovement). Zero means
	// "beyond chance decides".
	minCoImprovement float64
}

func (c redundancyConfig) resolvedMaxMargin() float64 {
	if c.maxMargin <= 0 {
		return defaultRedundancyMaxMargin
	}
	return c.maxMargin
}

// selectedForRedundancy is one already-selected Asset, carried forward so
// later candidates can be compared against it — measurement evidence within
// its destination, content evidence when both sides are KIND_KNOWLEDGE.
type selectedForRedundancy struct {
	assetID   string
	dest      knov1.Destination
	kind      knov1.Kind
	valuation *Valuation
	asset     *Asset // nil when no Pool was supplied
}

// caseDeltaVector reconstructs one Asset's per-Case, sign-corrected,
// trial-averaged delta over its own routed slice, from durably recorded
// measurements and the baseline's recorded scores.
//
// Mirrors core/value_measure.go's `pairs` under PAIRING_SCHEME_RECORDED_
// BASELINE exactly: a Case is included only when the treatment arm has at
// least one recoverable, non-errored measurement AND the baseline has a
// recoverable score for it. Anything else is DROPPED, never zero-filled —
// acceptance criterion 19 — because a delta computed against a substitute
// zero is not a delta anyone measured.
//
// Returns an empty (non-nil) map, never an error, when the Asset simply has
// no usable Cases — that is the honest UNKNOWN input, not a failure.
func caseDeltaVector(
	recorded []store.RecordedMeasurement,
	baseline map[string]store.CaseScore,
	caseIDs []string,
	direction knov1.Direction,
) map[string]float64 {
	dir := 1.0
	if direction == knov1.Direction_DIRECTION_MINIMIZE {
		dir = -1.0
	}

	// Group treatment scores by Case, trial-averaged — same shape as
	// value_measure.go's byCase + perCaseMeans, collapsed into one pass since
	// this reader needs only the per-Case mean, not the trial vector.
	sums := make(map[string]float64)
	counts := make(map[string]int)
	for _, m := range recorded {
		if m.Key.Arm != store.ArmTreatment || m.Err != "" || m.Unrecoverable {
			continue
		}
		sums[m.Key.CaseID] += m.Score
		counts[m.Key.CaseID]++
	}

	out := make(map[string]float64, len(caseIDs))
	for _, id := range caseIDs {
		n, ok := counts[id]
		if !ok || n == 0 {
			continue
		}
		b, ok := baseline[id]
		if !ok || b.Unrecoverable {
			continue
		}
		treatment := sums[id] / float64(n)
		out[id] = dir * (treatment - b.Value)
	}
	return out
}

// sharedCases returns the sorted intersection of two delta vectors' keys,
// restricted to Case IDs both Assets were actually routed to and measured
// for — the C the plan's two conditions are computed over.
func sharedCases(a, b map[string]float64) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	var out []string
	for id := range a {
		if _, ok := b[id]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// equivalenceMargin derives Condition 1's delta: the larger of the user's
// floor and the sample's own resolution, MinDetectableEffect(|C|, TWO_SIDED,
// level) — symmetric to Condition 2's chance floor. Reports which term won so
// the evidence can say so (finding F1's margin_source, mirrored for margin).
func equivalenceMargin(userMargin float64, n int, level float64) (float64, knov1.MarginSource) {
	resolution := interval.MinDetectableEffect(n, knov1.Sidedness_SIDEDNESS_TWO_SIDED, level)
	if userMargin > resolution {
		return userMargin, knov1.MarginSource_MARGIN_SOURCE_USER
	}
	return resolution, knov1.MarginSource_MARGIN_SOURCE_SAMPLE_RESOLUTION
}

// withinMargin reports whether a two-sided interval lies entirely inside
// +/- margin — the TOST decision rule for Condition 1.
func withinMargin(iv *Interval, margin float64) bool {
	if iv == nil || margin <= 0 {
		return false
	}
	return iv.GetLow() >= -margin && iv.GetHigh() <= margin
}

// improvementSet reports, for each Case in the shared slice C (already
// sorted), whether the Asset's delta counts as an improvement: delta > 0 for
// every domain this stage sees. There is no separate continuous noise floor
// here — the equivalence and chance-floor machinery already absorbs sampling
// noise; a second, ad hoc floor here would be an invented threshold the plan
// does not name.
func improvementSet(delta map[string]float64, cases []string) []bool {
	out := make([]bool, len(cases))
	for i, c := range cases {
		out[i] = delta[c] > 0
	}
	return out
}

// jaccard is the Jaccard similarity of two same-length boolean improvement
// sets over the shared slice.
func jaccard(a, b []bool) float64 {
	inter, union := 0, 0
	for i := range a {
		switch {
		case a[i] && b[i]:
			inter++
			union++
		case a[i] || b[i]:
			union++
		}
	}
	if union == 0 {
		return math.NaN()
	}
	return float64(inter) / float64(union)
}

// deterministicSeed derives a stable uint64 seed from a string key, so the
// co-improvement bootstrap (interval.Percentile) — the one place this stage
// uses randomness — produces byte-identical Portfolios across runs over the
// same store (acceptance criterion 12). FNV-1a rather than crypto/rand: this
// is a resampling seed, not a security boundary. Zero is avoided because
// interval.Percentile reads a zero Bootstrap.Seed as "use the package
// default" rather than "seed with zero" — collapsing every zero-hashing key
// onto one shared stream would be a real, if vanishingly unlikely, collision.
func deterministicSeed(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	s := h.Sum64()
	if s == 0 {
		s = 1
	}
	return s
}

// round4 rounds a POINT ESTIMATE to four decimal places, and roundInterval
// does the same to both bounds of an Interval — the same treatment
// judge/kappa.go applies to every statistic it reports, for the same two
// reasons. First: Go permits fusing a multiply-add into one FMA instruction,
// and arm64 fuses several of the expressions this file's arithmetic goes
// through (the t-quantile bisection behind interval.Paired, the mean-of-
// differences here) where amd64 does not, so the tail digits of an unrounded
// statistic genuinely differ by architecture — a value recorded on darwin/
// arm64 can fail to match the same computation on linux/amd64 bit for bit.
// Second: none of these numbers carry that much precision anyway. A
// bootstrap over a few dozen shared Cases, or a t-interval over the same,
// does not resolve to the fifteenth decimal place, and printing seventeen
// digits is a false-precision claim in a document whose whole job is honest
// reporting. Rounding happens HERE, at construction, not in the CLI
// renderer, so the human line, `--json`, and any consumer reading the proto
// directly all see the same number.
//
// Applied only to numbers already used to DECIDE — every redundancy verdict
// in this file compares withinMargin/coIv.GetLow() against the UNROUNDED
// values before evidence is ever built, so rounding here cannot flip a
// verdict; it only changes what gets reported about one already made.
func round4(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	return math.Round(f*1e4) / 1e4
}

// roundInterval rounds an Interval's bounds to four decimal places, to
// nearest rather than outward — see round4's godoc and judge/kappa.go's
// roundInterval, which makes the same choice for the same reason: outward
// rounding is LESS stable at the values these methods actually produce (a
// bound that already sits on the 1e-4 grid), and the width it buys back is
// three orders of magnitude below either method's real uncertainty.
func roundInterval(iv *Interval) *Interval {
	if iv == nil {
		return nil
	}
	out := &Interval{
		Level:     iv.GetLevel(),
		Method:    iv.GetMethod(),
		Sidedness: iv.GetSidedness(),
		Low:       round4(iv.GetLow()),
		High:      round4(iv.GetHigh()),
	}
	if n := iv.GetNPairs(); n != 0 {
		out.NPairs = &n
	}
	return out
}

// correctRedundancyInterval widens iv for nTests pairwise comparisons, the
// same Bonferroni discipline stats/portfolio.Correct applies to keep/reject
// intervals — but corrected against n_redundancy_tests, a different test
// family counted separately per the plan. Fewer than two tests need no
// correction, mirroring correctedLevel's own floor.
func correctRedundancyInterval(iv *Interval, nTests int) *Interval {
	if nTests < 2 {
		return iv
	}
	return portfolio.Correct(iv, nTests)
}

// measurementVerdict is one pairwise measurement-evidence comparison: whether
// Condition 1 and Condition 2 both hold, and the evidence behind the answer
// either way. n_overlap and level are always meaningful; the rest are set
// only when a real test was attempted (|C| >= MinOverlapCases).
type measurementVerdict struct {
	// attempted is true when |C| >= MinOverlapCases and a test was actually
	// run — what n_redundancy_tests counts.
	attempted bool

	// redundant is Condition 1 AND Condition 2, both true.
	redundant bool

	evidence *knov1.RedundancyEvidence
}

// evaluateMeasurement runs both conditions for one (candidate, already-
// selected) pair and returns the verdict plus its evidence, corrected for
// nTestsAssumed pairwise comparisons.
//
// withID/withDelta is the already-selected Asset (the "with" side of the
// evidence, and the reference the candidate's difference is measured
// against); candDelta is the candidate's own delta vector.
func evaluateMeasurement(
	withID string,
	withDelta map[string]float64,
	candDelta map[string]float64,
	cfg redundancyConfig,
	level float64,
	nTestsAssumed int,
	rngKey string,
) measurementVerdict {
	shared := sharedCases(withDelta, candDelta)
	if len(shared) < MinOverlapCases {
		return measurementVerdict{}
	}

	diffs := make([]float64, len(shared))
	for i, c := range shared {
		diffs[i] = withDelta[c] - candDelta[c]
	}
	//nolint:gosec // bounded by the eval set
	n := int32(len(shared))
	rawDiffIv := interval.Paired(diffs, knov1.ScoreDomain_SCORE_DOMAIN_CONTINUOUS_UNBOUNDED, 1, level)
	if rawDiffIv == nil {
		return measurementVerdict{attempted: true}
	}
	diffIv := correctRedundancyInterval(rawDiffIv, nTestsAssumed)
	if diffIv == nil {
		return measurementVerdict{attempted: true}
	}

	margin, marginSource := equivalenceMargin(cfg.margin, len(shared), level)
	cond1 := margin <= cfg.resolvedMaxMargin() && withinMargin(diffIv, margin)

	withImproved := improvementSet(withDelta, shared)
	candImproved := improvementSet(candDelta, shared)
	a, b := countTrue(withImproved), countTrue(candImproved)
	jChance := interval.JChance(a, b, len(shared))
	floor := jChance
	floorSource := knov1.CoImprovementFloorSource_CO_IMPROVEMENT_FLOOR_SOURCE_CHANCE
	if !math.IsNaN(jChance) && cfg.minCoImprovement > jChance {
		floor = cfg.minCoImprovement
		floorSource = knov1.CoImprovementFloorSource_CO_IMPROVEMENT_FLOOR_SOURCE_USER
	}

	rawJ := jaccard(withImproved, candImproved)
	var cond2 bool
	var coIv *Interval
	if !math.IsNaN(jChance) && !math.IsNaN(rawJ) {
		// Computed AT the corrected level directly rather than built at the
		// base level and rescaled afterward: stats/portfolio.Correct only
		// knows how to rescale the parametric methods (t, adjusted-Wald,
		// sign) by a quantile ratio, and a percentile bootstrap has no
		// quantile to ratio — it is exact at whatever level it is resampled
		// for. Asking for the corrected level up front is both correct and
		// simpler than teaching Correct a method it cannot derive a ratio for.
		//
		// interval.Percentile (stats/interval/bootstrap.go) is judge-
		// calibrate's (#177), which merged first and so owns the package's
		// one percentile-bootstrap implementation, per the plan's own
		// "whichever merges first owns it" call. This is the second
		// consumer, not a second implementation.
		coIv = interval.Percentile(len(shared),
			func(idx []int) float64 { return jaccardResample(withImproved, candImproved, idx) },
			interval.Bootstrap{
				Resamples: redundancyBootstrapIterations,
				Level:     correctedLevel(level, nTestsAssumed),
				Seed:      deterministicSeed(rngKey),
				Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
				Support:   &interval.Support{Low: 0, High: 1}, // a Jaccard index never leaves [0, 1]
			})
		cond2 = coIv != nil && coIv.GetLow() > floor
	}

	ev := &knov1.RedundancyEvidence{
		WithAssetId:              withID,
		Kind:                     knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT,
		NOverlap:                 n,
		PairedDifference:         round4(mean(diffs)),
		DifferenceInterval:       roundInterval(diffIv),
		Margin:                   round4(margin),
		MarginSource:             marginSource,
		CoImprovement:            round4(rawJ),
		CoImprovementInterval:    roundInterval(coIv),
		CoImprovementFloor:       round4(floor),
		CoImprovementFloorSource: floorSource,
	}
	return measurementVerdict{attempted: true, redundant: cond1 && cond2, evidence: ev}
}

// jaccardResample is the bootstrap statistic: the Jaccard of two boolean
// improvement sets over a resample of shared-slice indices.
func jaccardResample(a, b []bool, idx []int) float64 {
	inter, union := 0, 0
	for _, i := range idx {
		switch {
		case a[i] && b[i]:
			inter++
			union++
		case a[i] || b[i]:
			union++
		}
	}
	if union == 0 {
		return math.NaN()
	}
	return float64(inter) / float64(union)
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// mean is defined in core/validate_measure.go and reused here.

// costTieBreak decides which of two measurement-equivalent Assets survives,
// per finding F2: cost decides only when the two carrying costs differ by
// more than the docs/debt.md#68 bias band (redundancyCostBiasBand); inside
// the band the estimate cannot support the claim and Asset ID decides
// instead — deterministic and unbiased rather than a guess dressed as a
// measurement.
//
// Returns the ID of the SURVIVOR, the decision criterion, and the ratio of
// the more expensive cost to the cheaper (>= 1, or 0 when a cost is
// unavailable).
func costTieBreak(aID string, aCost int64, bID string, bCost int64) (survivor string, decidedBy knov1.RedundancyDecidedBy, ratio float64) {
	if aCost > 0 && bCost > 0 {
		hi, lo := float64(aCost), float64(bCost)
		if hi < lo {
			hi, lo = lo, hi
		}
		ratio = hi / lo
		if ratio > redundancyCostBiasBand {
			if aCost < bCost {
				return aID, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST, ratio
			}
			return bID, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST, ratio
		}
	}
	if aID < bID {
		return aID, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID, ratio
	}
	return bID, knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID, ratio
}

// contextTokensOf reads the carrying-cost figure the tie-break compares, 0
// when absent — costTieBreak treats 0 as "unavailable" and falls through to
// ID.
//
// Read from the VALUATION's copy of CostVector, not the Pool Asset's:
// Valuation.cost exists precisely so a Valuation is self-contained
// (valuation.proto field 10), and criterion 8 requires the measurement path
// to decide redundancy — tie-break included — with no Pool at all.
//
// Simplification, stated rather than hidden: the plan's worked tie-break
// varies the compared figure by destination (context_tokens for CONTEXT,
// content bytes for KNOWLEDGE_BASE, "one example" for TUNING_SET). This
// implementation uses context_tokens uniformly — the exact field
// docs/debt.md#68 discusses the bias of — rather than branching on
// destination; a knowledge-base byte comparison needs Asset content, which a
// poolless run does not have, and unifying on the field the bias band is
// ABOUT keeps the guard's own reasoning intact even where it is not the
// destination-ideal metric.
func contextTokensOf(v *Valuation) int64 {
	return v.GetCost().GetContextTokens()
}

// caseDeltaReader caches per-Asset delta vectors for one decide() pass, so a
// candidate compared against several already-selected Assets — and an
// already-selected Asset compared against several later candidates — reads
// the store at most once per Asset.
type caseDeltaReader struct {
	ctx        context.Context //nolint:containedctx // scoped to one decide() pass, mirrors the pass-scoped caches around it
	store      store.Store
	valueRunID string
	baseline   map[string]store.CaseScore
	direction  knov1.Direction
	cache      map[string]map[string]float64
}

func newCaseDeltaReader(ctx context.Context, st store.Store, valueRunID string, baseline map[string]store.CaseScore, direction knov1.Direction) *caseDeltaReader {
	return &caseDeltaReader{
		ctx: ctx, store: st, valueRunID: valueRunID, baseline: baseline, direction: direction,
		cache: make(map[string]map[string]float64),
	}
}

// forget drops one Asset's cached delta vector. Called once an Asset is
// finally decided and is NOT part of run.selected — a rejected candidate's
// vector is never read again, and holding it for the rest of the pass is
// exactly the "materialized for all Assets at once" shape CLAUDE.md's
// streaming-memory rule (iter.Seq is load-bearing) exists to avoid at pool
// scale. An Asset that IS selected keeps its cache entry: later candidates
// still need to compare against it.
func (r *caseDeltaReader) forget(assetID string) {
	delete(r.cache, assetID)
}

// deltasFor returns v's Asset's per-Case delta vector, reading the store on
// first use and caching thereafter. Returns an empty map (never nil, never an
// error for "nothing to read") when no baseline is available or the Asset was
// routed to nothing.
func (r *caseDeltaReader) deltasFor(v *Valuation) (map[string]float64, error) {
	id := v.GetAssetId()
	if d, ok := r.cache[id]; ok {
		return d, nil
	}
	if len(r.baseline) == 0 || len(v.GetCaseIds()) == 0 {
		r.cache[id] = map[string]float64{}
		return r.cache[id], nil
	}
	recorded, err := r.store.Measurements(r.ctx, r.valueRunID, id)
	if err != nil {
		return nil, fmt.Errorf("reading measurements for %s: %w", id, err)
	}
	d := caseDeltaVector(recorded, r.baseline, v.GetCaseIds(), r.direction)
	r.cache[id] = d
	return d, nil
}

// redundancyOutcome is one candidate's redundancy verdict against every
// already-selected Asset it is comparable to.
type redundancyOutcome struct {
	// candidateLoses is true when at least one already-selected Asset beats
	// this candidate — by measurement equivalence plus the cost tie-break, or
	// by content. The candidate is rejected REDUNDANT.
	candidateLoses bool

	// withIDs / evidence are the losing candidate's rejection: one entry per
	// already-selected Asset that beat it.
	withIDs  []string
	evidence []*knov1.RedundancyEvidence

	// evictIDs / evictEvidence are set only when candidateLoses is false and
	// the candidate beat every measurement-equivalent Asset it matched: those
	// Assets are evicted from the Portfolio and re-recorded as REDUNDANT
	// against this candidate.
	evictIDs      []string
	evictEvidence map[string]*knov1.RedundancyEvidence

	// testsPerformed is how many pairwise measurement tests this candidate's
	// comparisons actually ran (|C| >= MinOverlapCases), for the caller's
	// n_redundancy_tests count. Content-only comparisons do not count: they
	// are not the statistical test family the multiplicity correction covers.
	testsPerformed int
}

// evaluateRedundancyForCandidate compares one candidate against every
// already-selected Asset it is comparable to, and decides the outcome.
//
// Per pair: measurement evidence (same destination, any Kind) decides
// whenever it exists (|C| >= MinOverlapCases and both delta vectors are
// recoverable); content evidence (both KIND_KNOWLEDGE, destination-blind, the
// shipped 0.6 shingle threshold) decides only when measurement evidence does
// not exist for that pair — finding F3's split. A measurement-equivalent pair
// is resolved by costTieBreak; a content-decided pair always loses the
// candidate, first-seen-wins, exactly as `main` computes it today.
func (o SelectOptions) evaluateRedundancyForCandidate(
	v *Valuation,
	asset *Asset,
	dest knov1.Destination,
	selectedAssets []selectedForRedundancy,
	reader *caseDeltaReader,
	cfg redundancyConfig,
	level float64,
	nTestsAssumed int,
) (redundancyOutcome, error) {
	var out redundancyOutcome
	candDelta, err := reader.deltasFor(v)
	if err != nil {
		return out, err
	}

	type match struct {
		id string
		ev *knov1.RedundancyEvidence
	}
	var losing []match
	type winMatch struct {
		id  string
		sel selectedForRedundancy
		ev  *knov1.RedundancyEvidence
	}
	var winning []winMatch

	// Sorted by Asset ID: comparison order must not depend on map iteration
	// or on the order Assets happened to be selected in, or two runs over the
	// same store could disagree on which pair's bootstrap RNG key is drawn
	// first — moot for the RNG itself (it's keyed by ID, not draw order) but
	// load-bearing for keeping this loop's OWN behavior reviewable and
	// independent of caller iteration order.
	ordered := append([]selectedForRedundancy(nil), selectedAssets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].assetID < ordered[j].assetID })

	bothKnowledge := func(sel selectedForRedundancy) bool {
		return asset != nil && sel.asset != nil &&
			kindOf(v) == knov1.Kind_KIND_KNOWLEDGE && sel.kind == knov1.Kind_KIND_KNOWLEDGE
	}

	for _, sel := range ordered {
		if sel.dest == dest {
			withDelta, err := reader.deltasFor(sel.valuation)
			if err != nil {
				return out, err
			}
			rngKey := fmt.Sprintf("%s|with=%s|cand=%s", o.ValueRunID, sel.assetID, v.GetAssetId())
			mv := evaluateMeasurement(sel.assetID, withDelta, candDelta, cfg, level, nTestsAssumed, rngKey)
			if mv.attempted {
				out.testsPerformed++
				if mv.redundant {
					ev := mv.evidence
					if bothKnowledge(sel) {
						ev.ShingleOverlap = round4(shingleOverlap(shingles(asset.GetContent()), shingles(sel.asset.GetContent())))
					}
					survivor, decidedBy, ratio := costTieBreak(
						sel.assetID, contextTokensOf(sel.valuation), v.GetAssetId(), contextTokensOf(v),
					)
					ev.CostRatio = round4(ratio)
					ev.DecidedBy = decidedBy
					if survivor == sel.assetID {
						losing = append(losing, match{id: sel.assetID, ev: ev})
					} else {
						rngKey2 := fmt.Sprintf("%s|with=%s|cand=%s", o.ValueRunID, v.GetAssetId(), sel.assetID)
						mv2 := evaluateMeasurement(v.GetAssetId(), candDelta, withDelta, cfg, level, nTestsAssumed, rngKey2)
						ev2 := mv2.evidence
						if ev2 == nil {
							// Defensive only: mv attempted and formed evidence
							// over the same shared slice, so the mirrored call
							// should too. Falling back to ev keeps the
							// eviction's own rejection non-empty rather than
							// silently dropping the evidence.
							ev2 = ev
						} else {
							ev2.CostRatio = round4(ratio)
							ev2.DecidedBy = decidedBy
							ev2.ShingleOverlap = ev.ShingleOverlap
						}
						winning = append(winning, winMatch{id: sel.assetID, sel: sel, ev: ev2})
					}
					continue
				}
				// Measurement evidence existed and decided DISTINCT: content
				// is not consulted for this pair, per F3.
				continue
			}
		}
		// Measurement evidence unavailable for this pair (different
		// destination, or insufficient overlap): fall back to content,
		// exactly as `main` computes it — knowledge-only, destination-blind.
		if bothKnowledge(sel) {
			so := shingleOverlap(shingles(asset.GetContent()), shingles(sel.asset.GetContent()))
			if so >= defaultShingleOverlap {
				losing = append(losing, match{id: sel.assetID, ev: &knov1.RedundancyEvidence{
					WithAssetId:    sel.assetID,
					Kind:           knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE,
					ShingleOverlap: round4(so),
					DecidedBy:      knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_CONTENT,
				}})
			}
		}
	}

	if len(losing) > 0 {
		out.candidateLoses = true
		for _, m := range losing {
			out.withIDs = append(out.withIDs, m.id)
			out.evidence = append(out.evidence, m.ev)
		}
		return out, nil
	}
	if len(winning) > 0 {
		out.evictIDs = make([]string, 0, len(winning))
		out.evictEvidence = make(map[string]*knov1.RedundancyEvidence, len(winning))
		for _, w := range winning {
			out.evictIDs = append(out.evictIDs, w.id)
			out.evictEvidence[w.id] = w.ev
		}
	}
	return out, nil
}

// redundancyDetail renders the rejection's prose from its evidence, in the
// numbers a reader needs to disagree — the plan's own worked example shape.
func redundancyDetail(evidence []*knov1.RedundancyEvidence) string {
	if len(evidence) == 0 {
		return "redundant"
	}
	parts := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		switch ev.GetKind() {
		case knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT:
			diff := ev.GetDifferenceInterval()
			co := ev.GetCoImprovementInterval()
			s := fmt.Sprintf("equivalent to %s on %d shared Cases (paired difference %+.4f, CI [%+.4f, %+.4f] inside +/-%.4f)",
				ev.GetWithAssetId(), ev.GetNOverlap(), ev.GetPairedDifference(), diff.GetLow(), diff.GetHigh(), ev.GetMargin())
			if co != nil {
				s += fmt.Sprintf("; co-improved (J=%.2f, CI [%.2f, %.2f], against %.2f expected by chance)",
					ev.GetCoImprovement(), co.GetLow(), co.GetHigh(), ev.GetCoImprovementFloor())
			}
			switch ev.GetDecidedBy() {
			case knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_COST:
				s += fmt.Sprintf("; decided by cost (%.1fx apart, beyond the estimate's ~%.1fx content-type bias)",
					ev.GetCostRatio(), redundancyCostBiasBand)
			case knov1.RedundancyDecidedBy_REDUNDANCY_DECIDED_BY_ID:
				s += "; decided by Asset ID (costs indistinguishable)"
			}
			parts = append(parts, s)
		case knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_CONTENT_SHINGLE:
			parts = append(parts, fmt.Sprintf("duplicates %s by content (shingle overlap %.2f >= %.2f)",
				ev.GetWithAssetId(), ev.GetShingleOverlap(), defaultShingleOverlap))
		default:
			parts = append(parts, fmt.Sprintf("duplicates %s", ev.GetWithAssetId()))
		}
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; also " + p
	}
	return out
}
