package judge

import (
	"fmt"
	"os"
	"strings"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// BaselineEntry is one recorded (set, goal) calibration.
type BaselineEntry struct {
	SetName       string
	SetVersion    int
	ContentSHA256 string
	GoalName      string
	PromptSHA     string
	JudgeModel    string
	Kappa         float64
	NRecords      int

	// Verdicts is one character per record in the set's file order: '1' pass,
	// '0' fail, '-' errored. See baselineEntryFile for why the vector and not
	// just the scalar.
	Verdicts string
}

// Baseline is judge/calibration.baseline.json.
type Baseline struct {
	Entries []BaselineEntry
	Path    string
}

// LoadBaseline reads the committed baseline.
func LoadBaseline(path string) (*Baseline, error) {
	b, err := os.ReadFile(path) //nolint:gosec // an operator-supplied flag, not user input
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix("run `make record-calibration` to create the baseline, or pass " +
				"--baseline to point at one").
			Wrap(fmt.Errorf("reading the calibration baseline: %w", err))
	}
	f, err := decodeBaseline(b)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("fix the JSON, or re-record it").Wrap(err)
	}
	out := &Baseline{Path: path}
	for _, e := range f.Entries {
		out.Entries = append(out.Entries, BaselineEntry(e))
	}
	return out, nil
}

// Find returns the entry for one (set, goal) pair.
func (b *Baseline) Find(setName, goalName string) (BaselineEntry, bool) {
	for _, e := range b.Entries {
		if e.SetName == setName && e.GoalName == goalName {
			return e, true
		}
	}
	return BaselineEntry{}, false
}

// Ratchet is the verdict of comparing a run against its recorded baseline.
type Ratchet struct {
	// Comparable is false when the two runs are not measuring the same thing.
	// NotComparable says which operand moved.
	Comparable    bool
	NotComparable string

	BaselineKappa float64
	Kappa         float64

	// Diff is the PAIRED bootstrap interval on (new kappa − recorded kappa),
	// computed by resampling records once and recomputing BOTH runs on that
	// draw. Two independent intervals differenced afterwards discard the
	// co-movement and are far too permissive.
	Diff *knov1.Interval

	// Regressed is true when the paired difference interval lies entirely
	// below zero: a drop this run's noise does not explain.
	Regressed bool

	// ModelChanged distinguishes "the judge model moved" from "the prompt
	// regressed", so a gate failure names the right operand.
	ModelChanged bool
}

// CompareToBaseline computes the ratchet for one run.
//
// One-sided by construction: a prompt change that RAISES kappa passes. The
// gate exists to stop a silent drop, not to freeze a number.
//
// A drop that the paired interval cannot distinguish from noise also passes.
// Gating on the point estimate would fire on a two-record flip in a
// sixty-record set, and a gate that fires on noise is a gate that gets
// disabled.
func CompareToBaseline(prev BaselineEntry, set *Set, judge []bool, errored []bool, opts interval.Bootstrap) Ratchet {
	r := Ratchet{BaselineKappa: prev.Kappa, ModelChanged: false}

	switch {
	case prev.SetVersion != set.Version:
		r.NotComparable = fmt.Sprintf(
			"the set moved: recorded at version %d, this run is version %d",
			prev.SetVersion, set.Version)
		return r
	case prev.ContentSHA256 != set.ContentSHA256:
		r.NotComparable = fmt.Sprintf(
			"the set's contents moved: recorded %s, this run %s",
			short(prev.ContentSHA256), short(set.ContentSHA256))
		return r
	case len(prev.Verdicts) != len(set.Records):
		r.NotComparable = fmt.Sprintf(
			"the recorded verdict vector covers %d records; this set holds %d",
			len(prev.Verdicts), len(set.Records))
		return r
	}

	// Pair only over records BOTH runs judged. A record one run errored on has
	// no paired difference, and imputing one would invent agreement.
	var a, b, human []bool
	for i, r := range set.Records {
		if errored[i] || prev.Verdicts[i] == '-' {
			continue
		}
		a = append(a, judge[i])
		b = append(b, prev.Verdicts[i] == '1')
		human = append(human, r.Adjudicated.Passed)
	}
	if len(a) < MinRecords {
		r.NotComparable = "too few records were judged by both runs to pair"
		return r
	}

	r.Comparable = true
	r.Kappa = Agree(a, human).Kappa
	r.Diff = interval.Percentile(len(a), func(idx []int) float64 {
		return KappaOver(a, human, idx) - KappaOver(b, human, idx)
	}, opts)
	if r.Diff != nil && r.Diff.GetHigh() < 0 {
		r.Regressed = true
	}
	return r
}

// VerdictVector encodes per-record judgements for the baseline file.
func VerdictVector(judge, errored []bool) string {
	var b strings.Builder
	b.Grow(len(judge))
	for i := range judge {
		switch {
		case errored[i]:
			b.WriteByte('-')
		case judge[i]:
			b.WriteByte('1')
		default:
			b.WriteByte('0')
		}
	}
	return b.String()
}
