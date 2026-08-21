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
)

// jsonReport is the --json shape.
//
// Hand-written rather than the Run proto: this is a CLI contract aimed at a
// person's jq pipeline, and it should not shift underneath them when the
// schema gains a field.
type jsonReport struct {
	RunID      string   `json:"run_id"`
	Status     string   `json:"status"`
	Agent      string   `json:"agent"`
	Goal       string   `json:"goal"`
	DevCases   int      `json:"dev_cases"`
	Holdout    int      `json:"holdout_cases"`
	Attempted  int32    `json:"attempted"`
	Scored     int32    `json:"scored"`
	Errored    int32    `json:"errored"`
	Score      *float64 `json:"score"`
	SpentUSD   string   `json:"spent_usd"`
	Incomplete string   `json:"incomplete_reason,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

func renderJSON(
	out io.Writer,
	f baselineFlags,
	res *core.BaselineResult,
	counts jsonl.SplitCounts,
	runID string,
	warnings []string,
) error {
	rep := jsonReport{
		RunID:      runID,
		Status:     statusName(res.Run.GetStatus()),
		Agent:      f.agentRef,
		Goal:       f.goalName,
		DevCases:   counts.Dev,
		Holdout:    counts.Holdout,
		Attempted:  res.Run.GetAttemptedCaseCount(),
		Scored:     res.Run.GetScoredCaseCount(),
		Errored:    res.Run.GetErroredCaseCount(),
		Score:      res.AggregateScore,
		SpentUSD:   formatUSD(res.Spent.CostUSDMicros),
		Incomplete: res.Run.GetIncompleteReason(),
		Warnings:   warnings,
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("writing json report: %w", err)
	}
	return nil
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
