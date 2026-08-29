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
