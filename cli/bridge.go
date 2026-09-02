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

	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
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

	// priceServeUSDSet records whether --price-serve-per-minute was passed
	// at all, as opposed to left at its zero default — cli/baseline.go's
	// costPerCallSet precedent, applied here. 0.0 is a legitimate rate (a
	// provider that genuinely does not bill for serving), and without this
	// an explicit --price-serve-per-minute 0 would be indistinguishable
	// from an absent flag, which resolveServePrice must not confuse: see
	// its doc.
	priceServeUSDSet bool

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
--bridge-max-live-endpoints live endpoints at once. Invoking the deployed
model during the eval pass is priced separately, per provider: free when
the provider's serving rate already covers inference (the hosting line
above is the only charge), priced per call when it does not — the plan
prints an "Eval pass" line either way, so which case applies is never
silent. --evals is required once armed: it is where the Case CONTENT behind
the value.Plan's Case IDs comes from, resolved and sealed once, dev Cases
only — the holdout is never read. A Case ID the plan names with no Case in
--evals refuses the whole run before any job is submitted.`,
		Example: `  # See the plan and the price, without spending anything
  kno bridge --select-run-id <id> --pool assets.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50 --price-serve-per-minute 0.02

  # Arm it: submit every group's job, deploy and measure it, tear it down
  kno bridge --select-run-id <id> --pool assets.jsonl --evals cases.jsonl --tuner together:meta-llama/Llama-3-8b --price-train-per-mtok 1.50 --price-serve-per-minute 0.02 --bridge --yes`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// runBridgeCore receives a plain bridgeFlags struct and never
			// sees cmd, so the presence check has to happen here — the same
			// reason cli/baseline.go's costPerCallSet is captured in RunE
			// rather than inside runBaseline.
			f.priceServeUSDSet = cmd.Flags().Changed("price-serve-per-minute")
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
	servePrice, err := resolveServePrice(scheme, model, f.priceServeUSD, f.priceServeUSDSet)
	if err != nil {
		return err
	}
	// The third pricing dimension: what the eval pass itself costs, if
	// anything. Resolved ONCE, here, beside the other two, and threaded
	// down rather than re-derived — see resolveEvalPrice's doc.
	evalPrice := resolveEvalPrice(scheme, model)

	quotes, err := bridge.QuoteGroups(p, groups, assets, model, price, f.epochs)
	if err != nil {
		return err
	}
	// N+1 endpoints (one per group), sequentially by default
	// (--bridge-max-live-endpoints), each capped at
	// --bridge-max-serve-minutes — Step 4's cap-bounded worst case, never a
	// prediction: hosting is stoppable and usually costs less.
	hostingCapUSDMicros := pricing.EstimateServeCap(servePrice, int(f.maxServeMinutes), 1) * int64(len(quotes))

	// devCaseIDs needs only the value.Plan's persisted Case IDs — no
	// --evals, no network — so it (and the eval-pass ceiling built from it)
	// is available un-armed, ahead of renderBridgePlan. This computation
	// used to live inside confirmAndRun, reached only after arming; moving
	// it here is what lets BOTH the printed plan and confirmAndRun's
	// consent total carry the eval pass's worst case as a third addend —
	// docs/plans/2026-09-02-openai-tuner.md §4: "a ceiling enforced
	// silently at runtime without appearing in the figure the user agreed
	// to is the same defect in a new place."
	devCaseIDs := bridge.DevCaseIDsForGroups(plan, groups)
	evalCapUSDMicros, evalCalls, err := evalPassCeiling(evalPrice, model, groups, devCaseIDs, plan.ControlCaseIDs)
	if err != nil {
		return err
	}

	if err := renderBridgePlan(out, f, quotes, groups, hostingCapUSDMicros, evalCapUSDMicros); err != nil {
		return err
	}

	if !f.bridgeArmed {
		return nil
	}
	return confirmAndRun(ctx, in, out, f, db, plan, groups, scheme, model, quotes,
		servePrice, hostingCapUSDMicros, evalPrice, evalCapUSDMicros, evalCalls)
}

// evalPassCeiling reports the worst-case cost of the eval pass an armed run
// will make, and how many invocations that worst case is spread across —
// the shape pricing.EstimateServeCap already uses for hosting (a per-unit
// worst case times a unit count), applied to the eval-pass dimension per
// docs/plans/2026-09-02-openai-tuner.md §4.
//
// price nil (no per-token rate resolved for this base model, per
// resolveEvalPrice) reports zero for both — Together's case today, and any
// future provider whose eval calls are genuinely free.
//
// The per-call worst case is computed with pricing.EstimateWithPrice
// directly — the exact function openaicompat.Agent.WorstCase calls
// internally — rather than by constructing an Agent: openaicompat.New
// refuses construction without a bound credential, which would make an
// un-armed `kno bridge` (no OPENAI_API_KEY set) fail to print a plan at all,
// breaking the command's own "zero network calls, zero dollars spent"
// promise. The prompt shape mirrors WorstCase's own worst Case (the full
// DefaultMaxPromptBytes ceiling, no Asset — bridgeAgentFactory injects
// none) and DefaultMaxOutputTokens, matching what the real Agent will
// actually be constructed with once armed.
//
// The call count is the SUM over every group bridge.Run will actually
// measure: the all-in group's union pass (every leave-one-out group's dev
// Cases plus the control partition) plus each leave-one-out group's own
// dev-plus-control pass — mirroring bridge/measure.go's unpublished
// unionCaseIDs/groupCaseIDs exactly, duplicated here because this figure is
// needed before bridge.RunParams exists, un-armed. NOT the deduplicated
// Case-ID union across the whole run: the same Case is invoked once per
// group whose deployed model it measures, so deduplicating across groups
// would UNDER-count the real number of billed calls.
func evalPassCeiling(
	price *knov1.Price, model string, groups *bridge.GroupsPlan,
	devCaseIDs map[string][]string, controlCaseIDs []string,
) (capUSDMicros int64, calls int, err error) {
	if price == nil {
		return 0, 0, nil
	}

	total := len(evalGroupCaseIDs(bridge.AllIn, devCaseIDs, controlCaseIDs))
	for tag := range groups.LeaveOneOut {
		total += len(evalGroupCaseIDs(tag, devCaseIDs, controlCaseIDs))
	}
	if total == 0 {
		return 0, 0, nil
	}

	worst := pricing.Prompt{Input: strings.Repeat("x", openaicompat.DefaultMaxPromptBytes)}
	est, err := pricing.EstimateWithPrice(price, model, worst, openaicompat.DefaultMaxOutputTokens)
	if err != nil {
		return 0, 0, errs.ErrInvalidInput.WithFix(
			"the fine-tuned pricing table row for this model cannot be turned into an " +
				"estimate; check pricing.Version's table for a malformed row",
		).Wrap(err)
	}
	return saturatingMul(est.CostUSDMicros, int64(total)), total, nil
}

// evalGroupCaseIDs is one group's own Case-ID set: its cluster's dev Cases
// unioned with the control partition, or — when group is bridge.AllIn —
// every OTHER group's dev Cases unioned with the control partition.
// Matches bridge/measure.go's unionCaseIDs (the all-in union pass) and
// groupCaseIDs (one leave-one-out group's pass) exactly.
func evalGroupCaseIDs(group string, devCaseIDs map[string][]string, controlCaseIDs []string) map[string]struct{} {
	set := make(map[string]struct{}, len(controlCaseIDs))
	for _, id := range controlCaseIDs {
		set[id] = struct{}{}
	}
	if group == bridge.AllIn {
		for _, ids := range devCaseIDs {
			for _, id := range ids {
				set[id] = struct{}{}
			}
		}
		return set
	}
	for _, id := range devCaseIDs[group] {
		set[id] = struct{}{}
	}
	return set
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
//
// priceUSDPerMinuteSet must come from cmd.Flags().Changed("price-serve-per-minute")
// (newBridgeCmd's RunE captures it into bridgeFlags.priceServeUSDSet), NOT
// from priceUSDPerMinute > 0. A provider that genuinely does not bill for
// serving needs to say --price-serve-per-minute 0 and be believed: with
// serveTable carrying no row for it, ">0" would refuse the flag that is
// supposed to unblock exactly this case, and relaxing to ">=0" would make
// EVERY unset flag on EVERY scheme silently resolve to "confirmed free" —
// a far larger hole than the one being fixed. See cli/baseline.go's
// costPerCallSet for the identical reasoning applied to
// --cost-per-call-usd.
func resolveServePrice(scheme, model string, priceUSDPerMinute float64, priceUSDPerMinuteSet bool) (pricing.ServePrice, error) {
	if p, ok := pricing.LookupServePrice(scheme, model); ok {
		return p, nil
	}
	if priceUSDPerMinuteSet {
		if priceUSDPerMinute < 0 {
			return pricing.ServePrice{}, errs.ErrInvalidInput.
				WithFix("pass a non-negative --price-serve-per-minute").
				Wrap(fmt.Errorf("--price-serve-per-minute is %.6f; a negative rate would "+
					"credit the budget on every hosting tick", priceUSDPerMinute))
		}
		return pricing.ServePrice{PerMinuteUSDMicros: usdToMicros(priceUSDPerMinute)}, nil
	}
	return pricing.ServePrice{}, errs.ErrInvalidInput.
		WithFix(fmt.Sprintf("pass --price-serve-per-minute, naming the hosting rate for %s:%s "+
			"(pricing.Version %s carries no row for it; pass 0 if this provider genuinely "+
			"does not bill for serving)", scheme, model, pricing.Version)).
		Wrap(fmt.Errorf("%s:%s has no serve price", scheme, model))
}

// resolveEvalPrice looks up the published fine-tuned INFERENCE rate for
// --tuner's base model — the third pricing dimension alongside
// resolveTrainPrice and resolveServePrice, resolved once here and threaded
// down to bridgeAgentFactory and bridge.ScoreEvalRunner rather than
// re-derived from the deployed model's transport Ref: bridgeAgentFactory
// hardcodes Ref.Scheme "openai" for Together's OpenAI-compatible HTTP
// route, and re-deriving off that string would resolve OpenAI rates for a
// Together model — charging per call on top of its hosting ticker, the
// double-count docs/plans/2026-09-02-openai-tuner.md exists to prevent.
//
// Unlike resolveTrainPrice and resolveServePrice, an absent row is NOT
// refused here — there is no --accept-unknown-cost-style escape hatch for
// this dimension in this build (see docs/debt.md#162). A nil return feeds
// AcceptFreeCalls (§2 of the plan above): free only when this model has no
// per-token rate at all, which is exactly true for a Together dedicated
// endpoint (pricing.LookupFineTunedPrice has no "together" rows) and
// exactly false once a per-token Tuner's base model gets one.
func resolveEvalPrice(scheme, model string) *knov1.Price {
	p, ok := pricing.LookupFineTunedPrice(scheme, model)
	if !ok {
		return nil
	}
	return p
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
