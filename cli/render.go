package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/fake"
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

// resolveAgent turns an agent ref into an Agent.
//
// Only the fake is available today. A ref naming a real provider is refused
// with a message saying so, rather than falling back to something that would
// silently produce numbers the user would read as real.
func resolveAgent(ref string) (core.Agent, *knov1.AgentRef, error) {
	// Parse and resolve are separate steps because they answer different
	// questions. Parsing asks whether the reference is well formed; resolving
	// asks whether an adapter exists for it. Merging them means a typo and an
	// unsupported provider produce the same message, and the user cannot tell
	// which one they have.
	parsed, err := agentref.Parse(ref)
	if err != nil {
		return nil, nil, errs.ErrInvalidInput.WithFix(
			"write the reference as scheme:target — openai:gpt-4.1, " +
				"exec:my-agent-command, or fake: for the local agent that costs " +
				"nothing").Wrap(err)
	}

	if parsed.GetScheme() == agentref.SchemeFake {
		return fake.New(fake.Options{}), parsed, nil
	}

	return nil, nil, errs.ErrCapabilityUnsupported.WithFix(
		"only `fake:` is available in this build; provider adapters land in the next milestone").
		Wrap(fmt.Errorf("no adapter for agent ref %q", parsed.GetRef()))
}

// resolveGoal turns a goal name into a Goal.
func resolveGoal(name string) (core.Goal, error) {
	if name == "exact-match" {
		return &exactmatch.Goal{}, nil
	}
	return nil, errs.ErrInvalidInput.WithFix(
		"only `exact-match` is available in this build; judged goals land with the judge").
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
func confirmFunc(out io.Writer, yes, jsonOut bool) budget.ConfirmFunc {
	if yes {
		return func(context.Context, budget.Estimate, budget.Remaining) (bool, error) {
			return true, nil
		}
	}
	if jsonOut {
		// A machine-readable run has nobody to answer a prompt. Refusing is the
		// safe default: proceeding would spend money with no one watching.
		return func(_ context.Context, est budget.Estimate, _ budget.Remaining) (bool, error) {
			return false, fmt.Errorf(
				"this run would spend about %s and --json cannot prompt; pass --yes to proceed",
				formatUSD(est.CostUSDMicros))
		}
	}
	return func(_ context.Context, est budget.Estimate, rem budget.Remaining) (bool, error) {
		// Non-interactive by default in this build: the huh prompt lands with
		// the TUI. Refusing is the honest behavior until then — a run that
		// silently proceeded past the threshold would be the surprise bill
		// DESIGN.md forbids.
		msg := fmt.Sprintf("\nThis run would spend about %s (%s remaining).\n"+
			"Re-run with --yes to proceed.\n",
			formatUSD(est.CostUSDMicros), formatUSD(rem.CostUSDMicros))
		if _, err := io.WriteString(out, msg); err != nil {
			return false, fmt.Errorf("writing confirmation: %w", err)
		}
		return false, nil
	}
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
	res *core.BaselineResult,
	counts jsonl.SplitCounts,
	runID string,
) error {
	warnings := warningsFor(res, counts)

	if f.jsonOut {
		return renderJSON(out, f, res, counts, runID, warnings)
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
				"interval at validate", counts.Holdout))
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

	if res.AggregateScore != nil {
		fmt.Fprintf(&b, "  score      %.3f\n", *res.AggregateScore)
	} else {
		b.WriteString("  score      none\n")
	}
	fmt.Fprintf(&b, "  spent      %s over %d call(s)\n",
		formatUSD(res.Spent.CostUSDMicros), res.Spent.Calls)
	fmt.Fprintf(&b, "  status     %s\n", statusName(run.GetStatus()))

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
		return "Next: `kno value` to measure which of your assets earn their place."
	}
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
