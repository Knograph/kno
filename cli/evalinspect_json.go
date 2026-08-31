// This file is part of the --json contract. Like cli/jsonreport.go it encodes
// hand-written structs aimed at somebody's jq pipeline rather than kno.v1
// types, for the reason ADR-0001 gives — but it declares no encoder of its
// own: it builds the value and writeJSON in cli/jsonreport.go encodes it, so
// the encoding/json exemption stays scoped to that one filename.

package cli

// The `kno eval inspect --json` shape.
//
// Keys are stable from the first release. The `checks` array's `name` values
// are the part people pin CI to, so renaming one is a breaking change with a
// CHANGELOG note. Floats are unrounded: a consumer choosing its own threshold
// needs the number, not the two digits the human table prints.
//
// Every key here is a fact the eval source or a recorded run supplied. There
// is deliberately no score decomposition — no Goal in this build populates
// Score.components and no store reader surfaces them, so a percentage of
// total score would be derived from nothing (docs/debt.md).

// inspectJSON is the document.
type inspectJSON struct {
	Evals     string                `json:"evals"`
	Cases     inspectCasesJSON      `json:"cases"`
	Behaviors []inspectBehaviorJSON `json:"behaviors"`

	// CollapsedSpellings is how many distinct raw spellings collapsed into
	// the single most-collapsed behavior, which CollapsedTag names. Absent
	// when every tag was written one way.
	CollapsedSpellings int    `json:"collapsed_spellings,omitempty"`
	CollapsedTag       string `json:"collapsed_tag,omitempty"`

	UntaggedDevCases      int `json:"untagged_dev_cases"`
	MultiBehaviorDevCases int `json:"multi_behavior_dev_cases"`

	// MultiBehaviorShare is REPORTED AND NEVER FLAGGED. It appears in no
	// check, and sweeping it from 0 to 1 moves checks_flagged by zero.
	MultiBehaviorShare float64 `json:"multi_behavior_share"`

	// DominantBehavior is the tag carrying the most dev Cases, over ALL dev
	// Cases including untagged ones, non-exclusively. Absent when no dev Case
	// carries a tag.
	DominantBehavior *inspectDominantJSON `json:"dominant_behavior,omitempty"`

	// The three data-dependent observations. Absent when zero, because zero
	// is nothing to report rather than a finding.
	BlankTagRefs        int `json:"blank_tag_refs,omitempty"`
	DuplicateTagRefs    int `json:"duplicate_tag_refs,omitempty"`
	UnscoreableDevCases int `json:"unscoreable_dev_cases,omitempty"`
	UnsplitCases        int `json:"unsplit_cases,omitempty"`

	Checks        []inspectCheckJSON `json:"checks"`
	ChecksFlagged int                `json:"checks_flagged"`
	ChecksTotal   int                `json:"checks_total"`

	Suggestions []string `json:"suggestions,omitempty"`

	// Notes[0] is the standing conditional, always. A consumer that reads
	// only the first note reads the one that qualifies every per-tag number
	// in the document.
	Notes []string `json:"notes"`

	Observed *inspectObservedJSON `json:"observed,omitempty"`
}

// inspectCasesJSON is the split, from CountSplits.
type inspectCasesJSON struct {
	Total     int `json:"total"`
	Dev       int `json:"dev"`
	Holdout   int `json:"holdout"`
	WeakLabel int `json:"weak_label"`
}

// inspectBehaviorJSON is one normalized tag.
type inspectBehaviorJSON struct {
	Tag      string `json:"tag"`
	DevCases int    `json:"dev_cases"`

	// SeparableEffect is TWO-SIDED, and Sidedness says so on every entry so a
	// jq consumer cannot mistake it for observed.min_detectable_harm, which
	// is one-sided.
	SeparableEffect float64 `json:"separable_effect"`
	Sidedness       string  `json:"sidedness"`
	Level           float64 `json:"level"`

	Status    string `json:"status"`
	Spellings int    `json:"spellings"`
}

// inspectDominantJSON is the concentration finding.
type inspectDominantJSON struct {
	Tag      string  `json:"tag"`
	DevCases int     `json:"dev_cases"`
	Share    float64 `json:"share"`
}

// inspectCheckJSON is one flaggable check. Detail is prose and may change;
// Name and Status are the contract.
type inspectCheckJSON struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// inspectObservedJSON is what a recorded Value run's routing did. Absent
// without --value-run-id, and absent when the eval source has changed since
// the run.
type inspectObservedJSON struct {
	ValueRunID    string `json:"value_run_id"`
	BaselineRunID string `json:"baseline_run_id,omitempty"`
	RunStatus     string `json:"run_status"`
	RoutingMode   string `json:"routing_mode"`

	// EvalSourceMatchesRun is always true here: a mismatch withholds this
	// whole object. Carried anyway so a consumer reading `observed` knows the
	// join was checked rather than assumed.
	EvalSourceMatchesRun bool `json:"eval_source_matches_run"`

	ControlCases        int  `json:"control_cases"`
	ControlUnderpowered bool `json:"control_underpowered"`

	// MinDetectableHarm is Plan.MinDetectableHarm verbatim and therefore
	// ONE-SIDED — the directional "did this get worse" question. Never
	// comparable to behaviors[].separable_effect, which is two-sided.
	MinDetectableHarm          float64 `json:"min_detectable_harm"`
	MinDetectableHarmSidedness string  `json:"min_detectable_harm_sidedness"`

	Behaviors []inspectObservedBehaviorJSON `json:"behaviors"`
}

// inspectObservedBehaviorJSON is one plan cluster and its verdict.
type inspectObservedBehaviorJSON struct {
	Tag              string  `json:"tag"`
	ClusterCases     int     `json:"cluster_cases"`
	FailedAtBaseline int     `json:"failed_at_baseline"`
	GapStatus        string  `json:"gap_status"`
	BestAssetID      string  `json:"best_asset_id,omitempty"`
	BestDelta        float64 `json:"best_delta"`
	CoveredCount     int     `json:"covered_count"`
}

// The two sidedness labels. Spelled out rather than derived from the enum:
// SIDEDNESS_TWO_SIDED is a wire name, and this is a CLI contract.
const (
	sidednessTwoSided = "two-sided"
	sidednessOneSided = "one-sided"
)

// jsonReport builds the document from the analysis.
//
// Both renderings read the same inspection, so the checks array and the human
// findings cannot disagree — the equivalence golden pins that.
func (i *inspection) jsonReport() inspectJSON {
	doc := inspectJSON{
		Evals: i.Source,
		Cases: inspectCasesJSON{
			Total:     i.Counts.Total(),
			Dev:       i.Counts.Dev,
			Holdout:   i.Counts.Holdout,
			WeakLabel: i.Counts.WeakLabelCases,
		},
		Behaviors:             make([]inspectBehaviorJSON, 0, len(i.Behaviors)),
		CollapsedSpellings:    i.CollapsedSpellings,
		CollapsedTag:          i.CollapsedTag,
		UntaggedDevCases:      i.UntaggedDevCases,
		MultiBehaviorDevCases: i.MultiBehaviorDevCases,
		MultiBehaviorShare:    i.share(i.MultiBehaviorDevCases),
		BlankTagRefs:          i.BlankTagRefs,
		DuplicateTagRefs:      i.DuplicateTagRefs,
		UnscoreableDevCases:   i.UnscoreableDevCases,
		UnsplitCases:          i.Unsplit,
		Checks:                make([]inspectCheckJSON, 0, len(i.Checks)),
		ChecksFlagged:         i.flaggedCount(),
		ChecksTotal:           checksTotal,
		Suggestions:           inspectSuggestions(i),
		Notes:                 []string{standingConditional, noteSeparableEffect, noteMultiBehavior},
	}

	for _, b := range i.Behaviors {
		doc.Behaviors = append(doc.Behaviors, inspectBehaviorJSON{
			Tag:             b.Tag,
			DevCases:        b.DevCases,
			SeparableEffect: b.SeparableEffect,
			Sidedness:       sidednessTwoSided,
			Level:           inspectLevel,
			Status:          b.Status,
			Spellings:       b.Spellings,
		})
	}
	if d := i.dominant(); d != nil {
		doc.DominantBehavior = &inspectDominantJSON{
			Tag:      d.Tag,
			DevCases: d.DevCases,
			Share:    i.share(d.DevCases),
		}
	}
	for _, c := range i.Checks {
		// A conversion, not a literal: check and inspectCheckJSON carry the
		// same three fields in the same order, and a field added to one
		// without the other becomes a compile error rather than a key that
		// silently stops being emitted.
		doc.Checks = append(doc.Checks, inspectCheckJSON(c))
	}
	if i.Observed != nil {
		doc.Observed = observedJSON(i.Observed)
	}
	return doc
}

// observedJSON builds the observed object.
func observedJSON(obs *observed) *inspectObservedJSON {
	out := &inspectObservedJSON{
		ValueRunID:                 obs.ValueRunID,
		BaselineRunID:              obs.BaselineRunID,
		RunStatus:                  statusName(obs.RunStatus),
		RoutingMode:                obs.RoutingMode,
		EvalSourceMatchesRun:       true,
		ControlCases:               obs.ControlCases,
		ControlUnderpowered:        obs.ControlUnderpowered,
		MinDetectableHarm:          obs.MinDetectableHarm,
		MinDetectableHarmSidedness: sidednessOneSided,
		Behaviors:                  make([]inspectObservedBehaviorJSON, 0, len(obs.Behaviors)),
	}
	for _, b := range obs.Behaviors {
		out.Behaviors = append(out.Behaviors, inspectObservedBehaviorJSON{
			Tag:              b.Tag,
			ClusterCases:     b.ClusterCases,
			FailedAtBaseline: b.FailedAtBaseline,
			GapStatus:        b.GapStatus.String(),
			BestAssetID:      b.BestAssetID,
			BestDelta:        b.BestDelta,
			CoveredCount:     b.CoveredCount,
		})
	}
	return out
}
