package langfuse_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// fakeLangfuse is a minimal stand-in for the Langfuse REST API, serving the
// fixtures in the shapes the real one uses: a bare v2 dataset object at
// /api/public/v2/datasets/{name} and a {data, meta} envelope at
// /api/public/dataset-items, page-numbered.
type fakeLangfuse struct {
	dataset string            // raw v2 dataset object
	pages   map[string]string // page number string -> raw items envelope

	// notFound 404s every datasets request, as the real API does for an
	// unknown dataset name.
	notFound bool
	// unauthorized 401s every datasets request.
	unauthorized bool
	// retryOnce 429s the first dataset-items request, then serves normally.
	retryOnce bool
	// always429 429s every dataset-items request.
	always429 bool

	mu          sync.Mutex
	srv         *httptest.Server
	datasetsReq int // /api/public/v2/datasets requests seen
	itemsReq    int // /api/public/dataset-items requests seen
	lastAuth    string
	lastDataset string // datasetName query of the most recent items request
	lastLimit   string // limit query of the most recent items request
}

func newFake(t *testing.T, dataset string, pages map[string]string) *fakeLangfuse {
	t.Helper()
	f := &fakeLangfuse{dataset: dataset, pages: pages}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLangfuse) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/api/public/v2/datasets/"):
		f.mu.Lock()
		f.datasetsReq++
		f.mu.Unlock()
		if f.notFound {
			http.NotFound(w, r)
			return
		}
		if f.unauthorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, f.dataset)
	case r.URL.Path == "/api/public/dataset-items":
		f.mu.Lock()
		f.itemsReq++
		first := f.itemsReq == 1
		f.lastDataset = r.URL.Query().Get("datasetName")
		f.lastLimit = r.URL.Query().Get("limit")
		f.mu.Unlock()
		if f.always429 || (f.retryOnce && first) {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		page := r.URL.Query().Get("page")
		f.mu.Lock()
		body, ok := f.pages[page]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "unknown page", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

// datasetNameOf extracts the name of the dataset in a raw v2 dataset object,
// so an Evals built against the fake requests the name the fixture actually
// carries — the adapter resolves by exact name, and the fake serves whatever
// dataset object it was configured with.
func datasetNameOf(raw string) string {
	var d struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return ""
	}
	return d.Name
}

// newEvals builds an Evals against the fake server, opted into the insecure
// and private settings every fixture endpoint (127.0.0.1, plain http)
// requires. Keys are explicit so no test depends on the environment.
func (f *fakeLangfuse) newEvals(t *testing.T, opts ...func(*langfuse.Options)) *langfuse.Evals {
	t.Helper()
	dataset := "fixture-dataset"
	if name := datasetNameOf(f.dataset); name != "" {
		dataset = name
	}
	o := langfuse.Options{
		Dataset:              dataset,
		Host:                 f.srv.URL,
		PublicKey:            "pk",
		SecretKey:            "sk",
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	}
	for _, fn := range opts {
		fn(&o)
	}
	ev, err := langfuse.New(o)
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
func collectCases(t *testing.T, ev *langfuse.Evals) []*knov1.Case {
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

// datasetWith builds a v2 dataset object.
func datasetWith(name string) string {
	return fmt.Sprintf(`{"id":"ds-1","name":%q,"createdAt":"2026-01-01T00:00:00Z",`+
		`"updatedAt":"2026-08-01T12:00:00Z"}`, name)
}

// pageWith builds an items envelope from raw rows, as one page of a
// one-page dataset.
func pageWith(items ...string) string {
	return fmt.Sprintf(`{"data":[%s],"meta":{"page":1,"limit":100,"totalItems":%d,"totalPages":1}}`,
		strings.Join(items, ","), len(items))
}

// pages builds a multi-page envelope set from a list of per-page rows.
func pages(rowsByPage ...[]string) map[string]string {
	out := make(map[string]string, len(rowsByPage))
	for i, rows := range rowsByPage {
		out[fmt.Sprintf("%d", i+1)] = fmt.Sprintf(
			`{"data":[%s],"meta":{"page":%d,"limit":100,"totalItems":%d,"totalPages":%d}}`,
			strings.Join(rows, ","), i+1, 0, len(rowsByPage),
		)
	}
	return out
}

// item builds one dataset item row with ACTIVE status; extra fields (raw
// "key":value pairs) land inside the row object before the closing brace.
func item(id, input, expected string, extra ...string) string {
	s := fmt.Sprintf(`{"id":%q,"input":%s,"expectedOutput":%s,"status":"ACTIVE"`, id, input, expected)
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	f := newFake(t,
		fixture(t, "dataset-support-llm.json"),
		map[string]string{
			"1": fixture(t, "items-llm-page1.json"),
			"2": fixture(t, "items-llm-page2.json"),
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
			[]byte(fixture(t, "dataset-support-llm.json")), // request 0: the dataset object
			[]byte(fixture(t, "items-llm-page1.json")),     // requests 1+: pages
		},
	}
	ev, err := langfuse.New(langfuse.Options{
		Dataset:   "support-llm",
		Host:      "https://example.com", // never dialed; the probe intercepts
		PublicKey: "pk",
		SecretKey: "sk",
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

	// Request 0 (the dataset object) is closed by resolveDataset; request 1
	// (the open items page) must have been closed by the iterator's own
	// deferred cleanup, which is what an early break runs.
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.probes) < 2 {
		t.Fatalf("expected at least 2 requests, saw %d", len(transport.probes))
	}
	transport.probes[0].AssertClosed(t)
	transport.probes[1].AssertClosed(t)
}

func TestItemMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		row          string
		wantInput    string
		wantExpected string
		wantNote     string // substring of the provenance derivation note
		wantDerived  bool
		wantSkipped  bool   // the row is filtered (ARCHIVED), not yielded
		wantErr      string // substring of the fatal error
	}{
		{
			name:         "question and answer, canonicalized",
			row:          item("it-1", `{"question":"q1"}`, `{"answer":"a1"}`),
			wantInput:    `{"question":"q1"}`,
			wantExpected: `{"answer":"a1"}`,
		},
		{
			name:         "object keys sorted, regardless of document order",
			row:          item("it-2", `{"z":1,"a":{"y":2,"b":3}}`, `{"z":"x","a":"y"}`),
			wantInput:    `{"a":{"b":3,"y":2},"z":1}`,
			wantExpected: `{"a":"y","z":"x"}`,
		},
		{
			name:         "large integers survive the canonical pass",
			row:          item("it-2b", `{"n":12345678901234567890}`, `{"answer":"a"}`),
			wantInput:    `{"n":12345678901234567890}`,
			wantExpected: `{"answer":"a"}`,
		},
		{
			name:         "message array kept as canonical JSON, named",
			row:          item("it-3", `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]`, `{"answer":"hello"}`),
			wantInput:    `[{"content":"hi","role":"user"},{"content":"hello","role":"assistant"}]`,
			wantExpected: `{"answer":"hello"}`,
			wantNote:     "message array",
		},
		{
			name:         "null expectedOutput is an empty expected, named",
			row:          item("it-4", `{"question":"q4"}`, "null"),
			wantInput:    `{"question":"q4"}`,
			wantExpected: "",
			wantNote:     "expectedOutput is null",
		},
		{
			name:         "absent expectedOutput is an empty expected, named",
			row:          `{"id":"it-4b","input":{"question":"q4b"},"status":"ACTIVE"}`,
			wantInput:    `{"question":"q4b"}`,
			wantExpected: "",
			wantNote:     "expectedOutput is null",
		},
		{
			name:         "harvested from an observation is derived, named",
			row:          item("it-5", `{"question":"q5"}`, `{"answer":"a5"}`, `"sourceObservationId":"obs-5"`),
			wantInput:    `{"question":"q5"}`,
			wantExpected: `{"answer":"a5"}`,
			wantDerived:  true,
			wantNote:     "sourceObservationId obs-5",
		},
		{
			name:         "harvested from a trace is derived, named",
			row:          item("it-6", `{"question":"q6"}`, `{"answer":"a6"}`, `"sourceTraceId":"trace-6"`),
			wantInput:    `{"question":"q6"}`,
			wantExpected: `{"answer":"a6"}`,
			wantDerived:  true,
			wantNote:     "sourceTraceId trace-6",
		},
		{
			name:         "harvested from both names both",
			row:          item("it-7", `{"question":"q7"}`, `{"answer":"a7"}`, `"sourceObservationId":"obs-7"`, `"sourceTraceId":"trace-7"`),
			wantInput:    `{"question":"q7"}`,
			wantExpected: `{"answer":"a7"}`,
			wantDerived:  true,
			wantNote:     "sourceObservationId obs-7, sourceTraceId trace-7",
		},
		{
			name:         "authored item stays underived",
			row:          item("it-8", `{"question":"q8"}`, `{"answer":"a8"}`),
			wantInput:    `{"question":"q8"}`,
			wantExpected: `{"answer":"a8"}`,
		},
		{
			name:         "absent status is not archived",
			row:          `{"id":"it-9","input":{"question":"q9"},"expectedOutput":{"answer":"a9"}}`,
			wantInput:    `{"question":"q9"}`,
			wantExpected: `{"answer":"a9"}`,
		},
		{
			name:        "archived status is filtered, not yielded",
			row:         `{"id":"it-10","input":{"question":"q10"},"expectedOutput":{"answer":"a10"},"status":"ARCHIVED"}`,
			wantSkipped: true,
		},
		{
			name:    "null input is fatal",
			row:     item("it-11", "null", `{"answer":"a11"}`),
			wantErr: "null input",
		},
		{
			name:    "a row with a malformed input value is fatal at the row",
			row:     `{"id":"it-12","input":{"unclosed":` + `,"expectedOutput":{"answer":"a"},"status":"ACTIVE"}`,
			wantErr: "the item row is not a JSON object",
		},
		{
			name:    "a row with a malformed expectedOutput value is fatal at the row",
			row:     `{"id":"it-13","input":{"q":"x"},"expectedOutput":{"unclosed":` + `,"status":"ACTIVE"}`,
			wantErr: "the item row is not a JSON object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": pageWith(tt.row)})
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
			if tt.wantSkipped {
				if got != nil {
					t.Fatalf("an ARCHIVED item was yielded: %v", got)
				}
				return
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
			if p.GetSource() != "langfuse" {
				t.Errorf("Source = %q, want langfuse", p.GetSource())
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
		fixture(t, "dataset-support-llm.json"),
		map[string]string{
			"1": fixture(t, "items-llm-page1.json"),
			"2": fixture(t, "items-llm-page2.json"),
		})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	want := []string{"it-llm-0001", "it-llm-0002", "it-llm-0003", "it-llm-0004"}
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
	if f.lastDataset != "support-llm" {
		t.Errorf("the datasetName query = %q, want %q", f.lastDataset, "support-llm")
	}
	if f.lastLimit != "100" {
		t.Errorf("the limit query = %q, want 100", f.lastLimit)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk:sk"))
	if f.lastAuth != wantAuth {
		t.Errorf("the Authorization header = %q, want %q (public key as user, secret key as password)", f.lastAuth, wantAuth)
	}
}

func TestTornPageDuplicateIsFatal(t *testing.T) {
	t.Parallel()
	// The same item id on both sides of a page seam: a dataset edited
	// mid-pagination. Fatal, naming the id — the split is keyed on it, so two
	// rows sharing one would land in the same half, indistinguishable in every
	// later report.
	rows := []string{
		item("it-dup-0001", `{"question":"first read"}`, `{"answer":"a"}`),
		item("it-dup-0002", `{"question":"second row"}`, `{"answer":"a"}`),
	}
	f := newFake(t, datasetWith("fixture-dataset"), pages(rows[:1], rows))
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
		t.Fatal("a duplicate item id across a page seam was accepted")
	}
	for _, frag := range []string{"duplicate item id", "it-dup-0001"} {
		if !strings.Contains(gotErr.Error(), frag) {
			t.Errorf("error %q does not mention %q", gotErr, frag)
		}
	}
}

func TestItemWithoutIdIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("fixture-dataset"),
		map[string]string{"1": pageWith(`{"input":{"question":"q"},"expectedOutput":{"answer":"a"},"status":"ACTIVE"}`)})
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
		t.Fatal("an item without an id was accepted")
	}
	if !strings.Contains(gotErr.Error(), "item has no id") {
		t.Errorf("error %q does not name the missing id", gotErr)
	}
}

func TestArchiveExclusion(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("fixture-dataset"),
		map[string]string{"1": fixture(t, "items-archived.json")})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1 (the ARCHIVED row must be filtered out)", len(cases))
	}
	if cases[0].GetId() != "it-act-0002" {
		t.Errorf("surviving case = %q, want it-act-0002", cases[0].GetId())
	}
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 1 {
		t.Errorf("Total() = %d, want 1", counts.Total())
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

	check := func(ev *langfuse.Evals) {
		t.Helper()
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

	f := newFake(t, fixture(t, "dataset-support-llm.json"),
		map[string]string{
			"1": fixture(t, "items-llm-page1.json"),
			"2": fixture(t, "items-llm-page2.json"),
		})
	check(f.newEvals(t))

	f2 := newFake(t, datasetWith("msg-dataset"), map[string]string{"1": fixture(t, "items-message-array.json")})
	check(f2.newEvals(t))
}

func TestDerivedMarkingRoundTrips(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("fixture-dataset"),
		map[string]string{"1": fixture(t, "items-derived.json")})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 3 {
		t.Fatalf("got %d cases, want 3", len(cases))
	}
	byID := make(map[string]*knov1.Case, len(cases))
	for _, c := range cases {
		byID[c.GetId()] = c
	}
	if !byID["it-der-0001"].GetProvenance().GetDerived() {
		t.Error("it-der-0001 (sourceObservationId set) must be marked derived")
	}
	if !byID["it-der-0002"].GetProvenance().GetDerived() {
		t.Error("it-der-0002 (sourceTraceId set) must be marked derived")
	}
	if byID["it-auth-0003"].GetProvenance().GetDerived() {
		t.Error("it-auth-0003 (no source id) must not be marked derived")
	}
	if !strings.Contains(byID["it-der-0001"].GetProvenance().GetDerivationNote(), "obs-123") {
		t.Errorf("it-der-0001 note = %q, want it to name obs-123", byID["it-der-0001"].GetProvenance().GetDerivationNote())
	}

	// The weak-label count records the derived half, jsonl-identical.
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.WeakLabelCases != 2 {
		t.Errorf("WeakLabelCases = %d, want 2", counts.WeakLabelCases)
	}
}

func TestNullInputFixtureIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("fixture-dataset"),
		map[string]string{"1": fixture(t, "items-null-input.json")})
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
	for _, frag := range []string{"it-null-0001", "null input"} {
		if !strings.Contains(gotErr.Error(), frag) {
			t.Errorf("error %q does not mention %q", gotErr, frag)
		}
	}
}

func TestMessageArrayPrompt(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("msg-dataset"),
		map[string]string{"1": fixture(t, "items-message-array.json")})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	c := cases[0]
	if c.GetInput() != `[{"content":"hi","role":"user"},{"content":"hello","role":"assistant"}]` {
		t.Errorf("Input = %q, want the canonical message array", c.GetInput())
	}
	if !strings.Contains(c.GetProvenance().GetDerivationNote(), "message array") {
		t.Errorf("DerivationNote = %q, want it to name the message-array decision", c.GetProvenance().GetDerivationNote())
	}
}

func TestEmptyDatasetYieldsNoCases(t *testing.T) {
	t.Parallel()
	f := newFake(t, datasetWith("fixture-dataset"),
		map[string]string{"1": fixture(t, "items-empty.json")})
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

func TestUnknownDatasetIsRefused(t *testing.T) {
	t.Parallel()
	// The real API 404s /api/public/v2/datasets/{name} for a typo'd name; the
	// items endpoint would answer 200 with an empty data array, so the
	// resolution pass is what turns a typo into a refusal.
	f := newFake(t, ``, map[string]string{"1": fixture(t, "items-llm-page1.json")})
	f.notFound = true
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a missing dataset was accepted")
	}
	for _, frag := range []string{"no dataset named", "fixture-dataset"} {
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
	f := newFake(t, fixture(t, "dataset-support-llm.json"),
		map[string]string{"1": fixture(t, "items-llm-page1.json")})
	endpoint := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
	ev, err := langfuse.New(langfuse.Options{
		Dataset:              "support-llm",
		Host:                 endpoint,
		PublicKey:            "pk",
		SecretKey:            "sk",
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
	f := newFake(t, fixture(t, "dataset-support-llm.json"),
		map[string]string{"1": fixture(t, "items-llm-page1.json")})
	f.unauthorized = true
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not name the status", err)
	}
	for _, frag := range []string{"pk", "sk", "Basic "} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("error %q echoes the credential (%q)", err, frag)
		}
	}
}

func Test429IsRetriedWithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-support-llm.json"),
		map[string]string{
			"1": fixture(t, "items-llm-page1.json"),
			"2": fixture(t, "items-llm-page2.json"),
		})
	f.retryOnce = true
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.itemsReq != 3 {
		t.Errorf("the items API saw %d requests, want 3 (one 429, then page 1 and page 2)", f.itemsReq)
	}
}

func Test429PersistsIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "dataset-support-llm.json"),
		map[string]string{"1": fixture(t, "items-llm-page1.json")})
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
	if f.itemsReq != 3 {
		// Deliberately pins the retry budget (maxAttempts): the test fails
		// loudly if the budget ever changes, so the behavior is a reviewed
		// decision, not a drift.
		t.Errorf("the items API saw %d requests, want 3 (the retry budget)", f.itemsReq)
	}
}

func TestMissingMetaIsFatal(t *testing.T) {
	t.Parallel()
	body := `{"data":[` + item("it-1", `{"question":"q"}`, `{"answer":"a"}`) + `]}`
	f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": body})
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
		t.Fatal("a page without meta was accepted")
	}
	if !strings.Contains(gotErr.Error(), "no meta object") {
		t.Errorf("error %q does not name the missing meta", gotErr)
	}
}

func TestRowSizeCapNamesTheItem(t *testing.T) {
	t.Parallel()
	big := fmt.Sprintf(`{"id":"it-big","input":{"question":%q},"expectedOutput":{"answer":"x"},"status":"ACTIVE"}`,
		strings.Repeat("q", 5<<20))
	f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": pageWith(big)})
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
		rows = append(rows, item(fmt.Sprintf("it-%03d", i), fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": pageWith(rows...)})
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

// TestSameSplitAsJSONLAndLangsmith asserts the denominator math cannot vary
// by source: the same Case ids land in the same halves whether they come from
// a JSONL file, from LangSmith, or from Langfuse.
func TestSameSplitAsJSONLAndLangsmith(t *testing.T) {
	t.Parallel()
	ids := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	langfuseRows := make([]string, 0, len(ids))
	langsmithRows := make([]string, 0, len(ids))
	for i, id := range ids {
		langfuseRows = append(langfuseRows, item(id, fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
		langsmithRows = append(langsmithRows, fmt.Sprintf(`{"id":%q,"inputs":{"question":%q},"outputs":{"answer":"a"}}`, id, fmt.Sprintf("q%d", i)))
	}

	f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": pageWith(langfuseRows...)})
	ev := f.newEvals(t)
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	fromLangfuse := collectSplits(t, seq)

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

	if !splitsEqual(fromLangfuse, fromJSONL) {
		t.Errorf("splits differ by source:\nlangfuse: %v\njsonl: %v", fromLangfuse, fromJSONL)
	}
	if !splitsEqual(fromLangfuse, fromLangsmith) {
		t.Errorf("splits differ by source:\nlangfuse: %v\nlangsmith: %v", fromLangfuse, fromLangsmith)
	}
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

func TestContentHashFoldsDatasetAndSplit(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "dataset-support-llm.json"),
		map[string]string{"1": fixture(t, "items-llm-page1.json")})
	ev := f.newEvals(t)

	h1, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if len(h1) != sha256.Size*2 {
		t.Errorf("ContentHash = %q, want a sha256 hex digest", h1)
	}

	// Identical metadata must produce an identical fingerprint.
	h2, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h2 != h1 {
		t.Error("identical metadata must produce an identical fingerprint")
	}

	// A changed updatedAt moves the fingerprint.
	edited := strings.Replace(fixture(t, "dataset-support-llm.json"), "2026-08-01T12:00:00Z", "2026-08-02T12:00:00Z", 1)
	f2 := newFake(t, edited, map[string]string{"1": fixture(t, "items-llm-page1.json")})
	h3, err := f2.newEvals(t).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h3 == h1 {
		t.Error("an edited dataset must move the fingerprint")
	}

	// A changed SplitSeed re-splits the same eval set, so the fingerprint
	// must move with it — or a resumed run would restore the old division's
	// checkpoint under the new division's plan. Same for the fraction.
	f4 := newFake(t, fixture(t, "dataset-support-llm.json"), map[string]string{"1": fixture(t, "items-llm-page1.json")})
	h4, err := f4.newEvals(t, func(o *langfuse.Options) { o.SplitSeed = "2026-08-28" }).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h4 == h1 {
		t.Error("a changed SplitSeed must move the fingerprint")
	}
	f5 := newFake(t, fixture(t, "dataset-support-llm.json"), map[string]string{"1": fixture(t, "items-llm-page1.json")})
	h5, err := f5.newEvals(t, func(o *langfuse.Options) { o.HoldoutFrac = 0.5 }).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h5 == h1 {
		t.Error("a changed holdout fraction must move the fingerprint")
	}
}

func TestCountSplitsRecordsTheConfiguredFraction(t *testing.T) {
	t.Parallel()
	rows := make([]string, 0, 20)
	for i := range 20 {
		rows = append(rows, item(fmt.Sprintf("it-%02d", i), fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetWith("fixture-dataset"), map[string]string{"1": pageWith(rows...)})
	ev := f.newEvals(t, func(o *langfuse.Options) { o.HoldoutFrac = 0.5 })

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
		opts langfuse.Options
		want string // substring of the refusal; empty means New must succeed
	}{
		{name: "no dataset", opts: langfuse.Options{PublicKey: "pk", SecretKey: "sk"}, want: "no dataset name"},
		{name: "plain http refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "http://example.com"}, want: "plain HTTP"},
		{name: "plain http allowed by flag", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "http://example.com", AllowInsecureBaseURL: true}},
		{name: "loopback refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://127.0.0.1:8080"}, want: "private address"},
		{name: "loopback allowed by flag", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://127.0.0.1:8080", AllowPrivateAddress: true}},
		{name: "ipv6 loopback refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://[::1]:8080"}, want: "private address"},
		{name: "link-local refused with no override", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://169.254.169.254", AllowInsecureBaseURL: true, AllowPrivateAddress: true}, want: "link-local"},
		{name: "non-canonical address refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://2130706433"}, want: "neither a hostname nor a canonical IP"},
		{name: "userinfo refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://user:pass@example.com"}, want: "userinfo"},
		{name: "bad scheme refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "ftp://example.com"}, want: "not http or https"},
		{name: "no host refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://"}, want: "no host"},
		{name: "hosted endpoint accepted", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://cloud.langfuse.com"}},
		{name: "hosted endpoint with trailing slash", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://cloud.langfuse.com/"}},
		{name: "negative holdout frac refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://example.com", HoldoutFrac: -0.1}, want: "holdout fraction"},
		{name: "holdout frac at one refused", opts: langfuse.Options{Dataset: "d", PublicKey: "pk", SecretKey: "sk", Host: "https://example.com", HoldoutFrac: 1}, want: "holdout fraction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := langfuse.New(tt.opts)
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

// TestNewRequiresBothKeysFromTheEnvironment is serial: t.Setenv mutates
// process state and cannot run beside parallel tests.
func TestNewRequiresBothKeysFromTheEnvironment(t *testing.T) {
	t.Setenv(langfuse.PublicKeyEnv, "")
	t.Setenv(langfuse.SecretKeyEnv, "")
	_, err := langfuse.New(langfuse.Options{Dataset: "d", Host: "https://example.com"})
	if err == nil {
		t.Fatal("missing keys were accepted")
	}
	for _, name := range []string{langfuse.PublicKeyEnv, langfuse.SecretKeyEnv} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}

	// One key alone is still refused: the dataset API authenticates every
	// request with both, as basic auth.
	t.Setenv(langfuse.PublicKeyEnv, "pk")
	_, err = langfuse.New(langfuse.Options{Dataset: "d", Host: "https://example.com"})
	if err == nil {
		t.Fatal("a single key was accepted")
	}

	// Explicit keys are honored even with the environment empty.
	if _, err := langfuse.New(langfuse.Options{Dataset: "d", Host: "https://example.com", PublicKey: "pk", SecretKey: "sk"}); err != nil {
		t.Fatalf("New with explicit keys: %v", err)
	}
}

// TestHostComesFromTheEnvironment is serial for the same reason.
func TestHostComesFromTheEnvironment(t *testing.T) {
	t.Setenv(langfuse.PublicKeyEnv, "pk")
	t.Setenv(langfuse.SecretKeyEnv, "sk")

	t.Setenv(langfuse.HostEnv, "http://example.com")
	if _, err := langfuse.New(langfuse.Options{Dataset: "d"}); err == nil {
		t.Fatal("a plain-http env host was accepted")
	}

	t.Setenv(langfuse.HostEnv, "https://example.com")
	if _, err := langfuse.New(langfuse.Options{Dataset: "d"}); err != nil {
		t.Fatalf("New with an https env host: %v", err)
	}
}
