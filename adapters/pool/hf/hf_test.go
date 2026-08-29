package hf

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

const testRevision = "abc123def"

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeHF is a stand-in for the datasets-server API, shaped exactly like the
// Evals adapter's fake: /splits resolves the dataset, /rows serves one body
// per offset, and the x-revision header is the fingerprint.
type fakeHF struct {
	splitsStatus int
	splitsBody   string

	rowsStatus int
	rowsBodies map[string]string // keyed by offset query value
	revisions  map[string]string // per-offset x-revision override

	noRevision     bool
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
			case req.URL.Path == "/splits" && f.splitsRevision != "":
				rev = f.splitsRevision
			default:
				if r, ok := f.revisions[req.URL.Query().Get("offset")]; ok {
					rev = r
				}
			}
			w.Header().Set("x-revision", rev)
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
					// An unknown offset is the pagination terminator.
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

// textSplit is the standard two-page fixture: page one has text-bearing
// columns, page two has one row more.
func textSplit() *fakeHF {
	f := newFakeHF()
	f.splitsBody = `{"splits":[
		{"config":"main","split":"train"},
		{"config":"main","split":"test"}]}`
	f.rowsBodies = map[string]string{
		"0": `{"rows":[
			{"row_idx":0,"row":{"question":"q0","answer":"a0","score":1}},
			{"row_idx":1,"row":{"question":"q1","answer":"a1","note":null}},
			{"row_idx":2,"row":{"question":"q2","answer":2}}
		],"num_rows_total":3,"partial":false}`,
		"3": `{"rows":[
			{"row_idx":3,"row":{"question":"q3","answer":"a3"}}
		],"num_rows_total":4,"partial":false}`,
	}
	return f
}

func newPool(t *testing.T, f *fakeHF, opts Options) *Pool {
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
	if opts.Kind == "" {
		opts.Kind = "knowledge"
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func collectAssets(t *testing.T, seq iter.Seq2[*core.Asset, error]) []*core.Asset {
	t.Helper()
	var out []*core.Asset
	for a, err := range seq {
		if err != nil {
			t.Fatalf("iterator: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// firstErr ranges the iterator and returns the first yielded error, or nil
// if it completes clean.
func firstErr(seq iter.Seq2[*core.Asset, error]) error {
	var got error
	for _, err := range seq {
		if err != nil {
			got = err
			break
		}
	}
	return got
}

func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()
	p := newPool(t, textSplit(), Options{})

	coretest.ConformIterator(t, func(ctx context.Context) (iter.Seq2[*core.Asset, error], error) {
		return p.Assets(ctx)
	})
}

// TestAssetMapping pins one row's Asset: the id is the server's own
// addressing, the content is sorted "name: value" lines of the text-bearing
// columns, the kind and the override are the declared ones, and the cost is
// the shared bytes-over-divisor estimate.
func TestAssetMapping(t *testing.T) {
	t.Parallel()
	p := newPool(t, textSplit(), Options{Kind: "knowledge"})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	assets := collectAssets(t, seq)
	if len(assets) < 4 {
		t.Fatalf("got %d assets, want at least 4", len(assets))
	}
	a := assets[0]

	if a.GetId() != "org/name/main/train@0" {
		t.Errorf("id = %q, want the server's addressing org/name/main/train@0", a.GetId())
	}
	wantContent := "answer: a0\nquestion: q0"
	if string(a.GetContent()) != wantContent {
		t.Errorf("content = %q, want %q (sorted name: value lines, text only)", a.GetContent(), wantContent)
	}
	if a.GetKind() != knov1.Kind_KIND_KNOWLEDGE {
		t.Errorf("kind = %v, want knowledge", a.GetKind())
	}
	if !a.GetUserOverridden() {
		t.Error("UserOverridden must be true: the address declared the kind")
	}
	if a.GetProvenance().GetSource() != "hf" || a.GetProvenance().GetSourceRef() != "org/name/main/train@0" {
		t.Errorf("provenance = %v, want source hf with the id as the ref", a.GetProvenance())
	}
	if a.GetProvenance().GetIngestedAt() != "" {
		t.Errorf("IngestedAt = %q; an unset stamp is what keeps unchanged re-reads identical", a.GetProvenance().GetIngestedAt())
	}
	if a.GetCost() == nil || a.GetCost().GetContextTokens() <= 0 {
		t.Errorf("cost = %v; a non-empty asset must never cost zero tokens", a.GetCost())
	}

	// Row 1's null note is not text; row 2's numeric answer is not text.
	if a2 := assets[2]; strings.Contains(string(a2.GetContent()), "score") {
		t.Errorf("row 2 content %q carries a number as text", a2.GetContent())
	}
	if a1 := assets[1]; strings.Contains(string(a1.GetContent()), "note:") {
		t.Errorf("row 1 content %q carries a null as text", a1.GetContent())
	}
}

// TestContentIsDeterministic: content bytes depend on the row's values and
// nothing else — sorted by column name, so the server's key order cannot
// move the bytes.
func TestContentIsDeterministic(t *testing.T) {
	t.Parallel()
	rows := func(order string) string {
		var body string
		if order == "reversed" {
			body = `{"rows":[{"row_idx":0,"row":{"question":"q","answer":"a","extra":"x"}}],"num_rows_total":1,"partial":false}`
		} else {
			body = `{"rows":[{"row_idx":0,"row":{"extra":"x","answer":"a","question":"q"}}],"num_rows_total":1,"partial":false}`
		}
		return body
	}

	var contents []string
	for _, order := range []string{"canonical", "reversed"} {
		f := newFakeHF()
		f.splitsBody = `{"splits":[{"config":"main","split":"train"}]}`
		f.rowsBodies = map[string]string{"0": rows(order)}
		p := newPool(t, f, Options{})
		seq, err := p.Assets(context.Background())
		if err != nil {
			t.Fatalf("Assets: %v", err)
		}
		assets := collectAssets(t, seq)
		if len(assets) != 1 {
			t.Fatalf("got %d assets, want 1", len(assets))
		}
		contents = append(contents, string(assets[0].GetContent()))
	}
	if contents[0] != contents[1] {
		t.Errorf("content differs with key order:\n%q\n%q", contents[0], contents[1])
	}
	if want := "answer: a\nextra: x\nquestion: q"; contents[0] != want {
		t.Errorf("content = %q, want %q", contents[0], want)
	}
}

func TestKindRefusal(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "bogus", "KNOWLEDGE"} {
		_, err := New(Options{Dataset: "org/name", Config: "main", Split: "train", Kind: kind})
		if err == nil {
			t.Errorf("New accepted kind %q", kind)
			continue
		}
		if !strings.Contains(err.Error(), "kind") {
			t.Errorf("refusal %q does not name the kind", err)
		}
	}

	// The two declared spellings pass.
	for _, kind := range []string{"knowledge", "behavior"} {
		if _, err := New(Options{Dataset: "org/name", Config: "main", Split: "train", Kind: kind}); err != nil {
			t.Errorf("New refused the declared kind %q: %v", kind, err)
		}
	}
}

func TestEmptyPoolIsLegal(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = `{"splits":[{"config":"main","split":"train"}]}`
	f.rowsBodies = map[string]string{"0": `{"rows":[
		{"row_idx":0,"row":{"score":1,"flagged":false}},
		{"row_idx":1,"row":{"count":42,"tag":null}}
	],"num_rows_total":2,"partial":false}`}
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	assets := collectAssets(t, seq)
	if len(assets) != 0 {
		t.Fatalf("got %d assets from a text-free split, want an empty pool", len(assets))
	}
}

func TestLaterRowWithoutTextIsFatal(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = `{"splits":[{"config":"main","split":"train"}]}`
	f.rowsBodies = map[string]string{"0": `{"rows":[
		{"row_idx":0,"row":{"question":"q0","answer":"a0"}},
		{"row_idx":1,"row":{"score":9}}
	],"num_rows_total":2,"partial":false}`}
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a later row without text was accepted")
	}
	if !strings.Contains(got.Error(), "row 1") {
		t.Errorf("refusal %q does not name the row", got)
	}
}

func TestDuplicateIdAcrossPagesIsFatal(t *testing.T) {
	t.Parallel()
	f := textSplit()
	f.rowsBodies["3"] = `{"rows":[{"row_idx":1,"row":{"question":"q1","answer":"a1"}}],"num_rows_total":4,"partial":false}`
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a duplicate id across pages was accepted")
	}
	if !strings.Contains(got.Error(), "org/name/main/train@1") {
		t.Errorf("refusal %q does not name the duplicated id", got)
	}
}

func TestRevisionDriftMidStreamIsFatal(t *testing.T) {
	t.Parallel()
	f := textSplit()
	f.revisions = map[string]string{"3": "zzz999"}
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	got := firstErr(seq)
	if got == nil {
		t.Fatal("a drifting x-revision was accepted")
	}
	if !strings.Contains(got.Error(), "changed while it was being read") {
		t.Errorf("refusal %q does not say the split changed mid-read", got)
	}
}

func TestUnauthorizedOffersBothRemedies(t *testing.T) {
	t.Parallel()
	secret := "test-token-shape-not-a-real-credential"
	f := newFakeHF()
	f.splitsStatus = http.StatusUnauthorized
	p := newPool(t, f, Options{Token: secret})

	_, err := p.Assets(context.Background())
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the refusal echoes the token")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("refusal %q does not name the status and the token remedy", err)
	}
}

func TestPairAbsenceRefusalNamesTheList(t *testing.T) {
	t.Parallel()
	f := newFakeHF()
	f.splitsBody = `{"splits":[{"config":"main","split":"train"}]}`
	p := newPool(t, f, Options{Split: "valid"})

	_, err := p.Assets(context.Background())
	if err == nil {
		t.Fatal("a config/split pair the dataset does not offer was accepted")
	}
	if !strings.Contains(err.Error(), `config "main" split "train"`) {
		t.Errorf("refusal %q does not name the real list", err)
	}
}

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
				Dataset: "org/name", Config: "main", Split: "train", Kind: "knowledge",
				Host: tc.host, Token: "sk-secret",
			})
			if err == nil {
				t.Fatalf("New accepted %s without the allow flags", tc.host)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Error("the refusal echoes the token")
			}
		})
	}
}

func TestMissingRevisionHeaderIsFatal(t *testing.T) {
	t.Parallel()
	f := textSplit()
	f.noRevision = true
	p := newPool(t, f, Options{})

	_, err := p.Assets(context.Background())
	if err == nil {
		t.Fatal("a response without x-revision was accepted")
	}
	if !strings.Contains(err.Error(), "x-revision") {
		t.Errorf("refusal %q does not name the missing header", err)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no dataset", Options{Config: "main", Split: "train", Kind: "knowledge"}, "no dataset"},
		{"no config", Options{Dataset: "org/name", Split: "train", Kind: "knowledge"}, "no config"},
		{"no split", Options{Dataset: "org/name", Config: "main", Kind: "knowledge"}, "no split"},
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

func TestContextTokensUsesTheSharedDivisor(t *testing.T) {
	t.Parallel()
	// The estimate is ceil(bytes / 3.6), the markdown/CSV pool's number
	// (docs/debt.md#68). A one-line check pins the divisor so the pool
	// cannot silently drift to a different cost base.
	if got := contextTokens(36); got != 10 {
		t.Errorf("contextTokens(36) = %d, want 10 (36 / 3.6)", got)
	}
	if got := contextTokens(1); got != 1 {
		t.Errorf("contextTokens(1) = %d, want 1: a non-empty asset never costs zero", got)
	}
}

// TestPaginationAcrossPages keeps the offset accounting honest: the second
// page request resumes at the accumulated row count.
func TestPaginationAcrossPages(t *testing.T) {
	t.Parallel()
	f := textSplit()
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	assets := collectAssets(t, seq)
	if len(assets) != 4 {
		t.Fatalf("got %d assets, want 4", len(assets))
	}
	if id := assets[3].GetId(); id != "org/name/main/train@3" {
		t.Errorf("last id = %q, want org/name/main/train@3", id)
	}
}

// TestAssetIDStabilityAcrossReReads: the same split read twice yields the
// same ids — a pool that had not moved must not look changed on re-read.
func TestAssetIDStabilityAcrossReReads(t *testing.T) {
	t.Parallel()
	f := textSplit()
	p := newPool(t, f, Options{})

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	first := collectAssets(t, seq)
	seq, err = p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets (second read): %v", err)
	}
	second := collectAssets(t, seq)
	if len(first) != len(second) {
		t.Fatalf("re-read served %d assets, first read %d", len(second), len(first))
	}
	for i := range first {
		if first[i].GetId() != second[i].GetId() || string(first[i].GetContent()) != string(second[i].GetContent()) {
			t.Errorf("asset %d differs across reads: %q vs %q", i, first[i].GetId(), second[i].GetId())
		}
	}
}
