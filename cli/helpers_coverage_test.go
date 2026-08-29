package cli

// Table tests for the pure helpers the run paths hand their conversions to.
// These rows exist because every branch is a behavior: a cap that does not
// bind, a ceiling that wraps, a status that names the wrong next step.

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/evals/braintrust"
	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/langfuse"
	"github.com/knograph/kno/adapters/evals/langsmith"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

func TestGenerationParamsRows(t *testing.T) {
	t.Parallel()
	if v, err := generationParams(""); err != nil || v != nil {
		t.Errorf(`generationParams("") = %v, %v; want nil, nil`, v, err)
	}
	if v, err := generationParams("auto"); err != nil || v != nil {
		t.Errorf(`generationParams("auto") = %v, %v; want nil, nil`, v, err)
	}
	on, err := generationParams("on")
	if err != nil || on == nil || !*on {
		t.Errorf(`generationParams("on") = %v, %v; want true`, on, err)
	}
	off, err := generationParams("off")
	if err != nil || off == nil || *off {
		t.Errorf(`generationParams("off") = %v, %v; want false`, off, err)
	}
	if _, err := generationParams("sometimes"); err == nil ||
		!strings.Contains(err.Error(), "--generation-params auto, on, or off") {
		t.Errorf("generationParams(\"sometimes\") = %v; want the tri-state fix", err)
	}
}

func TestPriceOverrideRows(t *testing.T) {
	t.Parallel()
	if p, err := priceOverride(baselineFlags{}); err != nil || p != nil {
		t.Errorf("priceOverride(nothing) = %v, %v; want nil, nil", p, err)
	}
	if _, err := priceOverride(baselineFlags{priceInPerMTok: 3, priceOutPerMTok: 0}); err == nil ||
		!strings.Contains(err.Error(), "each above zero") {
		t.Errorf("priceOverride(half a pair) = %v; want the pair refusal", err)
	}
	if _, err := priceOverride(baselineFlags{priceInPerMTok: -1, priceOutPerMTok: 15}); err == nil {
		t.Error("priceOverride(negative input) = nil error; want the pair refusal")
	}
	p, err := priceOverride(baselineFlags{priceInPerMTok: 3.5, priceOutPerMTok: 15})
	if err != nil {
		t.Fatalf("priceOverride(pair) = %v", err)
	}
	if p == nil || p.InputPerMtokUsdMicros == nil || p.OutputPerMtokUsdMicros == nil ||
		*p.InputPerMtokUsdMicros != 3_500_000 || *p.OutputPerMtokUsdMicros != 15_000_000 {
		t.Errorf("priceOverride(pair) = %+v; want 3.5/15 in micros", p)
	}
}

func TestIntFromInt64Rows(t *testing.T) {
	t.Parallel()
	if got := intFromInt64(math.MaxInt32 + 1); got != math.MaxInt32 {
		t.Errorf("intFromInt64(over MaxInt32) = %d, want the clamp", got)
	}
	if got := intFromInt64(-1); got != 0 {
		t.Errorf("intFromInt64(-1) = %d, want 0", got)
	}
	if got := intFromInt64(42); got != 42 {
		t.Errorf("intFromInt64(42) = %d, want 42", got)
	}
}

func TestOptionalFloatIntRows(t *testing.T) {
	t.Parallel()
	if v := optionalFloat(math.NaN()); v != nil {
		t.Errorf("optionalFloat(NaN) = %v, want nil (unset)", v)
	}
	if v := optionalFloat(0); v == nil || *v != 0 {
		t.Errorf("optionalFloat(0) = %v, want 0 — zero is a legitimate temperature", v)
	}
	if v := optionalInt(7, false); v != nil {
		t.Errorf("optionalInt(7, false) = %v, want nil (flag not given)", v)
	}
	if v := optionalInt(0, true); v == nil || *v != 0 {
		t.Errorf("optionalInt(0, true) = %v, want 0 — zero is a legitimate seed", v)
	}
}

func TestFormatUSDRows(t *testing.T) {
	t.Parallel()
	if got := formatUSD(5_230_000); got != "$5.23" {
		t.Errorf("formatUSD(5230000) = %q, want $5.23", got)
	}
	if got := formatUSD(-5_230_000); got != "-$5.23" {
		t.Errorf("formatUSD(-5230000) = %q, want -$5.23", got)
	}
	if got := formatUSD(0); got != "$0.00" {
		t.Errorf("formatUSD(0) = %q, want $0.00", got)
	}
	if got := formatUSD(1_230_000_000); got != "$1230.00" {
		t.Errorf("formatUSD(1230000000) = %q, want $1230.00", got)
	}
}

func TestStatusNameRows(t *testing.T) {
	t.Parallel()
	tests := map[knov1.RunStatus]string{
		knov1.RunStatus_RUN_STATUS_COMPLETED:      "completed",
		knov1.RunStatus_RUN_STATUS_FAILED:         "failed",
		knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED: "budget-stopped",
		knov1.RunStatus_RUN_STATUS_INTERRUPTED:    "interrupted",
		knov1.RunStatus_RUN_STATUS_RUNNING:        "running",
		knov1.RunStatus_RUN_STATUS_UNSPECIFIED:    "unknown",
	}
	for status, want := range tests {
		if got := statusName(status); got != want {
			t.Errorf("statusName(%v) = %q, want %q", status, got, want)
		}
	}
}

func TestNextStepRows(t *testing.T) {
	t.Parallel()
	if got := nextStep(knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED); !strings.Contains(got, "--resume") {
		t.Errorf("budget-stopped next step = %q, want the resume pointer", got)
	}
	if got := nextStep(knov1.RunStatus_RUN_STATUS_INTERRUPTED); !strings.Contains(got, "--resume") {
		t.Errorf("interrupted next step = %q, want the resume pointer", got)
	}
	if got := nextStep(knov1.RunStatus_RUN_STATUS_FAILED); !strings.Contains(got, "re-run") {
		t.Errorf("failed next step = %q, want the re-run pointer", got)
	}
	if got := nextStep(knov1.RunStatus_RUN_STATUS_COMPLETED); !strings.Contains(got, "kno purge") {
		t.Errorf("completed next step = %q, want the purge pointer", got)
	}
}

func TestConcurrencyReasonNameRows(t *testing.T) {
	t.Parallel()
	if got := concurrencyReasonName(knov1.ConcurrencyReason_CONCURRENCY_REASON_COST_CAP); got != "cost-cap" {
		t.Errorf("cost-cap reason = %q, want cost-cap", got)
	}
	if got := concurrencyReasonName(knov1.ConcurrencyReason_CONCURRENCY_REASON_UNSPECIFIED); got != "unspecified" {
		t.Errorf("unspecified reason = %q, want unspecified", got)
	}
}

// TestIdentityFallsBackToBuildInfo covers the whole identity() fallback: the
// ldflags are empty in tests, so the version has to come from the module and
// the commit/date from the VCS stamp the toolchain embeds.
func TestIdentityFallsBackToBuildInfo(t *testing.T) {
	t.Parallel()
	id := identity()
	if id.Version == "" {
		t.Error("version is empty; the build-info fallback did not run")
	}
	// The rest must not crash, and a real build stamp usually fills these in.
	// Assert nothing stronger — CI and local builds embed different settings.
	if id.Commit == "" && id.Date == "" {
		t.Log("no VCS stamp in this test binary; only the version was checked")
	}
}

// TestLangsmithNewFixRows pins each refusal class to its own fix line.
func TestLangsmithNewFixRows(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		err  error
		want string
	}{
		{"missing key", errors.New("no " + langsmith.DefaultKeyEnv + " set"), "set " + langsmith.DefaultKeyEnv},
		{"unnamed dataset", errors.New("no dataset name"), "name the dataset in --evals, as langsmith:<dataset-name>"},
		{"holdout out of range", errors.New("holdout fraction"), "check --holdout-frac: it must be in [0, 1)"},
		{"endpoint refusal", errors.New("parse error"), "check LANGSMITH_ENDPOINT"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := langsmithNewFix(row.err)
			if !strings.Contains(got, row.want) {
				t.Errorf("langsmithNewFix(%q) = %q, want substring %q", row.err, got, row.want)
			}
		})
	}
}

// TestBraintrustNewFixRows pins each refusal class to its own fix line.
func TestBraintrustNewFixRows(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		err  error
		want string
	}{
		{"missing key", errors.New("no " + braintrust.KeyEnv + " set"), "set " + braintrust.KeyEnv},
		{"unnamed dataset", errors.New("no dataset name"), "name the dataset in --evals, as braintrust:<dataset-name>"},
		{"holdout out of range", errors.New("holdout fraction"), "check --holdout-frac: it must be in [0, 1)"},
		{"endpoint refusal", errors.New("parse error"), "check BRAINTRUST_API_BASE_URL"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := braintrustNewFix(row.err)
			if !strings.Contains(got, row.want) {
				t.Errorf("braintrustNewFix(%q) = %q, want substring %q", row.err, got, row.want)
			}
		})
	}
}

// TestCountsSplitFixRows names the fix by source: the dataset adapters have
// no line numbers, jsonl does.
func TestCountsSplitFixRows(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		src  evalSource
		want string
	}{
		{"langsmith", &langsmith.Evals{}, "dataset, endpoint, or example"},
		{"langfuse", &langfuse.Evals{}, "dataset, endpoint, or example"},
		{"braintrust", &braintrust.Evals{}, "dataset, endpoint, or example"},
		{"jsonl", &jsonl.Evals{}, "reported line"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := countsSplitFix(row.src); !strings.Contains(got, row.want) {
				t.Errorf("countsSplitFix(%T) = %q, want substring %q", row.src, got, row.want)
			}
		})
	}
}

// TestEnvRawValuePassthroughRows covers the two branches of the env decode:
// passthrough specs keep the raw text, typed specs run their parser.
func TestEnvRawValuePassthroughRows(t *testing.T) {
	t.Parallel()
	passthrough := specByKey["agent"]
	if got, err := envRawValue(passthrough, "exec:sh"); err != nil || got != "exec:sh" {
		t.Errorf("envRawValue(passthrough, exec:sh) = %v, %v; want exec:sh, nil", got, err)
	}
	typed := specByKey["timeout"]
	got, err := envRawValue(typed, "90s")
	if err != nil || got != 90*time.Second {
		t.Errorf("envRawValue(timeout, 90s) = %v, %v; want 90s duration, nil", got, err)
	}
}
