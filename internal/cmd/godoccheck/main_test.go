package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInspect is the test that makes this tool real.
//
// A doc-coverage checker that has only ever been run against a compliant tree
// has not been shown to work — it is indistinguishable from a program that
// prints "ok" unconditionally. This exercises both directions on every
// declaration kind it claims to cover.
func TestInspect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want []string // substrings expected in the findings, empty means clean
	}{
		{
			name: "documented func is accepted",
			src: `package p
// Do does a thing.
func Do() {}`,
		},
		{
			name: "undocumented exported func is caught",
			src: `package p
func Do() {}`,
			want: []string{"func Do"},
		},
		{
			name: "unexported func is not the public API",
			src: `package p
func do() {}`,
		},
		{
			name: "undocumented exported type is caught",
			src: `package p
type Thing struct{}`,
			want: []string{"type Thing"},
		},
		{
			name: "type documented on the GenDecl counts",
			src: `package p
// Thing is a thing.
type Thing struct{}`,
		},
		{
			name: "undocumented const and var are caught",
			src: `package p
const Limit = 3
var Default = 4`,
			want: []string{"value Limit", "value Default"},
		},
		{
			name: "documented members of a parenthesized block are accepted",
			src: `package p
const (
	// A is a.
	A = 1
	// B is b.
	B = 2
)`,
		},
		{
			name: "undocumented member inside a block is still caught",
			src: `package p
const (
	// A is a.
	A = 1
	B = 2
)`,
			want: []string{"value B"},
		},
		{
			name: "exported method on exported type is caught",
			src: `package p
// Thing is a thing.
type Thing struct{}
func (t Thing) Do() {}`,
			want: []string{"method Thing.Do"},
		},
		{
			name: "exported method on UNEXPORTED type is not public API",
			src: `package p
type thing struct{}
func (t thing) Do() {}`,
		},
		{
			name: "pointer receiver resolves to the type name",
			src: `package p
// Thing is a thing.
type Thing struct{}
func (t *Thing) Do() {}`,
			want: []string{"method Thing.Do"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tc.src, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing test source: %v", err)
			}

			got := inspect(fset, file)
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("want no findings, got %d: %+v", len(got), got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %d findings, got %d: %+v", len(tc.want), len(got), got)
			}
			for i, want := range tc.want {
				gotStr := got[i].kind + " " + got[i].name
				if !strings.Contains(gotStr, want) {
					t.Errorf("finding %d = %q, want it to contain %q", i, gotStr, want)
				}
			}
		})
	}
}

// TestReceiverName covers the receiver shapes the walker must resolve,
// including the generic case — easy to omit, and omitting it silently drops
// every method on a generic type from the check.
func TestReceiverName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, src, want string
	}{
		{"value receiver", "package p\ntype T struct{}\nfunc (t T) M() {}", "T"},
		{"pointer receiver", "package p\ntype T struct{}\nfunc (t *T) M() {}", "T"},
		{"generic value receiver", "package p\ntype T[A any] struct{}\nfunc (t T[A]) M() {}", "T"},
		{"generic pointer receiver", "package p\ntype T[A any] struct{}\nfunc (t *T[A]) M() {}", "T"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}

			var checked int
			for _, d := range file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
					continue
				}
				checked++
				if got := receiverName(fn.Recv.List[0].Type); got != tc.want {
					t.Errorf("receiverName = %q, want %q", got, tc.want)
				}
			}
			if checked == 0 {
				t.Fatal("no method found in the test source; the test is not exercising anything")
			}
		})
	}
}

// TestWalk exercises the filesystem traversal end to end: the skip list, the
// test-file exclusion, and that findings carry a real position.
func TestWalk(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	write("good/good.go", "// Package good is documented.\npackage good\n\n// Do does.\nfunc Do() {}\n")
	write("bad/bad.go", "// Package bad is documented.\npackage bad\n\nfunc Undocumented() {}\n")
	// Generated code is not ours to document; its comments come from .proto,
	// where buf lint's COMMENTS category already enforces them.
	write("gen/kno/v1/x.pb.go", "package v1\n\nfunc Generated() {}\n")
	// Test files are not public API.
	write("good/good_test.go", "package good\n\nfunc Helper() {}\n")
	// testdata is skipped wholesale.
	write("testdata/sample.go", "package sample\n\nfunc Fixture() {}\n")

	got, err := walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].name != "Undocumented" {
		t.Errorf("finding = %q, want Undocumented", got[0].name)
	}
	if !strings.Contains(got[0].pos, "bad.go") {
		t.Errorf("finding position = %q, want it to name bad.go", got[0].pos)
	}
}

// TestWalkCleanTreeIsSilent guards against the failure mode where the walker
// reports nothing because it traversed nothing.
func TestWalkCleanTreeIsSilent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"),
		[]byte("// Package p is documented.\npackage p\n\n// Do does.\nfunc Do() {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no findings on a clean tree, got %+v", got)
	}
}

// TestUnexportedMethodOnExportedTypeIsNotPublicAPI guards a bug this tool
// shipped with: it reported every unexported method on an exported type.
//
// The check was applied to the qualified name "Guard.reserve", which starts
// with the type's capital letter and therefore looks exported. Visibility is
// decided by the method identifier alone.
func TestUnexportedMethodOnExportedTypeIsNotPublicAPI(t *testing.T) {
	t.Parallel()

	src := `package p
// Guard guards.
type Guard struct{}
func (g *Guard) reserve() {}
func (g *Guard) Authorize() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got := inspect(fset, file)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 finding (the exported Authorize), got %d: %+v", len(got), got)
	}
	if got[0].name != "Guard.Authorize" {
		t.Errorf("flagged %q; only the exported method is public API", got[0].name)
	}
}
