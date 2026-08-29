package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// exportFixture is a Portfolio with three selected entries — one per
// Destination — plus the pool that renders them, and the Select run the
// Export run is recorded against.
func exportFixture(t *testing.T, st store.Store) (string, stubPool) {
	t.Helper()

	val := func(id, _ string, dest knov1.Destination, kind knov1.Kind) *PortfolioEntry {
		return &PortfolioEntry{
			AssetId:     id,
			Destination: dest,
			Rank:        1,
			Valuation: &Valuation{
				AssetId:   id,
				DeltaGoal: 0.5,
				DeltaInterval: &Interval{
					Low: 0.3, High: 0.7, Level: 0.95, Method: "t",
					Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED, NPairs: int32Ptr(10),
				},
				Kind: kind,
			},
		}
	}
	selRun := &knov1.Run{
		Id:              "sel-1",
		Stage:           knov1.Stage_STAGE_SELECT,
		Status:          knov1.RunStatus_RUN_STATUS_COMPLETED,
		GoalName:        "test-goal",
		GoalDirection:   knov1.Direction_DIRECTION_MAXIMIZE,
		GoalScoreDomain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY,
		DevCaseCount:    100,
	}
	require.NoError(t, st.CreateRun(context.Background(), selRun))
	p := &knov1.Portfolio{
		RunId:        "sel-1",
		SourceRunId:  "val-1",
		SourceStatus: knov1.RunStatus_RUN_STATUS_COMPLETED,
		Selected: []*PortfolioEntry{
			val("ctx-a", "", knov1.Destination_DESTINATION_CONTEXT, knov1.Kind_KIND_KNOWLEDGE),
			val("kb-a", "", knov1.Destination_DESTINATION_KNOWLEDGE_BASE, knov1.Kind_KIND_KNOWLEDGE),
			val("tune-a", "", knov1.Destination_DESTINATION_TUNING_SET, knov1.Kind_KIND_BEHAVIOR),
		},
	}
	// Ranks in selection order.
	for i, e := range p.Selected {
		e.Rank = int32(i + 1)
	}
	require.NoError(t, st.WritePortfolio(context.Background(), "sel-1", p))
	pool := stubPool{assets: []*Asset{
		{
			Id: "ctx-a", Content: []byte("The quick brown fox jumps over the lazy dog."),
			Title: "Context asset", Kind: knov1.Kind_KIND_KNOWLEDGE,
			Provenance: &knov1.Provenance{Source: "jsonl"},
		},
		{
			Id: "kb-a", Content: []byte("KB fact one: the sky is blue.\nKB fact two: gravity works."),
			Title: "KB asset", Kind: knov1.Kind_KIND_KNOWLEDGE,
			Provenance: &knov1.Provenance{Source: "mcp:notion"},
		},
		{
			Id: "tune-a", Content: []byte("translate the following to french: hello world"),
			Title: "Tune asset", Kind: knov1.Kind_KIND_BEHAVIOR,
			Provenance: &knov1.Provenance{Source: "transcripts"},
		},
	}}
	return "sel-1", pool
}

func exportOpts(st store.Store, pool Pool, dest knov1.Destination, path string, force bool) ExportOptions {
	return ExportOptions{
		RunID:       "exp-1",
		SelectRunID: "sel-1",
		Store:       st,
		Pool:        pool,
		Destination: dest,
		Path:        path,
		Force:       force,
	}
}

func runExport(t *testing.T, o ExportOptions) (*ExportResult, error) {
	t.Helper()
	res, err := o.Export(context.Background())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = o.Store.Close() })
	return res, nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// TestExportTuningSetPinned: the tuning set is OpenAI chat format JSONL —
// the pinned shape Tuner adapters will parse — one user message per selected
// Asset, in selection order, with the manifest beside it.
func TestExportTuningSetPinned(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	selRun, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	res, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
	require.NoError(t, err)
	require.Equal(t, 1, res.AssetCount)
	require.Equal(t, knov1.Destination_DESTINATION_TUNING_SET, res.Destination)

	want := "{\"messages\":[{\"role\":\"user\",\"content\":\"translate the following to french: hello world\"}]}\n"
	require.Equal(t, want, readFile(t, path))
	manifest := readFile(t, path+".manifest.md")
	require.Contains(t, manifest, "- Select run: `sel-1`")
	require.Contains(t, manifest, "- Source Value run: `val-1`")
	require.Contains(t, manifest, "- Destination: `tuning_set`")
	require.Contains(t, manifest, "1. `tune-a` — Tune asset (provenance: transcripts)")
	require.NotNil(t, res.BytesWritten)
	require.Equal(t, selRun, selRun) // keep the var: the fixture's run ID is asserted via the manifest
}

// TestExportContextPackPinned: the context grammar is the selected Assets'
// content, one document after another — the pack an agent actually reads.
func TestExportContextPackPinned(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "context.txt")
	res, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_CONTEXT, path, false))
	require.NoError(t, err)
	require.Equal(t, 1, res.AssetCount)
	require.Equal(t, "The quick brown fox jumps over the lazy dog.", readFile(t, path))
	require.Contains(t, readFile(t, path+".manifest.md"), "- Destination: `context`")
}

// TestExportKnowledgeBasePinned: the knowledge-base grammar is the
// human-readable instruction list the writable-KB adapters (v0.2) will
// consume — every document named with where it came from.
func TestExportKnowledgeBasePinned(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "kb.md")
	res, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_KNOWLEDGE_BASE, path, false))
	require.NoError(t, err)
	require.Equal(t, 1, res.AssetCount)
	got := readFile(t, path)
	require.Contains(t, got, "# Knowledge base add-list")
	require.Contains(t, got, "## 1. KB asset")
	require.Contains(t, got, "- Asset ID: `kb-a`")
	require.Contains(t, got, "- Provenance: `mcp:notion`")
	require.Contains(t, got, "KB fact one: the sky is blue.")
	require.Contains(t, readFile(t, path+".manifest.md"), "- Destination: `knowledge_base`")
}

// TestExportIdempotentAndAtomic: two exports of the same Portfolio are
// byte-identical — the artifact is a pure function of the record — and the
// run record cannot change the Portfolio it was derived from.
func TestExportIdempotentAndAtomic(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.jsonl")
	path2 := filepath.Join(dir, "b.jsonl")
	res1, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path1, false))
	require.NoError(t, err)
	o2 := exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path2, false)
	o2.RunID = "exp-2"
	res2, err := runExport(t, o2)
	require.NoError(t, err)
	require.Equal(t, readFile(t, path1), readFile(t, path2))
	require.Equal(t, readFile(t, path1+".manifest.md"), readFile(t, path2+".manifest.md"))
	require.Equal(t, res1.BytesWritten, res2.BytesWritten)
	// The store still holds the same Portfolio: export never mutates it.
	after, err := st.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	require.Len(t, after.GetSelected(), 3)
	require.Len(t, after.GetRejected(), 0)
}

// TestExportRefusesOverwriteWithoutForce: an existing target is refused —
// an overwritten export is a silent mutation — and --force is the explicit
// word that replaces it.
func TestExportRefusesOverwriteWithoutForce(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	o := exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false)
	_, err := runExport(t, o)
	require.NoError(t, err)
	first := readFile(t, path)

	// A fresh run ID: the file refusal must be what fires, not the
	// run-already-exists guard.
	o2 := o
	o2.RunID = "exp-2"
	_, err = runExport(t, o2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
	require.Equal(t, first, readFile(t, path), "refused export must leave the file untouched")

	o3 := o2
	o3.Force = true
	res, err := runExport(t, o3)
	require.NoError(t, err)
	require.Equal(t, 1, res.AssetCount)
	require.Equal(t, first, readFile(t, path), "a forced re-export of the same Portfolio is identical")
}

// TestExportNeverPartial: a failed write leaves no partial artifact — the
// temp file is cleaned up and the target never existed or keeps its old
// bytes.
func TestExportNeverPartial(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	// A path whose parent does not exist: the temp file cannot be created,
	// and nothing may appear at the target.
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "training.jsonl")
	_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
	require.Error(t, err)
	require.Contains(t, err.Error(), "temp file")
	_, err = os.Stat(path)
	require.Error(t, err)
}

// TestExportValidatesEverythingRefusable: every refusal names its fix.
func TestExportValidatesEverythingRefusable(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	full := exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false)
	cases := []struct {
		name string
		opts ExportOptions
		want string
	}{
		{"no run ID", func() ExportOptions { o := full; o.RunID = ""; return o }(), "run ID"},
		{"no select run", func() ExportOptions { o := full; o.SelectRunID = ""; return o }(), "Select run"},
		{"no store", func() ExportOptions { o := full; o.Store = nil; return o }(), "store"},
		{"no pool", func() ExportOptions { o := full; o.Pool = nil; return o }(), "pool"},
		{"no destination", func() ExportOptions { o := full; o.Destination = knov1.Destination_DESTINATION_UNSPECIFIED; return o }(), "destination"},
		{"no path", func() ExportOptions { o := full; o.Path = ""; return o }(), "output path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.opts.Export(context.Background())
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestExportRefusesWithoutPortfolio: exporting a run that never ran Select
// gets the fix spelled out — and the file must not exist afterwards.
func TestExportRefusesWithoutPortfolio(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	require.NoError(t, st.CreateRun(context.Background(), &knov1.Run{
		Id: "never-selected", Stage: knov1.Stage_STAGE_VALUE,
		Status: knov1.RunStatus_RUN_STATUS_COMPLETED, GoalName: "g",
	}))
	path := filepath.Join(t.TempDir(), "training.jsonl")
	o := exportOpts(st, stubPool{}, knov1.Destination_DESTINATION_TUNING_SET, path, false)
	o.SelectRunID = "never-selected"
	_, err := runExport(t, o)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no Portfolio")
	_, err = os.Stat(path)
	require.Error(t, err, "a refused export must not create files")
}

// TestExportRefusesMissingAsset: a Portfolio entry whose Asset is not in the
// pool would render a hole where a document belongs — refused, and the
// artifact must not exist.
func TestExportRefusesMissingAsset(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	pool = stubPool{assets: pool.assets[:2]} // drop tune-a
	path := filepath.Join(t.TempDir(), "training.jsonl")
	_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not in the pool")
	_, err = os.Stat(path)
	require.Error(t, err)
}

// TestExportEventSequence: RunStarted → ExportWritten → RunFinished, with
// the ExportWritten counts matching the artifact.
func TestExportEventSequence(t *testing.T) {
	t.Parallel()

	rec := &recordingStore{Store: openTestStore(t)}
	_, pool := exportFixture(t, rec)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	res, err := runExport(t, exportOpts(rec, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
	require.NoError(t, err)
	require.Len(t, rec.events, 3)
	require.NotNil(t, rec.events[0].GetRunStarted())
	ew := rec.events[1].GetExportWritten()
	require.NotNil(t, ew)
	require.Equal(t, knov1.Destination_DESTINATION_TUNING_SET, ew.GetDestination())
	require.Equal(t, int32(1), ew.GetAssetCount())
	require.Equal(t, res.BytesWritten, ew.GetBytesWritten())
	require.Equal(t, path, ew.GetPath())
	require.NotNil(t, rec.events[2].GetRunFinished())
}

// TestExportRunRecord: the Export run is recorded with its stage and the
// source run's goal — a STAGE_EXPORT run a reader can correlate.
func TestExportRunRecord(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
	require.NoError(t, err)
	run, err := st.GetRun(context.Background(), "exp-1")
	require.NoError(t, err)
	require.Equal(t, knov1.Stage_STAGE_EXPORT, run.GetStage())
	require.Equal(t, knov1.RunStatus_RUN_STATUS_COMPLETED, run.GetStatus())
	require.Equal(t, "test-goal", run.GetGoalName())
}

// TestExportEmptySelection: exporting a Portfolio that selected nothing for
// the Destination writes an empty artifact with a manifest — a legal,
// complete answer.
func TestExportEmptySelection(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	// The fixture's Portfolio selects one Asset per Destination; remove the
	// context entry so CONTEXT has nothing to render.
	p, err := st.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	p.Selected = []*PortfolioEntry{p.GetSelected()[1], p.GetSelected()[2]}
	require.NoError(t, st.WritePortfolio(context.Background(), "sel-1", p))
	path := filepath.Join(t.TempDir(), "context.txt")
	res, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_CONTEXT, path, false))
	require.NoError(t, err)
	require.Equal(t, 0, res.AssetCount)
	require.Equal(t, "", readFile(t, path))
	require.Contains(t, readFile(t, path+".manifest.md"), "- Assets: 0")
}

// CheckResumableForTest exposes checkResumable, which is the gate M2-10e arms
// and which no exported surface reaches.
func CheckResumableForTest(o BaselineOptions, run *knov1.Run) error {
	return o.checkResumable(run)
}

// ModelGateForTest exposes the resolved-model gate, which nothing exported
// reaches. The end-to-end path is covered by driving a real run; this is for
// the membership table, where a fixture Run would add nothing.
func ModelGateForTest(recorded ...string) func(now string) error {
	g := newModelGate(&knov1.Run{
		CaseExecution: &knov1.CaseExecution{ResolvedModels: recorded},
	})
	return g.check
}

var _ = proto.Marshal

// TestExportErrorPaths: every store failure surfaces as an error naming the
// operation — the run shape never swallows a failed read or write, and a
// failure before the write leaves no artifact behind.
func TestExportErrorPaths(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"GetRun", "CreateRun", "AppendEvent", "FinishRun", "Portfolio"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			st := &failStore{Store: openTestStore(t)}
			_, pool := exportFixture(t, st)
			st.fail = func(m string) error {
				if m == method {
					return errors.New("boom")
				}
				return nil
			}
			path := filepath.Join(t.TempDir(), "training.jsonl")
			_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
			require.Error(t, err)
			require.Contains(t, err.Error(), "boom")
			if method != "FinishRun" {
				_, statErr := os.Stat(path)
				require.Error(t, statErr, "a failed export must not leave an artifact")
			}
		})
	}
}

// TestExportEmitterRefusesAfterClose: appending to an emitter that already
// emitted RunFinished is refused — the sequence is one run, closed once.
func TestExportEmitterRefusesAfterClose(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	em := &exportEmitter{closed: true}
	err := ExportOptions{RunID: "x", Store: st}.
		append(context.Background(), em, func() *knov1.Event {
			return &knov1.Event{Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{Stage: knov1.Stage_STAGE_EXPORT}}}
		}, "run-started")
	require.Error(t, err)
	require.Contains(t, err.Error(), "RunFinished")
}

// TestExportAppendFails: an AppendEvent failure surfaces from the append,
// named by the event kind.
func TestExportAppendFails(t *testing.T) {
	t.Parallel()

	st := &failStore{Store: openTestStore(t), fail: func(string) error { return errors.New("boom") }}
	em := &exportEmitter{}
	err := ExportOptions{RunID: "x", Store: st}.
		append(context.Background(), em, func() *knov1.Event {
			return &knov1.Event{Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{Stage: knov1.Stage_STAGE_EXPORT}}}
		}, "run-started")
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// TestExportContextPackMultiEntry: more than one document is separated by a
// blank line — the pack's document boundary.
func TestExportContextPackMultiEntry(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	p, err := st.Portfolio(context.Background(), "sel-1")
	require.NoError(t, err)
	p.Selected = append(p.Selected, &PortfolioEntry{
		AssetId: "ctx-b", Destination: knov1.Destination_DESTINATION_CONTEXT, Rank: 4,
		Valuation: &Valuation{AssetId: "ctx-b", DeltaGoal: 0.2, Kind: knov1.Kind_KIND_KNOWLEDGE},
	})
	require.NoError(t, st.WritePortfolio(context.Background(), "sel-1", p))
	pool.assets = append(pool.assets, &Asset{
		Id: "ctx-b", Content: []byte("Second document."), Title: "Second", Kind: knov1.Kind_KIND_KNOWLEDGE,
	})
	path := filepath.Join(t.TempDir(), "context.txt")
	res, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_CONTEXT, path, false))
	require.NoError(t, err)
	require.Equal(t, 2, res.AssetCount)
	require.Contains(t, readFile(t, path), "\n\n")
}

// TestExportKnowledgeBaseTitleFallback: an Asset without a title is named by
// its ID in the add-list.
func TestExportKnowledgeBaseTitleFallback(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	pool.assets[1].Title = "" // kb-a
	path := filepath.Join(t.TempDir(), "kb.md")
	_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_KNOWLEDGE_BASE, path, false))
	require.NoError(t, err)
	require.Contains(t, readFile(t, path), "## 1. kb-a")
}

// TestExportContentOfPanicsOnMissingAsset: rendering past the missing-asset
// check is programmer error and panics — the check runs first, always.
func TestExportContentOfPanicsOnMissingAsset(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		contentOf(map[string]*Asset{}, &PortfolioEntry{AssetId: "ghost"})
	})
}

// TestExportRefuseExistingTargetStatError: a target whose parent is not a
// directory cannot be checked — the refusal names the check, not a pass.
func TestExportRefuseExistingTargetStatError(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	err := refuseExistingTarget(filepath.Join(file, "child"), false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checking")
}

// TestWriteAtomicRefusesDirectoryTarget: renaming over an existing directory
// fails, and the temp file is cleaned up — nothing partial is left behind.
func TestWriteAtomicRefusesDirectoryTarget(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "target-dir")
	require.NoError(t, os.Mkdir(dir, 0o755))
	err := writeAtomic(dir, []byte("data"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "moving")
	entries, err := os.ReadDir(filepath.Dir(dir))
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".kno-export-", "the temp file is cleaned up")
	}
}

// TestExportPoolOpenError: a pool that fails to open aborts before anything
// is written — the artifact cannot be rendered from a pool that did not load.
func TestExportPoolOpenError(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, _ = exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	o := exportOpts(st, failPool{openErr: errors.New("boom")}, knov1.Destination_DESTINATION_TUNING_SET, path, false)
	_, err := runExport(t, o)
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
	_, statErr := os.Stat(path)
	require.Error(t, statErr)
}

// TestExportManifestRefusesDirectoryTarget: with --force, a manifest path
// that is an existing directory cannot be replaced — the rename fails and
// the run reports it. The pair (artifact + manifest) stays consistent: the
// artifact exists only because the manifest write is the failing half.
func TestExportManifestRefusesDirectoryTarget(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, pool := exportFixture(t, st)
	path := filepath.Join(t.TempDir(), "training.jsonl")
	manifestDir := path + ".manifest.md"
	require.NoError(t, os.Mkdir(manifestDir, 0o755))
	_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, true))
	require.Error(t, err)
	require.Contains(t, err.Error(), "moving")
}

// TestExportAppendFailsMidRun: an AppendEvent failure after the run started
// surfaces with the event named — the run cannot finish with a missing
// export-written or run-finished record.
func TestExportAppendFailsMidRun(t *testing.T) {
	t.Parallel()

	for _, failOn := range []int{2, 3} {
		failOn := failOn
		t.Run(fmt.Sprintf("event-%d", failOn), func(t *testing.T) {
			t.Parallel()
			n := 0
			st := &failStore{Store: openTestStore(t), fail: func(m string) error {
				if m == "AppendEvent" {
					n++
					if n == failOn {
						return errors.New("boom")
					}
				}
				return nil
			}}
			_, pool := exportFixture(t, st)
			path := filepath.Join(t.TempDir(), "training.jsonl")
			_, err := runExport(t, exportOpts(st, pool, knov1.Destination_DESTINATION_TUNING_SET, path, false))
			require.Error(t, err)
			require.Contains(t, err.Error(), "boom")
		})
	}
}
