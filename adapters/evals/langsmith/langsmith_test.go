package langsmith_test

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

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/langsmith"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeLangSmith is a minimal stand-in for the LangSmith REST API, serving
// the fixtures in the same {items, next_cursor} shape the real one uses.
type fakeLangSmith struct {
	dataset string            // raw /datasets response
	pages   map[string]string // cursor -> raw /examples response

	// datasetsPages, when non-nil, serves /datasets by cursor exactly like
	// pages serves /examples, so multi-page dataset listings can be tested.
	datasetsPages map[string]string

	// retryOnce 429s the first /examples request, then serves normally.
	retryOnce bool
	// always429 429s every /examples request.
	always429 bool
	// unauthorized 401s every /datasets request.
	unauthorized bool
	mu           sync.Mutex
	srv          *httptest.Server
	examples     int    // /examples requests seen
	lastKey      string // the x-api-key header of the most recent request
}

func newFake(t *testing.T, dataset string, pages map[string]string) *fakeLangSmith {
	t.Helper()
	f := &fakeLangSmith{dataset: dataset, pages: pages}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLangSmith) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastKey = r.Header.Get("x-api-key")
	f.mu.Unlock()

	switch r.URL.Path {
	case "/datasets":
		if f.unauthorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if f.datasetsPages != nil {
			cursor := r.URL.Query().Get("cursor")
			f.mu.Lock()
			body, ok := f.datasetsPages[cursor]
			f.mu.Unlock()
			if !ok {
				http.Error(w, "unknown cursor", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, f.dataset)
	case "/examples":
		f.mu.Lock()
		f.examples++
		first := f.examples == 1
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

// firstDatasetName extracts the name of the first dataset in a raw
// /datasets response, so an Evals built against the fake requests the name
// the fixture actually carries — the adapter matches by exact name, and the
// fake serves whatever dataset string it was configured with.
func firstDatasetName(raw string) string {
	var env struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil || len(env.Items) == 0 {
		return ""
	}
	return env.Items[0].Name
}

// newEvals builds an Evals against the fake server, opted into the insecure
// and private settings every fixture endpoint (127.0.0.1, plain http)
// requires.
func (f *fakeLangSmith) newEvals(t *testing.T, opts ...func(*langsmith.Options)) *langsmith.Evals {
	t.Helper()
	dataset := "fixture-dataset"
	if name := firstDatasetName(f.dataset); name != "" {
		dataset = name
	}
	o := langsmith.Options{
		Dataset:              dataset,
		Endpoint:             f.srv.URL,
		APIKey:               "test-key",
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	}
	for _, fn := range opts {
		fn(&o)
	}
	ev, err := langsmith.New(o)
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
func collectCases(t *testing.T, ev *langsmith.Evals) []*knov1.Case {
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

// datasetWith builds a one-dataset /datasets envelope.
func datasetWith(name, id string) string {
	return fmt.Sprintf(`{"items":[{"id":%q,"name":%q,"modified_at":"2026-08-01T00:00:00Z","example_count":1}],"next_cursor":""}`, id, name)
}

// pageWith builds an /examples envelope from raw rows.
func pageWith(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `],"next_cursor":""}`
}

// row builds one example row; outputs is omitted when not given.
func row(id, inputs string, outputs ...string) string {
	s := fmt.Sprintf(`{"id":%q,"inputs":%s`, id, inputs)
	if len(outputs) > 0 {
		s += `,"outputs":` + outputs[0]
	}
	return s + "}"
}

func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	f := newFake(t,
		fixture(t, "datasets-llm.json"),
		map[string]string{
			"":         fixture(t, "examples-llm-page1.json"),
			"cursor-2": fixture(t, "examples-llm-page2.json"),
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
			[]byte(fixture(t, "datasets-chat.json")), // request 0: /datasets
			[]byte(fixture(t, "examples-chat.json")), // requests 1+: pages
		},
	}
	ev, err := langsmith.New(langsmith.Options{
		Dataset:  "support-chat",
		Endpoint: "https://example.com", // never dialed; the probe intercepts
		APIKey:   "test-key",
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

	// Request 0 (the datasets page) is closed by lookupDataset; request 1
	// (the open examples page) must have been closed by the iterator's own
	// deferred cleanup, which is what an early break runs.
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.probes) < 2 {
		t.Fatalf("expected at least 2 requests, saw %d", len(transport.probes))
	}
	transport.probes[0].AssertClosed(t)
	transport.probes[1].AssertClosed(t)
}

func TestRowMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		row        string
		wantInput  string
		wantExpect string
		wantNote   string // substring of the provenance derivation note
		wantErr    string // substring of the fatal error
	}{
		{
			name:       "llm question and answer",
			row:        row("ex-1", `{"question":"q1"}`, `{"answer":"a1"}`),
			wantInput:  "q1",
			wantExpect: "a1",
		},
		{
			name:       "llm input and output",
			row:        row("ex-2", `{"input":"q2"}`, `{"output":"o2"}`),
			wantInput:  "q2",
			wantExpect: "o2",
		},
		{
			name:       "named key beats document order",
			row:        row("ex-3", `{"question":"q3","extra":"e3"}`, `{"answer":"a3","extra":"x"}`),
			wantInput:  "q3",
			wantExpect: "a3",
		},
		{
			name:       "non-string named key falls through to the next named key",
			row:        row("ex-4", `{"question":42,"input":"q4"}`, `{"answer":"a4"}`),
			wantInput:  "q4",
			wantExpect: "a4",
		},
		{
			name:       "document order fallback",
			row:        row("ex-5", `{"text":"first","note":"second"}`, `{"out":"x","extra":"y"}`),
			wantInput:  "first\nsecond",
			wantExpect: "x\ny",
		},
		{
			name:       "chat messages concatenated",
			row:        row("ex-6", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`, `{"answer":"hello"}`),
			wantInput:  "hi\nhello",
			wantExpect: "hello",
		},
		{
			name:       "chat expected from the last assistant message",
			row:        row("ex-7", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"bye"}]}`, `{}`),
			wantInput:  "hi\nbye",
			wantExpect: "bye",
			wantNote:   "last assistant message",
		},
		{
			name:       "chat null outputs still falls back to the assistant",
			row:        row("ex-8", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"bye"}]}`, "null"),
			wantInput:  "hi\nbye",
			wantExpect: "bye",
			wantNote:   "last assistant message",
		},
		{
			name:       "llm null outputs is an empty expected, named",
			row:        row("ex-9", `{"question":"q9"}`, "null"),
			wantInput:  "q9",
			wantExpect: "",
			wantNote:   "outputs is null",
		},
		{
			name:       "absent outputs is an empty expected, named",
			row:        row("ex-10", `{"question":"q10"}`),
			wantInput:  "q10",
			wantExpect: "",
			wantNote:   "outputs is null",
		},
		{
			name:    "inputs with no string field is fatal",
			row:     row("ex-11", `{"deep":{"nested":1}}`, `{"answer":"a"}`),
			wantErr: `example "ex-11" has no input`,
		},
		{
			name:    "inputs that are not an object is fatal",
			row:     row("ex-13", `["not","an","object"]`, `{"answer":"a"}`),
			wantErr: `example "ex-13" has no input`,
		},
		{
			name:    "outputs with no string field is fatal",
			row:     row("ex-12", `{"question":"q"}`, `{"nested":{"deep":1}}`),
			wantErr: "outputs holds no string field",
		},
		{
			name:    "chat with no message content is fatal",
			row:     row("ex-14", `{"messages":[{"role":"user"}]}`, `{"answer":"a"}`),
			wantErr: `example "ex-14" has no input`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, datasetWith("fixture-dataset", "ds-1"), map[string]string{"": pageWith(tt.row)})
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
			if got.GetExpected() != tt.wantExpect {
				t.Errorf("Expected = %q, want %q", got.GetExpected(), tt.wantExpect)
			}
			p := got.GetProvenance()
			if p == nil {
				t.Fatal("no provenance")
			}
			if p.GetSource() != "langsmith" {
				t.Errorf("Source = %q, want langsmith", p.GetSource())
			}
			if want := "dataset:fixture-dataset:" + got.GetId(); p.GetSourceRef() != want {
				t.Errorf("SourceRef = %q, want %q", p.GetSourceRef(), want)
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
		fixture(t, "datasets-llm.json"),
		map[string]string{
			"":         fixture(t, "examples-llm-page1.json"),
			"cursor-2": fixture(t, "examples-llm-page2.json"),
		})
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	want := []string{"ex-llm-0001", "ex-llm-0002", "ex-llm-0003", "ex-llm-0004"}
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
	if got := f.lastKey; got != "test-key" {
		t.Errorf("the x-api-key header = %q, want %q", got, "test-key")
	}
}

func TestDuplicateExampleIDIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "datasets-duplicate.json"),
		map[string]string{"": fixture(t, "examples-duplicate.json")})
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
		t.Fatal("a duplicate example id was accepted")
	}
	for _, frag := range []string{"duplicate example id", "ex-dup-0001"} {
		if !strings.Contains(gotErr.Error(), frag) {
			t.Errorf("error %q does not mention %q", gotErr, frag)
		}
	}
}

func TestMalformedRowIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "datasets-malformed.json"),
		map[string]string{"": fixture(t, "examples-malformed.json")})
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
	if got == nil || got.GetId() != "ex-mal-0001" {
		t.Errorf("the healthy row must be yielded before the fatal, got %v", got)
	}
	if gotErr == nil {
		t.Fatal("the malformed row was skipped, not fatal")
	}
	if !strings.Contains(gotErr.Error(), "ex-mal-0002") {
		t.Errorf("error %q does not name the malformed example", gotErr)
	}
}

func TestEmptyDatasetYieldsNoCases(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "datasets-empty.json"),
		map[string]string{"": fixture(t, "examples-empty.json")})
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

func TestDatasetNotFoundIsOuterError(t *testing.T) {
	t.Parallel()
	f := newFake(t, `{"items":[],"next_cursor":""}`, map[string]string{"": fixture(t, "examples-llm-page1.json")})
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a missing dataset was accepted")
	}
	if !strings.Contains(err.Error(), "no dataset named") {
		t.Errorf("error %q does not say the dataset is missing", err)
	}
}

func TestMultipleDatasetsMatchedIsFatal(t *testing.T) {
	t.Parallel()
	body := `{"items":[` +
		`{"id":"ds-a","name":"support-llm","modified_at":"t","example_count":1},` +
		`{"id":"ds-b","name":"support-llm","modified_at":"t","example_count":1}` +
		`],"next_cursor":""}`
	f := newFake(t, body, map[string]string{"": fixture(t, "examples-llm-page1.json")})
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("two datasets with one name were accepted")
	}
	if !strings.Contains(err.Error(), "2 datasets match") {
		t.Errorf("error %q does not name the match count", err)
	}
}

func TestDatasetPaginationSpansPages(t *testing.T) {
	t.Parallel()
	// The dataset of interest sits on the second page of /datasets; the
	// cursor loop must find it, not give up after the first page.
	page1 := `{"items":[{"id":"ds-other","name":"other","modified_at":"t","example_count":1}],"next_cursor":"datasets-2"}`
	page2 := `{"items":[{"id":"ds-llm-001","name":"fixture-dataset","modified_at":"2026-08-01T12:00:00Z","example_count":4}],"next_cursor":""}`
	f := newFake(t, ``, map[string]string{
		"":         fixture(t, "examples-llm-page1.json"),
		"cursor-2": fixture(t, "examples-llm-page2.json"),
	})
	f.datasetsPages = map[string]string{"": page1, "datasets-2": page2}
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want the 4 across both examples pages", len(cases))
	}
}

func TestMultipleDatasetsMatchedAcrossPagesIsFatal(t *testing.T) {
	t.Parallel()
	// The multi-match guard must see later pages too: a name that appears
	// once per page is still ambiguous, and the fingerprint must not depend
	// on how far the first page happened to reach.
	page1 := `{"items":[{"id":"ds-a","name":"fixture-dataset","modified_at":"t","example_count":1}],"next_cursor":"datasets-2"}`
	page2 := `{"items":[{"id":"ds-b","name":"fixture-dataset","modified_at":"t","example_count":1}],"next_cursor":""}`
	f := newFake(t, ``, map[string]string{"": fixture(t, "examples-llm-page1.json")})
	f.datasetsPages = map[string]string{"": page1, "datasets-2": page2}
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("two datasets with one name were accepted")
	}
	if !strings.Contains(err.Error(), "2 datasets match") {
		t.Errorf("error %q does not name the match count", err)
	}
}

func TestDialTimeRecheckRefusesLocalhost(t *testing.T) {
	t.Parallel()
	// The config-time check lets "localhost" through — it reads like an
	// ordinary name — and the dial-time recheck is what catches it resolving
	// to loopback. AllowPrivateAddress is deliberately NOT set: the refusal
	// must come from the resolved address, not from the URL.
	f := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
	endpoint := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
	ev, err := langsmith.New(langsmith.Options{
		Dataset:              "fixture-dataset",
		Endpoint:             endpoint,
		APIKey:               "test-key",
		AllowInsecureBaseURL: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = ev.CountSplits(context.Background())
	if err == nil {
		t.Fatal("https://localhost must be refused at dial time")
	}
	if !strings.Contains(err.Error(), "private address") {
		t.Errorf("error %q does not name the refusal", err)
	}
}

func TestUnauthorizedDoesNotEchoTheKey(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
	f.unauthorized = true
	ev := f.newEvals(t)

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not name the status", err)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Errorf("error %q echoes the API key", err)
	}
}

func Test429IsRetriedWithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		fixture(t, "datasets-llm.json"),
		map[string]string{
			"":         fixture(t, "examples-llm-page1.json"),
			"cursor-2": fixture(t, "examples-llm-page2.json"),
		})
	f.retryOnce = true
	ev := f.newEvals(t)

	cases := collectCases(t, ev)
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.examples != 3 {
		t.Errorf("the examples API saw %d requests, want 3 (one 429, then page 1 and page 2)", f.examples)
	}
}

func Test429PersistsIsFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
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
	if f.examples != 3 {
		// Deliberately pins the retry budget (maxAttempts): the test fails
		// loudly if the budget ever changes, so the behavior is a reviewed
		// decision, not a drift.
		t.Errorf("the examples API saw %d requests, want 3 (the retry budget)", f.examples)
	}
}

func TestRepeatingCursorFailsLoudly(t *testing.T) {
	t.Parallel()
	// A page that keeps answering with its own cursor: the pagination can
	// never advance, and the stream must stop at the second page rather
	// than spin.
	repeatPage := pageWith(
		row("ex-rep-0001", `{"question":"r1"}`, `{"answer":"a"}`),
		row("ex-rep-0002", `{"question":"r2"}`, `{"answer":"a"}`),
	)
	repeatPage = strings.Replace(repeatPage, `"next_cursor":""`, `"next_cursor":"cursor-2"`, 1)
	f := newFake(t,
		fixture(t, "datasets-llm.json"),
		map[string]string{
			"":         fixture(t, "examples-llm-page1.json"),
			"cursor-2": repeatPage,
		})
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
		t.Fatal("a repeating cursor did not stop the stream")
	}
	if !strings.Contains(gotErr.Error(), "cursor") {
		t.Errorf("error %q does not name the cursor", gotErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.examples != 2 {
		t.Errorf("the examples API saw %d requests, want 2; a repeating cursor must stop at the second page", f.examples)
	}
}

func TestRowSizeCapNamesTheExample(t *testing.T) {
	t.Parallel()
	big := fmt.Sprintf(`{"id":"ex-big","inputs":{"question":%q},"outputs":{"answer":"x"}}`,
		strings.Repeat("q", 5<<20))
	f := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": pageWith(big)})
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
	for _, frag := range []string{"ex-big", "row cap"} {
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
		rows = append(rows, row(fmt.Sprintf("ex-%03d", i), fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetWith("fixture-dataset", "ds-1"), map[string]string{"": pageWith(rows...)})
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

// TestSameSplitAsJSONL asserts the denominator math cannot vary by source:
// the same Case ids land in the same halves whether they come from a JSONL
// file or from LangSmith.
func TestSameSplitAsJSONL(t *testing.T) {
	t.Parallel()
	ids := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	rows := make([]string, 0, len(ids))
	for i, id := range ids {
		rows = append(rows, row(id, fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetWith("fixture-dataset", "ds-1"), map[string]string{"": pageWith(rows...)})
	ev := f.newEvals(t)

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
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

	if !splitsEqual(fromLangsmith, fromJSONL) {
		t.Errorf("splits differ by source:\nlangsmith: %v\njsonl: %v", fromLangsmith, fromJSONL)
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
	f := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
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

	// A changed modified_at moves the fingerprint.
	edited := strings.Replace(fixture(t, "datasets-llm.json"), "2026-08-01T12:00:00Z", "2026-08-02T12:00:00Z", 1)
	f2 := newFake(t, edited, map[string]string{"": fixture(t, "examples-llm-page1.json")})
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
	f4 := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
	h4, err := f4.newEvals(t, func(o *langsmith.Options) { o.SplitSeed = "2026-08-28" }).ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h4 == h1 {
		t.Error("a changed SplitSeed must move the fingerprint")
	}
	f5 := newFake(t, fixture(t, "datasets-llm.json"), map[string]string{"": fixture(t, "examples-llm-page1.json")})
	h5, err := f5.newEvals(t, func(o *langsmith.Options) { o.HoldoutFrac = 0.5 }).ContentHash(context.Background())
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
		rows = append(rows, row(fmt.Sprintf("ex-%02d", i), fmt.Sprintf(`{"question":"q%d"}`, i), `{"answer":"a"}`))
	}
	f := newFake(t, datasetWith("fixture-dataset", "ds-1"), map[string]string{"": pageWith(rows...)})
	ev := f.newEvals(t, func(o *langsmith.Options) { o.HoldoutFrac = 0.5 })

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
		opts langsmith.Options
		want string // substring of the refusal; empty means New must succeed
	}{
		{name: "no dataset", opts: langsmith.Options{APIKey: "k"}, want: "no dataset name"},
		{name: "plain http refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "http://example.com"}, want: "plain HTTP"},
		{name: "plain http allowed by flag", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "http://example.com", AllowInsecureBaseURL: true}},
		{name: "loopback refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://127.0.0.1:8080"}, want: "private address"},
		{name: "loopback allowed by flag", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://127.0.0.1:8080", AllowPrivateAddress: true}},
		{name: "ipv6 loopback refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://[::1]:8080"}, want: "private address"},
		{name: "link-local refused with no override", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://169.254.169.254", AllowInsecureBaseURL: true, AllowPrivateAddress: true}, want: "link-local"},
		{name: "non-canonical address refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://2130706433"}, want: "neither a hostname nor a canonical IP"},
		{name: "userinfo refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://user:pass@example.com"}, want: "userinfo"},
		{name: "bad scheme refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "ftp://example.com"}, want: "not http or https"},
		{name: "no host refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://"}, want: "no host"},
		{name: "hosted endpoint accepted", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://api.smith.langchain.com"}},
		{name: "hosted endpoint with trailing slash", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://api.smith.langchain.com/"}},
		{name: "negative holdout frac refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://example.com", HoldoutFrac: -0.1}, want: "holdout fraction"},
		{name: "holdout frac at one refused", opts: langsmith.Options{Dataset: "d", APIKey: "k", Endpoint: "https://example.com", HoldoutFrac: 1}, want: "holdout fraction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := langsmith.New(tt.opts)
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

// TestNewRequiresTheKeyFromTheEnvironment is serial: t.Setenv mutates
// process state and cannot run beside parallel tests.
func TestNewRequiresTheKeyFromTheEnvironment(t *testing.T) {
	t.Setenv(langsmith.DefaultKeyEnv, "")
	_, err := langsmith.New(langsmith.Options{Dataset: "d", Endpoint: "https://example.com"})
	if err == nil {
		t.Fatal("a missing key was accepted")
	}
	if !strings.Contains(err.Error(), "LANGSMITH_API_KEY") {
		t.Errorf("error %q does not name LANGSMITH_API_KEY", err)
	}

	// An explicit key is honored even with the environment empty.
	if _, err := langsmith.New(langsmith.Options{Dataset: "d", Endpoint: "https://example.com", APIKey: "k"}); err != nil {
		t.Fatalf("New with an explicit key: %v", err)
	}
}

// TestEndpointComesFromTheEnvironment is serial for the same reason.
func TestEndpointComesFromTheEnvironment(t *testing.T) {
	t.Setenv(langsmith.DefaultKeyEnv, "k")

	t.Setenv(langsmith.EndpointEnv, "http://example.com")
	if _, err := langsmith.New(langsmith.Options{Dataset: "d"}); err == nil {
		t.Fatal("a plain-http env endpoint was accepted")
	}

	t.Setenv(langsmith.EndpointEnv, "https://example.com")
	if _, err := langsmith.New(langsmith.Options{Dataset: "d"}); err != nil {
		t.Fatalf("New with an https env endpoint: %v", err)
	}
}
