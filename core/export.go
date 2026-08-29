package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// ExportOptions configure the Export stage.
//
// Export renders the selected Assets of a Select run's Portfolio into the
// Destination's grammar, at a local path. It never touches the Destination
// itself — the adapters that write a knowledge base are the v0.2 half the
// manifest names — and it never mutates the Portfolio or any pool.
type ExportOptions struct {
	// RunID identifies this Export run.
	RunID string

	// SelectRunID is the run whose Portfolio is exported.
	SelectRunID string

	// Store is where the Portfolio and the Run records live.
	Store store.Store

	// Pool supplies Asset content. Required: the Portfolio carries
	// measurements, not content, and the pool is the only place content
	// lives — so an Export without one cannot render anything.
	Pool Pool

	// Destination is the grammar to render into. One of context,
	// knowledge_base, or tuning_set; UNSPECIFIED is refused.
	Destination knov1.Destination

	// Path is where the rendered artifact is written. The manifest is
	// written beside it at Path + ".manifest.md".
	Path string

	// Force replaces an existing file at Path. Refused without it: an
	// overwritten export is a silent mutation, and this stage's contract is
	// that nothing is silently mutated.
	Force bool
}

// ExportResult is what an Export run produced.
type ExportResult struct {
	// RunID identifies the run.
	RunID string

	// Destination rendered.
	Destination knov1.Destination

	// AssetCount is how many selected entries the artifact holds.
	AssetCount int

	// BytesWritten is the artifact and manifest bytes on disk.
	BytesWritten int64

	// Path is where the artifact was written.
	Path string
}

// Export executes the stage: load the Portfolio, render the selected Assets
// of one Destination into its grammar, and write atomically.
//
// The output is a pure function of the Portfolio and the pool, so a
// re-export is byte-identical (goldens pin it) and Export can never make a
// Portfolio lie: the artifact is derived, the record is the source.
func (o ExportOptions) Export(ctx context.Context) (*ExportResult, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	// The overwrite refusal fires before the run is created: a refused
	// export must leave nothing behind — not a file, not a dangling run.
	if err := refuseExistingTarget(o.Path, o.Force); err != nil {
		return nil, err
	}
	p, err := o.Store.Portfolio(ctx, o.SelectRunID)
	if err != nil {
		if errors.Is(err, store.ErrPortfolioNotFound) {
			return nil, errs.ErrInvalidInput.
				WithFix("run `kno select` first, then export the run it produced").
				Wrap(fmt.Errorf("run %s recorded no Portfolio", o.SelectRunID))
		}
		return nil, fmt.Errorf("loading the Portfolio for %s: %w", o.SelectRunID, err)
	}
	selectRun, err := o.Store.GetRun(ctx, o.SelectRunID)
	if err != nil {
		return nil, fmt.Errorf("loading the Select run %s: %w", o.SelectRunID, err)
	}

	assets, err := loadAssetsByID(ctx, o.Pool)
	if err != nil {
		return nil, err
	}

	run := &knov1.Run{
		Id:              o.RunID,
		Stage:           knov1.Stage_STAGE_EXPORT,
		CreatedAt:       time.Now().Format(time.RFC3339),
		Status:          knov1.RunStatus_RUN_STATUS_RUNNING,
		Budget:          selectRun.GetBudget(),
		GoalName:        selectRun.GetGoalName(),
		GoalDirection:   selectRun.GetGoalDirection(),
		GoalScoreDomain: selectRun.GetGoalScoreDomain(),
		DevCaseCount:    selectRun.GetDevCaseCount(),
	}
	if err := o.Store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("creating the run: %w", err)
	}
	em := &exportEmitter{}
	if err := o.emitRunStarted(ctx, em); err != nil {
		return nil, err
	}

	entries := destinationEntries(p, o.Destination)
	for _, e := range entries {
		if assets[e.GetAssetId()] == nil {
			return nil, errs.ErrInvalidInput.
				WithFix("check the pool: the artifact cannot omit a selected Asset").
				Wrap(fmt.Errorf("asset %s is in the Portfolio but not in the pool", e.GetAssetId()))
		}
	}
	artifact, manifest := render(p, entries, assets, o.Destination)
	if err := writeAtomic(o.Path, artifact); err != nil {
		return nil, err
	}
	if err := writeAtomic(o.Path+".manifest.md", manifest); err != nil {
		return nil, err
	}
	bytesWritten := int64(len(artifact) + len(manifest))

	if err := o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_ExportWritten{ExportWritten: &knov1.ExportWritten{
				Destination:  o.Destination,
				AssetCount:   int32(len(entries)), //nolint:gosec // bounded by the pool
				BytesWritten: bytesWritten,
				Path:         o.Path,
			}},
		}
	}, "export-written"); err != nil {
		return nil, err
	}

	status := knov1.RunStatus_RUN_STATUS_COMPLETED
	run.Status = status
	run.FinishedAt = proto.String(time.Now().Format(time.RFC3339))
	if err := o.Store.FinishRun(ctx, run); err != nil {
		return nil, fmt.Errorf("closing the run: %w", err)
	}
	if err := o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunFinished{RunFinished: &knov1.RunFinished{
				Status: status,
			}},
		}
	}, "run-finished"); err != nil {
		return nil, err
	}
	return &ExportResult{
		RunID:        o.RunID,
		Destination:  o.Destination,
		AssetCount:   len(entries),
		BytesWritten: bytesWritten,
		Path:         o.Path,
	}, nil
}

// validate refuses what can be refused before anything is read or written.
func (o ExportOptions) validate() error {
	switch {
	case o.RunID == "":
		return errs.ErrInvalidInput.Wrap(errors.New("export: a run ID is required"))
	case o.SelectRunID == "":
		return errs.ErrInvalidInput.
			WithFix("pass --select-run-id, or run `kno select` first").
			Wrap(errors.New("export: a Select run to export is required"))
	case o.Store == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("export: a store is required"))
	case o.Pool == nil:
		return errs.ErrInvalidInput.
			WithFix("pass --pool: the Portfolio carries measurements, not content").
			Wrap(errors.New("export: a pool is required to render Asset content"))
	case o.Destination != knov1.Destination_DESTINATION_CONTEXT &&
		o.Destination != knov1.Destination_DESTINATION_KNOWLEDGE_BASE &&
		o.Destination != knov1.Destination_DESTINATION_TUNING_SET:
		return errs.ErrInvalidInput.
			WithFix("pass --destination context, knowledge_base, or tuning_set").
			Wrap(fmt.Errorf("export: destination %s is not a writable grammar", o.Destination))
	case o.Path == "":
		return errs.ErrInvalidInput.
			WithFix("pass --out with the path to write").
			Wrap(errors.New("export: an output path is required"))
	}
	return nil
}

// destinationEntries returns the selected entries destined for one
// Destination, in selection order — the order rank already pins.
func destinationEntries(p *knov1.Portfolio, dest knov1.Destination) []*PortfolioEntry {
	var out []*PortfolioEntry
	for _, e := range p.GetSelected() {
		if e.GetDestination() == dest {
			out = append(out, e)
		}
	}
	return out
}

// render produces the Destination's artifact and its manifest. Both are
// deterministic: no timestamps, no absolute paths, no ordering that is not
// the Portfolio's own — so a re-export is byte-identical.
func render(
	p *knov1.Portfolio,
	entries []*PortfolioEntry,
	assets map[string]*Asset,
	dest knov1.Destination,
) (artifact []byte, manifest []byte) {
	manifest = renderManifest(p, entries, assets, dest)
	switch dest {
	case knov1.Destination_DESTINATION_TUNING_SET:
		artifact = renderTuningSet(entries, assets)
	case knov1.Destination_DESTINATION_KNOWLEDGE_BASE:
		artifact = renderKnowledgeBase(entries, assets)
	default: // CONTEXT
		artifact = renderContextPack(entries, assets)
	}
	return artifact, manifest
}

// renderContextPack is the context grammar: the selected Assets' content,
// one document after another, exactly as the agent will read it in context.
func renderContextPack(entries []*PortfolioEntry, assets map[string]*Asset) []byte {
	var out []byte
	for i, e := range entries {
		if i > 0 {
			out = append(out, '\n', '\n')
		}
		out = append(out, contentOf(assets, e)...)
	}
	return out
}

// renderKnowledgeBase is the knowledge-base grammar: a human-readable
// instruction list naming every document a writable-KB adapter should index.
//
// The adapters that WRITE a knowledge base are v0.2 (see the plan); what
// ships here is the instruction list the manifest points at, so a team whose
// KB already has an ingest pipeline can consume it directly.
func renderKnowledgeBase(entries []*PortfolioEntry, assets map[string]*Asset) []byte {
	var out []byte
	out = append(out, []byte("# Knowledge base add-list\n\n")...)
	for i, e := range entries {
		a := assets[e.GetAssetId()]
		title := a.GetTitle()
		if title == "" {
			title = e.GetAssetId()
		}
		out = append(out, fmt.Sprintf("## %d. %s\n\n", i+1, title)...)
		out = append(out, fmt.Sprintf("- Asset ID: `%s`\n", e.GetAssetId())...)
		if a.GetProvenance() != nil && a.GetProvenance().GetSource() != "" {
			out = append(out, fmt.Sprintf("- Provenance: `%s`\n", a.GetProvenance().GetSource())...)
		}
		out = append(out, '\n')
		out = append(out, contentOf(assets, e)...)
		out = append(out, '\n', '\n')
	}
	return out
}

// chatExample is the OpenAI chat format's per-line shape. A struct, not a
// map: field order in the JSONL is part of the pinned format.
type chatExample struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// renderTuningSet is the tuning-set grammar: OpenAI chat format JSONL, one
// example per selected Asset, in selection order — the shape DESIGN.md pins
// and the Tuner adapters will parse.
func renderTuningSet(entries []*PortfolioEntry, assets map[string]*Asset) []byte {
	var out []byte
	for _, e := range entries {
		ex := chatExample{Messages: []chatMessage{{
			Role:    "user",
			Content: string(contentOf(assets, e)),
		}}}
		line, err := json.Marshal(ex)
		if err != nil {
			// A string always marshals; this is unreachable.
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// destName is the CLI grammar's spelling of a Destination — the name a user
// typed, not the enum's wire spelling.
func destName(dest knov1.Destination) string {
	switch dest {
	case knov1.Destination_DESTINATION_KNOWLEDGE_BASE:
		return "knowledge_base"
	case knov1.Destination_DESTINATION_TUNING_SET:
		return "tuning_set"
	default:
		return "context"
	}
}

// renderManifest is the export manifest every Destination ships: where the
// artifact came from, and what is in it, asset by asset — the audit trail
// that lets a downstream pipeline trace any row to its origin.
func renderManifest(
	p *knov1.Portfolio,
	entries []*PortfolioEntry,
	assets map[string]*Asset,
	dest knov1.Destination,
) []byte {
	var out []byte
	out = append(out, []byte("# Export manifest\n\n")...)
	out = append(out, fmt.Sprintf("- Select run: `%s`\n", p.GetRunId())...)
	out = append(out, fmt.Sprintf("- Source Value run: `%s`\n", p.GetSourceRunId())...)
	out = append(out, fmt.Sprintf("- Destination: `%s`\n", destName(dest))...)
	out = append(out, fmt.Sprintf("- Assets: %d\n\n", len(entries))...)
	for i, e := range entries {
		a := assets[e.GetAssetId()]
		title := ""
		provenance := ""
		if a != nil {
			title = a.GetTitle()
			if a.GetProvenance() != nil {
				provenance = a.GetProvenance().GetSource()
			}
		}
		out = append(out, fmt.Sprintf("%d. `%s` — %s (provenance: %s)\n",
			i+1, e.GetAssetId(), title, provenance)...)
	}
	return out
}

// contentOf returns an entry's Asset content. Callers guarantee presence
// (checked before rendering); a missing Asset is programmer error.
func contentOf(assets map[string]*Asset, e *PortfolioEntry) []byte {
	a, ok := assets[e.GetAssetId()]
	if !ok || a == nil {
		panic(fmt.Sprintf("export: asset %s is in the Portfolio but not in the pool", e.GetAssetId()))
	}
	return a.GetContent()
}

// refuseExistingTarget is the overwrite policy: an existing target is
// refused unless force was asked for. Both files are checked — a leftover
// manifest is a leftover artifact, and silently replacing it while refusing
// the pack would split the pair.
func refuseExistingTarget(path string, force bool) error {
	if force {
		return nil
	}
	for _, p := range []string{path, path + ".manifest.md"} {
		if _, err := os.Stat(p); err == nil {
			return errs.ErrInvalidInput.
				WithFix("pass --force to replace the existing file").
				Wrap(fmt.Errorf("%s already exists", p))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking %s: %w", p, err)
		}
	}
	return nil
}

// writeAtomic writes data to path via a temp file in the same directory,
// then renames over the target — nothing partial ever exists at path, and a
// crash mid-write leaves the previous file intact.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kno-export-*")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		// The write failed; cleanup is best-effort on a path already doomed.
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("moving %s into place: %w", tmpName, err)
	}
	return nil
}

// exportEmitter serializes event writes; see selectEmitter.
type exportEmitter struct {
	mu     sync.Mutex
	seq    int64
	closed bool
}

func (o ExportOptions) append(ctx context.Context, em *exportEmitter, build func() *knov1.Event, what string) error {
	em.mu.Lock()
	defer em.mu.Unlock()
	if em.closed {
		return fmt.Errorf("appending %s event: the run already emitted RunFinished", what)
	}
	ev := build()
	ev.RunId = o.RunID
	ev.EmittedAt = time.Now().Format(time.RFC3339)
	ev.Sequence = em.next()
	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending %s event: %w", what, err)
	}
	if _, done := ev.GetPayload().(*knov1.Event_RunFinished); done {
		em.closed = true
	}
	return nil
}

func (em *exportEmitter) next() int64 {
	em.seq++
	return em.seq
}

func (o ExportOptions) emitRunStarted(ctx context.Context, em *exportEmitter) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
				Stage: knov1.Stage_STAGE_EXPORT,
			}},
		}
	}, "run-started")
}
