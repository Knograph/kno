package braintrust

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// event is one Braintrust dataset event, mapped to what a Case needs.
type event struct {
	ID             string
	Input          string // canonical JSON of input
	Expected       string // canonical JSON of expected; empty when null
	XactID         string // the version counter the merge rule and fingerprint key on
	Derived        bool   // origin present: copied from another object
	DerivationNote string // set when a mapping decision deserves naming
}

// kv is one key/value pair of a JSON object, in document order.
//
// The order is load-bearing for the row walk, and json.Unmarshal into
// map[string]any would lose it.
type kv struct {
	key string
	val json.RawMessage
}

// decodeEvent maps one Braintrust dataset event row to a Case's fields.
//
// Deterministic mapping: the id comes from the "id" key; Input is the
// canonical JSON of the event's input value; Expected is the canonical JSON
// of expected, empty when it is null. Never map[string]any — key order
// would be lost, and canonical JSON must not depend on a map's internal
// ordering.
//
// A row is fatal, never skipped: the same row would be handed back by
// Braintrust on a re-run, so a run that skipped it would measure a
// different population than the resume thinks it measures.
//
// No tags mapping: a Braintrust event carries a tags array, and this
// adapter deliberately does not read it — the decision is recorded in
// docs/plans/2026-08-29-braintrust-evals-adapter.md, and an unused schema
// field is a schema field ignored, not promised.
func decodeEvent(raw json.RawMessage) (*event, error) {
	if len(raw) > maxRowBytes {
		id := rowID(raw)
		if id == "" {
			id = "(id unreadable)"
		}
		return nil, fmt.Errorf("braintrust: event %s exceeds the %d-byte row cap; "+
			"an event is a prompt and an expectation, not a corpus", id, maxRowBytes)
	}

	kvs, err := orderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("braintrust: the event row is not a JSON object")
	}

	ev := &event{}
	var rawInput, rawExpected, rawOrigin json.RawMessage
	for _, kv := range kvs {
		switch kv.key {
		case "id":
			if err := json.Unmarshal(kv.val, &ev.ID); err != nil {
				return nil, fmt.Errorf("braintrust: the event id is not a string")
			}
		case "_xact_id":
			if err := json.Unmarshal(kv.val, &ev.XactID); err != nil {
				return nil, fmt.Errorf("braintrust: event %s: the _xact_id is not a string",
					displayID(ev.ID))
			}
		case "input":
			rawInput = kv.val
		case "expected":
			rawExpected = kv.val
		case "origin":
			rawOrigin = kv.val
		}
	}

	// A null input is fatal, langsmith-parity: a Case is a prompt and an
	// expectation, not a corpus, and canonical JSON of null would silently
	// hand "null" to the agent as a prompt.
	if isNull(rawInput) {
		return nil, fmt.Errorf("braintrust: event %s has a null input; a Case is a "+
			"prompt and an expectation, and null is neither", displayID(ev.ID))
	}
	in, err := canonicalJSON(rawInput)
	if err != nil {
		return nil, fmt.Errorf("braintrust: event %s: input is not valid JSON: %v",
			displayID(ev.ID), err)
	}
	ev.Input = in

	var notes []string

	// Derived exactly when the event carries an origin object: Braintrust
	// records "copied from another object" as origin {object_type,
	// object_id, _xact_id}, and a copied expectation is a weak label — it
	// was recorded, not judged. Hand-authored events stay underived, the
	// semantic difference documented in docs/what-the-numbers-mean.md. The
	// object_type values captured in the fixtures (experiment, span,
	// eval_result — see testdata/note.txt) are not enumerated here: the
	// PRESENCE of origin is the signal, whatever the object type.
	if !isNull(rawOrigin) {
		ev.Derived = true
		var objectType, objectID string
		if kvs2, err := orderedObject(rawOrigin); err == nil {
			for _, kv := range kvs2 {
				switch kv.key {
				case "object_type":
					_ = json.Unmarshal(kv.val, &objectType)
				case "object_id":
					_ = json.Unmarshal(kv.val, &objectID)
				}
			}
		}
		switch {
		case objectType != "" && objectID != "":
			notes = append(notes, "copied from a "+objectType+" (object_id "+objectID+")")
		case objectType != "":
			notes = append(notes, "copied from a "+objectType)
		case objectID != "":
			notes = append(notes, "copied from another object (object_id "+objectID+")")
		default:
			notes = append(notes, "copied from another object")
		}
	}

	if isNull(rawExpected) {
		// A null expected is legal in Braintrust; an empty expectation is the
		// honest reading of it, named rather than silent. A judge Goal scores
		// without it, and absence must not become a silent skip.
		ev.Expected = ""
		notes = append(notes, "expected is null; Expected left empty")
	} else {
		exp, err := canonicalJSON(rawExpected)
		if err != nil {
			return nil, fmt.Errorf("braintrust: event %s: expected is not valid JSON: %v",
				displayID(ev.ID), err)
		}
		ev.Expected = exp
	}

	ev.DerivationNote = strings.Join(notes, "; ")
	return ev, nil
}

// extractXactID reads the version counter out of an event row, for the
// resume fingerprint's freshness half. Best-effort by design: the row was
// already scanned by the page reader, and the missing and non-string cases
// are refused because the fingerprint is keyed on it.
func extractXactID(raw json.RawMessage) (string, error) {
	kvs, err := orderedObject(raw)
	if err != nil {
		return "", fmt.Errorf("the first event is not a JSON object")
	}
	for _, kv := range kvs {
		if kv.key != "_xact_id" {
			continue
		}
		var xid string
		if err := json.Unmarshal(kv.val, &xid); err != nil {
			return "", fmt.Errorf("the first event's _xact_id is not a string")
		}
		if xid == "" {
			return "", fmt.Errorf("the first event has an empty _xact_id; the resume " +
				"fingerprint is keyed on it")
		}
		return xid, nil
	}
	return "", fmt.Errorf("the first event has no _xact_id; the resume fingerprint " +
		"is keyed on it")
}

// canonicalJSON re-encodes a JSON value with object keys sorted and no
// insignificant whitespace — the deterministic encoding the goldens pin.
//
// The input is decoded with json.Number so numbers round-trip as their
// literal form (a float64 would silently rewrite 12345678901234567890), and
// re-marshaled with encoding/json, whose map ordering is key-sorted. The
// result is Kno's rendering of a Braintrust value; if Braintrust's own
// rendering differs, the golden records which one this is, and the
// provenance carries the event id so the source row stays findable.
func canonicalJSON(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isNull reports whether raw is a JSON null — or empty, which is how an
// absent field is represented before it is known to be absent.
func isNull(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == 'n'
		}
	}
	return true
}

// orderedObject decodes a JSON object into its key/value pairs in document
// order.
func orderedObject(raw json.RawMessage) ([]kv, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var kvs []kv
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("the object has a non-string key")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		kvs = append(kvs, kv{key: key, val: val})
	}
	return kvs, nil
}

// rowID best-effort reads the id field of a row, so the row-cap error can
// name the event.
//
// The row may be truncated at the cap — the partial bytes are all the error
// has — so a strict decode is tried first and a tolerant scan second: the id
// sits near the start of the row, and "id" quoted exactly cannot be confused
// with "input". A row big enough to trip the cap is usually malformed, so
// failure here is fine — the error falls back to "(id unreadable)".
func rowID(raw json.RawMessage) string {
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &s); err == nil {
		return s.ID
	}
	rest := raw
	for {
		idx := bytes.Index(rest, []byte(`"id"`))
		if idx < 0 {
			return ""
		}
		after := rest[idx+len(`"id"`):]
		i := 0
		for i < len(after) && (after[i] == ' ' || after[i] == '\t') {
			i++
		}
		if i >= len(after) || after[i] != ':' {
			rest = after
			continue
		}
		i++
		for i < len(after) && (after[i] == ' ' || after[i] == '\t') {
			i++
		}
		if i >= len(after) || after[i] != '"' {
			rest = after
			continue
		}
		i++
		var id strings.Builder
		for i < len(after) {
			switch after[i] {
			case '\\':
				i += 2
				continue
			case '"':
				return id.String()
			}
			id.WriteByte(after[i])
			i++
		}
		return ""
	}
}

// displayID quotes an event id for an error, so a row missing its id still
// reads coherently.
func displayID(id string) string {
	if id == "" {
		return "(no id)"
	}
	return fmt.Sprintf("%q", id)
}

// pageReader streams the rows of one {events, cursor} page envelope.
//
// The envelope is walked by a byte scanner rather than unmarshaled
// wholesale, so a page of any size is consumed a row at a time and the
// streaming memory profile of the whole pipeline is preserved. Rows are
// read with a hard cap: the scanner stops at maxRowBytes and fails, so a
// hostile row cannot force a whole page into memory before the cap check
// runs.
type pageReader struct {
	buf *bufio.Reader

	// row is the scan buffer, reused across calls. The slice nextRow returns
	// is borrowed from it; it is valid only until the next scan.
	row []byte

	cursor  string
	hasNext bool
	// itemsDone is set once the events array and whatever envelope keys
	// follow it have been consumed; it guards the one-time finish.
	itemsDone bool
}

// errRowTooLarge marks a scanValue that hit the row cap. The partial value
// is still in the reader's row buffer, so the caller can name the event.
var errRowTooLarge = errors.New("row exceeds the cap")

// newPageReader parses the envelope's head and positions the reader at the
// first row.
//
// Keys are matched by name and anything unknown is skipped raw, so a server
// that adds envelope fields keeps working. The events array and the cursor
// may appear in either order.
func newPageReader(body io.Reader) (*pageReader, error) {
	pr := &pageReader{buf: bufio.NewReaderSize(body, 64<<10)}

	if err := pr.skipSpace(); err != nil {
		return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
	}
	if b != '{' {
		return nil, errors.New("braintrust: the page envelope is not a JSON object")
	}

	foundEvents := false
	for {
		if err := pr.skipSpace(); err != nil {
			return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			break
		}
		if b != '"' {
			return nil, errors.New("braintrust: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return nil, err
		}
		switch key {
		case "events":
			if err := pr.skipSpace(); err != nil {
				return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
			}
			if b != '[' {
				return nil, errors.New("braintrust: events is not an array")
			}
			// The events array is now the active context: the head loop stops
			// here. Keys that follow events are read by finishEnvelope once
			// the array is exhausted.
			foundEvents = true
			goto positioned
		default:
			raw, err := pr.scanValue()
			if err != nil {
				return nil, fmt.Errorf("braintrust: decoding the page envelope: %w", err)
			}
			if key == "cursor" {
				var c string
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, fmt.Errorf("braintrust: decoding cursor: %w", err)
				}
				pr.cursor = c
				pr.hasNext = true
			}
		}
	}
positioned:
	if !foundEvents {
		return nil, errors.New("braintrust: the page envelope has no events array")
	}
	return pr, nil
}

// skipSpace consumes JSON inter-token whitespace, leaving the reader at the
// next significant byte.
func (pr *pageReader) skipSpace() error {
	for {
		b, err := pr.buf.ReadByte()
		if err != nil {
			return err
		}
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return pr.buf.UnreadByte()
	}
}

// consumeColon skips whitespace and the ':' that separates an object key
// from its value.
func (pr *pageReader) consumeColon() error {
	if err := pr.skipSpace(); err != nil {
		return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
	}
	if b != ':' {
		return fmt.Errorf("braintrust: decoding the page envelope: expected ':' after a key")
	}
	return nil
}

// readKeyString reads a JSON key's bytes (without the quotes) at the current
// position, where the opening quote was already consumed. Escapes are kept
// verbatim, which is fine for the envelope keys this reader compares.
func (pr *pageReader) readKeyString() (string, error) {
	var sb strings.Builder
	escaped := false
	for {
		b, err := pr.buf.ReadByte()
		if err != nil {
			return "", err
		}
		if escaped {
			sb.WriteByte(b)
			escaped = false
			continue
		}
		if b == '\\' {
			sb.WriteByte(b)
			escaped = true
			continue
		}
		if b == '"' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
}

// scanValue reads one JSON value at the current stream position into pr.row,
// bounded by maxRowBytes, and returns it.
//
// Scanning rather than decoding with a fresh json.Decoder per value: the
// decoder buffers ahead of the value's end, so the row cap could only be
// enforced AFTER the whole row had been buffered — a hostile server could
// push the full page into memory before the check ran. This scanner stops at
// the value's closing bracket, so the memory held at any moment is at most
// maxRowBytes, and a row over the cap fails with the partial value in hand,
// which still names the event.
func (pr *pageReader) scanValue() ([]byte, error) {
	pr.row = pr.row[:0]

	if err := pr.skipSpace(); err != nil {
		return nil, err
	}
	first, err := pr.buf.ReadByte()
	if err != nil {
		return nil, err
	}
	pr.row = append(pr.row, first)

	switch first {
	case '{', '[':
		depth := 1
		inString := false
		escaped := false
		for {
			if len(pr.row) == maxRowBytes {
				return pr.row, errRowTooLarge
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return pr.row, err
			}
			pr.row = append(pr.row, b)
			if inString {
				if escaped {
					escaped = false
					continue
				}
				if b == '\\' {
					escaped = true
					continue
				}
				if b == '"' {
					inString = false
				}
				continue
			}
			switch b {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return pr.row, nil
				}
			}
		}
	case '"':
		escaped := false
		for {
			if len(pr.row) == maxRowBytes {
				return pr.row, errRowTooLarge
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return pr.row, err
			}
			pr.row = append(pr.row, b)
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				return pr.row, nil
			}
		}
	default:
		// A scalar: number, true, false, or null. It ends at a delimiter.
		for {
			if len(pr.row) == maxRowBytes {
				return pr.row, errRowTooLarge
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return pr.row, err
			}
			switch b {
			case ',', '}', ']':
				if err := pr.buf.UnreadByte(); err != nil {
					return pr.row, err
				}
				return pr.row, nil
			}
			pr.row = append(pr.row, b)
		}
	}
}

// finishEnvelope consumes the events array's closing bracket and whatever
// envelope keys follow it. It runs exactly once, the first time the array is
// exhausted, so a cursor that appears after events is still read.
func (pr *pageReader) finishEnvelope() error {
	if pr.itemsDone {
		return nil
	}
	pr.itemsDone = true

	for {
		if err := pr.skipSpace(); err != nil {
			return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			return nil
		}
		if b != '"' {
			return errors.New("braintrust: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return err
		}
		if key == "events" {
			// A second events array after the first is a shape the stream was
			// never designed to carry; consume its value so the failure is
			// bounded, then refuse it.
			if _, err := pr.scanValue(); err != nil {
				return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
			}
			return errors.New("braintrust: the page envelope has two events arrays")
		}
		raw, err := pr.scanValue()
		if err != nil {
			return fmt.Errorf("braintrust: decoding the page envelope: %w", err)
		}
		if key == "cursor" {
			var c string
			if err := json.Unmarshal(raw, &c); err != nil {
				return fmt.Errorf("braintrust: decoding cursor: %w", err)
			}
			pr.cursor = c
			pr.hasNext = true
		}
	}
}

// nextRow returns the next row's raw JSON, or ok=false when the array is
// exhausted. The returned slice is borrowed from the reader's row buffer;
// the caller must copy before retaining.
func (pr *pageReader) nextRow() (json.RawMessage, bool, error) {
	if err := pr.skipSpace(); err != nil {
		return nil, false, fmt.Errorf("braintrust: reading the page: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("braintrust: the page envelope is truncated after events")
	}
	if b == ']' {
		if err := pr.finishEnvelope(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := pr.buf.UnreadByte(); err != nil {
		return nil, false, fmt.Errorf("braintrust: reading the page: %w", err)
	}

	raw, err := pr.scanValue()
	if err != nil {
		switch {
		case errors.Is(err, errRowTooLarge):
			id := rowID(raw)
			if id == "" {
				id = "(id unreadable)"
			}
			return nil, false, fmt.Errorf("braintrust: event %s exceeds the %d-byte row cap; "+
				"an event is a prompt and an expectation, not a corpus", id, maxRowBytes)
		case errors.Is(err, io.EOF):
			return nil, false, errors.New("braintrust: the page envelope is truncated after events")
		default:
			return nil, false, fmt.Errorf("braintrust: reading the page: %w", err)
		}
	}

	// The array continues if the next significant byte is a ','. A ']' ends
	// it; the byte is restored so the next call's end-of-array path sees it.
	if err := pr.skipSpace(); err != nil {
		return nil, false, fmt.Errorf("braintrust: reading the page: %w", err)
	}
	b, err = pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("braintrust: the page envelope is truncated after events")
	}
	if b == ']' {
		if err := pr.buf.UnreadByte(); err != nil {
			return nil, false, fmt.Errorf("braintrust: reading the page: %w", err)
		}
	}
	return raw, true, nil
}

// nextCursor reports the page's continuation cursor, and whether the stream
// should continue at all. A page with no cursor field is the last page; an
// empty cursor string is the same.
func (pr *pageReader) nextCursor() (string, bool) {
	if !pr.hasNext {
		return "", false
	}
	return pr.cursor, pr.cursor != ""
}
