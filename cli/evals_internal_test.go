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
