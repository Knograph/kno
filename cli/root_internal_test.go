package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
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
