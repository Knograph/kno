package cli

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core/errs"
)

// `kno demo` runs the whole loop — baseline, value, select, export, report —
// against `fake:`, for free, from files it writes itself.
//
// The stage nobody owned: the five commands all ship, and what did not ship
// was the ten minutes before the first one of them runs. The demo seeds the
// scenario, drives the five stages IN PROCESS through the same run functions
// the commands call, and leaves the input files on disk so the next thing the
// user does is edit them.
//
// Two properties are load-bearing and are held by tests rather than by
// convention:
//
//   - It reads no configuration. Not kno.yaml, not KNO_*. `configSpecs` maps
//     KNO_AGENT and kno.yaml's agent: onto --agent, so a demo that resolved
//     configuration the way `baseline` does would run against whatever
//     provider the user last configured, with their key, and bill them for
//     the privilege of being shown around. The flag structs below are
//     literals for exactly that reason.
//   - Its numbers are unimpressive on purpose. `fake:` answers every Case
//     with what the Case expects, and injection delegates to it unchanged, so
//     the score is 1.000, every delta is +0.0000, and the Portfolio is empty.
//     The epilogue says so in three sentences that are not optional in either
//     rendering. See cli/demodata/README.md.
//
// The cost of literal flag structs is that they take Go's ZERO value, not each
// flag's registered cobra default — a silent divergence the compiler cannot
// see. cli/demo_defaults_test.go is what converts that silence into a failing
// build; read its doc comment before deleting it.

//go:embed demodata/cases.jsonl
var demoCasesJSONL []byte

//go:embed demodata/pool.jsonl
var demoPoolJSONL []byte

// The demo's fixed names. The run IDs are fixed so the epilogue and the docs
// can name them and so the golden transcript is stable; the consequence is
// that a second run into the same directory is refused rather than merged.
const (
	demoDefaultDir = "kno-demo"

	demoCasesName     = "cases.jsonl"
	demoPoolName      = "pool.jsonl"
	demoTuningName    = "tuning.jsonl"
	demoManifestName  = demoTuningName + ".manifest.md"
	demoDBName        = "kno.db"
	demoMarkerName    = ".kno-demo"
	demoGitignoreName = ".gitignore"

	demoBaselineRunID = "demo-baseline"
	demoValueRunID    = "demo-value"
	demoSelectRunID   = "demo-select"
	demoExportRunID   = "demo-export"
)

// demoOwnedNames is what --force may delete, and nothing else.
//
// The .kno-demo marker proves the demo CREATED the directory. That is a weaker
// claim than owning everything currently in it — and the demo directory is
// precisely a place we invite people to work in, so a note, a scratch script
// or an edited copy of a Case must survive. Anything not on this list is left
// in place and named before the run starts.
var demoOwnedNames = []string{
	demoCasesName,
	demoPoolName,
	demoTuningName,
	demoManifestName,
	demoDBName,
	demoDBName + "-shm",
	demoDBName + "-wal",
	demoGitignoreName,
	demoMarkerName,
}

// The three honesty sentences.
//
// They are the whole design of the epilogue: a score of 1.000 with no caveat
// is a number that promises something the product cannot keep on the user's
// own data. Both renderings carry them — the human epilogue wraps these exact
// strings, and --json puts them in `notes` — and a golden pins that the two
// stay in step.
const (
	demoNoteScore = "score 1.000 — `fake:` answers every Case with what the Case expects, " +
		"by construction. The run happened, was scored against a declared Goal, cost nothing, " +
		"and sealed a holdout; nobody's agent is perfect."

	demoNoteDeltas = "deltas +0.0000 — injecting an Asset cannot change a deterministic answer, " +
		"so no Asset measured any effect. The intervals are real; the effects are zero."

	demoNotePortfolio = "portfolio empty — every corrected interval crosses zero, so nothing " +
		"earned its place. \"Include nothing new\" is a legal, first-class outcome, and the " +
		"rejection log says why for each Asset."
)

// demoNotes is the honesty epilogue, in the order both renderings print it.
func demoNotes() []string {
	return []string{demoNoteScore, demoNoteDeltas, demoNotePortfolio}
}

// demoConfigLine is the one sentence about configuration, in both renderings.
const demoConfigLine = "kno.yaml and KNO_* were not read: the demo is pinned to `fake:` " +
	"so it cannot bill anyone."

// demoFlags are the options `kno demo` accepts. The surface is deliberately
// three flags: --agent would make it a run rather than a demo, --yes would be
// a bypass flag on a free path waiting to be copied onto a paid one, and
// --config would be the configuration resolution this command exists without.
type demoFlags struct {
	dir     string
	force   bool
	jsonOut bool
}

func newDemoCmd() *cobra.Command {
	var f demoFlags

	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Run the whole loop against `fake:`, for free, on data it writes for you",
		Long: `Write a small eval set and asset pool into ./kno-demo, then run all five
stages over them — baseline, value, select, export, report — against the
built-in ` + "`fake:`" + ` agent.

It spends nothing, sends nothing anywhere, and reads no configuration: not
kno.yaml, not KNO_*. The agent is pinned to ` + "`fake:`" + `, so the demo cannot bill
anyone whatever your environment says.

The numbers are honest rather than flattering. ` + "`fake:`" + ` answers every Case with
what the Case expects, so the score reads 1.000 and every asset's delta is
zero — with real intervals around it, which is what lets select say "no
effect" rather than "underpowered". The empty portfolio is the tool doing its
job.

The files stay on disk afterwards, because the next thing worth doing is
editing them. Remove them with ` + "`rm -rf kno-demo`" + `.`,
		Example: `  # The whole loop, for free
  kno demo

  # Somewhere else, and again over a previous run
  kno demo --dir /tmp/kno-demo
  kno demo --force

  # For a script
  kno demo --json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDemo(cmd.Context(), cmd.InOrStdin(),
				cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.dir, "dir", demoDefaultDir,
		"directory the demo writes its files and its database into")
	flags.BoolVar(&f.force, "force", false,
		"replace the demo's own files in a directory a previous demo created")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	// No --config, and no addConfigFlag: this command resolves no
	// configuration at all. See the file comment.
	return cmd
}

// demoPaths are the paths the five stages are wired with.
//
// Relative as the user gave them, so what the stages print is what the user
// can paste back. The absolute form is computed only for the safety refusals,
// where the question is which directory this really is.
type demoPaths struct {
	dir     string
	cases   string
	pool    string
	tuning  string
	db      string
	jsonOut bool
}

func demoPathsFor(dir string, jsonOut bool) demoPaths {
	return demoPaths{
		dir:     dir,
		cases:   filepath.Join(dir, demoCasesName),
		pool:    filepath.Join(dir, demoPoolName),
		tuning:  filepath.Join(dir, demoTuningName),
		db:      filepath.Join(dir, demoDBName),
		jsonOut: jsonOut,
	}
}

// The five flag structs, written as literals.
//
// Every field whose flag carries a NON-ZERO registered default is set here by
// hand, because a keyed composite literal that omits one takes Go's zero value
// instead — silently, with no compiler signal. The parity test in
// cli/demo_defaults_test.go is what keeps this list honest as flags are added.
//
// The two structs legitimately differ on --temperature: `baseline` registers
// math.NaN() ("leave the provider default alone") and `value` registers 0.
// Copying one into the other would be the exact divergence the test exists to
// catch, in the other direction.

func demoBaselineFlags(p demoPaths) baselineFlags {
	return baselineFlags{
		evalsPath: p.cases,
		dbPath:    p.db,
		runID:     demoBaselineRunID,
		jsonOut:   p.jsonOut,

		// Registered defaults, hand-copied.
		agentRef: "fake:",
		goalName: "exact-match",
		// Without this the recorded Run.HoldoutFrac would be 0. The split
		// itself survives — jsonl.Options.holdoutFrac maps <= 0 back to
		// DefaultHoldoutFrac — so the bug this guards is a recorded value
		// that contradicts the run, which is exactly the kind that is found
		// late and believed in the meantime.
		holdoutFrac: jsonl.DefaultHoldoutFrac,
		temperature: math.NaN(),

		// yes is false, deliberately and permanently. Nothing prompts here —
		// fake: is not a core.Estimator and Spends() is false, so the quote
		// is $0.00 and consentDialog returns before the TTY check matters —
		// but a bypass flag sitting in a demo is one copy-paste away from a
		// paid path. The guard is exercised, not bypassed.
	}
}

func demoValueFlags(p demoPaths) valueFlags {
	return valueFlags{
		baselineFlags: baselineFlags{
			evalsPath: p.cases,
			dbPath:    p.db,
			runID:     demoValueRunID,
			jsonOut:   p.jsonOut,

			agentRef: "fake:",
			goalName: "exact-match",
			// No holdoutFrac: `value` registers no --holdout-frac, and
			// runValue passes 0 to jsonl.New itself. Setting it here would
			// look like parity with baseline and mean nothing.
			// temperature stays 0: that is what `value` registers.
		},
		poolPath:      p.pool,
		baselineRunID: demoBaselineRunID,

		// Registered default, hand-copied: a zero here would be a different
		// routing draw from the one every other `kno value` performs.
		routingSeed: 1,
	}
}

func demoSelectFlags(p demoPaths) selectFlags {
	return selectFlags{
		valueRunID: demoValueRunID,
		poolPath:   p.pool,
		dbPath:     p.db,
		runID:      demoSelectRunID,
		jsonOut:    p.jsonOut,

		// A budget is required, and these three are the tape's.
		maxContextTokens:    5000,
		maxTrainingExamples: 10,
		maxCostUSD:          1,
	}
}

func demoExportFlags(p demoPaths) exportFlags {
	return exportFlags{
		selectRunID: demoSelectRunID,
		destination: "tuning_set",
		poolPath:    p.pool,
		outPath:     p.tuning,
		dbPath:      p.db,
		runID:       demoExportRunID,
		jsonOut:     p.jsonOut,
	}
}

func demoReportFlags(p demoPaths) reportFlags {
	return reportFlags{
		valueRunID:  demoValueRunID,
		selectRunID: demoSelectRunID,
		dbPath:      p.db,
		jsonOut:     p.jsonOut,
	}
}

// runDemo seeds the scenario and drives the five stages.
func runDemo(ctx context.Context, in io.Reader, out, errOut io.Writer, f demoFlags) error {
	dir, left, err := prepareDemoDir(f)
	if err != nil {
		return err
	}
	p := demoPathsFor(dir, f.jsonOut)

	// Named BEFORE the run, so a user is told rather than surprised. Human
	// output only: in --json mode stdout is a machine contract, and the same
	// names travel in the document's left_in_place.
	if !f.jsonOut {
		for _, name := range left {
			if _, err := fmt.Fprintf(out, "%s left in place\n", filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("writing the demo preamble: %w", err)
			}
		}
	}

	stages, err := runDemoStages(ctx, in, out, errOut, p)
	if err != nil {
		return err
	}

	if f.jsonOut {
		return writeJSON(out, demoReport{
			Dir:         dir,
			Agent:       "fake:",
			Files:       demoFileList(dir),
			LeftInPlace: demoJoinAll(dir, left),
			Stages:      *stages,
			Notes:       demoNotes(),
			Config:      demoConfigLine,
			NextSteps:   demoNextSteps(p),
			Cleanup:     "rm -rf " + dir,
		})
	}
	return writeDemoEpilogue(out, p)
}

// runDemoStages runs the five stages in order, capturing each stage's own
// --json document when the demo is in --json mode.
//
// The context is checked between stages: a Ctrl-C that lands in the gap must
// not start the next stage, and the epilogue must not print after it — an
// epilogue after a failure would claim a completed loop.
func runDemoStages(
	ctx context.Context,
	in io.Reader,
	out, errOut io.Writer,
	p demoPaths,
) (*demoStages, error) {
	var stages demoStages
	steps := []struct {
		name string
		into *demoStageDoc
		run  func(w io.Writer) error
	}{
		{"baseline", &stages.Baseline, func(w io.Writer) error {
			return runBaseline(ctx, in, w, errOut, demoBaselineFlags(p))
		}},
		{"value", &stages.Value, func(w io.Writer) error {
			return runValue(ctx, in, w, errOut, demoValueFlags(p))
		}},
		{"select", &stages.Select, func(w io.Writer) error {
			return runSelect(ctx, w, demoSelectFlags(p))
		}},
		{"export", &stages.Export, func(w io.Writer) error {
			return runExport(ctx, w, demoExportFlags(p))
		}},
		{"report", &stages.Report, func(w io.Writer) error {
			return runReport(ctx, w, demoReportFlags(p))
		}},
	}

	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, errs.ErrInterrupted.Wrap(
				fmt.Errorf("the demo was interrupted before %s; %s still holds what ran",
					step.name, p.dir),
			)
		}
		w := out
		var buf bytes.Buffer
		if p.jsonOut {
			w = &buf
		}
		if err := step.run(w); err != nil {
			return nil, err
		}
		if p.jsonOut {
			doc, err := demoStageDocument(step.name, buf.Bytes())
			if err != nil {
				return nil, err
			}
			*step.into = doc
		}
	}
	return &stages, nil
}

// prepareDemoDir applies the refusals, clears what --force may clear, and
// writes the marker, the .gitignore and the two fixtures.
//
// It returns the directory as the user named it and the names of any files the
// demo found and did not touch.
func prepareDemoDir(f demoFlags) (dir string, left []string, err error) {
	dir = f.dir
	if strings.TrimSpace(dir) == "" {
		dir = demoDefaultDir
	}
	dir = filepath.Clean(dir)

	if err := refuseDemoCwd(dir); err != nil {
		return "", nil, err
	}

	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Fresh.
	case err != nil:
		return "", nil, errs.ErrInvalidInput.
			WithFix("pass a --dir the demo can read, or let it use ./" + demoDefaultDir).
			Wrap(fmt.Errorf("reading %s: %w", dir, err))
	case len(entries) > 0:
		left, err = clearDemoDir(dir, entries, f.force)
		if err != nil {
			return "", nil, err
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", nil, errs.ErrInvalidInput.
			WithFix("pass a --dir on a writable filesystem").
			Wrap(fmt.Errorf("creating %s: %w", dir, err))
	}

	// The marker and the .gitignore first, so a directory that cannot be
	// written to fails here rather than three stages in. The .gitignore is
	// the demo's job and not a repo-config change: a new user runs this
	// inside their own repository, where kno's .gitignore does not reach.
	files := []struct {
		name string
		data []byte
	}{
		{demoMarkerName, []byte(demoMarkerContents)},
		{demoGitignoreName, []byte("*\n")},
		{demoCasesName, demoCasesJSONL},
		{demoPoolName, demoPoolJSONL},
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			return "", nil, errs.ErrInvalidInput.
				WithFix("pass a --dir on a writable filesystem").
				Wrap(fmt.Errorf("writing %s: %w", path, err))
		}
	}
	return dir, left, nil
}

// demoMarkerContents is what proves the demo created a directory.
//
// Prose rather than an empty file, because the one person who will ever read
// it is someone wondering what this directory is and whether --force is safe.
const demoMarkerContents = "Written by `kno demo`. Its presence is what lets `kno demo --force` " +
	"replace the demo's own files here.\nIt does not make the demo the owner of anything else " +
	"in this directory: --force deletes an explicit list of names and leaves everything else " +
	"alone.\nRemove the whole directory with `rm -rf`.\n"

// refuseDemoCwd refuses a --dir that is the directory the user is standing in.
//
// Unconditional, and before anything else. --force deletes files; a --force
// that can reach an arbitrary path because someone typed `--dir .` is the kind
// of bug that ends a project's reputation.
func refuseDemoCwd(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("pass a --dir that resolves").
			Wrap(fmt.Errorf("resolving %s: %w", dir, err))
	}
	cwd, err := os.Getwd()
	if err != nil {
		// Without a working directory there is nothing to compare against,
		// and guessing would be the unsafe direction.
		return errs.ErrInvalidInput.WithFix("run the demo from a directory that exists").
			Wrap(fmt.Errorf("resolving the current directory: %w", err))
	}
	if abs == filepath.Clean(cwd) {
		return errs.ErrInvalidInput.
			WithFix("pass a --dir naming a subdirectory, or omit it for ./" + demoDefaultDir).
			Wrap(fmt.Errorf("--dir %s is the current directory; the demo writes a "+
				"directory of its own so `rm -rf` can remove it cleanly", dir))
	}
	return nil
}

// clearDemoDir applies the --force refusals and removes the demo-owned names.
//
// Two gates, and the second is the one the first draft of this design missed.
// The marker proves the demo CREATED the directory; it does not prove the demo
// owns everything now in it. So only demoOwnedNames are removed, and every
// surviving name is returned to be reported.
func clearDemoDir(dir string, entries []os.DirEntry, force bool) ([]string, error) {
	marker := false
	for _, e := range entries {
		if e.Name() == demoMarkerName {
			marker = true
			break
		}
	}
	if !marker {
		return nil, errs.ErrInvalidInput.
			WithFix("pass --dir naming an empty or new directory; --force only replaces a " +
				"directory a previous demo created").
			Wrap(fmt.Errorf("%s is not empty and has no %s marker, so the demo did not "+
				"create it and will not delete anything in it", dir, demoMarkerName))
	}
	if !force {
		return nil, errs.ErrInvalidInput.
			WithFix("re-run with --force to replace it, or --dir to pick another directory").
			Wrap(fmt.Errorf("%s already exists and is not empty; the demo writes a fixed set "+
				"of files and fixed run IDs, so it cannot merge into a previous run", dir))
	}

	owned := make(map[string]bool, len(demoOwnedNames))
	for _, n := range demoOwnedNames {
		owned[n] = true
	}

	// Pre-flight before anything is removed: a directory that will not accept
	// a write cannot accept a deletion either, and aborting here means
	// nothing is partially cleared.
	if err := demoProbeWritable(dir); err != nil {
		return nil, err
	}

	var left []string
	for _, e := range entries {
		name := e.Name()
		if !owned[name] {
			left = append(left, name)
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.RemoveAll(path); err != nil {
			return nil, errs.ErrInvalidInput.
				WithFix("remove it by hand, or pass --dir to pick another directory").
				Wrap(fmt.Errorf("replacing %s: %w", path, err))
		}
	}
	sort.Strings(left)
	return left, nil
}

// demoProbeWritable reports whether the directory will accept a create and a
// delete, which is what removing a file in it needs.
func demoProbeWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".kno-demo-probe-*")
	if err != nil {
		return errs.ErrInvalidInput.
			WithFix("make the directory writable, or pass --dir to pick another one").
			Wrap(fmt.Errorf("%s cannot be written to, so --force cannot replace anything "+
				"in it: %w", dir, err))
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return errs.ErrInvalidInput.
			WithFix("make the directory writable, or pass --dir to pick another one").
			Wrap(fmt.Errorf("closing a probe file in %s: %w", dir, err))
	}
	if err := os.Remove(name); err != nil {
		return errs.ErrInvalidInput.
			WithFix("make the directory writable, or pass --dir to pick another one").
			Wrap(fmt.Errorf("%s does not allow deletion, so --force cannot replace "+
				"anything in it: %w", dir, err))
	}
	return nil
}

// demoFileList is the epilogue's file list, in the order it prints.
func demoFileList(dir string) []string {
	return demoJoinAll(dir, []string{
		demoCasesName, demoPoolName, demoTuningName,
		demoManifestName, demoDBName, demoGitignoreName,
	})
}

func demoJoinAll(dir string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, n))
	}
	return out
}

// demoNextSteps is the "now do it with your data" pair, in both renderings.
func demoNextSteps(p demoPaths) []string {
	return []string{
		"edit " + p.cases + " — one scoreable interaction per line, with a stable id",
		"kno baseline --evals " + p.cases + " --agent openai:gpt-4.1 --max-cost-usd 2.00",
	}
}

// demoEpilogueOpening is the sentinel the golden test splits the transcript on.
const demoEpilogueOpening = "Demo complete — nothing was spent, and nothing was sent anywhere."

// writeDemoEpilogue prints the human epilogue.
//
// Not optional, and printed only after `report` returned: an epilogue after a
// failure would claim a completed loop.
func writeDemoEpilogue(out io.Writer, p demoPaths) error {
	var b strings.Builder

	b.WriteString("\n" + demoEpilogueOpening + "\n\n")

	descriptions := []string{
		"12 eval Cases (8 dev, 4 held back)",
		"3 candidate Assets",
		"the exported tuning set",
		"what the export claims, and from what",
		"every run, score and trace",
		"written by the demo, so this directory stays out of `git status`",
	}
	paths := demoFileList(p.dir)
	width := 0
	for _, path := range paths {
		width = max(width, len(path))
	}
	for i, path := range paths {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, path, descriptions[i])
	}

	b.WriteString("\nWhy the numbers look like this:\n")
	for _, note := range demoNotes() {
		b.WriteString(wrapIndent(note, demoWrapWidth, "  "))
	}

	b.WriteString("\nNow do it with your data:\n")
	for i, step := range demoNextSteps(p) {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
	}

	b.WriteString("\n" + demoConfigLine + "\n")
	fmt.Fprintf(&b, "Remove everything with: rm -rf %s\n", p.dir)

	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("writing the demo epilogue: %w", err)
	}
	return nil
}

// demoWrapWidth is the epilogue's wrap column. Fixed rather than read from the
// terminal, for the reason report_render.go fixes its own: output that changes
// shape with the window is output that cannot be golden-tested.
const demoWrapWidth = 88

// wrapIndent word-wraps s at width, prefixing every line with indent and
// hanging the continuation lines two spaces further.
//
// The wrap is what makes the human/--json equivalence testable: the sentences
// are one string in the source, the JSON prints them whole, and this is the
// only thing that reshapes them.
func wrapIndent(s string, width int, indent string) string {
	hang := indent + "  "
	var b strings.Builder
	line := indent
	prefix := indent
	for _, word := range strings.Fields(s) {
		switch {
		case line == prefix:
			line += word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			b.WriteString(line + "\n")
			prefix = hang
			line = hang + word
		}
	}
	if strings.TrimSpace(line) != "" {
		b.WriteString(line + "\n")
	}
	return b.String()
}
