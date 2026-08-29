package mine

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// csvPair is one row of a question,answer csv: already a pair, so csv is
// independent of --mode.
type csvPair struct {
	question string
	answer   string
}

// parseCSVFile reads a question,answer csv transcript.
//
// The pinned schema: a header row naming the columns, then one
// (question, answer) pair per row. The header is matched
// case-insensitively and by trimmed name; a missing column is fatal,
// because a row whose answer column is silently absent is a row whose
// label is silently lost. A row with an empty question or an empty answer
// is fatal, like a blank id in jsonl-chat: the split is keyed on the id,
// and an auto-generated one would depend on position.
//
// csv is the answer column as the human wrote it — no chit-chat, no
// pairing — so the row IS the pair: input is the question, expected is the
// answer, and --mode does not apply.
func parseCSVFile(ctx context.Context, path string) ([]csvPair, error) {
	f, err := os.Open(path) //nolint:gosec // an explicit user-supplied transcript path: reading it is the command's contract, not attacker-controlled inclusion
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	cr := csv.NewReader(f)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: reading header row: %w", path, err)
	}
	qIdx, aIdx := -1, -1
	for i, h := range header {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "question":
			qIdx = i
		case "answer":
			aIdx = i
		}
	}
	if qIdx < 0 || aIdx < 0 {
		return nil, fmt.Errorf(
			"%s: csv header must name the question and answer columns (found: %s)",
			path, strings.Join(header, ", "),
		)
	}

	var pairs []csvPair
	for row := 2; ; row++ { // the header is line 1
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, row, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := strings.TrimSpace(rec[qIdx])
		if q == "" {
			return nil, fmt.Errorf("%s line %d: empty question; a mined case needs an input", path, row)
		}
		a := strings.TrimSpace(rec[aIdx])
		if a == "" {
			return nil, fmt.Errorf(
				"%s line %d: empty answer; a mined case needs an expected, and an "+
					"auto-generated one would depend on position", path, row,
			)
		}
		pairs = append(pairs, csvPair{question: q, answer: a})
	}
	return pairs, nil
}
