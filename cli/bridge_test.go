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
		dbPath:           dbPath,
		selectRunID:      "sel-1",
		tuner:            "together:meta-llama/Llama-3-8b",
		maxGroups:        6,
		epochs:           3,
		priceTrainUSD:    1.50,
		priceServeUSD:    0.02,
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
