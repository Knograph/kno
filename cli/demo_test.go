package cli_test

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// updateGolden rewrites the demo transcript golden. `make update-golden` runs
// `go test ./... -update`; the diff is reviewed like code.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// demoGoldenPath is the pinned transcript of `kno demo`, with the glamour-
// rendered report page elided (see splitDemoTranscript).
const demoGoldenPath = "testdata/demo_transcript.golden"

// demoReportPlaceholder stands in for the report page in the golden.
//
// The four plain-text stages and the epilogue are pinned byte for byte. The
// report page is not: it is rendered by glamour, whose output depends on the
// detected color profile, and report_test.go already asserts that page by
// substring for the same reason. Eliding it here keeps the golden a signal
// about kno rather than about the terminal the suite ran in.
const demoReportPlaceholder = "«the report page, asserted by substring below»\n"

const (
	demoReportMarker   = "  # Kno report"
	demoEpilogueMarker = "Demo complete — nothing was spent, and nothing was sent anywhere."
)

// splitDemoTranscript cuts the transcript into the part that is golden-pinned,
// the glamour report page, and the epilogue.
func splitDemoTranscript(t *testing.T, s string) (head, report, tail string) {
	t.Helper()

	i := strings.Index(s, demoReportMarker)
	if i < 0 {
		t.Fatalf("the transcript has no report page:\n%s", s)
	}
	j := strings.Index(s, demoEpilogueMarker)
	if j < 0 {
		t.Fatalf("the transcript has no epilogue:\n%s", s)
	}
	if j < i {
		t.Fatalf("the epilogue printed before the report page:\n%s", s)
	}
	return s[:i], s[i:j], s[j:]
}

// runDemoIn runs `kno demo` with the current directory set to a fresh temp
// directory, which is what makes ./kno-demo — and therefore every path the
// transcript prints — stable.
//
// Not parallel, by construction: t.Chdir is process-global.
func runDemoIn(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Chdir(dir)
	return run(t, append([]string{"demo"}, args...)...)
}

// packageDir is the cli package's own directory, captured BEFORE any t.Chdir.
//
// The golden file and the embedded fixtures live beside the source; the demo
// has to run somewhere else for its output paths to be stable. Resolving them
// once, up front, is what lets both be true.
func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the package directory: %v", err)
	}
	return dir
}

// TestDemoRunsTheWholeLoop is acceptance criteria 1 through 7: one command,
// five stages, files on disk, and an epilogue that says why the numbers look
// the way they do.
func TestDemoRunsTheWholeLoop(t *testing.T) {
	golden := filepath.Join(packageDir(t), demoGoldenPath)
	work := t.TempDir()
	stdout, stderr, code := runDemoIn(t, work)
	if code != errs.ExitOK {
		t.Fatalf("demo exit = %d\nstderr: %s\nstdout: %s", code, stderr, stdout)
	}

	head, report, tail := splitDemoTranscript(t, stdout)
	got := head + demoReportPlaceholder + tail

	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatalf("writing the golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("the demo transcript drifted. Re-run with -update and review the diff.\n"+
			"got:\n%s\nwant:\n%s", got, string(want))
	}

	// Criterion 3's report block, by substring, matching report_test.go's
	// convention for the glamour page.
	for _, phrase := range []string{
		"Value run demo-value (completed)",
		"Baseline demo-baseline (completed)",
		"score **1.000**",
		"Asset verdicts",
		"Rejected, by reason",
		"no-effect",
		"Recorded aggregates only",
	} {
		if !strings.Contains(report, phrase) {
			t.Errorf("the report page no longer says %q:\n%s", phrase, report)
		}
	}

	// Criterion 1: exactly the demo's files, plus SQLite's siblings.
	demoDir := filepath.Join(work, "kno-demo")
	entries, err := os.ReadDir(demoDir)
	if err != nil {
		t.Fatalf("reading %s: %v", demoDir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	want1 := []string{
		".gitignore", ".kno-demo", "cases.jsonl", "kno.db",
		"pool.jsonl", "tuning.jsonl", "tuning.jsonl.manifest.md",
	}
	for _, n := range names {
		if !slices.Contains(want1, n) && !strings.HasPrefix(n, "kno.db-") {
			t.Errorf("the demo left an unexpected file: %s", n)
		}
	}
	for _, n := range want1 {
		if !slices.Contains(names, n) {
			t.Errorf("the demo did not write %s (it wrote %v)", n, names)
		}
	}

	// Criterion 4: nothing was spent, at every stage.
	if strings.Contains(stdout, "spent      $0.00") == false {
		t.Errorf("the baseline block does not report $0.00:\n%s", head)
	}
}

// TestDemoWritesTheEmbeddedFixturesVerbatim is criterion 2, and it is what
// makes the twelve-Case count enforceable: the files on disk are the files in
// the repository, byte for byte.
func TestDemoWritesTheEmbeddedFixturesVerbatim(t *testing.T) {
	demodata := filepath.Join(packageDir(t), "demodata")
	work := t.TempDir()
	if _, stderr, code := runDemoIn(t, work); code != errs.ExitOK {
		t.Fatalf("demo exit = %d\nstderr: %s", code, stderr)
	}

	for _, name := range []string{"cases.jsonl", "pool.jsonl"} {
		embedded, err := os.ReadFile(filepath.Join(demodata, name))
		if err != nil {
			t.Fatalf("reading the embedded %s: %v", name, err)
		}
		written, err := os.ReadFile(filepath.Join(work, "kno-demo", name))
		if err != nil {
			t.Fatalf("reading the written %s: %v", name, err)
		}
		if string(embedded) != string(written) {
			t.Errorf("%s on disk differs from cli/demodata/%s", name, name)
		}
	}

	// The count is load-bearing: at nine Cases the control arm rounds to one
	// and every interval comes back nil, at which point the epilogue's claim
	// that the deltas arrive WITH their intervals stops being true. See
	// cli/demodata/README.md.
	cases, err := os.ReadFile(filepath.Join(demodata, "cases.jsonl"))
	if err != nil {
		t.Fatalf("reading the embedded cases: %v", err)
	}
	if n := len(strings.Fields(strings.TrimSpace(string(cases)))); n == 0 {
		t.Fatal("the embedded eval set is empty")
	}
	if n := strings.Count(strings.TrimSpace(string(cases)), "\n") + 1; n != 12 {
		t.Errorf("the embedded eval set has %d Cases, not 12; the epilogue's intervals "+
			"depend on this count (cli/demodata/README.md)", n)
	}
}

// TestDemoJSONIsOneDocument is criteria 8, 4, 5 and 6, plus the human/--json
// equivalence golden: the caveats a human reads must be the caveats a jq
// pipeline reads.
//
// The decoding goes through cli.DecodeDemo* rather than encoding/json here:
// the CLI's depguard exemption is scoped to jsonreport.go, and the stage
// documents are parsed as the very structs the stages render, which is the
// assertion worth making.
func TestDemoJSONIsOneDocument(t *testing.T) {
	work := t.TempDir()
	stdout, stderr, code := runDemoIn(t, work, "--json")
	if code != errs.ExitOK {
		t.Fatalf("demo --json exit = %d\nstderr: %s", code, stderr)
	}

	doc, err := cli.DecodeDemoReport([]byte(stdout))
	if err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if doc.Agent != "fake:" {
		t.Errorf("the document reports agent %q", doc.Agent)
	}
	for name, raw := range map[string][]byte{
		"baseline": doc.Stages.Baseline,
		"value":    doc.Stages.Value,
		"select":   doc.Stages.Select,
		"export":   doc.Stages.Export,
		"report":   doc.Stages.Report,
	} {
		if len(raw) == 0 {
			t.Errorf("the document has no %s stage", name)
		}
	}

	base, err := cli.DecodeDemoBaseline(doc.Stages.Baseline)
	if err != nil {
		t.Fatalf("the baseline stage is not a `kno baseline --json` document: %v", err)
	}
	if base.SpentUSD != "$0.00" {
		t.Errorf("the baseline stage reports spend %q", base.SpentUSD)
	}
	if base.Score == nil || *base.Score != 1 {
		t.Errorf("the baseline score is %v, not 1.000", base.Score)
	}
	if base.DevCases != 8 || base.Holdout != 4 {
		t.Errorf("the split is %d dev / %d holdout, not 8 / 4", base.DevCases, base.Holdout)
	}
	if !slices.ContainsFunc(base.Warnings, func(w string) bool {
		return strings.Contains(w, "holdout has only 4 cases")
	}) {
		t.Errorf("the too-small-holdout warning is missing: %v", base.Warnings)
	}

	// Criterion 6: every delta is exactly zero AND carries an interval. The
	// interval is what lets select say "no effect" rather than the weaker
	// "underpowered", and it is what the twelve-Case fixture size buys.
	val, err := cli.DecodeDemoValue(doc.Stages.Value)
	if err != nil {
		t.Fatalf("the value stage is not a `kno value --json` document: %v", err)
	}
	if n := len(val.Valuations); n != 3 {
		t.Fatalf("the value run measured %d Assets, not 3", n)
	}
	for _, v := range val.Valuations {
		if v.DeltaGoal == nil || *v.DeltaGoal != 0 {
			t.Errorf("%s measured a non-zero delta (%v); injection delegates unchanged, "+
				"so this cannot happen without the demo having been staged",
				v.AssetID, v.DeltaGoal)
		}
		if v.Low == nil || v.High == nil {
			t.Errorf("%s has no interval, so the epilogue's claim that the intervals are "+
				"real is false; the Case count is too small (cli/demodata/README.md)",
				v.AssetID)
		}
	}

	sel, err := cli.DecodeDemoSelect(doc.Stages.Select)
	if err != nil {
		t.Fatalf("the select stage is not a `kno select --json` document: %v", err)
	}
	if n := len(sel.Selected); n != 0 {
		t.Errorf("the Portfolio selected %d Assets; an empty Portfolio is the correct "+
			"answer here, and a non-empty one means somebody staged the demo", n)
	}
	if n := len(sel.Rejected); n != 3 {
		t.Fatalf("the rejection log has %d entries, not 3", n)
	}
	for _, r := range sel.Rejected {
		if r.Reason != "no-effect" {
			t.Errorf("%s was rejected as %q, not no-effect; \"underpowered\" would mean "+
				"the intervals collapsed and the epilogue is lying", r.AssetID, r.Reason)
		}
		if !strings.Contains(r.Detail, "crosses zero") {
			t.Errorf("%s's rejection does not say why: %q", r.AssetID, r.Detail)
		}
	}

	// The equivalence: the same three sentences, in the same order, in both
	// renderings. The human epilogue wraps them, so it is compared with the
	// whitespace collapsed — the wrap is the only thing allowed to differ.
	humanWork := t.TempDir()
	humanOut, _, humanCode := runDemoIn(t, humanWork)
	if humanCode != errs.ExitOK {
		t.Fatalf("the human demo exit = %d", humanCode)
	}
	flat := strings.Join(strings.Fields(humanOut), " ")
	if len(doc.Notes) != 3 {
		t.Fatalf("the document carries %d notes, not the three honesty sentences", len(doc.Notes))
	}
	for _, note := range doc.Notes {
		if !strings.Contains(flat, strings.Join(strings.Fields(note), " ")) {
			t.Errorf("the human epilogue does not carry the note %q", note)
		}
	}
	if !strings.Contains(flat, strings.Join(strings.Fields(doc.Config), " ")) {
		t.Errorf("the human epilogue does not carry %q", doc.Config)
	}
	for _, step := range doc.NextSteps {
		if !strings.Contains(flat, strings.Join(strings.Fields(step), " ")) {
			t.Errorf("the human epilogue does not carry the next step %q", step)
		}
	}
	if !strings.Contains(flat, doc.Cleanup) {
		t.Errorf("the human epilogue does not carry %q", doc.Cleanup)
	}
	for _, path := range doc.Files {
		if !strings.Contains(flat, path) {
			t.Errorf("the human epilogue does not list %q", path)
		}
	}
}

// TestDemoRefusesASecondRun is criterion 11.
func TestDemoRefusesASecondRun(t *testing.T) {
	work := t.TempDir()
	if _, stderr, code := runDemoIn(t, work); code != errs.ExitOK {
		t.Fatalf("the first demo exit = %d\nstderr: %s", code, stderr)
	}

	_, stderr, code := run(t, "demo")
	if code != errs.ExitError {
		t.Fatalf("a second demo exit = %d, want %d", code, errs.ExitError)
	}
	for _, want := range []string{"already exists", "--force", "--dir"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
}

// TestDemoForceReplacesOnlyItsOwnFiles is criteria 12 and 20.
//
// The marker proves the demo CREATED the directory. It does not prove the demo
// owns everything now in it, and the demo directory is precisely a place we
// invite people to work in.
func TestDemoForceReplacesOnlyItsOwnFiles(t *testing.T) {
	work := t.TempDir()
	first, stderr, code := runDemoIn(t, work)
	if code != errs.ExitOK {
		t.Fatalf("the first demo exit = %d\nstderr: %s", code, stderr)
	}

	demoDir := filepath.Join(work, "kno-demo")
	notes := filepath.Join(demoDir, "notes.md")
	const kept = "my own scratch file\n"
	if err := os.WriteFile(notes, []byte(kept), 0o600); err != nil {
		t.Fatalf("writing notes.md: %v", err)
	}

	second, stderr, code := run(t, "demo", "--force")
	if code != errs.ExitOK {
		t.Fatalf("demo --force exit = %d\nstderr: %s", code, stderr)
	}

	got, err := os.ReadFile(notes)
	if err != nil {
		t.Fatalf("notes.md did not survive --force: %v", err)
	}
	if string(got) != kept {
		t.Errorf("notes.md was rewritten: %q", string(got))
	}
	if !strings.Contains(second, filepath.Join("kno-demo", "notes.md")+" left in place") {
		t.Errorf("--force did not name the file it left behind:\n%s", second)
	}

	// Criterion 12: byte-identical, once the pre-run line about notes.md is
	// removed. Fixed run IDs and a deterministic agent leave nothing else to
	// vary.
	_, _, firstTail := splitDemoTranscript(t, first)
	_, _, secondTail := splitDemoTranscript(t, second)
	if firstTail != secondTail {
		t.Errorf("the second run's epilogue drifted.\nfirst:\n%s\nsecond:\n%s",
			firstTail, secondTail)
	}
	firstHead, _, _ := splitDemoTranscript(t, first)
	secondHead, _, _ := splitDemoTranscript(t, second)
	secondHead = strings.TrimPrefix(secondHead,
		filepath.Join("kno-demo", "notes.md")+" left in place\n")
	if firstHead != secondHead {
		t.Errorf("the second run's stages drifted.\nfirst:\n%s\nsecond:\n%s",
			firstHead, secondHead)
	}

	// Idempotence extends to the artifact.
	tuning := filepath.Join(demoDir, "tuning.jsonl")
	before, err := os.ReadFile(tuning)
	if err != nil {
		t.Fatalf("reading tuning.jsonl: %v", err)
	}
	if _, _, code := run(t, "demo", "--force"); code != errs.ExitOK {
		t.Fatalf("a third demo --force exit = %d", code)
	}
	after, err := os.ReadFile(tuning)
	if err != nil {
		t.Fatalf("re-reading tuning.jsonl: %v", err)
	}
	if string(before) != string(after) {
		t.Error("tuning.jsonl is not byte-identical across two --force runs")
	}
}

// TestDemoForceRefusesADirectoryItDidNotCreate is criterion 13: --force never
// deletes a directory the demo did not make, marker absent.
func TestDemoForceRefusesADirectoryItDidNotCreate(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	target := filepath.Join(work, "not-ours")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatalf("creating the target: %v", err)
	}
	sentinel := filepath.Join(target, "important.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete\n"), 0o600); err != nil {
		t.Fatalf("writing the sentinel: %v", err)
	}

	_, stderr, code := run(t, "demo", "--force", "--dir", target)
	if code != errs.ExitError {
		t.Fatalf("exit = %d, want %d", code, errs.ExitError)
	}
	if !strings.Contains(stderr, ".kno-demo") {
		t.Errorf("the refusal does not name the marker:\n%s", stderr)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("--force deleted a file in a directory it did not create: %v", err)
	}
}

// TestDemoRefusesTheWorkingDirectory is criterion 14.
func TestDemoRefusesTheWorkingDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	_, stderr, code := run(t, "demo", "--dir", ".")
	if code != errs.ExitError {
		t.Fatalf("exit = %d, want %d", code, errs.ExitError)
	}
	if !strings.Contains(stderr, "current directory") {
		t.Errorf("the refusal does not say why:\n%s", stderr)
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Errorf("the refusal wrote into the working directory: %v %v", entries, err)
	}
}

// TestDemoHelpIsSnapshotted is criterion 16.
func TestDemoHelpIsSnapshotted(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "demo", "--help")
	if code != errs.ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{
		"fake:",
		"spends nothing",
		"./kno-demo",
		"reads no configuration",
		"--dir",
		"--force",
		"--json",
		// The honesty promise belongs in the help too: someone deciding
		// whether to run this should know the score reads 1.000 before it
		// does.
		"1.000",
		"rm -rf kno-demo",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help no longer mentions %q:\n%s", want, stdout)
		}
	}
	// The flags the demo deliberately does not accept.
	for _, notWant := range []string{"--agent", "--yes", "--config", "--keep", "--db "} {
		if strings.Contains(stdout, notWant) {
			t.Errorf("the demo accepts %q, which it must not:\n%s", notWant, stdout)
		}
	}
}

// TestDemoWritesItsOwnGitignore is criterion 21.
//
// The repository's own .gitignore covers *.db and training_set*.jsonl and
// nothing that matches kno-demo/{cases,pool,tuning}.jsonl, so ignoring these
// in THIS repo would help only kno's own developers. A new user runs the demo
// inside their own repository, which is why the demo writes the file itself.
func TestDemoWritesItsOwnGitignore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "demo@example.invalid"},
		{"config", "user.name", "demo"},
	} {
		cmd := exec.CommandContext(t.Context(), git, args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v\n%s", args, err, out)
		}
	}

	if _, stderr, code := runDemoIn(t, work); code != errs.ExitOK {
		t.Fatalf("demo exit = %d\nstderr: %s", code, stderr)
	}

	ignore, err := os.ReadFile(filepath.Join(work, "kno-demo", ".gitignore"))
	if err != nil {
		t.Fatalf("the demo wrote no .gitignore: %v", err)
	}
	if strings.TrimSpace(string(ignore)) != "*" {
		t.Errorf("the .gitignore is %q, not `*`", string(ignore))
	}

	status := exec.CommandContext(t.Context(), git, "status", "--porcelain")
	status.Dir = work
	out, err := status.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("the demo left untracked files in `git status`:\n%s", out)
	}
}

// TestDemoNeverReadsTheHoldout is the holdout canary: four of the twelve Cases
// are sealed at ingestion, and the demo runs no validate, so no holdout Case
// ID may appear in anything it produces.
func TestDemoNeverReadsTheHoldout(t *testing.T) {
	work := t.TempDir()
	stdout, _, code := runDemoIn(t, work, "--json")
	if code != errs.ExitOK {
		t.Fatalf("demo exit = %d", code)
	}

	// The scored Case IDs, read from the store the demo wrote.
	scored := demoScoredCaseIDs(t, filepath.Join(work, "kno-demo", "kno.db"))
	if len(scored) != 8 {
		t.Fatalf("the demo scored %d Cases, not the 8 dev half: %v", len(scored), scored)
	}

	all := demoCaseIDs(t, filepath.Join(work, "kno-demo", "cases.jsonl"))
	tuning, err := os.ReadFile(filepath.Join(work, "kno-demo", "tuning.jsonl"))
	if err != nil {
		t.Fatalf("reading tuning.jsonl: %v", err)
	}
	for _, id := range all {
		if slices.Contains(scored, id) {
			continue
		}
		// A held-back Case. It may not appear in any output.
		if strings.Contains(stdout, id) {
			t.Errorf("the holdout Case %q appears in the demo's output", id)
		}
		if strings.Contains(string(tuning), id) {
			t.Errorf("the holdout Case %q appears in tuning.jsonl", id)
		}
	}
}

// TestDemoMakesNoNetworkConnection is criterion 4's second half.
//
// fake: constructs no transport, so this must hold; the assertion is what
// stops a future "let's fetch the sample data" change. The bound is net/http,
// which is how every adapter in this tree reaches an endpoint.
func TestDemoMakesNoNetworkConnection(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })

	var dialed []string
	http.DefaultTransport = &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, network+" "+addr)
			return nil, errors.New("the demo must not open a connection")
		},
		ResponseHeaderTimeout: time.Second,
	}

	work := t.TempDir()
	if _, stderr, code := runDemoIn(t, work); code != errs.ExitOK {
		t.Fatalf("demo exit = %d\nstderr: %s", code, stderr)
	}
	if len(dialed) > 0 {
		t.Errorf("the demo opened connections: %v", dialed)
	}
}

// demoCaseIDs reads the ids out of a JSONL eval file.
//
// By hand rather than with encoding/json: the CLI's exemption is scoped to
// jsonreport.go, and one field of a fixture this test owns does not justify
// widening it.
func demoCaseIDs(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		_, after, ok := strings.Cut(line, `"id":"`)
		if !ok {
			t.Fatalf("%s has a line with no id: %s", path, line)
		}
		id, _, ok := strings.Cut(after, `"`)
		if !ok {
			t.Fatalf("%s has an unterminated id: %s", path, line)
		}
		ids = append(ids, id)
	}
	return ids
}

// demoScoredCaseIDs reads the Case IDs the run actually scored, from the store
// the demo wrote — the only record of which half of the split was touched.
func demoScoredCaseIDs(t *testing.T, dbPath string) []string {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(t.Context(),
		`SELECT case_id FROM outcomes WHERE run_id = ?`, "demo-baseline")
	if err != nil {
		t.Fatalf("reading outcomes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning an outcome: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading outcomes: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// TestDemoInterruptedLeavesNoFalseEpilogue is criterion 15.
//
// The cancellation lands in the gap between stages, which is the guard this
// command owns: mid-stage interruption is core's checkpointing, and the
// stages' own tests cover it. What matters here is the pair of properties an
// interrupted demo must have — exit 4, and no epilogue. An epilogue after a
// failure would claim a completed loop.
func TestDemoInterruptedLeavesNoFalseEpilogue(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out, errOut strings.Builder
	code := cli.Execute(ctx, []string{"demo"}, strings.NewReader(""), &out, &errOut)
	if code != errs.ExitInterrupted {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s",
			code, errs.ExitInterrupted, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), demoEpilogueMarker) {
		t.Errorf("the epilogue printed after an interruption:\n%s", out.String())
	}

	// The directory stays: an interrupted demo leaves what it wrote, and the
	// fixtures are the part worth keeping.
	for _, name := range []string{"cases.jsonl", "pool.jsonl", ".kno-demo", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(work, "kno-demo", name)); err != nil {
			t.Errorf("the interrupted demo did not leave %s in place: %v", name, err)
		}
	}
	// Nothing partial is presented as complete: the export never ran.
	if _, err := os.Stat(filepath.Join(work, "kno-demo", "tuning.jsonl")); err == nil {
		t.Error("an interrupted demo produced a tuning set")
	}
}
