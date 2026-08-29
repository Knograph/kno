package vertex

import (
	"testing"
)

func i64(v int64) *int64 { return &v }

// TestBilledInput asserts the sum, not the input field: the cached prefix is
// billed too.
func TestBilledInput(t *testing.T) {
	t.Parallel()

	u := &usage{InputTokens: i64(1000), CacheCreationInputTokens: i64(100), CacheReadInputTokens: i64(500)}
	if got := u.billedInput(); got != 1600 {
		t.Errorf("billedInput = %d, want 1600", got)
	}
	if got := (&usage{}).billedInput(); got != 0 {
		t.Errorf("empty billedInput = %d, want 0", got)
	}
}

// TestUsable asserts the block must describe the response it arrived with:
// required fields present, every dimension bounded, and a full answer not
// paired with zero output tokens.
func TestUsable(t *testing.T) {
	t.Parallel()

	ok := &usage{InputTokens: i64(1000), OutputTokens: i64(200)}
	if !ok.usable("hello") {
		t.Error("a complete block is not usable")
	}

	tests := []struct {
		name string
		u    *usage
	}{
		{"no input", &usage{OutputTokens: i64(200)}},
		{"no output", &usage{InputTokens: i64(1000)}},
		{"negative input", &usage{InputTokens: i64(-1), OutputTokens: i64(200)}},
		{"negative cache write", &usage{InputTokens: i64(1000), OutputTokens: i64(200), CacheCreationInputTokens: i64(-5)}},
		{"implausible input", &usage{InputTokens: i64(maxPlausibleTokens + 1), OutputTokens: i64(200)}},
		{"implausible cache read", &usage{InputTokens: i64(1000), OutputTokens: i64(200), CacheReadInputTokens: i64(maxPlausibleTokens + 1)}},
		{"zero billed input", &usage{InputTokens: i64(0), OutputTokens: i64(0)}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.u.usable("hello") {
				t.Errorf("%s is usable", tt.name)
			}
		})
	}

	// A refusal with zero output tokens is a measurement, not an absence:
	// the provider genuinely billed the prompt and produced no answer.
	refused := &usage{InputTokens: i64(1000), OutputTokens: i64(0)}
	if !refused.usable("") {
		t.Error("refusal block is not usable")
	}
	// A full answer must not pair with zero output tokens.
	if (&usage{InputTokens: i64(1000), OutputTokens: i64(0)}).usable("hello") {
		t.Error("zero-output block on a full answer is usable")
	}
}
