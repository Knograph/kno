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
	"github.com/knograph/kno/core/value"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// valueFlags are the options `kno value` accepts, on top of the baseline
// flags it shares for agent and provider wiring — the two stages invoke the
// same agents the same way, so the wiring surface is the same surface.
type valueFlags struct {
	baselineFlags

	poolPath string

	// SplitSections makes an md: pool yield its `## ` sections as Assets
	// instead of whole files.
	splitSections bool

	// BaselineRunID is the run whose recorded scores this one pairs against.
	// Required: a delta without a reference is not a delta.
	baselineRunID string

	// Routing knobs, all named after the plan's vocabulary.
	sampleRate        float64
	controlSampleRate float64
	controlReserve    float64
	trials            int32
	route             string
	unsafeBaseline    bool

	// RoutingSeed seeds the routing draw. Distinct from the provider's
	// --seed, which is a sampling parameter: the routing seed decides which
	// Cases measure which Asset, and it is what the run records for an
	// auditable selection.
	routingSeed int64
}

func newValueCmd() *cobra.Command {
	var f valueFlags

	cmd := &cobra.Command{
		Use:   "value",
		Short: "Measure the marginal value of each asset in a pool",
		Long: `Measure every asset in the pool against the recorded baseline: each asset is
injected into the agent's context, the agent is re-run over the cases the
asset was routed to, and the result is a delta — did it help, how sure are we,
and what did it cost.

The control arm never carries the asset. The holdout is never read here.

Interrupting is safe: measurements are checkpointed as they complete, and
--resume continues without paying for anything twice.`,
		Example: `  # Value a pool against a recorded baseline
  kno value --evals cases.jsonl --pool assets.jsonl --baseline-run-id <id>

  # No routing: measure every asset against a sample of everything
  kno value --evals cases.jsonl --pool assets.jsonl --baseline-run-id <id> --route none`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.costPerCallSet = cmd.Flags().Changed("cost-per-call-usd")
			f.seedSet = cmd.Flags().Changed("seed")
			return runValue(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.evalsPath, "evals", "", "eval cases: a JSONL file path, or langsmith:<dataset-name> (required)")
	flags.StringVar(&f.poolPath, "pool", "",
		"assets to value: a JSONL file path, csv:<file>, or md:<file-or-dir> (required)")
	flags.BoolVar(&f.splitSections, "split-sections", false,
		"split an md: pool into its ## sections, one asset per section")
	flags.StringVar(&f.baselineRunID, "baseline-run-id", "",
		"run ID of the recorded baseline to pair against (required; run `kno baseline` first)")
	flags.StringVar(&f.agentRef, "agent", "fake:", "agent to measure")
	flags.StringVar(&f.goalName, "goal", "exact-match", "goal to score against")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.StringVar(&f.runID, "run-id", "", "identifier for this run (generated if empty)")
	flags.IntVar(&f.concurrency, "concurrency", 0, "in-flight measurements (0 picks a conservative default)")
	flags.Float64Var(&f.sampleRate, "sample-rate", 0,
		"fraction of an asset's routed cases to measure (0 uses the default)")
	flags.Float64Var(&f.controlSampleRate, "control-sample-rate", 0,
		"fraction of the reserved partition the harm test measures (0 uses the default)")
	flags.Float64Var(&f.controlReserve, "control-reserve", 0,
		"fraction of eligible cases held out of routing for the harm test (0 uses the default)")
	flags.Int32Var(&f.trials, "trials", 0, "how many times each measurement is repeated (0 means once)")
	flags.StringVar(&f.route, "route", "",
		"routing mode: \"none\" measures every asset against a sample of everything")
	flags.Int64Var(&f.routingSeed, "routing-seed", 1,
		"seed for the routing draw; the same seed reproduces the same selection")
	flags.BoolVar(&f.unsafeBaseline, "unsafe-baseline", false,
		"accept a baseline that resolved more than one model; the refusal is the safe default")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0, "stop before spending more than this (0 is unlimited)")
	flags.Int64Var(&f.maxCalls, "max-calls", 0, "stop after this many agent calls (0 is unlimited)")
	flags.Float64Var(&f.costPerCall, "cost-per-call-usd", 0,
		"expected cost of one agent call; 0 asserts the calls are free. Not needed for an agent that prices itself")
	flags.BoolVar(&f.resume, "resume", false, "continue an interrupted run instead of starting one")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	flags.BoolVar(&f.yes, "yes", false, "proceed without being asked; the estimate is still printed")

	// Provider wiring: the same surface as baseline, so the two stages invoke
	// the same agents the same way.
	flags.StringVar(&f.baseURL, "base-url", "",
		"endpoint root for a compatible provider (or write it as @<url> in --agent)")
	flags.StringArrayVar(&f.keyEnv, "key-env", nil,
		"bind a host to the NAME of an environment variable holding its key, as host=VAR (repeatable)")
	flags.BoolVar(&f.allowInsecureURL, "allow-insecure-base-url", false,
		"permit a plain-http base URL")
	flags.BoolVar(&f.allowPrivateAddress, "allow-private-address", false,
		"permit loopback and private addresses, for a local model server")
	flags.Int64Var(&f.maxOutputTokens, "max-output-tokens", 0,
		"generation ceiling, which also bounds every cost estimate")
	flags.Int64Var(&f.maxPromptBytes, "max-prompt-bytes", 0,
		"refuse a Case whose prompt exceeds this (an injected Asset is bounded separately, and charged on top)")
	flags.Float64Var(&f.temperature, "temperature", 0,
		"sampling temperature (unset leaves the provider default)")
	flags.Int64Var(&f.seed, "seed", 0, "sampling seed, where the provider supports one")
	flags.StringVar(&f.system, "system", "", "system prompt prepended to every Case")
	flags.StringVar(&f.generationParams, "generation-params", "",
		"override whether this model accepts generation parameters: auto, on, or off")
	flags.BoolVar(&f.useLegacyMaxTokens, "use-legacy-max-tokens", false,
		"send max_tokens instead of max_completion_tokens, for older self-hosted servers")
	flags.DurationVar(&f.timeout, "timeout", 0, "per-call deadline")
	flags.Float64Var(&f.priceInPerMTok, "price-input-per-mtok", 0,
		"input price per million tokens, for a model with no table row (pairs with --price-output-per-mtok)")
	flags.Float64Var(&f.priceOutPerMTok, "price-output-per-mtok", 0,
		"output price per million tokens (needs --price-input-per-mtok)")
	flags.BoolVar(&f.acceptUnknownCost, "accept-unknown-cost", false,
		"run a model whose per-Case cost cannot be computed")
	flags.BoolVar(&f.traceSpans, "trace-spans", false,
		"write OpenTelemetry spans for this run to stderr")

	if err := cmd.MarkFlagRequired("evals"); err != nil {
		panic(fmt.Sprintf("cli: marking --evals required: %v", err))
	}
	if err := cmd.MarkFlagRequired("pool"); err != nil {
		panic(fmt.Sprintf("cli: marking --pool required: %v", err))
	}
	if err := cmd.MarkFlagRequired("baseline-run-id"); err != nil {
		panic(fmt.Sprintf("cli: marking --baseline-run-id required: %v", err))
	}
	return cmd
}

// routingOptions resolves the routing knobs, refusing combinations the plan
// already resolved as one-way doors.
func (f valueFlags) routingOptions() (value.Options, error) {
	opts := value.Options{
		SampleRate:        f.sampleRate,
		ControlSampleRate: f.controlSampleRate,
		ControlReserve:    f.controlReserve,
		Trials:            f.trials,
		Seed:              f.routingSeed,
	}
	switch f.route {
	case "", "auto":
		// The default: failure-clustered routing with a fresh control arm.
	case "none":
		opts.DisableRouting = true
	default:
		return opts, errs.ErrInvalidInput.
			WithFix("pass --route none, or omit --route for the default").
			Wrap(fmt.Errorf("--route %q: the only mode this stage ships besides the "+
				"default is \"none\"", f.route))
	}
	if f.sampleRate < 0 || f.controlSampleRate < 0 || f.controlReserve < 0 {
		return opts, errs.ErrInvalidInput.
			WithFix("pass fractions between 0 and 1").
			Wrap(fmt.Errorf("sampling rates are fractions: a negative one would invert the schedule"))
	}
	if f.trials < 0 {
		return opts, errs.ErrInvalidInput.
			WithFix("pass a positive --trials, or omit it for once").
			Wrap(fmt.Errorf("--trials is %d; a negative repeat count is not a schedule", f.trials))
	}
	return opts, nil
}

func runValue(ctx context.Context, out, errOut io.Writer, f valueFlags) error {
	stopTracing, err := startTracing(ctx, errOut, f.traceSpans)
	if err != nil {
		return err
	}
	defer stopTracing()

	evals, err := jsonl.New(jsonl.Options{
		Path:        f.evalsPath,
		HoldoutFrac: 0,
		SplitSeed:   f.splitSeed,
	})
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --evals").Wrap(err)
	}
	counts, err := evals.CountSplits(ctx)
	if err != nil {
		return errs.ErrInvalidInput.WithFix(countsSplitFix(evals)).Wrap(err)
	}

	pool, err := resolvePool(f.poolPath, f.splitSections)
	if err != nil {
		return err
	}

	if err := f.validateCaps(); err != nil {
		return err
	}
	routing, err := f.routingOptions()
	if err != nil {
		return err
	}

	agent, agentRef, err := resolveAgent(f.baselineFlags)
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

	opts := core.ValueOptions{
		RunID:                   runID,
		BaselineRunID:           f.baselineRunID,
		Agent:                   agent,
		AgentRef:                agentRef,
		Goal:                    goal,
		GoalName:                f.goalName,
		Guard:                   guard,
		Store:                   db,
		Evals:                   core.Seal(evals),
		Concurrency:             f.concurrency,
		Resume:                  f.resume,
		UnsafeBaseline:          f.unsafeBaseline,
		InputFingerprint:        fingerprint,
		EstCostPerCallUSDMicros: usdToMicros(f.costPerCall),
		Routing:                 routing,
	}

	// The consent figure, printed BEFORE the run in --yes human mode: the
	// measurement count the quote's own formula produces, so the scrollback
	// shows what was agreed to. JSON mode stays a pure document — the figure
	// travels in the report instead.
	if f.yes && !f.jsonOut {
		plan, quoteErr := opts.Quote(ctx, pool)
		if quoteErr != nil {
			return quoteErr
		}
		if _, err := fmt.Fprintf(out,
			"Planning %d measurements over %d assets against baseline %s.\n",
			plan.Measurements(), len(plan.Routed), f.baselineRunID); err != nil {
			return err
		}
	}

	res, runErr := opts.Value(ctx, pool)
	if res == nil {
		return runErr
	}
	renderErr := renderValue(out, f, opts, res, counts, runID)
	if runErr != nil {
		return runErr
	}
	return renderErr
}
