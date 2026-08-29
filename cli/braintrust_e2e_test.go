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

// TestBraintrustToBaselineCarriesTheWeakLabelCount drives a REAL baseline
// run over a Braintrust-shaped dataset server, mixing copied events
// (origin set) with authored ones, and asserts the Run records exactly the
// copied count as its weak-label number — the same invariant
// langfuse_e2e_test.go holds for the trace-harvested shape, for the source
// whose copy marker is Braintrust's origin object.
//
// Serial: the host and key come from the environment, as they do for the
// shipped CLI.
func TestBraintrustToBaselineCarriesTheWeakLabelCount(t *testing.T) {
	const derived = 10
	total := 40 // enough for both halves at the default 0.2 holdout
	rows := make([]string, 0, total)
	for i := range total {
		id := fmt.Sprintf("bt-%03d", i)
		row := fmt.Sprintf(`{"id":%q,"input":{"question":"q%d"},"expected":{"answer":"a%d"},"_xact_id":%q}`,
			id, i, i, fmt.Sprintf("%d", i+1))
		if i < derived {
			row = row[:len(row)-1] + `,"origin":{"object_type":"experiment","object_id":"exp-` + id + `","_xact_id":"1"}}`
		}
		rows = append(rows, row)
	}
	body := fmt.Sprintf(`{"events":[%s],"cursor":""}`, strings.Join(rows, ","))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dataset":
			_, _ = io.WriteString(w, `[{"id":"ds-e2e","name":"bt-e2e","project_id":"p-e2e"}]`)
		case "/v1/dataset/ds-e2e/fetch":
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("BRAINTRUST_API_BASE_URL", srv.URL)
	t.Setenv("BRAINTRUST_API_KEY", "e2e-key")

	db := filepath.Join(t.TempDir(), "kno.db")
	const runID = "braintrust-e2e"
	stdout, stderr, code := run(t, "baseline",
		"--evals", "braintrust:bt-e2e", "--agent", "fake:",
		"--db", db, "--run-id", runID,
		"--allow-insecure-base-url", "--allow-private-address")
	if code != 0 {
		t.Fatalf("baseline exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("weak-label %d of these Cases carry derived provenance", derived)) {
		t.Errorf("the human report does not name the weak-label count:\n%s", stdout)
	}
	if got := readRun(t, db, runID).GetWeakLabelCaseCount(); got != derived {
		t.Errorf("Run.WeakLabelCaseCount = %d, want %d (only copied events count)", got, derived)
	}

	jsonOut, stderr, code := run(t, "baseline",
		"--evals", "braintrust:bt-e2e", "--agent", "fake:",
		"--db", db, "--run-id", runID+"-json", "--json",
		"--allow-insecure-base-url", "--allow-private-address")
	if code != 0 {
		t.Fatalf("baseline --json exit %d\n%s", code, stderr)
	}
	if !strings.Contains(jsonOut, fmt.Sprintf(`"weak_label_case_count": %d`, derived)) {
		t.Errorf("the JSON report does not carry weak_label_case_count:\n%s", jsonOut)
	}
}
