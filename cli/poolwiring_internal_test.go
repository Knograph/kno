package cli

import (
	"strings"
	"testing"

	poolcsv "github.com/knograph/kno/adapters/pool/csv"
	poolhf "github.com/knograph/kno/adapters/pool/hf"
	pooljsonl "github.com/knograph/kno/adapters/pool/jsonl"
	poolmarkdown "github.com/knograph/kno/adapters/pool/markdown"
)

// TestPoolGrammarBarePathIsJSONL: a path with no prefix is the jsonl
// adapter, unchanged — the default a bare --pool has always meant.
func TestPoolGrammarBarePathIsJSONL(t *testing.T) {
	t.Parallel()

	p, err := resolvePool("assets.jsonl", false)
	if err != nil {
		t.Fatalf("resolvePool: %v", err)
	}
	if _, ok := p.(*pooljsonl.Pool); !ok {
		t.Fatalf("resolvePool(bare path) = %T, want *jsonl.Pool", p)
	}
}

// TestPoolGrammarCSVPrefix: the csv: prefix selects the csv adapter and
// strips the prefix from the path.
func TestPoolGrammarCSVPrefix(t *testing.T) {
	t.Parallel()

	p, err := resolvePool("csv:assets.csv", false)
	if err != nil {
		t.Fatalf("resolvePool: %v", err)
	}
	if _, ok := p.(*poolcsv.Pool); !ok {
		t.Fatalf("resolvePool(csv:) = %T, want *csv.Pool", p)
	}
}

// TestPoolGrammarMDPrefix: the md: prefix selects the markdown adapter, and
// --split-sections is the knob that reaches it.
func TestPoolGrammarMDPrefix(t *testing.T) {
	t.Parallel()

	p, err := resolvePool("md:docs", false)
	if err != nil {
		t.Fatalf("resolvePool: %v", err)
	}
	if _, ok := p.(*poolmarkdown.Pool); !ok {
		t.Fatalf("resolvePool(md:) = %T, want *markdown.Pool", p)
	}

	if _, err := resolvePool("md:", false); err == nil {
		t.Error("an empty md: path was accepted")
	}
	if _, err := resolvePool("csv:", false); err == nil {
		t.Error("an empty csv: path was accepted")
	}
}

// TestSplitSectionsRefusedOutsideMarkdown: a flag that silently does nothing
// for a source it cannot affect is a flag a user stops believing in, so the
// refusal names the only grammar it applies to.
func TestSplitSectionsRefusedOutsideMarkdown(t *testing.T) {
	t.Parallel()

	for _, pool := range []string{"csv:assets.csv", "assets.jsonl"} {
		_, err := resolvePool(pool, true)
		if err == nil {
			t.Errorf("--split-sections was accepted for %s", pool)
			continue
		}
		if !strings.Contains(err.Error(), "md:") {
			t.Errorf("refusal for %s does not name the md: grammar: %v", pool, err)
		}
	}

	if _, err := resolvePool("md:docs", true); err != nil {
		t.Errorf("--split-sections with an md: pool was refused: %v", err)
	}
}

// TestPoolGrammarHFPrefix: the hf: prefix selects the Hugging Face adapter
// and demands its kind — the kind is a routing decision, declared in the
// address, never guessed.
func TestPoolGrammarHFPrefix(t *testing.T) {
	t.Parallel()

	p, err := resolvePool("hf:org/name/main/train:knowledge", false)
	if err != nil {
		t.Fatalf("resolvePool: %v", err)
	}
	if _, ok := p.(*poolhf.Pool); !ok {
		t.Fatalf("resolvePool(hf:) = %T, want *hf.Pool", p)
	}

	// An undeclared kind is refused, not defaulted.
	_, err = resolvePool("hf:org/name/main/train", false)
	if err == nil {
		t.Error("an hf: pool without a declared kind was accepted")
	} else if !strings.Contains(err.Error(), ":<kind>") {
		t.Errorf("the kind refusal does not name the grammar: %v", err)
	}

	// A misspelled kind is refused with the closed spelling.
	_, err = resolvePool("hf:org/name/main/train:bogus", false)
	if err == nil {
		t.Error("an hf: pool with an unknown kind was accepted")
	} else if !strings.Contains(err.Error(), "knowledge") {
		t.Errorf("the kind refusal does not name the valid spellings: %v", err)
	}
}

// TestPoolGrammarHFRefusals: the hf: pool grammar is four slash-separated
// segments plus a colon-kind, and every refusal names the grammar.
func TestPoolGrammarHFRefusals(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"hf:org/name/main:knowledge",    // three segments
		"hf:org/name/main/train/more:x", // five segments
		"hf:org//main/train:knowledge",  // empty segment
		"hf:org/name/main/train:",       // empty kind
	} {
		_, err := resolvePool(path, false)
		if err == nil {
			t.Errorf("resolvePool(%q) accepted a malformed hf: pool", path)
			continue
		}
		if !strings.Contains(err.Error(), "hf:<org>/<name>/<config>/<split>") {
			t.Errorf("resolvePool(%q) refusal does not name the grammar: %v", path, err)
		}
	}

	// --split-sections cannot affect an hf: pool.
	_, err := resolvePool("hf:org/name/main/train:knowledge", true)
	if err == nil {
		t.Error("--split-sections was accepted for an hf: pool")
	} else if !strings.Contains(err.Error(), "md:") {
		t.Errorf("the --split-sections refusal does not name the md: grammar: %v", err)
	}
}
