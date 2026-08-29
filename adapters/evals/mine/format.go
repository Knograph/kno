// This file holds the on-disk formats mine reads and writes, and it is the
// only place in this package that decodes or encodes with encoding/json.
//
// The exemption is the same one adapters/evals/jsonl/format.go documents:
// these are FILE formats — the user-authored jsonl-chat transcript, the
// mined cases.jsonl output, and the review manifest — not kno.v1 messages,
// and protojson would force them to mirror the proto's field names and
// presence rules, which is exactly what they deliberately do not do. No
// kno.v1 type is touched in this file.

package mine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// chatRecord is one line of a jsonl-chat transcript.
//
// The pinned schema: one message per line, with an id, a role from the closed
// vocabulary, and content. timestamp is an optional RFC 3339 instant. Message
// ids are REQUIRED: they anchor thread identity, and a transcript without
// them cannot be paired or deduplicated — the same reasoning that makes the
// jsonl adapter treat a missing id as fatal.
//
// role is a string rather than an enum so the file stays hand-writable, but
// the vocabulary is closed: "assistant" and "agent" are the agent side,
// "user" and "human" are the human side. A role outside it is fatal, because
// a transcript role nobody can interpret is a transcript that cannot be
// paired without a guess.
type chatRecord struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// decodeChat parses one jsonl-chat line.
//
// Unknown fields are rejected rather than ignored, matching the jsonl
// adapter: a field a user wrote that we do not read is a field that would
// silently not mean what they think it means.
func decodeChat(line []byte) (chatRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()

	var rec chatRecord
	if err := dec.Decode(&rec); err != nil {
		return chatRecord{}, err //nolint:wrapcheck // the caller adds file and line context
	}
	return rec, nil
}

// chatRoles is the closed role vocabulary of the jsonl-chat schema.
var chatRoles = map[string]bool{
	"assistant": true,
	"agent":     true,
	"user":      true,
	"human":     true,
}

// outputRecord is one mined Case on disk, in the jsonl adapter's record
// format plus the additive provenance fields.
//
// The field spellings are load-bearing: the jsonl adapter decodes this exact
// shape into Case.Provenance, and the round-trip is what makes the weak-label
// marker survive ingestion. A field renamed here and not there is a mined
// Case whose provenance silently reverts to "authored".
type outputRecord struct {
	ID             string `json:"id"`
	Input          string `json:"input"`
	Expected       string `json:"expected"`
	Derived        bool   `json:"derived"`
	DerivationNote string `json:"derivation_note,omitempty"`
	SourceRef      string `json:"source_ref,omitempty"`
}

// EncodeOutput writes one mined Case in the output format.
//
// The output is the jsonl adapter's record format plus the additive
// provenance fields, so the set can be re-ingested by the same adapter and
// the derived marker survives the round-trip.
func EncodeOutput(w io.Writer, c Case) error {
	rec := outputRecord{
		ID:             c.ID,
		Input:          c.Input,
		Expected:       c.Expected,
		Derived:        true,
		DerivationNote: c.Note,
		SourceRef:      c.SourceRef,
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(rec); err != nil {
		return fmt.Errorf("encoding mined case %s: %w", c.ID, err)
	}
	return nil
}

// Manifest is the review record written next to the output.
//
// It is the "this set was reviewed" marker: every decision made during
// --review lands here, and re-mining reads it back so a curated drop can
// never resurrect and an edited expectation is re-applied on the next run.
type Manifest struct {
	// Version is the manifest schema version. Reads refuse anything else.
	Version int `json:"version"`

	// Decisions maps a mined Case's id to what the review decided.
	//
	// Keyed by the id the SOURCE produces — the pre-edit id, since an edit
	// changes the content and therefore the id. A case the source stops
	// producing is simply never looked up again.
	Decisions map[string]Decision `json:"decisions"`
}

// Decision is one reviewed case's disposition.
type Decision struct {
	// Decision is "keep", "drop", or "edit".
	Decision string `json:"decision"`

	// Expected is the corrected expectation, set when Decision is "edit".
	Expected string `json:"expected,omitempty"`
}

// manifestVersion is the current manifest schema.
const manifestVersion = 1

// decodeManifest parses a review manifest.
func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parsing review manifest: %w", err)
	}
	if m.Version != manifestVersion {
		return Manifest{}, fmt.Errorf(
			"review manifest schema version %d, expected %d; re-run the review to rewrite it",
			m.Version, manifestVersion,
		)
	}
	return m, nil
}

// encodeManifest serializes a review manifest.
func encodeManifest(m Manifest) ([]byte, error) {
	m.Version = manifestVersion
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding review manifest: %w", err)
	}
	return append(buf, '\n'), nil
}

// parseChatTime parses the optional jsonl-chat timestamp.
//
// RFC 3339, like every timestamp the schema carries. A present-but-unparseable
// timestamp is fatal rather than ignored: a transcript whose times are wrong
// re-ids differently than one whose times are absent, and silence would make
// that a surprise instead of a line in a file.
func parseChatTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not RFC 3339", s)
	}
	return t, nil
}
