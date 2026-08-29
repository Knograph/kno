package cli

import (
	"strings"
	"testing"

	poolcsv "github.com/knograph/kno/adapters/pool/csv"
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
