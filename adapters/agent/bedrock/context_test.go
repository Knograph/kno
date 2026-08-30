package bedrock

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

func anAsset(id, content string) *core.Asset {
	return &core.Asset{Id: id, Content: []byte(content)}
}

// TestWithContextReturnsACopy pins the treatment/control contract: the
// receiver is unmodified, so it stays the control arm of the same
// measurement.
func TestWithContextReturnsACopy(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, System: "S"}, nil)

	injected, err := a.WithContext(anAsset("a1", "the asset body"))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}

	// The receiver is untouched.
	if got := a.systemPrefix(); got != "S" {
		t.Errorf("receiver systemPrefix = %q, want the un-injected \"S\"", got)
	}

	// The injected Agent carries the Asset, joined after the system prompt.
	if got := injected.(*Agent).systemPrefix(); got != "S\n\nthe asset body" {
		t.Errorf("injected systemPrefix = %q", got)
	}
}

// TestWithContextRecomputesWorstCase pins that the Asset enters the planning
// figure: worst is memoized at construction, and carrying the receiver's
// number over would under-plan every Case in the treatment arm.
func TestWithContextRecomputesWorstCase(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024}, nil)
	before := a.WorstCase()

	injected, err := a.WithContext(anAsset("a1", strings.Repeat("x", 100_000)))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	after := injected.(core.Estimator).WorstCase()
	if after.CostUSDMicros <= before.CostUSDMicros {
		t.Errorf("WorstCase after injection = %d, want > receiver's %d", after.CostUSDMicros, before.CostUSDMicros)
	}

	// The receiver's memoized figure is unchanged — the copy recomputed its
	// own.
	if got := a.WorstCase().CostUSDMicros; got != before.CostUSDMicros {
		t.Errorf("receiver WorstCase changed: %d -> %d", before.CostUSDMicros, got)
	}
}

// TestWithContextRefusals pins every free refusal before a Case is sent.
func TestWithContextRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		asset  *core.Asset
		opts   Options
		second bool // inject into the Agent the first injection produced
		wants  string
	}{
		{"nil asset", nil, Options{}, false, "there is no Asset to inject"},
		{"empty asset", anAsset("a1", ""), Options{}, false, "is empty"},
		{"non-UTF8 asset", anAsset("a1", string([]byte{0xff, 0xfe, 'x'})), Options{}, false, "not valid UTF-8"},
		{"past the ceiling", anAsset("a1", strings.Repeat("a", 2048)), Options{MaxPromptBytes: 1024}, false, "--max-prompt-bytes"},
		{"second Asset", anAsset("a1", "x"), Options{}, true, "already carries an injected Asset"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, MaxPromptBytes: tc.opts.MaxPromptBytes}, nil)

			target := a
			if tc.second {
				first, err := a.WithContext(anAsset("a0", "first"))
				if err != nil {
					t.Fatalf("first injection: %v", err)
				}
				target = first.(*Agent)
			}

			got, err := target.WithContext(tc.asset)
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != nil {
				t.Errorf("a refused injection still returned an Agent (%T)", got)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not contain %q", err, tc.wants)
			}
		})
	}
}

// TestWithContextSharesTheTransport pins the deliberate sharing: the two arms
// share the connection pool, the rate limiter, and the skew-retry budget —
// one run, one clock.
func TestWithContextSharesTheTransport(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024}, nil)
	injected, err := a.WithContext(anAsset("a1", "x"))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	if injected.(*Agent).opts.HTTPClient != a.opts.HTTPClient {
		t.Error("the copy does not share the receiver's transport")
	}
}

// TestSystemPrefixOrder pins the byte order: configured system prompt, then
// the Asset — the stable prefix every Case in a sample shares.
func TestSystemPrefixOrder(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, System: "S"}, nil)
	if got := a.systemPrefix(); got != "S" {
		t.Errorf("systemPrefix = %q", got)
	}

	a2 := *a
	a2.asset = "A"
	if got := a2.systemPrefix(); got != "S\n\nA" {
		t.Errorf("systemPrefix = %q, want \"S\\n\\nA\"", got)
	}

	// No system prompt: the Asset alone.
	a3 := *a
	a3.opts.System = ""
	a3.asset = "A"
	if got := a3.systemPrefix(); got != "A" {
		t.Errorf("systemPrefix = %q, want \"A\"", got)
	}
}
