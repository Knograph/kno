package markdown_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	poolmd "github.com/knograph/kno/adapters/pool/markdown"
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

// writeFile writes one file into dir.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// writeDir makes a temp directory with the given files and returns its path.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	// Map iteration order is random; the tests rely on lexical order, so the
	// filenames must be written in a defined order — which they are, because
	// the walk sorts them regardless of creation order.
	for name, body := range files {
		writeFile(t, dir, name, body)
	}
	return dir
}

func openPool(t *testing.T, path string, split bool) *poolmd.Pool {
	t.Helper()

	p, err := poolmd.New(poolmd.Options{Path: path, SplitSections: split})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// assetsOf drains an iteration, failing on any yielded error.
func assetsOf(t *testing.T, p *poolmd.Pool) []*core.Asset {
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
func firstError(t *testing.T, p *poolmd.Pool) (int, error) {
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

	dir := writeDir(t, map[string]string{
		"one.md":   "# One\n\nbody one\n",
		"two.md":   "# Two\n\nbody two\n",
		"three.md": "# Three\n\nbody three\n",
		"four.md":  "# Four\n\nbody four\n",
	})

	coretest.ConformIterator(t, openPool(t, dir, false).Assets)
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

	dir := writeDir(t, map[string]string{
		"one.md": "# One\n\nbody one\n",
		"two.md": "# Two\n\nbody two\n",
	})
	p := openPool(t, dir, false)

	probe := func() uintptr {
		f, err := os.Open(dir)
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

// TestWholeFileIsOneAsset pins the default mode: id = file path, content =
// the whole file, front matter stripped, kind unset so routing judges it.
func TestWholeFileIsOneAsset(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"guide.md": "# Guide\n\nbody one\n",
	})

	got := assetsOf(t, openPool(t, dir, false))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	a := got[0]

	wantID := filepath.Join(dir, "guide.md")
	if a.GetId() != wantID {
		t.Errorf("id = %q, want the file path %q", a.GetId(), wantID)
	}
	if string(a.GetContent()) != "# Guide\n\nbody one\n" {
		t.Errorf("content = %q, want the whole file", a.GetContent())
	}
	if a.GetKind() != knov1.Kind_KIND_UNSPECIFIED {
		t.Errorf("kind = %v, want KIND_UNSPECIFIED for a file that named none", a.GetKind())
	}
	if a.GetUserOverridden() {
		t.Error("an Asset whose file named no kind was marked user_overridden")
	}
}

// TestDirectoryModeIsSortedAndRecursive: the walk is recursive, and the order
// a pool yields is the order every report sees, so it is pinned lexical.
func TestDirectoryModeIsSortedAndRecursive(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"b.md":             "body b\n",
		"a.md":             "body a\n",
		"nested/c.md":      "body c\n",
		"nested/deep/d.md": "body d\n",
		"ignore.txt":       "not an asset\n",
	})

	got := assetsOf(t, openPool(t, dir, false))
	if len(got) != 4 {
		t.Fatalf("yielded %d assets, want 4 (the .txt is not an Asset)", len(got))
	}
	for i, want := range []string{"a.md", "b.md", "nested/c.md", "nested/deep/d.md"} {
		wantID := filepath.Join(dir, filepath.FromSlash(want))
		if got[i].GetId() != wantID {
			t.Errorf("asset %d has id %q, want %q", i, got[i].GetId(), wantID)
		}
	}
}

// TestSectionsSplit pins the section-splitting rule: each `## ` heading is an
// Asset whose id is path + separator + heading, the heading line is not part
// of the content, deeper headings stay content, and the provenance names the
// heading's line.
func TestSectionsSplit(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", ""+
		"# Doc title\n\n"+
		"preamble that is not an asset\n\n"+
		"## Install\n\n"+
		"install body\n\n"+
		"### subheading stays content\n\n"+
		"## Usage\n\n"+
		"usage body\n")

	got := assetsOf(t, openPool(t, path, true))
	if len(got) != 2 {
		t.Fatalf("yielded %d assets, want 2", len(got))
	}

	wantIDs := []string{
		path + "::Install",
		path + "::Usage",
	}
	for i, a := range got {
		if a.GetId() != wantIDs[i] {
			t.Errorf("section %d has id %q, want %q", i, a.GetId(), wantIDs[i])
		}
	}
	// Bodies are verbatim — content is passed through unmodified, so only the
	// heading line itself is excluded, never trimmed around.
	if string(got[0].GetContent()) != "\ninstall body\n\n### subheading stays content\n\n" {
		t.Errorf("section 1 content = %q", got[0].GetContent())
	}
	if string(got[1].GetContent()) != "\nusage body\n" {
		t.Errorf("section 2 content = %q", got[1].GetContent())
	}
	if got[0].GetProvenance().GetSourceRef() != path+":5" {
		t.Errorf("section 1 source_ref = %q, want %q (the heading's line)", got[0].GetProvenance().GetSourceRef(), path+":5")
	}
	if got[1].GetProvenance().GetSourceRef() != path+":11" {
		t.Errorf("section 2 source_ref = %q, want %q (the heading's line)", got[1].GetProvenance().GetSourceRef(), path+":11")
	}
}

// TestHeadingContainingTheSeparatorIsEscaped pins the id rule: the separator
// is '::', and a heading containing it is escaped by doubling, so two
// headings produce distinct ids exactly when their texts differ.
func TestHeadingContainingTheSeparatorIsEscaped(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", ""+
		"## a::b\n\nbody one\n\n"+
		"## a::::b\n\nbody two\n")

	got := assetsOf(t, openPool(t, path, true))
	if len(got) != 2 {
		t.Fatalf("yielded %d assets, want 2", len(got))
	}
	wantIDs := []string{
		path + "::a::::b", // "a::b" escaped by doubling
		path + "::a::::::::b",
	}
	for i, a := range got {
		if a.GetId() != wantIDs[i] {
			t.Errorf("section %d has id %q, want %q", i, a.GetId(), wantIDs[i])
		}
	}
	if got[0].GetId() == got[1].GetId() {
		t.Error("the escaping collapsed two distinct headings onto one id")
	}
}

// TestDuplicateSectionHeadingIsFatal: the heading IS the id, so two sections
// sharing one in a file are refused, naming the heading and the line —
// matching the jsonl adapter's duplicate-id refusal.
func TestDuplicateSectionHeadingIsFatal(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", ""+
		"## Install\n\nbody one\n\n"+
		"## Install\n\nbody two\n")

	seen, gotErr := firstError(t, openPool(t, path, true))
	if gotErr == nil {
		t.Fatalf("a duplicate section heading was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), `duplicate section heading "Install"`) {
		t.Errorf("error = %q, want it to name the duplicated heading", gotErr)
	}
}

// TestSameHeadingAcrossFilesIsDistinct: the id is path + heading, so the same
// heading in two files names two different Assets — the duplicate refusal is
// scoped to one file, where the ids would collide.
func TestSameHeadingAcrossFilesIsDistinct(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"a.md": "## Install\n\nbody a\n",
		"b.md": "## Install\n\nbody b\n",
	})

	got := assetsOf(t, openPool(t, dir, true))
	if len(got) != 2 {
		t.Fatalf("yielded %d assets, want 2", len(got))
	}
	if got[0].GetId() == got[1].GetId() {
		t.Error("the same heading in two files collided onto one id")
	}
}

// TestFrontMatterReachesTheAsset: kind and tags from the block land on the
// Asset — for whole files and for every section of a split file — and are
// stripped from the content.
func TestFrontMatterReachesTheAsset(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		split    bool
		body     string
		wantN    int
		wantKind knov1.Kind
	}{
		{
			"whole file", false,
			"---\nkind: knowledge\ntags: billing; policy\n---\n# Body\n\ncontent\n",
			1, knov1.Kind_KIND_KNOWLEDGE,
		},
		{
			"split file", true,
			"---\nkind: behavior\ntags: one; two\n---\n## Alpha\n\nalpha body\n## Beta\n\nbeta body\n",
			2, knov1.Kind_KIND_BEHAVIOR,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeFile(t, t.TempDir(), "doc.md", tc.body)
			got := assetsOf(t, openPool(t, path, tc.split))
			if len(got) != tc.wantN {
				t.Fatalf("yielded %d assets, want %d", len(got), tc.wantN)
			}
			for _, a := range got {
				if strings.Contains(string(a.GetContent()), "---") {
					t.Errorf("content = %q; front matter must be stripped, not content", a.GetContent())
				}
				if a.GetKind() != tc.wantKind {
					t.Errorf("kind = %v, want %v", a.GetKind(), tc.wantKind)
				}
				if len(a.GetTags()) != 2 {
					t.Errorf("tags = %v, want the file's declared tags", a.GetTags())
				}
				if !a.GetUserOverridden() {
					t.Error("a file that named a kind must set user_overridden; the report cannot " +
						"otherwise tell an asserted routing decision from a measured one")
				}
			}
		})
	}
}

// TestFrontMatterMalformedIsFatal: the minimal parser is fail-closed. An
// unterminated block, an unknown key, a duplicated key, and a line that is
// not `key: value` are all refused, named, never guessed around.
func TestFrontMatterMalformedIsFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, body, wantSubstr string
	}{
		{"unterminated", "---\nkind: knowledge\n", "unterminated"},
		{"unknown key", "---\nkind: knowledge\ntitle: T\n---\nbody\n", `unknown key "title"`},
		{"duplicate key", "---\nkind: knowledge\nkind: behavior\n---\nbody\n", `duplicate key "kind"`},
		{"not a key value line", "---\nkind knowledge\n---\nbody\n", "is not a"},
		{"empty key", "---\n: knowledge\n---\nbody\n", "empty key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeFile(t, t.TempDir(), "doc.md", tc.body)
			seen, gotErr := firstError(t, openPool(t, path, false))
			if gotErr == nil {
				t.Fatalf("malformed front matter was tolerated; %d assets yielded", seen)
			}
			if !strings.Contains(gotErr.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", gotErr, tc.wantSubstr)
			}
		})
	}
}

// TestFrontMatterDoesNotParseYAML: quoted values are not unquoted — this is
// a minimal parse, not YAML, and the refusal is loud on purpose.
func TestFrontMatterDoesNotParseYAML(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md",
		"---\nkind: \"knowledge\"\n---\nbody\n")
	_, gotErr := firstError(t, openPool(t, path, false))
	if gotErr == nil {
		t.Fatal("a quoted kind was accepted as if the quotes were not there")
	}
	if !strings.Contains(gotErr.Error(), "unknown kind") {
		t.Errorf("error = %q, want the unknown-kind refusal", gotErr)
	}
}

// TestPreambleIsNotAnAsset: content before the first `## ` heading is the
// document's own introduction, not a candidate for a portfolio.
func TestPreambleIsNotAnAsset(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", ""+
		"# Doc title\n\nintro paragraph\n\n"+
		"## Alpha\n\nalpha body\n")

	got := assetsOf(t, openPool(t, path, true))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1 (the preamble is not an Asset)", len(got))
	}
	if got[0].GetId() != path+"::Alpha" {
		t.Errorf("id = %q, want the section id", got[0].GetId())
	}
}

// TestNoSectionsMeansWholeFile: a split request over a file with no `## `
// headings cannot split anything, so the whole content is one Asset with the
// file-path id — nothing is dropped and nothing is fabricated.
func TestNoSectionsMeansWholeFile(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", "# Only a title\n\nbody\n")

	got := assetsOf(t, openPool(t, path, true))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	if got[0].GetId() != path {
		t.Errorf("id = %q, want the file path", got[0].GetId())
	}
	if string(got[0].GetContent()) != "# Only a title\n\nbody\n" {
		t.Errorf("content = %q, want the whole file", got[0].GetContent())
	}
}

// TestEmptyDirectoryYieldsNothing: an empty pool is a decision for the caller
// to refuse before it spends, not an error for the adapter to invent.
func TestEmptyDirectoryYieldsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"empty dir", map[string]string{}},
		{"no markdown files", map[string]string{"notes.txt": "not markdown\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := writeDir(t, tc.files)
			seen, gotErr := firstError(t, openPool(t, dir, false))
			if gotErr != nil {
				t.Errorf("an empty pool was reported as an error: %v", gotErr)
			}
			if seen != 0 {
				t.Errorf("yielded %d assets from an empty directory", seen)
			}
		})
	}
}

// TestEmptyFileIsFatal: a .md file in the pool is a declared document, so an
// empty one is a malformed Asset — refused, never skipped and never counted.
func TestEmptyFileIsFatal(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{"empty.md": ""})
	seen, gotErr := firstError(t, openPool(t, dir, false))
	if gotErr == nil {
		t.Fatalf("an empty file was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), "has no content") {
		t.Errorf("error = %q, want the no-content refusal", gotErr)
	}
}

// TestEmptySectionIsFatal: a heading with no body is an Asset with no
// content, and no content means no measurable value — refused, named.
func TestEmptySectionIsFatal(t *testing.T) {
	t.Parallel()

	path := writeFile(t, t.TempDir(), "doc.md", "## Alpha\n\n## Beta\n\nbeta body\n")
	seen, gotErr := firstError(t, openPool(t, path, true))
	if gotErr == nil {
		t.Fatalf("an empty section was accepted; %d assets yielded", seen)
	}
	if !strings.Contains(gotErr.Error(), `section "Alpha"`) {
		t.Errorf("error = %q, want it to name the empty section", gotErr)
	}
	if !strings.Contains(gotErr.Error(), "has no content") {
		t.Errorf("error = %q, want the no-content refusal", gotErr)
	}
}

// TestProvenanceTracesBackToTheFile: an Asset that cannot be traced to the
// file it came from cannot be audited.
func TestProvenanceTracesBackToTheFile(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"a.md": "# A\n\nbody a\n",
		"b.md": "# B\n\nbody b\n",
	})

	got := assetsOf(t, openPool(t, dir, false))
	if len(got) != 2 {
		t.Fatalf("yielded %d assets, want 2", len(got))
	}
	for _, a := range got {
		if a.GetProvenance().GetSource() != "markdown" {
			t.Errorf("asset %s: source = %q, want %q", a.GetId(), a.GetProvenance().GetSource(), "markdown")
		}
		if got := a.GetProvenance().GetSourceRef(); got != a.GetId() {
			t.Errorf("asset %s: source_ref = %q, want the file path %q", a.GetId(), got, a.GetId())
		}
	}
}

// TestReadingTwiceProducesIdenticalAssets guards the ingestion timestamp that
// is deliberately not set: a pool that has not moved must not look changed on
// every read.
func TestReadingTwiceProducesIdenticalAssets(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"a.md": "# A\n\nbody a\n",
		"b.md": "# B\n\nbody b\n",
	})
	p := openPool(t, dir, false)

	first, second := assetsOf(t, p), assetsOf(t, p)
	if len(first) != len(second) {
		t.Fatalf("two reads of one source yielded %d and %d assets", len(first), len(second))
	}
	for i := range first {
		if first[i].GetProvenance().GetIngestedAt() != second[i].GetProvenance().GetIngestedAt() {
			t.Error("two reads of an unchanged pool disagreed on ingested_at; " +
				"a pool that has not moved would look changed on every read")
		}
	}
}

// TestContextTokensRankRatherThanReserve is the guard on docs/debt.md#68. See
// the jsonl adapter's test for the full reasoning; the assertion is on the
// RATIO, not merely on the arithmetic.
func TestContextTokensRankRatherThanReserve(t *testing.T) {
	t.Parallel()

	const (
		size      = 3600
		wantExact = 1000 // 3.6 bytes/token, prose-centered
	)

	// The content must be EXACTLY `size` bytes for the ratio to come out
	// exact, so the file holds nothing but the payload.
	path := writeFile(t, t.TempDir(), "doc.md", strings.Repeat("a", size))

	got := assetsOf(t, openPool(t, path, false))
	if len(got) != 1 {
		t.Fatalf("yielded %d assets, want 1", len(got))
	}
	if tokens := got[0].GetCost().GetContextTokens(); tokens != wantExact {
		t.Errorf("context_tokens = %d for ~%d bytes of prose, want %d",
			tokens, size, wantExact)
	}
}

// TestCancellationStopsIterationMidway: iter.Seq2 carries no cancellation
// channel of its own, so the producer checks ctx before each yield. Without
// it, a user's Ctrl-C keeps a large pool streaming.
func TestCancellationStopsIterationMidway(t *testing.T) {
	t.Parallel()

	files := make(map[string]string, 10)
	for i := range 10 {
		files[fmt.Sprintf("a%02d.md", i)] = fmt.Sprintf("# A%02d\n\nbody %d\n", i, i)
	}
	p := openPool(t, writeDir(t, files), false)

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

// TestRetainedAssetDoesNotCorruptTheNext is the borrow contract from the
// producer's side. A consumer that holds or edits a borrowed Asset must not
// silently rewrite the Assets that follow.
func TestRetainedAssetDoesNotCorruptTheNext(t *testing.T) {
	t.Parallel()

	dir := writeDir(t, map[string]string{
		"a.md": "## Alpha\n\nbody-a\n\n## Alpha Too\n\nbody-a2\n",
		"b.md": "## Beta\n\nbody-b\n",
	})
	p := openPool(t, dir, true)

	seq, err := p.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	wantBodies := []string{"\nbody-a\n\n", "\nbody-a2\n", "\nbody-b\n"}
	var retained []*core.Asset
	var i int
	for a, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		if string(a.GetContent()) != wantBodies[i] {
			t.Errorf("asset %d has content %q, want %q; a previous iteration's "+
				"retained value leaked into it", i, a.GetContent(), wantBodies[i])
		}

		// A badly-behaved consumer: keeps the pointer and edits it in place.
		retained = append(retained, a)
		a.Content = []byte("overwritten by the consumer")
		a.Destination = knov1.Destination_DESTINATION_CONTEXT
		i++
	}

	if len(retained) != len(wantBodies) {
		t.Fatalf("yielded %d assets, want %d", len(retained), len(wantBodies))
	}
	seenIDs := make(map[string]struct{}, len(retained))
	for _, a := range retained {
		if _, dup := seenIDs[a.GetId()]; dup {
			t.Errorf("two retained assets share the id %q; the producer handed out "+
				"one value repeatedly", a.GetId())
		}
		seenIDs[a.GetId()] = struct{}{}
	}
}

// TestFileErrorsSurfaceOnOpen: an unreadable source fails when the iterator
// is requested, not partway through a run that has already spent money.
func TestFileErrorsSurfaceOnOpen(t *testing.T) {
	t.Parallel()

	p := openPool(t, filepath.Join(t.TempDir(), "absent.md"), false)
	if _, err := p.Assets(context.Background()); err == nil {
		t.Error("a missing path was accepted")
	}
}

// TestNewRejectsBadOptions catches configuration errors before a run starts.
func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	if _, err := poolmd.New(poolmd.Options{}); err == nil {
		t.Error("an empty path was accepted")
	}
}
