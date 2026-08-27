package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	knov1 "github.com/knograph/kno/gen/kno/v1"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/store"
)

// purgeFlags are the options `kno purge` accepts.
type purgeFlags struct {
	runID  string
	dbPath string
	yes    bool
	force  bool
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
	flags.BoolVar(&f.force, "force", false, "purge even a run that is still executing")

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

	// A run still executing keeps writing traces after the purge, so the
	// command would report success over content that reappears seconds later.
	if run.GetStatus() == knov1.RunStatus_RUN_STATUS_RUNNING && !f.force {
		return errs.ErrInvalidInput.
			WithFix("wait for the run to finish, then purge; or pass --force if you know it is not running").
			Wrap(fmt.Errorf("run %s is still running, and Cases completing after a purge would write new traces", f.runID))
	}

	pending, err := db.PurgeableCount(ctx, f.runID)
	if err != nil {
		return fmt.Errorf("counting purgeable outcomes for %s: %w", f.runID, err)
	}

	if !f.yes {
		// Irreversible and not a spend, so it gets its own confirmation rather
		// than reusing the budget guard's.
		_, err := fmt.Fprintf(out,
			// "recorded row(s)", not "outcome(s)". The count spans outcomes and
			// measurements, and for a Value run it is entirely measurements —
			// so the old noun told a user "44 outcome(s)" about a run with no
			// outcomes at all, on the one prompt whose job is stating what is
			// about to be destroyed.
			"\nPurge would remove agent output and judge rationales from %d recorded row(s) "+
				"in run %s (%s, %s).\n"+
				"Scores, costs, and completion records are kept, so the run stays resumable.\n"+
				"This cannot be undone. Re-run with --yes to proceed.\n",
			pending, run.GetId(), run.GetGoalName(), statusName(run.GetStatus()))
		if err != nil {
			return fmt.Errorf("writing confirmation: %w", err)
		}
		// Exit non-zero. A retention job that forgets --yes would otherwise
		// succeed, log success, and delete nothing — the same silent no-op the
		// unknown-run check above exists to prevent.
		return errs.ErrConfirmationRequired.WithFix(
			"re-run with --yes to purge",
		).
			Wrap(fmt.Errorf("purge of run %s was not confirmed; nothing was removed", f.runID))
	}

	purged, err := db.Purge(ctx, f.runID)
	if err != nil {
		return fmt.Errorf("purging run %s: %w", f.runID, err)
	}

	if _, err := fmt.Fprintf(out,
		"\nPurged %d recorded row(s) for run %s.\nThe run is still resumable: %s\n",
		purged, run.GetId(), "completion records, costs, and scores were kept."); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}
