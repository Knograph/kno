package datasetserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// retryPause is a package var so tests can shrink it; the 5xx-retry tests
// would otherwise sleep two full pauses. The whole test binary runs with the
// short pause.
func init() {
	retryPause = 5 * time.Millisecond
}

const testRevision = "abc123def"

// newClient starts a fake datasets-server and returns a Client pointed at it
// with the security flags lifted (httptest serves plain HTTP on loopback).
func newClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{
		Host:                 srv.URL,
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// recording is the fake server: a configurable status and body per endpoint,
// plus the requests it served, so tests can assert on query strings and
// headers.
type recording struct {
	splitsStatus int
	splitsBody   string

	rowsStatus int
	rowsBodies map[string]string // keyed by offset query value
	rowsPages  []*http.Request   // in serve order
	requests   []*http.Request
	noRevision bool
	redirect   string // non-empty: answer a 302 to this location
}

func newRecording() *recording {
	return &recording{splitsStatus: 200, rowsStatus: 200}
}

func (r *recording) serve(t *testing.T) *Client {
	return newClient(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests = append(r.requests, req)
		if !r.noRevision {
			w.Header().Set("x-revision", testRevision)
		}
		if r.redirect != "" {
			http.Redirect(w, req, r.redirect, http.StatusFound)
			return
		}
		switch req.URL.Path {
		case "/splits":
			w.WriteHeader(r.splitsStatus)
			if r.splitsStatus == http.StatusOK {
				_, _ = fmt.Fprint(w, r.splitsBody)
			}
		case "/rows":
			r.rowsPages = append(r.rowsPages, req)
			w.WriteHeader(r.rowsStatus)
			if r.rowsStatus == http.StatusOK {
				_, _ = fmt.Fprint(w, r.rowsBodies[req.URL.Query().Get("offset")])
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSplitsResolvesAndCapturesRevision(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.splitsBody = `{"splits":[
		{"dataset":"org/name","config":"main","split":"train"},
		{"dataset":"org/name","config":"main","split":"test"},
		{"dataset":"org/name","config":"alt","split":"train"}]}`
	c := rec.serve(t)

	splits, rev, err := c.Splits(context.Background(), "org/name")
	if err != nil {
		t.Fatal(err)
	}
	if rev != testRevision {
		t.Fatalf("revision = %q, want %q", rev, testRevision)
	}
	if len(splits) != 3 {
		t.Fatalf("got %d splits, want 3", len(splits))
	}
	if !HasSplit(splits, "main", "train") || HasSplit(splits, "main", "valid") || HasSplit(splits, "alt", "test") {
		t.Fatalf("HasSplit does not match the resolved list: %v", splits)
	}

	q := rec.requests[0].URL.Query()
	if got := q.Get("dataset"); got != "org/name" {
		t.Fatalf("dataset query = %q", got)
	}
	if _, ok := q["revision"]; ok {
		t.Fatal("the request carried a revision query parameter, which the server ignores")
	}
}

func TestSplits401OffersBothRemedies(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.splitsStatus = http.StatusUnauthorized
	c := rec.serve(t)

	_, _, err := c.Splits(context.Background(), "org/name")
	if err == nil {
		t.Fatal("Splits accepted a 401")
	}
	for _, want := range []string{"does not exist", "HF_TOKEN", "gated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the 401 refusal does not offer the %q remedy: %v", want, err)
		}
	}
}

func TestSplits401WithTokenNamesTheTokenCheck(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.splitsStatus = http.StatusUnauthorized
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	c.token = "hf_test_secret_token"

	_, _, err := c.Splits(context.Background(), "org/name")
	if err == nil {
		t.Fatal("Splits accepted a 401")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") || !strings.Contains(err.Error(), "current") {
		t.Fatalf("the 401 refusal does not point at the token being current: %v", err)
	}
	if strings.Contains(err.Error(), "hf_test_secret_token") {
		t.Fatalf("the 401 refusal echoes the token: %v", err)
	}
}

func TestSplitsMissingRevisionIsFatal(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.noRevision = true
	rec.splitsBody = `{"splits":[]}`
	c := rec.serve(t)

	if _, _, err := c.Splits(context.Background(), "org/name"); err == nil {
		t.Fatal("Splits accepted a response without the x-revision header")
	} else if !strings.Contains(err.Error(), "x-revision") {
		t.Fatalf("the refusal does not name the header: %v", err)
	}
}

func TestSplitsMalformedEnvelopeIsFatal(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"notsplits":[]}`,
		`{"splits":[{"config":"main"}]}`,
		`{"splits":[{"config":"","split":""}]}`,
		`not json at all`,
	} {
		rec := newRecording()
		rec.splitsBody = body
		c := rec.serve(t)
		if _, _, err := c.Splits(context.Background(), "org/name"); err == nil {
			t.Fatalf("Splits accepted a malformed envelope: %s", body)
		}
	}
}

func TestOpenPageDecodesTheEnvelope(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.rowsBodies = map[string]string{
		"0": `{"rows":[
			{"row_idx":0,"row":{"input":"what is the capital?"}},
			{"row_idx":1,"row":{"input":"what is 2+2?","expected":"4"}}],
			"num_rows_total":2,"partial":false}`,
	}
	c := rec.serve(t)

	page, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Revision != testRevision {
		t.Fatalf("page revision = %q", page.Revision)
	}
	if page.NumRowsTotal != 2 || page.Partial || len(page.Rows) != 2 {
		t.Fatalf("page envelope decoded wrong: %+v", page)
	}
	if page.Rows[0].RowIdx != 0 || string(page.Rows[0].Row) != `{"input":"what is the capital?"}` {
		t.Fatalf("row 0 decoded wrong: %+v", page.Rows[0])
	}

	q := rec.requests[0].URL.Query()
	for k, want := range map[string]string{
		"dataset": "org/name", "config": "main", "split": "train",
		"offset": "0", "length": "100",
	} {
		if got := q.Get(k); got != want {
			t.Fatalf("query %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := q["revision"]; ok {
		t.Fatal("the rows request carried a revision query parameter, which the server ignores")
	}
}

func TestOpenPagePartialIsRefused(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.rowsBodies = map[string]string{"0": `{"rows":[{"row_idx":0,"row":{"input":"x"}}],"num_rows_total":1,"partial":true}`}
	c := rec.serve(t)

	_, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err == nil {
		t.Fatal("OpenPage accepted a partial subsample")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Fatalf("the refusal does not name partiality: %v", err)
	}
}

func TestOpenPage404NamesThePair(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.rowsStatus = http.StatusNotFound
	c := rec.serve(t)

	_, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err == nil {
		t.Fatal("OpenPage accepted a 404")
	}
	for _, want := range []string{"main", "train", "org/name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the 404 refusal does not name %q: %v", want, err)
		}
	}
}

func TestOpenPageMissingRevisionIsFatal(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.noRevision = true
	rec.rowsBodies = map[string]string{"0": `{"rows":[],"num_rows_total":0,"partial":false}`}
	c := rec.serve(t)

	if _, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0); err == nil {
		t.Fatal("OpenPage accepted a response without the x-revision header")
	}
}

func TestOpenPageEnvelopeValidationNamesTheField(t *testing.T) {
	t.Parallel()
	bodies := []struct {
		name string
		body string
		want string
	}{
		{name: "no num_rows_total", body: `{"rows":[],"partial":false}`, want: "num_rows_total"},
		{name: "no partial", body: `{"rows":[],"num_rows_total":0}`, want: "partial"},
		{name: "no rows", body: `{"num_rows_total":0,"partial":false}`, want: "rows"},
		{name: "num_rows_total not an integer", body: `{"rows":[],"num_rows_total":"0","partial":false}`, want: "not a JSON number"},
		{name: "partial not a boolean", body: `{"rows":[],"num_rows_total":0,"partial":"no"}`, want: "not a JSON boolean"},
		{name: "row without row_idx", body: `{"rows":[{"row":{"input":"x"}}],"num_rows_total":1,"partial":false}`, want: "row_idx"},
		{name: "row without row", body: `{"rows":[{"row_idx":0}],"num_rows_total":1,"partial":false}`, want: "row"},
	}
	for _, tt := range bodies {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecording()
			rec.rowsBodies = map[string]string{"0": tt.body}
			c := rec.serve(t)
			_, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
			if err == nil {
				t.Fatalf("OpenPage accepted a broken envelope: %s", tt.body)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the refusal does not name %q: %v", tt.want, err)
			}
		})
	}
}

func TestRows500IsRetriedThenSucceeds(t *testing.T) {
	var calls int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-revision", testRevision)
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"rows":[],"num_rows_total":0,"partial":false}`)
	}))

	page, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("served %d requests, want 3 (two 5xx then success)", got)
	}
	if page.NumRowsTotal != 0 {
		t.Fatalf("page decoded wrong after retries: %+v", page)
	}
}

func TestRows500PersistentIsBounded(t *testing.T) {
	var calls int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))

	_, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err == nil {
		t.Fatal("OpenPage accepted a persistently failing server")
	}
	if got := atomic.LoadInt32(&calls); got != maxAttempts {
		t.Fatalf("served %d requests, want exactly %d", got, maxAttempts)
	}
	if !strings.Contains(err.Error(), "kept answering") {
		t.Fatalf("the exhaustion refusal does not say what happened: %v", err)
	}
}

func TestRows429IsNotRetried(t *testing.T) {
	var calls int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("x-revision", testRevision)
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	_, err := c.OpenPage(context.Background(), "org/name", "main", "train", 0)
	if err == nil {
		t.Fatal("OpenPage accepted a 429")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("served %d requests, want 1 — a 429 is the server asking for less load, not more", got)
	}
}

// TestNoErrorEchoesTheToken drives the token through every failure class and
// asserts no error text contains it.
func TestNoErrorEchoesTheToken(t *testing.T) {
	const token = "hf_test_secret_token"

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "401 splits", status: http.StatusUnauthorized, body: ""},
		{name: "500 splits", status: http.StatusInternalServerError, body: ""},
		{name: "500 rows", status: http.StatusInternalServerError, body: ""},
		{name: "404 rows", status: http.StatusNotFound, body: ""},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			c.token = token

			var err error
			if tt.name == "401 splits" || tt.name == "500 splits" {
				_, _, err = c.Splits(context.Background(), "org/name")
			} else {
				_, err = c.OpenPage(context.Background(), "org/name", "main", "train", 0)
			}
			if err == nil {
				t.Fatal("the call succeeded when it should have failed")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("an error echoed the token: %v", err)
			}
		})
	}
}

func TestRedirectIsRefused(t *testing.T) {
	t.Parallel()
	rec := newRecording()
	rec.redirect = "https://elsewhere.example.com/rows"
	rec.splitsBody = `{"splits":[]}`
	c := rec.serve(t)

	_, _, err := c.Splits(context.Background(), "org/name")
	if err == nil {
		t.Fatal("a redirect was followed; the token would have traveled with it")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("the refusal does not say a redirect was refused: %v", err)
	}
}

func TestNewRefusesUnsafeHosts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		host     string
		allowInc bool
		allowPrv bool
		want     string
	}{
		{name: "plain http", host: "http://datasets.example.com", want: "plain HTTP"},
		{name: "private literal", host: "https://127.0.0.1:8000", want: "private address"},
		{name: "link-local", host: "https://169.254.169.254", allowPrv: true, want: "link-local"},
		{name: "userinfo credential", host: "https://sk-secret@host", want: "userinfo"},
		{name: "unparseable", host: "://host", want: "could not be parsed"},
		{name: "flag lifts http", host: "http://datasets.example.com", allowInc: true, want: ""},
		{name: "flag lifts private", host: "http://127.0.0.1:8000", allowInc: true, allowPrv: true, want: ""},
		{name: "default host", host: "", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Host: tt.host, AllowInsecureBaseURL: tt.allowInc, AllowPrivateAddress: tt.allowPrv})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New(%q) = %v, want nil", tt.host, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New(%q) = nil, want error containing %q", tt.host, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New(%q) error %q does not contain %q", tt.host, err, tt.want)
			}
		})
	}
}

// TestValueString pins the canonical-value rules: strings verbatim, null
// reported, everything else canonical JSON — key-sorted, HTML-escaped, and
// number-exact.
func TestValueString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		raw    string
		want   string
		isNull bool
	}{
		{name: "string verbatim", raw: `"what is the capital?"`, want: "what is the capital?"},
		{name: "escaped string", raw: `"line\nbreak"`, want: "line\nbreak"},
		{name: "null", raw: `null`, isNull: true},
		{name: "object key-sorted", raw: `{"b":1,"a":2}`, want: `{"a":2,"b":1}`},
		{name: "object html-escaped", raw: `{"a":"<b>&</b>"}`, want: `{"a":"\u003cb\u003e\u0026\u003c/b\u003e"}`},
		{name: "nested object", raw: `{"z":{"y":[1,{"x":2}]}}`, want: `{"z":{"y":[1,{"x":2}]}}`},
		{name: "array", raw: `[3,1,2]`, want: `[3,1,2]`},
		{name: "number literal preserved", raw: `42.10`, want: `42.10`},
		{name: "large integer exact", raw: `9007199254740993`, want: `9007199254740993`},
		{name: "boolean", raw: `true`, want: `true`},
		{name: "invalid json", raw: `{`, want: "", isNull: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, isNull, err := ValueString(json.RawMessage(tt.raw))
			if tt.name == "invalid json" {
				if err == nil {
					t.Fatal("ValueString accepted invalid JSON")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if isNull != tt.isNull || got != tt.want {
				t.Fatalf("ValueString(%s) = (%q, %v), want (%q, %v)", tt.raw, got, isNull, tt.want, tt.isNull)
			}
		})
	}
}

func TestHostNormalizationTrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/splits" {
			t.Fatalf("path = %q, want /splits — the trailing slash was not trimmed", req.URL.Path)
		}
		w.Header().Set("x-revision", testRevision)
		_, _ = fmt.Fprint(w, `{"splits":[]}`)
	}))
	defer srv.Close()

	c, err := New(Options{Host: srv.URL + "/", AllowInsecureBaseURL: true, AllowPrivateAddress: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Splits(context.Background(), "org/name"); err != nil {
		t.Fatalf("Splits with a trailing-slash host: %v", err)
	}
}
