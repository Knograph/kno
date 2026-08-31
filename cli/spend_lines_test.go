package cli

import (
	"strings"
	"testing"

	"github.com/knograph/kno/stats/budget"
)

// TestSpendLinesQualifiersAppearOnlyWhenTheyApply is the plan's AC7 and AC14's
// human half.
//
// The estimated-usage qualifier is the schema's own caveat —
// CaseExecution.usage_estimated_case_count, recorded since v0.1 and rendered
// by nothing until now: "the spend figure is a guess to this extent, and says
// so." It said so nowhere. The resumed note is here rather than only in
// --json because §6's case for reporting run-lifetime spend rests on what a
// CI log shows, and a CI log shows this rendering.
//
// The zero cases are asserted as hard as the non-zero ones: a caveat printed
// unconditionally is noise, and noise is what gets filtered out before the
// one run where it mattered.
func TestSpendLinesQualifiersAppearOnlyWhenTheyApply(t *testing.T) {
	t.Parallel()

	spend := budget.Spend{Calls: 8250, CostUSDMicros: 2_410_000, Tokens: 91}

	tests := []struct {
		name        string
		estimated   int32
		resumed     bool
		wantNotes   []string
		absentNotes []string
	}{
		{
			name:        "neither qualifier applies",
			absentNotes: []string{"engine's estimate", "resumed run"},
		},
		{
			name:        "priced from the estimate",
			estimated:   412,
			wantNotes:   []string{"412 case(s) priced from the engine's estimate"},
			absentNotes: []string{"resumed run"},
		},
		{
			name:        "resumed",
			resumed:     true,
			wantNotes:   []string{"resumed run — this figure covers earlier sessions"},
			absentNotes: []string{"engine's estimate"},
		},
		{
			name:      "both",
			estimated: 3,
			resumed:   true,
			wantNotes: []string{"3 case(s) priced", "resumed run"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			if err := spendLines(&b, spend, tt.estimated, tt.resumed); err != nil {
				t.Fatalf("spendLines: %v", err)
			}
			got := b.String()

			// The spend line itself, byte-identical to the one kno baseline
			// has printed since v0.1. The golden would catch a drift here too;
			// this says which line drifted.
			const want = "  spent      $2.41 over 8250 call(s)\n"
			if !strings.HasPrefix(got, want) {
				t.Errorf("spend line = %q, want prefix %q", got, want)
			}
			for _, note := range tt.wantNotes {
				if !strings.Contains(got, note) {
					t.Errorf("missing note %q:\n%s", note, got)
				}
			}
			for _, note := range tt.absentNotes {
				if strings.Contains(got, note) {
					t.Errorf("note %q printed when it does not apply:\n%s", note, got)
				}
			}
		})
	}
}

// TestSpendReportMirrorsTheHumanQualifiers pins the --json half to the same
// conditions: omitempty carries the distinction, so a qualifier that does not
// apply is an absent key rather than a zero.
func TestSpendReportMirrorsTheHumanQualifiers(t *testing.T) {
	t.Parallel()

	spend := budget.Spend{Calls: 4, CostUSDMicros: 1_500_000, Tokens: 20}

	quiet := newSpendReport(spend, 0, false)
	if !quiet.Guarded {
		t.Error("a stage that reached newSpendReport ran a guard; guarded must be true")
	}
	if quiet.SpentUSD != "$1.50" || quiet.SpentUSDMicros != 1_500_000 {
		t.Errorf("the string and the integer disagree: %q vs %d",
			quiet.SpentUSD, quiet.SpentUSDMicros)
	}
	if quiet.LLMCalls != 4 {
		t.Errorf("llm_calls = %d, want 4", quiet.LLMCalls)
	}
	if quiet.UsageEstimatedCases != 0 || quiet.Resumed {
		t.Errorf("qualifiers set when they do not apply: %+v", quiet)
	}

	loud := newSpendReport(spend, 7, true)
	if loud.UsageEstimatedCases != 7 || !loud.Resumed {
		t.Errorf("qualifiers dropped when they apply: %+v", loud)
	}
}

// TestReportSpendMarksIncompleteSourceRun is the plan's AC12: a total
// assembled from a run that stopped early is a FLOOR, and must not pass for a
// complete cost in either rendering.
func TestReportSpendMarksIncompleteSourceRun(t *testing.T) {
	t.Parallel()

	s := newReportSpend(
		"base-1", budget.Spend{Calls: 400, CostUSDMicros: 310_000},
		"value-1", budget.Spend{Calls: 12_480, CostUSDMicros: 23_800_000},
		true,
	)
	if s.TotalUSDMicros != 24_110_000 || s.TotalLLMCalls != 12_880 {
		t.Errorf("the total does not sum its parts: %+v", s)
	}
	if s.TotalUSD != "$24.11" {
		t.Errorf("total_usd = %q, want $24.11", s.TotalUSD)
	}
	if !s.Incomplete {
		t.Error("incomplete was not carried through")
	}
	if s.NoMeteredSpend {
		t.Error("no_metered_spend set on a pipeline that spent $24.11")
	}

	var b strings.Builder
	spendCostSection(&b, s)
	page := b.String()
	for _, want := range []string{
		"## Cost", "| baseline | `base-1` | $0.31 | 400 |",
		"| value | `value-1` | $23.80 | 12480 |",
		"**$24.11**", "floor on what the pipeline cost",
		"absent from this table rather than listed at zero",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Cost section is missing %q:\n%s", want, page)
		}
	}
}
