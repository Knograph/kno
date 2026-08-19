// Command godoccheck enforces that every exported symbol has a doc comment.
//
// CLAUDE.md requires it, and `make docs` gates on it. The rule matters more
// here than in most repositories: core, the adapter interfaces, and the
// generated proto are the public Go API, and an undocumented exported symbol
// ships as an undocumented part of that API.
//
// Generated code is skipped. It is not ours to document, and its comments come
// from the .proto source, where buf lint's COMMENTS category already enforces
// them.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skipPrefixes mirrors covercheck's and .golangci.yml's exclusions. Keeping
// the three in agreement is deliberate: a path excluded from linting but not
// from doc checking would produce findings nobody can act on.
var skipPrefixes = []string{"gen/", "tools/", "bin/"}

type finding struct {
	pos  string
	kind string
	name string
}

func main() {
	root := flag.String("root", ".", "directory to walk")
	flag.Parse()

	findings, err := walk(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "godoccheck: %v\n", err)
		os.Exit(1)
	}

	if len(findings) == 0 {
		fmt.Println("godoccheck: ok — every exported symbol is documented")
		return
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].pos < findings[j].pos })
	for _, f := range findings {
		fmt.Printf("  %s: exported %s %s has no doc comment\n", f.pos, f.kind, f.name)
	}
	fmt.Printf("godoccheck: FAILED — %d undocumented exported symbols\n", len(findings))
	os.Exit(1)
}

func walk(root string) ([]finding, error) {
	var out []finding
	fset := token.NewFileSet()
	ctx := build.Default

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "bin" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Relative to the walk ROOT, not the working directory: computing this
		// against CWD meant the skip list silently did nothing whenever the
		// tool was invoked with an absolute root.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
		for _, p := range skipPrefixes {
			if strings.HasPrefix(rel, p) {
				return nil
			}
		}
		// Honor build constraints so a file excluded on this platform is not
		// reported — it is not part of this build's public surface.
		ok, err := ctx.MatchFile(filepath.Dir(path), d.Name())
		if err != nil || !ok {
			return nil //nolint:nilerr // an unmatchable file is simply not in this build
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		out = append(out, inspect(fset, file)...)
		return nil
	})
	return out, err
}

func inspect(fset *token.FileSet, file *ast.File) []finding {
	var out []finding
	report := func(pos token.Pos, kind, name string, doc *ast.CommentGroup) {
		if !ast.IsExported(name) || doc != nil {
			return
		}
		out = append(out, finding{pos: fset.Position(pos).String(), kind: kind, name: name})
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				name = receiverName(d.Recv.List[0].Type) + "." + name
				// An unexported receiver's methods are not part of the public
				// API even when the method name is capitalized.
				if !ast.IsExported(strings.TrimPrefix(receiverName(d.Recv.List[0].Type), "*")) {
					continue
				}
			}
			report(d.Pos(), kind, name, d.Doc)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					doc := sp.Doc
					if doc == nil {
						// `type Foo struct{}` carries its comment on the
						// GenDecl when it is not part of a parenthesized block.
						doc = d.Doc
					}
					report(sp.Pos(), "type", sp.Name.Name, doc)
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						doc := sp.Doc
						if doc == nil {
							doc = d.Doc
						}
						report(n.Pos(), "value", n.Name, doc)
					}
				}
			}
		}
	}
	return out
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	}
	return ""
}
