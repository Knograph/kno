package cli

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
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
