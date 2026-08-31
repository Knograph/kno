package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spendRendererFile is the one file allowed to read the contents of a
// budget.Spend. See cli/spend.go's header for the rule.
const spendRendererFile = "spend.go"

// spendLineFormat is the human spend line, byte-identical to the one
// kno baseline has printed since v0.1.
const spendLineFormat = "  spent      %s over %d call(s)\n"

// TestSpendFieldsAreReadInOneFile is the mechanical form of "one spend
// renderer".
//
// It enumerates no callers, deliberately. The obvious test — an allowlist of
// formatUSD's callers — polices the wrong noun: formatUSD renders cost caps,
// pre-run estimates and asset carrying costs as well as spend, so such a list
// fails on the next legitimate cap rendering and gets edited to stay green. A
// test that must be edited to stay green is a test that will be.
//
// The rule here is about spend and nothing else: outside spend.go, no file in
// cli/ may reach THROUGH a spend value into its fields. Passing a whole
// budget.Spend to a renderer is one selector and stays legal anywhere;
// formatting its contents is confined to one file. Syntactic, so it needs no
// type information, and it fails on exactly the thing that matters — a second
// private spend formatter drifting from the first.
func TestSpendFieldsAreReadInOneFile(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	for _, path := range nonTestGoFiles(t) {
		if filepath.Base(path) == spendRendererFile {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			outer, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !reachesIntoSpend(inner.Sel.Name) {
				return true
			}
			t.Errorf("%s reads %s.%s. Spend fields are formatted in cli/%s and "+
				"nowhere else — pass the whole budget.Spend to spendLines or "+
				"newSpendReport instead, or the human line and the --json block "+
				"will drift apart",
				fset.Position(outer.Pos()), inner.Sel.Name, outer.Sel.Name,
				spendRendererFile)
			return true
		})
	}
}

// reachesIntoSpend reports whether a field of this name holds a spend value.
//
// Name-shaped rather than type-shaped, so the test needs no type checker: a
// field called Spent, or one whose name ends in Spend, holds money that was
// settled. The naming convention is the enforcement surface, which is why
// cli/spend.go's own accessors are named this way.
func reachesIntoSpend(field string) bool {
	return field == "Spent" || strings.HasSuffix(field, "Spend")
}

// TestSpendLineFormatIsWrittenOnce pins the companion half: the human spend
// line's format string appears in exactly one function.
//
// Separate from the test above because a private copy could be written
// without ever naming a spend field — someone with a budget.Spend in hand
// could Fprintf the same line from a local variable. This catches the string.
func TestSpendLineFormatIsWrittenOnce(t *testing.T) {
	t.Parallel()

	var found []string
	for _, path := range nonTestGoFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(b), strings.ReplaceAll(spendLineFormat, "\n", `\n`)) {
			found = append(found, filepath.Base(path))
		}
	}
	if len(found) != 1 || found[0] != spendRendererFile {
		t.Errorf("the spend line is formatted in %v, want only %s. A second copy is "+
			"how the two renderings start disagreeing about what a stage cost",
			found, spendRendererFile)
	}
}

// nonTestGoFiles lists this package's source, excluding tests.
func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	return out
}
