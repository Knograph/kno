package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// selectFlags are the options `kno select` accepts.
type selectFlags struct {
	dbPath     string
	runID      string
	valueRunID string
	poolPath   string

	// AllowPartial accepts a source Value run that did not complete
	// (BUDGET_STOPPED or INTERRUPTED) and builds from its recorded
	// Valuations. Refused without it: a partial source would rank an
	// incomplete measurement set as if it were the whole answer.
	allowPartial bool

	// Budget caps, all carrying costs of the SELECTED set. Zero is unset —
	// the constraint set is exactly the caps the user names.
	maxContextTokens    int64
	maxTrainingExamples int64
	maxCostUSD          float64

	// Redundancy knobs, all zero by default — no invented number ships in
	// the default path. See core.SelectOptions's own godoc for what each
	// zero means.
	redundancyMargin           float64
	redundancyMaxMargin        float64
	redundancyMinCoImprovement float64

	// explainAssetID is the Asset --explain prints the per-Case redundancy
	// table for, when set. Free, read-only: makes no provider call.
	explainAssetID string

	jsonOut bool
}

func newSelectCmd() *cobra.Command {
	var f selectFlags

	cmd := &cobra.Command{
		Use:   "select",
		Short: "Choose the assets that earn their place, under budget",
		Long: `Rank the recorded Valuations of a Value run on delta per dollar, decide each
asset in precedence order — regression, no effect, redundant, cost-dominated,
wrong mechanism — and record the Portfolio that survives.

The construction is greedy with no approximation guarantee: feasible,
deterministic, and reproducible, and the report says so. Every keep/reject
interval is Bonferroni-corrected for the number of assets screened, and the
portfolio-level gain is one corrected claim, winner's-curse inflation
included. The honest number arrives with validate, against the untouched
holdout.

Select makes no LLM calls, reads no evals, and never touches the holdout:
every decision is a pure function of what the store holds.

Because it runs no budget guard, select reports no spend: there is no
spent_usd key in --json, and "guarded": false says so rather than leaving you
to read a missing key as a zero. The measurement select ranks was paid for by
the Value run named in source_run_id — run kno report --value-run-id <id> for
what the pipeline cost. The two dollar figures select does report are
something else: max_cost_usd is the cap the Portfolio was built under, and
acquisition_usd is the carrying cost of the selected assets.`,
		Example: `  # Choose the portfolio under budget
  kno select --value-run-id <id> --max-context-tokens 10000

  # Enable the content rules (redundancy, wrong mechanism) by naming a pool
  kno select --value-run-id <id> --pool assets.jsonl --max-cost-usd 5`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSelect(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.valueRunID, "value-run-id", "",
		"run ID of the recorded Value run to build on (required; run `kno value` first)")
	flags.StringVar(&f.poolPath, "pool", "",
		"assets to decide: a JSONL file path, csv:<file>, or md:<file-or-dir>; enables the REDUNDANT and WRONG_MECHANISM rules")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.StringVar(&f.runID, "run-id", "", "identifier for this run (generated if empty)")
	flags.BoolVar(&f.allowPartial, "allow-partial", false,
		"build from a Value run that did not complete; the source status travels with the Portfolio")
	flags.Int64Var(&f.maxContextTokens, "max-context-tokens", 0,
		"carrying cap: tokens the selected context may add per call (0 is unset)")
	flags.Int64Var(&f.maxTrainingExamples, "max-training-examples", 0,
		"carrying cap: examples the tuning set may hold (0 is unset)")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0,
		"carrying cap: acquisition dollars for the selected assets (0 is unset)")
	flags.Float64Var(&f.redundancyMargin, "redundancy-margin", 0,
		"floor for the redundancy equivalence margin; 0 means the sample's own resolution decides (a user may only raise it)")
	flags.Float64Var(&f.redundancyMaxMargin, "redundancy-max-margin", 0,
		"ceiling on the redundancy equivalence margin; 0 means the stage default, 0.10")
	flags.Float64Var(&f.redundancyMinCoImprovement, "redundancy-min-coimprovement", 0,
		"floor for the redundancy co-improvement Jaccard; 0 means beyond-chance (J_chance) decides")
	flags.StringVar(&f.explainAssetID, "explain", "",
		"print the per-Case redundancy table for this asset ID and exit; free, read-only, no provider call")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	if err := cmd.MarkFlagRequired("value-run-id"); err != nil {
		panic(fmt.Sprintf("cli: marking --value-run-id required: %v", err))
	}
	return cmd
}

// runSelect executes the stage and renders the report.
func runSelect(ctx context.Context, out io.Writer, f selectFlags) error {
	// The constraint set is exactly the caps the user names: a budget with
	// no cap at all is not a budget, and the fix names the grammar.
	if f.maxContextTokens == 0 && f.maxTrainingExamples == 0 && f.maxCostUSD == 0 {
		return errs.ErrInvalidInput.
			WithFix("pass at least one budget cap: --max-context-tokens, " +
				"--max-training-examples, --max-cost-usd").
			Wrap(errors.New("select: a budget is required"))
	}

	// The pool is optional: without it the content rules degrade, and the
	// result says which ones did rather than silently deciding without them.
	var pool core.Pool
	if f.poolPath != "" {
		var err error
		pool, err = resolvePool(f.poolPath, false)
		if err != nil {
			return err
		}
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

	opts := core.SelectOptions{
		RunID:        runID,
		ValueRunID:   f.valueRunID,
		Store:        db,
		Pool:         pool,
		AllowPartial: f.allowPartial,
		Budget: &knov1.Budget{
			MaxContextTokens:    f.maxContextTokens,
			MaxTrainingExamples: f.maxTrainingExamples,
			MaxCostUsdMicros:    usdToMicros(f.maxCostUSD),
		},
		RedundancyMargin:           f.redundancyMargin,
		RedundancyMaxMargin:        f.redundancyMaxMargin,
		RedundancyMinCoImprovement: f.redundancyMinCoImprovement,
	}

	if f.explainAssetID != "" {
		return runExplain(ctx, out, opts, f)
	}

	res, runErr := opts.Select(ctx)
	if res == nil {
		return runErr
	}
	renderErr := renderSelect(out, f.jsonOut, res)
	if runErr != nil {
		return runErr
	}
	return renderErr
}

// runExplain prints the per-Case redundancy table for one Asset and exits —
// free, read-only, no provider call, no Run created, no Portfolio written.
func runExplain(ctx context.Context, out io.Writer, opts core.SelectOptions, f selectFlags) error {
	cmps, err := opts.Explain(ctx, f.explainAssetID, 0)
	if err != nil {
		return err
	}
	return renderExplain(out, f.jsonOut, f.explainAssetID, cmps)
}
