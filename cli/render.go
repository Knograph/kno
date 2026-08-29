package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/goal/exactmatch"
	"github.com/knograph/kno/stats/budget"
)

// confirmThresholdUSD is the estimated spend above which a run asks first.
//
// DESIGN.md's rule is that nobody gets a surprise bill. The threshold is low on
// purpose: the cost of an unnecessary prompt is one keystroke, and the cost of
// a missing one is somebody's money.
const confirmThresholdUSD = 1.00

// resolveGoal turns a goal name into a Goal.
func resolveGoal(name string) (core.Goal, error) {
	if name == "exact-match" {
		return &exactmatch.Goal{}, nil
	}
	return nil, errs.ErrInvalidInput.WithFix(
		"only `exact-match` is available in this build; judged goals land with the judge",
	).
		Wrap(fmt.Errorf("no goal named %q", name))
}

// newRunID returns a sortable, unique run identifier.
//
// Time-prefixed so runs sort chronologically in a listing, with random bytes so
// two runs started in the same second cannot collide and silently merge their
// outcomes.
func newRunID(now time.Time) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cannot happen on any supported platform; a failure here would mean
		// the system's entropy source is gone.
		panic(fmt.Sprintf("cli: reading random bytes: %v", err))
	}
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// confirmFunc asks before spending, unless --yes or --json.
//
// The recorder is the pre-run dialog's decision channel: the interactive
// closure returns the recorded answer, so nothing prompts twice. A guard
// driven without the dialog (a non-CLI caller, or a run whose threshold was
// crossed mid-flight) fails closed here — the printed message and refusal
// that were the whole surface before the dialog existed.
func confirmFunc(out io.Writer, yes, jsonOut bool, recorder *consentRecorder) budget.ConfirmFunc {
	if yes {
		// The printing lives in runBaseline, NOT here.
		//
		// PreConfirm short-circuits below the $1.00 threshold and never calls
		// this closure at all, so a --yes run under the threshold printed
		// nothing while the flag's help, the cookbook, the CI recipe, and the
		// plan all promised a figure unconditionally. A consent surface that
		// is silent exactly when the amount is small is still silent.
		return func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
			return true, nil
		}
	}
	if jsonOut {
		// A machine-readable run has nobody to answer a prompt. Refusing is the
		// safe default: proceeding would spend money with no one watching.
		return func(_ context.Context, est budget.Estimate, rem budget.Remaining) (bool, error) {
			return false, fmt.Errorf(
				"%s\n--json cannot prompt; pass --yes to proceed",
				quoteEstimate(est, rem),
			)
		}
	}
	return func(_ context.Context, est budget.Estimate, rem budget.Remaining) (bool, error) {
		// The recorded decision, when the pre-run dialog ran. On yes the
		// guard records the agreement; on no the run was already refused in
		// the dialog, so a declined decision should not be reachable here.
		recorder.mu.Lock()
		decided, yes := recorder.decided, recorder.yes
		recorder.mu.Unlock()
		if decided {
			return yes, nil
		}
		// No decision was recorded: this guard has no dialog in front of it.
		// Fail closed with the quote — which carries the width line when the
		// engine narrowed the run (#44's prompt half) — and today's refusal.
		msg := "\n" + quoteEstimate(est, rem) + "\nRe-run with --yes to proceed.\n"
		if _, err := io.WriteString(out, msg); err != nil {
			return false, fmt.Errorf("writing confirmation: %w", err)
		}
		return false, nil
	}
}

// printEstimate says what a --yes run is about to spend.
//
// Best effort by construction: the engine owns the real arithmetic, and this
// asks it for the same per-Case figure confirmRun would quote. An agent that
// cannot produce one is already refused before this point unless the user
// passed --accept-unknown-cost, and in that case there is genuinely no number
// to print — so it says that rather than printing a zero, which would read as
// "free".
func printEstimate(out io.Writer, opts core.BaselineOptions, devCases int) error {
	perCall := core.PlanningCostPerCall(opts)
	if perCall <= 0 {
		_, err := io.WriteString(out,
			"\nProceeding with --yes: this run's per-Case cost is unknown.\n")
		return err
	}
	total := perCall * int64(devCases)
	_, err := fmt.Fprintf(out,
		"\nProceeding with --yes: this run would spend about %s across %d Cases.\n",
		formatUSD(total), devCases)
	return err
}

// scoredOf and erroredOf read the presence-carrying counts, falling back to
// the flat ones.
//
// The fallback is not decoration. CaseExecution is written from a store READ
// at close, and a read that fails leaves it absent — at which point the
// chained getters return 0 and the report would print "0 scored, 0 errored"
// for a run that scored every Case, with the correct number sitting in the
// flat counter beside it. The flat counters are still written on every path
// and are still correct.
func scoredOf(run *knov1.Run) int32 {
	if ce := run.GetCaseExecution(); ce != nil {
		return ce.GetScoredCaseCount()
	}
	return run.GetScoredCaseCount()
}

func attemptedOf(run *knov1.Run) int32 {
	if ce := run.GetCaseExecution(); ce != nil {
		return ce.GetAttemptedCaseCount()
	}
	return run.GetAttemptedCaseCount()
}

func erroredOf(run *knov1.Run) int32 {
	if ce := run.GetCaseExecution(); ce != nil {
		return ce.GetErroredCaseCount()
	}
	return run.GetErroredCaseCount()
}

// formatUSD renders micro-USD as dollars, for display only.
func formatUSD(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	return fmt.Sprintf("%s$%d.%02d", sign, micros/1_000_000, (micros%1_000_000)/10_000)
}

// render writes the run's result.
func render(
	out io.Writer,
	f baselineFlags,
	opts core.BaselineOptions,
	res *core.BaselineResult,
	counts jsonl.SplitCounts,
	runID string,
) error {
	warnings := warningsFor(res, counts)

	if f.jsonOut {
		return renderJSON(out, f, opts, res, counts, runID, warnings)
	}
	return renderHuman(out, res, counts, runID, warnings)
}

// warningsFor collects the caveats that must travel with a result.
//
// These are not decoration. A number reported without the reason it might be
// wrong is the dishonesty this project's epistemics page exists to prevent.
func warningsFor(res *core.BaselineResult, counts jsonl.SplitCounts) []string {
	var w []string
	if counts.Underpowered() {
		w = append(w, fmt.Sprintf(
			"the holdout has only %d cases, too few for a meaningful confidence "+
				"interval at validate", counts.Holdout,
		))
	}
	if res.Run.GetErrorRateExceeded() {
		w = append(w, "too many cases errored for this to be a usable baseline")
	}
	// Two different absences. Saying "no cases scored" on a run that scored
	// every Case contradicts the count printed three lines above it, and sends
	// the user looking for a failure that did not happen.
	switch {
	case res.AggregateUnavailable:
		w = append(w, "some cases' scores cannot be read back, so this run has "+
			"no baseline number — the cases themselves are intact")
	case res.AggregateScore == nil:
		w = append(w, "no cases scored, so this run has no baseline number")
	}
	return w
}

func renderHuman(
	out io.Writer,
	res *core.BaselineResult,
	counts jsonl.SplitCounts,
	runID string,
	warnings []string,
) error {
	run := res.Run

	// Composed into a buffer, then written once. Writing field by field means
	// a dozen unchecked Fprintf calls, and a partial render on a broken pipe
	// looks like a partial run.
	var b strings.Builder

	fmt.Fprintf(&b, "\nBaseline %s\n", runID)
	fmt.Fprintf(&b, "  cases      %d scored, %d errored (of %d dev; %d held back)\n",
		scoredOf(run), erroredOf(run), counts.Dev, counts.Holdout)
	if n := run.GetWeakLabelCaseCount(); n > 0 {
		// The weak-label marker, printed when nonzero. Source-neutral wording:
		// jsonl/mine marks every Case derived (mined from transcripts), while
		// the langfuse adapter marks derived per item (harvested from a trace)
		// — a weak-label eval set cannot pass for a hand-authored one either
		// way, and the exact-match caveat that applies to derived expectations
		// travels with the number.
		fmt.Fprintf(&b, "  weak-label %d of these Cases carry derived provenance (mined from transcripts or harvested from traces, not authored)\n", n)
	}

	if res.AggregateScore != nil {
		fmt.Fprintf(&b, "  score      %.3f\n", *res.AggregateScore)
	} else {
		b.WriteString("  score      none\n")
	}
	fmt.Fprintf(&b, "  spent      %s over %d call(s)\n",
		formatUSD(res.Spent.CostUSDMicros), res.Spent.Calls)
	fmt.Fprintf(&b, "  status     %s\n", statusName(run.GetStatus()))

	// The width the run actually executed at, but ONLY when the engine chose a
	// smaller one than was asked for.
	//
	// checkFeasible narrows concurrency when the cost cap cannot admit the
	// requested width, and it did so with no event, no log line, and no field
	// on the Run — a 6x slowdown the user did not ask for and could not see.
	// A run that got the width it asked for has no news, so this stays quiet
	// there rather than adding a line to every report.
	//
	// docs/debt.md#44 also names the CONSENT PROMPT as the natural place for
	// this, and that half stays open: budget.ConfirmFunc is
	// func(ctx, Estimate, Remaining) and has no channel for a
	// ConcurrencyDecision, so putting it there means changing a signature in
	// stats/budget — the prime-directive-4 package — and breaking all three
	// CLI closures and any API caller.
	// Gated on the REASON, not on requested != effective. `requested` is
	// presence-carrying and absent means "the user asked for no particular
	// width", so a default run reports requested=0 against an effective 8 —
	// which is the default being applied, not a reduction. The first version
	// of this line printed "width 8 (asked for 0; unspecified)" on every
	// ordinary run.
	if d := run.GetConcurrency(); d.GetReason() != knov1.ConcurrencyReason_CONCURRENCY_REASON_UNSPECIFIED {
		// The "asked for" clause only when the user actually asked. Requested
		// is presence-carrying and absent means "no particular width", so
		// printing it unconditionally told someone who requested nothing that
		// they had requested zero.
		//
		// Fixed HERE rather than by recording the defaulted width as a
		// request: core deliberately does not, and says why — a report that
		// says "you requested 8, we gave you 5" to someone who requested
		// nothing is how a report earns distrust. There is a test pinning it,
		// and making that test pass would have been weakening it.
		if d.Requested != nil {
			fmt.Fprintf(&b, "  width      %d (asked for %d; %s)\n",
				d.GetEffective(), d.GetRequested(), concurrencyReasonName(d.GetReason()))
		} else {
			fmt.Fprintf(&b, "  width      %d (narrowed from the default; %s)\n",
				d.GetEffective(), concurrencyReasonName(d.GetReason()))
		}
	}

	for _, w := range warnings {
		fmt.Fprintf(&b, "\n  warning: %s\n", w)
	}
	if reason := run.GetIncompleteReason(); reason != "" {
		fmt.Fprintf(&b, "\n  %s\n", reason)
	}

	// Every command ends by naming the next one. The CLI teaches the loop by
	// using it.
	fmt.Fprintf(&b, "\n%s\n", nextStep(run.GetStatus()))

	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

// nextStep names what to do now.
func nextStep(status knov1.RunStatus) string {
	switch status {
	case knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED:
		return "Stopped at the budget. Run `kno baseline --resume` to continue where it left off."
	case knov1.RunStatus_RUN_STATUS_INTERRUPTED:
		return "Interrupted. Run `kno baseline --resume` to continue; nothing will be paid for twice."
	case knov1.RunStatus_RUN_STATUS_FAILED:
		return "The run failed. Fix the error above, then re-run."
	default:
		// Names a command that EXISTS. `kno value` is the next stage and is not
		// in this release, and a completed run whose closing line points at a
		// command the binary rejects with "unknown command" is the last thing a
		// first run should print. The line changes when the stage ships, not
		// before — a next step is a promise about this binary, not about the
		// roadmap.
		return "Scores and traces are recorded. `kno purge` removes trace content when you no longer need it."
	}
}

// concurrencyReasonName renders why the engine narrowed the width.
//
// Named rather than numeric, for the same reason statusName is: a jq pipeline
// branching on 1 breaks the day an enum value is inserted, and a person
// reading the JSON should not need the proto to interpret it.
func concurrencyReasonName(r knov1.ConcurrencyReason) string {
	if r == knov1.ConcurrencyReason_CONCURRENCY_REASON_COST_CAP {
		return "cost-cap"
	}
	return "unspecified"
}

func statusName(s knov1.RunStatus) string {
	switch s {
	case knov1.RunStatus_RUN_STATUS_COMPLETED:
		return "completed"
	case knov1.RunStatus_RUN_STATUS_FAILED:
		return "failed"
	case knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED:
		return "budget-stopped"
	case knov1.RunStatus_RUN_STATUS_INTERRUPTED:
		return "interrupted"
	case knov1.RunStatus_RUN_STATUS_RUNNING:
		return "running"
	default:
		return "unknown"
	}
}
