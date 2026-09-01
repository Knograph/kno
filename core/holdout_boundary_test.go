package core

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOnlyValidateOpensTheHoldout is the in-package half of the holdout
// boundary.
//
// IT DOES NOT INHERIT boundary_test.go's ROBUSTNESS, and this comment exists so
// nobody assumes it does. TestCoreImportsNothingAbove shells out to
// `go list -deps` and asserts over the IMPORT GRAPH — a coarse, total relation
// that is hard to fool and impossible to half-satisfy. This test parses core's
// own non-test files with go/ast and reasons about occurrences INSIDE a single
// package, where the import graph says nothing at all. Different technique,
// different failure modes: an AST walk can be evaded by an alias, a method
// value, or a wrapper it does not model, and it is the walk itself — not the
// toolchain — that has to be reviewed. It is worth having anyway, because
// nothing else can catch a second in-package call site. See docs/debt.md.
//
// It counts ast.CallExpr nodes and NOT identifier occurrences, and that is a
// correctness requirement rather than a refinement. The declaration
// `func openHoldout(...)` in core/holdout.go is itself an occurrence of the
// identifier, so the naive spec — "referenced in exactly one file, and that
// file is core/validate.go" — FAILS ON A CORRECT IMPLEMENTATION. That is worse
// than a merely wrong test: the cheapest way to turn it green is to loosen the
// walk until it also stops seeing the second call site the test exists to
// catch, converting a security boundary into a formality. Both directions are
// therefore asserted — deleting the call fails as loudly as adding one, and a
// walk loosened into matching nothing fails too.
func TestOnlyValidateOpensTheHoldout(t *testing.T) {
	t.Parallel()

	calls := countCallsByFile(t, ".", "openHoldout")

	// Every count is asserted, including the zeroes. A walk that has been
	// broken into matching nothing fails the validate.go assertion; a walk
	// that has grown a second call site fails one of the others.
	if got := calls["holdout.go"]; got != 0 {
		t.Errorf("core/holdout.go calls openHoldout %d time(s); it should only DECLARE it. "+
			"The declaration is not a call, and a call from the file that defines the "+
			"opener would be a second path into the holdout.", got)
	}
	if got := calls["validate.go"]; got != 1 {
		t.Errorf("core/validate.go calls openHoldout %d time(s), want exactly 1.\n"+
			"Zero means either the stage stopped opening the holdout or this walk stopped "+
			"seeing calls — and a gate that matches nothing is worse than no gate, because "+
			"it reads green. More than one means the stage opens the holdout twice.", got)
	}
	for file, n := range calls {
		if file == "holdout.go" || file == "validate.go" {
			continue
		}
		if n != 0 {
			t.Errorf("core/%s calls openHoldout %d time(s). Validate is the only stage "+
				"permitted to read SPLIT_HOLDOUT Cases (run.proto's Stage godoc), and "+
				"core/validate.go is the only file permitted to open one.", file, n)
		}
	}
}

// TestHoldoutEvalsIsNotReExportedFromCore is the concrete-type half of the
// exposure check.
//
// The compiler already guarantees this for every OTHER package — *holdoutEvals
// is unnameable outside core — so what this catches is an accidental
// re-export WITHIN core: an exported function, method or field whose signature
// mentions the concrete type, which would hand the value outward under a name
// no outside caller can write but every outside caller can use.
func TestHoldoutEvalsIsNotReExportedFromCore(t *testing.T) {
	t.Parallel()

	found := exportedSurfacesMentioning(t, ".", "holdoutEvals")
	if len(found) > 0 {
		t.Errorf("these exported surfaces in core mention *holdoutEvals:\n  %s\n"+
			"The holdout reader is unconstructible outside core; an exported surface "+
			"carrying it makes it holdable instead.", strings.Join(found, "\n  "))
	}
}

// evalsOutletAllowlist is the reviewed set of exported core surfaces whose type
// is the Evals interface.
//
// THIS IS THE INTERFACE-FORWARDING HOLE, and it is why the check below is an
// allowlist rather than an emptiness assertion. "Cannot construct" is not
// "cannot hold": interface satisfaction is indifferent to whether the concrete
// type is exported, so an exported core surface returning an Evals INTERFACE
// value backed by a *holdoutEvals would let an outside caller call its exported
// Cases method and read the holdout without ever naming the type — and
// TestHoldoutEvalsIsNotReExportedFromCore, which matches the concrete type,
// would see only `Evals` and pass.
//
// The plan for this stage specified "assert the enumeration is empty". It
// cannot be empty: the same plan requires ValidateOptions.Evals to be a plain
// Evals, so the design and the check as written contradict each other. The
// resolution keeps the check's PURPOSE, which is to make a human argue: every
// entry here is an INPUT the caller supplies and core only ever reads, so core
// cannot hand a *holdoutEvals out through it. A PR that adds an entry fails
// this test until its author adds it here with the same argument — and that
// argument, not the walk, is what the check is really for.
//
// An OUTPUT position — a function result, a method result, a field core
// assigns, a callback core invokes — must never be added to this list. The walk
// cannot tell inputs from outputs, so the reviewer must.
var evalsOutletAllowlist = map[string]string{
	"field ValidateOptions.Evals": "input: the caller supplies the unsealed source and " +
		"core.Validate opens the holdout from it internally; core never writes this field",
	"param Seal.e": "input: Seal consumes an Evals and returns the sealed concrete type",
}

// TestNoExportedEvalsOutletInCore enumerates every exported Evals-typed surface
// in core and holds it against the reviewed allowlist above.
func TestNoExportedEvalsOutletInCore(t *testing.T) {
	t.Parallel()

	found := exportedEvalsSurfaces(t, ".")
	for _, s := range found {
		if _, ok := evalsOutletAllowlist[s]; !ok {
			t.Errorf("core exposes a new Evals-typed surface: %s\n"+
				"An Evals interface value can be backed by *holdoutEvals, so a NEW outlet "+
				"is a new way for the holdout to leave this package under a type nobody "+
				"had to name. If it is an INPUT the caller supplies and core never writes, "+
				"add it to evalsOutletAllowlist with that argument written down. If it is "+
				"an output, it must not exist.", s)
		}
	}
	for s := range evalsOutletAllowlist {
		if !contains(found, s) {
			t.Errorf("evalsOutletAllowlist names %s, which no longer exists. "+
				"A stale allowlist entry is an argument nobody is making any more; "+
				"delete it.", s)
		}
	}
}

// TestHoldoutOpenerIsUnreachableOutsideCore is the grep-equivalent half: no
// file in the module outside core/ so much as names the opener or its type.
//
// The compiler already refuses both. This asserts the weaker, cheaper property
// anyway, because it is the one that catches a copy of the opener pasted into
// another package under the same name — which compiles, reads identical in
// review, and opens a holdout nothing here can see.
func TestHoldoutOpenerIsUnreachableOutsideCore(t *testing.T) {
	t.Parallel()

	root := ".."
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(rel, "core"+string(filepath.Separator)) {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, name := range []string{"holdoutEvals", "openHoldout"} {
			if strings.Contains(string(src), name) {
				offenders = append(offenders, fmt.Sprintf("%s names %s", rel, name))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("the holdout opener is named outside core:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestCorrectIsNotAppliedInValidate pins a deliberate ABSENCE.
//
// stats/portfolio.Correct is Bonferroni over n_screened — the number of Assets
// a SELECTION considered. Validate screens nothing: it makes one
// pre-registered comparison, decided before any holdout Case was read. A
// Bonferroni factor here would widen the one interval in the product that has
// earned its nominal coverage, and the person who "fixes" this will believe
// they are tightening the statistics.
//
// A call-expression walk with the same count-per-file discipline as
// TestOnlyValidateOpensTheHoldout, and not a bare identifier search, for the
// same reason: a search that also matched a comment would be satisfied by
// deleting the comment.
func TestCorrectIsNotAppliedInValidate(t *testing.T) {
	t.Parallel()

	calls := countCallsByFile(t, ".", "Correct")
	for _, file := range []string{"validate.go", "validate_loop.go", "validate_measure.go", "holdout.go"} {
		if got := calls[file]; got != 0 {
			t.Errorf("core/%s calls Correct %d time(s). Validate makes ONE pre-registered "+
				"comparison and screens nothing; correcting it for multiplicity would "+
				"widen the only interval in this product that has earned its nominal "+
				"coverage.", file, got)
		}
	}
	// The counterpart assertion: Select DOES correct, so a walk that matched
	// nothing at all would be broken rather than reassuring.
	if got := calls["select.go"]; got == 0 {
		t.Error("core/select.go calls Correct 0 times. Either Select stopped correcting " +
			"for the Assets it screened, or this walk stopped seeing calls — and a walk " +
			"that matches nothing makes the assertion above vacuous.")
	}
}

// countCallsByFile counts ast.CallExpr nodes whose callee is the named
// identifier, per file, over a package's non-test Go files.
//
// The callee is matched as a bare *ast.Ident or as the selector half of a
// qualified call (pkg.Name), so `portfolio.Correct(...)` and `Correct(...)`
// both count. A doc comment naming the function does not, and neither does the
// ast.FuncDecl that declares it — which is the whole reason this counts calls
// rather than identifiers.
func countCallsByFile(t *testing.T, dir, name string) map[string]int {
	t.Helper()

	out := make(map[string]int)
	for base, file := range parsePackage(t, dir) {
		out[base] += 0 // every file is present, so a zero is asserted rather than absent
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == name {
					out[base]++
				}
			case *ast.SelectorExpr:
				if fn.Sel != nil && fn.Sel.Name == name {
					out[base]++
				}
			}
			return true
		})
	}
	return out
}

// parsePackage parses a package directory's non-test Go files, keyed by base
// name.
//
// Per-file rather than parser.ParseDir, which is deprecated: ParseDir does not
// consider build tags when associating files with packages. core has no
// build-tagged files today, and this walk must not start depending on that.
func parsePackage(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := make(map[string]*ast.File, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		out[name] = file
	}
	return out
}

// exportedSurfacesMentioning lists exported functions, methods and struct
// fields in a package whose type mentions the named type.
func exportedSurfacesMentioning(t *testing.T, dir, typeName string) []string {
	t.Helper()

	var found []string
	forEachExportedSurface(t, dir, func(label string, expr ast.Expr) {
		if typeMentions(expr, typeName) {
			found = append(found, label)
		}
	})
	sort.Strings(found)
	return found
}

// exportedEvalsSurfaces lists every exported surface in a package whose type is
// (or contains) the Evals interface.
func exportedEvalsSurfaces(t *testing.T, dir string) []string {
	t.Helper()
	return exportedSurfacesMentioning(t, dir, "Evals")
}

// forEachExportedSurface visits every exported function result, method result,
// exported struct field, and function parameter in a package, with a label
// naming it.
//
// Parameters are visited as well as results because a CALLBACK parameter is an
// outlet in the other direction: core invoking a caller-supplied
// func(Evals) hands the value outward just as surely as returning one.
// Distinguishing a callback from an ordinary input is what the allowlist's
// written argument is for.
func forEachExportedSurface(t *testing.T, dir string, visit func(label string, expr ast.Expr)) {
	t.Helper()

	for _, file := range parsePackage(t, dir) {
		{
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !d.Name.IsExported() {
						continue
					}
					name := d.Name.Name
					if d.Recv != nil && len(d.Recv.List) > 0 {
						name = receiverName(d.Recv.List[0].Type) + "." + name
					}
					if d.Type.Results != nil {
						for _, r := range d.Type.Results.List {
							visit("result "+name, r.Type)
						}
					}
					if d.Type.Params != nil {
						for _, p := range d.Type.Params.List {
							visit("param "+name+"."+fieldNames(p), p.Type)
						}
					}
				case *ast.GenDecl:
					if d.Tok != token.TYPE {
						continue
					}
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || !ts.Name.IsExported() {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok || st.Fields == nil {
							continue
						}
						for _, f := range st.Fields.List {
							for _, n := range f.Names {
								if !n.IsExported() {
									continue
								}
								visit("field "+ts.Name.Name+"."+n.Name, f.Type)
							}
						}
					}
				}
			}
		}
	}
}

// receiverName renders a method receiver's type name.
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	default:
		return "?"
	}
}

// fieldNames renders a parameter's names, or "_" when it has none.
func fieldNames(f *ast.Field) string {
	if len(f.Names) == 0 {
		return "_"
	}
	names := make([]string, 0, len(f.Names))
	for _, n := range f.Names {
		names = append(names, n.Name)
	}
	return strings.Join(names, ",")
}

// typeMentions reports whether a type expression names the given identifier
// anywhere inside it — as itself, behind a pointer, or inside a slice, map,
// channel or function type.
func typeMentions(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// contains reports whether a sorted-or-not slice holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
