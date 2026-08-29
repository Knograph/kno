package mine

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/split"
)

// update rewrites the golden files. `make update-golden` runs `go test
// ./... -update`; a golden diff is reviewed like code.
var update = flag.Bool("update", false, "rewrite golden files")

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func mineOpts(paths []string, mode Mode) Options {
	return Options{
		Logs:              paths,
		Format:            "auto",
		Mode:              mode,
		MaxQuestionTokens: DefaultMaxQuestionTokens,
	}
}

func mineCases(t *testing.T, path string, mode Mode) []Case {
	t.Helper()
	cases, counts, err := Mine(context.Background(), mineOpts([]string{path}, mode))
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}
	if counts.Mined != len(cases) {
		t.Fatalf("counts.Mined = %d, len(cases) = %d", counts.Mined, len(cases))
	}
	return cases
}

// TestMineFormatsGolden pins the mined output for every format x mode pair.
//
// The output is the product surface: the ids are content-keyed, so the
// golden diff is exactly what changes when extraction changes — the loud
// re-mine the identity rule promises.
func TestMineFormatsGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		trans  string
		mode   Mode
		golden string
	}{
		{"jsonl-chat resolution", "testdata/transcript.jsonl", ModeResolution, "testdata/golden/chat-resolution.jsonl"},
		{"jsonl-chat immediate", "testdata/transcript.jsonl", ModeImmediate, "testdata/golden/chat-immediate.jsonl"},
		{"markdown resolution", "testdata/transcript.md", ModeResolution, "testdata/golden/markdown-resolution.jsonl"},
		{"markdown immediate", "testdata/transcript.md", ModeImmediate, "testdata/golden/markdown-immediate.jsonl"},
		{"csv resolution", "testdata/transcript.csv", ModeResolution, "testdata/golden/csv-resolution.jsonl"},
		{"csv immediate", "testdata/transcript.csv", ModeImmediate, "testdata/golden/csv-immediate.jsonl"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := mineOpts([]string{tt.trans}, tt.mode)
			opts.AgentName = "AI" // markdown requires the agent's speaker name
			cases, _, err := Mine(context.Background(), opts)
			if err != nil {
				t.Fatalf("Mine: %v", err)
			}
			var buf bytes.Buffer
			for _, c := range cases {
				if err := EncodeOutput(&buf, c); err != nil {
					t.Fatalf("EncodeOutput: %v", err)
				}
			}
			if *update {
				if err := os.WriteFile(tt.golden, buf.Bytes(), 0o600); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(tt.golden)
			if err != nil {
				t.Fatalf("reading golden %s (run with -update to write it): %v", tt.golden, err)
			}
			if buf.String() != string(want) {
				t.Fatalf("mined output differs from golden %s\n--- got ---\n%s\n--- want ---\n%s",
					tt.golden, buf.String(), want)
			}
		})
	}
}

// TestMineResolutionContents pins what resolution mode MINES, not just what
// it renders: the thread's final human message is the expected, shaped and
// modal-filtered.
func TestMineResolutionContents(t *testing.T) {
	t.Parallel()
	cases, counts, err := Mine(context.Background(), mineOpts([]string{"testdata/transcript.jsonl"}, ModeResolution))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Filtered[FilterGratitude] != 1 {
		t.Fatalf("gratitude filtered = %d, want 1", counts.Filtered[FilterGratitude])
	}
	want := []struct{ input, expected string }{
		{"Where is my refund?", "it should have arrived yesterday"},
		{"Why is my invoice wrong?", "invoice #42 should be $12, not $120"},
	}
	if len(cases) != len(want) {
		t.Fatalf("mined %d cases, want %d: %+v", len(cases), len(want), cases)
	}
	for i, w := range want {
		if cases[i].Input != w.input || cases[i].Expected != w.expected {
			t.Fatalf("case %d = (%q, %q), want (%q, %q)",
				i, cases[i].Input, cases[i].Expected, w.input, w.expected)
		}
	}
	if cases[0].SourceRef != "testdata/transcript.jsonl" {
		t.Fatalf("source_ref = %q", cases[0].SourceRef)
	}
}

// TestMineImmediateCounts pins the immediate-mode ledger: one filtered
// gratitude, one label per remaining agent answer.
func TestMineImmediateCounts(t *testing.T) {
	t.Parallel()
	_, counts, err := Mine(context.Background(), mineOpts([]string{"testdata/transcript.jsonl"}, ModeImmediate))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Mined != 2 {
		t.Fatalf("mined = %d, want 2", counts.Mined)
	}
	if counts.Filtered[FilterGratitude] != 1 {
		t.Fatalf("gratitude filtered = %d, want 1", counts.Filtered[FilterGratitude])
	}
	if len(counts.Pairing) != 1 {
		t.Fatalf("pairing rows = %d, want 1", len(counts.Pairing))
	}
	p := counts.Pairing[0]
	if p.AgentMessages != 3 || p.HumanReplies != 3 || p.Mined != 2 || p.Filtered != 1 {
		t.Fatalf("pairing = %+v, want 3 agent, 3 replies, 2 mined, 1 filtered", p)
	}
}

// TestMineIdempotent pins the identity rule end to end: mining the same
// transcript twice yields byte-identical output, which is what makes the
// manifest's curated drops matchable across re-mines.
func TestMineIdempotent(t *testing.T) {
	t.Parallel()
	a := mineCases(t, "testdata/transcript.jsonl", ModeResolution)
	b := mineCases(t, "testdata/transcript.jsonl", ModeResolution)
	if len(a) != len(b) {
		t.Fatalf("mined %d then %d cases", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Expected != b[i].Expected {
			t.Fatalf("case %d differs across re-mines: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestMineDedupAcrossFiles pins the cross-file dedup: the same exchange in
// two transcripts is one Case, with the duplicate counted.
func TestMineDedupAcrossFiles(t *testing.T) {
	t.Parallel()
	dup := "{\"id\":\"m1\",\"role\":\"user\",\"content\":\"q?\",\"thread_id\":\"t\"}\n" +
		"{\"id\":\"m2\",\"role\":\"agent\",\"content\":\"a\"}\n" +
		"{\"id\":\"m3\",\"role\":\"human\",\"content\":\"No, it should be X\"}\n"
	a := writeTemp(t, "a.jsonl", dup)
	b := writeTemp(t, "b.jsonl", dup)

	cases, counts, err := Mine(context.Background(), mineOpts([]string{a, b}, ModeResolution))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("mined %d cases, want 1 (the duplicate must be dropped)", len(cases))
	}
	if counts.Deduped != 1 {
		t.Fatalf("deduped = %d, want 1", counts.Deduped)
	}
}

// TestMineTokenCap pins the cap: a question alone over the cap is dropped
// with a count, never truncated, and the cap is part of the id so a cap
// change re-ids the set loudly.
func TestMineTokenCap(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cap.jsonl", "{\"id\":\"m1\",\"role\":\"user\",\"content\":\"Where is my refund?\",\"thread_id\":\"t\"}\n"+
		"{\"id\":\"m2\",\"role\":\"agent\",\"content\":\"a\"}\n"+
		"{\"id\":\"m3\",\"role\":\"human\",\"content\":\"No, it should be X\"}\n")

	opts := mineOpts([]string{path}, ModeResolution)
	opts.MaxQuestionTokens = 5 // far below the 20-byte question's ~15 tokens
	_, counts, err := Mine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if counts.OverCap != 1 {
		t.Fatalf("over-cap = %d, want 1", counts.OverCap)
	}
	if counts.Mined != 0 {
		t.Fatalf("mined = %d, want 0 (the over-cap question is dropped, not truncated)", counts.Mined)
	}

	// The cap is in the hash: the same content under different caps re-ids.
	small := caseID("t", "Where is my refund?", "it should be X", 100, time.Time{})
	large := caseID("t", "Where is my refund?", "it should be X", 200, time.Time{})
	if small == large {
		t.Fatal("changing the token cap did not change the id")
	}
}

// TestMineManifestDropSurvival pins the durable review contract: a curated
// drop recorded in the manifest is applied by every re-mine, counted as
// preserved, and can never resurrect.
func TestMineManifestDropSurvival(t *testing.T) {
	t.Parallel()
	trans := "{\"id\":\"m1\",\"role\":\"user\",\"content\":\"q?\",\"thread_id\":\"t\"}\n" +
		"{\"id\":\"m2\",\"role\":\"agent\",\"content\":\"a\"}\n" +
		"{\"id\":\"m3\",\"role\":\"human\",\"content\":\"No, it should be X\"}\n" +
		"{\"id\":\"m4\",\"role\":\"user\",\"content\":\"r?\",\"thread_id\":\"t2\"}\n" +
		"{\"id\":\"m5\",\"role\":\"agent\",\"content\":\"b\"}\n" +
		"{\"id\":\"m6\",\"role\":\"human\",\"content\":\"No, it should be Y\"}\n"
	path := writeTemp(t, "t.jsonl", trans)
	dir := filepath.Dir(path)
	manifestPath := filepath.Join(dir, "cases.jsonl.review.json")

	first := mineCases(t, path, ModeResolution)
	if len(first) != 2 {
		t.Fatalf("first mine produced %d cases, want 2", len(first))
	}
	dropID := first[0].ID
	manifest := Manifest{Decisions: map[string]Decision{
		dropID: {Decision: "drop"},
	}}
	if err := saveManifest(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}

	opts := mineOpts([]string{path}, ModeResolution)
	opts.Manifest = manifestPath
	second, counts, err := Mine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("re-mine produced %d cases, want 1 (the drop must survive)", len(second))
	}
	if second[0].ID == dropID {
		t.Fatalf("the curated drop %s resurrected", dropID)
	}
	if counts.PreservedDrops != 1 {
		t.Fatalf("preserved drops = %d, want 1", counts.PreservedDrops)
	}
}

// TestMineManifestEdit pins the durable review contract for edits: the
// corrected expected is re-applied by every re-mine, under a NEW id, while
// the manifest key stays the source's original id.
func TestMineManifestEdit(t *testing.T) {
	t.Parallel()
	trans := "{\"id\":\"m1\",\"role\":\"user\",\"content\":\"q?\",\"thread_id\":\"t\"}\n" +
		"{\"id\":\"m2\",\"role\":\"agent\",\"content\":\"a\"}\n" +
		"{\"id\":\"m3\",\"role\":\"human\",\"content\":\"No, it should be X\"}\n"
	path := writeTemp(t, "t.jsonl", trans)
	manifestPath := filepath.Join(filepath.Dir(path), "cases.jsonl.review.json")

	first := mineCases(t, path, ModeResolution)
	if len(first) != 1 {
		t.Fatalf("first mine produced %d cases, want 1", len(first))
	}
	original := first[0]
	manifest := Manifest{Decisions: map[string]Decision{
		original.ID: {Decision: "edit", Expected: "it should be Z"},
	}}
	if err := saveManifest(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}

	opts := mineOpts([]string{path}, ModeResolution)
	opts.Manifest = manifestPath
	second, _, err := Mine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("re-mine produced %d cases, want 1", len(second))
	}
	if second[0].Expected != "it should be Z" {
		t.Fatalf("edited expected = %q, want %q", second[0].Expected, "it should be Z")
	}
	if second[0].ID == original.ID {
		t.Fatal("an edit must re-id the case")
	}
	// The manifest key must remain the SOURCE id, not the edited one.
	secondFirst, _, err := Mine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if secondFirst[0].ID != second[0].ID {
		t.Fatalf("the manifest edit did not apply deterministically: %q vs %q", secondFirst[0].ID, second[0].ID)
	}
}

// TestMineMarkdownRequiresAgentName pins the refusal: without --agent-name
// there is no way to tell the agent's messages from the human's, and
// guessing would pair the wrong sides.
func TestMineMarkdownRequiresAgentName(t *testing.T) {
	t.Parallel()
	opts := mineOpts([]string{"testdata/transcript.md"}, ModeResolution)
	_, _, err := Mine(context.Background(), opts)
	if err == nil {
		t.Fatal("markdown without --agent-name must be refused")
	}
	if err.Error() != ErrAgentNameRequired.Error() {
		t.Fatalf("error = %v, want ErrAgentNameRequired", err)
	}
}

// TestMineMarkdownSpeakerInventory pins the pairing summary's speaker data:
// every speaker is named, and the agent-name match decides the sides.
func TestMineMarkdownSpeakerInventory(t *testing.T) {
	t.Parallel()
	opts := mineOpts([]string{"testdata/transcript.md"}, ModeResolution)
	opts.AgentName = "AI"
	cases, counts, err := Mine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("mined %d cases, want 2", len(cases))
	}
	if counts.Speakers["Alice"] != 4 || counts.Speakers["AI"] != 2 {
		t.Fatalf("speaker inventory = %+v, want Alice 4, AI 2", counts.Speakers)
	}
}

// TestMineCSVMissingColumn pins the strict header contract: a row whose
// answer column is silently absent is a row whose label is silently lost.
func TestMineCSVMissingColumn(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "bad.csv", "question,response\nq?,a\n")
	opts := mineOpts([]string{path}, ModeResolution)
	opts.Format = "csv" // the strict header contract lives in the CSV parser, not the sniffer
	_, _, err := Mine(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "question and answer columns") {
		t.Fatalf("error = %v, want the missing-column refusal", err)
	}
}

// TestMineCSVEmptyRow pins the strict row contract: an empty answer is
// fatal, because an auto-generated expected would depend on position.
func TestMineCSVEmptyRow(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "bad.csv", "question,answer\nq?,\n")
	opts := mineOpts([]string{path}, ModeResolution)
	opts.Format = "csv"
	_, _, err := Mine(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "empty answer") {
		t.Fatalf("error = %v, want the empty-answer refusal", err)
	}
}

// TestMineSniff pins the auto detection: a leading { is jsonl-chat, a
// heading or **speaker:** line is markdown, a question,answer header is
// csv, and anything else is refused with the candidates named.
func TestMineSniff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		first   string
		want    string
		wantErr bool
	}{
		{"json", `{"id":"m1"}`, "jsonl-chat", false},
		{"markdown heading", "# Support thread", "markdown", false},
		{"markdown speaker", "**Alice:** hi", "markdown", false},
		{"csv header", "question,answer", "csv", false},
		{"csv header order", "answer,question", "csv", false},
		{"plain prose", "Hello there", "", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, "t."+tt.name, tt.first+"\n")
			got, err := resolveFormat("auto", path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveFormat(%q) = %q, want an error", tt.first, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFormat(%q): %v", tt.first, err)
			}
			if got != tt.want {
				t.Fatalf("resolveFormat(%q) = %q, want %q", tt.first, got, tt.want)
			}
		})
	}
}

// TestMineNoFiles pins the empty-source refusal: a --logs path that yields
// no transcript files is an error, not a silent "mined 0 Cases".
func TestMineNoFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := mineOpts([]string{dir}, ModeResolution)
	_, _, err := Mine(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "no transcript files") {
		t.Fatalf("error = %v, want the no-files refusal", err)
	}
}

// TestReviewFlow drives the interactive loop end to end: drop, edit (with a
// re-id), keep, quit — and the manifest records exactly the decisions made.
func TestReviewFlow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	mk := func(q, e string) Case {
		c := Case{Input: q, Expected: e, ident: identity{thread: "t", question: q, expected: e, cap: 32_000, time: now}}
		c.reID()
		return c
	}
	a, b, c := mk("q1", "e1"), mk("q2", "e2"), mk("q3", "e3")
	manifestPath := filepath.Join(t.TempDir(), "cases.jsonl.review.json")

	var out bytes.Buffer
	kept, m, err := Review([]Case{a, b, c}, manifestPath,
		strings.NewReader("d\n"+"e\n"+"new expected\n"+"q\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d cases, want 2 (drop + edit + quit)", len(kept))
	}
	if kept[0].Expected != "new expected" {
		t.Fatalf("edited expected = %q", kept[0].Expected)
	}
	if kept[0].ID == b.ID {
		t.Fatal("an edit must re-id the case")
	}
	if kept[1].ID != c.ID {
		t.Fatalf("the quit case changed id: %q vs %q", kept[1].ID, c.ID)
	}
	if len(m.Decisions) != 2 {
		t.Fatalf("manifest has %d decisions, want 2 (the quit case gets none)", len(m.Decisions))
	}
	if m.Decisions[a.ID].Decision != "drop" {
		t.Fatalf("decision on a = %+v, want drop", m.Decisions[a.ID])
	}
	if m.Decisions[b.ID].Decision != "edit" || m.Decisions[b.ID].Expected != "new expected" {
		t.Fatalf("decision on b = %+v, want edit with the new expected", m.Decisions[b.ID])
	}
	if _, ok := m.Decisions[c.ID]; ok {
		t.Fatalf("the quit case was recorded as reviewed")
	}
}

// TestMineSplitCrossCheck pins the weak-label pipeline end to end: a mined
// set is re-ingested by the jsonl adapter with its provenance intact, and
// the adapter's split assignment matches the shared split exactly.
func TestMineSplitCrossCheck(t *testing.T) {
	t.Parallel()
	cases := mineCases(t, "testdata/transcript.jsonl", ModeResolution)
	out := filepath.Join(t.TempDir(), "mined.jsonl")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if err := EncodeOutput(f, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	evals, err := jsonl.New(jsonl.Options{Path: out})
	if err != nil {
		t.Fatal(err)
	}
	seq, err := evals.Cases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for c, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if !c.GetProvenance().GetDerived() {
			t.Fatalf("case %s lost its derived marker on the round-trip", c.GetId())
		}
		if c.GetProvenance().GetDerivationNote() == "" {
			t.Fatalf("case %s lost its derivation note", c.GetId())
		}
		want := split.AssignSplit(c.GetId(), "", jsonl.DefaultHoldoutFrac)
		if c.GetSplit() != want {
			t.Fatalf("case %s: adapter assigned %s, split assigns %s",
				c.GetId(), c.GetSplit(), want)
		}
	}
	if seen != len(cases) {
		t.Fatalf("re-ingested %d cases, want %d", seen, len(cases))
	}
	counts, err := evals.CountSplits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.WeakLabelCases != len(cases) {
		t.Fatalf("weak label count on re-ingestion = %d, want %d", counts.WeakLabelCases, len(cases))
	}
}
