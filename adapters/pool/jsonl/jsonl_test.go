package jsonl_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/pool/jsonl"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.uber.org/goleak"
)

// TestMain installs the process-wide goroutine leak check, matching the Evals
// adapter. Per docs/debt.md#18, coretest's own leak check is opt-in because
// goleak's census is process-global; VerifyTestMain is goleak's recommendation
// for a parallel suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func writeAssets(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pool.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing pool: %v", err)
	}
	return path
}

func assetLine(id, content string) string {
	return fmt.Sprintf(`{"id":%q,"content":%q}`, id, content)
}

func openPool(t *testing.T, path string) *jsonl.Pool {
	t.Helper()

	p, err := jsonl.New(jsonl.Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// assetsOf drains an iteration, failing on any yielded error.
func assetsOf(t *testing.T, p *jsonl.Pool) []*core.Asset {
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
func firstError(t *testing.T, p *jsonl.Pool) (int, error) {
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

// TestConformsToTheIteratorContract runs the shared Ring-0 harness.
func TestConformsToTheIteratorContract(t *testing.T) {
	t.Parallel()

	path := writeAssets(t,
		assetLine("a1", "one"), assetLine("a2", "two"),
		assetLine("a3", "three"), assetLine("a4", "four"))

	coretest.ConformIterator(t, openPool(t, path).Assets)
}

// TestEarlyBreakClosesTheFile proves cleanup is deferred INSIDE the iterator
// closure, which the conformance harness cannot observe from outside.
//
// It observes descriptors rather than exhausting them. The Evals adapter's
// equivalent test opens and abandons 200 iterations and calls a successful
// reopen proof of closure — but on a host whose descriptor limit is 1,048,576
// (the default on this project's darwin machines) 200 leaked descriptors are
// invisible, so that test passes whether or not the file is ever closed. On
// unix a fresh open takes the LOWEST free descriptor, so a leak shows up
// directly as the probe's number climbing.
//
// The abandoned iterators are kept reachable until after the probe. An
// unreferenced *os.File is closed by the runtime's finalizer, which reclaimed
// most of a 256-descriptor leak here and dropped the signal below any tolerance
// that survives a parallel sibling — the bug would have hidden behind the GC's
// timing.
//
// Deliberately not parallel, and the tolerance is generous, because the count
// is process-wide: a sibling test holding a file would otherwise read as a leak.
// iterations is far above that noise floor.
func TestEarlyBreakClosesTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("File.Fd returns a handle, not a lowest-free descriptor, on windows")
	}

	const (
		iterations = 128
		tolerance  = 16
	)

	path := writeAssets(t, assetLine("a1", "one"), assetLine("a2", "two"), assetLine("a3", "three"))
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

// TestMalformedRecordIsFatal pins the contract's central rule: a bad record
// stops iteration rather than being skipped.
//
// Skipping would shrink the pool with nothing showing it. The Asset count is
// the denominator behind every later "N of M assets earned their place", and if
// one adapter skipped while another halted, two runs measured over different
// populations would look identical.
func TestMalformedRecordIsFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, bad, wantSubstr string
	}{
		{"invalid json", `{"id":"a2","content":`, "line 2"},
		{"missing content", `{"id":"a2"}`, "no content"},
		{"empty content", `{"id":"a2","content":""}`, "no content"},
		{"missing id", `{"content":"anonymous"}`, "has no id"},
		{"duplicate id", `{"id":"a1","content":"again"}`, "duplicate asset id"},
		{"unknown field", `{"id":"a2","content":"x","difficulty":"hard"}`, "unknown field"},
		{"unknown kind", `{"id":"a2","content":"x","kind":"behaviour"}`, "unknown kind"},
		{"destination is not the file's to declare", `{"id":"a2","content":"x","destination":"context"}`, "unknown field"},
		{"context tokens are not the file's to declare", `{"id":"a2","content":"x","cost":{"context_tokens":1}}`, "unknown field"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeAssets(t, assetLine("a1", "one"), tc.bad, assetLine("a3", "three"))
			seen, gotErr := firstError(t, openPool(t, path))

			if gotErr == nil {
				t.Fatalf("a malformed record was tolerated; %d assets yielded", seen)
			}
			if !strings.Contains(gotErr.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", gotErr, tc.wantSubstr)
			}
			if seen > 1 {
				t.Errorf("yielded %d assets past the bad record; iteration must stop at it", seen-1)
			}
		})
	}
}

// TestDuplicateIDIsRefusedAcrossTheWholeFile: the two records need not be
// adjacent, and refusing only neighbours would be a dedupe that happened to
// look like a check.
func TestDuplicateIDIsRefusedAcrossTheWholeFile(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, 40)
	for i := range 39 {
		lines = append(lines, assetLine(fmt.Sprintf("a%02d", i), "content"))
	}
	lines = append(lines, assetLine("a00", "a different body, the same id"))

	seen, gotErr := firstError(t, openPool(t, writeAssets(t, lines...)))
	if gotErr == nil {
		t.Fatalf("a duplicate id 39 lines away from its twin was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), `duplicate asset id "a00"`) {
		t.Errorf("error = %q, want it to name the duplicated id", gotErr)
	}
}

// TestEmptyPoolYieldsNothing: an empty pool is a decision for the caller to
// refuse before it spends, not an error for the adapter to invent. Blank lines
// are skipped rather than being treated as records — a trailing newline is not
// a malformed Asset.
func TestEmptyPoolYieldsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty file", ""},
		{"only newlines", "\n\n\n"},
		{"only whitespace", "   \n\t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "pool.jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("writing: %v", err)
			}

			seen, gotErr := firstError(t, openPool(t, path))
			if gotErr != nil {
				t.Errorf("an empty pool was reported as an error: %v", gotErr)
			}
			if seen != 0 {
				t.Errorf("yielded %d assets from an empty pool", seen)
			}
		})
	}
}

// TestRecordFieldsReachTheAsset covers the whole optional surface at once: a
// field the format accepts but never maps would be a field the user writes into
// a void.
func TestRecordFieldsReachTheAsset(t *testing.T) {
	t.Parallel()

	const line = `{"id":"a1","content":"refund policy: 30 days","title":"Refunds",` +
		`"tags":["billing","policy"],"kind":"knowledge",` +
		`"cost":{"acquisition_usd_micros":12500,"stale":true}}`

	got := assetsOf(t, openPool(t, writeAssets(t, line)))
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
	if a.GetTitle() != "Refunds" {
		t.Errorf("title = %q, want %q", a.GetTitle(), "Refunds")
	}
	if strings.Join(a.GetTags(), ",") != "billing,policy" {
		t.Errorf("tags = %v, want [billing policy]", a.GetTags())
	}
	if a.GetKind() != knov1.Kind_KIND_KNOWLEDGE {
		t.Errorf("kind = %v, want KIND_KNOWLEDGE", a.GetKind())
	}
	if !a.GetUserOverridden() {
		t.Error("a file that named a kind must set user_overridden; the report cannot " +
			"otherwise tell an asserted routing decision from a measured one")
	}
	if a.GetCost().GetAcquisitionUsdMicros() != 12500 {
		t.Errorf("acquisition = %d, want 12500", a.GetCost().GetAcquisitionUsdMicros())
	}
	if !a.GetCost().GetStale() {
		t.Error("stale was declared in the file and did not reach the Asset")
	}
	if a.GetDestination() != knov1.Destination_DESTINATION_UNSPECIFIED {
		t.Errorf("destination = %v; it is assigned by Select after measurement, "+
			"never read from the pool", a.GetDestination())
	}
}

// TestBothKindsAreSpelled: a Kind the format accepts but maps to the wrong enum
// routes the Asset to the wrong destination, and KIND_UNSPECIFIED's own proto
// comment is that a silent zero reads as knowledge.
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

			line := fmt.Sprintf(`{"id":"a1","content":"body","kind":%q}`, tc.spelling)
			got := assetsOf(t, openPool(t, writeAssets(t, line)))
			if len(got) != 1 {
				t.Fatalf("yielded %d assets, want 1", len(got))
			}
			if got[0].GetKind() != tc.want {
				t.Errorf("kind %q mapped to %v, want %v", tc.spelling, got[0].GetKind(), tc.want)
			}
		})
	}
}

// TestOmittedFieldsMeanNotKnown: an absent kind must stay unspecified and must
// NOT be reported as a user override, or every Asset in a plain pool would look
// hand-pinned and the report could no longer distinguish the two.
func TestOmittedFieldsMeanNotKnown(t *testing.T) {
	t.Parallel()

	got := assetsOf(t, openPool(t, writeAssets(t, assetLine("a1", "body"))))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	a := got[0]

	if a.GetKind() != knov1.Kind_KIND_UNSPECIFIED {
		t.Errorf("kind = %v, want KIND_UNSPECIFIED for a record that named none", a.GetKind())
	}
	if a.GetUserOverridden() {
		t.Error("an Asset whose file named no kind was marked user_overridden")
	}
	if a.GetCost().GetStale() {
		t.Error("stale defaulted to true; false means 'not known to be stale'")
	}
	if a.GetCost().GetAcquisitionUsdMicros() != 0 {
		t.Errorf("acquisition = %d for a record with no cost block", a.GetCost().GetAcquisitionUsdMicros())
	}
}

// TestProvenanceTracesBackToTheLine: an Asset that cannot be traced to the byte
// range it came from cannot be audited, and the rejection log's whole job is to
// explain a decision about a specific record.
func TestProvenanceTracesBackToTheLine(t *testing.T) {
	t.Parallel()

	path := writeAssets(t,
		assetLine("a1", "one"), "", assetLine("a2", "two"), assetLine("a3", "three"))

	got := assetsOf(t, openPool(t, path))
	if len(got) != 3 {
		t.Fatalf("yielded %d assets, want 3", len(got))
	}

	// Line 2 is blank, so a2 is on line 3: the number must be the file's, not
	// the Asset's ordinal, or it points at the wrong record.
	wantRefs := []string{path + ":1", path + ":3", path + ":4"}
	for i, a := range got {
		if a.GetProvenance().GetSource() != "jsonl" {
			t.Errorf("asset %s: source = %q, want %q", a.GetId(), a.GetProvenance().GetSource(), "jsonl")
		}
		if got := a.GetProvenance().GetSourceRef(); got != wantRefs[i] {
			t.Errorf("asset %s: source_ref = %q, want %q", a.GetId(), got, wantRefs[i])
		}
	}
}

// TestReadingTwiceProducesIdenticalAssets guards the ingestion timestamp that
// is deliberately not set: a pool that has not moved must not look changed on
// every read.
func TestReadingTwiceProducesIdenticalAssets(t *testing.T) {
	t.Parallel()

	p := openPool(t, writeAssets(t, assetLine("a1", "one"), assetLine("a2", "two")))

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
// content type — so the assertion is on the RATIO, not merely on the arithmetic.
func TestContextTokensRankRatherThanReserve(t *testing.T) {
	t.Parallel()

	const (
		size      = 3600
		wantExact = 1000 // 3.6 bytes/token, prose-centered
		// The reservation path's effective ratio is ~1.33 bytes/token, which
		// would put this at ~2700. Anything at or above this is that number,
		// not a ranking denominator.
		reservationFloor = 2000
	)

	lines := []string{
		fmt.Sprintf(`{"id":"prose","content":%q}`, strings.Repeat("a", size)),
		fmt.Sprintf(`{"id":"double","content":%q}`, strings.Repeat("a", 2*size)),
		`{"id":"tiny","content":"x"}`,
	}
	got := assetsOf(t, openPool(t, writeAssets(t, lines...)))
	if len(got) != 3 {
		t.Fatalf("yielded %d assets, want 3", len(got))
	}
	prose, double, tiny := got[0].GetCost(), got[1].GetCost(), got[2].GetCost()

	if prose.GetContextTokens() != wantExact {
		t.Errorf("context_tokens = %d for %d bytes, want %d",
			prose.GetContextTokens(), size, wantExact)
	}
	if prose.GetContextTokens() >= reservationFloor {
		t.Errorf("context_tokens = %d for %d bytes; that is the pessimistic reservation "+
			"count, which ranks a pool by content type (docs/debt.md#68)",
			prose.GetContextTokens(), size)
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

// TestLargeContentSurvivesIntact: Assets are documents, not prompts, so a
// multi-megabyte record is a normal pool and not an attack.
func TestLargeContentSurvivesIntact(t *testing.T) {
	t.Parallel()

	const size = 5 << 20 // comfortably past the scanner's starting buffer
	body := strings.Repeat("kno", size/3)

	path := writeAssets(t,
		assetLine("small", "one"),
		fmt.Sprintf(`{"id":"large","content":%q}`, body),
		assetLine("after", "three"))

	got := assetsOf(t, openPool(t, path))
	if len(got) != 3 {
		t.Fatalf("yielded %d assets, want 3", len(got))
	}
	if string(got[1].GetContent()) != body {
		t.Errorf("large content came back at %d bytes, want %d",
			len(got[1].GetContent()), len(body))
	}
	if string(got[2].GetContent()) != "three" {
		t.Errorf("the record after a large one read as %q; the scanner buffer did not reset",
			got[2].GetContent())
	}
}

// TestOversizedRecordIsRejected covers the memory cap. Without it, one enormous
// line reads itself into memory in full — the failure mode the streaming
// profile exists to prevent.
func TestOversizedRecordIsRejected(t *testing.T) {
	t.Parallel()

	const limit = 4 << 10
	huge := fmt.Sprintf(`{"id":"big","content":%q}`, strings.Repeat("x", 8<<10))
	path := writeAssets(t, assetLine("a1", "one"), huge)

	p, err := jsonl.New(jsonl.Options{Path: path, MaxRecordBytes: limit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seen, gotErr := firstError(t, p)
	if gotErr == nil {
		t.Fatalf("an oversized record was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), "capped at") {
		t.Errorf("error = %q, want it to explain the cap", gotErr)
	}
	if !strings.Contains(gotErr.Error(), "MaxRecordBytes") {
		t.Errorf("error = %q, want it to name the knob that would fix it", gotErr)
	}
}

// TestRaisingTheCapAcceptsTheRecord proves MaxRecordBytes is load-bearing
// rather than a field that is read and ignored.
func TestRaisingTheCapAcceptsTheRecord(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", 8<<10)
	path := writeAssets(t, fmt.Sprintf(`{"id":"big","content":%q}`, body))

	p, err := jsonl.New(jsonl.Options{Path: path, MaxRecordBytes: 64 << 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := assetsOf(t, p)
	if len(got) != 1 || string(got[0].GetContent()) != body {
		t.Errorf("a record within a raised cap was not read back intact (%d assets)", len(got))
	}
}

// TestCancellationStopsIterationMidway: iter.Seq2 carries no cancellation
// channel of its own, so the producer checks ctx before each yield. Without it,
// a user's Ctrl-C keeps a 1M-asset pool streaming.
func TestCancellationStopsIterationMidway(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, 50)
	for i := range 50 {
		lines = append(lines, assetLine(fmt.Sprintf("a%02d", i), "content"))
	}
	p := openPool(t, writeAssets(t, lines...))

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

// TestRetainedAssetDoesNotCorruptTheNextRecord is the borrow contract from the
// producer's side.
//
// Values are borrowed for one iteration and consumers are told to clone — but a
// consumer that holds or edits one anyway must not silently rewrite the records
// that follow. A producer that reused one Asset struct across yields would turn
// a consumer's own bookkeeping into corrupted data with nothing to see it: the
// second Asset would arrive already carrying the first's tags.
func TestRetainedAssetDoesNotCorruptTheNextRecord(t *testing.T) {
	t.Parallel()

	const n = 6
	lines := make([]string, 0, n)
	for i := range n {
		lines = append(lines, fmt.Sprintf(`{"id":"a%d","content":"body-%d","tags":["t%d"]}`, i, i, i))
	}
	p := openPool(t, writeAssets(t, lines...))

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

	p := openPool(t, filepath.Join(t.TempDir(), "absent.jsonl"))
	if _, err := p.Assets(context.Background()); err == nil {
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
		{"negative cap", jsonl.Options{Path: "x", MaxRecordBytes: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := jsonl.New(tc.opts); err == nil {
				t.Error("bad options were accepted")
			}
		})
	}
}
