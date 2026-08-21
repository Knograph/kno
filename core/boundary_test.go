package core_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCoreImportsNothingAbove enforces CLAUDE.md's third prime directive
// mechanically rather than by review discipline.
//
// core is the engine. cli, tui, and api are thin shells over identical core
// calls, and that seam is what lets the open-core boundary be a directory
// boundary rather than a fork. An upward import is rejected regardless of
// quality — but "rejected" only means something if something rejects it.
//
// Known limit: this constrains the Go import graph only. It cannot see schema
// contamination — the platform adding fields to shared kno.v1 messages — which
// is a proto-review problem tracked as docs/debt.md#7.
func TestCoreImportsNothingAbove(t *testing.T) {
	t.Parallel()

	const module = "github.com/knograph/kno"

	// Packages core must never reach.
	//
	// The rule is "core imports nothing ABOVE it", and DESIGN.md is explicit
	// about what above means: cli, tui, and api are thin shells over identical
	// core calls, never the reverse. Ring-1 adapters, bridge, judge, and the
	// plugin ring are pluggable implementations of core's contracts — core
	// defines the interfaces, so depending on an implementation would invert
	// the ring.
	//
	// store, executor, and stats are BELOW core: infrastructure it orchestrates
	// while running a stage. An earlier version of this test was an allowlist
	// of gen/ and core/ alone, which made the pipeline stages DESIGN.md places
	// in core/ impossible to write there. Widened deliberately rather than
	// silently; see the M1-5 PR.
	forbidden := []string{
		module + "/cli",
		module + "/tui",
		module + "/api",
		module + "/adapters/",
		module + "/bridge",
		module + "/judge",
		module + "/plugin",
		module + "/goal/",
	}

	// Deps, not Imports: a transitive upward import is just as much a
	// violation as a direct one, and only the transitive set catches a
	// laundering package in between.
	//
	// Test dependencies are deliberately EXCLUDED (no -test flag). The rule
	// constrains what ships: core's own tests legitimately construct a fake
	// agent and a concrete Goal to exercise a stage against, and that is not
	// the ring inverting — nothing in the shipped binary depends on them.
	// Phase-1 finding G10 raised this as undecided; it is decided here.
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		if dep == "" || !strings.HasPrefix(dep, module) {
			continue // standard library and third-party are not the concern
		}
		// The synthesized test binary shows up as "pkg [pkg.test]".
		dep = strings.Fields(dep)[0]

		for _, prefix := range forbidden {
			if dep == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(dep, prefix) {
				t.Errorf("core depends on %s.\n"+
					"core imports nothing above it (CLAUDE.md prime directive 3): cli, tui, and "+
					"api are shells over core, and adapters implement core's contracts rather "+
					"than the reverse.", dep)
			}
		}
	}
}
