package langsmith

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Named keys, consulted in this order for Input and Expected. Everything else
// in the object is joined in document order only after these are all absent
// or non-string.
const (
	keyQuestion = "question"
	keyInput    = "input"
	keyAnswer   = "answer"
	keyOutput   = "output"
	keyMessages = "messages"
)

// example is one LangSmith dataset row, mapped to what a Case needs.
type example struct {
	ID             string
	Input          string
	Expected       string
	DerivationNote string // set when a mapping decision deserves naming
}

// kv is one key/value pair of a JSON object, in document order.
//
// The order is load-bearing: Input falls back to string values in document
// order, and json.Unmarshal into map[string]any would lose it.
type kv struct {
	key string
	val json.RawMessage
}

// decodeRow maps one LangSmith example row to a Case's fields.
//
// Deterministic mapping: the id comes from the "id" key; Input and Expected
// come from named keys first (question/input, answer/output), then from the
// remaining string values in document order. Never map[string]any — key
// order would be lost, and every fallback Case's Input would depend on the
// map's internal ordering.
//
// A row is fatal, never skipped: the same row would be handed back by
// LangSmith on a re-run, so a run that skipped it would measure a different
// population than the resume thinks it measures.
func decodeRow(raw json.RawMessage) (*example, error) {
	if len(raw) > maxRowBytes {
		id := rowID(raw)
		if id == "" {
			id = "(id unreadable)"
		}
		return nil, fmt.Errorf("langsmith: example %s exceeds the %d-byte row cap; "+
			"an example is a prompt and an expectation, not a corpus", id, maxRowBytes)
	}

	kvs, err := orderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("langsmith: the example row is not a JSON object")
	}

	ex := &example{}
	var rawInput, rawOutput json.RawMessage
	inputsSeen := false
	for _, kv := range kvs {
		switch kv.key {
		case "id":
			if err := json.Unmarshal(kv.val, &ex.ID); err != nil {
				return nil, fmt.Errorf("langsmith: the example id is not a string")
			}
		case "inputs":
			inputsSeen = true
			rawInput = kv.val
		case "outputs":
			rawOutput = kv.val
		}
	}

	// Chat-format datasets carry inputs.messages; the message contents are
	// concatenated into Case.Input, one message per line.
	var chat []chatMessage
	if inputsSeen && isObject(rawInput) {
		inKvs, err := orderedObject(rawInput)
		if err != nil {
			return nil, fmt.Errorf("langsmith: example %s: inputs: %w", displayID(ex.ID), err)
		}
		msgs, isChat, err := decodeChatMessages(inKvs)
		if err != nil {
			return nil, fmt.Errorf("langsmith: example %s: inputs.messages: %w", displayID(ex.ID), err)
		}
		if isChat {
			chat = msgs
			ex.Input, _ = joinContents(msgs)
		}
		if ex.Input == "" {
			// LLM format (or a chat row with no usable messages): named keys
			// first, then document order.
			if s, ok := kvString(inKvs, keyQuestion); ok {
				ex.Input = s
			} else if s, ok := kvString(inKvs, keyInput); ok {
				ex.Input = s
			} else if s := docOrderStrings(inKvs, keyQuestion, keyInput, keyMessages); s != "" {
				ex.Input = s
			}
		}
	}
	if ex.Input == "" {
		return nil, fmt.Errorf("langsmith: example %s has no input; give its inputs a "+
			"question or input field", displayID(ex.ID))
	}

	expected, note, err := mapExpected(rawOutput, chat)
	if err != nil {
		return nil, fmt.Errorf("langsmith: example %s: %w", displayID(ex.ID), err)
	}
	ex.Expected = expected
	ex.DerivationNote = note
	return ex, nil
}

// mapExpected derives Expected from the row's outputs object, or — for chat
// rows — from the last assistant message.
//
// Deterministic order: outputs.answer, then (chat rows) the last assistant
// message, then outputs.output, then the remaining outputs strings in
// document order. A null or absent outputs maps to an empty Expected that is
// named in Provenance rather than silently: the row is legal in LangSmith,
// and an empty expectation is the honest reading of it.
func mapExpected(rawOutput json.RawMessage, chat []chatMessage) (expected, note string, err error) {
	if isNull(rawOutput) {
		if v, ok := lastAssistant(chat); ok {
			return v, "Expected from the last assistant message", nil
		}
		return "", "outputs is null; Expected left empty", nil
	}
	outKvs, err := orderedObject(rawOutput)
	if err != nil {
		return "", "", errors.New("outputs is not a JSON object")
	}
	if v, ok := kvString(outKvs, keyAnswer); ok {
		return v, "", nil
	}
	if v, ok := lastAssistant(chat); ok {
		return v, "Expected from the last assistant message", nil
	}
	if v, ok := kvString(outKvs, keyOutput); ok {
		return v, "", nil
	}
	if v := docOrderStrings(outKvs, keyAnswer, keyOutput); v != "" {
		return v, "", nil
	}
	return "", "", errors.New("outputs holds no string field")
}

// chatMessage is one LangSmith chat-format message.
type chatMessage struct {
	role    string
	content string
}

// decodeChatMessages extracts the messages array from an inputs object, when
// the row is chat format. The second result reports whether messages was
// present at all: an LLM-format row has no messages key, and that is not an
// error.
func decodeChatMessages(inKvs []kv) ([]chatMessage, bool, error) {
	var rawMessages json.RawMessage
	found := false
	for _, kv := range inKvs {
		if kv.key == keyMessages {
			rawMessages = kv.val
			found = true
			break
		}
	}
	if !found {
		return nil, false, nil
	}

	dec := json.NewDecoder(bytes.NewReader(rawMessages))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, true, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, true, errors.New("messages is not a JSON array")
	}
	var msgs []chatMessage
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, true, err
		}
		kvs, err := orderedObject(raw)
		if err != nil {
			return nil, true, errors.New("a message is not a JSON object")
		}
		role, _ := kvString(kvs, "role")
		content, _ := kvString(kvs, "content")
		msgs = append(msgs, chatMessage{role: role, content: content})
	}
	return msgs, true, nil
}

// joinContents concatenates the message contents, one message per line.
//
// The separator mirrors the jsonl adapter's record format: each line of a
// Case's Input is one prompt turn, and an agent that sees a joined chat
// transcript should see the same turns.
func joinContents(msgs []chatMessage) (string, error) {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.content == "" {
			continue
		}
		parts = append(parts, m.content)
	}
	return strings.Join(parts, "\n"), nil
}

// lastAssistant returns the content of the last assistant message, when the
// conversation has one.
func lastAssistant(msgs []chatMessage) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].role == "assistant" {
			return msgs[i].content, true
		}
	}
	return "", false
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

// kvString returns the value of the first key in keys whose value is a JSON
// string. Keys earlier in the list win over later ones; the first writer of
// a key in the object decides, and a non-string value there does not fall
// through to a later duplicate of the same key.
func kvString(kvs []kv, keys ...string) (string, bool) {
	for _, k := range keys {
		for _, kv := range kvs {
			if kv.key != k {
				continue
			}
			var s string
			if err := json.Unmarshal(kv.val, &s); err == nil {
				return s, true
			}
			break
		}
	}
	return "", false
}

// docOrderStrings joins the string values of the object's remaining keys in
// document order.
//
// skip names the keys the caller already consulted — a named key whose value
// is not a string must not leak in through the document-order door.
func docOrderStrings(kvs []kv, skip ...string) string {
	skipped := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipped[s] = true
	}
	var parts []string
	for _, kv := range kvs {
		if skipped[kv.key] {
			continue
		}
		var s string
		if err := json.Unmarshal(kv.val, &s); err != nil {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
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

// isObject reports whether raw is a JSON object.
func isObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == '{'
		}
	}
	return false
}

// rowID best-effort reads the id field of a row, so the row-cap error can
// name the example.
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

// displayID quotes an example id for an error, so a row missing its id still
// reads coherently.
func displayID(id string) string {
	if id == "" {
		return "(no id)"
	}
	return fmt.Sprintf("%q", id)
}

// pageReader streams the rows of one {items, next_cursor} page envelope.
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
	// itemsDone is set once the items array and whatever envelope keys
	// follow it have been consumed; it guards the one-time finish.
	itemsDone bool
}

// errRowTooLarge marks a scanValue that hit the row cap. The partial value
// is still in the reader's row buffer, so the caller can name the example.
var errRowTooLarge = errors.New("row exceeds the cap")

// newPageReader parses the envelope's head and positions the reader at the
// first row.
//
// Keys are matched by name and anything unknown is skipped raw, so a server
// that adds envelope fields keeps working.
func newPageReader(body io.Reader) (*pageReader, error) {
	pr := &pageReader{buf: bufio.NewReaderSize(body, 64<<10)}

	if err := pr.skipSpace(); err != nil {
		return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
	}
	if b != '{' {
		return nil, errors.New("langsmith: the page envelope is not a JSON object")
	}

	foundItems := false
	for {
		if err := pr.skipSpace(); err != nil {
			return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			break
		}
		if b != '"' {
			return nil, errors.New("langsmith: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return nil, err
		}
		switch key {
		case "items":
			if err := pr.skipSpace(); err != nil {
				return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
			}
			b, err := pr.buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
			}
			if b != '[' {
				return nil, errors.New("langsmith: items is not an array")
			}
			// The items array is now the active context: the head loop stops
			// here. Keys that follow items are read by finishEnvelope once
			// the array is exhausted.
			foundItems = true
			goto positioned
		default:
			raw, err := pr.scanValue()
			if err != nil {
				return nil, fmt.Errorf("langsmith: decoding the page envelope: %w", err)
			}
			if key == "next_cursor" {
				var c string
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, fmt.Errorf("langsmith: decoding next_cursor: %w", err)
				}
				pr.cursor = c
				pr.hasNext = true
			}
		}
	}
positioned:
	if !foundItems {
		return nil, errors.New("langsmith: the page envelope has no items array")
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
		return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
	}
	if b != ':' {
		return fmt.Errorf("langsmith: decoding the page envelope: expected ':' after a key")
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
// which still names the example.
func (pr *pageReader) scanValue() ([]byte, error) {
	pr.row = pr.row[:0]

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

// finishEnvelope consumes the items array's closing bracket and whatever
// envelope keys follow it. It runs exactly once, the first time the array is
// exhausted, so a next_cursor that appears after items is still read.
func (pr *pageReader) finishEnvelope() error {
	if pr.itemsDone {
		return nil
	}
	pr.itemsDone = true

	for {
		if err := pr.skipSpace(); err != nil {
			return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		b, err := pr.buf.ReadByte()
		if err != nil {
			return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		if b == ',' {
			continue // pair separator; the next iteration reads the next key
		}
		if b == '}' {
			return nil
		}
		if b != '"' {
			return errors.New("langsmith: the page envelope has a non-string key")
		}
		key, err := pr.readKeyString()
		if err != nil {
			return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		if err := pr.consumeColon(); err != nil {
			return err
		}
		if key == "items" {
			// A second items array after the first is a shape the stream was
			// never designed to carry; consume its value so the failure is
			// bounded, then refuse it.
			if _, err := pr.scanValue(); err != nil {
				return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
			}
			return errors.New("langsmith: the page envelope has two items arrays")
		}
		raw, err := pr.scanValue()
		if err != nil {
			return fmt.Errorf("langsmith: decoding the page envelope: %w", err)
		}
		if key == "next_cursor" {
			var c string
			if err := json.Unmarshal(raw, &c); err != nil {
				return fmt.Errorf("langsmith: decoding next_cursor: %w", err)
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
		return nil, false, fmt.Errorf("langsmith: reading the page: %w", err)
	}
	b, err := pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("langsmith: the page envelope is truncated after items")
	}
	if b == ']' {
		if err := pr.finishEnvelope(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := pr.buf.UnreadByte(); err != nil {
		return nil, false, fmt.Errorf("langsmith: reading the page: %w", err)
	}

	raw, err := pr.scanValue()
	if err != nil {
		switch {
		case errors.Is(err, errRowTooLarge):
			id := rowID(raw)
			if id == "" {
				id = "(id unreadable)"
			}
			return nil, false, fmt.Errorf("langsmith: example %s exceeds the %d-byte row cap; "+
				"an example is a prompt and an expectation, not a corpus", id, maxRowBytes)
		case errors.Is(err, io.EOF):
			return nil, false, errors.New("langsmith: the page envelope is truncated after items")
		default:
			return nil, false, fmt.Errorf("langsmith: reading the page: %w", err)
		}
	}

	// The array continues if the next significant byte is a ','. A ']' ends
	// it; the byte is restored so the next call's end-of-array path sees it.
	if err := pr.skipSpace(); err != nil {
		return nil, false, fmt.Errorf("langsmith: reading the page: %w", err)
	}
	b, err = pr.buf.ReadByte()
	if err != nil {
		return nil, false, errors.New("langsmith: the page envelope is truncated after items")
	}
	if b == ']' {
		if err := pr.buf.UnreadByte(); err != nil {
			return nil, false, fmt.Errorf("langsmith: reading the page: %w", err)
		}
	}
	return raw, true, nil
}

// nextCursor reports the page's continuation cursor, and whether the stream
// should continue at all. A page with no next_cursor field is the last page;
// an empty next_cursor string is the same.
func (pr *pageReader) nextCursor() (string, bool) {
	if !pr.hasNext {
		return "", false
	}
	return pr.cursor, pr.cursor != ""
}
