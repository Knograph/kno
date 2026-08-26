// Package exactmatch scores a Response by exact string equality.
package exactmatch

import (
	"context"
	"strings"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Goal scores 1 when the output matches the Case's expected answer, 0
// otherwise.
//
// Deliberately the simplest possible Goal. It exists so the pipeline can be
// exercised end to end without an LLM judge, and so that a run's numbers are
// verifiable by hand — a property no judged Goal has.
type Goal struct {
	// TrimSpace ignores leading and trailing whitespace.
	TrimSpace bool

	// CaseInsensitive ignores letter case.
	CaseInsensitive bool
}

// Name identifies this Goal in reports and on the Run.
func (g *Goal) Name() string { return "exact-match" }

// Score compares the Response's output to the Case's expected answer.
func (g *Goal) Score(_ context.Context, c *core.Case, r *core.Response) (*core.Score, error) {
	got, want := r.GetOutput(), c.GetExpected()
	if g.TrimSpace {
		got, want = strings.TrimSpace(got), strings.TrimSpace(want)
	}
	if g.CaseInsensitive {
		got, want = strings.ToLower(got), strings.ToLower(want)
	}

	passed := got == want
	value := 0.0
	rationale := "output did not match the expected answer"
	if passed {
		value = 1
		rationale = "output matched the expected answer exactly"
	}

	return &knov1.Score{
		CaseId:    c.GetId(),
		Value:     value,
		Passed:    passed,
		Rationale: rationale,
		// No judge model: this Goal does not use one, and naming a model it
		// did not consult would misrepresent how the number was produced.
	}, nil
}

// Direction reports that higher is better.
//
// Without this the sign of every delta measured against this Goal would be
// uninterpretable.
func (g *Goal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// Domain reports that this Goal's Scores are binary.
//
// Exact match is a verdict: an answer either equals the expected string or it
// does not, so Score.value is 0.0 or 1.0 and never anything between. A paired
// difference of two such Scores lands in {-1, 0, +1} — the McNemar setting,
// where the information is in the discordant counts and the right interval is
// a score-based one on paired proportions. A bootstrap returns zero width on
// this data, which reads as certainty.
func (g *Goal) Domain() core.ScoreDomain {
	return knov1.ScoreDomain_SCORE_DOMAIN_BINARY
}

var _ core.Goal = (*Goal)(nil)
