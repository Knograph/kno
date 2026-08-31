package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// testDebt is a fixed ledger reading. The real counts come from
// scripts/ledger-check.py at generation time; nothing here should depend on the
// ledger's current size, or every debt-touching PR would edit this file too.
var testDebt = StatusDebt{Total: 42, Open: 7}

// repoRoot walks up from the package directory to the module root.
//
// The README and the committed artifact live there, and a test that resolved
// them relative to the working directory would pass or fail depending on how
// `go test` was invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the package directory: %v", err)
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

// TestEveryStageIsDeclared is section 4's check 1: the declaration names every
// Stage the proto carries, so adding a stage without deciding its status fails
// rather than silently omitting it from the artifact.
func TestEveryStageIsDeclared(t *testing.T) {
	t.Parallel()

	declared := map[knov1.Stage]int{}
	for _, f := range stageFacts() {
		declared[f.Stage]++
	}

	for value, name := range knov1.Stage_name {
		if knov1.Stage(value) == knov1.Stage_STAGE_UNSPECIFIED {
			continue
		}
		switch declared[knov1.Stage(value)] {
		case 1:
		case 0:
			t.Errorf("%s is in the Stage enum and not in stageFacts(). Add a row "+
				"declaring it shipped, partial or planned — a proto-first change "+
				"announces itself in docs/status.json", name)
		default:
			t.Errorf("%s is declared %d times in stageFacts(); each Stage gets exactly one row",
				name, declared[knov1.Stage(value)])
		}
	}

	for stage := range declared {
		if _, ok := knov1.Stage_name[int32(stage)]; !ok {
			t.Errorf("stageFacts() declares %v, which is not a Stage value", stage)
		}
	}
}

// TestEveryDeclaredStageHasAKnownState pins the tri-state, and pins that
// "partial" cannot ship as a bare word.
func TestEveryDeclaredStageHasAKnownState(t *testing.T) {
	t.Parallel()

	for _, f := range stageFacts() {
		name := stageName(f.Stage)
		switch f.State {
		case stageShipped, stagePlanned:
			if f.Note != "" {
				t.Errorf("%s: a note belongs on a partial stage; %q is not partial", name, f.State)
			}
		case stagePartial:
			if f.Note == "" {
				t.Errorf("%s is declared partial with no note. A half-shipped stage "+
					"must say what it excludes, or the word is a shrug", name)
			}
		default:
			t.Errorf("%s: state %q is not one of %q, %q, %q",
				name, f.State, stageShipped, stagePartial, stagePlanned)
		}
		if f.Milestone == "" {
			t.Errorf("%s declares no milestone", name)
		}
	}
}

// TestShippedStagesHaveACommand is section 4's check 2. Only this direction is
// asserted: a command with no Stage is legal and there are five of them
// (init, demo, mine, report, doctor, purge).
func TestShippedStagesHaveACommand(t *testing.T) {
	t.Parallel()

	registered := map[string]bool{}
	for _, name := range registeredCommands() {
		registered[name] = true
	}

	for _, f := range stageFacts() {
		name := stageName(f.Stage)
		if f.State == stagePlanned {
			continue
		}
		if !registered[name] {
			t.Errorf("%s is declared %q and NewRootCmd() registers no `kno %s`. "+
				"A stage is not shipped until a user can run it", name, f.State, name)
		}
	}
}

// TestStatusJSONCarriesNoVersionKey is the P0's other half. Reinstating any of
// these keys is then a deliberate, failing-test decision that sends the author
// to the plan's section 7 rather than past it: release-please bumps
// .release-please-manifest.json in an ordinary PR that runs `make check`, so a
// version copied into this artifact fails status-check on every release.
func TestStatusJSONCarriesNoVersionKey(t *testing.T) {
	t.Parallel()

	raw := renderStatusRaw(t)
	for _, key := range []string{"version", "commit", "built_from", "released_version", "date"} {
		if _, ok := raw[key]; ok {
			t.Errorf("docs/status.json carries %q. The artifact's version anchor is the git "+
				"ref the site fetches it at; a copy inside the file goes stale, and a copy "+
				"derived from .release-please-manifest.json breaks every release PR "+
				"(docs/plans/2026-08-30-kno-status.md, section 7)", key)
		}
	}
}

// TestStatusJSONKeySet is the jq contract. Keys are added, never renamed or
// removed within a major; a rename must land here as a failing test rather than
// silently on somebody's pipeline.
func TestStatusJSONKeySet(t *testing.T) {
	t.Parallel()

	want := []string{
		"_generated", "adapters", "commands", "debt", "goals",
		"price_table", "schema_version", "stages",
	}
	got := make([]string, 0, len(want))
	for key := range renderStatusRaw(t) {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("docs/status.json key set drifted.\n got: %v\nwant: %v\n"+
			"Adding a key is fine and does not bump schema_version; renaming or "+
			"removing one does. See CONTRIBUTING.md", got, want)
	}
}

// TestStatusReportsTheLedgerCountsItWasGiven pins that the counts travel from
// scripts/ledger-check.py rather than being recomputed here. Nothing in Go
// parses docs/debt.md; one table, one parser.
func TestStatusReportsTheLedgerCountsItWasGiven(t *testing.T) {
	t.Parallel()

	doc := statusDocument(StatusDebt{Total: 9, Open: 3, Skipped: 0})
	if doc.Debt.Total != 9 || doc.Debt.Open != 3 || doc.Debt.Skipped != 0 {
		t.Errorf("debt = %+v, want {9 3 0}", doc.Debt)
	}
}

// TestStatusDeclaresNoStatusCommand is the cut, asserted. `status` in
// commands[] would mean somebody registered the command the plan removed
// without re-reading why.
func TestStatusDeclaresNoStatusCommand(t *testing.T) {
	t.Parallel()

	for _, name := range registeredCommands() {
		if name == "status" {
			t.Fatal("a `kno status` command is registered. The command is deliberately cut " +
				"(docs/debt.md#85): a command reports the binary in front of you, " +
				"docs/status.json reports a release. Read cli/status.go's header before reviving it")
		}
	}
}

// statusTableRow matches a Markdown table row's cells.
var statusTableSplit = regexp.MustCompile(`\s*\|\s*`)

// readmeTable returns the rows of the first Markdown table under heading.
func readmeTable(t *testing.T, heading string) [][]string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	lines := strings.Split(string(body), "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("README.md has no %q heading. The Status tables are test-pinned; "+
			"renaming a heading needs this test updated in the same edit", heading)
	}

	rows := [][]string{}
	seenHeader := false
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			if len(rows) > 0 || seenHeader {
				break
			}
			continue
		}
		cells := statusTableSplit.Split(strings.Trim(trimmed, "|"), -1)
		if !seenHeader {
			seenHeader = true
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatalf("README.md's %q section has no table rows", heading)
	}
	return rows
}

// TestReadmeStagesTableMatchesTheDeclaration is section 4's check 3. The table
// a human reads and the declaration a machine reads are the same claim, so they
// are compared row for row and in order.
func TestReadmeStagesTableMatchesTheDeclaration(t *testing.T) {
	t.Parallel()

	// The word the README prints for each declared state. "partial" gets its
	// own word so the table cannot flatten a half-shipped stage into "Shipped".
	word := map[string]string{
		stageShipped: "shipped",
		stagePartial: "partial",
		stagePlanned: "planned",
	}

	rows := readmeTable(t, "### Stages")
	facts := stageFacts()
	if len(rows) != len(facts) {
		t.Fatalf("README.md's Stages table has %d rows, the declaration has %d. "+
			"Report is NOT a stage — the Stage enum has no REPORT member; it belongs "+
			"in the Commands table", len(rows), len(facts))
	}

	for i, f := range facts {
		name := stageName(f.Stage)
		gotName := strings.ToLower(strings.Trim(rows[i][0], "*` "))
		gotState := strings.ToLower(strings.Trim(rows[i][2], "*` "))
		if gotName != name {
			t.Errorf("README Stages row %d is %q; the declaration's row %d is %q. "+
				"The tables are compared in order, matching the enum", i+1, gotName, i+1, name)
		}
		if gotState != word[f.State] {
			t.Errorf("README says %s is %q; the declaration says %q",
				name, gotState, word[f.State])
		}
	}
}

// TestReadmeCommandsTableMatchesTheCommandTree is check 3's other half, and the
// reason the README's single table had to become two: it listed Report as a
// stage and omitted init, mine, doctor and purge.
func TestReadmeCommandsTableMatchesTheCommandTree(t *testing.T) {
	t.Parallel()

	documented := []string{}
	for _, row := range readmeTable(t, "### Commands") {
		name := strings.Trim(row[0], "*` ")
		name = strings.TrimSpace(strings.TrimPrefix(name, "kno"))
		documented = append(documented, name)
	}
	sort.Strings(documented)

	want := registeredCommands()
	if strings.Join(documented, ",") != strings.Join(want, ",") {
		t.Errorf("README.md's Commands table and NewRootCmd() disagree.\n"+
			"documented: %v\nregistered: %v", documented, want)
	}
}

// TestStatusRendersDeterministically pins that two renders of one tree are
// byte-identical. `make status-check` compares bytes, so any map iteration or
// unstable sort inside the renderer would make the gate flap.
func TestStatusRendersDeterministically(t *testing.T) {
	t.Parallel()

	var first, second bytes.Buffer
	if err := WriteStatus(&first, testDebt); err != nil {
		t.Fatalf("rendering status: %v", err)
	}
	if err := WriteStatus(&second, testDebt); err != nil {
		t.Fatalf("rendering status: %v", err)
	}
	if first.String() != second.String() {
		t.Error("two renders of the same tree differ; the drift gate compares bytes")
	}
}

// renderStatusRaw renders the artifact and decodes it the way a jq pipeline
// sees it: a map, so a missing key is observable.
func renderStatusRaw(t *testing.T) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	if err := WriteStatus(&buf, testDebt); err != nil {
		t.Fatalf("rendering status: %v", err)
	}
	raw, err := decodeRaw(buf.Bytes())
	if err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	return raw
}
