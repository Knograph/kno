package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/stats/budget"
)

// ptyPair opens a pty and returns (master, slave); both are closed by
// cleanup. The slave is what the dialog sees: a terminal on stdin AND stdout.
func ptyPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatalf("opening pty: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = s.Close() })
	return m, s
}

// dialogIntent is the shape of intent that crosses the $1.00 threshold:
// $5.00 over 10 calls, under a $10.00 cap, so the bounded figure is $5.00.
var dialogIntent = budget.Estimate{Calls: 10, CostUSDMicros: 5_000_000}

var dialogLimits = budget.Limits{MaxCostUSDMicros: 10_000_000}

// signalWriter records that a byte sequence reached the dialog's output,
// under a mutex so a test can wait on it without racing the dialog's writes.
type signalWriter struct {
	mu     sync.Mutex
	w      io.Writer
	needle string
	seen   bool
}

func (s *signalWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	if bytes.Contains(p, []byte(s.needle)) {
		s.seen = true
	}
	s.mu.Unlock()
	return s.w.Write(p)
}

func (s *signalWriter) got() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// waitFor blocks until the writer saw the needle or the deadline passes.
func (s *signalWriter) waitFor(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.got() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q to reach the dialog's output", s.needle)
}

func TestShouldPromptRequiresBothEnds(t *testing.T) {
	t.Run("both ends terminals", func(t *testing.T) {
		_, s := ptyPair(t)
		if !shouldPrompt(s, s) {
			t.Error("shouldPrompt(slave, slave) = false, want true")
		}
	})
	t.Run("stdin not a terminal", func(t *testing.T) {
		_, s := ptyPair(t)
		var buf bytes.Buffer
		if shouldPrompt(&buf, s) {
			t.Error("shouldPrompt(buffer, slave) = true, want false")
		}
	})
	t.Run("stdout not a terminal", func(t *testing.T) {
		_, s := ptyPair(t)
		var buf bytes.Buffer
		if shouldPrompt(s, &buf) {
			t.Error("shouldPrompt(slave, buffer) = true, want false")
		}
	})
	t.Run("neither is a file", func(t *testing.T) {
		var in, out bytes.Buffer
		if shouldPrompt(&in, &out) {
			t.Error("shouldPrompt(buffer, buffer) = true, want false")
		}
	})
}

func TestConsentDialogYes(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	decision, err := consentDialog(context.Background(), strings.NewReader("y\n"),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if !decision.proceed {
		t.Error("proceed = false, want true")
	}
	recorder.mu.Lock()
	decided, yes := recorder.decided, recorder.yes
	recorder.mu.Unlock()
	if !decided || !yes {
		t.Errorf("recorded = (%v, %v), want (true, true)", decided, yes)
	}
	if !strings.Contains(out.String(), "This run would spend about $5.00") {
		t.Errorf("output = %q, want the bounded quote", out.String())
	}
	if !strings.Contains(out.String(), "Proceed? [y]es, [n]o, [c]hange the cap:") {
		t.Errorf("output = %q, want the prompt", out.String())
	}
}

func TestConsentDialogNo(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	_, err := consentDialog(context.Background(), strings.NewReader("n\n"),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err == nil {
		t.Fatal("declining must error")
	}
	if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d", got, errs.ExitBudgetStopped)
	}
	if !strings.Contains(err.Error(), "nothing was spent") {
		t.Errorf("error = %q, want it to say nothing was spent", err.Error())
	}
	recorder.mu.Lock()
	decided := recorder.decided
	recorder.mu.Unlock()
	if decided {
		t.Error("a decline must not record a decision the guard would honor")
	}
}

func TestConsentDialogAdjustCap(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	decision, err := consentDialog(context.Background(), strings.NewReader("c\n5\n"),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if !decision.proceed {
		t.Error("proceed = false, want true")
	}
	if got := decision.limits.MaxCostUSDMicros; got != 5_000_000 {
		t.Errorf("MaxCostUSDMicros = %d, want 5000000", got)
	}
	// The SAME bounded figure is re-quoted after the change: one number in
	// one flow. $5.00 is min(intent $5.00, new headroom $5.00).
	if got := strings.Count(out.String(), "This run would spend about $5.00"); got != 2 {
		t.Errorf("bounded figure quoted %d times, want 2:\n%s", got, out.String())
	}
	recorder.mu.Lock()
	decided, yes := recorder.decided, recorder.yes
	recorder.mu.Unlock()
	if !decided || !yes {
		t.Errorf("recorded = (%v, %v), want (true, true)", decided, yes)
	}
}

func TestConsentDialogAdjustAboveNewCap(t *testing.T) {
	// The re-quote is the NEW bounded figure when the cap tightens the intent:
	// $8.00 intent under a $3.00 cap quotes $3.00 after the change — the same
	// bounded figure mechanism, one number in one flow.
	var out bytes.Buffer
	recorder := &consentRecorder{}
	intent := budget.Estimate{Calls: 10, CostUSDMicros: 8_000_000}
	decision, err := consentDialog(context.Background(), strings.NewReader("c\n3\n"),
		&out, intent, budget.Limits{}, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if got := decision.limits.MaxCostUSDMicros; got != 3_000_000 {
		t.Errorf("MaxCostUSDMicros = %d, want 3000000", got)
	}
	got := out.String()
	if !strings.Contains(got, "This run would spend about $8.00") {
		t.Errorf("first quote = %q, want the pre-change intent", got)
	}
	if !strings.Contains(got, "This run would spend about $3.00 ($3.00 remaining)") {
		t.Errorf("re-quote = %q, want the new bounded figure", got)
	}
	if n := strings.Count(got, "This run would spend about"); n != 2 {
		t.Errorf("quotes shown %d times, want exactly 2:\n%s", n, got)
	}
}

func TestConsentDialogInvalidCapReprompts(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	// An invalid cap returns to the main prompt, so each attempt needs its
	// own "c"; "abc" and "-1" are refused and the cap question repeats, and
	// "5" lands.
	decision, err := consentDialog(context.Background(),
		strings.NewReader("c\nabc\nc\n-1\nc\n5\n"), &out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if !decision.proceed || decision.limits.MaxCostUSDMicros != 5_000_000 {
		t.Errorf("decision = (%v, %d), want (true, 5000000)", decision.proceed, decision.limits.MaxCostUSDMicros)
	}
	if got := strings.Count(out.String(), "New --max-cost-usd cap"); got != 3 {
		t.Errorf("cap prompt shown %d times, want 3:\n%s", got, out.String())
	}
}

func TestConsentDialogUnlimitedCap(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	decision, err := consentDialog(context.Background(), strings.NewReader("c\n0\n"),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if got := decision.limits.MaxCostUSDMicros; got != 0 {
		t.Errorf("MaxCostUSDMicros = %d, want 0 (unlimited)", got)
	}
}

func TestConsentDialogBelowThresholdDoesNotAsk(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	decision, err := consentDialog(context.Background(), strings.NewReader(""),
		&out, budget.Estimate{Calls: 1, CostUSDMicros: 10_000}, dialogLimits, budget.Spend{}, recorder)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}
	if !decision.proceed {
		t.Error("proceed = false, want true for a run that cannot spend a dollar")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none below the threshold", out.String())
	}
	recorder.mu.Lock()
	decided := recorder.decided
	recorder.mu.Unlock()
	if decided {
		t.Error("below the threshold there is nothing to record")
	}
}

func TestConsentDialogEOFDeclines(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	_, err := consentDialog(context.Background(), strings.NewReader(""),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err == nil {
		t.Fatal("EOF at the prompt must decline")
	}
	if got := errs.ExitCodeOf(err); got != errs.ExitBudgetStopped {
		t.Errorf("exit code = %d, want %d", got, errs.ExitBudgetStopped)
	}
}

func TestConsentDialogGarbageRepromptsThenYields(t *testing.T) {
	var out bytes.Buffer
	recorder := &consentRecorder{}
	decision, err := consentDialog(context.Background(), strings.NewReader("maybe\nn\n"),
		&out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	if err == nil {
		t.Fatal("declining after garbage must error")
	}
	if decision.proceed {
		t.Error("proceed = true after a decline")
	}
}

func TestConsentDialogSIGINTIsExitFour(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	defer func() { _ = pr.CloseWithError(io.ErrClosedPipe) }() // unblock the readLine goroutine

	var out bytes.Buffer
	recorder := &consentRecorder{}
	done := make(chan struct{})
	var dialogErr error
	go func() {
		defer close(done)
		_, dialogErr = consentDialog(ctx, pr, &out, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	}()

	// The dialog prints the quote and blocks on the read; cancellation lands
	// in readLine's select and surfaces as ErrInterrupted.
	cancel()
	<-done
	if dialogErr == nil {
		t.Fatal("cancellation at the prompt must error")
	}
	if got := errs.ExitCodeOf(dialogErr); got != errs.ExitInterrupted {
		t.Errorf("exit code = %d, want %d", got, errs.ExitInterrupted)
	}
	_ = pw.Close()
}

func TestConsentDialogSIGINTWhileTypingCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	defer func() { _ = pr.CloseWithError(io.ErrClosedPipe) }()

	// The first line ("c") lands; the dialog asks for the cap and blocks.
	go func() {
		_, _ = pw.Write([]byte("c\n"))
	}()

	sw := &signalWriter{w: io.Discard, needle: "New --max-cost-usd cap"}
	recorder := &consentRecorder{}
	done := make(chan struct{})
	var dialogErr error
	go func() {
		defer close(done)
		_, dialogErr = consentDialog(ctx, pr, sw, dialogIntent, dialogLimits, budget.Spend{}, recorder)
	}()

	// Wait until the cap prompt reached the output, then interrupt.
	sw.waitFor(t, 5*time.Second)
	cancel()
	<-done
	if dialogErr == nil {
		t.Fatal("cancellation while typing the cap must error")
	}
	if got := errs.ExitCodeOf(dialogErr); got != errs.ExitInterrupted {
		t.Errorf("exit code = %d, want %d", got, errs.ExitInterrupted)
	}
	_ = pw.Close()
}

func TestConsentRecorderFailsClosedWithoutADecision(t *testing.T) {
	recorder := &consentRecorder{}
	yes, err := recorder.confirm(context.Background(), dialogIntent, budget.Remaining{Unlimited: true})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if yes {
		t.Error("confirm = true without a recorded decision: a guard with no dialog in front of it must refuse")
	}
}

func TestQuoteEstimateWidthLine(t *testing.T) {
	t.Run("reduced from a request", func(t *testing.T) {
		est := budget.Estimate{
			CostUSDMicros: 5_000_000,
			Width:         &budget.WidthDecision{Requested: 8, Effective: 2, ReducedReason: "cost-cap"},
		}
		got := quoteEstimate(est, budget.Remaining{CostUSDMicros: 10_000_000})
		if !strings.Contains(got, "This run would spend about $5.00 ($10.00 remaining).") {
			t.Errorf("quote = %q, want the spend line", got)
		}
		if !strings.Contains(got, "It will run at width 2 (asked for 8; cost-cap).") {
			t.Errorf("quote = %q, want the width line naming both numbers", got)
		}
	})

	t.Run("reduced from the default", func(t *testing.T) {
		est := budget.Estimate{
			CostUSDMicros: 5_000_000,
			Width:         &budget.WidthDecision{Requested: 0, Effective: 2, ReducedReason: "cost-cap"},
		}
		got := quoteEstimate(est, budget.Remaining{Unlimited: true})
		if !strings.Contains(got, "(uncapped).") {
			t.Errorf("quote = %q, want the uncapped remainder", got)
		}
		if !strings.Contains(got, "It will run at width 2 (narrowed from the default; cost-cap).") {
			t.Errorf("quote = %q, want the narrowed-from-default width line", got)
		}
	})

	t.Run("no width line when the width was not reduced", func(t *testing.T) {
		est := budget.Estimate{CostUSDMicros: 5_000_000}
		got := quoteEstimate(est, budget.Remaining{CostUSDMicros: 10_000_000})
		if strings.Contains(got, "width") {
			t.Errorf("quote = %q, want no width line for an unreduced run", got)
		}
	})
}

func TestConfirmFuncClosures(t *testing.T) {
	est := budget.Estimate{CostUSDMicros: 5_000_000}
	rem := budget.Remaining{CostUSDMicros: 10_000_000}

	t.Run("yes skips the prompt", func(t *testing.T) {
		var out bytes.Buffer
		recorder := &consentRecorder{}
		yes, err := confirmFunc(&out, true, false, recorder)(context.Background(), est, rem)
		if err != nil || !yes {
			t.Errorf("confirm = (%v, %v), want (true, nil)", yes, err)
		}
	})

	t.Run("json refuses with the quote", func(t *testing.T) {
		var out bytes.Buffer
		recorder := &consentRecorder{}
		_, err := confirmFunc(&out, false, true, recorder)(context.Background(), est, rem)
		if err == nil {
			t.Fatal("json mode must refuse to prompt")
		}
		if !strings.Contains(err.Error(), "This run would spend about $5.00") {
			t.Errorf("error = %q, want the quote", err.Error())
		}
		if !strings.Contains(err.Error(), "pass --yes") {
			t.Errorf("error = %q, want the pass --yes fix", err.Error())
		}
	})

	t.Run("undecided interactive refuses and prints the quote", func(t *testing.T) {
		var out bytes.Buffer
		recorder := &consentRecorder{}
		yes, err := confirmFunc(&out, false, false, recorder)(context.Background(), est, rem)
		if err != nil || yes {
			t.Errorf("confirm = (%v, %v), want (false, nil)", yes, err)
		}
		if !strings.Contains(out.String(), "Re-run with --yes to proceed.") {
			t.Errorf("output = %q, want today's refusal", out.String())
		}
	})

	t.Run("recorded decision is returned without prompting", func(t *testing.T) {
		var out bytes.Buffer
		recorder := &consentRecorder{}
		recorder.record(true)
		yes, err := confirmFunc(&out, false, false, recorder)(context.Background(), est, rem)
		if err != nil || !yes {
			t.Errorf("confirm = (%v, %v), want (true, nil)", yes, err)
		}
		if out.Len() != 0 {
			t.Errorf("output = %q, want none: the dialog already asked", out.String())
		}
	})
}

func TestBoundedQuoteMirrorsPermitted(t *testing.T) {
	t.Run("uncapped limits pass the intent through", func(t *testing.T) {
		got := boundedQuote(dialogIntent, budget.Limits{}, budget.Remaining{Unlimited: true})
		if got != dialogIntent {
			t.Errorf("bounded = %+v, want the intent unchanged", got)
		}
	})
	t.Run("cost ceiling binds to the remaining headroom", func(t *testing.T) {
		intent := budget.Estimate{Calls: 10, CostUSDMicros: 5_000_000}
		limits := budget.Limits{MaxCostUSDMicros: 3_000_000}
		rem := remainingAfter(limits, budget.Spend{CostUSDMicros: 1_000_000})
		got := boundedQuote(intent, limits, rem)
		if got.CostUSDMicros != 2_000_000 {
			t.Errorf("CostUSDMicros = %d, want 2000000 (cap 3M minus 1M settled)", got.CostUSDMicros)
		}
	})
	t.Run("calls bind before cost", func(t *testing.T) {
		intent := budget.Estimate{Calls: 10, CostUSDMicros: 5_000_000}
		limits := budget.Limits{MaxCostUSDMicros: 10_000_000, MaxLLMCalls: 4}
		rem := remainingAfter(limits, budget.Spend{Calls: 1})
		got := boundedQuote(intent, limits, rem)
		if got.Calls != 3 {
			t.Errorf("Calls = %d, want 3 (4 allowed minus 1 settled)", got.Calls)
		}
		if got.CostUSDMicros != 1_500_000 {
			t.Errorf("CostUSDMicros = %d, want 1500000 (per-call $0.50 * 3)", got.CostUSDMicros)
		}
	})
}
