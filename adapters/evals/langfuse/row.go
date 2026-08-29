package langfuse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// item is one Langfuse dataset item, mapped to what a Case needs.
type item struct {
	ID             string
	Input          string // canonical JSON of input
	Expected       string // canonical JSON of expectedOutput; empty when null
	Archived       bool   // status == ARCHIVED; filtered client-side
	Derived        bool   // sourceObservationId or sourceTraceId set
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

// decodeItem maps one Langfuse dataset item row to a Case's fields.
//
// Deterministic mapping: the id comes from the "id" key; Input is the
// canonical JSON of the item's input value; Expected is the canonical JSON
// of expectedOutput, empty when it is null. Never map[string]any — key order
// would be lost, and canonical JSON must not depend on a map's internal
// ordering.
//
// A row is fatal, never skipped: the same row would be handed back by
// Langfuse on a re-run, so a run that skipped it would measure a different
// population than the resume thinks it measures. The one exception is an
// ARCHIVED item, which is a deliberate client-side filter (the API has no
// status query parameter), reported via the Archived flag.
func decodeItem(raw json.RawMessage) (*item, error) {
	if len(raw) > maxRowBytes {
		id := rowID(raw)
		if id == "" {
			id = "(id unreadable)"
		}
		return nil, fmt.Errorf("langfuse: item %s exceeds the %d-byte row cap; "+
			"an item is a prompt and an expectation, not a corpus", id, maxRowBytes)
	}

	kvs, err := orderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("langfuse: the item row is not a JSON object")
	}

	it := &item{}
	var rawInput, rawExpected, rawStatus, rawSourceObs, rawSourceTrace json.RawMessage
	for _, kv := range kvs {
		switch kv.key {
		case "id":
			if err := json.Unmarshal(kv.val, &it.ID); err != nil {
				return nil, fmt.Errorf("langfuse: the item id is not a string")
			}
		case "input":
			rawInput = kv.val
		case "expectedOutput":
			rawExpected = kv.val
		case "status":
			rawStatus = kv.val
		case "sourceObservationId":
			rawSourceObs = kv.val
		case "sourceTraceId":
			rawSourceTrace = kv.val
		}
	}

	// A missing status is kept: the filter excludes exactly the documented
	// "ARCHIVED" value, and an absent status is not a claim one way or the
	// other.
	if !isNull(rawStatus) {
		var status string
		if err := json.Unmarshal(rawStatus, &status); err != nil {
			return nil, fmt.Errorf("langfuse: item %s: status is not a string", displayID(it.ID))
		}
		it.Archived = status == "ARCHIVED"
	}

	if it.Archived {
		return it, nil
	}

	// A null input is fatal, langsmith-parity: a Case is a prompt and an
	// expectation, not a corpus, and canonical JSON of null would silently
	// hand "null" to the agent as a prompt.
	if isNull(rawInput) {
		return nil, fmt.Errorf("langfuse: item %s has a null input; a Case is a "+
			"prompt and an expectation, and null is neither", displayID(it.ID))
	}
	in, err := canonicalJSON(rawInput)
	if err != nil {
		return nil, fmt.Errorf("langfuse: item %s: input is not valid JSON: %v", displayID(it.ID), err)
	}
	it.Input = in
	if isArray(rawInput) {
		// Message-array inputs map to raw canonical JSON as the prompt — the
		// divergence from langsmith's turn-mapping is accepted in writing in
		// the langfuse plan, not absorbed silently.
		it.DerivationNote = "input is a message array; kept as canonical JSON rather " +
			"than turn-mapped (accepted in docs/plans/2026-08-29-langfuse-evals-adapter.md)"
	}

	if isNull(rawExpected) {
		// A null expectedOutput is legal in Langfuse; an empty expectation
		// is the honest reading of it, named rather than silent. A judge
		// Goal scores without it, and absence must not become a silent skip.
		it.Expected = ""
		if it.DerivationNote == "" {
			it.DerivationNote = "expectedOutput is null; Expected left empty"
		} else {
			it.DerivationNote += "; expectedOutput is null; Expected left empty"
		}
	} else {
		exp, err := canonicalJSON(rawExpected)
		if err != nil {
			return nil, fmt.Errorf("langfuse: item %s: expectedOutput is not valid JSON: %v",
				displayID(it.ID), err)
		}
		it.Expected = exp
	}

	// Derived exactly when the expectation was harvested from a trace: the
	// per-item signal langsmith lacks. Hand-authored items stay undderived —
	// the semantic difference is documented in docs/what-the-numbers-mean.md.
	var obs, trc string
	_ = json.Unmarshal(rawSourceObs, &obs)
	_ = json.Unmarshal(rawSourceTrace, &trc)
	switch {
	case obs != "" && trc != "":
		it.Derived = true
		it.DerivationNote = "harvested from a trace (sourceObservationId " + obs +
			", sourceTraceId " + trc + ")"
	case obs != "":
		it.Derived = true
		it.DerivationNote = "harvested from a trace (sourceObservationId " + obs + ")"
	case trc != "":
		it.Derived = true
		it.DerivationNote = "harvested from a trace (sourceTraceId " + trc + ")"
	}
	return it, nil
}

// canonicalJSON re-encodes a JSON value with object keys sorted and no
// insignificant whitespace — the deterministic encoding the goldens pin.
//
// The input is decoded with json.Number so numbers round-trip as their
// literal form (a float64 would silently rewrite 12345678901234567890), and
// re-marshaled with encoding/json, whose map ordering is key-sorted. The
// result is Kno's rendering of a Langfuse value; if Langfuse's own rendering
// differs, the golden records which one this is, and the provenance carries
// the item id so the source row stays findable.
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

// isArray reports whether raw is a JSON array.
func isArray(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == '['
		}
	}
	return false
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
// name the item.
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

// displayID quotes an item id for an error, so a row missing its id still
// reads coherently.
func displayID(id string) string {
	if id == "" {
		return "(no id)"
	}
	return fmt.Sprintf("%q", id)
}

// envelopeMeta is the pagination half of the {data, meta} envelope.
type envelopeMeta struct {
	Page       int
	Limit      int
	TotalItems int
	TotalPages int
}

// pageReader streams the rows of one {data, meta} page envelope.
//
// The envelope is walked by a byte scanner rather than unmarshaled
// wholesale, so a page of any size is consumed a row at a time and the
// streaming memory profile of the whole pipeline is preserved. Rows are
// read with a hard cap: the scanner stops at maxRowBytes and fails, so a
// hostile row cannot force a whole page into memory before the cap check
// runs. The meta object is small by contract, so it is scanned raw and
// unmarshaled.
type pageReader struct {
	buf *bufio.Reader

	// row is the scan buffer, reused across calls. The slice nextRow returns
	// is borrowed from it; it is valid only until the next scan.
	row []byte

	meta      *envelopeMeta
	itemsDone bool
}

// errRowTooLarge marks a scanValue that hit the row cap. The partial value
// is still in the reader's row buffer, so the caller can name the item.
var errRowTooLarge = errors.New("row exceeds the cap")

// newPageReader parses the envelope's head and positions the reader at the
// first row.
//
// Keys are matched by name and anything unknown is skipped raw, so a server
// that adds envelope fields keeps working. The data array and the meta
// object may appear in either order.
func newPageReader(body io.Reader) (*pageReader, error) {
	pr := &pageReader{buf: bufio.NewReaderSize(body, 64<<10)}

	if err := pr.skipSpace(); err != nil {
		return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
	}
	if b != '{' {
		return nil, errors.New("langfuse: the page envelope is not a JSON object")
	}

	foundData := false
	for {
		if err := pr.skipSpace(); err != nil {
			return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			break
		}
		if b != '"' {
			return nil, errors.New("langfuse: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return nil, err
		}
		switch key {
		case "data":
			if err := pr.skipSpace(); err != nil {
				return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
			}
			if b != '[' {
				return nil, errors.New("langfuse: data is not an array")
			}
			// The data array is now the active context: the head loop stops
			// here. Keys that follow data are read by finishEnvelope once the
			// array is exhausted.
			foundData = true
			goto positioned
		case "meta":
			raw, err := pr.scanValue()
			if err != nil {
				return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
			}
			var m envelopeMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("langfuse: decoding the page meta: %w", err)
			}
			pr.meta = &m
		default:
			if _, err := pr.scanValue(); err != nil {
				return nil, fmt.Errorf("langfuse: decoding the page envelope: %w", err)
			}
		}
	}
positioned:
	if !foundData {
		return nil, errors.New("langfuse: the page envelope has no data array")
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
		return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
	}
	if b != ':' {
		return fmt.Errorf("langfuse: decoding the page envelope: expected ':' after a key")
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
// which still names the item.
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

// finishEnvelope consumes the data array's closing bracket and whatever
// envelope keys follow it. It runs exactly once, the first time the array is
// exhausted, so a meta that appears after data is still read.
func (pr *pageReader) finishEnvelope() error {
	if pr.itemsDone {
		return nil
	}
	pr.itemsDone = true

	for {
		if err := pr.skipSpace(); err != nil {
			return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			return nil
		}
		if b != '"' {
			return errors.New("langfuse: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return err
		}
		if key == "data" {
			// A second data array after the first is a shape the stream was
			// never designed to carry; consume its value so the failure is
			// bounded, then refuse it.
			if _, err := pr.scanValue(); err != nil {
				return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
			}
			return errors.New("langfuse: the page envelope has two data arrays")
		}
		raw, err := pr.scanValue()
		if err != nil {
			return fmt.Errorf("langfuse: decoding the page envelope: %w", err)
		}
		if key == "meta" {
			var m envelopeMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("langfuse: decoding the page meta: %w", err)
			}
			pr.meta = &m
		}
	}
}

// nextRow returns the next row's raw JSON, or ok=false when the array is
// exhausted. The returned slice is borrowed from the reader's row buffer;
// the caller must copy before retaining.
func (pr *pageReader) nextRow() (json.RawMessage, bool, error) {
	if err := pr.skipSpace(); err != nil {
		return nil, false, fmt.Errorf("langfuse: reading the page: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("langfuse: the page envelope is truncated after data")
	}
	if b == ']' {
		if err := pr.finishEnvelope(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := pr.buf.UnreadByte(); err != nil {
		return nil, false, fmt.Errorf("langfuse: reading the page: %w", err)
	}

	raw, err := pr.scanValue()
	if err != nil {
		switch {
		case errors.Is(err, errRowTooLarge):
			id := rowID(raw)
			if id == "" {
				id = "(id unreadable)"
			}
			return nil, false, fmt.Errorf("langfuse: item %s exceeds the %d-byte row cap; "+
				"an item is a prompt and an expectation, not a corpus", id, maxRowBytes)
		case errors.Is(err, io.EOF):
			return nil, false, errors.New("langfuse: the page envelope is truncated after data")
		default:
			return nil, false, fmt.Errorf("langfuse: reading the page: %w", err)
		}
	}

	// The array continues if the next significant byte is a ','. A ']' ends
	// it; the byte is restored so the next call's end-of-array path sees it.
	if err := pr.skipSpace(); err != nil {
		return nil, false, fmt.Errorf("langfuse: reading the page: %w", err)
	}
	b, err = pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("langfuse: the page envelope is truncated after data")
	}
	if b == ']' {
		if err := pr.buf.UnreadByte(); err != nil {
			return nil, false, fmt.Errorf("langfuse: reading the page: %w", err)
		}
	}
	return raw, true, nil
}

// morePages reports whether the stream should continue at a higher page
// number, after the current page has been fully consumed.
//
// Pagination is page-numbered: continue while the meta's totalPages exceeds
// the page just read. A page with no meta object is a malformed envelope —
// without it the adapter cannot know when to stop, so it refuses rather
// than guess.
func (pr *pageReader) morePages() (bool, error) {
	if pr.meta == nil {
		return false, errors.New("langfuse: the page envelope has no meta object; " +
			"the adapter cannot know when the pagination ends")
	}
	return pr.meta.TotalPages > pr.meta.Page, nil
}
