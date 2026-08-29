package hf

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/knograph/kno/adapters/internal/datasetserver"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The column vocabulary. The input column is the first present of these, the
// expected column the first present of those — decided once per split from
// the first row, because a column's presence is a dataset fact, not a
// per-row guess. A dataset whose rows disagree about their own shape is
// answered with the no-input refusal, which names the columns it actually
// has.
var (
	inputCandidates    = []string{"input", "prompt", "question"}
	expectedCandidates = []string{"expected", "completion", "answer"}
)

// columns is the dataset-level mapping decision for one split.
type columns struct {
	// input is the column read as the Case input. Empty means the split has
	// no input column, which is refused at open unless the split is empty.
	input string

	// expected is the single-winner expected column. Empty means the split
	// carries no expectation — those Cases score against rubric alone.
	expected string
}

// discoverColumns decides the mapping from the first page's rows. First
// present wins; other candidates are dropped — a golden with two winners is
// a dataset that has not decided what it is testing, and pinning one keeps
// the Cases identical across runs and sources.
func discoverColumns(rows []datasetserver.Row) columns {
	if len(rows) == 0 {
		return columns{}
	}
	row := mapRowFields(rows[0])
	var c columns
	for _, name := range inputCandidates {
		if _, ok := row[name]; ok {
			c.input = name
			break
		}
	}
	for _, name := range expectedCandidates {
		if _, ok := row[name]; ok {
			c.expected = name
			break
		}
	}
	return c
}

// mapRowFields decodes a row's raw object into a field map. A row whose
// object is malformed is fatal here — the server's contract violation, named
// with the row it happened on.
func mapRowFields(row datasetserver.Row) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row.Row, &fields); err != nil {
		return nil
	}
	return fields
}

// actualColumns lists a row's column names in sorted order, for the
// no-input refusal.
func actualColumns(row datasetserver.Row) []string {
	fields := mapRowFields(row)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mapRow turns one row into a Case.
//
// The id is the row_idx as a string: it is what the server addresses rows
// by, it is stable across re-reads at the same revision, and it is what the
// split assignment and the duplicate check key on.
//
// A null input is fatal, naming the row: a Case without an input cannot be
// scored, and silently dropping it would shrink the denominator. A null
// expected maps to an empty string, exactly as the langfuse adapter maps a
// missing expectation — a Case without an expectation is a rubric-only
// score, not a broken row.
func mapRow(row datasetserver.Row, cols columns, dataset, config, split string) (*core.Case, error) {
	fields := mapRowFields(row)
	if fields == nil {
		return nil, fmt.Errorf("hf: row %d is not a JSON object", row.RowIdx)
	}

	id := strconv.FormatInt(row.RowIdx, 10)

	// A row missing the dataset's chosen input column is the same failure as
	// a null one: the row has no input, and a Case without an input cannot be
	// scored. Absent and null are both named with the row_idx so the fix can
	// point at the row.
	rawInput := fields[cols.input]
	if len(rawInput) == 0 {
		return nil, fmt.Errorf("hf: row %d: the input column %q is absent; every Case "+
			"needs an input", row.RowIdx, cols.input)
	}
	input, isNull, err := datasetserver.ValueString(rawInput)
	if err != nil {
		return nil, fmt.Errorf("hf: row %d: the %s column: %w", row.RowIdx, cols.input, err)
	}
	if isNull {
		return nil, fmt.Errorf("hf: row %d: the input column %q is null; every Case "+
			"needs an input", row.RowIdx, cols.input)
	}

	// An absent or null expected maps to an empty string, exactly as the
	// langfuse adapter maps a missing expectation — a Case without an
	// expectation is a rubric-only score, not a broken row.
	expected := ""
	if cols.expected != "" {
		if raw := fields[cols.expected]; len(raw) > 0 {
			expected, _, err = datasetserver.ValueString(raw)
			if err != nil {
				return nil, fmt.Errorf("hf: row %d: the %s column: %w", row.RowIdx, cols.expected, err)
			}
		}
	}

	return &core.Case{
		Id:       id,
		Input:    input,
		Expected: expected,
		Provenance: &knov1.Provenance{
			Source:    "hf",
			SourceRef: locator(dataset, config, split, row.RowIdx),
		},
	}, nil
}
