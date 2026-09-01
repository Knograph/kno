package cli_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/knograph/kno/cli"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
)

// The fake agent (fake.New(fake.Options{})) always echoes the expected
// answer, so a real `kno baseline` + `kno value` chain scores every Case
// and every Asset shows zero delta — there is no room for two Assets to
// look equivalent. The redundancy CLI surface (--explain, redundancy_evidence,
// n_redundancy_tests) is therefore exercised against a store seeded directly
// through the PUBLIC store.Store API — the same API `kno baseline`/`kno value`
// write through — rather than through the fake agent. This is the CLI
// surface under real conditions: `kno select` and `kno select --explain`
// read exactly this store through exactly the flags a user would pass.

// seedRedundantPair writes a completed Baseline run, a completed Value run
// naming it as baseline, and two behavior Assets ("a", "b") whose measured
// per-Case deltas are identical over 20 shared Cases — a fixture built to
// cross the equivalence and co-improvement thresholds once
// --redundancy-max-margin is widened, exactly as core's own redundancy
// tests do. Returns the Value run ID.
func seedRedundantPair(t *testing.T, dbPath string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	const baselineRunID, valueRunID = "base-1", "val-1"

	require.NoError(t, st.CreateRun(ctx, &knov1.Run{
		Id: baselineRunID, Stage: knov1.Stage_STAGE_BASELINE,
		Status:   knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalName: "test-goal", GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		GoalScoreDomain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
	}))
	caseIDs := make([]string, 20)
	for i := range caseIDs {
		caseIDs[i] = fmt.Sprintf("c%d", i)
		require.NoError(t, st.RecordOutcome(ctx, baselineRunID, &store.Outcome{
			CaseID: caseIDs[i],
			Score:  &knov1.Score{CaseId: caseIDs[i], Value: 0, Passed: false},
		}))
	}

	require.NoError(t, st.CreateRun(ctx, &knov1.Run{
		Id: valueRunID, Stage: knov1.Stage_STAGE_VALUE,
		Status:   knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalName: "test-goal", GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
		GoalScoreDomain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
		DevCaseCount:    100,
		BaselineRunId:   baselineRunID,
	}))

	for _, assetID := range []string{"a", "b"} {
		for i, c := range caseIDs {
			score := 0.0
			if i < 16 { // 16 of 20 pass — identical for both Assets
				score = 1
			}
			require.NoError(t, st.RecordMeasurement(ctx, valueRunID, &store.Measurement{
				Key:   store.MeasurementKey{AssetID: assetID, CaseID: c, Arm: store.ArmTreatment, Trial: 1},
				Score: &knov1.Score{CaseId: c, Value: score, Passed: score > 0},
			}))
		}
		require.NoError(t, st.WriteValuation(ctx, valueRunID, &knov1.Valuation{
			AssetId:   assetID,
			DeltaGoal: 0.8,
			DeltaInterval: &knov1.Interval{
				Low: 0.6, High: 1.0, Level: 0.95, Method: "t",
				Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED, NPairs: int32Ptr(10),
			},
			Kind:    knov1.Kind_KIND_BEHAVIOR,
			CaseIds: caseIDs,
			NRouted: int32Ptr(20),
			NDev:    int32Ptr(100),
			Cost:    &knov1.CostVector{ContextTokens: 100, AcquisitionUsdMicros: 100},
		}))
	}
	return valueRunID
}

func int32Ptr(v int32) *int32 { return &v }

// TestSelectExplainEndToEnd is acceptance criteria 15 and 16, driven through
// the actual CLI surface a user touches: `kno select` on a poolless store
// whose two Assets are measurement-equivalent emits a REDUNDANT rejection
// carrying structured RedundancyEvidence in --json, `n_redundancy_tests` is
// present, and `kno select --explain <asset-id>` prints the per-Case table,
// exits 0, in both human and --json form.
func TestSelectExplainEndToEnd(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "kno.db")
	valueRunID := seedRedundantPair(t, dbPath)

	// --- kno select (human) ---
	selectOut, selectErr, code := run(
		t,
		"select", "--value-run-id", valueRunID, "--db", dbPath,
		"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
	)
	if code != errs.ExitOK {
		t.Fatalf("select exit = %d\nstdout: %s\nstderr: %s", code, selectOut, selectErr)
	}
	require.Contains(t, selectOut, "redundant")
	require.True(t,
		strings.Contains(selectOut, "equivalent to a") || strings.Contains(selectOut, "equivalent to b"),
		"the rendered detail names the evidence, not just the reason:\n%s", selectOut)

	// --- kno select --json ---
	jsonOut, _, code := run(
		t,
		"select", "--value-run-id", valueRunID, "--db", dbPath,
		"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4", "--json",
	)
	if code != errs.ExitOK {
		t.Fatalf("select --json exit = %d\n%s", code, jsonOut)
	}
	raw, err := cli.DecodeRaw([]byte(jsonOut))
	require.NoError(t, err)

	nTests, ok := raw["n_redundancy_tests"].(float64)
	require.True(t, ok, "n_redundancy_tests missing from --json:\n%s", jsonOut)
	require.Equal(t, float64(1), nTests)

	rejected, ok := raw["rejected"].([]any)
	require.True(t, ok)
	require.Len(t, rejected, 1)
	rej, ok := rejected[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "redundant", rej["reason"])
	evidence, ok := rej["redundancy_evidence"].([]any)
	require.True(t, ok, "redundancy_evidence missing from the rejection:\n%s", jsonOut)
	require.Len(t, evidence, 1)
	ev, ok := evidence[0].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{
		"with_asset_id", "kind", "n_overlap", "paired_difference",
		"margin", "margin_source", "co_improvement", "co_improvement_floor",
		"co_improvement_floor_source", "decided_by",
	} {
		if _, present := ev[key]; !present {
			t.Errorf("redundancy_evidence missing key %q:\n%s", key, jsonOut)
		}
	}
	require.Equal(t, "measurement", ev["kind"])

	rejectedAssetID, _ := rej["asset_id"].(string)
	require.Contains(t, []string{"a", "b"}, rejectedAssetID)

	// --- kno select --explain <asset-id> (human) ---
	explainOut, explainErr, code := run(
		t,
		"select", "--value-run-id", valueRunID, "--db", dbPath,
		"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
		"--explain", rejectedAssetID,
	)
	if code != errs.ExitOK {
		t.Fatalf("--explain exit = %d\nstdout: %s\nstderr: %s", code, explainOut, explainErr)
	}
	require.Contains(t, explainOut, rejectedAssetID)
	require.Contains(t, explainOut, "CASE")
	require.Contains(t, explainOut, "c0")

	// --- kno select --explain <asset-id> --json ---
	explainJSON, _, code := run(
		t,
		"select", "--value-run-id", valueRunID, "--db", dbPath,
		"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
		"--explain", rejectedAssetID, "--json",
	)
	if code != errs.ExitOK {
		t.Fatalf("--explain --json exit = %d\n%s", code, explainJSON)
	}
	eraw, err := cli.DecodeRaw([]byte(explainJSON))
	require.NoError(t, err)
	require.Equal(t, rejectedAssetID, eraw["asset_id"])
	comparisons, ok := eraw["comparisons"].([]any)
	require.True(t, ok)
	require.Len(t, comparisons, 1)
	cmp, ok := comparisons[0].(map[string]any)
	require.True(t, ok)
	rows, ok := cmp["rows"].([]any)
	require.True(t, ok, "explain --json carries no per-Case rows:\n%s", explainJSON)
	require.Len(t, rows, 20)

	// --- --explain on an Asset that IS selected: nothing to explain, exit 0 ---
	survivor := "a"
	if rejectedAssetID == "a" {
		survivor = "b"
	}
	nothingOut, _, code := run(
		t,
		"select", "--value-run-id", valueRunID, "--db", dbPath,
		"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
		"--explain", survivor,
	)
	if code != errs.ExitOK {
		t.Fatalf("--explain on a non-redundant asset should still exit 0, got %d:\n%s", code, nothingOut)
	}
	require.Contains(t, nothingOut, "nothing to explain")
}

// poisonTransport fails any HTTP request made through it, so a test using it
// as http.DefaultTransport proves the code under test never reached the
// network — the instrument acceptance criteria 15 and 17 both call for
// ("asserted by a transport that fails on any request").
type poisonTransport struct {
	t     *testing.T
	calls atomic.Int64
}

func (p *poisonTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.calls.Add(1)
	p.t.Errorf("an HTTP request reached the network: %s %s — this command must make zero provider calls", req.Method, req.URL)
	return nil, fmt.Errorf("poisoned transport: no request is legitimate in this test")
}

// TestSelectAndExplainMakeNoProviderCall is acceptance criterion 17 as a
// test, not a comment — "Select makes no LLM call, constructs no Agent,
// creates no budget guard" — and criterion 15's requirement that --explain
// "makes zero provider calls", both driven through the SAME instrument: a
// poisoned http.DefaultTransport that fails the test if anything reaches it.
// core/redundancy_test.go's TestSelectOptionsConstructsNoAgentAndNoGuard is
// this test's structural sibling — a reflection check that no field of
// SelectOptions COULD reach the network in the first place. This one proves
// the running command actually does not, at the transport a real Agent
// adapter would use.
//
// NOT parallel: it swaps the process-global http.DefaultTransport for its
// duration, and a sibling test issuing a real HTTP request during that
// window would wrongly fail (or, worse, wrongly pass by exercising the
// wrong transport) — the same reason cli_test.go's withoutEnv is not run
// under t.Parallel either.
func TestSelectAndExplainMakeNoProviderCall(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kno.db")
	valueRunID := seedRedundantPair(t, dbPath)

	poisoned := &poisonTransport{t: t}
	old := http.DefaultTransport
	http.DefaultTransport = poisoned
	t.Cleanup(func() { http.DefaultTransport = old })

	scenarios := [][]string{
		{"select", "--value-run-id", valueRunID, "--db", dbPath, "--max-context-tokens", "100000", "--redundancy-max-margin", "0.4"},
		{"select", "--value-run-id", valueRunID, "--db", dbPath, "--max-context-tokens", "100000", "--redundancy-max-margin", "0.4", "--json"},
		{"select", "--value-run-id", valueRunID, "--db", dbPath, "--max-context-tokens", "100000", "--redundancy-max-margin", "0.4", "--explain", "a"},
		{"select", "--value-run-id", valueRunID, "--db", dbPath, "--max-context-tokens", "100000", "--redundancy-max-margin", "0.4", "--explain", "b", "--json"},
	}
	for _, args := range scenarios {
		out, stderr, code := run(t, args...)
		if code != errs.ExitOK {
			t.Fatalf("exit = %d for %v\nstdout: %s\nstderr: %s", code, args, out, stderr)
		}
	}
	require.Zero(t, poisoned.calls.Load(), "select/--explain reached the network")
}

// TestNoSelectJSONFloatCarriesMoreThanFourPlaces is the same treatment
// cli/judge_test.go's TestNoJudgeJSONFloatCarriesMoreThanFourPlaces applies,
// for the same reason: a redundancy statistic's tail digits differ between
// arm64 and amd64 (interval.Paired's t-quantile bisection, the percentile
// bootstrap's order statistics), so no golden holding an unrounded value can
// pass on both. core/redundancy.go rounds every RedundancyEvidence float at
// the source (round4/roundInterval); this asserts the PROPERTY across the
// actual emitted documents rather than trusting that every call site
// remembered to.
func TestNoSelectJSONFloatCarriesMoreThanFourPlaces(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "kno.db")
	valueRunID := seedRedundantPair(t, dbPath)

	scenarios := [][]string{
		{
			"select", "--value-run-id", valueRunID, "--db", dbPath,
			"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4", "--json",
		},
		{
			"select", "--value-run-id", valueRunID, "--db", dbPath,
			"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
			"--explain", "a", "--json",
		},
		{
			"select", "--value-run-id", valueRunID, "--db", dbPath,
			"--max-context-tokens", "100000", "--redundancy-max-margin", "0.4",
			"--explain", "b", "--json",
		},
	}
	for _, args := range scenarios {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			out, stderr, code := run(t, args...)
			if code != errs.ExitOK {
				t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", code, out, stderr)
			}
			raw, err := cli.DecodeRaw([]byte(out))
			require.NoError(t, err)

			// Scoped to what this change actually emits: `--explain`'s whole
			// document is new, so it is walked in full. `select --json`'s
			// document is not — dev_estimated_gain and its interval are a
			// PRE-EXISTING, unrounded computation in core/select.go that
			// predates the redundancy-detection plan (docs/debt.md#164) and
			// is out of this change's scope to fix; walking the whole
			// document here would make this test enforce an invariant on
			// code this PR does not touch. `rejected` is where every
			// float this PR introduces (redundancy_evidence) lives, so
			// that subtree is walked in full instead.
			if rejected, ok := raw["rejected"]; ok {
				walkFloatsRedundancy(t, "rejected", rejected)
			} else {
				walkFloatsRedundancy(t, "", raw)
			}
		})
	}
}

// walkFloatsRedundancy is cli/judge_test.go's walkFloats, duplicated rather
// than shared: that one lives in an internal test file scoped to judge's own
// scenarios, and the two packages' test binaries do not share unexported
// helpers across files by convention here — each CLI surface's float
// property test owns its own walk.
func walkFloatsRedundancy(t *testing.T, path string, v any) {
	t.Helper()
	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			walkFloatsRedundancy(t, path+"."+k, child)
		}
	case []any:
		for i, child := range typed {
			walkFloatsRedundancy(t, fmt.Sprintf("%s[%d]", path, i), child)
		}
	case float64:
		if want := math.Round(typed*1e4) / 1e4; typed != want {
			t.Errorf("%s = %v carries more than four decimal places.\n"+
				"An unrounded statistic differs between arm64 and amd64 in its tail "+
				"digits, so no golden can hold it. Round it at the source, in "+
				"core/redundancy.go, as judge/kappa.go does for its own statistics.",
				strings.TrimPrefix(path, "."), typed)
		}
	}
}
