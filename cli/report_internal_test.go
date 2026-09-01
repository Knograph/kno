package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// reportFixture is a store holding one complete story: a finished Baseline
// over ten scored Cases, a Value run over it, and the Select and Export
// artifacts (Portfolio with correction metadata and rejection log, gaps
// record with one cluster per verdict). Watch tests flip the Value run's
// status through the store between ticks.
type reportFixture struct {
	db        *store.SQLite
	dbPath    string
	baseline  string
	value     string
	selectRun string
	exportRun string
}

func newReportFixture(t *testing.T, valueStatus knov1.RunStatus) *reportFixture {
	t.Helper()
	ctx := context.Background()

	fx := &reportFixture{
		dbPath:    filepath.Join(t.TempDir(), "kno.db"),
		baseline:  "baseline-1",
		value:     "value-1",
		selectRun: "select-1",
		exportRun: "export-1",
	}
	db, err := store.NewSQLite(ctx, fx.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	fx.db = db
	t.Cleanup(func() { _ = db.Close() })

	baseline := &knov1.Run{
		Id:     fx.baseline,
		Stage:  knov1.Stage_STAGE_BASELINE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
	}
	if err := db.CreateRun(ctx, baseline); err != nil {
		t.Fatalf("creating baseline: %v", err)
	}
	// Ten scored Cases, all passing — a reference the report can sum.
	for i := range 10 {
		id := fmt.Sprintf("case-%03d", i)
		out := &store.Outcome{CaseID: id, Score: &knov1.Score{CaseId: id, Value: 1}}
		if err := db.RecordOutcome(ctx, fx.baseline, out); err != nil {
			t.Fatalf("recording baseline outcome: %v", err)
		}
	}

	value := &knov1.Run{
		Id:            fx.value,
		Stage:         knov1.Stage_STAGE_VALUE,
		Status:        valueStatus,
		BaselineRunId: fx.baseline,
		GoalDirection: knov1.Direction_DIRECTION_MAXIMIZE,
	}
	if valueStatus == knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED {
		value.IncompleteReason = "budget cap spent"
	}
	if err := db.CreateRun(ctx, value); err != nil {
		t.Fatalf("creating value run: %v", err)
	}
	writeValueValuations(ctx, t, db, fx.value)

	selectRun := &knov1.Run{
		Id:     fx.selectRun,
		Stage:  knov1.Stage_STAGE_SELECT,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
	}
	if err := db.CreateRun(ctx, selectRun); err != nil {
		t.Fatalf("creating select run: %v", err)
	}
	scale := 0.75
	portfolio := &knov1.Portfolio{
		RunId:                fx.selectRun,
		SourceRunId:          fx.value,
		SourceStatus:         knov1.RunStatus_RUN_STATUS_COMPLETED,
		DevEstimatedGain:     0.1123,
		DevEstimatedInterval: &knov1.Interval{Low: -0.0210, High: 0.2456, NPairs: i32(20)},
		Selected: []*knov1.PortfolioEntry{
			{AssetId: "asset-0", Destination: knov1.Destination_DESTINATION_CONTEXT, Rank: 1, NRoutedScale: &scale},
			{AssetId: "asset-1", Destination: knov1.Destination_DESTINATION_CONTEXT, Rank: 2},
		},
		Rejected: []*knov1.Rejection{
			{AssetId: "asset-2", Reason: knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED},
			{AssetId: "asset-3", Reason: knov1.RejectionReason_REJECTION_REASON_REGRESSION},
			{AssetId: "asset-4", Reason: knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED},
		},
	}
	if err := db.WritePortfolio(ctx, fx.selectRun, portfolio); err != nil {
		t.Fatalf("writing portfolio: %v", err)
	}

	exportRun := &knov1.Run{
		Id:     fx.exportRun,
		Stage:  knov1.Stage_STAGE_EXPORT,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
	}
	if err := db.CreateRun(ctx, exportRun); err != nil {
		t.Fatalf("creating export run: %v", err)
	}
	gaps := &knov1.Gaps{
		RunId: fx.exportRun,
		Clusters: []*knov1.GapCluster{
			{
				Tag: "tag-a", CaseCount: 6, CoveredCount: 6,
				Status:      knov1.GapStatus_GAP_STATUS_IMPROVED,
				BestAssetId: "asset-0", BestDelta: 0.0520,
				BestInterval: &knov1.Interval{Low: 0.0010, High: 0.1030, NPairs: i32(6)},
			},
			{
				Tag: "tag-b", CaseCount: 4, CoveredCount: 4,
				Status:      knov1.GapStatus_GAP_STATUS_GAP,
				BestAssetId: "asset-1", BestDelta: -0.1200,
				BestInterval: &knov1.Interval{Low: -0.2400, High: -0.0030, NPairs: i32(4)},
			},
			{
				// Nothing covered this cluster: UNKNOWN for want of
				// coverage — the min-cluster-size boundary flavor.
				Tag: "tag-c", CaseCount: 5, CoveredCount: 0,
				Status: knov1.GapStatus_GAP_STATUS_UNKNOWN,
			},
			{
				// Covered but the interval never formed: UNKNOWN despite
				// coverage — "routed but underpowered", one below the
				// minimum of five usable pairs.
				Tag: "tag-d", CaseCount: 4, CoveredCount: 4,
				Status:      knov1.GapStatus_GAP_STATUS_UNKNOWN,
				BestAssetId: "asset-2",
			},
		},
		MultipleTesting: true,
	}
	if err := db.WriteGaps(ctx, fx.exportRun, gaps); err != nil {
		t.Fatalf("writing gaps: %v", err)
	}
	return fx
}

// writeValueValuations records the fixture's three Asset verdicts: two
// measured with intervals, one not measured at all.
func writeValueValuations(ctx context.Context, t *testing.T, db *store.SQLite, valueRun string) {
	t.Helper()
	vals := []*knov1.Valuation{
		{
			AssetId:       "asset-0",
			DeltaGoal:     0.0423,
			DeltaInterval: &knov1.Interval{Low: -0.0102, High: 0.0948, NPairs: i32(20)},
		},
		{
			AssetId:       "asset-1",
			DeltaGoal:     0.0311,
			DeltaInterval: &knov1.Interval{Low: -0.0195, High: 0.0817, NPairs: i32(20)},
		},
		{
			AssetId:     "asset-2",
			NotMeasured: knov1.RejectionReason_REJECTION_REASON_IRRELEVANT,
		},
	}
	for _, v := range vals {
		if err := db.WriteValuation(ctx, valueRun, v); err != nil {
			t.Fatalf("writing valuation: %v", err)
		}
	}
}

// i32 points at n, for the pointer-typed NPairs fields of the proto.
func i32(n int32) *int32 { return &n }

// finishRun flips a run's status through the store, the way a real run's
// writer would.
func (fx *reportFixture) finishRun(t *testing.T, runID string, status knov1.RunStatus) {
	t.Helper()
	ctx := context.Background()
	run, err := fx.db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("loading %s: %v", runID, err)
	}
	run.Status = status
	if err := fx.db.FinishRun(ctx, run); err != nil {
		t.Fatalf("finishing %s: %v", runID, err)
	}
}

// syncBuf is a bytes.Buffer safe for the watch test's concurrent reader.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds, failing the test after a deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWatchExitsOnTerminal drives the watch loop on a real store: the Value
// run starts RUNNING, the watch renders a first page, the run reaches its
// terminal status mid-watch, and the next tick exits 0 after one more
// render — the final, authoritative page.
func TestWatchExitsOnTerminal(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_RUNNING)

	var buf syncBuf
	tick := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- watchReport(context.Background(), &buf, fx.db,
			reportFlags{valueRunID: fx.value, selectRunID: fx.selectRun, exportRunID: fx.exportRun},
			tick)
	}()

	waitFor(t, "first render", func() bool {
		return strings.Contains(buf.String(), "# Kno report")
	})
	// The Select and Export runs are legitimately completed in the fixture;
	// the watched Value run is the one that must still show as running.
	if strings.Contains(buf.String(), "- Value run `value-1` (completed)") {
		t.Fatalf("first render already shows a completed value run:\n%s", buf.String())
	}

	fx.finishRun(t, fx.value, knov1.RunStatus_RUN_STATUS_COMPLETED)
	tick <- time.Now()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch exited with %v, want nil (exit 0)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not exit after the run became terminal")
	}
	if !strings.Contains(buf.String(), "(completed)") {
		t.Fatalf("the final render does not show the terminal status:\n%s", buf.String())
	}
}

// TestWatchAlreadyTerminalRendersOnce pins the other half of the exit
// contract: a finished run renders the page once and exits 0 without ever
// waiting on the ticker.
func TestWatchAlreadyTerminalRendersOnce(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_COMPLETED)

	var buf syncBuf
	done := make(chan error, 1)
	go func() {
		done <- watchReport(context.Background(), &buf, fx.db,
			reportFlags{valueRunID: fx.value, selectRunID: fx.selectRun, exportRunID: fx.exportRun},
			// A ticker that never fires: the loop must not wait on it.
			make(chan time.Time))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch exited with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not exit on an already-terminal run")
	}
	if got := strings.Count(buf.String(), "# Kno report"); got != 1 {
		t.Fatalf("rendered %d pages, want exactly 1:\n%s", got, buf.String())
	}
}

// TestWatchInterruptedBeforeTerminal pins the Ctrl-C path: an interrupted
// watch is an interrupted run, exit 4, not a silent hang.
func TestWatchInterruptedBeforeTerminal(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_RUNNING)

	var buf syncBuf
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchReport(ctx, &buf, fx.db,
			reportFlags{valueRunID: fx.value, selectRunID: fx.selectRun, exportRunID: fx.exportRun},
			tick)
	}()
	waitFor(t, "first render", func() bool { return strings.Contains(buf.String(), "# Kno report") })

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errs.ErrInterrupted) {
			t.Fatalf("watch exited with %v, want ErrInterrupted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not exit on cancellation")
	}
}

// TestReportWatchRefusesNonTerminal pins the --watch refusal: stdout is a
// bytes.Buffer here, not a terminal, so the process must exit 2 with the
// fix line — before the watch loop ever starts.
func TestReportWatchRefusesNonTerminal(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_RUNNING)

	var out, errOut bytes.Buffer
	code := Execute(context.Background(),
		[]string{
			"report", "--value-run-id", fx.value, "--select-run-id", fx.selectRun,
			"--export-run-id", fx.exportRun, "--db", fx.dbPath, "--watch",
		},
		bytes.NewReader(nil), &out, &errOut)
	if code != errs.ExitBudgetStopped {
		t.Fatalf("exit = %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "needs a terminal") {
		t.Errorf("stderr missing the refusal message:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "drop --watch") {
		t.Errorf("stderr missing the fix:\n%s", errOut.String())
	}
}

// TestReportMarkdownFullStory pins the composed document for the complete
// story: Baseline + Value + Select + Export. Every number below is the
// fixture's, and the --json equivalence test pins the same numbers — the
// two renderers cannot drift apart.
func TestReportMarkdownFullStory(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_COMPLETED)

	data, err := composeReport(context.Background(), fx.db, reportFlags{
		valueRunID:  fx.value,
		selectRunID: fx.selectRun,
		exportRunID: fx.exportRun,
	})
	if err != nil {
		t.Fatalf("composing: %v", err)
	}
	got := buildReportMarkdown(data)
	const want = `# Kno report

- Value run ` + "`value-1`" + ` (completed)
- Baseline ` + "`baseline-1`" + ` (completed)

## Baseline

score **1.000** — 10 scored

## Asset verdicts

_Deltas are in the Goal's own units; positive is toward the Goal._

| Asset | Delta (95% CI) | Corrected |
|---|---|---|
| asset-0 | +0.0423 [-0.0102, +0.0948] | ×0.7500 |
| asset-1 | +0.0311 [-0.0195, +0.0817] | — |
| asset-2 | — (routed to nothing) | — |

_×scale rows carry the Select run's routing-fraction correction to the tagged delta._

## Portfolio

Select run ` + "`select-1`" + ` (completed)

- dev-estimated gain **+0.1123** [-0.0210, +0.2456]  (selection-time; winner's-curse inflated)
- **not yet validated on holdout** — this is a selection-time estimate, winner's-curse inflation included. Run ` + "`kno validate`" + ` to measure this portfolio against the untouched holdout.

### Rejected, by reason

| Reason | Count | Assets |
|---|---|---|
| underpowered | 2 | asset-2, asset-4 |
| regression | 1 | asset-3 |

## Gaps

Export run ` + "`export-1`" + ` (completed)

| Cluster | Verdict | Coverage | Best asset | Best delta (95% CI) |
|---|---|---|---|---|
| ` + "`tag-a`" + ` | **improved** | 6 of 6 | asset-0 | +0.0520 [+0.0010, +0.1030] |
| ` + "`tag-b`" + ` | **gap** | 4 of 4 | asset-1 | -0.1200 [-0.2400, -0.0030] |
| ` + "`tag-c`" + ` | unknown — nothing routed to ≥ 5 cases | 0 of 5 | — | — |
| ` + "`tag-d`" + ` | unknown — routed but underpowered | 4 of 4 | asset-2 | — |

_This list is a discovery aid, not a test — with 4 clusters, as many as 4 of these verdicts can be noise under screening._

## Cost

| Stage | Run | Spent | LLM calls |
|---|---|---|---|
| baseline | ` + "`baseline-1`" + ` | $0.00 | 0 |
| value | ` + "`value-1`" + ` | $0.00 | 0 |
| **total** | | **$0.00** | **0** |

_Select and export make no LLM calls and are absent from this table rather than listed at zero._

_Both runs were metered and settled nothing — a zero measured, not a missing meter. The call counts above are what the meter counted._

_Recorded aggregates only: no LLM calls, no evals re-read, no trace content — the money above was spent by the runs named, not by this page._
`
	if got != want {
		t.Errorf("markdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestReportMarkdownBaselineValueOnly pins the page for the minimal story:
// the stage combos grow from here, and a regression in one combo must not
// depend on the others existing.
func TestReportMarkdownBaselineValueOnly(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_COMPLETED)

	data, err := composeReport(context.Background(), fx.db, reportFlags{valueRunID: fx.value})
	if err != nil {
		t.Fatalf("composing: %v", err)
	}
	got := buildReportMarkdown(data)
	for _, want := range []string{
		"score **1.000** — 10 scored",
		"| asset-0 | +0.0423 [-0.0102, +0.0948] | — |",
		"| asset-2 | — (routed to nothing) | — |",
		"_No Portfolio recorded: intervals are raw, uncorrected for screening._",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not yet recorded") || strings.Contains(got, "Corrected | ×") {
		t.Errorf("value-only page must not show stage artifacts:\n%s", got)
	}
}

// TestReportMarkdownBudgetStoppedSource pins the budget-stopped source: the
// page reports it in the status line, with the incomplete reason, and still
// renders every recorded verdict — a stopped run is incomplete, not absent.
func TestReportMarkdownBudgetStoppedSource(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED)

	data, err := composeReport(context.Background(), fx.db, reportFlags{
		valueRunID: fx.value, selectRunID: fx.selectRun, exportRunID: fx.exportRun,
	})
	if err != nil {
		t.Fatalf("composing: %v", err)
	}
	got := buildReportMarkdown(data)
	for _, want := range []string{
		"- Value run `value-1` (budget-stopped)",
		"- value run incomplete: budget cap spent",
		"| asset-0 | +0.0423 [-0.0102, +0.0948] | ×0.7500 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
}

// TestReportMarkdownRunningSelectPinned pins the watching case: a Select
// run that has not recorded its Portfolio yet renders "portfolio not yet
// recorded" rather than refusing the snapshot.
func TestReportMarkdownRunningSelectPinned(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_COMPLETED)

	// A second Select run, still running, that has not recorded a
	// Portfolio — the shape a live `kno select` leaves behind.
	ctx := context.Background()
	select2 := &knov1.Run{
		Id: "select-2", Stage: knov1.Stage_STAGE_SELECT,
		Status: knov1.RunStatus_RUN_STATUS_RUNNING,
	}
	if err := fx.db.CreateRun(ctx, select2); err != nil {
		t.Fatalf("creating select-2: %v", err)
	}

	data, err := composeReport(ctx, fx.db, reportFlags{
		valueRunID: fx.value, selectRunID: "select-2",
	})
	if err != nil {
		t.Fatalf("composing: %v", err)
	}
	got := buildReportMarkdown(data)
	for _, want := range []string{
		"Select run `select-2` (running)",
		"portfolio not yet recorded",
		"_No Portfolio recorded: intervals are raw, uncorrected for screening._",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
}

// TestReportDirtyBaselineRefused pins the dirty-reference refusal: a
// Baseline that Value's own rules would refuse is refused here, before a
// page can compose around it. Two flavors: ended by the error rate, and
// blended across models.
func TestReportDirtyBaselineRefused(t *testing.T) {
	ctx := context.Background()

	// The store has no valid story; only the dirty pair exists.
	db, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "kno.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	errored := &knov1.Run{
		Id: "baseline-errored", Stage: knov1.Stage_STAGE_BASELINE,
		Status:            knov1.RunStatus_RUN_STATUS_FAILED,
		ErrorRateExceeded: true,
		IncompleteReason:  "error rate exceeded",
	}
	if err := db.CreateRun(ctx, errored); err != nil {
		t.Fatalf("creating errored baseline: %v", err)
	}
	blended := &knov1.Run{
		Id: "baseline-blended", Stage: knov1.Stage_STAGE_BASELINE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED,
		CaseExecution: &knov1.CaseExecution{
			ResolvedModels: []string{"model-a", "model-b"},
		},
	}
	if err := db.CreateRun(ctx, blended); err != nil {
		t.Fatalf("creating blended baseline: %v", err)
	}
	for _, tc := range []struct {
		name        string
		runID       string
		wantRefusal string
		wantFix     string
	}{
		{
			"error rate exceeded", "baseline-errored",
			"unusable as a reference", "re-run the baseline until it completes",
		},
		{
			"blended models", "baseline-blended",
			"not an estimator", "pinned to one model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := &knov1.Run{
				Id: "value-dirty-" + tc.name, Stage: knov1.Stage_STAGE_VALUE,
				Status: knov1.RunStatus_RUN_STATUS_COMPLETED, BaselineRunId: tc.runID,
			}
			if err := db.CreateRun(ctx, value); err != nil {
				t.Fatalf("creating value run: %v", err)
			}
			_, err := composeReport(ctx, db, reportFlags{valueRunID: value.GetId()})
			if err == nil {
				t.Fatal("composed a page over a dirty baseline reference")
			}
			var a *errs.Actionable
			if !errors.As(err, &a) {
				t.Fatalf("error is %T, want *errs.Actionable", err)
			}
			if !strings.Contains(err.Error(), tc.wantRefusal) {
				t.Errorf("error missing the refusal words %q:\n%v", tc.wantRefusal, err)
			}
			if !strings.Contains(a.Fix, tc.wantFix) {
				t.Errorf("fix %q missing %q", a.Fix, tc.wantFix)
			}
		})
	}
}

// TestReportJSONGoldenAndEquivalence pins the --json contract and holds it
// to the human page: both renderers draw from one reportData, so the same
// numbers must appear in both.
func TestReportJSONGoldenAndEquivalence(t *testing.T) {
	fx := newReportFixture(t, knov1.RunStatus_RUN_STATUS_COMPLETED)

	data, err := composeReport(context.Background(), fx.db, reportFlags{
		valueRunID: fx.value, selectRunID: fx.selectRun, exportRunID: fx.exportRun,
	})
	if err != nil {
		t.Fatalf("composing: %v", err)
	}
	var buf bytes.Buffer
	if err := writeReportJSON(&buf, data); err != nil {
		t.Fatalf("writing json: %v", err)
	}
	got := buf.String()
	rep, err := decodeReportJSON([]byte(got))
	if err != nil {
		t.Fatalf("decoding json: %v", err)
	}

	if rep.ValueRunID != fx.value || rep.Baseline.RunID != fx.baseline {
		t.Errorf("run ids wrong: %+v", rep)
	}
	if rep.Baseline.Score == nil || *rep.Baseline.Score != 1.0 || rep.Baseline.Scored != 10 {
		t.Errorf("baseline wrong: %+v", rep.Baseline)
	}
	if len(rep.Assets) != 3 {
		t.Fatalf("assets = %d, want 3:\n%s", len(rep.Assets), got)
	}
	// The machine twin of the human table's rows: same asset, same
	// un-negated interval, same correction flag.
	if a := rep.Assets[0]; a.AssetID != "asset-0" ||
		*a.DeltaGoal != 0.0423 || *a.Low != -0.0102 || *a.High != 0.0948 ||
		a.NRoutedScale == nil || *a.NRoutedScale != 0.75 {
		t.Errorf("asset-0 wrong: %+v", a)
	}
	if a := rep.Assets[1]; a.AssetID != "asset-1" || a.NRoutedScale != nil {
		t.Errorf("asset-1 (unscaled) wrong: %+v", a)
	}
	if a := rep.Assets[2]; a.AssetID != "asset-2" || a.NotMeasured != "irrelevant" || a.DeltaGoal != nil {
		t.Errorf("asset-2 (not measured) wrong: %+v", a)
	}

	if rep.Portfolio == nil || rep.Portfolio.NotRecorded {
		t.Fatalf("portfolio missing or unrecorded: %+v", rep.Portfolio)
	}
	if rep.Portfolio.ValidatedOnHoldout {
		t.Error("validated_on_holdout must be false in this release: validate does not exist")
	}
	if *rep.Portfolio.DevGain != 0.1123 || *rep.Portfolio.GainLow != -0.0210 || *rep.Portfolio.GainHigh != 0.2456 {
		t.Errorf("dev estimate wrong: %+v", rep.Portfolio)
	}
	if len(rep.Portfolio.Rejected) != 2 || rep.Portfolio.Rejected[0].Reason != "underpowered" ||
		rep.Portfolio.Rejected[0].Count != 2 || len(rep.Portfolio.Rejected[0].Assets) != 2 {
		t.Errorf("rejection rows wrong: %+v", rep.Portfolio.Rejected)
	}

	if rep.Gaps == nil || rep.Gaps.NoClusterData || !rep.Gaps.MultipleTesting {
		t.Fatalf("gaps wrong: %+v", rep.Gaps)
	}
	if len(rep.Gaps.Clusters) != 4 {
		t.Fatalf("clusters = %d, want 4:\n%s", len(rep.Gaps.Clusters), got)
	}
	if c := rep.Gaps.Clusters[0]; c.Status != "improved" || c.BestAssetID != "asset-0" ||
		c.CaseCount != 6 || c.CoveredCount != 6 || *c.BestDelta != 0.0520 {
		t.Errorf("improved cluster wrong: %+v", c)
	}
	if c := rep.Gaps.Clusters[2]; c.Status != "unknown" || c.CoveredCount != 0 || c.BestAssetID != "" {
		t.Errorf("uncovered cluster wrong: %+v", c)
	}
	if c := rep.Gaps.Clusters[3]; c.Status != "unknown" || c.CoveredCount != 4 || c.BestDelta != nil {
		t.Errorf("underpowered cluster wrong: %+v", c)
	}

	// Equivalence: every number the JSON carries appears in the human
	// page's markdown — one pinned content, two renderers.
	md := buildReportMarkdown(data)
	for _, want := range []string{
		"+0.0423 [-0.0102, +0.0948]", "+0.0311 [-0.0195, +0.0817]",
		"dev-estimated gain **+0.1123**", "[-0.0210, +0.2456]", "×0.7500",
		"underpowered | 2 | asset-2, asset-4", "regression | 1 | asset-3",
		"+0.0520 [+0.0010, +0.1030]", "-0.1200 [-0.2400, -0.0030]",
		"unknown — nothing routed to ≥ 5 cases", "unknown — routed but underpowered",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("human page missing %q that --json carries", want)
		}
	}
}
