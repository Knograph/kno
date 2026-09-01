package judge

import (
	"github.com/knograph/kno/core"
)

// Provenance sources. A calibration record is authored by a human or generated
// from a human-written template; it is never harvested.
const (
	// SourceAuthored is a record written by hand.
	SourceAuthored = "authored"

	// SourceSynthetic is a record generated from a human-written template.
	SourceSynthetic = "synthetic"
)

// HumanLabel is one person's verdict on one record.
type HumanLabel struct {
	// LabelerID identifies the person, pseudonymously. It is a roster handle
	// (`labeler-a`), never a name or an email: the set is public.
	LabelerID string

	// Value is the label on the Goal's own scale. For a binary judge it is
	// 0 or 1 and Passed carries the same information; for a graded one it is
	// the anchor the labeler chose.
	Value float64

	// Passed is the binary reading of the label — the thing kappa is computed
	// over.
	Passed bool

	// Note is why. Free text, read by humans reviewing a disagreement.
	Note string
}

// Provenance says where a record came from.
//
// It exists to make one rule checkable rather than cultural: the calibration
// set is public and permanent, and CLAUDE.md's security section says traces
// are customer data. A record derived from a real deployment cannot be
// spelled here, because the loader refuses any Source that is not one of the
// two constants above.
type Provenance struct {
	// Source is SourceAuthored or SourceSynthetic. Anything else is refused.
	Source string

	// License is the license of a record that was not written for this set.
	// Empty means original to this repository, under the repository's license.
	License string

	// Note is context a reviewer needs — the template a synthetic record came
	// from, the reason a behavior is represented.
	Note string
}

// Record is one calibration unit: an interaction, and what humans said about
// it.
//
// A record is NOT a Case. core.Goal.Score takes a Case AND a Response, so a
// judge cannot be calibrated without the agent output it is judging — a
// calibration format that carried only Cases would be missing the operand.
type Record struct {
	// ID is stable and referenced by the baseline file, by issues, and by the
	// disagreement table. Renaming one is a breaking change to those.
	ID string

	// Case is the input, rubric and tags the judge scores against.
	Case *core.Case

	// Response is the output being judged.
	Response *core.Response

	// Labels are the independent human verdicts. At least two: a record with
	// one label is a record labeled by one person's judgement, which is
	// exactly what the set exists to hold a judge to.
	Labels []HumanLabel

	// Adjudicated is the reference verdict — what the set says is true.
	// Required, so the set can never contain an unresolved disagreement.
	Adjudicated HumanLabel

	// Provenance says where this record came from.
	Provenance Provenance
}

// Set is a loaded calibration set.
type Set struct {
	// Name is the set's directory name and half of the baseline key.
	Name string

	// Version is monotonic and the other half of the baseline key. A set edit
	// and a prompt edit are therefore distinguishable: the gate reports which
	// operand moved.
	Version int

	// ContentSHA256 is the hash of records.jsonl as committed. It is what makes
	// a route around the gate — deleting the records a judge fails — visible.
	ContentSHA256 string

	// Labelers is the roster, in the manifest so a reader can see how many
	// people the ceiling rests on.
	Labelers []string

	// Description says what the set covers and what it deliberately does not.
	Description string

	// Records are in file order, which is the order the baseline's verdict
	// vector is indexed by.
	Records []Record

	// Source is where the set was read from, for error messages. The builtin
	// set reports the embedded path.
	Source string
}

// MinorityShare is the fraction of records whose adjudicated verdict is in the
// smaller class.
//
// The balance invariant is checked against this. Kappa is depressed by extreme
// prevalence — the well-known kappa paradox — and the mitigation chosen here
// is to fix balance at authoring time rather than to swap in a statistic that
// reports a flattering number on a lopsided set.
func (s *Set) MinorityShare() float64 {
	if len(s.Records) == 0 {
		return 0
	}
	passed := 0
	for _, r := range s.Records {
		if r.Adjudicated.Passed {
			passed++
		}
	}
	failed := len(s.Records) - passed
	minority := passed
	if failed < minority {
		minority = failed
	}
	return float64(minority) / float64(len(s.Records))
}

// Reference returns the adjudicated verdicts in record order.
func (s *Set) Reference() []bool {
	out := make([]bool, len(s.Records))
	for i, r := range s.Records {
		out[i] = r.Adjudicated.Passed
	}
	return out
}
