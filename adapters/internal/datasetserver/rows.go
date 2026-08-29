package datasetserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Row is one dataset row: its index within the split, and its raw JSON
// object. The row object is kept raw so the adapters can do their own column
// discovery and value decoding with ValueString — the mapping from row to
// Case or Asset is adapter vocabulary, not transport vocabulary.
type Row struct {
	// RowIdx is the row's index within the split, from the server's row_idx.
	RowIdx int64

	// Row is the raw JSON object for the row, exactly as the server sent it.
	Row json.RawMessage
}

// Page is one page of rows plus the envelope fields.
type Page struct {
	// Rows is this page's rows, in server order.
	Rows []Row

	// NumRowsTotal is the split's total row count as the server reports it.
	NumRowsTotal int64

	// Partial is the server's subsample flag. True means the split is a
	// partial subsample — see OpenPage for the refusal.
	Partial bool

	// Revision is the x-revision header from THIS page's response. The
	// adapters compare it against the first page's revision and treat any
	// drift as fatal: the dataset changed mid-read, and the split is no
	// longer one object.
	Revision string
}

// OpenPage fetches one page of rows starting at offset.
//
// The request carries exactly dataset, config, split, offset, and length —
// never a revision query parameter, which the server ignores (the header is
// the fingerprint, per the package doc).
//
// Refusals, in order:
//
//   - A 404 names the config/split pair: the dataset resolved but this pair
//     is not served, which is the one place a 404 is meaningful on this API.
//   - A partial:true envelope is refused — a subsample would shift the
//     holdout statistics with every server-side change, and no one asked
//     for a different population than the split names.
//   - A response without the x-revision header is refused as broken.
//   - An envelope missing any of rows, num_rows_total, or partial is refused
//     and names the missing field: a contract violation is a contract
//     violation, and silently defaulting partial to false would un-refuse
//     the one signal that must never be defaulted.
//   - A row missing row_idx or row is refused and names the row.
//
// Any other status is a statusError. The caller owns the body of the
// returned response; OpenPage always closes what it does not return.
func (c *Client) OpenPage(ctx context.Context, dataset, config, split string, offset int64) (*Page, error) {
	q := url.Values{}
	q.Set("dataset", dataset)
	q.Set("config", config)
	q.Set("split", split)
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("length", strconv.Itoa(PageSize))

	resp, err := c.do(ctx, "/rows", q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// The only meaningful 404 on this API: the dataset resolved but the
		// pair does not exist. It names the pair so the fix does not require
		// a second round-trip to find out what the dataset does offer.
		return nil, fmt.Errorf("the datasets-server API answered 404 for config %q split %q of "+
			"dataset %q: no such split", config, split, dataset)
	default:
		return nil, c.statusError(dataset, config, split, resp)
	}

	rev, err := c.revision(resp)
	if err != nil {
		return nil, err
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	page, err := decodePage(body)
	if err != nil {
		return nil, err
	}
	if page.Partial {
		return nil, fmt.Errorf("the server marked config %q split %q of dataset %q partial "+
			"in the response — it served a subsample of the split, not the split. Refused: a "+
			"subsample would change the measurement's population without being asked for", config, split, dataset)
	}
	page.Revision = rev
	return page, nil
}

// decodePage parses the rows envelope and validates every field it depends
// on. See OpenPage for why the validation is presence-checked rather than
// zero-value-tolerated.
func decodePage(body []byte) (*Page, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("the rows response is not a JSON object")
	}
	for _, k := range []string{"rows", "num_rows_total", "partial"} {
		if _, ok := env[k]; !ok {
			return nil, fmt.Errorf("the rows envelope has no %q field; the envelope is "+
				"rows, num_rows_total, partial", k)
		}
	}

	// A JSON string also unmarshals into json.Number, so the leading byte is
	// checked first: num_rows_total is an integer literal or the envelope is
	// lying about its own shape.
	rawTotal := env["num_rows_total"]
	if len(rawTotal) == 0 || !isJSONIntLiteral(rawTotal) {
		return nil, fmt.Errorf("num_rows_total is not a JSON number")
	}
	var total json.Number
	if err := json.Unmarshal(rawTotal, &total); err != nil {
		return nil, fmt.Errorf("num_rows_total is not a JSON number")
	}
	n, err := total.Int64()
	if err != nil {
		return nil, fmt.Errorf("num_rows_total is not a JSON integer")
	}

	var partial bool
	if err := json.Unmarshal(env["partial"], &partial); err != nil {
		return nil, fmt.Errorf("partial is not a JSON boolean")
	}

	var rawRows []json.RawMessage
	if err := json.Unmarshal(env["rows"], &rawRows); err != nil {
		return nil, fmt.Errorf("rows is not a JSON array")
	}
	rows := make([]Row, 0, len(rawRows))
	for i, raw := range rawRows {
		row, err := decodeRow(raw, i)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return &Page{Rows: rows, NumRowsTotal: n, Partial: partial}, nil
}

// isJSONIntLiteral reports whether raw is a JSON integer literal: an
// optional minus followed by digits, with no leading plus, fraction, or
// exponent. A strictness note would live in the function name if Go allowed
// it.
func isJSONIntLiteral(raw []byte) bool {
	i := 0
	if raw[0] == '-' {
		i++
	}
	if i == len(raw) || raw[i] < '0' || raw[i] > '9' {
		return false
	}
	for ; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return false
		}
	}
	return true
}

// decodeRow parses one row object and requires both of its fields. A row
// without row_idx cannot be addressed and a row without a row object has no
// content — either is a server contract violation worth a named refusal.
func decodeRow(raw json.RawMessage, i int) (Row, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Row{}, fmt.Errorf("row %d is not a JSON object", i)
	}
	rix, ok := m["row_idx"]
	if !ok {
		return Row{}, fmt.Errorf("row %d has no row_idx field", i)
	}
	var idx int64
	if err := json.Unmarshal(rix, &idx); err != nil {
		return Row{}, fmt.Errorf("row %d has a row_idx that is not an integer", i)
	}
	rv, ok := m["row"]
	if !ok {
		return Row{}, fmt.Errorf("row %d has no row field", i)
	}
	return Row{RowIdx: idx, Row: rv}, nil
}
