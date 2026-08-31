package cli_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
)

// v01ShapePath is the frozen capture of what v0.1.0's --json emitted.
const v01ShapePath = "testdata/json/v0.1-shape.json"

// jsonGoldenDir holds one golden per stage document.
const jsonGoldenDir = "testdata/json"

// stageDocuments renders all five stage documents once, from `kno demo
// --json`, which runs the whole pipeline against the pinned fake: agent with
// stable run IDs.
//
// The demo is the right source rather than five hand-built fixtures: it is
// the only place the five stages are produced by the real commands, in order,
// against the same store — so a document here is what a user's jq pipeline
// actually receives, not what a test harness thought it would.
func stageDocuments(t *testing.T) map[string][]byte {
	t.Helper()

	stdout, stderr, code := runDemoIn(t, t.TempDir(), "--json")
	if code != errs.ExitOK {
		t.Fatalf("demo --json exit = %d\nstderr: %s", code, stderr)
	}
	doc, err := cli.DecodeDemoReport([]byte(stdout))
	if err != nil {
		t.Fatalf("the demo document is not valid json: %v", err)
	}
	return map[string][]byte{
		"baseline": doc.Stages.Baseline,
		"value":    doc.Stages.Value,
		"select":   doc.Stages.Select,
		"export":   doc.Stages.Export,
		"report":   doc.Stages.Report,
	}
}

// TestV01ShapeIsStillASubset is the released contract's regression guard.
//
// Every key v0.1.0 emitted unconditionally must still be there, with the same
// JSON type. It is a SUBSET check by design: ADR-0006 rule 2 permits adding
// keys freely and forbids renaming, removing or retyping one, so the test
// asserts exactly that and nothing more.
//
// The frozen fixture lives in this repository on purpose. The original plan
// checked the fenced sample in docs/cookbook/ci-gate.md; that page moved to
// uknoAI/kno-examples in #163 and is now a one-line tombstone, so the check
// would have depended on a file this repository's CI cannot see change.
func TestV01ShapeIsStillASubset(t *testing.T) {
	frozen := readShapeFixture(t)
	docs := stageDocuments(t)

	stages := make([]string, 0, len(frozen))
	for stage := range frozen {
		stages = append(stages, stage)
	}
	sort.Strings(stages)

	for _, stage := range stages {
		want := frozen[stage]
		t.Run(stage, func(t *testing.T) {
			raw, ok := docs[stage]
			if !ok {
				t.Fatalf("no %s document to check", stage)
			}
			got, err := cli.DecodeRaw(raw)
			if err != nil {
				t.Fatalf("the %s document is not valid json: %v", stage, err)
			}
			for _, key := range sortedKeys(want) {
				value, present := got[key]
				if !present {
					t.Errorf("kno %s --json no longer emits %q. It was released in v0.1.0; "+
						"removing or renaming a released key needs a CHANGELOG migration "+
						"note (ADR-0006 rule 2)", stage, key)
					continue
				}
				if gotType := jsonTypeOf(value); !typeMatches(want[key], gotType) {
					t.Errorf("kno %s --json emits %q as %s, but v0.1.0 emitted it as %s. "+
						"Retyping a released key breaks every jq pipeline that reads it",
						stage, key, gotType, want[key])
				}
			}
		})
	}
}

// TestStageJSONGoldens pins each stage document key-for-key.
//
// The goldens are the mechanical form of ADR-0006 rule 2: a renamed or
// removed key shows up as a diff and is reviewed like code. Regenerate with
// `make update-golden`.
//
// The golden records the document's KEYS AND TYPES rather than its bytes.
// Values would pin the fixture's arithmetic, which report_test.go and
// demo_test.go already assert and which would make every numeric change look
// like a contract change — the opposite of what a contract golden is for.
func TestStageJSONGoldens(t *testing.T) {
	// packageDir BEFORE stageDocuments: the demo runs under t.Chdir, and a
	// path resolved after it points at the temp directory.
	dir := filepath.Join(packageDir(t), jsonGoldenDir)
	docs := stageDocuments(t)

	stages := []string{"baseline", "value", "select", "export", "report"}
	for _, stage := range stages {
		got := renderShape(t, docs[stage])
		path := filepath.Join(dir, stage+".golden")
		if *updateGolden {
			if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v — run `make update-golden`", path, err)
		}
		if got != string(want) {
			t.Errorf("the kno %s --json shape drifted. Re-run `make update-golden` and "+
				"review the diff: an added key is fine, a renamed or removed one needs a "+
				"CHANGELOG migration note (ADR-0006 rule 2).\ngot:\n%s\nwant:\n%s",
				stage, got, string(want))
		}
	}
}

// renderShape flattens one document to sorted `key: type` lines.
func renderShape(t *testing.T, raw []byte) string {
	t.Helper()
	doc, err := cli.DecodeRaw(raw)
	if err != nil {
		t.Fatalf("the document is not valid json: %v", err)
	}
	var b strings.Builder
	for _, k := range sortedAnyKeys(doc) {
		b.WriteString(k + ": " + jsonTypeOf(doc[k]) + "\n")
	}
	return b.String()
}

// readShapeFixture parses the frozen v0.1 capture, dropping its _readme.
func readShapeFixture(t *testing.T) map[string]map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(packageDir(t), v01ShapePath))
	if err != nil {
		t.Fatalf("reading the frozen v0.1 shape: %v", err)
	}
	raw, err := cli.DecodeRaw(b)
	if err != nil {
		t.Fatalf("the frozen v0.1 shape is not valid json: %v", err)
	}
	out := map[string]map[string]string{}
	for stage, keys := range raw {
		m, ok := keys.(map[string]any)
		if !ok {
			continue // _readme
		}
		shape := map[string]string{}
		for k, v := range m {
			s, ok := v.(string)
			if !ok {
				t.Fatalf("the frozen shape's %s.%s is not a type name", stage, k)
			}
			shape[k] = s
		}
		out[stage] = shape
	}
	if len(out) != 5 {
		t.Fatalf("the frozen shape covers %d stages, want 5", len(out))
	}
	return out
}

// jsonTypeOf names the JSON type of a decoded value.
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// typeMatches accepts a pipe-separated set of legal types, so a key whose
// value is legitimately nullable (`score`) or a possibly-nil slice
// (`selected`) is not a false failure.
func typeMatches(want, got string) bool {
	for _, alt := range strings.Split(want, "|") {
		if alt == got {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
