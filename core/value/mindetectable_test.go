package value_test

import (
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
)

// TestTheHarmBoundIsTheSharedArithmetic pins minDetectableHarm to
// stats/interval.MinDetectableEffect under SIDEDNESS_UPPER, to full float64
// equality, at every control size a run can plausibly reach.
//
// The point is not that the numbers are right — TestTheHarmGateClearsWhereThePowerActuallyArrives
// and the interval package's published-constant test cover that. The point is
// that there is ONE implementation. `kno eval inspect` prints the two-sided
// figure from the same function; two copies of this arithmetic would let the
// command that reports power and the gate that enforces it drift apart
// silently, which is the failure mode a diagnostic cannot have.
func TestTheHarmBoundIsTheSharedArithmetic(t *testing.T) {
	t.Parallel()

	for m := range 400 {
		want := interval.MinDetectableEffect(m, knov1.Sidedness_SIDEDNESS_UPPER, 0.95)
		if got := value.MinDetectableHarmFor(m); got != want {
			t.Fatalf("m=%d: the harm bound is %v, the shared arithmetic says %v", m, got, want)
		}
	}
}

// TestNormalizeTagIsRoutingsRule pins the exported normalizer's behavior. It is
// public API now, and `kno eval inspect` reports behavior counts through it: a
// change here changes what a user is told their eval set contains.
func TestNormalizeTagIsRoutingsRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"refunds", "refunds"},
		{"Refunds", "refunds"},
		{"  refunds  ", "refunds"},
		{"\tREFUNDS\n", "refunds"},
		{"", ""},
		{"   ", ""},
		{"tool use", "tool use"},
	} {
		if got := value.NormalizeTag(tc.in); got != tc.want {
			t.Errorf("NormalizeTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
