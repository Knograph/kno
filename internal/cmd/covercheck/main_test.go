package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/knograph/kno"

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestParseProfile covers the profile arithmetic: statements are weighted, and
// a statement counts as covered only when its execution count is non-zero.
func TestParseProfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	profile := filepath.Join(dir, "coverage.out")
	writeFile(t, dir, "coverage.out", `mode: atomic
`+module+`/core/errs/errs.go:10.1,12.2 5 1
`+module+`/core/errs/errs.go:14.1,16.2 3 0
`+module+`/stats/budget/budget.go:8.1,9.2 2 7
`+module+`/gen/kno/v1/asset.pb.go:1.1,2.2 100 0
`)

	got, err := parseProfile(profile, module)
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}

	if c := got["core/errs"]; c.covered != 5 || c.total != 8 {
		t.Errorf("core/errs = %+v, want covered 5 of 8", c)
	}
	if c := got["stats/budget"]; c.covered != 2 || c.total != 2 {
		t.Errorf("stats/budget = %+v, want covered 2 of 2", c)
	}
	// Generated code must never enter the numbers. It is full of function
	// bodies and would drag the repo-wide floor down for code nobody tests.
	if _, ok := got["gen/kno/v1"]; ok {
		t.Error("gen/ leaked into the coverage numbers; it must be excluded")
	}
}

// TestHasFunctionBody is the exemption rule, and it is the part most worth
// getting right: keying exemption off "absent from the profile" would exempt a
// package full of real code with zero tests, forever and silently.
func TestHasFunctionBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name:  "doc.go only has nothing to cover",
			files: map[string]string{"doc.go": "// Package p is a charter.\npackage p\n"},
			want:  false,
		},
		{
			name: "interfaces and aliases have no statements",
			files: map[string]string{"iface.go": `package p
type Agent interface{ Do() error }
type Alias = Agent
`},
			want: false,
		},
		{
			name: "a real function body is code",
			files: map[string]string{"impl.go": `package p
func Do() error { return nil }
`},
			want: true,
		},
		{
			name:  "an empty function body is not worth covering",
			files: map[string]string{"impl.go": "package p\nfunc Do() {}\n"},
			want:  false,
		},
		{
			name: "tests alone do not make a package code-bearing",
			files: map[string]string{"p_test.go": `package p
import "testing"
func TestX(t *testing.T) { t.Log("x") }
`},
			want: false,
		},
		{
			name: "a file excluded by build tags was never compiled here",
			files: map[string]string{"other.go": `//go:build plan9 && arm64

package p

func Do() error { return nil }
`},
			// go test only instruments files it compiles. Counting this one
			// would demand coverage for statements that never existed on this
			// platform.
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for name, body := range tc.files {
				writeFile(t, dir, name, body)
			}
			got, err := hasFunctionBody(dir)
			if err != nil {
				t.Fatalf("hasFunctionBody: %v", err)
			}
			if got != tc.want {
				t.Errorf("hasFunctionBody = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCheck exercises the policy itself in BOTH directions. A gate that has
// only ever been seen to pass has not been shown to work — see docs/debt.md#16.
func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pkgs       []pkgCoverage
		baseline   string
		wantFailed bool
	}{
		{
			name: "package above its floor passes",
			pkgs: []pkgCoverage{
				{path: "core/errs", covered: 90, total: 100, inProfile: true, hasCode: true},
			},
		},
		{
			name: "package below its floor FAILS",
			pkgs: []pkgCoverage{
				{path: "core/errs", covered: 80, total: 100, inProfile: true, hasCode: true},
			},
			wantFailed: true,
		},
		{
			name: "stats floor applies to nested packages",
			pkgs: []pkgCoverage{
				{path: "stats/budget", covered: 10, total: 100, inProfile: true, hasCode: true},
			},
			wantFailed: true,
		},
		{
			name: "code-bearing package with NO coverage data FAILS",
			// The dangerous case: real code, zero tests, or a package that
			// failed to build. It must never pass by being absent.
			pkgs: []pkgCoverage{
				{path: "adapters/agent", inProfile: false, hasCode: true},
			},
			wantFailed: true,
		},
		{
			name: "body-less package is exempt, not failed",
			pkgs: []pkgCoverage{
				{path: "core", inProfile: false, hasCode: false, exempt: true, exemptWhy: "no bodies"},
				{path: "core/errs", covered: 90, total: 100, inProfile: true, hasCode: true},
			},
		},
		{
			name: "repo-wide floor FAILS even when every package has a floor pass",
			pkgs: []pkgCoverage{
				// No per-package floor applies to internal/, so only the
				// repo-wide rule can catch this.
				{path: "internal/thing", covered: 10, total: 100, inProfile: true, hasCode: true},
			},
			wantFailed: true,
		},
		{
			name: "coverage decrease against the baseline FAILS",
			pkgs: []pkgCoverage{
				{path: "internal/thing", covered: 80, total: 100, inProfile: true, hasCode: true},
			},
			baseline:   "internal/thing 95.0\n",
			wantFailed: true,
		},
		{
			name: "coverage increase against the baseline passes",
			pkgs: []pkgCoverage{
				{path: "internal/thing", covered: 95, total: 100, inProfile: true, hasCode: true},
			},
			baseline: "internal/thing 80.0\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "baseline")
			if tc.baseline != "" {
				if err := os.WriteFile(path, []byte(tc.baseline), 0o600); err != nil {
					t.Fatalf("writing baseline: %v", err)
				}
			}
			if got := check(path, tc.pkgs); got != tc.wantFailed {
				t.Errorf("check failed = %v, want %v", got, tc.wantFailed)
			}
		})
	}
}

// TestBaselineRoundTrip asserts a written baseline reads back identically and
// omits exempt packages, so the file stays stable across regenerations rather
// than churning in every diff.
func TestBaselineRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "baseline")
	pkgs := []pkgCoverage{
		{path: "core/errs", covered: 90, total: 100, inProfile: true, hasCode: true},
		{path: "core", exempt: true, exemptWhy: "no bodies"},
	}
	if err := writeBaseline(path, pkgs); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}

	got, err := readBaseline(path)
	if err != nil {
		t.Fatalf("readBaseline: %v", err)
	}
	if got["core/errs"] != 90 {
		t.Errorf("core/errs = %v, want 90", got["core/errs"])
	}
	if _, ok := got["core"]; ok {
		t.Error("exempt package was written to the baseline; it has nothing to ratchet")
	}
}

// TestFloorFor pins which packages carry the 85% floor, since CLAUDE.md names
// them explicitly and a silent change here would weaken the rule invisibly.
func TestFloorFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pkg     string
		wantMin float64
		wantOK  bool
	}{
		{"core", 85, true},
		{"core/errs", 85, true},
		{"stats/budget", 85, true},
		{"bridge", 85, true},
		{"plugin", 85, true},
		{"adapters/agent", 0, false},
		{"internal/cmd/covercheck", 0, false},
		// Must not match by bare prefix: "coreutils" is not "core".
		{"coreutils", 0, false},
	} {
		floor, ok := floorFor(tc.pkg)
		if ok != tc.wantOK || floor != tc.wantMin {
			t.Errorf("floorFor(%q) = (%v, %v), want (%v, %v)", tc.pkg, floor, ok, tc.wantMin, tc.wantOK)
		}
	}
}

// TestSourceDirs covers the traversal that finds candidate packages, including
// the exclusions. A package missed here is a package that silently escapes the
// coverage policy entirely.
func TestSourceDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "core"), "core.go", "package core\n")
	writeFile(t, filepath.Join(root, "core", "errs"), "errs.go", "package errs\n")
	writeFile(t, filepath.Join(root, "gen", "kno", "v1"), "x.pb.go", "package v1\n")
	writeFile(t, filepath.Join(root, "tools"), "tools.go", "package tools\n")
	writeFile(t, filepath.Join(root, "testdata"), "fixture.go", "package fixture\n")
	writeFile(t, filepath.Join(root, "onlytests"), "x_test.go", "package onlytests\n")

	got, err := sourceDirs(root)
	if err != nil {
		t.Fatalf("sourceDirs: %v", err)
	}

	found := map[string]bool{}
	for _, d := range got {
		found[filepath.Base(d)] = true
	}

	for _, want := range []string{"core", "errs"} {
		if !found[want] {
			t.Errorf("sourceDirs missed %q; it would escape the coverage policy", want)
		}
	}
	for _, skip := range []string{"v1", "tools", "testdata"} {
		if found[skip] {
			t.Errorf("sourceDirs included %q, which must be excluded", skip)
		}
	}
	if found["onlytests"] {
		t.Error("sourceDirs included a directory holding only _test.go files")
	}
}

// TestSourceDirsSkipsDotDirectories pins the exclusion that agent worktrees
// under .claude/ made load-bearing. A checkout of this module nested inside a
// dot-directory is invisible to the go tool, so its packages can never appear
// in a coverage profile — counting them reports the whole module as uncovered.
func TestSourceDirsSkipsDotDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "core"), "a.go", "package core\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(root, ".claude", "worktrees", "wt", "core"), "a.go",
		"package core\n\nfunc A() {}\n")

	got, err := sourceDirs(root)
	if err != nil {
		t.Fatalf("sourceDirs: %v", err)
	}
	for _, d := range got {
		if strings.Contains(d, ".claude") {
			t.Errorf("sourceDirs returned %q; a dot-directory is not source, and the "+
				"go tool cannot build or test it", d)
		}
	}
	if len(got) != 1 {
		t.Errorf("sourceDirs = %v, want only the real core package", got)
	}
}

// TestSourceDirsWalksARelativeRoot guards the root exemption in the
// dot-directory skip: the tool is invoked as ".", whose own base name starts
// with a dot. Skipping it would silently return no packages at all — every
// coverage floor passing because nothing was checked.
func TestSourceDirsWalksARelativeRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "core"), "a.go", "package core\n\nfunc A() {}\n")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restoring cwd: %v", err)
		}
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got, err := sourceDirs(".")
	if err != nil {
		t.Fatalf("sourceDirs: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("sourceDirs(\".\") returned nothing; the walk skipped its own root, " +
			"so every coverage floor would pass vacuously")
	}
}

// TestAnalyze is the join between the coverage profile and the source scan.
// That join is the whole point of the tool: the profile alone cannot tell
// "nothing to cover" from "real code, no tests".
func TestAnalyze(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// A package with real code and coverage data.
	writeFile(t, filepath.Join(root, "withtests"), "x.go", "package withtests\n\nfunc Do() error { return nil }\n")
	// A package with real code and NO coverage data — the dangerous case.
	writeFile(t, filepath.Join(root, "notests"), "y.go", "package notests\n\nfunc Do() error { return nil }\n")
	// A charter-only package: nothing to cover.
	writeFile(t, filepath.Join(root, "stub"), "doc.go", "// Package stub is a charter.\npackage stub\n")

	writeFile(t, root, "coverage.out", "mode: atomic\n"+module+"/withtests/x.go:3.1,3.30 1 1\n")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restoring cwd: %v", err)
		}
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	pkgs, err := analyze("coverage.out", module)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	byPath := map[string]pkgCoverage{}
	for _, p := range pkgs {
		byPath[p.path] = p
	}

	if p := byPath["withtests"]; !p.inProfile || !p.hasCode || p.exempt {
		t.Errorf("withtests = %+v, want in-profile code-bearing and not exempt", p)
	}
	if p := byPath["notests"]; p.inProfile || !p.hasCode || p.exempt {
		t.Errorf("notests = %+v, want code-bearing, absent from profile, NOT exempt — "+
			"this is the case that must fail rather than pass silently", p)
	}
	if p := byPath["stub"]; !p.exempt {
		t.Errorf("stub = %+v, want exempt: a doc.go charter has nothing to cover", p)
	}
}

// TestParseProfileRejectsMissingFile confirms a missing profile is an error
// rather than an empty-and-therefore-passing result.
func TestParseProfileRejectsMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := parseProfile(filepath.Join(t.TempDir(), "absent.out"), module); err == nil {
		t.Fatal("want an error for a missing profile; a missing profile must never read as full coverage")
	}
}

// TestRatchetToleratesBaselineRounding guards a defect that made the ratchet
// fail packages against their own unchanged coverage.
//
// The baseline stores one decimal place. Comparing an unrounded 73.68 against
// a stored 73.7 reads as a decrease, so a package whose coverage had not moved
// at all would fail on every run — and the obvious "fix" for a gate that fails
// spuriously is to delete it.
func TestRatchetToleratesBaselineRounding(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "baseline")
	// 73.684...%, which writeBaseline rounds to 73.7.
	pkgs := []pkgCoverage{
		{path: "internal/thing", covered: 14, total: 19, inProfile: true, hasCode: true},
	}
	if err := writeBaseline(path, pkgs); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	if failed := check(path, pkgs); failed {
		t.Error("identical coverage failed its own baseline; the ratchet is rounding-sensitive")
	}

	// A real decrease must still fail.
	worse := []pkgCoverage{
		{path: "internal/thing", covered: 10, total: 19, inProfile: true, hasCode: true},
	}
	if failed := check(path, worse); !failed {
		t.Error("a real coverage decrease passed the ratchet")
	}
}

// TestRatchetToleratesSchedulingJitterButNotRot.
//
// Coverage for packages with concurrent code varies between runs: which paths
// a scheduler takes differs under -race and -shuffle, so the same commit can
// measure 84.1% and then 83.4%. Without a tolerance this gate fails randomly,
// and a randomly-failing gate gets deleted rather than fixed.
//
// The tolerance must be small enough that real rot still fails.
func TestRatchetToleratesSchedulingJitterButNotRot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		baseline   string
		covered    int64
		total      int64
		wantFailed bool
	}{
		{
			name:     "jitter of half a point is tolerated",
			baseline: "internal/thing 84.1\n",
			covered:  836, total: 1000, // 83.6%
		},
		{
			name:     "jitter at the edge of the tolerance is tolerated",
			baseline: "internal/thing 84.1\n",
			covered:  831, total: 1000, // 83.1%
		},
		{
			name:     "a real regression still fails",
			baseline: "internal/thing 84.1\n",
			covered:  700, total: 1000, // 70.0%, well past jitter
			wantFailed: true,
		},
		{
			name:     "an improvement passes",
			baseline: "internal/thing 84.1\n",
			covered:  950, total: 1000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "baseline")
			if err := os.WriteFile(path, []byte(tc.baseline), 0o600); err != nil {
				t.Fatalf("writing baseline: %v", err)
			}
			pkgs := []pkgCoverage{{
				path: "internal/thing", covered: tc.covered, total: tc.total,
				inProfile: true, hasCode: true,
			}}
			if got := check(path, pkgs); got != tc.wantFailed {
				t.Errorf("check failed = %v, want %v (%.1f%% against an 84.1%% baseline)",
					got, tc.wantFailed, float64(tc.covered)/float64(tc.total)*100)
			}
		})
	}
}
