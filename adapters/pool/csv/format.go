// This file holds the on-disk format: the column contract, the tag
// delimiter, and the kind spelling, all pinned here so the iteration code in
// csv.go can read them without knowing the format's details.

package csv

import (
	"fmt"
	"math"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// The column contract. id and content are required; kind and tags are
// optional. The header is validated loudly — an unknown column is a load
// error, not a guess — for the same reason the jsonl adapter refuses unknown
// fields: a column that decodes into nothing gives its author no error, no
// warning, and no data, and a misspelled "contnet" that fell through would
// read as "no content" in a later run with nothing showing it.
const (
	colID      = "id"
	colContent = "content"
	colKind    = "kind"
	colTags    = "tags"
)

// TagsSeparator separates the entries of a tags cell.
//
// Pinned rather than configurable: comma is the field delimiter, so a comma
// list would live inside quoted fields and invite the ambiguity this format
// refuses everywhere else, and any other choice is a decision the file's
// readers must agree on — a constant is a decision everyone agrees on.
const TagsSeparator = ";"

// columnsOf validates the header and returns each recognized column's index.
//
// The return is keyed by column name so the row reader cannot mis-index: a
// header reordered by an editor stays correct, and a header whose columns
// changed shape is refused here, before any row is read.
func columnsOf(path string, header []string) (map[string]int, error) {
	cols := make(map[string]int, len(header))
	for i, name := range header {
		if _, dup := cols[name]; dup {
			return nil, fmt.Errorf("%s: duplicate column %q in the header", path, name)
		}
		cols[name] = i
	}
	if _, ok := cols[colID]; !ok {
		return nil, fmt.Errorf(
			"%s: no id column (found: %s); every measurement is keyed on the id, "+
				"and a row-number fallback would re-number every asset whenever the "+
				"file is edited — write an id column with a stable id per row",
			path, strings.Join(header, ", "),
		)
	}
	if _, ok := cols[colContent]; !ok {
		return nil, fmt.Errorf(
			"%s: no content column (found: %s); an asset with no content cannot be "+
				"injected and would rank at an undefined delta_per_cost — write a content column",
			path, strings.Join(header, ", "),
		)
	}
	for _, name := range header {
		switch name {
		case colID, colContent, colKind, colTags:
		default:
			return nil, fmt.Errorf(
				"%s: unknown column %q (found: %s); the contract is id, content, kind, tags",
				path, name, strings.Join(header, ", "),
			)
		}
	}
	return cols, nil
}

// parseTags splits a tags cell on TagsSeparator.
//
// Entries are trimmed of surrounding whitespace, and empty entries (a
// trailing separator, or two separators in a row) are dropped rather than
// tagging an Asset with the empty string.
func parseTags(cell string) []string {
	var tags []string
	for _, t := range strings.Split(cell, TagsSeparator) {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// kindOf maps the file's spelling of a Kind onto the enum.
//
// Exact, lower-case, and closed, with the same contract as the jsonl
// adapter's: an unrecognized spelling is refused rather than defaulted,
// because the default is KIND_UNSPECIFIED and that enum's own proto comment
// warns a silent zero "would read as knowledge and route the Asset to the
// wrong destination".
func kindOf(s string) (knov1.Kind, error) {
	switch s {
	case "":
		return knov1.Kind_KIND_UNSPECIFIED, nil
	case "knowledge":
		return knov1.Kind_KIND_KNOWLEDGE, nil
	case "behavior":
		return knov1.Kind_KIND_BEHAVIOR, nil
	default:
		return knov1.Kind_KIND_UNSPECIFIED, fmt.Errorf(
			`unknown kind %q; write "knowledge" or "behavior", or omit the field `+
				`and let routing judge it`, s,
		)
	}
}

// bytesPerToken is the divisor behind contextTokens. See its godoc for why it
// is this number and not the one the reservation path uses.
const bytesPerToken = 3.6

// contextTokens estimates what carrying this Asset adds to every request.
//
// This is the RANKING denominator of delta_per_cost, and it must not be the
// reservation path's countTokens (adapters/agent/pricing): that deliberately
// over-counts by about 3x on prose and takes a model argument, so feeding it
// in here would rank the portfolio by content type instead of by value
// (docs/debt.md#68). Bytes over a fixed divisor, centered on the one
// measurement in this tree — English prose at 3.6 bytes/token. Rounded up, so
// a non-empty Asset never costs zero tokens: delta_per_cost over a zero
// denominator is an infinity, and an infinity sorts to the top of a greedy
// ranking.
func contextTokens(sizeBytes int) int64 {
	return int64(math.Ceil(float64(sizeBytes) / bytesPerToken))
}
