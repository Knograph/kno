package split

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestAssignSplitIsDeterministic: the same id and seed must land in the same
// half on every call, forever. The split is keyed on the id alone (a seed is
// the deliberate exception); anything that moves a Case across runs silently
// changes the population behind every later number.
func TestAssignSplitIsDeterministic(t *testing.T) {
	t.Parallel()
	ids := []string{"a", "case-001", "case-002", "support-llm:ex-0001"}
	for _, id := range ids {
		want := AssignSplit(id, "", DefaultHoldoutFrac)
		for range 10 {
			if got := AssignSplit(id, "", DefaultHoldoutFrac); got != want {
				t.Errorf("AssignSplit(%q) = %v, want %v (must be stable)", id, got, want)
			}
		}
	}
}

// TestAssignSplitDivergesOnlyForAnExplicitSeed: the seed is a deliberate
// re-split, recorded on the Run; an empty seed must never change behavior
// between versions of the hash inputs.
func TestAssignSplitDivergesOnlyForAnExplicitSeed(t *testing.T) {
	t.Parallel()
	want := AssignSplit("case-001", "", 0.2)
	for range 10 {
		if AssignSplit("case-001", "", 0.2) != want {
			t.Fatal("same inputs, different split")
		}
	}
	// The seed actually changes the outcome for at least one common id — a
	// seed that never moved anything would be a silent no-op that users could
	// not detect.
	moved := 0
	for i := range 200 {
		id := strings.Repeat("x", 1+i%7) + string(rune('a'+i%26))
		if AssignSplit(id, "seed-1", 0.2) != AssignSplit(id, "", 0.2) {
			moved++
		}
	}
	if moved == 0 {
		t.Error("a seed that moves nothing is a no-op the user cannot see")
	}
}

// TestAssignSplitHonorsTheFractionRoughly: across enough ids the measured
// holdout share approaches the configured fraction. The bucket granularity is
// 0.01%, so 10k ids must land within a point of the target.
func TestAssignSplitHonorsTheFractionRoughly(t *testing.T) {
	t.Parallel()
	for _, frac := range []float64{0.05, 0.2, 0.5, 0.8} {
		holdout := 0
		const n = 10_000
		for i := range n {
			id := fmt.Sprintf("ex-%06d", i)
			if AssignSplit(id, "", frac) == knov1.Split_SPLIT_HOLDOUT {
				holdout++
			}
		}
		got := float64(holdout) / n
		if got < frac-0.01 || got > frac+0.01 {
			t.Errorf("frac %.2f: measured %.4f, want within 0.01", frac, got)
		}
	}
}

func TestCountsTotalAndUnderpowered(t *testing.T) {
	t.Parallel()
	if (Counts{Dev: 10, Holdout: 5}).Total() != 15 {
		t.Error("Total is not the sum")
	}
	if !(Counts{Dev: 10, Holdout: 5}).Underpowered() {
		t.Error("a holdout below MinHoldout must be underpowered")
	}
	if (Counts{Dev: 10, Holdout: MinHoldout}).Underpowered() {
		t.Error("a holdout at MinHoldout is the first honest size")
	}
	if (Counts{Dev: 10}).Underpowered() {
		t.Error("a zero holdout is not 'underpowered' — it is invalid, and Validate says so")
	}
}

func TestCountsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    Counts
		want string // substring of the error; empty means valid
	}{
		{name: "empty set", c: Counts{}, want: "empty"},
		{name: "no holdout", c: Counts{Dev: 5, HoldoutFrac: 0.2}, want: "no holdout"},
		{name: "no holdout honors recorded frac", c: Counts{Dev: 5, HoldoutFrac: 0.05}, want: "21"},
		{name: "no holdout falls back to the default frac", c: Counts{Dev: 5}, want: "6"},
		{name: "no dev", c: Counts{Holdout: 3}, want: "nothing to measure"},
		{name: "healthy", c: Counts{Dev: 80, Holdout: 20, HoldoutFrac: 0.2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.c.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %+v", tt.c)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestFingerprintSplitChangesWithEitherInput: the fingerprint feeds the
// resume check, so a change in seed or fraction must change it — a resume
// that silently reclassified Cases would mix two populations into one run.
func TestFingerprintSplitChangesWithEitherInput(t *testing.T) {
	t.Parallel()
	base := FingerprintSplit("", 0.2)
	if !bytes.Equal(FingerprintSplit("", 0.2), base) {
		t.Error("identical configuration must fingerprint identically")
	}
	if bytes.Equal(FingerprintSplit("seed-1", 0.2), base) {
		t.Error("a new seed must move the fingerprint")
	}
	if bytes.Equal(FingerprintSplit("", 0.3), base) {
		t.Error("a new fraction must move the fingerprint")
	}
}
