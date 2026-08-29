package braintrust_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/knograph/kno/adapters/evals/braintrust"
	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/langfuse"
	"github.com/knograph/kno/adapters/evals/langsmith"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeBraintrust is a minimal stand-in for the Braintrust REST API, serving
// the fixtures in the shapes the real one uses: a bare JSON array of
// dataset objects at /v1/dataset (filtered by the dataset_name query) and a
// {events, cursor} envelope at /v1/dataset/{id}/fetch, keyed by the opaque
// cursor.
type fakeBraintrust struct {
	datasetList string            // raw /v1/dataset response (a JSON array)
	pages       map[string]string // fetch cursor -> raw {events, cursor} envelope

	// unauthorized 401s every datasets request.
	unauthorized bool
	// retryOnce 429s the first fetch request, then serves normally.
	retryOnce bool
	// always429 429s every fetch request.
	always429 bool

	mu              sync.Mutex
	srv             *httptest.Server
	datasetsReq     int // /v1/dataset requests seen
	fetchReq        int // /v1/dataset/{id}/fetch requests seen
	lastPath        string
	lastAuth        string
	lastDatasetName string // dataset_name query of the most recent datasets request
	lastCursor      string // cursor query of the most recent fetch request
	lastLimit       string // limit query of the most recent fetch request
	lastOrg         string // org_name query of the most recent request
}

func newFake(t *testing.T, datasetList string, pages map[string]string) *fakeBraintrust {
	t.Helper()
	f := &fakeBraintrust{datasetList: datasetList, pages: pages}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBraintrust) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.lastPath = r.URL.RequestURI()
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/v1/dataset":
		f.mu.Lock()
		f.datasetsReq++
		f.lastDatasetName = r.URL.Query().Get("dataset_name")
		f.lastOrg = r.URL.Query().Get("org_name")
		f.mu.Unlock()
		if f.unauthorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, f.datasetList)
	case strings.HasPrefix(r.URL.Path, "/v1/dataset/") && strings.HasSuffix(r.URL.Path, "/fetch"):
		f.mu.Lock()
		f.fetchReq++
		first := f.fetchReq == 1
		f.lastCursor = r.URL.Query().Get("cursor")
		f.lastLimit = r.URL.Query().Get("limit")
		f.lastOrg = r.URL.Query().Get("org_name")
		f.mu.Unlock()
		if f.always429 || (f.retryOnce && first) {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		cursor := r.URL.Query().Get("cursor")
		f.mu.Lock()
		body, ok := f.pages[cursor]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "unknown cursor", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

// datasetNameOf extracts the name of the dataset in a raw /v1/dataset list,
// so an Evals built against the fake requests the name the fixture actually
// carries — the adapter resolves by exact name, and the fake serves whatever
// list it was configured with.
func datasetNameOf(raw string) string {
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return ""
	}
	if len(list) == 0 {
		return ""
	}
	return list[0].Name
}

// newEvals builds an Evals against the fake server, opted into the insecure
// and private settings every fixture endpoint (127.0.0.1, plain http)
// requires. Keys are explicit so no test depends on the environment.
func (f *fakeBraintrust) newEvals(t *testing.T, opts ...func(*braintrust.Options)) *braintrust.Evals {
	t.Helper()
	dataset := "fixture-dataset"
	if name := datasetNameOf(f.datasetList); name != "" {
		dataset = name
	}
	o := braintrust.Options{
		Dataset:              dataset,
		Host:                 f.srv.URL,
		APIKey:               "bt-key",
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	}
	for _, fn := range opts {
		fn(&o)
	}
	ev, err := braintrust.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ev
}

// fixture reads one hand-authored fixture file (see testdata/note.txt).
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(b)
}

// collectCases runs an Evals to completion, failing on any fatal error.
func collectCases(t *testing.T, ev *braintrust.Evals) []*knov1.Case {
	t.Helper()
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var out []*knov1.Case
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// collectSplits runs any Evals-shaped iterator and records id -> split.
func collectSplits(t *testing.T, seq iter.Seq2[*core.Case, error]) map[string]knov1.Split {
	t.Helper()
	out := make(map[string]knov1.Split)
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		out[c.GetId()] = c.GetSplit()
	}
	return out
}

// datasetListWith builds a /v1/dataset response (a bare array) with one
// dataset.
func datasetListWith(name string) string {
	return fmt.Sprintf(`[{"id":"8f1c4b2e-5d6a-4f8e-9c2d-1a3b5c7d9e0f","name":%q,`+
		`"project_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","created_at":"2026-08-01T12:00:00Z"}]`, name)
}

// pageWith builds a {events, cursor} envelope from raw rows, as the one and
// only page of a dataset.
func pageWith(events ...string) string {
	return fmt.Sprintf(`{"events":[%s],"cursor":""}`, strings.Join(events, ","))
}

// eventWith builds one dataset event row; extra fields (raw "key":value
// pairs) land inside the row object before the closing brace.
func eventWith(id, xactID, input, expected string, extra ...string) string {
	s := fmt.Sprintf(`{"id":%q,"input":%s,"expected":%s,"_xact_id":%q`, id, input, expected, xactID)
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	ev := f.newEvals(t)

	coretest.ConformIterator(t, func(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
		return ev.Cases(ctx)
	})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	coretest.EvalsDuplicateIDs(t, seq)
}

// probeTransport answers every request with the given bodies, one probe per
// request, so a test can assert each response body was closed.
type probeTransport struct {
	// bodies per request, in order; the last one repeats for later requests.
	bodies [][]byte

	mu     sync.Mutex
	next   int
	probes []*coretest.CleanupProbe
}

func (t *probeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	body := t.bodies[0]
	if t.next < len(t.bodies) {
		body = t.bodies[t.next]
	}
	probe := &coretest.CleanupProbe{}
	t.probes = append(t.probes, probe)
	t.next++
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       &probeBody{probe: probe, r: bytes.NewReader(body)},
	}, nil
}

// probeBody is an io.ReadCloser whose Close is recorded on the probe.
type probeBody struct {
	probe *coretest.CleanupProbe
	r     *bytes.Reader
}

func (b *probeBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *probeBody) Close() error               { return b.probe.Close() }

func TestEarlyBreakClosesThePageBody(t *testing.T) {
	t.Parallel()

	transport := &probeTransport{
		bodies: [][]byte{
			[]byte(fixture(t, "dataset-list.json")), // request 0: the dataset list
			[]byte(fixture(t, "events-page1.json")), // requests 1+: pages
		},
	}
	ev, err := braintrust.New(braintrust.Options{
		Dataset: "support-llm",
		Host:    "https://example.com", // never dialed; the probe intercepts
		APIKey:  "bt-key",
		HTTPClient: &http.Client{
			Transport: transport,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	for range seq {
		break // the consumer loses interest after the first case
	}

	// Request 0 (the dataset list) is closed by resolveDataset; request 1
	// (the open fetch page) must have been closed by the iterator's own
	// deferred cleanup, which is what an early break runs.
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.probes) < 2 {
		t.Fatalf("expected at least 2 requests, saw %d", len(transport.probes))
	}
	transport.probes[0].AssertClosed(t)
	transport.probes[1].AssertClosed(t)
}

func TestEventMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		row          string
		wantInput    string
		wantExpected string
		wantNote     string // substring of the provenance derivation note
		wantDerived  bool
		wantErr      string // substring of the fatal error
	}{
		{
			name:         "question and answer, canonicalized",
			row:          eventWith("it-1", "10", `{"question":"q1"}`, `{"answer":"a1"}`),
			wantInput:    `{"question":"q1"}`,
			wantExpected: `{"answer":"a1"}`,
		},
		{
			name:         "object keys sorted, regardless of document order",
			row:          eventWith("it-2", "11", `{"z":1,"a":{"y":2,"b":3}}`, `{"z":"x","a":"y"}`),
			wantInput:    `{"a":{"b":3,"y":2},"z":1}`,
			wantExpected: `{"a":"y","z":"x"}`,
		},
		{
			name:         "large integers survive the canonical pass",
			row:          eventWith("it-2b", "12", `{"n":12345678901234567890}`, `{"answer":"a"}`),
			wantInput:    `{"n":12345678901234567890}`,
			wantExpected: `{"answer":"a"}`,
		},
		{
			name:         "tags are not read",
			row:          eventWith("it-2c", "13", `{"question":"q"}`, `{"answer":"a"}`, `"tags":["high","priority"]`),
			wantInput:    `{"question":"q"}`,
			wantExpected: `{"answer":"a"}`,
		},
		{
			name:         "null expected is an empty expected, named",
			row:          eventWith("it-3", "14", `{"question":"q3"}`, "null"),
			wantInput:    `{"question":"q3"}`,
			wantExpected: "",
			wantNote:     "expected is null",
		},
		{
			name:         "absent expected is an empty expected, named",
			row:          `{"id":"it-3b","input":{"question":"q3b"},"_xact_id":"15"}`,
			wantInput:    `{"question":"q3b"}`,
			wantExpected: "",
			wantNote:     "expected is null",
		},
		{
			name:         "copied from an experiment is derived, named",
			row:          eventWith("it-4", "16", `{"question":"q4"}`, `{"answer":"a4"}`, `"origin":{"object_type":"experiment","object_id":"exp-1","_xact_id":"501"}`),
			wantInput:    `{"question":"q4"}`,
			wantExpected: `{"answer":"a4"}`,
			wantDerived:  true,
			wantNote:     "copied from a experiment (object_id exp-1)",
		},
		{
			name:         "copied from a span is derived, named",
			row:          eventWith("it-5", "17", `{"question":"q5"}`, `{"answer":"a5"}`, `"origin":{"object_type":"span","object_id":"span-9","_xact_id":"502"}`),
			wantInput:    `{"question":"q5"}`,
			wantExpected: `{"answer":"a5"}`,
			wantDerived:  true,
			wantNote:     "copied from a span (object_id span-9)",
		},
		{
			name:         "copied from an eval result is derived, named",
			row:          eventWith("it-6", "18", `{"question":"q6"}`, `{"answer":"a6"}`, `"origin":{"object_type":"eval_result","object_id":"eval-3","_xact_id":"503"}`),
			wantInput:    `{"question":"q6"}`,
			wantExpected: `{"answer":"a6"}`,
			wantDerived:  true,
			wantNote:     "copied from a eval_result (object_id eval-3)",
		},
		{
			name:         "origin without an object_type still marks derived",
			row:          eventWith("it-6b", "19", `{"question":"q6b"}`, `{"answer":"a6b"}`, `"origin":{"object_id":"obj-1","_xact_id":"504"}`),
			wantInput:    `{"question":"q6b"}`,
			wantExpected: `{"answer":"a6b"}`,
			wantDerived:  true,
			wantNote:     "copied from another object (object_id obj-1)",
		},
		{
			name:         "authored event stays underived",
			row:          eventWith("it-7", "20", `{"question":"q7"}`, `{"answer":"a7"}`),
			wantInput:    `{"question":"q7"}`,
			wantExpected: `{"answer":"a7"}`,
		},
		{
			name:         "derived and null expected name both decisions",
			row:          eventWith("it-7b", "21", `{"question":"q7b"}`, "null", `"origin":{"object_type":"span","object_id":"s-2","_xact_id":"505"}`),
			wantInput:    `{"question":"q7b"}`,
			wantExpected: "",
			wantDerived:  true,
			wantNote:     "copied from a span (object_id s-2); expected is null",
		},
		{
			name:    "null input is fatal",
			row:     eventWith("it-8", "22", "null", `{"answer":"a8"}`),
			wantErr: "null input",
		},
		{
			name:    "a row with a malformed input value is fatal at the row",
			row:     `{"id":"it-9","input":{"unclosed":` + `,"expected":{"answer":"a"},"_xact_id":"23"}`,
			wantErr: "the event row is not a JSON object",
		},
		{
			name:    "an event without a _xact_id is fatal at the case",
			row:     `{"id":"it-10","input":{"question":"q"},"expected":{"answer":"a"}}`,
			wantErr: "no _xact_id",
		},
		{
			name:    "an event without an id is fatal at the case",
			row:     `{"input":{"question":"q"},"expected":{"answer":"a"},"_xact_id":"24"}`,
			wantErr: "event has no id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, datasetListWith("fixture-dataset"), map[string]string{"": pageWith(tt.row)})
			ev := f.newEvals(t)

			seq, err := ev.Cases(context.Background())
			if err != nil {
				t.Fatalf("Cases: %v", err)
			}
			var got *knov1.Case
			var gotErr error
			for c, err := range seq {
				if err != nil {
					gotErr = err
					break
				}
				got = c
			}
			if tt.wantErr != "" {
				if gotErr == nil {
					t.Fatalf("expected a fatal error containing %q, got none", tt.wantErr)
				}
				if !strings.Contains(gotErr.Error(), tt.wantErr) {
					t.Fatalf("fatal error %q does not contain %q", gotErr, tt.wantErr)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("unexpected fatal error: %v", gotErr)
			}
			if got == nil {
				t.Fatal("no case yielded")
			}
			if got.GetInput() != tt.wantInput {
				t.Errorf("Input = %q, want %q", got.GetInput(), tt.wantInput)
			}
			if got.GetExpected() != tt.wantExpected {
				t.Errorf("Expected = %q, want %q", got.GetExpected(), tt.wantExpected)
			}
			p := got.GetProvenance()
			if p == nil {
				t.Fatal("no provenance")
			}
			if p.GetSource() != "braintrust" {
				t.Errorf("Source = %q, want braintrust", p.GetSource())
			}
			if want := "dataset:fixture-dataset:" + got.GetId(); p.GetSourceRef() != want {
				t.Errorf("SourceRef = %q, want %q", p.GetSourceRef(), want)
			}
			if p.GetDerived() != tt.wantDerived {
				t.Errorf("Derived = %v, want %v", p.GetDerived(), tt.wantDerived)
			}
			if tt.wantNote != "" && !strings.Contains(p.GetDerivationNote(), tt.wantNote) {
				t.Errorf("DerivationNote = %q, want it to contain %q", p.GetDerivationNote(), tt.wantNote)
			}
		})
	}
}

func TestPaginationAcrossPages(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	want := []string{
		"1f0e0e6d-2d3e-4a1f-9a5c-0d2f6e1b4a3c",
		"2d1f2f7e-3e4f-4b2a-9b6d-1e3f7a2c5b4d",
		"3e2a3d8f-4f5a-4c3b-9c7e-2f4a8b3d6c5e",
		"4f3b4e9a-5a6b-4d4c-9d8f-3a5b9c4e7d6f",
	}
	if len(cases) != len(want) {
		t.Fatalf("got %d cases, want %d", len(cases), len(want))
	}
	for i, c := range cases {
		if c.GetId() != want[i] {
			t.Errorf("case %d id = %q, want %q (order must survive pagination)", i, c.GetId(), want[i])
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastDatasetName != "support-llm" {
		t.Errorf("the dataset_name query = %q, want %q", f.lastDatasetName, "support-llm")
	}
	if f.lastLimit != "100" {
		t.Errorf("the limit query = %q, want 100", f.lastLimit)
	}
	if f.lastCursor != "cur-2" {
		t.Errorf("the cursor query of the last fetch = %q, want %q (the opaque cursor must be passed through)", f.lastCursor, "cur-2")
	}
	if f.lastAuth != "Bearer bt-key" {
		t.Errorf("the Authorization header = %q, want %q", f.lastAuth, "Bearer bt-key")
	}
}

// TestDedupeKeepsNewestAcrossPages pins the plan's P0-2 merge rule: the
// version-history walk re-exposes a row that already appeared, with an
// EARLIER _xact_id (events-dup-page1.json serves ev-dup-0001 at 200, page 2
// serves the same id at 150). One Case is yielded with the newest version's
// content, the duplicate is dropped, and nothing is fatal — a fatal would
// make every edited multi-page dataset unrunnable.
func TestDedupeKeepsNewestAcrossPages(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-dup-page1.json"),
			"cur-2": fixture(t, "events-dup-page2.json"),
		})
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var cases []*knov1.Case
	for c, err := range seq {
		if err != nil {
			t.Fatalf("a duplicate must be merged, not fatal: %v", err)
		}
		cases = append(cases, c)
	}
	if len(cases) != 3 {
		t.Fatalf("got %d cases, want 3 (the duplicate must be merged away)", len(cases))
	}

	byID := make(map[string]*knov1.Case, len(cases))
	for _, c := range cases {
		byID[c.GetId()] = c
	}
	dup := byID["5a4c5f0b-6b7c-4e5d-8e0a-4b6c0d5f8e7a"]
	if dup == nil {
		t.Fatal("ev-dup-0001 was dropped entirely; the merge must keep the newest occurrence")
	}
	if !strings.Contains(dup.GetExpected(), "NEW version") {
		t.Errorf("ev-dup-0001 Expected = %q, want the NEWEST version's content (the walk is newest-first)", dup.GetExpected())
	}

	seq, err = ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	coretest.EvalsDuplicateIDs(t, seq)
}

func TestFingerprintSensitivity(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	ev := f.newEvals(t)

	h1, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if len(h1) != sha256.Size*2 {
		t.Errorf("ContentHash = %q, want a sha256 hex digest", h1)
	}

	// The fingerprint reads the newest event's _xact_id with a fetch?limit=1
	// request — the fetch limit counts traces, so the response may exceed one
	// event, and the first event of the array is the one keyed on.
	f.mu.Lock()
	if f.lastLimit != "1" {
		t.Errorf("the fingerprint fetch limit = %q, want 1", f.lastLimit)
	}
	f.mu.Unlock()

	// Identical dataset must produce an identical fingerprint.
	h2, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h2 != h1 {
		t.Error("identical metadata must produce an identical fingerprint")
	}

	// A bumped newest _xact_id moves the fingerprint: the plan's P0-3 —
	// Braintrust has no dataset-level updatedAt, so the version counter is
	// the freshness signal.
	edited := strings.Replace(fixture(t, "events-page1.json"), `"_xact_id": "100"`, `"_xact_id": "101"`, 1)
	f2 := newFake(t, fixture(t, "dataset-list.json"), map[string]string{"": edited})
	h3, err := f2.newEvals(t).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h3 == h1 {
		t.Error("an edited dataset must move the fingerprint")
	}

	// A renamed dataset (resolved under its new name) moves the fingerprint.
	f3 := newFake(t, datasetListWith("renamed-llm"), map[string]string{"": fixture(t, "events-page1.json")})
	h4, err := f3.newEvals(t).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h4 == h1 {
		t.Error("a renamed dataset must move the fingerprint")
	}

	// A recreated dataset (a new id under the same name) moves it too.
	f4 := newFake(t,
		strings.Replace(fixture(t, "dataset-list.json"), "8f1c4b2e-5d6a-4f8e-9c2d-1a3b5c7d9e0f", "0f0e0d0c-0b0a-4f8e-9c2d-1a3b5c7d9e0f", 1),
		map[string]string{"": fixture(t, "events-page1.json")})
	h5, err := f4.newEvals(t).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h5 == h1 {
		t.Error("a recreated dataset must move the fingerprint")
	}

	// A changed SplitSeed re-splits the same eval set, so the fingerprint
	// must move with it — or a resumed run would restore the old division's
	// checkpoint under the new division's plan. Same for the fraction.
	f5 := newFake(t, fixture(t, "dataset-list.json"), map[string]string{"": fixture(t, "events-page1.json")})
	h6, err := f5.newEvals(t, func(o *braintrust.Options) { o.SplitSeed = "2026-08-29" }).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h6 == h1 {
		t.Error("a changed SplitSeed must move the fingerprint")
	}
	f6 := newFake(t, fixture(t, "dataset-list.json"), map[string]string{"": fixture(t, "events-page1.json")})
	h7, err := f6.newEvals(t, func(o *braintrust.Options) { o.HoldoutFrac = 0.5 }).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h7 == h1 {
		t.Error("a changed holdout fraction must move the fingerprint")
	}
}

func TestFingerprintEmptyDatasetIsRefused(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-empty.json")})
	ev := f.newEvals(t)

	_, err := ev.ContentHash(context.Background())
	if err == nil {
		t.Fatal("an empty dataset was fingerprinted")
	}
	for _, frag := range []string{"has no events", "empty"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}
}

func TestUnknownDatasetIsRefused(t *testing.T) {
	t.Parallel()
	// The filter endpoint answers a miss with an empty array, never a 404 —
	// so the resolution pass is what turns a typo into a refusal, before any
	// page is fetched.
	f := newFake(t, fixture(t, "dataset-list-miss.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a missing dataset was accepted")
	}
	for _, frag := range []string{"no dataset named", "fixture-dataset", f.srv.URL} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}

	// With an org selected, the refusal says where to look: the dataset may
	// simply live in another org.
	f2 := newFake(t, fixture(t, "dataset-list-miss.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	ev2 := f2.newEvals(t, func(o *braintrust.Options) { o.OrgName = "acme" })
	_, err = ev2.Cases(context.Background())
	if err == nil {
		t.Fatal("a missing dataset was accepted with an org set")
	}
	if !strings.Contains(err.Error(), braintrust.OrgNameEnv) {
		t.Errorf("error %q does not name %s", err, braintrust.OrgNameEnv)
	}
}

func TestMultiMatchIsRefused(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list-two.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("two datasets with the same name were accepted")
	}
	if !strings.Contains(err.Error(), "2 datasets match") {
		t.Errorf("error %q does not name the multi-match", err)
	}
}

func TestFilterIgnoredIsRefused(t *testing.T) {
	t.Parallel()
	// A single dataset with a different name: a server that ignored the
	// dataset_name filter. Refused loudly — the fingerprint must not depend
	// on what the server decided to return.
	f := newFake(t, fixture(t, "dataset-list-wrong-name.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	// The fixture carries a DIFFERENT name than the one the eval asks for;
	// newEvals would otherwise derive the queried name from the fixture and
	// make the test self-consistent. Pin the queried name explicitly.
	ev := f.newEvals(t, func(o *braintrust.Options) { o.Dataset = "fixture-dataset" })

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a filter-ignoring server was accepted")
	}
	for _, frag := range []string{"some-other-dataset", "fixture-dataset"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}
}

func TestDialTimeRecheckRefusesLocalhost(t *testing.T) {
	t.Parallel()
	// The config-time check lets "localhost" through — it reads like an
	// ordinary name — and the dial-time recheck is what catches it resolving
	// to loopback. AllowPrivateAddress is deliberately NOT set: the refusal
	// must come from the resolved address, not from the URL.
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	endpoint := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
	ev, err := braintrust.New(braintrust.Options{
		Dataset:              "support-llm",
		Host:                 endpoint,
		APIKey:               "bt-key",
		AllowInsecureBaseURL: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = ev.CountSplits(context.Background())
	if err == nil {
		t.Fatal("http://localhost must be refused at dial time")
	}
	if !strings.Contains(err.Error(), "private address") {
		t.Errorf("error %q does not name the refusal", err)
	}
}

func TestUnauthorizedDoesNotEchoTheKeys(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	f.unauthorized = true
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not name the status", err)
	}
	for _, frag := range []string{"bt-key", "Bearer "} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("error %q echoes the credential (%q)", err, frag)
		}
	}
}

func Test429IsRetriedWithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	f.retryOnce = true
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchReq != 3 {
		t.Errorf("the fetch API saw %d requests, want 3 (one 429, then page 1 and page 2)", f.fetchReq)
	}
}

func Test429PersistsIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-page1.json")})
	f.always429 = true
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("a permanently throttled API was treated as success")
	}
	if !strings.Contains(gotErr.Error(), "429") {
		t.Errorf("error %q does not name 429", gotErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchReq != 3 {
		// Deliberately pins the retry budget (maxAttempts): the test fails
		// loudly if the budget ever changes, so the behavior is a reviewed
		// decision, not a drift.
		t.Errorf("the fetch API saw %d requests, want 3 (the retry budget)", f.fetchReq)
	}
}

func TestRepeatingCursorIsFatal(t *testing.T) {
	t.Parallel()
	// A server that answers every page with the same continuation cursor:
	// caught by the seen-cursor guard before the request is even repeated.
	page := fixture(t, "events-page1.json")
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": page, "cur-2": page})
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("a repeating cursor was accepted")
	}
	if !strings.Contains(gotErr.Error(), "again") {
		t.Errorf("error %q does not name the repeating cursor", gotErr)
	}
}

func TestNullInputFixtureIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-null-input.json")})
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("a null input was accepted")
	}
	for _, frag := range []string{"8d7f8c3e-9eaf-4b8a-9b3d-7e9f3a8c1b0d", "null input"} {
		if !strings.Contains(gotErr.Error(), frag) {
			t.Errorf("error %q does not mention %q", gotErr, frag)
		}
	}
}

func TestNullExpectedFixtureNamesTheDecision(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-null-expected.json")})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	if cases[0].GetExpected() != "" {
		t.Errorf("Expected = %q, want empty", cases[0].GetExpected())
	}
	if !strings.Contains(cases[0].GetProvenance().GetDerivationNote(), "expected is null") {
		t.Errorf("DerivationNote = %q, want it to name the null expected", cases[0].GetProvenance().GetDerivationNote())
	}
}

func TestCanonicalJSONGolden(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", "canonical.json"))
	if err != nil {
		t.Fatalf("reading the golden file: %v", err)
	}
	var golden map[string]struct {
		Input    string `json:"input"`
		Expected string `json:"expected"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatalf("parsing the golden file: %v", err)
	}

	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	ev := f.newEvals(t)
	for _, c := range collectCases(t, ev) {
		g, ok := golden[c.GetId()]
		if !ok {
			t.Fatalf("the golden file has no entry for %s", c.GetId())
		}
		if c.GetInput() != g.Input {
			t.Errorf("%s: Input = %q, golden %q", c.GetId(), c.GetInput(), g.Input)
		}
		if c.GetExpected() != g.Expected {
			t.Errorf("%s: Expected = %q, golden %q", c.GetId(), c.GetExpected(), g.Expected)
		}
	}
}

func TestDerivedMarkingRoundTrips(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-origin.json")})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}
	byID := make(map[string]*knov1.Case, len(cases))
	for _, c := range cases {
		byID[c.GetId()] = c
	}
	for _, id := range []string{
		"9e8a9d4f-afb1-4c9b-8c4e-8f0a4b9d2c1e", // experiment
		"a0f9be5a-b0c2-4d0c-9d5f-9a1b5c0e3d2f", // span
		"b1a0cf6b-c1d3-4e1d-8e6a-ab2c6d1f4e3a", // eval_result
	} {
		if !byID[id].GetProvenance().GetDerived() {
			t.Errorf("%s (origin set) must be marked derived", id)
		}
	}
	if byID["c2b1d07c-d2e4-4f2e-9f7b-bc3d7e2a5f4b"].GetProvenance().GetDerived() {
		t.Error("the authored event (no origin) must not be marked derived")
	}
	if !strings.Contains(byID["a0f9be5a-b0c2-4d0c-9d5f-9a1b5c0e3d2f"].GetProvenance().GetDerivationNote(), "span-9") {
		t.Errorf("the span note = %q, want it to name span-9", byID["a0f9be5a-b0c2-4d0c-9d5f-9a1b5c0e3d2f"].GetProvenance().GetDerivationNote())
	}

	// The weak-label count records the derived half, jsonl-identical.
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.WeakLabelCases != 3 {
		t.Errorf("WeakLabelCases = %d, want 3", counts.WeakLabelCases)
	}
}

func TestEmptyDatasetYieldsNoCases(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-list.json"),
		map[string]string{"": fixture(t, "events-empty.json")})
	ev := f.newEvals(t)

	if cases := collectCases(t, ev); len(cases) != 0 {
		t.Fatalf("got %d cases, want 0", len(cases))
	}
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("Total() = %d, want 0", counts.Total())
	}
	if err := counts.Validate(); err == nil {
		t.Error("an empty eval set must not validate")
	}
}

func TestRowSizeCapNamesTheEvent(t *testing.T) {
	t.Parallel()
	big := fmt.Sprintf(`{"id":"it-big","input":{"question":%q},"expected":{"answer":"x"},"_xact_id":"1"}`,
		strings.Repeat("q", 5<<20))
	f := newFake(t, datasetListWith("fixture-dataset"), map[string]string{"": pageWith(big)})
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("an oversized row was accepted")
	}
	for _, frag := range []string{"it-big", "row cap"} {
		if !strings.Contains(gotErr.Error(), frag) {
			t.Errorf("error %q does not mention %q", gotErr, frag)
		}
	}
}

func TestSplitIsStableAcrossRuns(t *testing.T) {
	t.Parallel()
	// 120 rows: enough that both halves are non-empty at the default
	// fraction, and stable across two full passes.
	rows := make([]string, 0, 120)
	for i := range 120 {
		rows = append(rows, eventWith(fmt.Sprintf("it-%03d", i), fmt.Sprintf("%d", i+1),
			fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetListWith("fixture-dataset"), map[string]string{"": pageWith(rows...)})
	ev := f.newEvals(t)

	seq1, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	first := collectSplits(t, seq1)
	seq2, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	second := collectSplits(t, seq2)
	if !splitsEqual(first, second) {
		t.Error("the split changed between two reads of the same dataset")
	}

	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 120 {
		t.Errorf("Total() = %d, want 120", counts.Total())
	}
	if counts.Dev == 0 || counts.Holdout == 0 {
		t.Errorf("expected both halves, got dev=%d holdout=%d", counts.Dev, counts.Holdout)
	}
}

// newLangSmithFake serves the two langsmith-shaped endpoints a cross-adapter
// split test needs: a one-dataset /datasets envelope and cursor-keyed
// /examples pages.
func newLangSmithFake(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()
	datasets := `{"items":[{"id":"ds-1","name":"fixture-dataset","modified_at":"2026-08-01T12:00:00Z","example_count":6}],"next_cursor":""}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/datasets":
			_, _ = io.WriteString(w, datasets)
		case "/examples":
			_, _ = io.WriteString(w, pages[r.URL.Query().Get("cursor")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSameSplitAsJSONLAndLangsmithAndLangfuse asserts the denominator math
// cannot vary by source: the same Case ids land in the same halves whether
// they come from a JSONL file, from LangSmith, from Langfuse, or from
// Braintrust.
func TestSameSplitAsJSONLAndLangsmithAndLangfuse(t *testing.T) {
	t.Parallel()
	ids := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	braintrustRows := make([]string, 0, len(ids))
	langfuseRows := make([]string, 0, len(ids))
	langsmithRows := make([]string, 0, len(ids))
	for i, id := range ids {
		braintrustRows = append(braintrustRows, eventWith(id, fmt.Sprintf("%d", i+1),
			fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
		langfuseRows = append(langfuseRows, fmt.Sprintf(`{"id":%q,"input":{"question":%q},"expectedOutput":{"answer":"a"},"status":"ACTIVE"}`, id, fmt.Sprintf("q%d", i)))
		langsmithRows = append(langsmithRows, fmt.Sprintf(`{"id":%q,"inputs":{"question":%q},"outputs":{"answer":"a"}}`, id, fmt.Sprintf("q%d", i)))
	}

	f := newFake(t, datasetListWith("fixture-dataset"), map[string]string{"": pageWith(braintrustRows...)})
	ev := f.newEvals(t)
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	fromBraintrust := collectSplits(t, seq)

	lsSrv := newLangSmithFake(t, map[string]string{"": `{"items":[` + strings.Join(langsmithRows, ",") + `],"next_cursor":""}`})
	lse, err := langsmith.New(langsmith.Options{
		Dataset:              "fixture-dataset",
		Endpoint:             lsSrv.URL,
		APIKey:               "test-key",
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	})
	if err != nil {
		t.Fatalf("langsmith.New: %v", err)
	}
	seq, err = lse.Cases(context.Background())
	if err != nil {
		t.Fatalf("langsmith Cases: %v", err)
	}
	fromLangsmith := collectSplits(t, seq)

	lfSrv := newLangfuseFake(t, fmt.Sprintf(
		`{"data":[%s],"meta":{"page":1,"limit":100,"totalItems":6,"totalPages":1}}`,
		strings.Join(langfuseRows, ","),
	))
	lfe, err := langfuse.New(langfuse.Options{
		Dataset:              "fixture-dataset",
		Host:                 lfSrv.URL,
		PublicKey:            "pk",
		SecretKey:            "sk",
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	})
	if err != nil {
		t.Fatalf("langfuse.New: %v", err)
	}
	seq, err = lfe.Cases(context.Background())
	if err != nil {
		t.Fatalf("langfuse Cases: %v", err)
	}
	fromLangfuse := collectSplits(t, seq)

	path := filepath.Join(t.TempDir(), "cases.jsonl")
	var sb strings.Builder
	for i, id := range ids {
		fmt.Fprintf(&sb, `{"id":%q,"input":%q,"expected":%q}`+"\n", id, fmt.Sprintf("q%d", i), "a")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("writing the jsonl fixture: %v", err)
	}
	jl, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("jsonl.New: %v", err)
	}
	seq, err = jl.Cases(context.Background())
	if err != nil {
		t.Fatalf("jsonl Cases: %v", err)
	}
	fromJSONL := collectSplits(t, seq)

	if !splitsEqual(fromBraintrust, fromJSONL) {
		t.Errorf("splits differ by source:\nbraintrust: %v\njsonl: %v", fromBraintrust, fromJSONL)
	}
	if !splitsEqual(fromBraintrust, fromLangsmith) {
		t.Errorf("splits differ by source:\nbraintrust: %v\nlangsmith: %v", fromBraintrust, fromLangsmith)
	}
	if !splitsEqual(fromBraintrust, fromLangfuse) {
		t.Errorf("splits differ by source:\nbraintrust: %v\nlangfuse: %v", fromBraintrust, fromLangfuse)
	}
}

// newLangfuseFake serves the two langfuse-shaped endpoints a cross-adapter
// split test needs: a one-dataset v2 object and a {data, meta} items page.
func newLangfuseFake(t *testing.T, page1 string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/public/v2/datasets/"):
			_, _ = io.WriteString(w, `{"id":"ds-1","name":"fixture-dataset","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-08-01T12:00:00Z"}`)
		case r.URL.Path == "/api/public/dataset-items":
			_, _ = io.WriteString(w, page1)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// splitsEqual compares two id -> split maps element-wise. The split enum is
// an int32, so a plain loop is exact and keeps the linters' proto-compare
// rule out of the picture.
func splitsEqual(a, b map[string]knov1.Split) bool {
	if len(a) != len(b) {
		return false
	}
	for id, split := range a {
		if b[id] != split {
			return false
		}
	}
	return true
}

func TestCountSplitsRecordsTheConfiguredFraction(t *testing.T) {
	t.Parallel()
	rows := make([]string, 0, 20)
	for i := range 20 {
		rows = append(rows, eventWith(fmt.Sprintf("it-%02d", i), fmt.Sprintf("%d", i+1),
			fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetListWith("fixture-dataset"), map[string]string{"": pageWith(rows...)})
	ev := f.newEvals(t, func(o *braintrust.Options) { o.HoldoutFrac = 0.5 })

	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.HoldoutFrac != 0.5 {
		t.Errorf("HoldoutFrac = %v, want 0.5", counts.HoldoutFrac)
	}
	if counts.Total() != 20 {
		t.Errorf("Total() = %d, want 20", counts.Total())
	}
	if counts.Dev == 0 || counts.Holdout == 0 {
		t.Errorf("expected both halves at 0.5, got dev=%d holdout=%d", counts.Dev, counts.Holdout)
	}
}

func TestNewRefusesUnsafeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts braintrust.Options
		want string // substring of the refusal; empty means New must succeed
	}{
		{name: "no dataset", opts: braintrust.Options{APIKey: "bt-key"}, want: "no dataset name"},
		{name: "plain http refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "http://example.com"}, want: "plain HTTP"},
		{name: "plain http allowed by flag", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "http://example.com", AllowInsecureBaseURL: true}},
		{name: "loopback refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://127.0.0.1:8080"}, want: "private address"},
		{name: "loopback allowed by flag", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://127.0.0.1:8080", AllowPrivateAddress: true}},
		{name: "ipv6 loopback refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://[::1]:8080"}, want: "private address"},
		{name: "link-local refused with no override", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://169.254.169.254", AllowInsecureBaseURL: true, AllowPrivateAddress: true}, want: "link-local"},
		{name: "non-canonical address refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://2130706433"}, want: "neither a hostname nor a canonical IP"},
		{name: "userinfo refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://user:pass@example.com"}, want: "userinfo"},
		{name: "bad scheme refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "ftp://example.com"}, want: "not http or https"},
		{name: "no host refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://"}, want: "no host"},
		{name: "hosted endpoint accepted", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://api.braintrust.dev"}},
		{name: "hosted endpoint with trailing slash", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://api.braintrust.dev/"}},
		{name: "negative holdout frac refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://example.com", HoldoutFrac: -0.1}, want: "holdout fraction"},
		{name: "holdout frac at one refused", opts: braintrust.Options{Dataset: "d", APIKey: "bt-key", Host: "https://example.com", HoldoutFrac: 1}, want: "holdout fraction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := braintrust.New(tt.opts)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New accepted an input that must be refused (%s)", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestNewRequiresKeyFromTheEnvironment is serial: t.Setenv mutates process
// state and cannot run beside parallel tests.
func TestNewRequiresKeyFromTheEnvironment(t *testing.T) {
	t.Setenv(braintrust.KeyEnv, "")
	_, err := braintrust.New(braintrust.Options{Dataset: "d", Host: "https://example.com"})
	if err == nil {
		t.Fatal("a missing key was accepted")
	}
	if !strings.Contains(err.Error(), braintrust.KeyEnv) {
		t.Errorf("error %q does not name %s", err, braintrust.KeyEnv)
	}

	// An explicit key is honored even with the environment empty.
	if _, err := braintrust.New(braintrust.Options{Dataset: "d", Host: "https://example.com", APIKey: "bt-key"}); err != nil {
		t.Fatalf("New with an explicit key: %v", err)
	}
}

// TestHostComesFromTheEnvironment is serial for the same reason.
func TestHostComesFromTheEnvironment(t *testing.T) {
	t.Setenv(braintrust.KeyEnv, "bt-key")

	t.Setenv(braintrust.HostEnv, "http://example.com")
	if _, err := braintrust.New(braintrust.Options{Dataset: "d"}); err == nil {
		t.Fatal("a plain-http env host was accepted")
	}

	t.Setenv(braintrust.HostEnv, "https://example.com")
	if _, err := braintrust.New(braintrust.Options{Dataset: "d"}); err != nil {
		t.Fatalf("New with an https env host: %v", err)
	}
}

// TestOrgNameFlowsAsAQueryParameter: the org selection is a query
// parameter on both endpoints, never a header.
func TestOrgNameFlowsAsAQueryParameter(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-list.json"),
		map[string]string{
			"":      fixture(t, "events-page1.json"),
			"cur-2": fixture(t, "events-page2.json"),
		})
	ev := f.newEvals(t, func(o *braintrust.Options) { o.OrgName = "acme" })

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastOrg != "acme" {
		t.Errorf("the org_name query = %q, want acme", f.lastOrg)
	}
}
