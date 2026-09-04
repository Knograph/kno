package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// bridgeFixturePool is a minimal core.Pool for the bridge CLI's tests.
type bridgeFixturePool struct{ assets []*core.Asset }

func (p bridgeFixturePool) Assets(context.Context) (iter.Seq2[*core.Asset, error], error) {
	return func(yield func(*core.Asset, error) bool) {
		for _, a := range p.assets {
			if !yield(a, nil) {
				return
			}
		}
	}, nil
}

// bridgeFixture builds a database carrying: a Value run with a persisted
// value.Plan (one qualifying cluster), and a Select run's Portfolio with
// one tuning-set Asset routed to that cluster. Returns the database's file
// path (runBridge opens its own handle on it, matching every other CLI
// command) and the pool to inject.
func bridgeFixture(t *testing.T) (dbPath string, pool core.Pool) {
	t.Helper()
	ctx := context.Background()
	dbPath = filepath.Join(t.TempDir(), "kno.db")
	st, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Two assets in two different qualifying clusters: "tune-a" in
	// "refunds", "tune-b" in "billing". This makes each leave-one-out
	// training set non-empty (leave-refunds-out = [tune-b], and vice
	// versa) — a single-cluster, single-asset population would make the
	// one leave-one-out group empty, which QuoteGroups correctly refuses as
	// a zero-example fine-tune, but that refusal is not what these tests
	// are checking.
	refundsCases := []string{"c1", "c2", "c3", "c4", "c5"} // core.MinClusterCases
	billingCases := []string{"b1", "b2", "b3", "b4", "b5"}
	plan := value.Plan{
		Routed: []value.AssetRouting{
			{AssetID: "tune-a", CaseIDs: refundsCases},
			{AssetID: "tune-b", CaseIDs: billingCases},
		},
		Clusters: []value.ClusterSnapshot{
			{Tag: "refunds", CaseIDs: refundsCases},
			{Tag: "billing", CaseIDs: billingCases},
		},
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(plan); err != nil {
		t.Fatalf("encoding value plan: %v", err)
	}

	valueRun := &knov1.Run{
		Id: "val-1", Stage: knov1.Stage_STAGE_VALUE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, ValuePlan: buf.Bytes(),
	}
	if err := st.CreateRun(ctx, valueRun); err != nil {
		t.Fatalf("CreateRun val-1: %v", err)
	}

	selectRun := &knov1.Run{Id: "sel-1", Stage: knov1.Stage_STAGE_SELECT, Status: knov1.RunStatus_RUN_STATUS_COMPLETED}
	if err := st.CreateRun(ctx, selectRun); err != nil {
		t.Fatalf("CreateRun sel-1: %v", err)
	}

	p := &knov1.Portfolio{
		RunId: "sel-1", SourceRunId: "val-1",
		Selected: []*knov1.PortfolioEntry{
			{
				AssetId: "tune-a", Destination: knov1.Destination_DESTINATION_TUNING_SET,
				Valuation: &knov1.Valuation{AssetId: "tune-a", Kind: knov1.Kind_KIND_BEHAVIOR},
			},
			{
				AssetId: "tune-b", Destination: knov1.Destination_DESTINATION_TUNING_SET,
				Valuation: &knov1.Valuation{AssetId: "tune-b", Kind: knov1.Kind_KIND_BEHAVIOR},
			},
		},
	}
	if err := st.WritePortfolio(ctx, "sel-1", p); err != nil {
		t.Fatalf("WritePortfolio: %v", err)
	}

	pool = bridgeFixturePool{assets: []*core.Asset{
		{Id: "tune-a", Content: []byte("demonstrate refunding a duplicate charge")},
		{Id: "tune-b", Content: []byte("demonstrate resolving a billing dispute")},
	}}
	return dbPath, pool
}

func bridgeTestFlags(dbPath string) bridgeFlags {
	return bridgeFlags{
		dbPath:        dbPath,
		selectRunID:   "sel-1",
		tuner:         "together:meta-llama/Llama-3-8b",
		maxGroups:     6,
		epochs:        3,
		priceTrainUSD: 1.50,
		priceServeUSD: 0.02,
		// bridgeFlags{} literals bypass cobra entirely, so priceServeUSDSet
		// has to be set by hand here to stand in for
		// cmd.Flags().Changed("price-serve-per-minute") — see
		// TestNewBridgeCmdCapturesPriceServeUSDSet for the cobra-level
		// proof that the real flag parsing wires this correctly.
		priceServeUSDSet: true,
		maxLiveEndpoints: 1,
		maxServeMinutes:  30,
	}
}

// TestBridgeUnarmedPrintsThePlanAndSpendsNothing is acceptance criterion 1:
// without --bridge, the command exits 0, prints a per-job table and a
// total. This build's un-armed path never constructs a core.Tuner or an
// HTTP client at all (see runBridgeCore: bridge.QuoteGroups takes no
// Tuner), which makes "zero HTTP requests" true by construction rather
// than by a mocked transport.
func TestBridgeUnarmedPrintsThePlanAndSpendsNothing(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err != nil {
		t.Fatalf("runBridgeCore: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "all-in") {
		t.Errorf("output does not mention the all-in job: %q", got)
	}
	if !strings.Contains(got, "refunds") {
		t.Errorf("output does not mention the refunds leave-one-out job: %q", got)
	}
	if !strings.Contains(got, "Total") {
		t.Errorf("output does not print a total: %q", got)
	}
	if !strings.Contains(got, "Re-run with --bridge to submit") {
		t.Errorf("output does not carry the un-armed fix line: %q", got)
	}
}

// TestBridgeArmedWithoutYesDeclines is acceptance criterion 2's shape: an
// armed run with nobody able to confirm declines through
// errs.ErrBudgetExceeded's path, exit 2, and never reaches a Tuner (this
// build never constructs one at all — see confirmAndStop).
func TestBridgeArmedWithoutYesDeclines(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)
	f.bridgeArmed = true
	// The fixture's training files are a few dozen bytes each, so the real
	// per-MTok rate leaves the total estimate well under
	// confirmThresholdUSD — and Guard.Authorize does not ask for
	// confirmation at all below it. A synthetic, very high rate is what
	// makes this test actually exercise the confirmation path rather than
	// silently passing through it.
	f.priceTrainUSD = 1_000_000

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want a decline; nobody confirmed the spend")
	}
	if !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Errorf("err = %v, want errs.ErrBudgetExceeded", err)
	}
	if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d", got, errs.ExitBudgetStopped)
	}
}

// TestBridgeArmedWithYesRequiresEvals pins that --yes clears confirmation
// but the command still refuses to claim success without --evals: the
// per-group measurement needs Case CONTENT, which only --evals supplies,
// so an armed run with no --evals refuses before any Tuner is even
// constructed rather than exiting 0 (or reaching the network) having
// nothing to measure with.
func TestBridgeArmedWithYesRequiresEvals(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)
	f.bridgeArmed = true
	f.yes = true

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want an error: --evals is required once --bridge is armed")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "--evals") {
		t.Errorf("error does not name --evals: %q", err.Error())
	}
	// The plan was still printed before the stop.
	if !strings.Contains(out.String(), "Total") {
		t.Errorf("plan was not printed before the stop: %q", out.String())
	}
}

// TestBridgeArmedRefusesAPlanCaseIDMissingFromEvals is edge case 1: a Case
// ID the value.Plan names with no Case behind it in --evals refuses the
// WHOLE run before any job is submitted, naming the missing Case.
func TestBridgeArmedRefusesAPlanCaseIDMissingFromEvals(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)
	f.bridgeArmed = true
	f.yes = true
	// split-seed "8" is the one this file's TestBridgeArmedWithEvalsReachesTunerConstruction
	// verified lands every one of these 10 Case IDs in DEV under the
	// default holdout fraction — deterministic, so c5 is the ONLY Case ID
	// this test's omission can produce as missing.
	f.splitSeed = "8"
	// Only 4 of the 5 "refunds" Cases the value.Plan names — c5 is
	// missing.
	f.evalsPath = writeBridgeEvalsFixture(t, "c1", "c2", "c3", "c4", "b1", "b2", "b3", "b4", "b5")

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want a refusal: c5 has no Case in --evals")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "c5") {
		t.Errorf("error does not name the missing Case c5: %q", err.Error())
	}
}

// TestBridgeArmedWithEvalsReachesTunerConstruction proves the pipeline
// this PR wires — --evals resolution, the pre-flight Case-ID
// completeness check, Goal resolution — all succeed and the run reaches
// real Tuner construction, which then refuses for the only reason it can
// in a test environment: no TOGETHER_API_KEY. This is as far as a unit
// test can drive the armed path without a live network double for
// together.Tuner and openaicompat.Agent (see this PR's report).
func TestBridgeArmedWithEvalsReachesTunerConstruction(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "")
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)
	f.bridgeArmed = true
	f.yes = true
	// split-seed "8" was found by brute force to land every one of these
	// 10 Case IDs in DEV under DefaultHoldoutFrac (adapters/evals/split's
	// hash-based per-Case split has no seed that forces zero holdout in
	// general — 0.0 itself means "use the default fraction", per
	// jsonl.Options.HoldoutFrac's own doc — so a specific seed is the only
	// deterministic way to pin every fixture Case to dev).
	f.splitSeed = "8"
	f.evalsPath = writeBridgeEvalsFixture(t, "c1", "c2", "c3", "c4", "c5", "b1", "b2", "b3", "b4", "b5")

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want an error: no TOGETHER_API_KEY is set")
	}
	if strings.Contains(err.Error(), "--evals") || strings.Contains(err.Error(), "Case ID") {
		t.Errorf("got an evals-shaped refusal, want a credential refusal — evals resolution should have succeeded: %q", err.Error())
	}
}

// writeBridgePoolFixture writes a JSONL --pool file carrying the same two
// Assets bridgeFixture's injected core.Pool does, for the cobra-level tests
// that need a real --pool STRING (going through resolvePool's grammar)
// rather than an injected Pool value.
func writeBridgePoolFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pool.jsonl")
	content := `{"id":"tune-a","content":"demonstrate refunding a duplicate charge"}
{"id":"tune-b","content":"demonstrate resolving a billing dispute"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing pool fixture: %v", err)
	}
	return path
}

// TestNewBridgeCmdCapturesPriceServeUSDSet is the cobra-level half of
// resolveServePrice's "confirmed zero" fix
// (docs/plans/2026-09-02-openai-tuner.md §5). Every other bridge CLI test
// builds a bridgeFlags{} literal directly, bypassing cobra entirely — which
// can only exercise resolveServePrice ONCE priceServeUSDSet is already set,
// never prove that newBridgeCmd's RunE actually captures
// cmd.Flags().Changed("price-serve-per-minute") into it. This drives the
// real *cobra.Command both ways.
func TestNewBridgeCmdCapturesPriceServeUSDSet(t *testing.T) {
	dbPath, _ := bridgeFixture(t)
	poolPath := writeBridgePoolFixture(t)
	base := []string{
		"--select-run-id", "sel-1", "--db", dbPath, "--pool", poolPath,
		"--tuner", "together:meta-llama/Llama-3-8b", "--price-train-per-mtok", "1.50",
	}

	t.Run("omitted: refused, naming the escape hatch", func(t *testing.T) {
		cmd := newBridgeCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(base)
		err := cmd.Execute()
		if err == nil {
			t.Fatal("want a refusal: no --price-serve-per-minute and no table row for together:meta-llama/Llama-3-8b")
		}
		if !strings.Contains(err.Error(), "--price-serve-per-minute") {
			t.Errorf("error does not name the escape hatch: %q", err.Error())
		}
	})

	t.Run("explicit zero: accepted as a confirmed zero, not an absent flag", func(t *testing.T) {
		cmd := newBridgeCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(append([]string{}, base...), "--price-serve-per-minute", "0"))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("explicit --price-serve-per-minute 0 should be accepted: %v", err)
		}
		if !strings.Contains(out.String(), "Total") {
			t.Errorf("plan was not printed: %q", out.String())
		}
		if !strings.Contains(out.String(), "Eval pass") {
			t.Errorf("plan does not carry the eval-pass line: %q", out.String())
		}
	})
}

// writeBridgeEvalsFixture writes a minimal JSONL --evals file with one
// Case per id, for the bridge CLI tests that need real Case content
// behind the value.Plan's Case IDs.
func writeBridgeEvalsFixture(t *testing.T, ids ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evals.jsonl")
	var buf bytes.Buffer
	for _, id := range ids {
		buf.WriteString(`{"id":"` + id + `","input":"hello"}` + "\n")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing evals fixture: %v", err)
	}
	return path
}

// TestResolveServePriceExplicitZeroVersusAbsent is the unit-level half of
// TestNewBridgeCmdCapturesPriceServeUSDSet: resolveServePrice itself must
// accept an explicit zero (Set true) as a confirmed rate and still refuse
// an unset flag (Set false), for a model with no table row.
func TestResolveServePriceExplicitZeroVersusAbsent(t *testing.T) {
	got, err := resolveServePrice("together", "meta-llama/Llama-3-8b", 0, true)
	if err != nil {
		t.Fatalf("explicit zero: resolveServePrice returned %v, want no error", err)
	}
	if got.PerMinuteUSDMicros != 0 {
		t.Errorf("explicit zero: PerMinuteUSDMicros = %d, want 0", got.PerMinuteUSDMicros)
	}

	if _, err := resolveServePrice("together", "meta-llama/Llama-3-8b", 0, false); err == nil {
		t.Fatal("unset flag with no table row: want a refusal, got none")
	} else if !strings.Contains(err.Error(), "--price-serve-per-minute") {
		t.Errorf("error does not name the escape hatch: %q", err.Error())
	}

	if _, err := resolveServePrice("together", "meta-llama/Llama-3-8b", -1, true); err == nil {
		t.Fatal("negative explicit rate: want a refusal, got none")
	}
}

// TestResolveEvalPriceTogetherStaysNil pins the eval-seam pricing plan's
// core "no behaviour change" claim (docs/plans/2026-09-02-openai-tuner.md
// §2): pricing.LookupFineTunedPrice carries zero "together" rows, so
// resolveEvalPrice must return nil for it — the same nil that made
// AcceptFreeCalls true before this PR existed.
func TestResolveEvalPriceTogetherStaysNil(t *testing.T) {
	if got := resolveEvalPrice("together", "meta-llama/Llama-3-8b"); got != nil {
		t.Errorf("resolveEvalPrice(together, ...) = %v, want nil (table.go carries no together fine-tuned rows)", got)
	}
}

// TestBridgeUnarmedTogetherPlanOmitsEvalPassCost is the integration-level
// half of TestResolveEvalPriceTogetherStaysNil: the printed plan's eval-pass
// line is $0.00 and the total is unaffected, so a Together run's figures
// are byte-for-byte what they were before this PR.
func TestBridgeUnarmedTogetherPlanOmitsEvalPassCost(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)

	var out bytes.Buffer
	if err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool); err != nil {
		t.Fatalf("runBridgeCore: %v", err)
	}
	if !strings.Contains(out.String(), "Eval pass (worst case):        $0.00") {
		t.Errorf("eval-pass line is not a confirmed zero: %q", out.String())
	}
}

// TestResolveEvalPriceOpenAIStaysNilInThisPR pins
// docs/plans/2026-09-02-openai-tuner.md's explicit scope line: fineTunedTable
// ships empty for "openai" too, in THIS PR — inventing a rate is exactly
// what three plan-review rounds rejected. resolveEvalPrice must return nil
// for every OpenAI model until a reviewed diff (internal/cmd/pricingcheck)
// adds rows, the same as it does for "together".
func TestResolveEvalPriceOpenAIStaysNilInThisPR(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if got := resolveEvalPrice("openai", model); got != nil {
			t.Errorf("resolveEvalPrice(openai, %q) = %v, want nil (fineTunedTable ships empty for openai in this PR)", model, got)
		}
	}
}

// TestAcceptFreeCalls pins docs/debt.md#162's repayment: AcceptFreeCalls is
// true ONLY when both an absent per-token price AND a real, nonzero hosting
// charge already cover the eval pass — see acceptFreeCalls's own doc.
func TestAcceptFreeCalls(t *testing.T) {
	tests := []struct {
		name       string
		evalPrice  *knov1.Price
		servePrice pricing.ServePrice
		want       bool
	}{
		{
			name:       "together today: no per-token price, real hosting ticker",
			evalPrice:  nil,
			servePrice: pricing.ServePrice{PerMinuteUSDMicros: 20_000},
			want:       true,
		},
		{
			name:       "openai today: no per-token price (fineTunedTable ships empty), genuine-zero hosting",
			evalPrice:  nil,
			servePrice: pricing.ServePrice{PerMinuteUSDMicros: 0},
			want:       false, // the refusal IS the behaviour — see acceptFreeCalls's doc
		},
		{
			name:       "a future per-token scheme with a real fineTunedTable row",
			evalPrice:  &knov1.Price{InputPerMtokUsdMicros: int64Ptr(4_000_000)},
			servePrice: pricing.ServePrice{PerMinuteUSDMicros: 0},
			want:       false,
		},
		{
			name:       "a priced model even alongside a nonzero hosting rate never double-asserts free",
			evalPrice:  &knov1.Price{InputPerMtokUsdMicros: int64Ptr(4_000_000)},
			servePrice: pricing.ServePrice{PerMinuteUSDMicros: 20_000},
			want:       false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptFreeCalls(tc.evalPrice, tc.servePrice); got != tc.want {
				t.Errorf("acceptFreeCalls(%v, %+v) = %v, want %v", tc.evalPrice, tc.servePrice, got, tc.want)
			}
		})
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestBridgeRefusesUnpricedModelWithoutTheEscapeHatch is acceptance
// criterion 11: a base model with no training price is refused, naming
// --price-train-per-mtok as the fix.
func TestBridgeRefusesUnpricedModelWithoutTheEscapeHatch(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	f := bridgeTestFlags(dbPath)
	f.priceTrainUSD = 0 // no escape hatch supplied

	var out bytes.Buffer
	err := runBridgeCore(context.Background(), &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want a refusal: together:meta-llama/Llama-3-8b has no shipped price")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want errs.ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "--price-train-per-mtok") {
		t.Errorf("error does not name the escape hatch: %q", err.Error())
	}
}

// TestBridgeRefusesKnowledgeAssetInTuningSet is acceptance criterion 15,
// driven through the CLI layer.
func TestBridgeRefusesKnowledgeAssetInTuningSet(t *testing.T) {
	dbPath, pool := bridgeFixture(t)
	ctx := context.Background()

	st, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}
	p, err := st.Portfolio(ctx, "sel-1")
	if err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	p.Selected = append(p.Selected, &knov1.PortfolioEntry{
		AssetId: "leaked", Destination: knov1.Destination_DESTINATION_TUNING_SET,
		Valuation: &knov1.Valuation{AssetId: "leaked", Kind: knov1.Kind_KIND_KNOWLEDGE},
	})
	if err := st.WritePortfolio(ctx, "sel-1", p); err != nil {
		t.Fatalf("WritePortfolio: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}

	f := bridgeTestFlags(dbPath)
	var out bytes.Buffer
	err = runBridgeCore(ctx, &bytes.Buffer{}, &out, f, pool)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "leaked") {
		t.Errorf("error does not name the Asset: %q", err.Error())
	}
}

// TestBridgeCommandHelpMentionsItsFlags is acceptance criterion 25:
// `kno bridge --help` mentions every flag the plan names, plus the two
// literal sentences about irreversibility and per-minute hosting billing.
func TestBridgeCommandHelpMentionsItsFlags(t *testing.T) {
	cmd := newBridgeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	help := out.String()
	for _, flag := range []string{
		"--bridge", "--tuner", "--bridge-max-groups", "--bridge-timeout",
		"--price-train-per-mtok", "--price-serve-per-minute",
		"--bridge-max-live-endpoints", "--bridge-max-serve-minutes",
	} {
		if !strings.Contains(help, flag) {
			t.Errorf("--help does not mention %s", flag)
		}
	}
	for _, phrase := range []string{"cannot be un-submitted", "per minute", "idle"} {
		if !strings.Contains(help, phrase) {
			t.Errorf("--help does not mention %q (the irreversibility / per-minute-hosting sentence)", phrase)
		}
	}
}
