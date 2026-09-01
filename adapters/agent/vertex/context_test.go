package vertex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// anAsset builds a text Asset.
func anAsset(id, content string) *core.Asset {
	return &core.Asset{Id: id, Content: []byte(content)}
}

// TestWithContext asserts the treatment arm is a COPY: the injected Agent
// sends the Asset, the receiver stays the control, and the two share one
// transport (one RoundTrips counter).
func TestWithContext(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{System: "sys"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})

	injected, err := a.WithContext(anAsset("a1", "the asset text"))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}

	if a.asset != "" {
		t.Error("the receiver carries the injected Asset — it must stay the control")
	}
	inj := injected.(*Agent)
	if inj.asset != "the asset text" {
		t.Errorf("injected asset = %q", inj.asset)
	}
	if inj == a {
		t.Error("WithContext returned the receiver itself")
	}

	// The injected Agent sends the system string plus the Asset, in that
	// order — the byte-identical cache prefix every Case shares.
	ctrl, err := a.Invoke(context.Background(), aCase("c-ctrl", "in"))
	if err != nil {
		t.Fatalf("control Invoke: %v", err)
	}
	treat, err := injected.(core.Agent).Invoke(context.Background(), aCase("c-treat", "in"))
	if err != nil {
		t.Fatalf("treatment Invoke: %v", err)
	}
	if ctrl.CaseId != "c-ctrl" || treat.CaseId != "c-treat" {
		t.Errorf("case ids = %q, %q", ctrl.CaseId, treat.CaseId)
	}

	// The control arm sends only the configured system prompt; the treatment
	// arm appends the Asset to it, byte-identically across its Cases.
	ctrlBody := rec.body(t, 0)
	if !strings.Contains(string(ctrlBody), `"system":"sys"`) {
		t.Errorf("control body has no system prompt: %s", ctrlBody)
	}
	if strings.Contains(string(ctrlBody), "the asset text") {
		t.Errorf("control body carries the Asset: %s", ctrlBody)
	}
	treatBody := rec.body(t, 1)
	if !strings.Contains(string(treatBody), `"system":"sys\n\nthe asset text"`) {
		t.Errorf("treatment body has no system prefix: %s", treatBody)
	}
	if a.RoundTrips() != 2 {
		t.Errorf("RoundTrips = %d, want 2 (both arms, one transport)", a.RoundTrips())
	}
}

// TestWithContextForwardsEstimator asserts the copy is still a full Agent:
// the budget guard keeps its per-Asset reservation through the injected arm.
func TestWithContextForwardsEstimator(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	injected, err := a.WithContext(anAsset("a1", "asset"))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}

	if _, ok := injected.(core.Estimator); !ok {
		t.Fatal("the injected Agent is not an Estimator")
	}
	est, err := injected.(core.Estimator).Estimate(context.Background(), aCase("c1", "in"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	base, err := a.Estimate(context.Background(), aCase("c1", "in"))
	if err != nil {
		t.Fatalf("control Estimate: %v", err)
	}
	if est.CostUSDMicros <= base.CostUSDMicros {
		t.Errorf("treatment reservation %d is not above the control's %d",
			est.CostUSDMicros, base.CostUSDMicros)
	}
}

// TestWithContextRefusals asserts every injection refusal happens before any
// Case is sent, with a fix that names the path forward.
func TestWithContextRefusals(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused injection reached the network")
	})

	tests := []struct {
		name  string
		asset *core.Asset
		want  string
	}{
		{"nil asset", nil, "no Asset to inject"},
		{"empty asset", anAsset("a1", ""), "has no content"},
		{"invalid utf8", anAsset("a1", string([]byte{0xff, 0xfe})), "not valid UTF-8"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.WithContext(tt.asset)
			if err == nil {
				t.Fatalf("WithContext succeeded, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
	if rec.calls() != 0 {
		t.Errorf("calls = %d, want 0", rec.calls())
	}
}

// TestWithContextAlreadyInjected asserts one Agent carries one Asset.
func TestWithContextAlreadyInjected(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	first, err := a.WithContext(anAsset("a1", "asset one"))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	if _, err := first.(*Agent).WithContext(anAsset("a2", "asset two")); err == nil {
		t.Fatal("second injection succeeded")
	}
}

// TestWithContextPromptCeiling asserts the one statement a caller makes about
// prompt size is enforced at the one moment the adapter holds the dominant
// term.
func TestWithContextPromptCeiling(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{MaxPromptBytes: 100, System: "sys"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	_, err := a.WithContext(anAsset("a1", strings.Repeat("x", 500)))
	if err == nil {
		t.Fatal("a 500-byte Asset passed a 100-byte ceiling")
	}
	if !strings.Contains(err.Error(), "max-prompt-bytes") {
		t.Errorf("err = %v, want it to name the ceiling flag", err)
	}
	_, err = a.WithContext(anAsset("a1", "small"))
	if err != nil {
		t.Fatalf("a small Asset was refused: %v", err)
	}
}

// These tests cover WithContextSet: the Portfolio-injection half of the same
// contract WithContext already proves for one Asset. See core/ring0.go's
// ContextSetInjector for why order and a whole-set ceiling are the two
// properties that matter here and nowhere else.

// TestWithContextSetJoinsEveryAssetInRankOrder asserts the treatment arm
// sends every Asset, joined in the given order, as one system prefix — and
// that the receiver stays untouched.
//
// ORDER IS PART OF THE MEASUREMENT: Validate applies PortfolioEntry.rank
// before calling this, and a caller-supplied order that the adapter silently
// resorted would measure a Portfolio other than the one Validate asked about.
func TestWithContextSetJoinsEveryAssetInRankOrder(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{System: "sys"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})

	first := anAsset("a1", "FIRST-ASSET-CONTENT")
	second := anAsset("a2", "SECOND-ASSET-CONTENT")

	injected, err := a.WithContextSet([]*core.Asset{first, second})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}
	if a.asset != "" {
		t.Error("the receiver carries the injected set — it must stay the control")
	}
	inj := injected.(*Agent)
	if want := "sys\n\nFIRST-ASSET-CONTENT\n\nSECOND-ASSET-CONTENT"; inj.systemPrefix() != want {
		t.Errorf("systemPrefix = %q, want %q", inj.systemPrefix(), want)
	}

	if _, err := injected.(core.Agent).Invoke(context.Background(), aCase("c1", "in")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got := rec.body(t, 0)
	iFirst := strings.Index(string(got), "FIRST-ASSET-CONTENT")
	iSecond := strings.Index(string(got), "SECOND-ASSET-CONTENT")
	if iFirst == -1 || iSecond == -1 {
		t.Fatalf("the request does not carry both Assets: %s", got)
	}
	if iFirst > iSecond {
		t.Errorf("the Assets were not sent in the given order: %s", got)
	}
}

// TestWithContextSetForwardsEstimator mirrors TestWithContextForwardsEstimator
// for the whole-set path: the copy is still a full Agent, so the budget
// guard keeps its reservation through the injected arm.
func TestWithContextSetForwardsEstimator(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	injected, err := a.WithContextSet([]*core.Asset{anAsset("a1", "asset one"), anAsset("a2", "asset two")})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}

	if _, ok := injected.(core.Estimator); !ok {
		t.Fatal("the injected Agent is not an Estimator")
	}
	est, err := injected.(core.Estimator).Estimate(context.Background(), aCase("c1", "in"))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	base, err := a.Estimate(context.Background(), aCase("c1", "in"))
	if err != nil {
		t.Fatalf("control Estimate: %v", err)
	}
	if est.CostUSDMicros <= base.CostUSDMicros {
		t.Errorf("treatment reservation %d is not above the control's %d",
			est.CostUSDMicros, base.CostUSDMicros)
	}
}

// TestWithContextSetRefusesAnEmptyOrNilSet asserts an Agent carrying no
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
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
				t.Error("a refused injection reached the network")
			})

			got, err := a.WithContextSet(tt.assets)
			if err == nil {
				t.Fatalf("WithContextSet accepted it and returned %T", got)
			}
			if got != nil {
				t.Errorf("a refused injection still returned an Agent (%T)", got)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("err = %v, want ErrInvalidInput", err)
			}
			if !strings.Contains(err.Error(), "control arm") {
				t.Errorf("the refusal does not explain why an empty set cannot be measured: %v", err)
			}
			if rec.calls() != 0 {
				t.Errorf("calls = %d, want 0", rec.calls())
			}
		})
	}
}

// TestWithContextSetRefusals mirrors TestWithContextRefusals for the
// whole-set path: every Asset is checked with the same rule WithContext
// applies to one, naming the offending Asset so the refusal is actionable.
func TestWithContextSetRefusals(t *testing.T) {
	t.Parallel()

	a, rec := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused injection reached the network")
	})

	tests := []struct {
		name  string
		asset *core.Asset
		want  string
	}{
		{"nil element", nil, "there is no Asset at index"},
		{"empty asset", anAsset("a1", ""), "has no content"},
		{"invalid utf8", anAsset("a1", string([]byte{0xff, 0xfe})), "not valid UTF-8"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.WithContextSet([]*core.Asset{anAsset("good", "good content"), tt.asset})
			if err == nil {
				t.Fatalf("WithContextSet succeeded, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
	if rec.calls() != 0 {
		t.Errorf("calls = %d, want 0", rec.calls())
	}
}

// TestWithContextSetAlreadyInjected asserts one Agent carries one injected
// payload, whether it arrived through WithContext or WithContextSet.
func TestWithContextSetAlreadyInjected(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	first, err := a.WithContextSet([]*core.Asset{anAsset("a1", "asset one")})
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}
	if _, err := first.(*Agent).WithContextSet([]*core.Asset{anAsset("a2", "asset two")}); err == nil {
		t.Fatal("second injection succeeded")
	}
}

// TestWithContextSetPromptCeiling asserts the whole joined set is bound
// against --max-prompt-bytes ONCE, before any Case is sent.
func TestWithContextSetPromptCeiling(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{MaxPromptBytes: 100, System: "sys"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	_, err := a.WithContextSet([]*core.Asset{
		anAsset("a1", strings.Repeat("x", 300)),
		anAsset("a2", strings.Repeat("y", 300)),
	})
	if err == nil {
		t.Fatal("a set past a 100-byte ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "max-prompt-bytes") {
		t.Errorf("err = %v, want it to name the ceiling flag", err)
	}
}

// TestWithContextSetDeclaresContextSetInject asserts the treatment arm
// reports the capability the Value stage checks before routing a Portfolio.
func TestWithContextSetDeclaresContextSetInject(t *testing.T) {
	t.Parallel()

	a, _ := newAgent(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, rawPredictOK)
	})
	injected, err := a.WithContextSet([]*core.Asset{anAsset("a1", "asset")})
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
}
