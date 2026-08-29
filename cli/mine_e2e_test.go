package cli_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// readRun fetches the stored Run with the given id, the way the CLI's own
// store reads it back on resume.
func readRun(t *testing.T, dbPath, runID string) *knov1.Run {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	var blob []byte
	if err := db.QueryRow(`SELECT proto FROM runs WHERE id = ?`, runID).Scan(&blob); err != nil {
		t.Fatalf("reading run: %v", err)
	}
	var r knov1.Run
	if err := proto.Unmarshal(blob, &r); err != nil {
		t.Fatalf("unmarshaling run: %v", err)
	}
	return &r
}

// TestMineToBaselineCarriesTheWeakLabelCount is the mine workstream's end
// to end: mined Cases flow through the jsonl adapter's derived provenance
// into the split counts, into the Run's WeakLabelCaseCount, and into both
// renderers — the human report line and the --json field. A run that
// measured mined Cases must be able to say so.
//
// The transcript is generated large enough (30 exchanges) that a holdout
// actually exists at the default fraction — the split refuses an empty
// holdout, which is itself the point of keeping the weak-label count on
// the Run: it is recorded for a run that measured something.
func TestMineToBaselineCarriesTheWeakLabelCount(t *testing.T) {
	t.Parallel()
	const n = 30
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	var b strings.Builder
	for i := 0; i < n; i++ {
		thread := "t" + string(rune('0'+i%10)) + string(rune('0'+(i/10)%10))
		b.WriteString(`{"id":"q` + thread + `","role":"user","content":"Where is invoice ` + thread + `?","thread_id":"` + thread + `"}` + "\n")
		b.WriteString(`{"id":"a` + thread + `","role":"assistant","content":"It is in the mail."}` + "\n")
		b.WriteString(`{"id":"c` + thread + `","role":"user","content":"It should arrive by Tuesday."}` + "\n")
	}
	if err := os.WriteFile(transcript, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing transcript: %v", err)
	}

	cases := filepath.Join(dir, "mined.jsonl")
	db := filepath.Join(dir, "kno.db")

	stdout, stderr, code := run(t, "mine",
		"--logs", transcript, "--format", "jsonl-chat", "--out", cases)
	if code != 0 {
		t.Fatalf("mine exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "mined 30 Cases") {
		t.Errorf("mine does not report the yield:\n%s", stdout)
	}

	const runID = "mine-e2e"
	stdout, stderr, code = run(t, "baseline",
		"--evals", cases, "--agent", "fake:", "--db", db, "--run-id", runID)
	if code != 0 {
		t.Fatalf("baseline exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "weak-label 30 of these Cases carry derived provenance") {
		t.Errorf("the human report does not name the weak-label count:\n%s", stdout)
	}
	if got := readRun(t, db, runID).GetWeakLabelCaseCount(); got != n {
		t.Errorf("Run.WeakLabelCaseCount = %d, want %d", got, n)
	}

	jsonOut, stderr, code := run(t, "baseline",
		"--evals", cases, "--agent", "fake:", "--db", db, "--run-id", runID+"-json", "--json")
	if code != 0 {
		t.Fatalf("baseline --json exit %d\n%s", code, stderr)
	}
	if !strings.Contains(jsonOut, `"weak_label_case_count": 30`) {
		t.Errorf("the JSON report does not carry weak_label_case_count:\n%s", jsonOut)
	}
}
