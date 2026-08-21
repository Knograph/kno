// This file holds the on-disk format, and it is the only place in the tree
// that decodes with encoding/json.
//
// ADR-0001 bans encoding/json because proto3 JSON encodes int64 as quoted
// strings and enums as names, so using it on a kno.v1 type silently diverges
// from the generated OpenAPI spec. That reasoning is about kno.v1 types. This
// file decodes a USER-AUTHORED file format into a plain Go struct — no proto
// message is involved, and protojson would be the wrong tool: it would force
// the file format to mirror the proto's field names and presence rules, which
// is exactly what the format deliberately does not do.
//
// The exemption is scoped to files named format.go under adapters/, so it
// cannot quietly spread to code that touches kno.v1 types.

package jsonl

import "encoding/json"

// record is the on-disk shape of one Case.
//
// Deliberately hand-written rather than protojson over knov1.Case. The file
// format is a user-facing contract that should read naturally to someone
// writing it by hand, and it should not shift underneath them when the proto
// gains a field. Split is absent on purpose: it is assigned at ingestion, and
// letting a file declare its own holdout would let a user put a Case wherever
// produced the number they wanted.
type record struct {
	ID       string            `json:"id"`
	Input    string            `json:"input"`
	Expected string            `json:"expected,omitempty"`
	Rubric   string            `json:"rubric,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// decode parses one line into a record.
func decode(line []byte) (record, error) {
	var rec record
	err := json.Unmarshal(line, &rec)
	return rec, err //nolint:wrapcheck // the caller adds file and line context
}
