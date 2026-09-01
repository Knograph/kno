// This file is part of the --json contract. Like cli/evalinspect_json.go it
// builds the value and writeJSON in cli/jsonreport.go encodes it, so the
// encoding/json exemption stays scoped to that one filename.
//
// ADR-0006: the document is the same data as the page, not a second rendering
// of it. Every number the human page prints is a key here, including the ones
// that qualify a verdict — a caveat that survives in one renderer and not the
// other is the drift the equivalence rule exists to catch.
//
// Every float here carries at most four decimal places, and nothing in this
// file rounds: the rounding happens in judge/, where the statistic is
// computed, so the human line, this document and the goldens carry one number
// rather than three renderings of an unrounded one. That is ADR-0006 rule 6,
// and it is enforced as a property by
// TestNoJudgeJSONFloatCarriesMoreThanFourPlaces rather than by these goldens
// alone — a golden only fails on the architecture CI happens to run, which is
// how the unrounded version reached main's CI in the first place.

package cli

import (
	"math"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/judge"
)

// judgeCalibrateJSON is the `kno judge calibrate --json` shape.
type judgeCalibrateJSON struct {
	Goal    string `json:"goal"`
	Set     string `json:"set"`
	Version int    `json:"set_version"`
	SetSHA  string `json:"set_content_sha256"`

	// Source says where the judgements came from. "local" for a goal that
	// calls no model, "replay" for recorded responses, "live" for a run that
	// spent money — three values rather than a boolean, because a consumer
	// reading a kappa needs to know whether a model was involved at all.
	Source     string `json:"source"`
	PromptSHA  string `json:"prompt_sha"`
	JudgeModel string `json:"judge_model,omitempty"`

	NRecords int `json:"n_records"`
	NScored  int `json:"n_scored"`
	NErrored int `json:"n_errored"`

	Kappa            *float64           `json:"kappa"`
	KappaInterval    *judgeIntervalJSON `json:"kappa_interval"`
	RawAgreement     *float64           `json:"raw_agreement"`
	ConstantJudgeRaw *float64           `json:"constant_judge_raw_agreement"`
	Sensitivity      *float64           `json:"sensitivity"`
	Specificity      *float64           `json:"specificity"`
	SymmetryGap      *float64           `json:"symmetry_gap"`
	JudgePositive    *float64           `json:"judge_positive_rate"`
	HumanPositive    *float64           `json:"human_positive_rate"`
	InterHumanKappa  *float64           `json:"inter_human_kappa"`

	Graded *judgeGradedJSON `json:"graded,omitempty"`

	MinKappa float64 `json:"min_kappa"`
	Verdict  string  `json:"verdict"`
	Cause    string  `json:"cause,omitempty"`
	Fix      string  `json:"fix,omitempty"`

	Ratchet *judgeRatchetJSON `json:"ratchet,omitempty"`

	Disagreements []judgeDisagreementJSON `json:"disagreements,omitempty"`

	Spend *spendReport `json:"spend,omitempty"`
}

type judgeIntervalJSON struct {
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	Level  float64 `json:"level"`
	Method string  `json:"method"`
	N      int32   `json:"n"`
}

type judgeGradedJSON struct {
	WeightedKappa *float64 `json:"weighted_kappa"`
	Spearman      *float64 `json:"spearman_rho"`
	MAE           *float64 `json:"mean_absolute_error"`
	NBins         int      `json:"n_anchor_bins"`
}

type judgeRatchetJSON struct {
	Comparable    bool               `json:"comparable"`
	NotComparable string             `json:"not_comparable,omitempty"`
	BaselineKappa float64            `json:"baseline_kappa"`
	Kappa         float64            `json:"kappa"`
	Difference    *judgeIntervalJSON `json:"paired_difference,omitempty"`
	Regressed     bool               `json:"regressed"`
	ModelChanged  bool               `json:"judge_model_changed"`
}

type judgeDisagreementJSON struct {
	RecordID  string `json:"record_id"`
	Human     bool   `json:"human_passed"`
	Judge     bool   `json:"judge_passed"`
	Rationale string `json:"rationale,omitempty"`
}

// judgeCalibrateDocument is what the command emits: one entry per calibrated
// pair, plus the summary a CI gate reads.
type judgeCalibrateDocument struct {
	Calibrations []judgeCalibrateJSON `json:"calibrations"`

	// Verdict is the worst verdict across every calibration, so the common
	// case — one pair — needs no array indexing to gate on, and the --all case
	// cannot report PASS while one pair failed.
	Verdict string `json:"verdict"`
	Failed  int    `json:"failed"`
}

// newJudgeCalibrateJSON builds the document.
func newJudgeCalibrateJSON(results []*judge.Result, f judgeFlags) judgeCalibrateDocument {
	doc := judgeCalibrateDocument{Verdict: judge.VerdictPass}
	for _, res := range results {
		doc.Calibrations = append(doc.Calibrations, oneCalibrationJSON(res, f))
		if res.Failed() {
			doc.Failed++
		}
		doc.Verdict = worstVerdict(doc.Verdict, res.Verdict)
	}
	return doc
}

// worstVerdict picks the verdict a gate should act on.
//
// FAIL beats INDETERMINATE beats PASS. NOT_APPLICABLE never lowers the summary:
// a graded Goal is reported and not gated, and letting it downgrade a run's
// verdict would gate it by the back door.
func worstVerdict(a, b string) string {
	rank := map[string]int{
		judge.VerdictNotApplicable: 0,
		judge.VerdictPass:          1,
		judge.VerdictIndeterminate: 2,
		judge.VerdictFail:          3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func oneCalibrationJSON(res *judge.Result, f judgeFlags) judgeCalibrateJSON {
	out := judgeCalibrateJSON{
		Goal:       res.GoalName,
		Set:        res.SetName,
		Version:    res.SetVersion,
		SetSHA:     res.SetSHA,
		Source:     sourceWord(res),
		PromptSHA:  res.PromptSHA,
		JudgeModel: res.JudgeModel,
		NRecords:   res.NRecords,
		NScored:    res.NScored,
		NErrored:   res.NErrored,
		MinKappa:   res.MinKappa,
		Verdict:    res.Verdict,
		Cause:      res.Cause,
		Fix:        res.Fix,
	}
	if !res.BudgetStopped && res.Graded == nil {
		out.Kappa = finite(res.Agreement.Kappa)
		out.RawAgreement = finite(res.Agreement.Raw)
		out.ConstantJudgeRaw = finite(prevalence(res))
		out.Sensitivity = finite(res.Agreement.Sensitivity)
		out.Specificity = finite(res.Agreement.Specificity)
		out.SymmetryGap = finite(res.Agreement.SymmetryGap())
		out.JudgePositive = finite(res.Agreement.JudgePositiveRate)
		out.HumanPositive = finite(res.Agreement.HumanPositiveRate)
		out.KappaInterval = intervalJSON(res.KappaInterval)
	}
	out.InterHumanKappa = finite(res.InterHuman.Kappa)
	if res.Graded != nil {
		out.Graded = &judgeGradedJSON{
			WeightedKappa: finite(res.Graded.WeightedKappa),
			Spearman:      finite(res.Graded.Spearman),
			MAE:           finite(res.Graded.MAE),
			NBins:         res.Graded.NBins,
		}
	}
	if res.Ratchet != nil {
		out.Ratchet = &judgeRatchetJSON{
			Comparable:    res.Ratchet.Comparable,
			NotComparable: res.Ratchet.NotComparable,
			BaselineKappa: res.Ratchet.BaselineKappa,
			Kappa:         res.Ratchet.Kappa,
			Difference:    intervalJSON(res.Ratchet.Diff),
			Regressed:     res.Ratchet.Regressed,
			ModelChanged:  res.Ratchet.ModelChanged,
		}
	}
	if f.showDisagreements {
		for _, d := range res.Disagreements {
			out.Disagreements = append(out.Disagreements, judgeDisagreementJSON{
				RecordID:  d.RecordID,
				Human:     d.Human,
				Judge:     d.Judge,
				Rationale: d.Rationale,
			})
		}
	}
	if res.Guarded {
		s := newSpendReport(res.Spend, 0, false)
		out.Spend = &s
	}
	return out
}

func sourceWord(res *judge.Result) string {
	switch {
	case !res.Replay:
		return "live"
	case res.PromptSHA == judge.NoPromptSHA:
		return "local"
	default:
		return "replay"
	}
}

// finite renders a possibly-undefined statistic as null rather than as a
// number. NaN has no JSON spelling, and a consumer that reads 0 for
// "undefined" reads a judge no better than chance where there was no answer.
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func intervalJSON(iv *knov1.Interval) *judgeIntervalJSON {
	if iv == nil {
		return nil
	}
	return &judgeIntervalJSON{
		Low:    iv.GetLow(),
		High:   iv.GetHigh(),
		Level:  iv.GetLevel(),
		Method: iv.GetMethod(),
		N:      iv.GetNPairs(),
	}
}
