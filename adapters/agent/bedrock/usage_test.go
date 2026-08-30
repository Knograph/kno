package bedrock

import (
	"testing"
)

func i64(n int64) *int64 { return &n }

// TestBilledInputSumsTheThreeInputFields pins the billing rule: billed input
// is InputTokens PLUS the cache fields, never InputTokens alone.
func TestBilledInputSumsTheThreeInputFields(t *testing.T) {
	t.Parallel()

	u := &usage{
		InputTokens:              i64(1000),
		CacheCreationInputTokens: i64(100),
		CacheReadInputTokens:     i64(500),
	}
	if got := u.billedInput(); got != 1600 {
		t.Errorf("billedInput = %d, want 1600", got)
	}

	// Absent fields are arithmetic zero.
	u2 := &usage{InputTokens: i64(7)}
	if got := u2.billedInput(); got != 7 {
		t.Errorf("billedInput = %d, want 7", got)
	}
	if got := (&usage{}).billedInput(); got != 0 {
		t.Errorf("billedInput(empty) = %d, want 0", got)
	}
}

// TestUsageUsableMatrix pins when a usage block may settle a Case. The shape
// of the rule: absent input or output is not a measurement, negative or
// absurd dimensions are not a measurement, zero billed input is not a
// measurement, and an empty output may settle zero output only when the
// answer really is empty.
func TestUsageUsableMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		u      *usage
		output string
		want   bool
	}{
		{"complete", &usage{InputTokens: i64(10), OutputTokens: i64(5)}, "hi", true},
		{"input absent", &usage{OutputTokens: i64(5)}, "hi", false},
		{"output absent", &usage{InputTokens: i64(10)}, "hi", false},
		{"both absent", &usage{}, "hi", false},
		{"negative input", &usage{InputTokens: i64(-1), OutputTokens: i64(5)}, "hi", false},
		{"negative output", &usage{InputTokens: i64(10), OutputTokens: i64(-5)}, "hi", false},
		{"beyond plausible", &usage{InputTokens: i64(10_000_001), OutputTokens: i64(5)}, "hi", false},
		{"zero billed input", &usage{InputTokens: i64(0), OutputTokens: i64(5)}, "hi", false},
		{"zero output with answer", &usage{InputTokens: i64(10), OutputTokens: i64(0)}, "hi", false},
		{"zero output with empty answer", &usage{InputTokens: i64(10), OutputTokens: i64(0)}, "", true},
		{"cached-only request", &usage{InputTokens: i64(1), CacheReadInputTokens: i64(1000), OutputTokens: i64(2)}, "x", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.u.usable(tc.output); got != tc.want {
				t.Errorf("usable(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestMaxPlausibleTokensPinned keeps the sentinel visible: it is the shared
// bound both adapters' sanity arithmetic leans on.
func TestMaxPlausibleTokensPinned(t *testing.T) {
	t.Parallel()
	if MaxPlausibleTokens != 10_000_000 {
		t.Errorf("MaxPlausibleTokens = %d", MaxPlausibleTokens)
	}
}
