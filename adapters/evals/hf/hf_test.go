package hf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

const testRevision = "abc123def"

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fixture reads a recorded body from testdata/. The bodies are shapes the
// real datasets-server serves, frozen here so the tests never need network
// access, a token, or spend.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(b)
}

// fakeHF is a stand-in for the datasets-server API: /splits resolves the
// dataset, /rows serves one body per offset. The x-revision header is the
// fingerprint, exactly as the real API answers it.
type fakeHF struct {
	splitsStatus int
	splitsBody   string

	rowsStatus int
	rowsBodies map[string]string // keyed by offset query value
	// revisions keys a per-page x-revision, so a test can drift the
	// fingerprint mid-read; absent entries serve testRevision.
	revisions map[string]string

	noRevision bool
	// noSplitsRevision omits the header on /splits only.
	noSplitsRevision bool
	// splitsRevision overrides the x-revision served on /splits.
	splitsRevision string

	mu       sync.Mutex
	requests []*http.Request
}

func newFakeHF() *fakeHF {
	return &fakeHF{splitsStatus: 200, rowsStatus: 200}
}

func (f *fakeHF) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()

		if !f.noRevision {
			rev := testRevision
			switch {
			case req.URL.Path == "/splits" && f.noSplitsRevision:
				rev = ""
			case req.URL.Path == "/splits" && f.splitsRevision != "":
				rev = f.splitsRevision
			default:
				if r, ok := f.revisions[req.URL.Query().Get("offset")]; ok {
					rev = r
				}
			}
			if rev != "" {
				w.Header().Set("x-revision", rev)
			}
		}
		switch req.URL.Path {
		case "/splits":
			w.WriteHeader(f.splitsStatus)
			if f.splitsStatus == http.StatusOK {
				_, _ = fmt.Fprint(w, f.splitsBody)
			}
		case "/rows":
			w.WriteHeader(f.rowsStatus)
			if f.rowsStatus == http.StatusOK {
				body, ok := f.rowsBodies[req.URL.Query().Get("offset")]
				if !ok {
					// An unknown offset is the pagination terminator: the
					// iterator fetches past the last page and ends on the
					// empty one.
					body = `{"rows":[],"num_rows_total":0,"partial":false}`
				}
				_, _ = fmt.Fprint(w, body)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newHF starts a fake and returns an Evals pointed at it with the security
// flags lifted — httptest serves plain HTTP on loopback.
func newHF(t *testing.T, f *fakeHF, opts Options) *Evals {
	t.Helper()
	srv := f.serve(t)
	opts.Host = srv.URL
	opts.AllowInsecureBaseURL = true
	opts.AllowPrivateAddress = true
	if opts.Dataset == "" {
		opts.Dataset = "org/name"
	}
	if opts.Config == "" {
		opts.Config = "main"
	}
	if opts.Split == "" {
		opts.Split = "train"
	}
	ev, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ev
}

// standardFixture wires the two-page eval set: six rows on page one (row_idx
// 0..5) and two on page two (row_idx 6..7).
func standardFixture(t *testing.T) *fakeHF {
	t.Helper()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{
		"0": fixture(t, "page1.json"),
		"6": fixture(t, "page2.json"),
	}
	return f
}

func collectCases(t *testing.T, seq iter.Seq2[*core.Case, error]) []*core.Case {
	t.Helper()
	var out []*core.Case
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterator: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// firstErr ranges the iterator and returns the first yielded error, or nil
// if it completes clean. Callers must not range the same iterator twice.
func firstErr(seq iter.Seq2[*core.Case, error]) error {
	var got error
	for _, err := range seq {
		if err != nil {
			got = err
			break
		}
	}
	return got
}

func collectSplits(t *testing.T, seq iter.Seq2[*core.Case, error]) map[string]knov1.Split {
	t.Helper()
	out := make(map[string]knov1.Split)
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterator: %v", err)
		}
		out[c.GetId()] = c.GetSplit()
	}
	return out
}

func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{})

	coretest.ConformIterator(t, func(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
		return ev.Cases(ctx)
	})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	coretest.EvalsDuplicateIDs(t, seq)
}

// probeTransport serves every request the given bodies, recording that each
// response body was closed.
type probeTransport struct {
	bodies [][]byte

	mu     sync.Mutex
	next   int
	probes []*coretest.CleanupProbe
}

func (t *probeTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	body := t.bodies[0]
	if t.next < len(t.bodies) {
		body = t.bodies[t.next]
	}
	probe := &coretest.CleanupProbe{}
	t.probes = append(t.probes, probe)
	t.next++
	hdr := make(http.Header)
	hdr.Set("x-revision", testRevision)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     hdr,
		Body:       &probeBody{probe: probe, r: bytes.NewReader(body)},
	}, nil
}

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
			[]byte(fixture(t, "splits.json")), // request 0: the split list
			[]byte(fixture(t, "page1.json")),  // request 1+: pages
		},
	}
	ev, err := New(Options{
		Dataset: "org/name",
		Config:  "main",
		Split:   "train",
		Host:    "https://example.com", // never dialed; the probe intercepts
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

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.probes) < 2 {
		t.Fatalf("expected at least 2 requests, saw %d", len(transport.probes))
	}
	transport.probes[0].AssertClosed(t) // the splits list, closed by Splits
	transport.probes[1].AssertClosed(t) // the open page, closed by the iterator's own cleanup
}

// TestRowMappingGolden pins the mapping of one row: id from row_idx, input
// from the first-present input column, expected from the first-present
// winner, and the provenance locator. The golden file is what a reviewer
// diffs when the mapping changes.
func TestRowMappingGolden(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": fixture(t, "page-singlewinner.json")}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	cases := collectCases(t, seq)
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	c := cases[0]

	type golden struct {
		ID        string `json:"id"`
		Input     string `json:"input"`
		Expected  string `json:"expected"`
		Source    string `json:"source"`
		SourceRef string `json:"source_ref"`
	}
	var want golden
	if err := json.Unmarshal([]byte(fixture(t, filepath.Join("golden", "single-winner.golden"))), &want); err != nil {
		t.Fatalf("parsing the golden file: %v", err)
	}
	got := golden{
		ID:        c.GetId(),
		Input:     c.GetInput(),
		Expected:  c.GetExpected(),
		Source:    c.GetProvenance().GetSource(),
		SourceRef: c.GetProvenance().GetSourceRef(),
	}
	if got != want {
		t.Errorf("mapping differs from golden:\n got: %+v\nwant: %+v", got, want)
	}
	if c.GetExpected() == "loser" || c.GetExpected() == "dropped" {
		t.Errorf("expected=%q: a second-present winner overwrote the first", c.GetExpected())
	}
	if c.GetProvenance().GetDerived() {
		t.Error("an hf row is not derived; nothing here marks it so")
	}
}

// TestInputColumnPrecedence: input/prompt/question resolve first-present at
// the dataset level, so the same-name rule holds across all rows.
func TestInputColumnPrecedence(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	cases := collectCases(t, seq)
	want := map[string]string{
		"0": "q0", // question
		"1": "q1", // prompt
		"2": "q2", // input
	}
	for id, in := range want {
		c := cases[int(id[0]-'0')]
		if c.GetInput() != in {
			t.Errorf("case %s input = %q, want %q", id, c.GetInput(), in)
		}
	}
}

func TestStructuredInputIsCanonicalJSON(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": `{"rows":[
		{"row_idx":0,"row":{"input":{"a":"<b>&</b>","z":1},"answer":"x"}}
	],"num_rows_total":1,"partial":false}`}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	cases := collectCases(t, seq)
	// The input object is rendered as canonical JSON: keys sorted, HTML
	// escaped, byte-stable whatever key order the server used.
	if got, want := cases[0].GetInput(), `{"a":"\u003cb\u003e\u0026\u003c/b\u003e","z":1}`; got != want {
		t.Errorf("structured input = %q, want %q", got, want)
	}
}

func TestNullInputIsFatalNamingTheRow(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": fixture(t, "page-nullinput.json")}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a null input was accepted")
	}
	if !strings.Contains(got.Error(), "row 1") {
		t.Errorf("refusal %q does not name the row_idx", got)
	}
	if !strings.Contains(got.Error(), `"question"`) || !strings.Contains(got.Error(), "null") {
		t.Errorf("refusal %q does not name the column and the null", got)
	}

	// A row missing the chosen column is the same failure and names it as
	// absent.
	f2 := newFakeHF()
	f2.splitsBody = fixture(t, "splits.json")
	f2.rowsBodies = map[string]string{"0": `{"rows":[
		{"row_idx":0,"row":{"question":"q0","answer":"a0"}},
		{"row_idx":1,"row":{"answer":"a1"}}
	],"num_rows_total":2,"partial":false}`}
	ev2 := newHF(t, f2, Options{})
	seq2, err := ev2.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	got = firstErr(seq2)
	if got == nil {
		t.Fatal("a row without the chosen input column was accepted")
	}
	if !strings.Contains(got.Error(), "row 1") || !strings.Contains(got.Error(), "absent") {
		t.Errorf("refusal %q does not name the row and the absence", got)
	}
}

func TestNoInputColumnRefusalNamesTheColumns(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": fixture(t, "page-noinput.json")}
	ev := newHF(t, f, Options{})

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a split with no input column was accepted")
	}
	if !strings.Contains(err.Error(), "answer, notes") {
		t.Errorf("refusal %q does not name the actual columns, sorted", err)
	}
}

func TestPaginationAcrossPagesAndOffset(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	cases := collectCases(t, seq)
	if len(cases) != 8 {
		t.Fatalf("got %d cases, want 8", len(cases))
	}
	if id := cases[7].GetId(); id != "7" {
		t.Errorf("last case id = %q, want %q", id, "7")
	}

	var offsets []string
	f.mu.Lock()
	for _, req := range f.requests {
		if req.URL.Path == "/rows" {
			offsets = append(offsets, req.URL.Query().Get("offset"))
		}
	}
	f.mu.Unlock()
	wantOffsets := []string{"0", "6", "8"}
	if strings.Join(offsets, ",") != strings.Join(wantOffsets, ",") {
		t.Errorf("row offsets = %v, want %v", offsets, wantOffsets)
	}
}

func TestDuplicateRowIdxAcrossPagesIsFatal(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	f.rowsBodies["6"] = `{"rows":[{"row_idx":3,"row":{"question":"q3","answer":"a3"}}],` +
		`"num_rows_total":9,"partial":false}`
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a duplicate row_idx across pages was accepted")
	}
	if !strings.Contains(got.Error(), "row 3") || !strings.Contains(got.Error(), "twice") {
		t.Errorf("refusal %q does not name the row and the duplication", got)
	}
}

func TestPartialSubsampleIsRefusedAtOpen(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": fixture(t, "page-partial.json")}
	ev := newHF(t, f, Options{})

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a partial subsample was accepted")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("refusal %q does not name the partial flag", err)
	}
}

func TestMissingRevisionHeaderIsFatal(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	f.noRevision = true
	ev := newHF(t, f, Options{})

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a response without x-revision was accepted")
	}
	if !strings.Contains(err.Error(), "x-revision") {
		t.Errorf("refusal %q does not name the missing header", err)
	}
}

func TestRevisionDriftMidStreamIsFatal(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	f.revisions = map[string]string{"6": "zzz999"}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a drifting x-revision was accepted")
	}
	if !strings.Contains(got.Error(), "changed while it was being read") {
		t.Errorf("refusal %q does not say the split changed mid-read", got)
	}
}

// TestUnauthorizedDoesNotEchoTheToken: the 401 taxonomy offers both remedies
// (wrong name, or gated) and never repeats the credential back.
func TestUnauthorizedDoesNotEchoTheToken(t *testing.T) {
	t.Parallel()
	secret := "test-token-shape-not-a-real-credential"

	for _, tc := range []struct {
		name  string
		token string
		want  string // remedy fragment the refusal must offer
	}{
		{"no token", "", "set HF_TOKEN"},
		{"stale token", secret, "current for this account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standardFixture(t)
			f.splitsStatus = http.StatusUnauthorized
			ev := newHF(t, f, Options{Token: tc.token})

			_, err := ev.Cases(context.Background())
			if err == nil {
				t.Fatal("a 401 was accepted")
			}
			if !strings.Contains(err.Error(), "401") {
				t.Errorf("refusal %q does not name the status", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not offer the remedy %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Error("the refusal echoes the token")
			}
		})
	}
}

func TestPairAbsenceRefusalNamesTheList(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{Split: "valid"})

	_, err := ev.Cases(context.Background())
	if err == nil {
		t.Fatal("a config/split pair the dataset does not offer was accepted")
	}
	for _, frag := range []string{`config "main" split "valid"`, `config "main" split "train"`} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("refusal %q does not name %q", err, frag)
		}
	}
}

func TestEmptySplitYieldsZeroCases(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	f.rowsBodies = map[string]string{"0": `{"rows":[],"num_rows_total":0,"partial":false}`}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	if cases := collectCases(t, seq); len(cases) != 0 {
		t.Fatalf("got %d cases from an empty split", len(cases))
	}
}

// TestNewRefusesUnsafeInputs: the security trio, part one — the config-time
// check. A plain-http host, a loopback address, and a link-local address are
// all refused unless the matching flag opts in.
func TestNewRefusesUnsafeInputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host string
	}{
		{"plain http", "http://example.com"},
		{"loopback address", "https://127.0.0.1:8000"},
		{"link-local", "https://169.254.169.254"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Options{
				Dataset: "org/name",
				Config:  "main",
				Split:   "train",
				Host:    tc.host,
				Token:   "sk-secret",
			})
			if err == nil {
				t.Fatalf("New accepted %s without the allow flags", tc.host)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Error("the refusal echoes the token")
			}
		})
	}

	// The allow flags are the explicit opt-in: the same hosts pass.
	for _, tc := range []struct {
		name       string
		host       string
		allowFlags []bool
	}{
		{"plain http opted in", "http://example.com", []bool{true, false}},
		{"loopback opted in", "https://127.0.0.1:8000", []bool{false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Options{
				Dataset:              "org/name",
				Config:               "main",
				Split:                "train",
				Host:                 tc.host,
				AllowInsecureBaseURL: tc.allowFlags[0],
				AllowPrivateAddress:  tc.allowFlags[1],
			}); err != nil {
				t.Errorf("New refused the opted-in host %s: %v", tc.host, err)
			}
		})
	}
}

// TestDialTimeRecheckRefusesLocalhost: the security trio, part two — the
// config-time check lets "localhost" through (it reads like an ordinary
// name), and the dial-time recheck is what catches it resolving to loopback.
func TestDialTimeRecheckRefusesLocalhost(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	srv := f.serve(t)
	endpoint := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	ev, err := New(Options{
		Dataset:              "org/name",
		Config:               "main",
		Split:                "train",
		Host:                 endpoint,
		AllowInsecureBaseURL: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = ev.CountSplits(context.Background())
	if err == nil {
		t.Fatal("localhost must be refused at dial time")
	}
	if !strings.Contains(err.Error(), "private address") {
		t.Errorf("refusal %q does not name the resolved address", err)
	}
}

// TestHoldoutCanaryViaSeal: the holdout is invisible through core.Seal, and
// the canary is a real run — a sealed Evals over a split that does have
// holdout cases must not see a single one.
func TestHoldoutCanaryViaSeal(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{HoldoutFrac: 0.5})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	splits := collectSplits(t, seq)
	holdout := 0
	for _, s := range splits {
		if s == knov1.Split_SPLIT_HOLDOUT {
			holdout++
		}
	}
	if holdout == 0 {
		t.Skip("the fixture's ids did not land any holdout case at frac 0.5")
	}

	sealed := core.Seal(ev)
	seq, err = sealed.Cases(context.Background())
	if err != nil {
		t.Fatalf("sealed Cases: %v", err)
	}
	for c, err := range seq {
		if err != nil {
			t.Fatalf("sealed iterator: %v", err)
		}
		if c.GetSplit() == knov1.Split_SPLIT_HOLDOUT {
			t.Errorf("case %s leaked the holdout through the seal", c.GetId())
		}
	}
}

// firstFingerprint resolves the ContentHash once against a fresh fake.
func firstFingerprint(t *testing.T, newFixture func() *fakeHF) string {
	t.Helper()
	ev := newHF(t, newFixture(), Options{})
	h, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	return h
}

func TestContentHashTracksRevisionAndSplitConfig(t *testing.T) {
	t.Parallel()
	base := func() *fakeHF {
		f := standardFixture(t)
		return f
	}

	first := firstFingerprint(t, base)

	// Identical input must produce an identical fingerprint.
	if second := firstFingerprint(t, base); second != first {
		t.Error("identical input produced different fingerprints")
	}

	// A moved revision must move the fingerprint.
	f := base()
	f.splitsRevision = "zzz999"
	ev := newHF(t, f, Options{})
	h, err := ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h == first {
		t.Error("a moved x-revision did not move the fingerprint")
	}

	// A re-split must move the fingerprint: a resumed run would restore the
	// old division's checkpoint under the new division's plan.
	ev = newHF(t, base(), Options{SplitSeed: "re-split"})
	h, err = ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h == first {
		t.Error("a changed SplitSeed did not move the fingerprint")
	}

	// So must a changed fraction.
	ev = newHF(t, base(), Options{HoldoutFrac: 0.4})
	h, err = ev.ContentHash(context.Background())
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h == first {
		t.Error("a changed holdout fraction did not move the fingerprint")
	}
}

func TestCountSplitsReportsDevAndHoldout(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{HoldoutFrac: 0.5})

	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Dev+counts.Holdout != 8 {
		t.Errorf("CountSplits totals %d, want 8", counts.Dev+counts.Holdout)
	}
	if counts.WeakLabelCases != 0 {
		t.Errorf("WeakLabelCases = %d; hf rows carry no derivation note, so it must be zero", counts.WeakLabelCases)
	}

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	splits := collectSplits(t, seq)
	dev, holdout := 0, 0
	for _, s := range splits {
		switch s {
		case knov1.Split_SPLIT_DEV:
			dev++
		case knov1.Split_SPLIT_HOLDOUT:
			holdout++
		}
	}
	if dev != counts.Dev || holdout != counts.Holdout {
		t.Errorf("CountSplits (%d dev, %d holdout) disagrees with a full pass (%d dev, %d holdout)",
			counts.Dev, counts.Holdout, dev, holdout)
	}
}

func TestCountSplitsRefusesAnUnreadableSource(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	f.splitsStatus = http.StatusInternalServerError
	ev := newHF(t, f, Options{})

	if _, err := ev.CountSplits(context.Background()); err == nil {
		t.Fatal("a 500 from /splits was accepted")
	}
	if _, err := ev.ContentHash(context.Background()); err == nil {
		t.Fatal("a 500 from /splits was accepted for ContentHash")
	}
}

// TestSplitIdentityMatchesJSONL asserts the denominator math cannot vary by
// source: the same row ids land in the same halves whether they come from a
// Hugging Face split (row_idx "0".."5") or a JSONL file with ids "0".."5".
func TestSplitIdentityMatchesJSONL(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = fixture(t, "splits.json")
	var rows []string
	for i := 0; i < 6; i++ {
		rows = append(rows, fmt.Sprintf(`{"row_idx":%d,"row":{"question":"q%d","answer":"a%d"}}`, i, i, i))
	}
	f.rowsBodies = map[string]string{"0": `{"rows":[` + strings.Join(rows, ",") + `],"num_rows_total":6,"partial":false}`}
	ev := newHF(t, f, Options{})

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	fromHF := collectSplits(t, seq)

	path := filepath.Join(t.TempDir(), "cases.jsonl")
	var sb strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&sb, `{"id":"%d","input":"q%d","expected":"a%d"}`+"\n", i, i, i)
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

	for id, s := range fromHF {
		if fromJSONL[id] != s {
			t.Errorf("case %s split = %v from hf, %v from jsonl", id, s, fromJSONL[id])
		}
	}
	if len(fromHF) != len(fromJSONL) {
		t.Errorf("hf served %d cases, jsonl %d", len(fromHF), len(fromJSONL))
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no dataset", Options{Config: "main", Split: "train"}, "no dataset"},
		{"no config", Options{Dataset: "org/name", Split: "train"}, "no config"},
		{"no split", Options{Dataset: "org/name", Config: "main"}, "no split"},
		{"holdout too big", Options{Dataset: "org/name", Config: "main", Split: "train", HoldoutFrac: 1}, "holdout fraction"},
		{"negative holdout", Options{Dataset: "org/name", Config: "main", Split: "train", HoldoutFrac: -0.1}, "holdout fraction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil {
				t.Fatalf("New accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestCancellationStopsTheOpen(t *testing.T) {
	t.Parallel()
	f := standardFixture(t)
	ev := newHF(t, f, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ev.Cases(ctx); err == nil {
		t.Fatal("a canceled context was accepted")
	}
}
