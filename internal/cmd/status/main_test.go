package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden rewrites docs/status.json. `make update-golden` passes it;
// `make status` is the normal path and writes the same bytes.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestMain runs the generator's tests with an environment holding nothing but
// what the process itself needs.
//
// This is acceptance criterion 12 as a test rather than a claim: `make status`
// contacts no network and resolves no credential, so its tests must pass with
// HOME and every provider variable gone. PATH stays because the generator execs
// python3; TMPDIR because the tests write scratch trees. Same posture and same
// argument as cli/main_test.go's allowlist.
func TestMain(m *testing.M) {
	keep := map[string]string{}
	for _, name := range []string{"PATH", "TMPDIR"} {
		if v, ok := os.LookupEnv(name); ok {
			keep[name] = v
		}
	}
	os.Clearenv()
	for name, v := range keep {
		if err := os.Setenv(name, v); err != nil {
			panic("restoring " + name + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

// repoRoot walks up to the module root, where every path the generator uses is
// rooted.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	start := dir
	for range 8 {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no go.mod above %s", start)
	return ""
}

// scratchTree builds a temp tree holding the only two files the generator
// reads, plus a release manifest. Everything else it reports is compiled in,
// which is exactly the property the release-PR test exists to pin.
func scratchTree(t *testing.T, manifestVersion string) string {
	t.Helper()

	root := repoRoot(t)
	dir := t.TempDir()
	for _, rel := range []string{"docs/debt.md", "scripts/ledger-check.py"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o750); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(rel), err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), body, 0o600); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	writeManifest(t, dir, manifestVersion)
	return dir
}

// writeManifest writes .release-please-manifest.json exactly as release-please
// maintains it.
func writeManifest(t *testing.T, dir, version string) {
	t.Helper()

	body := "{\".\":\"" + version + "\"}\n"
	if err := os.WriteFile(filepath.Join(dir, ".release-please-manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the release manifest: %v", err)
	}
}

// generateIn runs the generator with dir as the working directory and returns
// the bytes it wrote.
func generateIn(t *testing.T, dir string) []byte {
	t.Helper()

	t.Chdir(dir)
	out := filepath.Join(dir, "docs", "status.json")
	if err := run(out, false); err != nil {
		t.Fatalf("generating in %s: %v", dir, err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the generated artifact: %v", err)
	}
	return body
}

// TestAReleasePRProducesNoStatusDiff is the P0's regression guard, and the
// reason docs/status.json carries no released_version.
//
// release-please bumps .release-please-manifest.json in an ORDINARY PR against
// main, and that PR runs `make check` like any other. A field derived from that
// manifest would make the regenerated artifact disagree with the committed one
// the moment the bump landed — so `make status-check` would fail on every
// release, blocking the release it exists to describe. The field is deleted
// rather than automated around, which makes the hazard structurally impossible;
// this test is what keeps it impossible. See
// docs/plans/2026-08-30-kno-status.md, section 7.
func TestAReleasePRProducesNoStatusDiff(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	before := generateIn(t, dir)

	writeManifest(t, dir, "0.1.2")
	after := generateIn(t, dir)

	if string(before) != string(after) {
		t.Errorf("bumping .release-please-manifest.json changed docs/status.json.\n"+
			"Something in the artifact now derives from the release manifest, which "+
			"means `make status-check` fails on every release PR.\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
	if strings.Contains(string(after), "0.1.2") {
		t.Error("the generated artifact contains the manifest's version. It must carry no " +
			"version key at all; the version anchor is the git ref the site fetches at")
	}
}

// TestGeneratedStatusMatchesTheCommittedArtifact is the golden. It is also what
// `make status-check` runs, so a failure here and a red gate say the same thing:
// run `make status` and commit the result.
func TestGeneratedStatusMatchesTheCommittedArtifact(t *testing.T) {
	t.Chdir(repoRoot(t))

	if *updateGolden {
		if err := run(artifact, false); err != nil {
			t.Fatalf("regenerating %s: %v", artifact, err)
		}
	}
	if err := run(artifact, true); err != nil {
		t.Errorf("%v", err)
	}
}

// TestStatusCheckFailsWhenTheArtifactDrifts is the gate watched failing.
// docs/debt.md#16's principle: a gate nobody has seen fail is not a gate.
func TestStatusCheckFailsWhenTheArtifactDrifts(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	body := generateIn(t, dir)

	// Exactly what a hand-edit looks like: one stage's declared state changed
	// in the artifact without the declaration behind it moving.
	drifted := strings.Replace(string(body), `"shipped": "planned"`, `"shipped": "shipped"`, 1)
	if drifted == string(body) {
		t.Fatal("the fixture no longer contains a planned stage to mutate")
	}
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte(drifted), 0o600); err != nil {
		t.Fatalf("writing the drifted artifact: %v", err)
	}

	err := run(filepath.Join(dir, artifact), true)
	if err == nil {
		t.Fatal("status-check passed over a hand-edited artifact")
	}
	if !strings.Contains(err.Error(), "make status") {
		t.Errorf("the failure does not name the fix. what failed → why → fix.\ngot: %v", err)
	}
}

// TestTheGeneratorRefusesAnUnparseableLedgerRow pins the one place this plan
// makes the ledger parser stricter, and pins that it is strict only here: a
// skipped row is an undercount presented as a fact, so the artifact refuses to
// publish over it. The release gate keeps tolerating it deliberately.
func TestTheGeneratorRefusesAnUnparseableLedgerRow(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	t.Chdir(dir)

	ledger := filepath.Join(dir, "docs", "debt.md")
	body, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("reading the fixture ledger: %v", err)
	}
	broken := string(body) + "| 999 | a row with no anchor | why | trigger | owner |\n"
	if err := os.WriteFile(ledger, []byte(broken), 0o600); err != nil {
		t.Fatalf("writing the broken ledger: %v", err)
	}

	_, err = readLedger()
	if err == nil {
		t.Fatal("the generator published a ledger count over an unparseable row")
	}
	if !strings.Contains(err.Error(), "do not parse") {
		t.Errorf("the failure does not say what is wrong.\ngot: %v", err)
	}
}

// TestStatusCheckSaysWhatToDoWhenTheArtifactIsMissing keeps the missing-file
// path actionable rather than a bare open error — the CLI grammar applies to a
// build tool a contributor will meet in a red `make check`.
func TestStatusCheckSaysWhatToDoWhenTheArtifactIsMissing(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	t.Chdir(dir)

	err := run(artifact, true)
	if err == nil {
		t.Fatal("status-check passed with no artifact to compare against")
	}
	if !strings.Contains(err.Error(), "make status") {
		t.Errorf("the failure does not name the fix.\ngot: %v", err)
	}
}

// TestTheGeneratorSaysPython3IsRequired keeps the missing-interpreter failure
// actionable. python3 is already a hard dependency — `make ledger-check` and
// the release workflow both invoke it — so the error says that rather than
// surfacing a bare exec failure.
func TestTheGeneratorSaysPython3IsRequired(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())

	_, err := readLedger()
	if err == nil {
		t.Fatal("readLedger succeeded with no python3 on PATH")
	}
	if !strings.Contains(err.Error(), "python3 is required") {
		t.Errorf("the failure does not name the dependency.\ngot: %v", err)
	}
}

// TestTheGeneratorReportsALedgerScriptFailure covers the other exec path: the
// script ran and refused.
func TestTheGeneratorReportsALedgerScriptFailure(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	t.Chdir(dir)
	replaceLedgerScript(t, dir, "import sys\nsys.exit(3)\n")

	_, err := readLedger()
	if err == nil {
		t.Fatal("readLedger succeeded over a failing ledger script")
	}
	if !strings.Contains(err.Error(), "--json failed") {
		t.Errorf("the failure does not name the script.\ngot: %v", err)
	}
}

// TestTheGeneratorRefusesUndecodableLedgerOutput pins that a malformed reply is
// a failure rather than a zeroed count. A debt figure of 0 printed on a website
// is worse than no figure.
func TestTheGeneratorRefusesUndecodableLedgerOutput(t *testing.T) {
	dir := scratchTree(t, "0.1.1")
	t.Chdir(dir)
	replaceLedgerScript(t, dir, "print('not json')\n")

	_, err := readLedger()
	if err == nil {
		t.Fatal("readLedger succeeded over undecodable output")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("the failure does not say what could not be read.\ngot: %v", err)
	}
}

// replaceLedgerScript swaps the scratch tree's ledger script for a stub.
func replaceLedgerScript(t *testing.T, dir, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, ledgerScript), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the stub ledger script: %v", err)
	}
}
