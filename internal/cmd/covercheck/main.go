// Command covercheck enforces the coverage floors and the no-decrease ratchet
// that CLAUDE.md requires.
//
// It exists because `go test -cover` reports coverage but cannot fail a build
// on policy. Two policies are enforced:
//
//   - Floors: 85% on core, stats, bridge, and plugin; 70% repo-wide.
//   - Ratchet: no package's coverage may decrease against .coverage-baseline.
//
// The exemption rule is deliberately narrow. A package is exempt only if it
// contains no non-test Go file with a function body — interfaces, type
// aliases, and doc.go stubs have nothing to cover. Keying exemption off
// "absent from the coverage profile" would instead exempt the dangerous case:
// a package full of real code and zero tests, forever and silently.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Floors are the per-package coverage minimums from CLAUDE.md. The prefix is
// matched against the package path relative to the module root.
var floors = []struct {
	prefix string
	min    float64
}{
	{"core", 85},
	{"stats", 85},
	{"bridge", 85},
	{"plugin", 85},
}

const repoWideFloor = 70

// ratchetTolerance is how far a package may drift below its baseline before
// the no-decrease rule fires, in percentage points.
//
// It exists because concurrent code's measured coverage is not deterministic,
// not because small regressions are acceptable. See the comment at its use.
const ratchetTolerance = 1.0

// skipPrefixes are never measured. Generated code is not ours to test, and the
// tools module is build tooling rather than shipping code.
//
// godoccheck applies the same exclusion; keeping them symmetric matters,
// because generated .pb.go files are full of function bodies and would
// otherwise be judged as code-bearing and untested.
var skipPrefixes = []string{"gen/", "tools/"}

type pkgCoverage struct {
	path      string
	covered   int64
	total     int64
	inProfile bool
	hasCode   bool
	exempt    bool
	exemptWhy string
}

func (p pkgCoverage) percent() float64 {
	if p.total == 0 {
		return 0
	}
	return float64(p.covered) / float64(p.total) * 100
}

func main() {
	var (
		profile  = flag.String("profile", "coverage.out", "coverage profile to read")
		baseline = flag.String("baseline", ".coverage-baseline", "committed baseline to ratchet against")
		write    = flag.Bool("write", false, "write the baseline instead of checking it")
		module   = flag.String("module", "github.com/knograph/kno", "module path to strip from package paths")
	)
	flag.Parse()

	pkgs, err := analyze(*profile, *module)
	if err != nil {
		fmt.Fprintf(os.Stderr, "covercheck: %v\n", err)
		os.Exit(1)
	}

	if *write {
		if err := writeBaseline(*baseline, pkgs); err != nil {
			fmt.Fprintf(os.Stderr, "covercheck: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("covercheck: wrote %s\n", *baseline)
		return
	}

	if failed := check(*baseline, pkgs); failed {
		os.Exit(1)
	}
}

// analyze merges the coverage profile with a source scan. The source scan is
// what makes "this package has code but no tests" distinguishable from "this
// package has nothing to cover" — the profile alone cannot tell them apart.
func analyze(profilePath, module string) ([]pkgCoverage, error) {
	fromProfile, err := parseProfile(profilePath, module)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*pkgCoverage{}
	for path, c := range fromProfile {
		byPath[path] = &pkgCoverage{
			path:      path,
			covered:   c.covered,
			total:     c.total,
			inProfile: true,
		}
	}

	dirs, err := sourceDirs(".")
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		p, ok := byPath[dir]
		if !ok {
			p = &pkgCoverage{path: dir}
			byPath[dir] = p
		}
		hasCode, err := hasFunctionBody(dir)
		if err != nil {
			return nil, err
		}
		p.hasCode = hasCode
		if !hasCode {
			p.exempt = true
			p.exemptWhy = "no non-test function bodies"
		}
	}

	out := make([]pkgCoverage, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

type counts struct{ covered, total int64 }

// parseProfile reads a Go coverage profile. Lines look like:
//
//	github.com/knograph/kno/core/errs/errs.go:12.34,15.2 3 1
//
// where the trailing numbers are statement count and execution count.
func parseProfile(path, module string) (map[string]counts, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied flag, not user input
	if err != nil {
		return nil, fmt.Errorf("opening profile: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable

	out := map[string]counts{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		file := line[:colon]
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			continue
		}
		stmts, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		execCount, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}

		pkg := filepath.Dir(strings.TrimPrefix(strings.TrimPrefix(file, module), "/"))
		if pkg == "." {
			pkg = ""
		}
		if skipped(pkg) {
			continue
		}
		c := out[pkg]
		c.total += stmts
		if execCount > 0 {
			c.covered += stmts
		}
		out[pkg] = c
	}
	return out, sc.Err()
}

// sourceDirs lists every directory holding non-test Go files.
func sourceDirs(root string) ([]string, error) {
	seen := map[string]bool{}
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
		// Relative to the walk ROOT, not the working directory. Computed
		// against CWD, the skip list silently did nothing whenever the tool
		// was invoked with an absolute root — so gen/ and tools/ would have
		// been held to the coverage floor.
		dir, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			dir = filepath.Dir(path)
		}
		dir = strings.TrimPrefix(filepath.ToSlash(dir), "./")
		if dir == "." {
			dir = ""
		}
		if !skipped(dir) {
			seen[dir] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// hasFunctionBody reports whether a directory holds any non-test Go file with a
// function body, honoring build constraints for the CURRENT platform.
//
// The build-constraint filtering is load-bearing: go test only instruments
// files it compiles, so an AST walk that counted a //go:build windows file on
// Linux would demand coverage for statements that were never compiled here.
func hasFunctionBody(dir string) (bool, error) {
	ctx := build.Default
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// MatchFile applies build tags and GOOS/GOARCH filename constraints,
		// so this mirrors what the compiler actually saw.
		ok, err := ctx.MatchFile(dir, name)
		if err != nil || !ok {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return false, fmt.Errorf("parsing %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Body != nil && len(fn.Body.List) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func skipped(pkg string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(pkg+"/", p) {
			return true
		}
	}
	return false
}

func floorFor(pkg string) (float64, bool) {
	for _, f := range floors {
		if pkg == f.prefix || strings.HasPrefix(pkg, f.prefix+"/") {
			return f.min, true
		}
	}
	return 0, false
}

// check enforces floors and the ratchet, printing every judgement so a reader
// can see what ran rather than trusting a bare exit code.
func check(baselinePath string, pkgs []pkgCoverage) (failed bool) {
	prev, err := readBaseline(baselinePath)
	if err != nil {
		fmt.Printf("covercheck: no baseline at %s — floors enforced, ratchet bootstrapping\n", baselinePath)
	}

	var totalCovered, totalStmts int64
	for _, p := range pkgs {
		totalCovered += p.covered
		totalStmts += p.total

		switch {
		case p.exempt:
			fmt.Printf("  exempt  %-40s (%s)\n", p.path, p.exemptWhy)
			continue

		case p.hasCode && !p.inProfile:
			// The dangerous case the exemption rule exists to catch: real code
			// that produced no coverage data at all. Either it has no tests, or
			// its package failed to build — both must fail, never pass quietly.
			fmt.Printf("  FAIL    %-40s has function bodies but no coverage data\n", p.path)
			fmt.Printf("          (no tests, or the package failed to build)\n")
			failed = true
			continue
		}

		pct := p.percent()
		if floor, ok := floorFor(p.path); ok && pct < floor {
			fmt.Printf("  FAIL    %-40s %.1f%% < %.1f%% floor\n", p.path, pct, floor)
			failed = true
			continue
		}
		// Compare at the baseline's own precision, with a tolerance.
		//
		// Two separate problems live here. The file stores one decimal place,
		// so an unrounded 73.68 would read as a decrease against a stored 73.7
		// — failing a package against its own unchanged coverage.
		//
		// And coverage genuinely varies between runs for packages with
		// concurrent code: which paths a scheduler happens to take differs
		// under -race and -shuffle, so the same commit measured twice can
		// report 84.1% and then 83.4%. Without a tolerance this gate fails
		// randomly, and a gate that fails randomly gets deleted rather than
		// fixed — which would cost more than the drift it was catching. The
		// tolerance is small enough that real rot, which arrives in whole
		// points, still fails.
		if was, ok := prev[p.path]; ok && round1(pct) < was-ratchetTolerance {
			fmt.Printf("  FAIL    %-40s %.1f%% < %.1f%% baseline (coverage may not decrease)\n",
				p.path, pct, was)
			failed = true
			continue
		}
		fmt.Printf("  ok      %-40s %.1f%%\n", p.path, pct)
	}

	repoPct := 0.0
	if totalStmts > 0 {
		repoPct = float64(totalCovered) / float64(totalStmts) * 100
	}
	if repoPct < repoWideFloor {
		fmt.Printf("  FAIL    repo-wide %.1f%% < %.1f%% floor\n", repoPct, float64(repoWideFloor))
		failed = true
	} else {
		fmt.Printf("  ok      repo-wide %.1f%%\n", repoPct)
	}

	if failed {
		fmt.Println("covercheck: FAILED")
	} else {
		fmt.Println("covercheck: ok")
	}
	return failed
}

// round1 rounds to one decimal place, matching the baseline file's precision.
func round1(f float64) float64 { return math.Round(f*10) / 10 }

func readBaseline(path string) (map[string]float64, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied flag, not user input
	if err != nil {
		return map[string]float64{}, err
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable

	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pct, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		out[fields[0]] = pct
	}
	return out, sc.Err()
}

// writeBaseline emits one sorted line per package, so concurrent PRs conflict
// per-package and resolve mechanically rather than fighting over one blob.
func writeBaseline(path string, pkgs []pkgCoverage) error {
	var b strings.Builder
	b.WriteString("# Coverage baseline. Regenerate with `make update-coverage-baseline`.\n")
	b.WriteString("# Coverage may not decrease against these numbers. Review diffs like code.\n")
	for _, p := range pkgs {
		if p.exempt {
			continue
		}
		name := p.path
		if name == "" {
			name = "."
		}
		fmt.Fprintf(&b, "%s %.1f\n", name, round1(p.percent()))
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
