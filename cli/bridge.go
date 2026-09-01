package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// bridgeFlags are the options `kno bridge` accepts.
type bridgeFlags struct {
	dbPath      string
	selectRunID string
	poolPath    string
	tuner       string // scheme:model, e.g. together:meta-llama/Llama-3-8b

	// bridgeArmed is --bridge: without it, nothing is ever submitted. See
	// the tuner-bridge plan's Step 4.
	bridgeArmed bool

	maxGroups     int
	epochs        int32
	maxCostUSD    float64
	priceTrainUSD float64 // dollars per million TRAINING tokens; the --price-train-per-mtok escape hatch

	yes     bool
	jsonOut bool
}

func newBridgeCmd() *cobra.Command {
	var f bridgeFlags

	cmd := &cobra.Command{
		Use:   "bridge",
		Short: "Group-ablate the tuning set on a proxy model (Tier 3)",
		Long: `Fine-tune a small proxy model on the tuning-set behavior Assets a Select run
chose, and measure leave-one-group-out: does each failure cluster's Assets
actually transfer under fine-tuning, and does tuning on them regress
anything else. This is Tier 3 of the bridge — see DESIGN.md — and it costs
real, irreversible money: each ablation group is one fine-tuning job.

WITHOUT --bridge, this command plans and prints and submits nothing. It
reads the Portfolio, forms the ablation groups from the source Value run's
failure clusters, renders every group's training file with the exact
bytes 'kno export --destination tuning_set' would write, counts tokens,
and prices them — all locally, with zero network calls and zero dollars
spent. That is the whole of the un-armed run: read the plan before you
decide whether to pay for it.

WITH --bridge, the plan is the same and a job is submitted for every group
in it once confirmed. Every job is charged when it is submitted and cannot
be un-submitted.

NOT YET IMPLEMENTED IN THIS BUILD: actual job submission, polling, and the
per-group leave-one-out measurement. --bridge plans, prices, and asks for
confirmation; it stops before submitting anything and says so.`,
		Example: `  # See the plan and the price, without spending anything
  kno bridge --select-run-id <id> --pool assets.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50

  # Arm it (submission is not yet implemented; this still stops before spending)
  kno bridge --select-run-id <id> --pool assets.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50 --bridge --yes`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBridge(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.selectRunID, "select-run-id", "",
		"run ID of the recorded Portfolio to bridge (required; run `kno select` first)")
	flags.StringVar(&f.poolPath, "pool", "",
		"assets whose content the training files render: a JSONL file path, csv:<file>, or md:<file-or-dir> (required)")
	flags.StringVar(&f.tuner, "tuner", "",
		"the base model to tune, as scheme:model, e.g. together:meta-llama/Llama-3-8b (required)")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.BoolVar(&f.bridgeArmed, "bridge", false,
		"submit jobs; without it the plan is printed and nothing is submitted or spent")
	flags.IntVar(&f.maxGroups, "bridge-max-groups", 6,
		"refuse the run rather than merge groups beyond this many leave-one-out jobs")
	flags.Int32Var(&f.epochs, "epochs", 3, "training epochs per job; the provider default is used if this is 0")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0, "total spend cap in USD across every job; 0 means unlimited")
	flags.Float64Var(&f.priceTrainUSD, "price-train-per-mtok", 0,
		"dollars per million TRAINING tokens for an unpriced base model; required until a pricing table row exists for --tuner")
	flags.BoolVar(&f.yes, "yes", false, "skip the confirmation prompt")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	for _, name := range []string{"select-run-id", "pool", "tuner"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("cli: marking --%s required: %v", name, err))
		}
	}
	return cmd
}

// runBridge opens the store and resolves the pool, then hands off to
// runBridgeCore — the split exists so tests can inject a fixture pool and
// an already-populated database without going through --pool's string
// grammar or a fresh `kno value`/`kno select` run.
func runBridge(ctx context.Context, in io.Reader, out io.Writer, f bridgeFlags) error {
	pool, err := resolvePool(f.poolPath, false)
	if err != nil {
		return err
	}
	return runBridgeCore(ctx, in, out, f, pool)
}

// runBridgeCore computes the bridge's plan and, if armed and confirmed,
// stops before submitting anything — see newBridgeCmd's Long description
// for why.
func runBridgeCore(ctx context.Context, in io.Reader, out io.Writer, f bridgeFlags, pool core.Pool) error {
	scheme, model, err := parseTunerRef(f.tuner)
	if err != nil {
		return err
	}

	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is writable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	p, err := db.Portfolio(ctx, f.selectRunID)
	if err != nil {
		if errors.Is(err, store.ErrPortfolioNotFound) {
			return errs.ErrInvalidInput.
				WithFix("run `kno select` first, then bridge the run it produced").
				Wrap(fmt.Errorf("run %s recorded no Portfolio", f.selectRunID))
		}
		return fmt.Errorf("loading the Portfolio for %s: %w", f.selectRunID, err)
	}

	plan, err := loadValuePlan(ctx, db, p.GetSourceRunId())
	if err != nil {
		return err
	}

	population, err := bridge.Population(p)
	if err != nil {
		return err
	}
	if len(population) == 0 {
		if _, err := io.WriteString(out, "Nothing to bridge: the Portfolio has no tuning-set entries.\n"); err != nil {
			return err
		}
		return nil
	}

	groups, err := bridge.BuildGroups(plan, population, f.maxGroups)
	if err != nil {
		return err
	}

	assets, err := core.LoadAssetsByID(ctx, pool)
	if err != nil {
		return err
	}

	price, err := resolveTrainPrice(scheme, model, f.priceTrainUSD)
	if err != nil {
		return err
	}

	quotes, err := bridge.QuoteGroups(p, groups, assets, model, price, f.epochs)
	if err != nil {
		return err
	}

	if err := renderBridgePlan(out, f, quotes, groups); err != nil {
		return err
	}

	if !f.bridgeArmed {
		return nil
	}
	return confirmAndStop(ctx, in, out, f, quotes)
}

// confirmAndStop runs the SAME budget-confirmation machinery every other
// spend path in Kno uses (stats/budget.Guard, cli's confirmFunc) against
// the bridge's total quote, so an armed-but-unconfirmed run declines
// through the identical errs.ErrBudgetExceeded path a Case-level spend
// would. It never reaches a Tuner: job submission, polling, and per-group
// measurement are not implemented in this build (see newBridgeCmd's Long
// description and this PR's report), so a confirmed run stops here rather
// than pretending to have submitted anything.
func confirmAndStop(ctx context.Context, _ io.Reader, out io.Writer, f bridgeFlags, quotes []bridge.GroupQuote) error {
	total := bridge.TotalEstimatedCostUSDMicros(quotes)
	var totalTokens int64
	for _, q := range quotes {
		totalTokens += q.TrainTokens
	}

	recorder := &consentRecorder{}
	guard := budget.New(
		budget.Limits{MaxCostUSDMicros: usdToMicros(f.maxCostUSD)},
		confirmFunc(out, f.yes, f.jsonOut, recorder),
		usdToMicros(confirmThresholdUSD),
	)

	res, err := guard.Authorize(ctx, budget.Estimate{
		Calls:         int64(len(quotes)),
		CostUSDMicros: total,
		Tokens:        totalTokens,
	})
	if err != nil {
		// The SAME refusal a declined Case-level spend produces:
		// errs.ErrBudgetExceeded, exit 2, resumable in spirit (nothing was
		// spent). Nothing was written, nothing was submitted.
		return err
	}
	// Nothing was actually authorized against real work: this build never
	// calls Tuner.Submit. Release rather than Settle — a Settle here would
	// record spend for nothing, which is exactly the silent-spend failure
	// prime directive 4 exists to prevent.
	res.Release()

	return errs.ErrInvalidInput.
		WithFix("this build plans, prices, and confirms a bridge run, but does not yet submit jobs; " +
			"bridge.SubmitGroup is implemented and tested (see bridge/submit.go) for a caller that " +
			"wires its own orchestration loop").
		Wrap(fmt.Errorf("bridge: job submission is not implemented in this build; nothing was spent"))
}

// parseTunerRef splits --tuner's scheme:model grammar.
func parseTunerRef(raw string) (scheme, model string, err error) {
	scheme, model, ok := strings.Cut(raw, ":")
	if !ok || scheme == "" || model == "" {
		return "", "", errs.ErrInvalidInput.
			WithFix("pass --tuner as scheme:model, for example together:meta-llama/Llama-3-8b").
			Wrap(fmt.Errorf("--tuner %q is not scheme:model", raw))
	}
	return scheme, model, nil
}

// resolveTrainPrice looks up a training rate, falling back to the explicit
// --price-train-per-mtok escape hatch. There is no --accept-unknown-cost
// path for the bridge — see the tuner-bridge plan's Step 2(a): the estimate
// is the only control on a single irreversible commitment, so the escape
// is the explicit one, not a waiver.
func resolveTrainPrice(scheme, model string, priceUSDPerMTok float64) (pricing.TrainPrice, error) {
	if p, ok := pricing.LookupTrainPrice(scheme, model); ok {
		return p, nil
	}
	if priceUSDPerMTok > 0 {
		return pricing.TrainPrice{PerMTokUSDMicros: usdToMicros(priceUSDPerMTok)}, nil
	}
	return pricing.TrainPrice{}, errs.ErrInvalidInput.
		WithFix(fmt.Sprintf("pass --price-train-per-mtok, naming the training rate for %s:%s "+
			"(pricing.Version %s carries no row for it)", scheme, model, pricing.Version)).
		Wrap(fmt.Errorf("%s:%s has no training price", scheme, model))
}

// loadValuePlan decodes the source Value run's persisted value.Plan.
func loadValuePlan(ctx context.Context, db store.Store, sourceRunID string) (*value.Plan, error) {
	if sourceRunID == "" {
		return nil, errs.ErrInvalidInput.
			WithFix("re-run `kno select` against a Portfolio produced from a `kno value` run").
			Wrap(fmt.Errorf("the Portfolio names no source Value run"))
	}
	source, err := db.GetRun(ctx, sourceRunID)
	if err != nil {
		return nil, fmt.Errorf("loading the source Value run %s: %w", sourceRunID, err)
	}
	if len(source.GetValuePlan()) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("re-run `kno value`; this run predates the persisted routing plan the bridge needs").
			Wrap(fmt.Errorf("run %s recorded no value plan", sourceRunID))
	}
	var plan value.Plan
	if err := gob.NewDecoder(bytes.NewReader(source.GetValuePlan())).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decoding the source Value plan for %s: %w", sourceRunID, err)
	}
	if len(plan.Clusters) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("re-run `kno value`; no clusters to group by means nothing routed to failure tags").
			Wrap(fmt.Errorf("run %s recorded no clusters", sourceRunID))
	}
	return &plan, nil
}
