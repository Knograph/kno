package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
)

// The pre-run spend dialog (docs/debt.md#59).
//
// The consent surface lives HERE, in the CLI, and runs before any of the run
// is authorized. The engine's PreConfirm stays as the fail-closed backstop for
// callers that construct a guard without this dialog in front of them — the
// CLI's ConfirmFunc returns the decision this dialog recorded, so nothing
// prompts twice.

// shouldPrompt reports whether both ends of the prompt are terminals.
//
// Both directions, because `kno baseline < /dev/null` (stdin not a TTY) must
// not hang and `kno baseline | tee` (stdout not a TTY) must not pollute the
// pipe. GHA is not the whole CI story — some runners allocate a pty — so this
// is explicit, not "CI means no TTY".
func shouldPrompt(in io.Reader, out io.Writer) bool {
	inFile, inOK := in.(*os.File)
	if !inOK {
		return false
	}
	outFile, outOK := out.(*os.File)
	if !outOK {
		return false
	}
	return isatty.IsTerminal(inFile.Fd()) && isatty.IsTerminal(outFile.Fd())
}

// consentRecorder is the channel between the pre-run dialog and the guard's
// ConfirmFunc: the dialog records what the human decided, and the closure
// returns it without prompting again.
type consentRecorder struct {
	mu      sync.Mutex
	decided bool
	yes     bool
}

// confirm returns the recorded decision, or fails closed when none was
// recorded — a guard driven without the pre-run dialog (or a run that crossed
// the threshold mid-flight) refuses rather than silently proceeding.
func (r *consentRecorder) confirm(_ context.Context, _ budget.Estimate, _ budget.Remaining) (bool, error) {
	r.mu.Lock()
	decided, yes := r.decided, r.yes
	r.mu.Unlock()
	if decided {
		return yes, nil
	}
	// Fail closed, with today's exact non-TTY message: a printed quote and a
	// refusal. Nothing here prompts — prompting is the dialog's job, and a
	// guard that reaches this closure without a recorded decision has no
	// human at the prompt.
	return false, nil
}

// consentDecision is what the dialog decided.
type consentDecision struct {
	// proceed is false when the human declined.
	proceed bool

	// limits are the caps the run will enforce: the original ones on plain
	// yes, the adjusted ones after yes-with-adjusted-cap.
	limits budget.Limits
}

// consentDialog shows the bounded spend figure and asks yes / no /
// yes-with-adjusted-cap.
//
// The quote is the BOUNDED figure — on a fresh run, the core-estimated total;
// on a resume, the remainder after SettledSpend — computed here by the same
// arithmetic PreConfirm uses (permitted and remainingLocked, mirrored below).
// The two cannot disagree because both read the same persisted spend.
//
// The guard is NOT rebuilt here: the caller has the pieces (the confirm
// closure, the threshold), and the decision returns the limits to rebuild
// with.
//
// Returns ErrInterrupted (exit 4) when the context is cancelled at the prompt
// — a SIGINT before any spend, resumable like any interruption. A prompt
// failure — EOF, an unreadable line — fails closed as the same exit-2 refusal
// a decline produces.
func consentDialog(
	ctx context.Context,
	in io.Reader,
	out io.Writer,
	intent budget.Estimate,
	limits budget.Limits,
	settled budget.Spend,
	recorder *consentRecorder,
) (consentDecision, error) {
	decision := consentDecision{proceed: true, limits: limits}

	// The bounded quote, computed the way PreConfirm would.
	rem := remainingAfter(limits, settled)
	est := boundedQuote(intent, limits, rem)

	// The bounded figure is what the threshold compares against, matching
	// PreConfirm: a run that cannot spend more than the threshold does not
	// ask about it.
	if est.CostUSDMicros < usdToMicros(confirmThresholdUSD) {
		return decision, nil
	}

	reader := bufio.NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return decision, errs.ErrInterrupted.Wrap(
				fmt.Errorf("interrupted at the confirmation prompt before anything was spent"),
			)
		}
		if _, err := io.WriteString(out, "\n"+quoteEstimate(est, rem)+"\n"); err != nil {
			return decision, promptFailure(err)
		}
		if _, err := io.WriteString(out, "Proceed? [y]es, [n]o, [c]hange the cap: "); err != nil {
			return decision, promptFailure(err)
		}

		answer, err := readLine(ctx, reader)
		if err != nil {
			if ctx.Err() != nil {
				return decision, errs.ErrInterrupted.Wrap(
					fmt.Errorf("interrupted at the confirmation prompt before anything was spent"),
				)
			}
			// EOF or a read error is a prompt failure: fail closed as the
			// same exit-2 refusal a decline produces.
			return consentDecision{proceed: false, limits: decision.limits}, declined()
		}

		switch normalize(answer) {
		case "y", "yes":
			recorder.record(true)
			return decision, nil
		case "n", "no":
			return consentDecision{proceed: false, limits: decision.limits}, declined()
		case "c", "change":
			// Rebuild the caps around the human's number, then re-quote the
			// SAME bounded figure against the new headroom — one number in
			// one flow. The changed cap is consent: the human said yes to
			// the figure that number admits.
			if _, err := io.WriteString(out, "New --max-cost-usd cap (0 for unlimited): "); err != nil {
				return decision, promptFailure(err)
			}
			capLine, err := readLine(ctx, reader)
			if err != nil {
				if ctx.Err() != nil {
					return decision, errs.ErrInterrupted.Wrap(
						fmt.Errorf("interrupted at the confirmation prompt before anything was spent"),
					)
				}
				return decision, promptFailure(err)
			}
			newCap, err := parseCapUSD(capLine)
			if err != nil {
				// Invalid input re-prompts; only a read failure fails closed.
				continue
			}
			decision.limits.MaxCostUSDMicros = usdToMicros(newCap)
			rem = remainingAfter(decision.limits, settled)
			est = boundedQuote(intent, decision.limits, rem)
			// The re-quote of the SAME bounded figure: one number in one
			// flow. The human agreed to this figure by giving the cap.
			if _, err := io.WriteString(out, "\n"+quoteEstimate(est, rem)+"\n"); err != nil {
				return decision, promptFailure(err)
			}
			recorder.record(true)
			return decision, nil
		default:
			// Unrecognized input re-prompts; the loop reprints the quote.
			continue
		}
	}
}

func (r *consentRecorder) record(yes bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decided = true
	r.yes = yes
}

// readLine reads one line from the prompt, or reports the context first.
//
// A blocked read cannot be interrupted by the signal handler alone, so the
// read happens on a goroutine and the select surfaces the context's
// cancellation. The goroutine stays blocked until the stream closes or a byte
// arrives; in the CLI process that is fine (the process exits right after),
// and in tests the stream is closed in cleanup so goleak stays quiet.
func readLine(ctx context.Context, r *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// parseCapUSD turns the human's number into dollars, refusing negatives and
// trailing garbage.
func parseCapUSD(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cap")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number")
	}
	if v < 0 {
		return 0, fmt.Errorf("negative cap")
	}
	return v, nil
}

// normalize folds an answer for matching.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// declined is the exit-2 refusal, the same error PreConfirm's own decline
// produces.
func declined() error {
	return errs.ErrBudgetExceeded.WithFix(
		"re-run with --yes to proceed, or lower --max-cost-usd",
	).
		Wrap(fmt.Errorf("the run was declined at the confirmation prompt; nothing was spent"))
}

// promptFailure fails closed as the same exit-2 refusal, naming what broke.
func promptFailure(err error) error {
	return errs.ErrBudgetExceeded.WithFix(
		"re-run with --yes to proceed, or lower --max-cost-usd",
	).
		Wrap(fmt.Errorf("the confirmation prompt failed; the run was not confirmed: %w", err))
}

// saturatingMul multiplies without wrapping, mirroring core's own guard: an
// overflowed product goes negative and sails past the cap clamp and under the
// confirmation threshold, so PreConfirm silently returns true — a consent
// path skipped by arithmetic.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// remainingAfter is the headroom a fresh or resumed run has, mirroring the
// Guard's remainingLocked before any of this run's spend exists.
//
// settled is the store's SettledSpend for the run — zero on a fresh run, the
// recorded spend on a resume. The CLI reads it so the quote shows the same
// remainder PreConfirm would.
func remainingAfter(limits budget.Limits, settled budget.Spend) budget.Remaining {
	if limits.MaxCostUSDMicros == 0 && limits.MaxLLMCalls == 0 {
		return budget.Remaining{Unlimited: true}
	}
	rem := budget.Remaining{}
	if limits.MaxCostUSDMicros > 0 {
		rem.CostUSDMicros = max(0, limits.MaxCostUSDMicros-settled.CostUSDMicros)
	}
	if limits.MaxLLMCalls > 0 {
		rem.LLMCalls = max(0, limits.MaxLLMCalls-settled.Calls)
	}
	return rem
}

// boundedQuote bounds an intent by what the caps admit, mirroring
// stats/budget.permitted exactly.
//
// The dialog must show the same figure the guard will stop at; a divergence
// here would quote one number and enforce another. Calls first, then cost —
// the same order permitted uses.
func boundedQuote(intent budget.Estimate, limits budget.Limits, rem budget.Remaining) budget.Estimate {
	est := intent
	if limits.MaxLLMCalls > 0 && est.Calls > rem.LLMCalls {
		var perCall int64
		if est.Calls > 0 {
			perCall = est.CostUSDMicros / est.Calls
		}
		est.Calls = rem.LLMCalls
		est.CostUSDMicros = perCall * rem.LLMCalls
	}
	if limits.MaxCostUSDMicros > 0 {
		ceiling := min(rem.CostUSDMicros, limits.MaxCostUSDMicros)
		if est.CostUSDMicros > ceiling {
			est.CostUSDMicros = ceiling
		}
	}
	return est
}

// quoteEstimate renders the spend figure and, when the engine narrowed the
// width, the width line (#44's prompt half).
//
// The width line is quoted from the estimate PreConfirm carries, so it appears
// wherever a quote is produced after checkFeasible has run — the fail-closed
// backstop and the --json refusal. The pre-run dialog runs before the engine
// made its feasibility decision, so its quote carries no Width yet; the run
// then reports the narrowed width in its own line.
func quoteEstimate(est budget.Estimate, rem budget.Remaining) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This run would spend about %s", formatUSD(est.CostUSDMicros))
	if rem.Unlimited {
		b.WriteString(" (uncapped).")
	} else {
		fmt.Fprintf(&b, " (%s remaining).", formatUSD(rem.CostUSDMicros))
	}
	if w := est.Width; w != nil && w.ReducedReason != "" {
		if w.Requested > 0 {
			fmt.Fprintf(&b, "\nIt will run at width %d (asked for %d; %s).",
				w.Effective, w.Requested, w.ReducedReason)
		} else {
			fmt.Fprintf(&b, "\nIt will run at width %d (narrowed from the default; %s).",
				w.Effective, w.ReducedReason)
		}
	}
	return b.String()
}
