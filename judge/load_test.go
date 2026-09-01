package judge_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/judge"
)

// TestTheCommittedSetsSatisfyTheirOwnInvariants.
//
// The embedded sets go through the same loader a contributed one does. A
// binary carrying a set that would be refused from a directory is a gate that
// passes because of where its input lives.
func TestTheCommittedSetsSatisfyTheirOwnInvariants(t *testing.T) {
	t.Parallel()

	for _, name := range judge.BuiltinSets() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			set, err := judge.Builtin(name)
			if err != nil {
				t.Fatalf("the committed set does not load: %v", err)
			}
			if share := set.MinorityShare(); share < judge.MinMinorityShare {
				t.Errorf("minority class %.1f%% is below the %.0f%% invariant",
					share*100, judge.MinMinorityShare*100)
			}
			for _, r := range set.Records {
				if r.Provenance.Source != judge.SourceAuthored &&
					r.Provenance.Source != judge.SourceSynthetic {
					t.Errorf("record %s declares provenance %q", r.ID, r.Provenance.Source)
				}
			}
		})
	}
}

// TestTheSetContainsNoDerivedContent is the security invariant, asserted
// rather than trusted.
//
// The set is public and permanent, and CLAUDE.md's security section says
// traces are customer data. There is no spelling for a harvested record —
// but a test that only checks the loader's enum would pass on a set whose
// records were harvested and then relabeled "authored". This checks the
// manifest's own statement too, so the claim and the data are pinned together.
func TestTheSetContainsNoDerivedContent(t *testing.T) {
	t.Parallel()

	for _, name := range judge.BuiltinSets() {
		set, err := judge.Builtin(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ToLower(set.Description), "synthetic") &&
			!strings.Contains(strings.ToLower(set.Description), "authored") {
			t.Errorf("set %q does not say in its manifest where its records came from", name)
		}
		for _, r := range set.Records {
			if r.Provenance.Source == "derived" {
				t.Errorf("record %s is derived from a real deployment", r.ID)
			}
		}
	}
}

// TestBadSetsAreRefusedAtLoad drives every invariant against a set that breaks
// exactly one of them.
//
// Each fixture is a deliberately broken set in testdata/bad/. The invariants
// are refused at LOAD rather than reported afterwards, because a statistic
// computed over a set that breaks one would be read as an answer.
func TestBadSetsAreRefusedAtLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dir  string
		want string
		why  string
	}{
		{"imbalanced", "minority class", "kappa is depressed by extreme prevalence"},
		{"single-label", "at least 2", "one label is one person's judgement"},
		{"unadjudicated", "adjudicated", "the set may not hold an unresolved record"},
		{"stale-hash", "hashes to", "deleting the records a judge fails is the route around this gate"},
		{"derived-provenance", "provenance", "a harvested record has no spelling here"},
		{"too-small", "at least 2 records", "no interval is computable over one unit"},
		{"duplicate-id", "more than once", "a duplicate is counted twice by every statistic"},
	}
	for _, tc := range tests {
		t.Run(tc.dir, func(t *testing.T) {
			t.Parallel()

			_, err := judge.Load(filepath.Join("testdata", "bad", tc.dir))
			if err == nil {
				t.Fatalf("a broken set loaded: %s", tc.why)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("the refusal is not an Actionable: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name %q:\n%v", tc.want, err)
			}
		})
	}
}

// TestAMissingSetIsNamedNotSilentlyEmpty: never a zero-record kappa.
func TestAMissingSetIsNamedNotSilentlyEmpty(t *testing.T) {
	t.Parallel()

	_, err := judge.Load(filepath.Join("testdata", "calibration", "does-not-exist"))
	if err == nil {
		t.Fatal("a missing set loaded")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error does not name the path:\n%v", err)
	}
}

// TestContentHashCoversTheFileNotItsMeaning. A gate that only noticed semantic
// edits could be routed around by a formatter.
func TestContentHashCoversTheFileNotItsMeaning(t *testing.T) {
	t.Parallel()

	set, err := judge.Builtin("starter")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.ContentSHA256) != 64 {
		t.Errorf("content hash %q is not a sha256", set.ContentSHA256)
	}
}
