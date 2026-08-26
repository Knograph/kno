package cli

import (
	"context"
	"fmt"
	"io"
	"math"
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

	// Provider wiring. Every one of these reaches a real endpoint, which is
	// what makes this command able to spend money for the first time.
	//
	// There is no KNO_* env mirror for any of them. DESIGN.md specifies three
	// layers — flag, env var, kno.yaml — and NONE of the machinery exists:
	// os.Getenv appears outside tests in exactly two places, both for API keys.
	// Shipping a mirror column here would be specifying a config system in a
	// flag table. Tracked as docs/debt.md#62.
	baseURL             string
	keyEnv              []string
	allowInsecureURL    bool
	allowPrivateAddress bool
	maxOutputTokens     int64
	maxPromptBytes      int64
	temperature         float64
	seed                int64
	system              string
	generationParams    string
	useLegacyMaxTokens  bool
	timeout             time.Duration
	priceInPerMTok      float64
	priceOutPerMTok     float64
	acceptUnknownCost   bool
	traceSpans          bool

	// costPerCallSet records whether --cost-per-call-usd was passed at all, as
	// opposed to left at its zero default. An explicit zero is a claim that
	// the calls are free; an absent flag is no claim.
	costPerCallSet bool

	// seedSet does the same for --seed, where 0 is a legitimate value.
	seedSet bool
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
			f.costPerCallSet = cmd.Flags().Changed("cost-per-call-usd")
			f.seedSet = cmd.Flags().Changed("seed")
			return runBaseline(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
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
	flags.Float64Var(&f.costPerCall, "cost-per-call-usd", 0,
		"expected cost of one agent call; 0 asserts the calls are free. Not needed for an agent that prices itself")
	flags.BoolVar(&f.resume, "resume", false, "continue an interrupted run instead of starting one")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	flags.BoolVar(&f.yes, "yes", false, "proceed without being asked; the estimate is still printed")

	// Provider wiring.
	//
	// --base-url is COMPOSED INTO the agent ref rather than passed to the
	// adapter beside it. agentref.Parse is where the credential-in-a-URL and
	// control-character refusals live, and a second entry point that skipped
	// them would be a second place for a key to reach the Run record — which
	// openaicompat.Options.Ref's own godoc says in as many words.
	flags.StringVar(&f.baseURL, "base-url", "",
		"endpoint root for a compatible provider (or write it as @<url> in --agent)")
	// The NAME of an environment variable, never a key. A key on a command
	// line lands in shell history, in ps output, and in CI logs.
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
	flags.Float64Var(&f.temperature, "temperature", math.NaN(),
		"sampling temperature (unset leaves the provider default)")
	flags.Int64Var(&f.seed, "seed", 0, "sampling seed, where the provider supports one")
	flags.StringVar(&f.system, "system", "", "system prompt prepended to every Case")
	flags.StringVar(&f.generationParams, "generation-params", "",
		"override whether this model accepts generation parameters: auto, on, or off")
	flags.BoolVar(&f.useLegacyMaxTokens, "use-legacy-max-tokens", false,
		"send max_tokens instead of max_completion_tokens, for older self-hosted servers")
	flags.DurationVar(&f.timeout, "timeout", 0, "per-call deadline")
	// A PAIR. EstimateWithPrice refuses unless both are set, because half a
	// price is not a price.
	flags.Float64Var(&f.priceInPerMTok, "price-input-per-mtok", 0,
		"input price per million tokens, for a model with no table row (pairs with --price-output-per-mtok)")
	flags.Float64Var(&f.priceOutPerMTok, "price-output-per-mtok", 0,
		"output price per million tokens (needs --price-input-per-mtok)")
	flags.BoolVar(&f.acceptUnknownCost, "accept-unknown-cost", false,
		"run a model whose per-Case cost cannot be computed")
	// Local only. Exporting to a collector over OTLP is the v0.3 half of this
	// (DESIGN.md:399) and costs ten more dependency modules including gRPC.
	flags.BoolVar(&f.traceSpans, "trace-spans", false,
		"write OpenTelemetry spans for this run to stderr")

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

// validateCaps refuses budget flags that would disable the thing they name.
func (f baselineFlags) validateCaps() error {
	switch {
	case f.maxCostUSD < 0:
		return errs.ErrInvalidInput.WithFix("pass a positive --max-cost-usd, or omit it for no cap").
			Wrap(fmt.Errorf("--max-cost-usd is %.2f; a negative cap would disable the limit, not tighten it", f.maxCostUSD))
	case f.maxCalls < 0:
		return errs.ErrInvalidInput.WithFix("pass a positive --max-calls, or omit it for no cap").
			Wrap(fmt.Errorf("--max-calls is %d; a negative cap would disable the limit, not tighten it", f.maxCalls))
	case (f.priceInPerMTok > 0) != (f.priceOutPerMTok > 0):
		// A PAIR. EstimateWithPrice needs both terms, and half a price
		// produces an estimate wrong in the direction that UNDER-reserves —
		// a cap that does not bind. Checked at the flag rather than inside the
		// adapter constructors, so a typo is refused whichever scheme it is
		// paired with rather than only where an override is consumed.
		return errs.ErrInvalidInput.WithFix(
			"pass both --price-input-per-mtok and --price-output-per-mtok",
		).
			Wrap(fmt.Errorf("a price override needs an input and an output rate; got %.4f and %.4f",
				f.priceInPerMTok, f.priceOutPerMTok))
	case f.priceInPerMTok < 0 || f.priceOutPerMTok < 0:
		return errs.ErrInvalidInput.WithFix(
			"pass positive per-million-token prices",
		).
			Wrap(fmt.Errorf("a negative price would credit the budget on every call"))
	case f.costPerCall < 0:
		return errs.ErrInvalidInput.WithFix("pass a positive --cost-per-call-usd").
			Wrap(fmt.Errorf("--cost-per-call-usd is %.2f; a negative estimate would credit the budget on every call", f.costPerCall))
	}
	return nil
}

func runBaseline(ctx context.Context, out, errOut io.Writer, f baselineFlags) error {
	// Before anything that could emit a span. Spans written to stderr, never
	// stdout: stdout is the report, and --json makes it a machine contract
	// that a span document would corrupt — measured once already with a
	// one-line consent notice.
	// errOut, not os.Stderr: CLAUDE.md forbids reaching for the process
	// streams outside tui/ (the lint bundle enforces it), and taking the
	// writer cobra already holds is also what makes this testable without
	// capturing a global.
	stopTracing, err := startTracing(ctx, errOut, f.traceSpans)
	if err != nil {
		return err
	}
	defer stopTracing()

	evals, err := jsonl.New(jsonl.Options{
		Path:        f.evalsPath,
		HoldoutFrac: f.holdoutFrac,
		SplitSeed:   f.splitSeed,
	})
	if err != nil {
		return errs.ErrInvalidInput.WithFix(
			"check --evals and --holdout-frac",
		).Wrap(err)
	}

	// Count the split before anything is spent. A run that can never produce a
	// holdout number is refused here rather than discovered at validate, after
	// the money is gone.
	counts, err := evals.CountSplits(ctx)
	if err != nil {
		return errs.ErrInvalidInput.WithFix(
			"fix the reported line, then re-run",
		).Wrap(err)
	}
	if err := counts.Validate(); err != nil {
		return errs.ErrInvalidInput.WithFix(
			"add more cases, or lower --holdout-frac",
		).Wrap(err)
	}

	// A negative cap must not read as an absent one. The guard treats a limit
	// as active only when positive, so --max-cost-usd -1 would sail past the
	// dollar check entirely and spend without a ceiling — the silent-spend
	// failure prime directive 4 exists to prevent, reached by a typo.
	if err := f.validateCaps(); err != nil {
		return err
	}

	agent, agentRef, err := resolveAgent(f)
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
		// An EXPLICIT --cost-per-call-usd 0 is an assertion that the calls are
		// free, which is the only thing a local model server can honestly say.
		// Read from Changed rather than from the value, because 0 is also the
		// default and the two must not mean the same thing: the documented
		// local-server recipe passed --cost-per-call-usd 0 and was refused
		// with a fix line naming the flag it had just passed.
		AcceptUnknownCost: f.acceptUnknownCost || f.costPerCallSet,
	}

	// Printed BEFORE the run, unconditionally, when the user waived the
	// prompt. --yes is currently the only usable invocation against a real
	// provider — the interactive path declines by default until the TUI lands
	// (docs/debt.md#59) — so a blanket flag is the whole consent surface, and
	// it must at least say what it is agreeing to. In the scrollback and in
	// the CI log, a figure that turns out wrong is evidence rather than a
	// mystery.
	//
	// Not from the ConfirmFunc: PreConfirm short-circuits below the threshold
	// and never calls it, so that version was silent for exactly the runs
	// small enough not to prompt.
	// Human output only. In --json mode stdout is a machine contract, and a
	// prose line ahead of the document makes it unparseable — measured, as
	// `invalid character 'P' looking for beginning of value`. The figure
	// travels in the report instead, as estimated_usd.
	if f.yes && !f.jsonOut {
		if err := printEstimate(out, opts, counts.Dev); err != nil {
			return err
		}
	}

	res, runErr := core.Baseline(ctx, core.Seal(evals), opts)
	if res == nil {
		return runErr
	}

	renderErr := render(out, f, opts, res, counts, runID)
	// The run's own error wins. Rendering happens first so a budget stop or an
	// interruption still shows what it accomplished — but if stdout is a
	// closed pipe, reporting THAT instead would exit 1 ("broken") for a run
	// that in fact stopped exactly as configured, which is the misclassification
	// the exit-code grammar exists to avoid.
	if runErr != nil {
		return runErr
	}
	if renderErr != nil {
		return renderErr
	}
	return nil
}
