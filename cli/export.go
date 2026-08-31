package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// exportFlags are the options `kno export` accepts.
type exportFlags struct {
	dbPath      string
	runID       string
	selectRunID string
	poolPath    string
	outPath     string

	// Destination is the grammar to render into: context, knowledge_base,
	// or tuning_set.
	destination string

	// Force replaces an existing file at --out. Refused without it: an
	// overwritten export is a silent mutation, and this stage's contract is
	// that nothing is silently mutated.
	force bool

	jsonOut bool
}

func newExportCmd() *cobra.Command {
	var f exportFlags

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write a portfolio's selected assets to a destination",
		Long: `Render the selected assets of a recorded Portfolio into one destination's
grammar: context (a context-pack manifest plus the rendered pack),
knowledge_base (a manifest plus a human-readable instruction list; writable
knowledge-base adapters arrive with v0.2), or tuning_set (OpenAI chat format
JSONL, the shape the Tuner adapters will parse).

The artifact is a pure function of the Portfolio and the pool: re-exporting
the same Portfolio is byte-identical, and export never mutates a destination.
An existing target file is refused unless --force; writes are temp-then-rename.

Export makes no LLM calls and runs no budget guard, so it reports no spend:
no spent_usd key in --json, and "guarded": false says so positively. What the
exported assets cost to measure belongs to the Value run behind the
Portfolio; kno report --value-run-id <id> sums the pipeline.`,
		Example: `  # Write the tuning set
  kno export --select-run-id <id> --pool assets.jsonl --destination tuning_set --out tuning.jsonl

  # Replace an earlier export, deliberately
  kno export --select-run-id <id> --pool assets.jsonl --destination context --out pack.md --force`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExport(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.selectRunID, "select-run-id", "",
		"run ID of the recorded Portfolio to export (required; run `kno select` first)")
	flags.StringVar(&f.destination, "destination", "",
		"grammar to render into: context, knowledge_base, or tuning_set (required)")
	flags.StringVar(&f.poolPath, "pool", "",
		"assets whose content the artifact renders: a JSONL file path, csv:<file>, or md:<file-or-dir> (required)")
	flags.StringVar(&f.outPath, "out", "",
		"path the artifact is written to; the manifest is written beside it at <out>.manifest.md (required)")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.StringVar(&f.runID, "run-id", "", "identifier for this run (generated if empty)")
	flags.BoolVar(&f.force, "force", false, "replace an existing file at --out")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	if err := cmd.MarkFlagRequired("select-run-id"); err != nil {
		panic(fmt.Sprintf("cli: marking --select-run-id required: %v", err))
	}
	if err := cmd.MarkFlagRequired("destination"); err != nil {
		panic(fmt.Sprintf("cli: marking --destination required: %v", err))
	}
	if err := cmd.MarkFlagRequired("pool"); err != nil {
		panic(fmt.Sprintf("cli: marking --pool required: %v", err))
	}
	if err := cmd.MarkFlagRequired("out"); err != nil {
		panic(fmt.Sprintf("cli: marking --out required: %v", err))
	}
	return cmd
}

// runExport executes the stage and renders the report.
func runExport(ctx context.Context, out io.Writer, f exportFlags) error {
	dest, err := parseDestination(f.destination)
	if err != nil {
		return err
	}
	pool, err := resolvePool(f.poolPath, false)
	if err != nil {
		return err
	}

	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is writable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	runID := f.runID
	if runID == "" {
		runID = newRunID(time.Now())
	}

	opts := core.ExportOptions{
		RunID:       runID,
		SelectRunID: f.selectRunID,
		Store:       db,
		Pool:        pool,
		Destination: dest,
		Path:        f.outPath,
		Force:       f.force,
	}

	res, runErr := opts.Export(ctx)
	if res == nil {
		return runErr
	}
	renderErr := renderExport(out, f.jsonOut, res)
	if runErr != nil {
		return runErr
	}
	return renderErr
}

// parseDestination resolves the --destination grammar, refusing anything
// outside the three the design ships.
func parseDestination(s string) (knov1.Destination, error) {
	switch s {
	case "context":
		return knov1.Destination_DESTINATION_CONTEXT, nil
	case "knowledge_base":
		return knov1.Destination_DESTINATION_KNOWLEDGE_BASE, nil
	case "tuning_set":
		return knov1.Destination_DESTINATION_TUNING_SET, nil
	default:
		return knov1.Destination_DESTINATION_UNSPECIFIED, errs.ErrInvalidInput.
			WithFix("pass one of context, knowledge_base, tuning_set").
			Wrap(fmt.Errorf("--destination %q is not a destination grammar", s))
	}
}
