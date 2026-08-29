package cli_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestLangfuseToBaselineCarriesTheWeakLabelCount drives a REAL baseline run
// over a Langfuse-shaped dataset server, mixing trace-harvested items
// (sourceObservationId set) with hand-authored ones, and asserts the Run
// records exactly the harvested count as its weak-label number — the same
// invariant mine_e2e_test.go holds for the jsonl/mine path, for the source
// that marks derived per item instead of wholesale.
//
// Serial: the host and keys come from the environment, as they do for the
// shipped CLI.
func TestLangfuseToBaselineCarriesTheWeakLabelCount(t *testing.T) {
	const derived = 10
	total := 40 // enough for both halves at the default 0.2 holdout
	rows := make([]string, 0, total)
	for i := range total {
		id := fmt.Sprintf("lf-%03d", i)
		row := fmt.Sprintf(`{"id":%q,"input":{"question":"q%d"},"expectedOutput":{"answer":"a%d"},"status":"ACTIVE"}`,
			id, i, i)
		if i < derived {
			row = row[:len(row)-1] + `,"sourceObservationId":"obs-` + id + `"}`
		}
		rows = append(rows, row)
	}
	body := fmt.Sprintf(`{"data":[%s],"meta":{"page":1,"limit":100,"totalItems":%d,"totalPages":1}}`,
		strings.Join(rows, ","), total)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/public/v2/datasets/lf-e2e":
			_, _ = io.WriteString(w, `{"id":"ds-e2e","name":"lf-e2e","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-08-01T12:00:00Z"}`)
		case "/api/public/dataset-items":
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LANGFUSE_HOST", srv.URL)
	t.Setenv("LANGFUSE_PUBLIC_KEY", "e2e-pk")
	t.Setenv("LANGFUSE_SECRET_KEY", "e2e-sk")

	db := filepath.Join(t.TempDir(), "kno.db")
	const runID = "langfuse-e2e"
	stdout, stderr, code := run(t, "baseline",
		"--evals", "langfuse:lf-e2e", "--agent", "fake:",
		"--db", db, "--run-id", runID,
		"--allow-insecure-base-url", "--allow-private-address")
	if code != 0 {
		t.Fatalf("baseline exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("weak-label %d of these Cases carry derived provenance", derived)) {
		t.Errorf("the human report does not name the weak-label count:\n%s", stdout)
	}
	if got := readRun(t, db, runID).GetWeakLabelCaseCount(); got != derived {
		t.Errorf("Run.WeakLabelCaseCount = %d, want %d (only trace-harvested items count)", got, derived)
	}

	jsonOut, stderr, code := run(t, "baseline",
		"--evals", "langfuse:lf-e2e", "--agent", "fake:",
		"--db", db, "--run-id", runID+"-json", "--json",
		"--allow-insecure-base-url", "--allow-private-address")
	if code != 0 {
		t.Fatalf("baseline --json exit %d\n%s", code, stderr)
	}
	if !strings.Contains(jsonOut, fmt.Sprintf(`"weak_label_case_count": %d`, derived)) {
		t.Errorf("the JSON report does not carry weak_label_case_count:\n%s", jsonOut)
	}
}
