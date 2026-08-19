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

	// Packages core is permitted to reach. gen is the generated contract
	// (ADR-0001 makes those messages the domain types), and core/... is
	// core itself.
	allowed := []string{
		module + "/gen/",
		module + "/core",
	}

	// Deps, not Imports: a transitive upward import is just as much a
	// violation as a direct one, and only the transitive set catches a
	// laundering package in between.
	//
	// -deps includes the test binary's dependencies too, so a test file
	// reaching upward for a fixture is caught as well.
	out, err := exec.Command("go", "list", "-deps", "-test", "./...").Output()
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

		var ok bool
		for _, prefix := range allowed {
			if dep == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(dep, prefix) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("core depends on %s.\n"+
				"core imports nothing above it (CLAUDE.md prime directive 3): cli, tui, and api "+
				"are shells over core, never the other way round.", dep)
		}
	}
}
