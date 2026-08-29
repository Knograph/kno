// This file holds the --json contract, and it is the only place in the CLI
// that encodes with encoding/json.
//
// ADR-0001 bans encoding/json because proto3 JSON encodes int64 as quoted
// strings and enums as names, so using it on a kno.v1 type silently diverges
// from the generated OpenAPI spec. That reasoning is about kno.v1 types. This
// file encodes a hand-written struct aimed at somebody's jq pipeline —
// protojson would force the CLI's output shape to mirror the proto's field
// names and presence rules, which is exactly what a stable CLI contract must
// not do.
//
// The exemption is scoped to this filename so it cannot spread to code that
// touches kno.v1 types.

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// jsonReport is the --json shape.
//
// Hand-written rather than the Run proto: this is a CLI contract aimed at a
// person's jq pipeline, and it should not shift underneath them when the
// schema gains a field.
type jsonReport struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Agent    string `json:"agent"`
	Goal     string `json:"goal"`
	DevCases int    `json:"dev_cases"`
	Holdout  int    `json:"holdout_cases"`
	// WeakLabelCases is how many Cases in the eval set carry derived
	// provenance — mined from transcripts rather than authored. Present when
	// nonzero; a machine consumer deciding whether exact-match over a mined
	// set is honest needs the number on the record.
	WeakLabelCases int32    `json:"weak_label_case_count,omitempty"`
	Attempted      int32    `json:"attempted"`
	Scored         int32    `json:"scored"`
	Errored        int32    `json:"errored"`
	Score          *float64 `json:"score"`
	// ScoreUnavailable distinguishes the two reasons score can be null. A
	// machine consumer reading only `"score": null` beside `"scored": 20`
	// cannot tell a run that scored nothing from one whose numbers cannot be
	// read back, and those call for different responses: the first is a broken
	// run, the second is intact data with a lost measurement.
	ScoreUnavailable bool   `json:"score_unavailable,omitempty"`
	SpentUSD         string `json:"spent_usd"`

	// EstimatedUSD is what the run expected to spend before it started.
	//
	// Present so a --json run that waived the prompt still records the figure
	// it waived — the human line cannot be printed there, because a prose line
	// ahead of the document makes stdout unparseable. Beside spent_usd it also
	// answers "was the estimate any good", which is the question a cost cap's
	// honesty rests on.
	EstimatedUSD string `json:"estimated_usd,omitempty"`

	// Concurrency is what the run actually executed at, and why.
	//
	// Hand-written rather than embedding *knov1.ConcurrencyDecision: that
	// would re-couple this contract to the proto and emit its int64 fields as
	// bare numbers, which is the divergence from the generated OpenAPI spec
	// ADR-0001 bans.
	Concurrency       int32    `json:"concurrency"`
	ConcurrencyAsked  int32    `json:"concurrency_requested,omitempty"`
	ConcurrencyReason string   `json:"concurrency_reduced_reason,omitempty"`
	Incomplete        string   `json:"incomplete_reason,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

func renderJSON(
	out io.Writer,
	f baselineFlags,
	opts core.BaselineOptions,
	res *core.BaselineResult,
	counts jsonl.SplitCounts,
	runID string,
	warnings []string,
) error {
	rep := jsonReport{
		RunID:    runID,
		Status:   statusName(res.Run.GetStatus()),
		Agent:    f.agentRef,
		Goal:     f.goalName,
		DevCases: counts.Dev,
		Holdout:  counts.Holdout,
		// From CaseExecution, which is aggregated from what is durable rather
		// than from in-memory counters — so it survives a crash and stays
		// correct across a resume. The flat counters on Run still carry the
		// same numbers and are still written; this reads the one that has
		// presence, per docs/debt.md#26.
		Attempted:        res.Run.GetCaseExecution().GetAttemptedCaseCount(),
		Scored:           res.Run.GetCaseExecution().GetScoredCaseCount(),
		Errored:          res.Run.GetCaseExecution().GetErroredCaseCount(),
		Score:            res.AggregateScore,
		ScoreUnavailable: res.AggregateUnavailable,
		SpentUSD:         formatUSD(res.Spent.CostUSDMicros),
		WeakLabelCases:   res.Run.GetWeakLabelCaseCount(),
		Incomplete:       res.Run.GetIncompleteReason(),
		Warnings:         warnings,
	}
	concurrencyFields(&rep, res.Run)
	if perCall := core.PlanningCostPerCall(opts); perCall > 0 {
		rep.EstimatedUSD = formatUSD(perCall * int64(counts.Dev))
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("writing json report: %w", err)
	}
	return nil
}

// writeJSON encodes any hand-written report shape.
//
// Shared by the baseline report and by `kno doctor`, both of which are
// hand-written structs for the same reason: a CLI contract aimed at somebody's
// jq pipeline must not shift when a proto message gains a field (ADR-0001).
func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("writing json report: %w", err)
	}
	return nil
}

// concurrencyFields fills the three concurrency keys from the Run's record.
//
// Absent on a Run that recorded no decision — a resume with no Cases left
// returns before checkFeasible — so the extra keys are omitempty rather than
// zero, because 0 is not a width anything ran at.
func concurrencyFields(rep *jsonReport, run *knov1.Run) {
	d := run.GetConcurrency()
	if d == nil {
		return
	}
	rep.Concurrency = d.GetEffective()
	// The REASON is the discriminator, not requested != effective: `requested`
	// is absent when the user named no width, so a default run would otherwise
	// report having been reduced from zero.
	if d.GetReason() != knov1.ConcurrencyReason_CONCURRENCY_REASON_UNSPECIFIED {
		// Asked stays absent when the user named no width, rather than
		// reporting a request of zero. omitempty carries that distinction.
		rep.ConcurrencyAsked = d.GetRequested()
		rep.ConcurrencyReason = concurrencyReasonName(d.GetReason())
	}
}

// decodeReport parses a rendered report. Used by tests, which would otherwise
// need their own encoding/json import.
func decodeReport(b []byte) (jsonReport, error) {
	var rep jsonReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return jsonReport{}, fmt.Errorf("decoding report: %w", err)
	}
	return rep, nil
}

// decodeRaw parses a rendered report into a map, so a test can assert which
// keys the contract carries.
func decodeRaw(b []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decoding report: %w", err)
	}
	return raw, nil
}

// valueReport is the JSON shape of a Value run. Deltas travel beside their
// intervals, or the reason they are absent — the same discipline the proto
// godocs now enforce. Lives in this file because the --json contract is the
// one encoding/json exemption the depguard config grants, for the same reason
// as jsonReport: a hand-written shape aimed at a jq pipeline, not a kno.v1
// type.
type valueReport struct {
	RunID         string                 `json:"run_id"`
	Status        string                 `json:"status"`
	GoalDirection string                 `json:"goal_direction"`
	Valuations    []valueReportValuation `json:"valuations"`
}

type valueReportValuation struct {
	AssetID      string   `json:"asset_id"`
	NotMeasured  string   `json:"not_measured,omitempty"`
	DeltaGoal    *float64 `json:"delta_goal,omitempty"`
	Low          *float64 `json:"low,omitempty"`
	High         *float64 `json:"high,omitempty"`
	DeltaControl *float64 `json:"delta_control,omitempty"`
	ControlLow   *float64 `json:"control_low,omitempty"`
	NPairs       int32    `json:"n_pairs"`
	NDropped     int32    `json:"n_dropped"`
	NRouted      int32    `json:"n_routed"`
	NDev         int32    `json:"n_dev"`
}

// renderValueJSON emits the machine-readable Value report.
func renderValueJSON(out io.Writer, res *core.ValueResult, runID string, dir float64) error {
	rep := valueReport{RunID: runID, Status: res.Status.String(), GoalDirection: res.GoalDirection.String()}
	for _, v := range res.Valuations {
		row := valueReportValuation{
			AssetID:     v.GetAssetId(),
			NotMeasured: v.GetNotMeasured().String(),
			NPairs:      v.GetNPairs(),
			NDropped:    v.GetNDropped(),
			NRouted:     v.GetNRouted(),
			NDev:        v.GetNDev(),
		}
		if iv := v.GetDeltaInterval(); iv != nil {
			// Un-negated like the human report: the document carries
			// goal_direction, so a MINIMIZE consumer can read the Goal's own
			// units.
			d := dir * v.GetDeltaGoal()
			low, high := dir*iv.GetLow(), dir*iv.GetHigh()
			row.DeltaGoal, row.Low, row.High = &d, &low, &high
		}
		if iv := v.GetControlInterval(); iv != nil {
			d := dir * v.GetDeltaControl()
			low := dir * iv.GetLow()
			row.DeltaControl, row.ControlLow = &d, &low
		}
		rep.Valuations = append(rep.Valuations, row)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("writing the value report: %w", err)
	}
	return nil
}

// selectReport is the JSON shape of a Select run.
//
// The Portfolio's proto shape is a wire type that will gain fields; this is a
// CLI contract aimed at somebody's jq pipeline, hand-written for the same
// reason as the reports above (ADR-0001).
type selectReport struct {
	RunID        string                  `json:"run_id"`
	Status       string                  `json:"status"`
	SourceRunID  string                  `json:"source_run_id"`
	SourceStatus string                  `json:"source_status"`
	Budget       selectReportBudget      `json:"budget"`
	Selected     []selectReportEntry     `json:"selected"`
	Rejected     []selectReportRejection `json:"rejected"`
	// DevGain is absent when nothing was selected — no claim, no row.
	DevGain       *float64   `json:"dev_estimated_gain,omitempty"`
	GainLow       *float64   `json:"dev_estimated_low,omitempty"`
	GainHigh      *float64   `json:"dev_estimated_high,omitempty"`
	DegradedRules []string   `json:"degraded_rules,omitempty"`
	TotalCost     costReport `json:"total_cost"`
}

// selectReportBudget carries the caps the Portfolio was built under, as the
// flag names that set them. Absent caps are absent keys.
type selectReportBudget struct {
	MaxContextTokens    int64  `json:"max_context_tokens,omitempty"`
	MaxTrainingExamples int64  `json:"max_training_examples,omitempty"`
	MaxCostUSD          string `json:"max_cost_usd,omitempty"`
}

// selectReportEntry is one selected Asset and the measurement behind the
// decision, in selection order. The interval is the recorded one; the
// keep/reject decision used the Bonferroni-corrected interval, which the
// schema does not carry per entry.
type selectReportEntry struct {
	AssetID      string   `json:"asset_id"`
	Destination  string   `json:"destination"`
	Rank         int32    `json:"rank"`
	DeltaGoal    *float64 `json:"delta_goal,omitempty"`
	Low          *float64 `json:"low,omitempty"`
	High         *float64 `json:"high,omitempty"`
	NRoutedScale *float64 `json:"n_routed_scale,omitempty"`
}

// selectReportRejection is one excluded Asset and why. RedundantWith names
// the already-selected Assets it duplicates when the reason is "redundant".
type selectReportRejection struct {
	AssetID       string   `json:"asset_id"`
	Reason        string   `json:"reason"`
	Detail        string   `json:"detail,omitempty"`
	RedundantWith []string `json:"redundant_with,omitempty"`
}

// costReport is the carrying cost of the selected set, dollars rendered like
// every other dollar in the CLI contract.
type costReport struct {
	ContextTokens  int64  `json:"context_tokens"`
	FTTokens       int64  `json:"ft_tokens"`
	AcquisitionUSD string `json:"acquisition_usd"`
}

// renderSelectJSON emits the machine-readable Select report.
func renderSelectJSON(out io.Writer, res *core.SelectResult) error {
	p := res.Portfolio
	rep := selectReport{
		RunID:        res.RunID,
		Status:       res.Status.String(),
		SourceRunID:  p.GetSourceRunId(),
		SourceStatus: statusName(p.GetSourceStatus()),
		Budget: selectReportBudget{
			MaxContextTokens:    p.GetBudget().GetMaxContextTokens(),
			MaxTrainingExamples: p.GetBudget().GetMaxTrainingExamples(),
		},
		DegradedRules: res.DegradedRules,
		TotalCost: costReport{
			ContextTokens:  p.GetTotalCost().GetContextTokens(),
			FTTokens:       p.GetTotalCost().GetFtTokens(),
			AcquisitionUSD: formatUSD(p.GetTotalCost().GetAcquisitionUsdMicros()),
		},
	}
	if b := p.GetBudget(); b.GetMaxCostUsdMicros() > 0 {
		rep.Budget.MaxCostUSD = formatUSD(b.GetMaxCostUsdMicros())
	}
	for _, e := range p.GetSelected() {
		row := selectReportEntry{
			AssetID:     e.GetAssetId(),
			Destination: destinationName(e.GetDestination()),
			Rank:        e.GetRank(),
		}
		if v := e.GetValuation(); v.GetDeltaInterval() != nil {
			iv := v.GetDeltaInterval()
			row.DeltaGoal, row.Low, row.High = &v.DeltaGoal, &iv.Low, &iv.High
		}
		if e.NRoutedScale != nil {
			row.NRoutedScale = e.NRoutedScale
		}
		rep.Selected = append(rep.Selected, row)
	}
	for _, r := range p.GetRejected() {
		rep.Rejected = append(rep.Rejected, selectReportRejection{
			AssetID:       r.GetAssetId(),
			Reason:        rejectReasonName(r.GetReason()),
			Detail:        r.GetDetail(),
			RedundantWith: r.GetRedundantWithAssetIds(),
		})
	}
	if iv := p.GetDevEstimatedInterval(); iv != nil {
		rep.DevGain, rep.GainLow, rep.GainHigh = &p.DevEstimatedGain, &iv.Low, &iv.High
	}
	return writeJSON(out, rep)
}

// exportReport is the JSON shape of an Export run.
type exportReport struct {
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	SelectRunID  string `json:"select_run_id"`
	Destination  string `json:"destination"`
	AssetCount   int    `json:"asset_count"`
	BytesWritten int64  `json:"bytes_written"`
	Path         string `json:"path"`
	Manifest     string `json:"manifest_path"`
}

// renderExportJSON emits the machine-readable Export report.
func renderExportJSON(out io.Writer, res *core.ExportResult) error {
	rep := exportReport{
		RunID:        res.RunID,
		Status:       "completed",
		Destination:  destinationName(res.Destination),
		AssetCount:   res.AssetCount,
		BytesWritten: res.BytesWritten,
		Path:         res.Path,
		Manifest:     res.Path + ".manifest.md",
	}
	return writeJSON(out, rep)
}
