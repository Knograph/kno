package bedrock

import (
	"errors"
	"net/http"
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

// These tests cover WithContextSet: the Portfolio-injection half of the same
// contract WithContext already proves for one Asset. See core/ring0.go's
// ContextSetInjector for why order and a whole-set ceiling are the two
// properties that matter here and nowhere else.

// TestWithContextSetJoinsEveryAssetInRankOrder pins that assets are joined
// in the given order, with "\n\n" between them, into the same asset field
// WithContext uses.
//
// ORDER IS PART OF THE MEASUREMENT: Validate applies PortfolioEntry.rank
// before calling this, and a caller-supplied order that the adapter silently
// resorted would measure a Portfolio other than the one Validate asked about.
func TestWithContextSetJoinsEveryAssetInRankOrder(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, System: "S"}, nil)

	first := anAsset("a1", "FIRST-ASSET-CONTENT")
	second := anAsset("a2", "SECOND-ASSET-CONTENT")

	injected, err := a.WithContextSet([]*core.Asset{first, second})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}

	if got, want := injected.(*Agent).systemPrefix(), "S\n\nFIRST-ASSET-CONTENT\n\nSECOND-ASSET-CONTENT"; got != want {
		t.Errorf("systemPrefix = %q, want %q", got, want)
	}
}

// TestWithContextSetReturnsACopyAndRecomputesWorstCase mirrors
// TestWithContextReturnsACopy and TestWithContextRecomputesWorstCase for the
// whole-set path: the receiver stays the control arm, and the treatment
// arm's planning figure accounts for the whole Portfolio, not a stale
// memoized number.
func TestWithContextSetReturnsACopyAndRecomputesWorstCase(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, System: "S"}, nil)
	beforeWorst := a.WorstCase()

	injected, err := a.WithContextSet([]*core.Asset{
		anAsset("a1", strings.Repeat("x", 100_000)),
	})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}

	// The receiver is untouched.
	if got := a.systemPrefix(); got != "S" {
		t.Errorf("receiver systemPrefix = %q, want the un-injected \"S\"", got)
	}
	if got := a.WorstCase().CostUSDMicros; got != beforeWorst.CostUSDMicros {
		t.Errorf("receiver WorstCase changed: %d -> %d", beforeWorst.CostUSDMicros, got)
	}

	// The treatment arm carries the set and planned for it.
	inj := injected.(*Agent)
	if want := "S\n\n" + strings.Repeat("x", 100_000); inj.systemPrefix() != want {
		t.Errorf("injected systemPrefix does not carry the joined set (len %d, want %d)",
			len(inj.systemPrefix()), len(want))
	}
	if got := inj.WorstCase().CostUSDMicros; got <= beforeWorst.CostUSDMicros {
		t.Errorf("WorstCase after set injection = %d, want > receiver's %d", got, beforeWorst.CostUSDMicros)
	}
}

// TestWithContextSetRefusesAnEmptyOrNilSet pins that an Agent carrying no
// Assets IS the control arm: answering here would measure the control
// against itself and report the difference as zero, with an interval.
func TestWithContextSetRefusesAnEmptyOrNilSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		assets []*core.Asset
	}{
		{"nil set", nil},
		{"empty set", []*core.Asset{}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024}, nil)

			got, err := a.WithContextSet(tc.assets)
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != nil {
				t.Errorf("a refused injection still returned an Agent (%T)", got)
			}
			if !strings.Contains(err.Error(), "control arm") {
				t.Errorf("the refusal does not explain why an empty set cannot be measured: %v", err)
			}
		})
	}
}

// TestWithContextSetRefusesASetPastTheCeiling pins that the WHOLE joined set
// is bound against --max-prompt-bytes ONCE, before any Case is sent — the
// same free-refusal property injectable gives a single Asset, applied to the
// set as a whole because it rides as one system-array payload.
func TestWithContextSetRefusesASetPastTheCeiling(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024, MaxPromptBytes: 1024}, nil)

	got, err := a.WithContextSet([]*core.Asset{
		anAsset("a1", strings.Repeat("a", 700)),
		anAsset("a2", strings.Repeat("b", 700)),
	})
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if got != nil {
		t.Errorf("a refused set still returned an Agent (%T)", got)
	}
	if !strings.Contains(err.Error(), "--max-prompt-bytes") {
		t.Errorf("error %q does not name the flag", err)
	}
}

// TestWithContextSetRefusesAnInvalidAssetInTheSet pins that every Asset is
// checked with the same rule WithContext applies to one, naming the
// offending Asset so the refusal is actionable rather than a run that fails
// on every Case in the sample.
func TestWithContextSetRefusesAnInvalidAssetInTheSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		asset *core.Asset
		wants string
	}{
		{"nil element", nil, "there is no Asset at index"},
		{"empty content", anAsset("asset-empty", ""), "asset-empty"},
		{"non-UTF8 content", anAsset("asset-binary", string([]byte{0xff, 0xfe, 'x'})), "asset-binary"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024}, nil)

			got, err := a.WithContextSet([]*core.Asset{anAsset("good", "good content"), tc.asset})
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

// TestWithContextSetRefusesASecondInjection pins that one Agent carries one
// injected payload, whether it arrived through WithContext or WithContextSet.
func TestWithContextSetRefusesASecondInjection(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024}, nil)
	first, err := a.WithContextSet([]*core.Asset{anAsset("a0", "first")})
	if err != nil {
		t.Fatalf("first injection: %v", err)
	}

	got, err := first.(*Agent).WithContextSet([]*core.Asset{anAsset("a1", "second")})
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if got != nil {
		t.Errorf("a refused injection still returned an Agent (%T)", got)
	}
	if !strings.Contains(err.Error(), "already carries an injected Asset") {
		t.Errorf("error %q does not say the Agent already carries an injected Asset", err)
	}
}

// TestWithContextSetInjectedAgentDeclaresContextSetInject asserts the
// treatment arm reports the capability the Value stage checks before
// routing a Portfolio, and that it survives an actual Invoke.
func TestWithContextSetInjectedAgentDeclaresContextSetInject(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{Model: testModel, MaxOutputTokens: 1024},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(converseOK))
		})

	injected, err := a.WithContextSet([]*core.Asset{
		anAsset("a1", "FIRST"), anAsset("a2", "SECOND"),
	})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}

	capable, ok := injected.(core.Capable)
	if !ok {
		t.Fatal("the injected Agent is not Capable")
	}
	if !capable.Capabilities().GetContextSetInject() {
		t.Error("the injected Agent does not declare context_set_inject")
	}

	if _, err := injected.(core.Agent).Invoke(t.Context(), aCase("c1", "hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got := rec.body(t, 0)
	if !strings.Contains(string(got), "FIRST") || !strings.Contains(string(got), "SECOND") {
		t.Errorf("the request does not carry both Assets: %s", got)
	}
}
