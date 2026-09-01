package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// validateFlags are the options `kno validate` accepts, on top of the
// baseline flags it shares for agent and provider wiring — validate invokes
// the same agents the same way, so the wiring surface is the same surface.
type validateFlags struct {
	baselineFlags

	// selectRunID names the Portfolio to validate. Required: validate
	// measures a Portfolio, and a stage with nothing to measure is not a run.
	selectRunID string

	poolPath      string
	splitSections bool

	trials int32

	// contextOnly validates the DESTINATION_CONTEXT entries of a mixed
	// Portfolio and labels the result a subset.
	contextOnly bool

	// allowRepeatHoldout permits a second, different Portfolio against a
	// holdout that has already been used. Counted and disclosed, never
	// corrected for.
	allowRepeatHoldout bool

	// requireGain promotes an inconclusive or unmeasured verdict to exit 3.
	//
	// Off by default, and that default is the load-bearing part: an interval
	// crossing zero means "not enough evidence at this sample size", not "it
	// failed", so blocking on it would make a 20-Case holdout block every
	// deploy forever and train people to pass --force.
	requireGain bool

	// maxContextTokens overrides the Portfolio's own recorded carrying cap.
	maxContextTokens int64
}

func newValidateCmd() *cobra.Command {
	var f validateFlags

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Measure the selected portfolio against the untouched holdout",
		Long: `Run the whole selected portfolio against the holdout — the half of your evals
nothing before this stage was allowed to read — and report what it is worth.

This is HOLDOUT CONFIRMATION, not schema validation. Nothing here checks a
file format.

Two arms, both measured in this run: a control arm with nothing injected and a
treatment arm carrying the whole portfolio. That is why the call count is
double the number of holdout cases — the quote shows the arithmetic. The
alternative, pairing against the recorded dev baseline, is half the price and
is not an estimator of anything: no baseline run has ever scored a holdout
case, so the difference would carry the portfolio's effect, a random dev/holdout
population difference, and provider drift, in one number.

THE HOLDOUT IS CONSUMED ONCE PER PORTFOLIO, and the record is written before
the first agent call — a run that crashed half way through has already looked.
Re-running the same portfolio is refused; an interrupted run continues with
--resume; a DIFFERENT portfolio against the same holdout needs
--allow-repeat-holdout, and the count is printed with the number.

What the number claims: with this portfolio in context, the agent scored X
better on N cases it had never been measured on, under context injection — an
upper bound on what retrieval would deliver. It is unbiased for the effect of
THIS portfolio on the holdout population. It is not a corrected version of the
dev estimate, and it is not the effect of the best achievable portfolio.

Exit codes: 0 confirmed or inconclusive, 3 when the interval sits at or below
zero, and 3 for inconclusive too if you pass --require-gain. 2 if a budget cap
stopped the run, 4 if it was interrupted; both are resumable.`,
		Example: `  # Confirm the portfolio on the holdout
  kno validate --evals cases.jsonl --pool assets.jsonl --select-run-id <id>

  # Gate a deploy on a demonstrated gain
  kno validate --evals cases.jsonl --pool assets.jsonl --select-run-id <id> --yes --require-gain`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.costPerCallSet = cmd.Flags().Changed("cost-per-call-usd")
			f.seedSet = cmd.Flags().Changed("seed")
			cfg, err := loadConfigFile(cmd)
			if err != nil {
				return err
			}
			if _, err := f.applyFileAndEnv(cmd, cfg); err != nil {
				return err
			}
			err = runValidate(cmd.Context(), cmd.InOrStdin(),
				cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
			return configAwareFix(err, cfg.path)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.evalsPath, "evals", "", "eval cases: a JSONL file path, langsmith:<dataset-name>, langfuse:<dataset-name>, braintrust:<dataset-name>, or hf:<org>/<name>/<config>/<split> (required)")
	flags.StringVar(&f.poolPath, "pool", "",
		"assets the portfolio names: a JSONL file path, csv:<file>, md:<file-or-dir>, or hf:<org>/<name>/<config>/<split>:<kind> (required)")
	flags.BoolVar(&f.splitSections, "split-sections", false,
		"split an md: pool into its ## sections, one asset per section")
	flags.StringVar(&f.selectRunID, "select-run-id", "",
		"run ID of the Select run whose Portfolio to validate (required; run `kno select` first)")
	flags.StringVar(&f.agentRef, "agent", "fake:", "agent to measure")
	flags.StringVar(&f.goalName, "goal", "exact-match", "goal to score against")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.StringVar(&f.runID, "run-id", "", "identifier for this run (generated if empty)")
	flags.IntVar(&f.concurrency, "concurrency", 0, "in-flight measurements (0 picks a conservative default)")
	flags.Int32Var(&f.trials, "trials", 0, "how many times each measurement is repeated (0 means once)")
	flags.Float64Var(&f.holdoutFrac, "holdout-frac", split.DefaultHoldoutFrac,
		"share of cases held back; it must match the value the pipeline was measured under")
	flags.StringVar(&f.splitSeed, "split-seed", "",
		"deliberately re-split the evals; a re-split holdout is a DIFFERENT holdout and is counted as one")
	flags.BoolVar(&f.contextOnly, "context-only", false,
		"validate only the context-destination entries of a mixed portfolio; the number is then labelled a subset")
	flags.BoolVar(&f.allowRepeatHoldout, "allow-repeat-holdout", false,
		"measure a second portfolio against a holdout that has already been used; the count is recorded and printed")
	flags.BoolVar(&f.requireGain, "require-gain", false,
		"exit 3 unless the holdout interval demonstrates a gain; an inconclusive result is not a failure by default")
	flags.Int64Var(&f.maxContextTokens, "max-context-tokens", 0,
		"refuse a portfolio carrying more than this (0 uses the cap the portfolio was built under)")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0, "stop before spending more than this (0 is unlimited)")
	flags.Int64Var(&f.maxCalls, "max-calls", 0, "stop after this many agent calls (0 is unlimited)")
	flags.Float64Var(&f.costPerCall, "cost-per-call-usd", 0,
		"expected cost of one agent call; 0 asserts the calls are free. Not needed for an agent that prices itself")
	flags.BoolVar(&f.resume, "resume", false, "continue an interrupted run instead of starting one")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	flags.BoolVar(&f.yes, "yes", false, "proceed without being asked; the estimate is still printed")

	// Provider wiring: the same surface as baseline and value.
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
		"refuse a Case whose prompt exceeds this (the injected portfolio is bounded separately, and charged on top)")
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

	addConfigFlag(cmd, defaultConfigPath)

	for _, name := range []string{"evals", "pool", "select-run-id"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("cli: marking --%s required: %v", name, err))
		}
	}
	return cmd
}

// validatePlanningCostPerCall is what one measurement may cost, mirroring
// valuePlanningCostPerCall: an Estimator prices each Case from the Case, so
// its worst case wins over the caller's scalar.
func validatePlanningCostPerCall(opts core.ValidateOptions) int64 {
	if e, ok := opts.Agent.(core.Estimator); ok {
		if w := e.WorstCase(); w.CostUSDMicros > 0 {
			return w.CostUSDMicros
		}
	}
	return opts.EstCostPerCallUSDMicros
}

// holdoutFingerprint identifies the HOLDOUT, not the run.
//
// The eval source's content hash and the split configuration together, because
// either one changing produces a different set of holdout Cases. Keying the
// one-shot record on this is what makes `--split-seed` an honest escape hatch
// rather than a silent one: a re-split holdout genuinely IS a different
// holdout, and it is counted as its own rather than colliding with the first.
func holdoutFingerprint(content string, splitSeed string, holdoutFrac float64) string {
	h := sha256.New()
	_, _ = h.Write([]byte(content))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(split.FingerprintSplit(splitSeed, holdoutFrac))
	return hex.EncodeToString(h.Sum(nil))
}

// validateInputFingerprint pins everything a resume must not silently change.
//
// The holdout's identity, the Portfolio, the agent and the repeat count. A
// resume whose evals were re-split would continue a run against a holdout
// containing formerly-dev Cases, which is not a holdout; a resume against a
// different Portfolio would pair new measurements against rows recorded for
// another set.
func validateInputFingerprint(holdout, selectRunID, agentRef string, trials int32) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d", holdout, selectRunID, agentRef, trials)
	return hex.EncodeToString(h.Sum(nil))
}

func runValidate(ctx context.Context, in io.Reader, out, errOut io.Writer, f validateFlags) error {
	stopTracing, err := startTracing(ctx, errOut, f.traceSpans)
	if err != nil {
		return err
	}
	defer stopTracing()

	evals, err := resolveEvals(f.evalsFlags())
	if err != nil {
		return err
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
	if f.trials < 0 {
		return errs.ErrInvalidInput.
			WithFix("pass a positive --trials, or omit it for once").
			Wrap(fmt.Errorf("--trials is %d; a negative repeat count is not a schedule", f.trials))
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

	content, err := evals.ContentHash(ctx)
	if err != nil {
		return errs.ErrInvalidInput.Wrap(err)
	}
	holdoutFP := holdoutFingerprint(content, f.splitSeed, f.holdoutFrac)

	recorder := &consentRecorder{}
	guard := budget.New(
		budget.Limits{
			MaxCostUSDMicros: usdToMicros(f.maxCostUSD),
			MaxLLMCalls:      f.maxCalls,
		},
		confirmFunc(out, f.yes, f.jsonOut, recorder),
		usdToMicros(confirmThresholdUSD),
	)

	runID := f.runID
	if runID == "" {
		runID = newRunID(time.Now())
	}

	opts := core.ValidateOptions{
		RunID:              runID,
		SelectRunID:        f.selectRunID,
		Agent:              agent,
		AgentRef:           agentRef,
		Goal:               goal,
		GoalName:           f.goalName,
		Guard:              guard,
		Evals:              evals,
		Pool:               pool,
		Store:              db,
		Concurrency:        f.concurrency,
		Trials:             f.trials,
		Resume:             f.resume,
		ContextOnly:        f.contextOnly,
		AllowRepeatHoldout: f.allowRepeatHoldout,
		MaxContextTokens:   f.maxContextTokens,
		// split.MinHoldout, passed rather than imported: core cannot reach
		// adapters/ (prime directive 3), so the threshold travels from the
		// package that owns the split. TestValidatePassesTheSplitPackagesMinHoldout
		// pins this, which is what makes changing the constant change the
		// verdict here.
		MinHoldout:              split.MinHoldout,
		EvalFingerprint:         holdoutFP,
		InputFingerprint:        validateInputFingerprint(holdoutFP, f.selectRunID, agentRef.GetRef(), f.trials),
		EstCostPerCallUSDMicros: usdToMicros(f.costPerCall),
	}

	// The pre-run consent dialog. The quote SHOWS THE DOUBLING: a figure of
	// n x trials would understate the run by exactly the arm count, which is
	// the failure core/ring0.go records having already happened once at a
	// different multiple.
	quote, err := opts.Quote(ctx)
	if err != nil {
		return err
	}
	if quote.NothingToValidate {
		res, runErr := opts.Validate(ctx)
		if res == nil {
			return runErr
		}
		return renderValidate(out, f, res, quote, counts, budget.Spend{})
	}

	if !f.yes && !f.jsonOut && shouldPrompt(in, out) {
		var settled budget.Spend
		if f.resume {
			sp, err := db.SettledSpend(ctx, runID)
			if err != nil {
				return errs.ErrInvalidInput.WithFix("check --db is readable").
					Wrap(fmt.Errorf("reading settled spend for %s: %w", runID, err))
			}
			settled = sp
		}
		if _, err := fmt.Fprintf(out, "\nValidating %d asset(s) against the holdout: %s.\n",
			quote.AssetCount, quote.Derivation()); err != nil {
			return err
		}
		intent := budget.Estimate{
			Calls:         quote.Calls,
			CostUSDMicros: saturatingMul(quote.Calls, validatePlanningCostPerCall(opts)),
		}
		decision, err := consentDialog(ctx, in, out, intent, guard.Limits(), settled, recorder)
		if err != nil {
			return err
		}
		if decision.limits != guard.Limits() {
			guard = budget.New(decision.limits,
				confirmFunc(out, f.yes, f.jsonOut, recorder),
				usdToMicros(confirmThresholdUSD))
			opts.Guard = guard
		}
	}

	// The consent figure, printed BEFORE the run in --yes human mode, so the
	// scrollback shows what was agreed to. JSON mode stays a pure document.
	if f.yes && !f.jsonOut {
		if _, err := fmt.Fprintf(out,
			"Validating %d asset(s) against the holdout: %s.\n",
			quote.AssetCount, quote.Derivation()); err != nil {
			return err
		}
	}

	var restored budget.Spend
	if f.resume {
		sp, spErr := db.SettledSpend(ctx, runID)
		if spErr == nil {
			restored = sp
		}
	}

	res, runErr := opts.Validate(ctx)
	if res == nil {
		return runErr
	}
	renderErr := renderValidate(out, f, res, quote, counts, restored)
	if runErr != nil {
		return runErr
	}
	if renderErr != nil {
		return renderErr
	}
	return validateExit(res, f.requireGain)
}
