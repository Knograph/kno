package exec

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// caseN builds one Case with the ring-0 fields a run needs.
func caseN(i int) *core.Case {
	return &core.Case{
		Id:    fmt.Sprintf("case-%03d", i),
		Input: fmt.Sprintf("question %d", i),
		Split: knov1.Split_SPLIT_DEV,
	}
}

// TestInvokeEchoesInput pins the happy path: stdin carries the Case, stdout
// is the answer, and a free command costs nothing on the Response.
func TestInvokeEchoesInput(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := caseN(1)
	c.Input = "the question"
	resp, err := a.Invoke(context.Background(), c)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Output != "the question" {
		t.Errorf("Output = %q, want the echoed input", resp.Output)
	}
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
	if resp.CostUsdMicros != 0 {
		t.Errorf("CostUsdMicros = %d, want 0 for a free command", resp.CostUsdMicros)
	}
	if resp.UsageEstimated {
		t.Error("UsageEstimated = true, want false for a free command")
	}
}

// TestInvokeFailingScriptIsProviderFailure pins the classification: a nonzero
// exit is an errored Case with the capped stderr as context, NOT a score of
// zero, and it is NOT retryable — core retries only ErrRateLimited and
// ErrTransportTransient, and neither is wrapped.
func TestInvokeFailingScriptIsProviderFailure(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "failing.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := a.Invoke(context.Background(), caseN(1))
	if err == nil {
		t.Fatal("Invoke succeeded, want an error")
	}
	if !errors.Is(err, ErrFailed) {
		t.Errorf("err = %v, want ErrFailed", err)
	}
	if !strings.Contains(err.Error(), "status 2") {
		t.Errorf("error does not carry the exit status: %v", err)
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error does not carry the capped stderr context: %v", err)
	}
	if resp == nil {
		t.Fatal("no Response for the errored Case")
	}
	if !strings.Contains(resp.Error, "division by zero") {
		t.Errorf("Response.Error = %q, want the stderr context", resp.Error)
	}
	if errors.Is(err, errs.ErrRateLimited) {
		t.Error("a failing script must not be classified as rate-limited (retryable)")
	}
	if errors.Is(err, errs.ErrTransportTransient) {
		t.Error("a failing script must not be classified as transport-transient (retryable)")
	}
}

// TestInvokeOutputCapTruncatesAndErrors pins the plan's edge-table row:
// output beyond the cap is truncated and counted, and the Case is errored
// with the cap in the fix line.
func TestInvokeOutputCapTruncatesAndErrors(t *testing.T) {
	t.Parallel()
	const cap = 1024
	a, err := New(Options{Command: "sh " + fixture(t, "huge.sh"), OutputCapBytes: cap})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := a.Invoke(context.Background(), caseN(1))
	if err == nil {
		t.Fatal("Invoke succeeded, want ErrOutputTooLarge")
	}
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("err = %v, want ErrOutputTooLarge", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(cap)) {
		t.Errorf("fix line does not name the cap: %v", err)
	}
	if len(resp.Output) > cap {
		t.Errorf("Output is %d bytes, want at most the %d cap", len(resp.Output), cap)
	}
	if resp.Output == "" {
		t.Error("Output is empty; the truncated prefix must be counted")
	}
}

// TestInvokeStderrCap pins that stored error context is bounded: a script
// debug-printing its stdin cannot turn Case content into stored error text.
func TestInvokeStderrCap(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "debug-prints-stdin.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const tailMarker = "UNIQUE-TAIL-MARKER-9f3a2"
	input := strings.Repeat("x", 100_000) + tailMarker // ~100KB, unique tail
	c := caseN(1)
	c.Input = input
	resp, err := a.Invoke(context.Background(), c)
	if err == nil {
		t.Fatal("Invoke succeeded, want an error")
	}
	if len(resp.Error) > DefaultStderrCapBytes {
		t.Errorf("stored Error is %d bytes, want at most the %d cap",
			len(resp.Error), DefaultStderrCapBytes)
	}
	if !strings.Contains(err.Error(), "[stderr truncated") {
		t.Errorf("error does not mark the truncation: %v", err)
	}
	if strings.Contains(err.Error(), tailMarker) {
		t.Error("the tail of the Case input reached the stored error text; the stderr cap is a boundary")
	}
}

// TestInvokeTimeoutReturnsErroredCase pins the per-call deadline: a hung
// script becomes an errored Case (ErrTimedOut), never run-fatal, and the
// error is not retryable.
func TestInvokeTimeoutReturnsErroredCase(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "hung.sh"), Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	resp, err := a.Invoke(context.Background(), caseN(1))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Invoke succeeded, want ErrTimedOut")
	}
	if !errors.Is(err, ErrTimedOut) {
		t.Errorf("err = %v, want ErrTimedOut", err)
	}
	if resp == nil {
		t.Fatal("no Response for the timed-out Case")
	}
	if elapsed > 10*time.Second {
		t.Errorf("Invoke took %s; the kill sequence must bound the hang", elapsed)
	}
	if errors.Is(err, errs.ErrRateLimited) || errors.Is(err, errs.ErrTransportTransient) {
		t.Error("a hung script must not be retryable")
	}
}

// TestInvokeCancelKillsProcessGroup is the fixture the plan calls out: a
// script with its own background CHILD must die as a GROUP, so neither the
// parent nor the child keeps ticking after cancellation.
func TestInvokeCancelKillsProcessGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parentTicks := dir + "/parent-ticks"
	childTicks := dir + "/child-ticks"
	a, err := New(Options{
		Command: "sh " + fixture(t, "hang-parent-with-child.sh") + " " +
			parentTicks + " " + childTicks,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	_, err = a.Invoke(ctx, caseN(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if parentTicks == "" || childTicks == "" {
		t.Fatal("no tick files; the fixture did not run")
	}
	parentBefore := tickCount(t, parentTicks)
	childBefore := tickCount(t, childTicks)
	if parentBefore == 0 || childBefore == 0 {
		t.Errorf("the fixture did not tick before the kill (parent %d, child %d)",
			parentBefore, childBefore)
	}
	// Give a surviving process time to write more ticks, then prove silence.
	time.Sleep(400 * time.Millisecond)
	if parentAfter := tickCount(t, parentTicks); parentAfter != parentBefore {
		t.Errorf("the PARENT kept ticking after cancellation (%d to %d); "+
			"the group must be killed, not just the direct child", parentBefore, parentAfter)
	}
	if childAfter := tickCount(t, childTicks); childAfter != childBefore {
		t.Errorf("the CHILD kept ticking after cancellation (%d to %d); "+
			"the background process must die with its parent's group", childBefore, childAfter)
	}
}

// TestInvokeCancelReturnsUnwrapped pins the resume contract: a run shutdown
// returns the parent's context error UNWRAPPED, so core records nothing and
// --resume picks the Case up without paying twice. The sentinel exists, so
// the script is truly hung when the cancellation lands; without it the
// fixture would answer before the cancel and the test would prove nothing.
func TestInvokeCancelReturnsUnwrapped(t *testing.T) {
	t.Parallel()
	sentinel := t.TempDir() + "/sentinel"
	if err := os.WriteFile(sentinel, []byte("armed"), 0o600); err != nil {
		t.Fatalf("arming the sentinel: %v", err)
	}
	a, err := New(Options{Command: "sh " + fixture(t, "hang-once-then-fast.sh") + " " + sentinel})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if _, err := a.Invoke(ctx, caseN(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled unwrapped", err)
	}
}

// TestInvokePreCancelledContextPinsRunShutdown: a run already shutting down
// must not spawn a process at all.
func TestInvokePreCancelledContextPinsRunShutdown(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Invoke(ctx, caseN(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestInvokeNilCasePins the nil guard.
func TestInvokeNilCasePins(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Invoke(context.Background(), nil); err == nil {
		t.Fatal("Invoke(nil) succeeded, want an error")
	}
}

// TestInvokeEnvAllowlistPinsTheBoundary is the plan's central security test:
// the child sees the allowlist plus the grants — a key exported in the parent
// is not visible to the child.
//
// The one imprecision: the fixture runs through sh, and sh adds PWD, SHLVL,
// and `_` of its own to the env(1) it spawns. Those three are shell
// artifacts, not inherited parent state, and are allowed explicitly; the
// exact-set pin lives in TestBuildEnvAllowlist, where no shell sits between
// the map and the child.
func TestInvokeEnvAllowlistPinsTheBoundary(t *testing.T) {
	t.Setenv("KNO_EXEC_PARENT_SECRET", "s3cr3t")
	a, err := New(Options{
		Command: "sh " + fixture(t, "env.sh"),
		Env:     []string{"KNO_EXEC_GRANT=hello"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := a.Invoke(context.Background(), caseN(1))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	shellArtifacts := map[string]bool{"PWD": true, "SHLVL": true, "_": true}
	got := map[string]string{}
	for _, line := range strings.Split(resp.Output, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	want := map[string]string{"PATH": os.Getenv("PATH"), "KNO_EXEC_GRANT": "hello"}
	if v, ok := os.LookupEnv("HOME"); ok {
		want["HOME"] = v
	}
	if v, ok := os.LookupEnv("TMPDIR"); ok {
		want["TMPDIR"] = v
	}
	if _, present := got["KNO_EXEC_PARENT_SECRET"]; present {
		t.Error("a parent-exported key reached the child; the allowlist is a boundary")
	}
	for k := range got {
		if _, wanted := want[k]; !wanted && !shellArtifacts[k] {
			t.Errorf("child env contains %q, which neither the allowlist nor the grants grant", k)
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("child env %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestInvokeArgsArriveLiterally pins the split rule end to end: a quoted
// argument reaches the child as literal characters, quote characters and all.
func TestInvokeArgsArriveLiterally(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: `sh ` + fixture(t, "args.sh") + ` "hello world"`})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := a.Invoke(context.Background(), caseN(1))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Output != "arg1=\"hello\narg2=world\"\n" {
		t.Errorf("Output = %q, want the arguments literally, quotes included", resp.Output)
	}
}

// TestSpendsPinsCostDeclaration: Spends is true exactly when a cost was
// declared, and zero is a declaration that the command is free.
func TestSpendsPinsCostDeclaration(t *testing.T) {
	t.Parallel()
	free, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if free.Spends() {
		t.Error("Spends() = true for a zero-cost command; free must be declarable")
	}
	costed, err := New(Options{Command: "sh " + fixture(t, "good.sh"), CostPerCallUSDMicros: 12345})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !costed.Spends() {
		t.Error("Spends() = false for a declared cost; the consent prompt must fire")
	}
}

// TestInvokeStampsDeclaredCost pins the honest-reporting contract: with a
// declared cost, every Response — success and failure alike — carries the
// scalar with UsageEstimated set.
func TestInvokeStampsDeclaredCost(t *testing.T) {
	t.Parallel()
	const cost = 12345
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh"), CostPerCallUSDMicros: cost})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := a.Invoke(context.Background(), caseN(1))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.CostUsdMicros != cost {
		t.Errorf("CostUsdMicros = %d, want the declared %d", resp.CostUsdMicros, cost)
	}
	if !resp.UsageEstimated {
		t.Error("UsageEstimated = false, want true for a declared cost")
	}

	failing, err := New(Options{
		Command: "sh " + fixture(t, "failing.sh"), CostPerCallUSDMicros: cost,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	failResp, _ := failing.Invoke(context.Background(), caseN(2))
	if failResp == nil {
		t.Fatal("no Response for the failing Case")
	}
	if failResp.CostUsdMicros != cost {
		t.Errorf("errored Response CostUsdMicros = %d, want the declared %d",
			failResp.CostUsdMicros, cost)
	}
}

// TestCapabilitiesDeclareNothing pins the plan: exec declares NO capabilities
// in v0.1, so the Value stage refuses exec arms for injected measurement.
func TestCapabilitiesDeclareNothing(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := a.Capabilities()
	if caps.ContextInject {
		t.Error("ContextInject declared; the plan pins it NOT declared in v0.1")
	}
	if caps.KnowledgeWrite || caps.Stream || caps.TokenCounts || caps.GenerationParams {
		t.Error("a capability beyond ContextInject is declared; exec supports none in v0.1")
	}
}

// TestExecutorConformance drives the executor's guarantees through a real
// subprocess per Case — the harness the fake and anthropic adapters run,
// applied to the one adapter whose work is a process lifecycle.
func TestExecutorConformance(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh " + fixture(t, "good.sh")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const cases = 40
	var (
		mu       sync.Mutex
		seenIDs  = map[string]int{}
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	work := func(ctx context.Context, c *core.Case) (*knov1.Response, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return a.Invoke(ctx, c)
	}

	sink := func(_ context.Context, r executor.Result[*core.Case, knov1.Response]) error {
		mu.Lock()
		defer mu.Unlock()
		seenIDs[r.Item.GetId()]++
		return nil
	}

	const concurrency = 4
	stats, err := executor.Run(t.Context(), casesSeq(cases), work, sink, executor.Options{
		Concurrency: concurrency,
		ID:          func(item any) string { return item.(*core.Case).GetId() },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Dispatched != cases {
		t.Errorf("dispatched %d of %d Cases", stats.Dispatched, cases)
	}
	if stats.Recorded() != cases {
		t.Errorf("recorded %d outcomes for %d Cases; an unrecorded outcome is one a "+
			"resume pays for again", stats.Recorded(), cases)
	}
	if stats.Succeeded+stats.Failed != stats.Dispatched {
		t.Errorf("succeeded+failed = %d, dispatched = %d; the two must partition",
			stats.Succeeded+stats.Failed, stats.Dispatched)
	}
	if stats.Failed != 0 {
		t.Errorf("%d Cases failed against the good fixture", stats.Failed)
	}
	if len(seenIDs) != cases {
		t.Errorf("%d distinct Cases reached the sink, want %d", len(seenIDs), cases)
	}
	for id, n := range seenIDs {
		if n != 1 {
			t.Errorf("%s was recorded %d times; a duplicate inflates the denominator "+
				"behind every later delta", id, n)
		}
	}
	if got := peak.Load(); got > concurrency {
		t.Errorf("peak concurrency %d exceeded the bound of %d", got, concurrency)
	}
}

// casesSeq yields n Cases, honoring the ring-0 iterator contract.
func casesSeq(n int) iter.Seq2[*core.Case, error] {
	return func(yield func(*core.Case, error) bool) {
		for i := range n {
			if !yield(caseN(i), nil) {
				return
			}
		}
	}
}

// tickCount counts lines in a tick file, tolerating its absence (0).
func tickCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}
