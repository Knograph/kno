package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/store"
)

// purgeFlags are the options `kno purge` accepts.
type purgeFlags struct {
	runID  string
	dbPath string
	yes    bool
}

func newPurgeCmd() *cobra.Command {
	var f purgeFlags

	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete stored agent output and judge rationales for a run",
		Long: `Remove trace content from a run: the agent's output and the judge's
rationale, which are the parts that can contain end-user conversation content.

What survives, deliberately: which Cases completed, what they cost, whether they
scored, and the score itself. Those are what makes a run resumable and its
numbers readable, and none of them is conversation content.

That distinction is not cosmetic. Kno has no separate "this Case is done"
marker — the recorded outcome IS the marker — so a purge that removed those
rows would make a resumed run pay for every Case a second time.

This cannot be undone.`,
		Example: `  # Purge one run's traces, asking first
  kno purge --run-id 20260821T091515-ffc3097d49da

  # In a scheduled retention job
  kno purge --run-id "nightly-$(date -I -d yesterday)" --yes`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPurge(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.runID, "run-id", "", "the run whose traces to remove (required)")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.BoolVar(&f.yes, "yes", false, "skip the confirmation")

	if err := cmd.MarkFlagRequired("run-id"); err != nil {
		panic(fmt.Sprintf("cli: marking --run-id required: %v", err))
	}
	return cmd
}

func runPurge(ctx context.Context, out io.Writer, f purgeFlags) error {
	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is writable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	// Resolve the run first, so a typo in --run-id is a clear refusal rather
	// than a silent no-op that reads as "already purged".
	run, err := db.GetRun(ctx, f.runID)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			return errs.ErrInvalidInput.
				WithFix("check --run-id and --db; `kno baseline` prints the run id it used").
				Wrap(fmt.Errorf("no run %q in %s", f.runID, f.dbPath))
		}
		return fmt.Errorf("loading run %s: %w", f.runID, err)
	}

	if !f.yes {
		// Irreversible and not a spend, so it gets its own confirmation rather
		// than reusing the budget guard's.
		_, err := fmt.Fprintf(out,
			"\nPurge would remove agent output and judge rationales for run %s (%s, %s).\n"+
				"Scores, costs, and completion records are kept, so the run stays resumable.\n"+
				"This cannot be undone. Re-run with --yes to proceed.\n",
			run.GetId(), run.GetGoalName(), statusName(run.GetStatus()))
		if err != nil {
			return fmt.Errorf("writing confirmation: %w", err)
		}
		return nil
	}

	purged, err := db.Purge(ctx, f.runID)
	if err != nil {
		return fmt.Errorf("purging run %s: %w", f.runID, err)
	}

	if _, err := fmt.Fprintf(out,
		"\nPurged %d outcome(s) for run %s.\nThe run is still resumable: %s\n",
		purged, run.GetId(), "completion records, costs, and scores were kept."); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}
