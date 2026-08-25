package cli

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"go.opentelemetry.io/otel"
)

// TestSignalHandlerIsUnregisteredOnTheFirstSignal.
//
// signal.NotifyContext keeps swallowing signals until stop is called. The
// original code deferred stop to the end of Execute, so every Ctrl-C after the
// first was silently eaten for the whole shutdown drain — precisely when a user
// looking at an apparently hung command reaches for it again. The comment above
// it claimed the opposite behavior.
//
// Delivering a real second SIGINT would kill the test process, which is why the
// restoration is a separate function: the observable is that stop runs as soon
// as the context is cancelled, not when Execute returns.
func TestSignalHandlerIsUnregisteredOnTheFirstSignal(t *testing.T) {
	t.Parallel()

	var stopped atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wait := restoreDefaultOnFirstSignal(ctx, func() { stopped.Store(true) })

	if stopped.Load() {
		t.Fatal("the handler was unregistered before any signal arrived")
	}

	// Stand in for the first Ctrl-C.
	cancel()

	deadline := time.After(2 * time.Second)
	for !stopped.Load() {
		select {
		case <-deadline:
			t.Fatal("the default signal behavior was never restored, so a second " +
				"Ctrl-C during shutdown would be swallowed")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	wait()
}

// TestSignalWatcherExitsWhenNoSignalArrives: the normal-exit path releases the
// goroutine rather than leaking one per invocation.
func TestSignalWatcherExitsWhenNoSignalArrives(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	wait := restoreDefaultOnFirstSignal(ctx, func() { stopped.Store(true); cancel() })

	// What Execute's defer does on a clean exit.
	cancel()
	wait()

	if !stopped.Load() {
		t.Error("the watcher returned without restoring the default behavior")
	}
}

// TestWarningsDistinguishTheTwoEmptyScores.
//
// A nil AggregateScore has meant "nothing scored" since the field existed. It
// now has a second meaning, and reporting the old sentence for the new case
// contradicts the count printed three lines above it in the same report: "20
// scored" beside "no cases scored". The user is sent looking for a run failure
// that did not happen.
func TestWarningsDistinguishTheTwoEmptyScores(t *testing.T) {
	t.Parallel()

	counts := jsonl.SplitCounts{Dev: 20, Holdout: 20}

	tests := []struct {
		name    string
		res     *core.BaselineResult
		want    string
		notWant string
	}{
		{
			name: "nothing scored",
			res: &core.BaselineResult{
				Run: &knov1.Run{ScoredCaseCount: 0, ErroredCaseCount: 0},
			},
			want:    "no cases scored",
			notWant: "cannot be read back",
		},
		{
			name: "scored, but the numbers are gone",
			res: &core.BaselineResult{
				Run:                  &knov1.Run{ScoredCaseCount: 20},
				AggregateUnavailable: true,
			},
			want:    "cannot be read back",
			notWant: "no cases scored",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(warningsFor(tt.res, counts), "\n")
			if !strings.Contains(got, tt.want) {
				t.Errorf("warnings = %q, want it to mention %q", got, tt.want)
			}
			if strings.Contains(got, tt.notWant) {
				t.Errorf("warnings = %q, must NOT say %q — it contradicts the case "+
					"count printed beside it", got, tt.notWant)
			}
		})
	}
}

// TestTheCountsFallBackWhenCaseExecutionIsAbsent.
//
// CaseExecution is composed from a store READ at close. A read that fails
// leaves it absent, and the chained getters then return 0 — so the report
// would print "0 scored, 0 errored" for a run that scored every Case, with the
// correct number sitting in the flat counter beside it. Reporting zero work
// for a run that did the work is worse than reporting nothing: a CI gate reads
// it as a total failure, and a human reads it as a bug in the engine.
//
// The flat counters are written on every path, including that one.
func TestTheCountsFallBackWhenCaseExecutionIsAbsent(t *testing.T) {
	t.Parallel()

	run := &knov1.Run{
		AttemptedCaseCount: 25,
		ScoredCaseCount:    20,
		ErroredCaseCount:   5,
	}

	if got := attemptedOf(run); got != 25 {
		t.Errorf("attempted = %d, want 25", got)
	}
	if got := scoredOf(run); got != 20 {
		t.Errorf("scored = %d, want 20 — the run scored 20 Cases", got)
	}
	if got := erroredOf(run); got != 5 {
		t.Errorf("errored = %d, want 5", got)
	}

	// And when it IS present it wins: it aggregates what is durable, so it is
	// the copy that survives a crash and stays correct across a resume.
	run.CaseExecution = &knov1.CaseExecution{
		AttemptedCaseCount: 40,
		ScoredCaseCount:    36,
		ErroredCaseCount:   4,
	}
	if got := scoredOf(run); got != 36 {
		t.Errorf("scored = %d, want 36 — CaseExecution is the presence-carrying "+
			"copy and must win where the two disagree", got)
	}
	if got := attemptedOf(run); got != 40 {
		t.Errorf("attempted = %d, want 40", got)
	}
	if got := erroredOf(run); got != 4 {
		t.Errorf("errored = %d, want 4", got)
	}
}

// TestStartTracingRestoresThePreviousProvider.
//
// startTracing installs a PROCESS-GLOBAL TracerProvider, and its exporter
// writes to a writer captured at install time. Leaving it installed means
// every later run in the same process keeps exporting into a writer that is
// long gone — a CLI runs once so it would never notice, but a test binary and
// any embedder do.
//
// This is the root cause behind both of this flag's tests passing on spans
// emitted by OTHER tests: 1032 Case spans and 10 run spans landed in a test
// that ran 30 Cases. Serializing the tests hides that; restoring the provider
// fixes it, so this asserts the restore directly rather than through stderr,
// where the leaked spans are invisible by construction (they go to the first
// buffer, not the current one).
func TestStartTracingRestoresThePreviousProvider(t *testing.T) {
	before := otel.GetTracerProvider()

	var buf bytes.Buffer
	stop, err := startTracing(context.Background(), &buf, true)
	if err != nil {
		t.Fatalf("startTracing: %v", err)
	}

	if otel.GetTracerProvider() == before {
		t.Fatal("startTracing installed no provider, so this proves nothing")
	}

	stop()

	if got := otel.GetTracerProvider(); got != before {
		t.Error("startTracing left its provider installed. Every later run in " +
			"this process keeps exporting into a writer captured when this one " +
			"started.")
	}
}

// TestStartTracingIsANoOpWhenOff, so the flag is a real opt-in rather than a
// filter over output that was being produced anyway.
func TestStartTracingIsANoOpWhenOff(t *testing.T) {
	before := otel.GetTracerProvider()

	var buf bytes.Buffer
	stop, err := startTracing(context.Background(), &buf, false)
	if err != nil {
		t.Fatalf("startTracing: %v", err)
	}
	defer stop()

	if otel.GetTracerProvider() != before {
		t.Error("tracing off still installed a provider")
	}
	if buf.Len() != 0 {
		t.Errorf("tracing off wrote %d bytes", buf.Len())
	}
}
