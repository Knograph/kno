// This file holds the on-disk format, and it is the only place in this package
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

import (
	"bytes"
	"encoding/json"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// record is the on-disk shape of one Asset.
//
// Hand-written rather than protojson over knov1.Asset, for the reason the
// Evals adapter hand-writes its own: the file is a user-facing contract that
// should read naturally to someone writing it by hand, and it should not shift
// underneath them when the proto gains a field.
//
// The dividing line is that the file carries what only its author knows, and
// the adapter computes what it can measure. So `destination` is absent — it is
// assigned by Select after measurement, and a file declaring one would assert
// the answer the pipeline exists to compute. `provenance` is absent — it is
// written from where the record was actually read, not from what it claims
// about itself. And `cost.context_tokens` is absent — it is the denominator of
// the Asset's own ranking, and a pool author who could set it could rank their
// own pool.
type record struct {
	ID      string   `json:"id"`
	Content string   `json:"content"`
	Title   string   `json:"title,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Cost    *cost    `json:"cost,omitempty"`
}

// cost is the part of a CostVector only the pool's author can know.
//
// What an Asset cost to acquire or label is a fact about a purchase, and
// whether it goes stale is a fact about the world; neither is derivable from
// the bytes. The other two CostVector fields are not here: context_tokens is
// computed (see contextTokens), and ft_tokens depends on a Destination that is
// not assigned until Select has run.
type cost struct {
	AcquisitionUSDMicros int64 `json:"acquisition_usd_micros,omitempty"`
	Stale                bool  `json:"stale,omitempty"`
}

// decode parses one line into a record.
//
// Unknown fields are rejected rather than ignored, matching the Evals adapter:
// a field that is decoded and then silently discarded gives the user writing it
// no error, no warning, and no data. Failing closed is also what keeps the
// omissions above honest — someone who writes `"destination": "context"`
// learns that it does nothing, instead of believing it did.
func decode(line []byte) (record, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()

	var rec record
	if err := dec.Decode(&rec); err != nil {
		return record{}, err //nolint:wrapcheck // the caller adds file and line context
	}
	return rec, nil
}

// kindOf maps the file's spelling of a Kind onto the enum.
//
// Exact, lower-case, and closed. An unrecognized spelling is refused rather
// than defaulted, because the default is KIND_UNSPECIFIED and that enum's own
// proto comment warns a silent zero "would read as knowledge and route the
// Asset to the wrong destination". A misspelled "behaviour" is precisely how
// that zero arrives, and it would arrive on an Asset whose author was trying to
// be explicit.
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

// costVector builds the Asset's CostVector from the record's declared cost
// plus the computed ranking denominator.
//
// A record with no `cost` block is the common case, and it means "not known":
// zero acquisition cost, and `stale` false in the sense the proto field
// documents — "not known to be stale", never "verified durable". This adapter
// cannot detect staleness at all.
func (r record) costVector(contextTokens int64) *knov1.CostVector {
	v := &knov1.CostVector{ContextTokens: contextTokens}
	if r.Cost != nil {
		v.AcquisitionUsdMicros = r.Cost.AcquisitionUSDMicros
		v.Stale = r.Cost.Stale
	}
	return v
}
