package jsonl_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

// TestMain installs the process-wide goroutine leak check.
//
// Per docs/debt.md#18: coretest's own leak check is opt-in because goleak's
// census is process-global and unreliable under t.Parallel(). VerifyTestMain is
// goleak's recommendation for a parallel suite, and this is the first adapter,
// so it is where the pattern gets established.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func writeCases(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}
	return path
}

func caseLine(id, input string) string {
	return fmt.Sprintf(`{"id":%q,"input":%q,"expected":"yes"}`, id, input)
}

func collect(t *testing.T, seq iter.Seq2[*core.Case, error]) []*core.Case {
	t.Helper()

	var out []*core.Case
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		// Cloned: yielded Cases are borrowed for one iteration.
		out = append(out, cloneCase(c))
	}
	return out
}

func cloneCase(c *core.Case) *core.Case {
	return &core.Case{
		Id: c.GetId(), Input: c.GetInput(), Expected: c.GetExpected(),
		Rubric: c.GetRubric(), Tags: c.GetTags(), Split: c.GetSplit(),
	}
}

// TestConformsToTheIteratorContract runs the shared harness. This adapter is
// its first real subject.
func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	path := writeCases(t,
		caseLine("c1", "one"), caseLine("c2", "two"),
		caseLine("c3", "three"), caseLine("c4", "four"))

	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	coretest.ConformIterator(t, ev.Cases)
	seq, err := ev.Cases(t.Context())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	coretest.EvalsDuplicateIDs(t, seq)
}

// TestEarlyBreakClosesTheFile proves cleanup is deferred INSIDE the iterator
// closure, which the conformance harness cannot observe from outside.
func TestEarlyBreakClosesTheFile(t *testing.T) {
	t.Parallel()

	path := writeCases(t, caseLine("c1", "one"), caseLine("c2", "two"), caseLine("c3", "three"))
	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	for range seq {
		break
	}

	// A leaked descriptor is invisible directly, but a closed file is
	// observable by the process being able to reopen and fully read the file
	// many times without exhausting descriptors.
	for range 200 {
		seq, err := ev.Cases(context.Background())
		if err != nil {
			t.Fatalf("reopening after early break: %v", err)
		}
		for range seq {
			break
		}
	}
}

// TestMalformedRecordIsFatal pins the contract's central rule: a bad record
// stops iteration rather than being skipped.
//
// Skipping would shrink the denominator behind every later delta with nothing
// showing it — and if one adapter skipped while another halted, two runs would
// be measured over different populations while looking identical.
func TestMalformedRecordIsFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, bad, wantSubstr string
	}{
		{"invalid json", `{"id":"c2","input":`, "line 2"},
		{"missing input", `{"id":"c2"}`, "no input"},
		{"duplicate id", `{"id":"c1","input":"again"}`, "duplicate case id"},
		{"missing id", `{"input":"no id here"}`, "has no id"},
		{"unknown field", `{"id":"c2","input":"x","difficulty":"hard"}`, "unknown field"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeCases(t, caseLine("c1", "one"), tc.bad, caseLine("c3", "three"))
			ev, err := jsonl.New(jsonl.Options{Path: path})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			seq, err := ev.Cases(context.Background())
			if err != nil {
				t.Fatalf("Cases: %v", err)
			}

			var seen int
			var gotErr error
			for _, err := range seq {
				if err != nil {
					gotErr = err
					break
				}
				seen++
			}

			if gotErr == nil {
				t.Fatalf("a malformed record was tolerated; %d cases yielded", seen)
			}
			if !strings.Contains(gotErr.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", gotErr, tc.wantSubstr)
			}
			if seen > 1 {
				t.Errorf("yielded %d cases past the bad record; iteration must stop at it", seen-1)
			}
		})
	}
}

// TestOversizedRecordIsRejected covers the memory cap. Without it, one
// enormous line reads itself into memory in full.
func TestOversizedRecordIsRejected(t *testing.T) {
	t.Parallel()

	huge := fmt.Sprintf(`{"id":"big","input":%q}`, strings.Repeat("x", 5<<20))
	path := writeCases(t, caseLine("c1", "one"), huge)

	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("an oversized record was accepted")
	}
	if !strings.Contains(gotErr.Error(), "capped at") {
		t.Errorf("error = %q, want it to explain the cap", gotErr)
	}
}

// TestSplitIsStableAcrossRuns is the property that keeps two runs comparable.
func TestSplitIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, 200)
	for i := range 200 {
		lines = append(lines, caseLine(fmt.Sprintf("case-%03d", i), "q"))
	}
	path := writeCases(t, lines...)

	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := splitMap(t, ev)
	for range 3 {
		if got := splitMap(t, ev); !sameSplits(first, got) {
			t.Fatal("the split changed between runs over an unchanged file; " +
				"two runs of the same eval set would be incomparable")
		}
	}
}

// TestAddingCasesDoesNotMoveExistingOnes is why the split is keyed on the Case
// ID alone.
//
// If it were keyed on position, or on a per-run salt, adding a Case would
// reshuffle the rest — and a Case scored and reported in dev on one run could
// become "untouched holdout" on the next, which does not prevent a leak, it
// delays it.
func TestAddingCasesDoesNotMoveExistingOnes(t *testing.T) {
	t.Parallel()

	original := make([]string, 0, 100)
	for i := range 100 {
		original = append(original, caseLine(fmt.Sprintf("case-%03d", i), "q"))
	}

	before := splitMapFor(t, writeCases(t, original...))

	extended := make([]string, len(original), len(original)+50)
	copy(extended, original)
	for i := 100; i < 150; i++ {
		extended = append(extended, caseLine(fmt.Sprintf("case-%03d", i), "q"))
	}
	after := splitMapFor(t, writeCases(t, extended...))

	for id, split := range before {
		if after[id] != split {
			t.Errorf("case %s moved from %v to %v when unrelated cases were added",
				id, split, after[id])
		}
	}
}

// TestSplitSeedRepartitions confirms a deliberate re-split is possible, and
// that it actually changes the assignment — otherwise the option would be a
// no-op that silently did nothing.
func TestSplitSeedRepartitions(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, 200)
	for i := range 200 {
		lines = append(lines, caseLine(fmt.Sprintf("case-%03d", i), "q"))
	}
	path := writeCases(t, lines...)

	plain, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seeded, err := jsonl.New(jsonl.Options{Path: path, SplitSeed: "2026-q3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a, b := splitMap(t, plain), splitMap(t, seeded)
	moved := 0
	for id, split := range a {
		if b[id] != split {
			moved++
		}
	}
	if moved == 0 {
		t.Error("a split seed changed nothing; the option would silently do nothing")
	}
}

// TestHoldoutFractionIsHonored checks the split lands near the requested share.
func TestHoldoutFractionIsHonored(t *testing.T) {
	t.Parallel()

	const n = 2000
	lines := make([]string, 0, n)
	for i := range n {
		lines = append(lines, caseLine(fmt.Sprintf("case-%05d", i), "q"))
	}
	path := writeCases(t, lines...)

	for _, frac := range []float64{0.1, 0.2, 0.5} {
		ev, err := jsonl.New(jsonl.Options{Path: path, HoldoutFrac: frac})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		counts, err := ev.CountSplits(context.Background())
		if err != nil {
			t.Fatalf("CountSplits: %v", err)
		}

		got := float64(counts.Holdout) / float64(counts.Total())
		if diff := got - frac; diff > 0.05 || diff < -0.05 {
			t.Errorf("holdout fraction %.3f, want ~%.2f (%d of %d)",
				got, frac, counts.Holdout, counts.Total())
		}
	}
}

// TestEmptyHoldoutIsRefused covers the decision that a run which can never
// produce a holdout number is not a run.
func TestEmptyHoldoutIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		counts jsonl.SplitCounts
		wantOK bool
	}{
		{"healthy", jsonl.SplitCounts{Dev: 80, Holdout: 20}, true},
		{"small but valid", jsonl.SplitCounts{Dev: 8, Holdout: 2}, true},
		{"no holdout", jsonl.SplitCounts{Dev: 12, Holdout: 0}, false},
		{"no dev", jsonl.SplitCounts{Dev: 0, Holdout: 12}, false},
		{"empty", jsonl.SplitCounts{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.counts.Validate()
			if tc.wantOK && err != nil {
				t.Errorf("Validate = %v, want nil", err)
			}
			if !tc.wantOK && err == nil {
				t.Error("Validate accepted a split that cannot produce a holdout number")
			}
		})
	}
}

// TestUnderpoweredHoldoutIsFlaggedNotRefused: a small holdout still runs, but
// the caveat travels with it rather than the number being presented as though
// it meant the same thing.
func TestUnderpoweredHoldoutIsFlaggedNotRefused(t *testing.T) {
	t.Parallel()

	small := jsonl.SplitCounts{Dev: 40, Holdout: 5}
	if err := small.Validate(); err != nil {
		t.Errorf("a small holdout was refused: %v", err)
	}
	if !small.Underpowered() {
		t.Error("a 5-case holdout is not flagged as underpowered")
	}

	big := jsonl.SplitCounts{Dev: 400, Holdout: 100}
	if big.Underpowered() {
		t.Error("a 100-case holdout is flagged as underpowered")
	}
}

// TestContentHashTracksContentNotMtime pins what invalidates a resume.
func TestContentHashTracksContentNotMtime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "cases.jsonl")
	body := caseLine("c1", "one") + "\n" + caseLine("c2", "two") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := ev.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}

	// Rewriting identical content must NOT invalidate a resume: a checkout
	// that touches mtime is not a change to the evals.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewriting: %v", err)
	}
	same, err := ev.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if same != first {
		t.Error("rewriting identical content changed the hash; a checkout would break resume")
	}

	// An in-place edit MUST invalidate it.
	if err := os.WriteFile(path, []byte(body+caseLine("c3", "three")+"\n"), 0o600); err != nil {
		t.Fatalf("editing: %v", err)
	}
	changed, err := ev.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if changed == first {
		t.Error("editing the file did not change the hash; resume would mix results " +
			"measured against different eval sets")
	}
}

// TestContentHashCoversSplitConfiguration: changing the split reclassifies
// Cases without the file changing, so it must invalidate a resume too.
func TestContentHashCoversSplitConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := writeCases(t, caseLine("c1", "one"), caseLine("c2", "two"))

	base, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	baseHash, err := base.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}

	for _, tc := range []struct {
		name string
		opts jsonl.Options
	}{
		{"different fraction", jsonl.Options{Path: path, HoldoutFrac: 0.4}},
		{"different seed", jsonl.Options{Path: path, SplitSeed: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev, err := jsonl.New(tc.opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := ev.ContentHash(ctx)
			if err != nil {
				t.Fatalf("ContentHash: %v", err)
			}
			if got == baseHash {
				t.Error("the hash ignored a split change; a resume would silently " +
					"reclassify Cases mid-run")
			}
		})
	}
}

// TestEveryCaseGetsASplit guards against an unassigned Split reaching the
// pipeline, where the seal filters it out and it would vanish silently.
func TestEveryCaseGetsASplit(t *testing.T) {
	t.Parallel()

	path := writeCases(t, caseLine("c1", "one"), caseLine("c2", "two"), caseLine("c3", "three"))
	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	for _, c := range collect(t, seq) {
		if c.GetSplit() == knov1.Split_SPLIT_UNSPECIFIED {
			t.Errorf("case %s has no split assigned", c.GetId())
		}
	}
}

// TestFileErrorsSurfaceOnOpen: an unreadable source fails when the iterator is
// requested, not partway through a run that has already spent money.
func TestFileErrorsSurfaceOnOpen(t *testing.T) {
	t.Parallel()

	ev, err := jsonl.New(jsonl.Options{Path: filepath.Join(t.TempDir(), "absent.jsonl")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := ev.Cases(context.Background()); err == nil {
		t.Error("a missing file was accepted")
	}
}

// TestNewRejectsBadOptions catches configuration errors before a run starts.
func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts jsonl.Options
	}{
		{"no path", jsonl.Options{}},
		{"fraction of one", jsonl.Options{Path: "x", HoldoutFrac: 1}},
		{"fraction above one", jsonl.Options{Path: "x", HoldoutFrac: 1.5}},
		{"negative fraction", jsonl.Options{Path: "x", HoldoutFrac: -0.1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := jsonl.New(tc.opts); err == nil {
				t.Error("bad options were accepted")
			}
		})
	}
}

func splitMap(t *testing.T, ev *jsonl.Evals) map[string]knov1.Split {
	t.Helper()

	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	out := make(map[string]knov1.Split)
	for _, c := range collect(t, seq) {
		out[c.GetId()] = c.GetSplit()
	}
	return out
}

func splitMapFor(t *testing.T, path string) map[string]knov1.Split {
	t.Helper()

	ev, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return splitMap(t, ev)
}

func sameSplits(a, b map[string]knov1.Split) bool {
	if len(a) != len(b) {
		return false
	}
	for id, split := range a {
		if b[id] != split {
			return false
		}
	}
	return true
}

// TestMissingIDIsFatalNotPositional is the regression test for a leak that
// reached a PR.
//
// An earlier version defaulted an absent id to "path:line". Because the split
// is keyed on the id, that made a Case's half depend on its POSITION in the
// file — so inserting a line, reordering, or renaming reclassified every Case
// after it. A Case scored and reported as dev on one run would become
// untouched holdout on the next, which is the exact failure split.go's own
// comment rejects a per-run salt for.
//
// The fix is to refuse the file. This proves the refusal, and proves the
// scenario that used to break: identical Cases at different line offsets get
// identical splits.
func TestMissingIDIsFatalNotPositional(t *testing.T) {
	t.Parallel()

	t.Run("a case without an id is refused", func(t *testing.T) {
		t.Parallel()

		path := writeCases(t, `{"input":"anonymous"}`)
		ev, err := jsonl.New(jsonl.Options{Path: path})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		seq, err := ev.Cases(context.Background())
		if err != nil {
			t.Fatalf("Cases: %v", err)
		}

		var gotErr error
		for _, err := range seq {
			if err != nil {
				gotErr = err
				break
			}
		}
		if gotErr == nil {
			t.Fatal("a Case with no id was accepted; its split would come from its line number")
		}
		if !strings.Contains(gotErr.Error(), "split is keyed on it") {
			t.Errorf("error = %q, want it to explain why an id is required", gotErr)
		}
	})

	t.Run("line position never decides a split", func(t *testing.T) {
		t.Parallel()

		// The same Cases, shifted by unrelated lines above them.
		compact := writeCases(t, caseLine("keep-1", "a"), caseLine("keep-2", "b"))
		padded := writeCases(t,
			caseLine("filler-1", "x"), caseLine("filler-2", "y"), caseLine("filler-3", "z"),
			caseLine("keep-1", "a"), caseLine("keep-2", "b"))

		before, after := splitMapFor(t, compact), splitMapFor(t, padded)
		for _, id := range []string{"keep-1", "keep-2"} {
			if before[id] != after[id] {
				t.Errorf("case %s moved from %v to %v when unrelated lines were inserted above it",
					id, before[id], after[id])
			}
		}
	})
}

// TestRenamingTheFileDoesNotMoveCases covers the other half of the same defect.
//
// The auto-generated id embedded the file path, so a rename changed every
// split — while ContentHash reported no change at all, because the bytes were
// identical. A resumed run would have proceeded believing nothing had moved.
func TestRenamingTheFileDoesNotMoveCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	body := caseLine("c1", "one") + "\n" + caseLine("c2", "two") + "\n"

	first := filepath.Join(dir, "before.jsonl")
	second := filepath.Join(dir, "after.jsonl")
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}

	if a, b := splitMapFor(t, first), splitMapFor(t, second); !sameSplits(a, b) {
		t.Error("identical Cases in differently-named files landed in different splits")
	}

	// The path still participates in the fingerprint: a resume that switched
	// files of identical content is measuring a different run than it thinks.
	evA, _ := jsonl.New(jsonl.Options{Path: first})
	evB, _ := jsonl.New(jsonl.Options{Path: second})
	hashA, err := evA.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	hashB, err := evB.ContentHash(ctx)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if hashA == hashB {
		t.Error("two different files hashed identically; a resume could silently switch sources")
	}
}

// TestSplitDistributesSharedPrefixIDs guards the ID shapes real users have.
//
// ULIDs share a time prefix and sequential ids share everything but a short
// suffix. FNV-1a diffuses per byte, so a shared prefix is where a weak hash
// would skew — and a skewed split means a holdout that is not representative
// of the dev set it is meant to validate against.
//
// The tolerance is scaled to the binomial standard deviation rather than being
// a round number: at n=4000, p=0.2 the expected deviation is ~0.6 points, so
// four sigma is ~2.5 points. A looser bound would let a real skew pass.
func TestSplitDistributesSharedPrefixIDs(t *testing.T) {
	t.Parallel()

	shapes := map[string]func(int) string{
		"ulid-like":   func(i int) string { return fmt.Sprintf("01JBQ8Z3XK9YV2NPQR%06d", i) },
		"path-like":   func(i int) string { return fmt.Sprintf("evals/regression/suite.jsonl:%d", i) },
		"uuid-like":   func(i int) string { return fmt.Sprintf("3f2504e0-4f89-11d3-9a0c-%012d", i) },
		"long-common": func(i int) string { return strings.Repeat("prefix-", 8) + strconv.Itoa(i) },
	}

	const (
		n         = 4000
		frac      = 0.2
		tolerance = 0.025 // ~4 sigma at this n and p
	)

	for name, makeID := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lines := make([]string, 0, n)
			for i := range n {
				lines = append(lines, caseLine(makeID(i), "q"))
			}
			ev, err := jsonl.New(jsonl.Options{Path: writeCases(t, lines...), HoldoutFrac: frac})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			counts, err := ev.CountSplits(context.Background())
			if err != nil {
				t.Fatalf("CountSplits: %v", err)
			}

			got := float64(counts.Holdout) / float64(counts.Total())
			if diff := got - frac; diff > tolerance || diff < -tolerance {
				t.Errorf("holdout fraction %.4f for %s ids, want %.2f ± %.3f (%d of %d)",
					got, name, frac, tolerance, counts.Holdout, counts.Total())
			}
		})
	}
}

// TestValidateGuidanceUsesTheConfiguredFraction: an error whose fix does not
// fix is worse than no fix, and CLAUDE.md's error grammar requires the real
// one.
func TestValidateGuidanceUsesTheConfiguredFraction(t *testing.T) {
	t.Parallel()

	err := jsonl.SplitCounts{Dev: 12, Holdout: 0, HoldoutFrac: 0.05}.Validate()
	if err == nil {
		t.Fatal("a zero-case holdout was accepted")
	}
	if !strings.Contains(err.Error(), "0.05") {
		t.Errorf("error = %q, want it to name the configured fraction rather than the default", err)
	}
	if strings.Contains(err.Error(), "roughly 6 ") {
		t.Error("the guidance was computed from the default fraction, not the configured one")
	}
}
