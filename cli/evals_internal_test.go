package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/langfuse"
	"github.com/knograph/kno/adapters/evals/langsmith"
)

// TestEvalsGrammarBarePathIsJSONL: a path with no prefix is the jsonl
// adapter, unchanged.
func TestEvalsGrammarBarePathIsJSONL(t *testing.T) {
	t.Parallel()
	f := &baselineFlags{
		evalsPath:   filepath.Join(t.TempDir(), "cases.jsonl"),
		holdoutFrac: 0.2,
	}
	ev, err := resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals: %v", err)
	}
	if _, ok := ev.(*jsonl.Evals); !ok {
		t.Fatalf("resolveEvals(bare path) = %T, want *jsonl.Evals", ev)
	}
}

// TestEvalsGrammarLangsmithPrefix: the langsmith: prefix selects the
// LangSmith adapter, and the endpoint-security opt-out flags reach it: the
// resolver's own result must CountSplits against a plain-HTTP loopback
// endpoint, which the adapter refuses without both flags.
//
// Serial: the endpoint and key come from the environment, as they do for the
// shipped CLI.
func TestEvalsGrammarLangsmithPrefix(t *testing.T) {
	srv := newEvalsServer(t)
	t.Setenv(langsmith.EndpointEnv, srv.URL)
	t.Setenv(langsmith.DefaultKeyEnv, "test-key")
	f := &baselineFlags{
		evalsPath:           "langsmith:support-llm",
		holdoutFrac:         0.3,
		splitSeed:           "seed-1",
		allowInsecureURL:    true,
		allowPrivateAddress: true,
	}
	ev, err := resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals: %v", err)
	}
	ls, ok := ev.(*langsmith.Evals)
	if !ok {
		t.Fatalf("resolveEvals(langsmith:) = %T, want *langsmith.Evals", ev)
	}
	counts, err := ls.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 2 {
		t.Errorf("Total() = %d, want 2", counts.Total())
	}
}

// TestEvalsLangsmithCountsAgainstEndpoint drives a real resolveEvals result
// through CountSplits against a fake LangSmith API.
//
// Serial: the endpoint and key come from the environment, as they do for the
// shipped CLI.
func TestEvalsLangsmithCountsAgainstEndpoint(t *testing.T) {
	srv := newEvalsServer(t)
	t.Setenv(langsmith.EndpointEnv, srv.URL)
	t.Setenv(langsmith.DefaultKeyEnv, "test-key")
	f := &baselineFlags{
		evalsPath:           "langsmith:support-llm",
		allowInsecureURL:    true,
		allowPrivateAddress: true,
	}
	ev, err := resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals: %v", err)
	}
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 2 {
		t.Errorf("Total() = %d, want 2", counts.Total())
	}
	if err := counts.Validate(); err == nil {
		t.Error("a 2-case eval set must not validate")
	}
}

// TestEvalsLangsmithMissingKeyIsRefused: the key is environment-only, and a
// missing one is refused at resolve time with the variable named.
//
// Serial: binds the environment.
func TestEvalsLangsmithMissingKeyIsRefused(t *testing.T) {
	t.Setenv(langsmith.EndpointEnv, "")
	t.Setenv(langsmith.DefaultKeyEnv, "")
	f := &baselineFlags{evalsPath: "langsmith:support-llm"}
	_, err := resolveEvals(f)
	if err == nil {
		t.Fatal("a missing key was accepted")
	}
	if !strings.Contains(err.Error(), langsmith.DefaultKeyEnv) {
		t.Errorf("error %q does not name %s", err, langsmith.DefaultKeyEnv)
	}
}

// TestEvalsLangsmithEmptyNameIsRefused: the grammar demands a name after the
// prefix.
func TestEvalsLangsmithEmptyNameIsRefused(t *testing.T) {
	t.Setenv(langsmith.DefaultKeyEnv, "test-key")
	f := &baselineFlags{evalsPath: "langsmith:"}
	_, err := resolveEvals(f)
	if err == nil {
		t.Fatal("an empty dataset name was accepted")
	}
}

// newEvalsServer serves the two LangSmith envelope shapes a CountSplits pass
// needs, over plain HTTP on loopback (hence the opt-in flags everywhere).
func newEvalsServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/datasets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":"ds-1","name":"support-llm",`+
			`"modified_at":"2026-08-01T12:00:00Z","example_count":2}],"next_cursor":""}`)
	})
	mux.HandleFunc("/examples", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[`+
			`{"id":"ex-1","inputs":{"question":"q1"},"outputs":{"answer":"a1"}},`+
			`{"id":"ex-2","inputs":{"question":"q2"},"outputs":{"answer":"a2"}}`+
			`],"next_cursor":""}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestEvalsGrammarLangfusePrefix: the langfuse: prefix selects the Langfuse
// adapter, and the endpoint-security opt-out flags reach it: the resolver's
// own result must CountSplits against a plain-HTTP loopback endpoint, which
// the adapter refuses without both flags.
//
// Serial: the host and keys come from the environment, as they do for the
// shipped CLI.
func TestEvalsGrammarLangfusePrefix(t *testing.T) {
	srv := newLangfuseEvalsServer(t, false)
	t.Setenv(langfuse.HostEnv, srv.URL)
	t.Setenv(langfuse.PublicKeyEnv, "test-pk")
	t.Setenv(langfuse.SecretKeyEnv, "test-sk")
	f := &baselineFlags{
		evalsPath:           "langfuse:support-llm",
		holdoutFrac:         0.3,
		splitSeed:           "seed-1",
		allowInsecureURL:    true,
		allowPrivateAddress: true,
	}
	ev, err := resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals: %v", err)
	}
	lf, ok := ev.(*langfuse.Evals)
	if !ok {
		t.Fatalf("resolveEvals(langfuse:) = %T, want *langfuse.Evals", ev)
	}
	counts, err := lf.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 2 {
		t.Errorf("Total() = %d, want 2", counts.Total())
	}
}

// TestEvalsLangfuseCountsAgainstEndpoint drives a real resolveEvals result
// through CountSplits against a fake Langfuse API, and refuses the unknown
// dataset with an actionable error before any page is fetched.
//
// Serial: the host and keys come from the environment.
func TestEvalsLangfuseCountsAgainstEndpoint(t *testing.T) {
	srv := newLangfuseEvalsServer(t, false)
	t.Setenv(langfuse.HostEnv, srv.URL)
	t.Setenv(langfuse.PublicKeyEnv, "test-pk")
	t.Setenv(langfuse.SecretKeyEnv, "test-sk")
	f := &baselineFlags{
		evalsPath:           "langfuse:support-llm",
		allowInsecureURL:    true,
		allowPrivateAddress: true,
	}
	ev, err := resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals: %v", err)
	}
	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() != 2 {
		t.Errorf("Total() = %d, want 2", counts.Total())
	}
	if err := counts.Validate(); err == nil {
		t.Error("a 2-case eval set must not validate")
	}

	// The 404 path: a typo'd dataset name is refused at the first read —
	// CountSplits, which is where a baseline run meets the dataset — naming
	// the dataset, not answered with an empty page. The construction is lazy
	// (New resolves nothing); the refusal is what the run surfaces.
	srvUnknown := newLangfuseEvalsServer(t, true)
	t.Setenv(langfuse.HostEnv, srvUnknown.URL)
	f = &baselineFlags{
		evalsPath:           "langfuse:no-such-dataset",
		allowInsecureURL:    true,
		allowPrivateAddress: true,
	}
	ev, err = resolveEvals(f)
	if err != nil {
		t.Fatalf("resolveEvals must not resolve the dataset: %v", err)
	}
	if _, err := ev.CountSplits(context.Background()); err == nil {
		t.Fatal("an unknown dataset was accepted")
	} else if !strings.Contains(err.Error(), "no dataset named") {
		t.Errorf("error %q does not say the dataset is missing", err)
	}
}

// TestEvalsLangfuseMissingKeysAreRefused: the keys are environment-only, and
// a missing pair is refused at resolve time with both variables named.
//
// Serial: binds the environment.
func TestEvalsLangfuseMissingKeysAreRefused(t *testing.T) {
	t.Setenv(langfuse.HostEnv, "")
	t.Setenv(langfuse.PublicKeyEnv, "")
	t.Setenv(langfuse.SecretKeyEnv, "")
	f := &baselineFlags{evalsPath: "langfuse:support-llm"}
	_, err := resolveEvals(f)
	if err == nil {
		t.Fatal("missing keys were accepted")
	}
	for _, name := range []string{langfuse.PublicKeyEnv, langfuse.SecretKeyEnv} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

// TestEvalsLangfuseEmptyNameIsRefused: the grammar demands a name after the
// prefix.
func TestEvalsLangfuseEmptyNameIsRefused(t *testing.T) {
	t.Setenv(langfuse.PublicKeyEnv, "test-pk")
	t.Setenv(langfuse.SecretKeyEnv, "test-sk")
	f := &baselineFlags{evalsPath: "langfuse:"}
	_, err := resolveEvals(f)
	if err == nil {
		t.Fatal("an empty dataset name was accepted")
	}
}

// newLangfuseEvalsServer serves the two Langfuse API shapes a CountSplits
// pass needs — the bare v2 dataset object and the {data, meta} items
// envelope — over plain HTTP on loopback (hence the opt-in flags
// everywhere). When notFound, every datasets request 404s, as the real API
// does for an unknown name.
func newLangfuseEvalsServer(t *testing.T, notFound bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/public/v2/datasets/support-llm", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"ds-1","name":"support-llm",`+
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-08-01T12:00:00Z"}`)
	})
	mux.HandleFunc("/api/public/dataset-items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[`+
			`{"id":"it-1","input":{"question":"q1"},"expectedOutput":{"answer":"a1"},"status":"ACTIVE"},`+
			`{"id":"it-2","input":{"question":"q2"},"expectedOutput":{"answer":"a2"},"status":"ACTIVE"}`+
			`],"meta":{"page":1,"limit":100,"totalItems":2,"totalPages":1}}`)
	})
	if notFound {
		mux.HandleFunc("/api/public/v2/datasets/no-such-dataset", func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
