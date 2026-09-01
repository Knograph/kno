package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
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

	// bridgeTimeout is --bridge-timeout: how long a job may run before
	// bridge.Run stops WAITING on it (never cancels) — acceptance
	// criterion 24.
	bridgeTimeout    time.Duration
	cancelOnTimeout  bool
	priceServeUSD    float64 // dollars per replica per minute; the --price-serve-per-minute escape hatch
	maxLiveEndpoints int
	maxServeMinutes  int32

	// evalsPath is --evals: eval cases behind the value.Plan's Case IDs,
	// resolved and sealed at THIS choke point — see runBridgeMeasured's
	// doc. Only required once armed: the un-armed plan needs no Case
	// content, only Asset content (rendering the training files).
	evalsPath string
	goalName  string

	// keyEnv binds a host to the environment variable naming its
	// credential, shared by the Tuner and by the openaicompat agent this
	// build points at a deployed endpoint — both reach the same provider.
	keyEnv              []string
	allowInsecureURL    bool
	allowPrivateAddress bool
	holdoutFrac         float64
	splitSeed           string

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
in it once confirmed. Each job is charged when it is submitted and cannot be un-submitted.
Hosting a tuned model for its eval passes is charged per minute per endpoint,
including while idle, capped by --bridge-max-serve-minutes and serialized to
--bridge-max-live-endpoints live endpoints at once. --evals is required once
armed: it is where the Case CONTENT behind the value.Plan's Case IDs comes
from, resolved and sealed once, dev Cases only — the holdout is never read.
A Case ID the plan names with no Case in --evals refuses the whole run
before any job is submitted.`,
		Example: `  # See the plan and the price, without spending anything
  kno bridge --select-run-id <id> --pool assets.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50 --price-serve-per-minute 0.02

  # Arm it: submit every group's job, deploy and measure it, tear it down
  kno bridge --select-run-id <id> --pool assets.jsonl --evals cases.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50 --price-serve-per-minute 0.02 --bridge --yes`,
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
	flags.DurationVar(&f.bridgeTimeout, "bridge-timeout", 60*time.Minute,
		"how long to wait for one job to reach a terminal status before giving up on WAITING; the job is never cancelled and --resume keeps polling it")
	flags.BoolVar(&f.cancelOnTimeout, "bridge-cancel-on-timeout", false,
		"cancel a job that outlives --bridge-timeout instead of leaving it running")
	flags.Float64Var(&f.priceServeUSD, "price-serve-per-minute", 0,
		"dollars per replica per minute for an unpriced served model; required until a pricing table row exists for --tuner")
	flags.IntVar(&f.maxLiveEndpoints, "bridge-max-live-endpoints", 1,
		"at most this many dedicated endpoints live at once; each one bills per minute per replica, including while idle")
	flags.Int32Var(&f.maxServeMinutes, "bridge-max-serve-minutes", 30,
		"tear an endpoint down and report its group unknown after this many served minutes, per endpoint")
	flags.StringVar(&f.evalsPath, "evals", "",
		"eval cases behind the value.Plan's Case IDs: a JSONL file path, langsmith:<dataset-name>, "+
			"langfuse:<dataset-name>, braintrust:<dataset-name>, or hf:<org>/<name>/<config>/<split> "+
			"(required once --bridge is armed; the un-armed plan needs no Case content)")
	flags.StringVar(&f.goalName, "goal", "exact-match", "goal to score each group's deployed model against")
	flags.StringSliceVar(&f.keyEnv, "key-env", nil,
		"host=VAR credential bindings for the tuner and the deployed model it serves, e.g. api.together.xyz=TOGETHER_API_KEY")
	flags.BoolVar(&f.allowInsecureURL, "allow-insecure-base-url", false, "permit a plain-HTTP evals or provider base URL")
	flags.BoolVar(&f.allowPrivateAddress, "allow-private-address", false, "permit a loopback or private evals or provider address")
	flags.Float64Var(&f.holdoutFrac, "holdout-frac", 0.2, "share of --evals held back, matching the original kno baseline/value run")
	flags.StringVar(&f.splitSeed, "split-seed", "", "the --evals split seed, matching the original kno baseline/value run")
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
	// Refused exactly like an unpriced training rate — Step 2(f): "There is
	// no --accept-unknown-cost escape for the bridge", extended to hosting.
	// Resolved up front, at planning time, so the un-armed plan can print
	// the hosting cap even though nothing has been deployed yet.
	servePrice, err := resolveServePrice(scheme, model, f.priceServeUSD)
	if err != nil {
		return err
	}

	quotes, err := bridge.QuoteGroups(p, groups, assets, model, price, f.epochs)
	if err != nil {
		return err
	}
	// N+1 endpoints (one per group), sequentially by default
	// (--bridge-max-live-endpoints), each capped at
	// --bridge-max-serve-minutes — Step 4's cap-bounded worst case, never a
	// prediction: hosting is stoppable and usually costs less.
	hostingCapUSDMicros := pricing.EstimateServeCap(servePrice, int(f.maxServeMinutes), 1) * int64(len(quotes))

	if err := renderBridgePlan(out, f, quotes, groups, hostingCapUSDMicros); err != nil {
		return err
	}

	if !f.bridgeArmed {
		return nil
	}
	return confirmAndRun(ctx, in, out, f, db, plan, groups, scheme, model, quotes, servePrice, hostingCapUSDMicros)
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

// resolveServePrice mirrors resolveTrainPrice for the hosting dimension —
// Step 2(f): "an unpriced serve rate is refused exactly as an unpriced
// train rate is", with --price-serve-per-minute as the same explicit
// escape and no --accept-unknown-cost path.
func resolveServePrice(scheme, model string, priceUSDPerMinute float64) (pricing.ServePrice, error) {
	if p, ok := pricing.LookupServePrice(scheme, model); ok {
		return p, nil
	}
	if priceUSDPerMinute > 0 {
		return pricing.ServePrice{PerMinuteUSDMicros: usdToMicros(priceUSDPerMinute)}, nil
	}
	return pricing.ServePrice{}, errs.ErrInvalidInput.
		WithFix(fmt.Sprintf("pass --price-serve-per-minute, naming the hosting rate for %s:%s "+
			"(pricing.Version %s carries no row for it)", scheme, model, pricing.Version)).
		Wrap(fmt.Errorf("%s:%s has no serve price", scheme, model))
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
