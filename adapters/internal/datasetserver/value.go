package datasetserver

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ValueString renders one row value the way the adapters consume it.
//
// The rules are the mapping rules both adapters share, pinned here so the
// Evals and Pool adapters cannot drift apart on them:
//
//   - A JSON string is returned as itself, verbatim.
//   - JSON null is reported with isNull true; null is the caller's problem,
//     not a rendered string, because the Evals adapter answers null input
//     with a fatal and the pool treats null as a non-column.
//   - Any other JSON value — object, array, number, boolean — is returned
//     as canonical JSON: keys sorted, HTML-escaped, decoded with UseNumber
//     so numbers round-trip exactly. The same structured value therefore
//     maps identically wherever it appears, no matter the server's key
//     order. This is the langfuse adapter's canonicalJSON pattern.
func ValueString(raw json.RawMessage) (s string, isNull bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false, fmt.Errorf("the row value is not valid JSON")
	}
	if v == nil {
		return "", true, nil
	}
	if str, ok := v.(string); ok {
		return str, false, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", false, fmt.Errorf("canonicalizing the row value: %w", err)
	}
	return string(out), false, nil
}
