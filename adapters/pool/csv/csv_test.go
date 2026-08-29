package csv_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	poolcsv "github.com/knograph/kno/adapters/pool/csv"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

// TestMain installs the process-wide goroutine leak check, matching the jsonl
// adapter. Per docs/debt.md#18, coretest's own leak check is opt-in because
// goleak's census is process-global; VerifyTestMain is goleak's recommendation
// for a parallel suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func writePool(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pool.csv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing pool: %v", err)
	}
	return path
}

func openPool(t *testing.T, path string) *poolcsv.Pool {
	t.Helper()

	p, err := poolcsv.New(poolcsv.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// assetsOf drains an iteration, failing on any yielded error.
func assetsOf(t *testing.T, p *poolcsv.Pool) []*core.Asset {
	t.Helper()

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	var out []*core.Asset
	for a, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		// Cloned: yielded Assets are borrowed for one iteration.
		out = append(out, cloneAsset(a))
	}
	return out
}

func cloneAsset(a *core.Asset) *core.Asset {
	content := make([]byte, len(a.GetContent()))
	copy(content, a.GetContent())
	return &core.Asset{
		Id: a.GetId(), Content: content, Kind: a.GetKind(),
		Destination: a.GetDestination(), Cost: a.GetCost(),
		Provenance: a.GetProvenance(), Title: a.GetTitle(),
		Tags: append([]string(nil), a.GetTags()...), UserOverridden: a.GetUserOverridden(),
	}
}

// firstError drains an iteration and reports the first yielded error together
// with how many Assets arrived before it.
func firstError(t *testing.T, p *poolcsv.Pool) (int, error) {
	t.Helper()

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	var seen int
	for _, err := range seq {
		if err != nil {
			return seen, err
		}
		seen++
	}
	return seen, nil
}

const header = "id,content,kind,tags\n"

// TestConformsToTheIteratorContract runs the shared Ring-0 harness.
func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+
		"a1,one,,\n"+
		"a2,two,,\n"+
		"a3,three,,\n"+
		"a4,four,,\n")

	coretest.ConformIterator(t, openPool(t, path).Assets)
}

// TestEarlyBreakClosesTheFile proves cleanup is deferred INSIDE the iterator
// closure, which the conformance harness cannot observe from outside.
//
// It observes descriptors rather than exhausting them — on unix a fresh open
// takes the LOWEST free descriptor, so a leak shows up directly as the
// probe's number climbing. See the jsonl adapter's equivalent test for the
// full reasoning; the abandoned iterators are kept reachable until after the
// probe so the GC's finalizers cannot hide the leak.
//
// Deliberately not parallel, and the tolerance is generous, because the count
// is process-wide: a sibling test holding a file would otherwise read as a
// leak.
func TestEarlyBreakClosesTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("File.Fd returns a handle, not a lowest-free descriptor, on windows")
	}

	const (
		iterations = 128
		tolerance  = 16
	)

	path := writePool(t, header+"a1,one,,\n"+"a2,two,,\n"+"a3,three,,\n")
	p := openPool(t, path)

	probe := func() uintptr {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("probing descriptors: %v", err)
		}
		fd := f.Fd()
		if err := f.Close(); err != nil {
			t.Fatalf("closing probe: %v", err)
		}
		return fd
	}

	before := probe()
	kept := make([]iter.Seq2[*core.Asset, error], 0, iterations)
	for range iterations {
		seq, err := p.Assets(context.Background())
		if err != nil {
			t.Fatalf("Assets: %v", err)
		}
		for range seq {
			break // the consumer loses interest after the first Asset
		}
		kept = append(kept, seq)
	}
	after := probe()
	runtime.KeepAlive(kept)

	if after > before+tolerance {
		t.Errorf("descriptor number rose from %d to %d across %d abandoned iterations; "+
			"cleanup must be deferred INSIDE the iterator closure, where an early break "+
			"still runs it", before, after, iterations)
	}
}

// TestMalformedRowIsFatal pins the contract's central rule: a bad row stops
// iteration rather than being skipped, with the row number named.
func TestMalformedRowIsFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, body, wantSubstr string
	}{
		{"no id column", "content,kind\na2,x,,\n", "no id column"},
		{"no content column", "id,kind\na2,x,,\n", "no content column"},
		{"unknown column", "id,content,difficulty\na2,x,hard\n", "unknown column"},
		{"duplicate column", "id,content,content\na2,x,y\n", "duplicate column"},
		{"missing id", header + "a1,one,,\n,content-two,,\n", "has no id"},
		{"missing content", header + "a1,one,,\na2,,,\n", "no content"},
		{"duplicate id", header + "a1,one,,\na1,two,,\n", "duplicate asset id"},
		{"unknown kind", header + "a1,one,behaviour,\n", "unknown kind"},
		{"field count drift", header + "a1,one,,\na2,two\n", "row 3"},
		{"broken quotes", header + "a1,one,,\na2,\"two,,\n", "row 3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writePool(t, tc.body)
			seen, gotErr := firstError(t, openPool(t, path))

			if gotErr == nil {
				t.Fatalf("a malformed row was tolerated; %d assets yielded", seen)
			}
			if !strings.Contains(gotErr.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", gotErr, tc.wantSubstr)
			}
		})
	}
}

// TestMissingIDColumnNamesTheFoundColumns: the refusal must be actionable —
// naming what WAS found is what lets a user see the misspelling that hid the
// id column.
func TestMissingIDColumnNamesTheFoundColumns(t *testing.T) {
	t.Parallel()

	path := writePool(t, "id,content\n") // valid header, no data rows
	_, gotErr := firstError(t, openPool(t, path))
	if gotErr != nil {
		t.Errorf("a header-only file with a valid header errored: %v", gotErr)
	}

	path = writePool(t, "Id,content\nid2,stuff\n") // capital I hides the id column
	_, gotErr = firstError(t, openPool(t, path))
	if gotErr == nil {
		t.Fatal("a header whose id column is misspelled was accepted")
	}
	for _, want := range []string{"no id column", "Id, content"} {
		if !strings.Contains(gotErr.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", gotErr, want)
		}
	}
}

// TestMissingIDRefusesAnyRowNumberFallback: the plan pins that a row-number
// fallback is not even available — an id derived from file position would
// re-number every asset whenever the file is edited, orphaning paid
// measurements.
func TestMissingIDRefusesAnyRowNumberFallback(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+"a1,one,,\n,a2,,\n")
	seen, gotErr := firstError(t, openPool(t, path))
	if gotErr == nil {
		t.Fatalf("an id-less row was given a fallback id; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), "has no id") {
		t.Errorf("error = %q, want the jsonl refusal grammar", gotErr)
	}
}

// TestDuplicateIDIsRefusedAcrossTheWholeFile: the two rows need not be
// adjacent, and refusing only neighbours would be a dedupe that happened to
// look like a check.
func TestDuplicateIDIsRefusedAcrossTheWholeFile(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(header)
	for i := range 39 {
		fmt.Fprintf(&b, "a%02d,content,,\n", i)
	}
	b.WriteString("a00,a different body,,\n")
	path := writePool(t, b.String())

	seen, gotErr := firstError(t, openPool(t, path))
	if gotErr == nil {
		t.Fatalf("a duplicate id 39 rows away from its twin was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), `duplicate asset id "a00"`) {
		t.Errorf("error = %q, want it to name the duplicated id", gotErr)
	}
}

// TestEmptyPoolYieldsNothing: an empty pool is a decision for the caller to
// refuse before it spends, not an error for the adapter to invent. Blank
// lines are skipped rather than being treated as records — a trailing newline
// is not a malformed Asset.
func TestEmptyPoolYieldsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty file", ""},
		{"only newlines", "\n\n\n"},
		{"header only", header},
		{"blank lines between rows", header + "a1,one,,\n\n\na2,two,,\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writePool(t, tc.body)
			seen, gotErr := firstError(t, openPool(t, path))
			if gotErr != nil {
				t.Errorf("an empty pool was reported as an error: %v", gotErr)
			}
			if tc.name == "blank lines between rows" {
				if seen != 2 {
					t.Errorf("yielded %d assets, want 2", seen)
				}
				return
			}
			if seen != 0 {
				t.Errorf("yielded %d assets from an empty pool", seen)
			}
		})
	}
}

// TestOptionalColumnsReachTheAsset covers the optional surface at once: a
// column the format accepts but never maps would be a column the user writes
// into a void.
func TestOptionalColumnsReachTheAsset(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+"a1,refund policy: 30 days,knowledge,billing; policy\n")

	got := assetsOf(t, openPool(t, path))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	a := got[0]

	if a.GetId() != "a1" {
		t.Errorf("id = %q, want %q", a.GetId(), "a1")
	}
	if string(a.GetContent()) != "refund policy: 30 days" {
		t.Errorf("content = %q", a.GetContent())
	}
	if strings.Join(a.GetTags(), ",") != "billing,policy" {
		t.Errorf("tags = %v, want [billing policy]", a.GetTags())
	}
	if a.GetKind() != knov1.Kind_KIND_KNOWLEDGE {
		t.Errorf("kind = %v, want KIND_KNOWLEDGE", a.GetKind())
	}
	if !a.GetUserOverridden() {
		t.Error("a row that named a kind must set user_overridden; the report cannot " +
			"otherwise tell an asserted routing decision from a measured one")
	}
	if a.GetDestination() != knov1.Destination_DESTINATION_UNSPECIFIED {
		t.Errorf("destination = %v; it is assigned by Select after measurement, "+
			"never read from the pool", a.GetDestination())
	}
}

// TestTagsDelimiterIsPinned: the semicolon is a contract, not a default — a
// comma list inside a quoted field is refused the ambiguity this format
// refuses everywhere else.
func TestTagsDelimiterIsPinned(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+"a1,body,,\"one,two\"\n")
	got := assetsOf(t, openPool(t, path))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	// A comma list is ONE tag, exactly as written — never split silently.
	if len(got[0].GetTags()) != 1 || got[0].GetTags()[0] != "one,two" {
		t.Errorf("tags = %v, want the cell verbatim as one tag", got[0].GetTags())
	}
}

// TestTagsTrimAndDropEmpties: surrounding whitespace is trimmed and empty
// entries (a trailing separator, two separators in a row) are dropped rather
// than tagging an Asset with the empty string.
func TestTagsTrimAndDropEmpties(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+"a1,body,,\" alpha ;beta; ; gamma \"\n")
	got := assetsOf(t, openPool(t, path))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	want := []string{"alpha", "beta", "gamma"}
	gotTags := got[0].GetTags()
	if strings.Join(gotTags, ",") != strings.Join(want, ",") {
		t.Errorf("tags = %v, want %v", gotTags, want)
	}
}

// TestBothKindsAreSpelled: a Kind the format accepts but maps to the wrong
// enum routes the Asset to the wrong destination.
func TestBothKindsAreSpelled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		spelling string
		want     knov1.Kind
	}{
		{"knowledge", knov1.Kind_KIND_KNOWLEDGE},
		{"behavior", knov1.Kind_KIND_BEHAVIOR},
	} {
		t.Run(tc.spelling, func(t *testing.T) {
			t.Parallel()

			path := writePool(t, "id,content,kind\n"+fmt.Sprintf("a1,body,%s\n", tc.spelling))
			got := assetsOf(t, openPool(t, path))
			if len(got) != 1 {
				t.Fatalf("yielded %d assets, want 1", len(got))
			}
			if got[0].GetKind() != tc.want {
				t.Errorf("kind %q mapped to %v, want %v", tc.spelling, got[0].GetKind(), tc.want)
			}
		})
	}
}

// TestOmittedColumnsMeanNotKnown: an absent kind must stay unspecified and
// must NOT be reported as a user override, or every Asset in a plain pool
// would look hand-pinned and the report could no longer distinguish the two.
func TestOmittedColumnsMeanNotKnown(t *testing.T) {
	t.Parallel()

	path := writePool(t, "id,content\na1,body\n")
	got := assetsOf(t, openPool(t, path))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	a := got[0]

	if a.GetKind() != knov1.Kind_KIND_UNSPECIFIED {
		t.Errorf("kind = %v, want KIND_UNSPECIFIED for a row that named none", a.GetKind())
	}
	if a.GetUserOverridden() {
		t.Error("an Asset whose row named no kind was marked user_overridden")
	}
	if a.GetTags() != nil {
		t.Errorf("tags = %v, want none for a file with no tags column", a.GetTags())
	}
}

// TestProvenanceTracesBackToTheRow: an Asset that cannot be traced to the row
// it came from cannot be audited, and the rejection log's whole job is to
// explain a decision about a specific record.
func TestProvenanceTracesBackToTheRow(t *testing.T) {
	t.Parallel()

	path := writePool(t, header+"a1,one,,\na2,two,,\na3,three,,\n")

	got := assetsOf(t, openPool(t, path))
	if len(got) != 3 {
		t.Fatalf("yielded %d assets, want 3", len(got))
	}

	wantRefs := []string{path + ":2", path + ":3", path + ":4"}
	for i, a := range got {
		if a.GetProvenance().GetSource() != "csv" {
			t.Errorf("asset %s: source = %q, want %q", a.GetId(), a.GetProvenance().GetSource(), "csv")
		}
		if got := a.GetProvenance().GetSourceRef(); got != wantRefs[i] {
			t.Errorf("asset %s: source_ref = %q, want %q", a.GetId(), got, wantRefs[i])
		}
	}
}

// TestQuotedFieldsSurviveIntact: CSV content is documents, not prompts — a
// field carrying a comma or a newline (quoted) is a normal pool row, and the
// field count must not be confused by the contents of a quote.
func TestQuotedFieldsSurviveIntact(t *testing.T) {
	t.Parallel()

	// A quoted field: the newline and the comma live INSIDE the quotes, so
	// the record stays one row with one field.
	body := "multi\nline, with a comma"
	path := writePool(t, "id,content\na1,\""+body+"\"\n")

	got := assetsOf(t, openPool(t, path))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	if string(got[0].GetContent()) != body {
		t.Errorf("content came back as %q, want %q", got[0].GetContent(), body)
	}
}

// TestReadingTwiceProducesIdenticalAssets guards the ingestion timestamp that
// is deliberately not set: a pool that has not moved must not look changed on
// every read.
func TestReadingTwiceProducesIdenticalAssets(t *testing.T) {
	t.Parallel()

	p := openPool(t, writePool(t, header+"a1,one,,\na2,two,,\n"))

	first, second := assetsOf(t, p), assetsOf(t, p)
	if len(first) != len(second) {
		t.Fatalf("two reads of one file yielded %d and %d assets", len(first), len(second))
	}
	for i := range first {
		if first[i].GetProvenance().GetIngestedAt() != second[i].GetProvenance().GetIngestedAt() {
			t.Error("two reads of an unchanged file disagreed on ingested_at; " +
				"a pool that has not moved would look changed on every read")
		}
	}
}

// TestContextTokensRankRatherThanReserve is the guard on docs/debt.md#68.
//
// The reservation path deliberately over-counts by about 3x on prose, which is
// correct for bounding money and wrong for a ranking denominator. If someone
// wires countTokens in here, delta_per_cost starts ordering the portfolio by
// content type — so the assertion is on the RATIO, not merely on the
// arithmetic.
func TestContextTokensRankRatherThanReserve(t *testing.T) {
	t.Parallel()

	const (
		size      = 3600
		wantExact = 1000 // 3.6 bytes/token, prose-centered
	)

	path := writePool(t, "id,content\n"+
		fmt.Sprintf("prose,%s\n", strings.Repeat("a", size))+
		fmt.Sprintf("double,%s\n", strings.Repeat("a", 2*size))+
		"tiny,x\n")

	got := assetsOf(t, openPool(t, path))
	if len(got) != 3 {
		t.Fatalf("yielded %d assets, want 3", len(got))
	}
	prose, double, tiny := got[0].GetCost(), got[1].GetCost(), got[2].GetCost()

	if prose.GetContextTokens() != wantExact {
		t.Errorf("context_tokens = %d for %d bytes, want %d",
			prose.GetContextTokens(), size, wantExact)
	}
	if double.GetContextTokens() <= prose.GetContextTokens() {
		t.Errorf("twice the content cost %d tokens against %d; the denominator must grow "+
			"with the Asset", double.GetContextTokens(), prose.GetContextTokens())
	}
	if tiny.GetContextTokens() < 1 {
		t.Errorf("a one-byte Asset costs %d tokens; a zero denominator makes "+
			"delta_per_cost an infinity, which sorts to the top of a greedy ranking",
			tiny.GetContextTokens())
	}
}

// TestCancellationStopsIterationMidway: iter.Seq2 carries no cancellation
// channel of its own, so the producer checks ctx before each yield. Without
// it, a user's Ctrl-C keeps a 1M-asset pool streaming.
func TestCancellationStopsIterationMidway(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(header)
	for i := range 50 {
		fmt.Fprintf(&b, "a%02d,content,,\n", i)
	}
	p := openPool(t, writePool(t, b.String()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seq, err := p.Assets(ctx)
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	const cancelAfter = 3
	var seen int
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
		seen++
		if seen == cancelAfter {
			cancel()
		}
	}

	if gotErr == nil {
		t.Fatalf("iteration ran to completion after cancellation; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), context.Canceled.Error()) {
		t.Errorf("error = %q, want the cancellation to be reported as itself", gotErr)
	}
	if seen > cancelAfter {
		t.Errorf("yielded %d assets after cancellation at %d; ctx must be checked "+
			"before each yield", seen-cancelAfter, cancelAfter)
	}
}

// TestRetainedAssetDoesNotCorruptTheNextRow is the borrow contract from the
// producer's side.
//
// Values are borrowed for one iteration and consumers are told to clone — but
// a consumer that holds or edits one anyway must not silently rewrite the
// records that follow. A producer that reused one Asset struct across yields
// would turn a consumer's own bookkeeping into corrupted data with nothing to
// see it.
func TestRetainedAssetDoesNotCorruptTheNextRow(t *testing.T) {
	t.Parallel()

	const n = 6
	var b strings.Builder
	b.WriteString(header)
	for i := range n {
		fmt.Fprintf(&b, "a%d,body-%d,,\"t%d\"\n", i, i, i)
	}
	p := openPool(t, writePool(t, b.String()))

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	var retained []*core.Asset
	var i int
	for a, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		// Every field must be exactly this record's, whatever a previous
		// iteration did to the value it was handed.
		if want := fmt.Sprintf("a%d", i); a.GetId() != want {
			t.Fatalf("asset %d has id %q, want %q", i, a.GetId(), want)
		}
		if want := fmt.Sprintf("body-%d", i); string(a.GetContent()) != want {
			t.Errorf("asset %d has content %q, want %q; a previous iteration's "+
				"retained value leaked into it", i, a.GetContent(), want)
		}
		if want := fmt.Sprintf("t%d", i); len(a.GetTags()) != 1 || a.GetTags()[0] != want {
			t.Errorf("asset %d has tags %v, want [%s]", i, a.GetTags(), want)
		}

		// A badly-behaved consumer: keeps the pointer and edits it in place.
		retained = append(retained, a)
		a.Tags = append(a.Tags, "consumer-added")
		a.Content = []byte("overwritten by the consumer")
		a.Destination = knov1.Destination_DESTINATION_CONTEXT
		i++
	}

	if len(retained) != n {
		t.Fatalf("yielded %d assets, want %d", len(retained), n)
	}
	seenIDs := make(map[string]struct{}, n)
	for _, a := range retained {
		if _, dup := seenIDs[a.GetId()]; dup {
			t.Errorf("two retained assets share the id %q; the producer handed out "+
				"one value repeatedly", a.GetId())
		}
		seenIDs[a.GetId()] = struct{}{}
	}
}

// TestFileErrorsSurfaceOnOpen: an unreadable source fails when the iterator is
// requested, not partway through a run that has already spent money.
func TestFileErrorsSurfaceOnOpen(t *testing.T) {
	t.Parallel()

	p := openPool(t, filepath.Join(t.TempDir(), "absent.csv"))
	if _, err := p.Assets(context.Background()); err == nil {
		t.Error("a missing file was accepted")
	}
}

// TestNewRejectsBadOptions catches configuration errors before a run starts.
func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	if _, err := poolcsv.New(poolcsv.Options{}); err == nil {
		t.Error("an empty path was accepted")
	}
}
