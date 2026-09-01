package core

import (
	"context"
	"fmt"
	"sort"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// ExplainRow is one shared Case in a redundancy comparison's per-Case table
// — `kno select --explain`'s "way to actually check it": free, read-only, no
// provider call, over the shared slice a REDUNDANT verdict rests on.
type ExplainRow struct {
	// CaseID identifies the Case.
	CaseID string

	// Baseline is the recorded baseline score for this Case.
	Baseline float64

	// ThisDelta is the explained Asset's sign-corrected delta on this Case.
	ThisDelta float64

	// ThisImproved reports whether this Case counted as an improvement
	// (ThisDelta > 0) for Condition 2's co-improvement set.
	ThisImproved bool

	// WithDelta and WithImproved are the same two facts for the
	// already-selected Asset this comparison is against.
	WithDelta    float64
	WithImproved bool
}

// ExplainComparison is one already-selected Asset a candidate's redundancy
// verdict rests on: the shared per-Case table for BOTH sides, so a reader who
// disagrees with a REDUNDANT claim can see the disjoint columns for
// themselves.
type ExplainComparison struct {
	// WithAssetID is the already-selected Asset compared against.
	WithAssetID string

	// Evidence is the recorded RedundancyEvidence for this pair.
	Evidence *knov1.RedundancyEvidence

	// Rows is the per-Case table over the shared slice: one row per Case in
	// C, both Assets' deltas side by side, in ascending Case ID order —
	// deterministic, so the same query prints the same table twice. Empty
	// for content evidence, which makes no per-Case claim.
	Rows []ExplainRow
}

// Explain reconstructs the per-Case evidence table for one Asset's
// redundancy comparisons against a Select decision computed from o —
// `kno select --explain <asset-id>`'s core.
//
// Free, read-only, and makes no provider call: it re-runs the SAME pure
// decision core Select uses (core/select.go's decide) in memory, without
// writing a Run or a Portfolio, and reads only the two runs the holdout
// canary permits (o.ValueRunID's Measurements, and that run's recorded
// baseline's CaseScores) — the same posture Select itself has.
//
// Returns (nil, nil) when assetID was not rejected REDUNDANT by this
// decision — that is not an error, it is "there is nothing to explain".
func (o SelectOptions) Explain(ctx context.Context, assetID string, level float64) ([]ExplainComparison, error) {
	if level == 0 {
		level = defaultLevel
	}
	if err := o.validate(level); err != nil {
		return nil, err
	}

	source, err := o.Store.GetRun(ctx, o.ValueRunID)
	if err != nil {
		return nil, fmt.Errorf("loading the source Value run %s: %w", o.ValueRunID, err)
	}
	if got := source.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return nil, fmt.Errorf("run %s is a %s run, not a Value run", o.ValueRunID, got)
	}

	valuations, err := o.Store.Valuations(ctx, o.ValueRunID)
	if err != nil {
		return nil, fmt.Errorf("reading Valuations for %s: %w", o.ValueRunID, err)
	}
	var assets map[string]*Asset
	if o.Pool != nil {
		assets, err = loadAssetsByID(ctx, o.Pool)
		if err != nil {
			return nil, err
		}
	}

	p, _, err := o.decide(ctx, valuations, assets, level, source.GetBaselineRunId(), source.GetGoalDirection())
	if err != nil {
		return nil, err
	}

	var rej *Rejection
	for _, r := range p.GetRejected() {
		if r.GetAssetId() == assetID {
			rej = r
			break
		}
	}
	if rej == nil || rej.GetReason() != knov1.RejectionReason_REJECTION_REASON_REDUNDANT {
		return nil, nil
	}

	byID := make(map[string]*Valuation, len(valuations))
	for _, v := range valuations {
		byID[v.GetAssetId()] = v
	}
	thisV, ok := byID[assetID]
	if !ok {
		return nil, nil
	}

	var baseline map[string]store.CaseScore
	if bid := source.GetBaselineRunId(); bid != "" {
		baseline, err = o.Store.CaseScores(ctx, bid)
		if err != nil {
			return nil, fmt.Errorf("reading baseline scores for %s: %w", bid, err)
		}
	}
	reader := newCaseDeltaReader(ctx, o.Store, o.ValueRunID, baseline, source.GetGoalDirection())
	thisDelta, err := reader.deltasFor(thisV)
	if err != nil {
		return nil, err
	}

	var out []ExplainComparison
	for _, ev := range rej.GetRedundancyEvidence() {
		cmp := ExplainComparison{WithAssetID: ev.GetWithAssetId(), Evidence: ev}
		if ev.GetKind() != knov1.RedundancyEvidenceKind_REDUNDANCY_EVIDENCE_KIND_MEASUREMENT {
			// Content evidence has no per-Case table: it is not a claim
			// about Cases at all.
			out = append(out, cmp)
			continue
		}
		withV, ok := byID[ev.GetWithAssetId()]
		if !ok {
			out = append(out, cmp)
			continue
		}
		withDelta, err := reader.deltasFor(withV)
		if err != nil {
			return nil, err
		}
		for _, c := range sharedCases(withDelta, thisDelta) {
			b := baseline[c]
			td, wd := thisDelta[c], withDelta[c]
			cmp.Rows = append(cmp.Rows, ExplainRow{
				CaseID: c, Baseline: b.Value,
				ThisDelta: td, ThisImproved: td > 0,
				WithDelta: wd, WithImproved: wd > 0,
			})
		}
		out = append(out, cmp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WithAssetID < out[j].WithAssetID })
	return out, nil
}
