package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// baselineFlags are the options `kno baseline` accepts.
//
// Every one mirrors a field the engine already has, and nothing is computed
// here that the engine could compute itself — the CLI is a shell over identical
// engine calls, per DESIGN.md's open-core seam.
type baselineFlags struct {
	evalsPath   string
	agentRef    string
	goalName    string
	dbPath      string
	runID       string
	concurrency int
	holdoutFrac float64
	splitSeed   string
	maxCostUSD  float64
	maxCalls    int64
	costPerCall float64
	resume      bool
	jsonOut     bool
	yes         bool
}

func newBaselineCmd() *cobra.Command {
	var f baselineFlags

	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "Run your agent over your evals and score it",
		Long: `Run the agent over the dev half of your evals, score each answer against
the goal, and record every result.

This is the reference every later measurement is compared against. The holdout
half is never read here — it stays untouched until validate, because selecting
against it would inflate every number that follows.

Interrupting is safe: work is checkpointed as it completes, and --resume
continues without paying for anything twice.`,
		Example: `  # Score a fake agent against an eval set, no provider needed
  kno baseline --evals cases.jsonl --agent fake:

  # Cap spend, and continue an interrupted run
  kno baseline --evals cases.jsonl --agent fake: --max-cost-usd 5.00
  kno baseline --evals cases.jsonl --agent fake: --resume`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		// Errors are rendered by the top-level runner in the CLI's grammar, so
		// cobra must not also print its own.
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBaseline(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.evalsPath, "evals", "", "path to a JSONL file of eval cases (required)")
	flags.StringVar(&f.agentRef, "agent", "fake:", "agent to measure")
	flags.StringVar(&f.goalName, "goal", "exact-match", "goal to score against")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.StringVar(&f.runID, "run-id", "", "identifier for this run (generated if empty)")
	flags.IntVar(&f.concurrency, "concurrency", 0, "in-flight cases (0 picks a conservative default)")
	flags.Float64Var(&f.holdoutFrac, "holdout-frac", jsonl.DefaultHoldoutFrac, "share of cases held back for validate")
	flags.StringVar(&f.splitSeed, "split-seed", "", "deliberately re-split the evals (changes which cases are held back)")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0, "stop before spending more than this (0 is unlimited)")
	flags.Int64Var(&f.maxCalls, "max-calls", 0, "stop after this many agent calls (0 is unlimited)")
	flags.Float64Var(&f.costPerCall, "cost-per-call-usd", 0, "expected cost of one agent call, required with --max-cost-usd")
	flags.BoolVar(&f.resume, "resume", false, "continue an interrupted run instead of starting one")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	flags.BoolVar(&f.yes, "yes", false, "skip the spend confirmation")

	if err := cmd.MarkFlagRequired("evals"); err != nil {
		panic(fmt.Sprintf("cli: marking --evals required: %v", err))
	}
	return cmd
}

// usdToMicros converts dollars to the int64 micro-USD the engine uses.
//
// The conversion happens once, at the edge. Everything inward is integer, so
// float representation error cannot accumulate across a run — which matters
// because refusing to spend is decided on these numbers.
func usdToMicros(usd float64) int64 {
	return int64(usd*1_000_000 + 0.5)
}

func runBaseline(ctx context.Context, out io.Writer, f baselineFlags) error {
	evals, err := jsonl.New(jsonl.Options{
		Path:        f.evalsPath,
		HoldoutFrac: f.holdoutFrac,
		SplitSeed:   f.splitSeed,
	})
	if err != nil {
		return errs.ErrInvalidInput.WithFix(
			"check --evals and --holdout-frac").Wrap(err)
	}

	// Count the split before anything is spent. A run that can never produce a
	// holdout number is refused here rather than discovered at validate, after
	// the money is gone.
	counts, err := evals.CountSplits(ctx)
	if err != nil {
		return errs.ErrInvalidInput.WithFix(
			"fix the reported line, then re-run").Wrap(err)
	}
	if err := counts.Validate(); err != nil {
		return errs.ErrInvalidInput.WithFix(
			"add more cases, or lower --holdout-frac").Wrap(err)
	}

	agent, agentRef, err := resolveAgent(f.agentRef)
	if err != nil {
		return err
	}
	goal, err := resolveGoal(f.goalName)
	if err != nil {
		return err
	}

	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is writable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	fingerprint, err := evals.ContentHash(ctx)
	if err != nil {
		return errs.ErrInvalidInput.Wrap(err)
	}

	guard := budget.New(
		budget.Limits{
			MaxCostUSDMicros: usdToMicros(f.maxCostUSD),
			MaxLLMCalls:      f.maxCalls,
		},
		confirmFunc(out, f.yes, f.jsonOut),
		usdToMicros(confirmThresholdUSD),
	)

	runID := f.runID
	if runID == "" {
		runID = newRunID(time.Now())
	}

	opts := core.BaselineOptions{
		RunID:                   runID,
		Agent:                   agent,
		AgentRef:                agentRef,
		Goal:                    goal,
		GoalName:                f.goalName,
		Guard:                   guard,
		Store:                   db,
		Concurrency:             f.concurrency,
		Resume:                  f.resume,
		InputFingerprint:        fingerprint,
		EvalContentHash:         fingerprint,
		SplitSeed:               f.splitSeed,
		HoldoutFrac:             f.holdoutFrac,
		DevCases:                counts.Dev,
		HoldoutCases:            counts.Holdout,
		HoldoutUnderpowered:     counts.Underpowered(),
		EstCostPerCallUSDMicros: usdToMicros(f.costPerCall),
	}

	res, runErr := core.Baseline(ctx, core.Seal(evals), opts)
	if res == nil {
		return runErr
	}

	if err := render(out, f, res, counts, runID); err != nil {
		return err
	}
	// The run's own error is returned AFTER rendering, so a budget stop or an
	// interruption still shows what it accomplished before exiting non-zero.
	return runErr
}
