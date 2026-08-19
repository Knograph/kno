package coretest_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/knograph/kno/coretest"
)

// goodIter is a conforming producer: it checks ctx before each yield, defers
// its cleanup inside the closure, and stops when the consumer stops.
func goodIter(probe *coretest.CleanupProbe) coretest.IteratorFactory[int] {
	return func(ctx context.Context) (iter.Seq2[int, error], error) {
		return func(yield func(int, error) bool) {
			// Deferred INSIDE the closure, so an early break still runs it.
			defer probe.Close() //nolint:errcheck // probe.Close cannot fail
			for i := range 5 {
				if err := ctx.Err(); err != nil {
					return
				}
				if !yield(i, nil) {
					return
				}
			}
		}, nil
	}
}

// TestConformIteratorAcceptsAConformingProducer is the baseline: the harness
// must pass something correct, or every other result is meaningless.
func TestConformIteratorAcceptsAConformingProducer(t *testing.T) {
	t.Parallel()

	var probe coretest.CleanupProbe
	coretest.ConformIterator(t, goodIter(&probe))
}

// TestCleanupProbeCatchesAMissingClose demonstrates the check ConformIterator
// cannot make from outside, and that adapters therefore have to make.
func TestCleanupProbeCatchesAMissingClose(t *testing.T) {
	t.Parallel()

	var probe coretest.CleanupProbe

	// A producer that registers cleanup OUTSIDE the closure — the exact
	// mistake the contract warns about. On an early break the closure returns,
	// but nothing runs the cleanup.
	leaky := func(_ context.Context) (iter.Seq2[int, error], error) {
		seq := func(yield func(int, error) bool) {
			for i := range 5 {
				if !yield(i, nil) {
					return
				}
			}
		}
		return seq, nil
	}

	seq, err := leaky(context.Background())
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	for range seq {
		break
	}

	if probe.Closed() {
		t.Fatal("test setup is wrong: the leaky producer should not have closed anything")
	}
	// The probe is what an adapter would assert on; here we confirm it can
	// distinguish the two cases rather than always passing.
	var closed coretest.CleanupProbe
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Closed() {
		t.Error("CleanupProbe did not record a Close; it cannot detect anything")
	}
}

// TestFatalErrorStopsIteration exercises both directions on the consumer rule.
func TestFatalErrorStopsIteration(t *testing.T) {
	t.Parallel()

	t.Run("a conforming consumer stops at the first error", func(t *testing.T) {
		t.Parallel()
		coretest.FatalErrorStopsIteration(t, func(seq iter.Seq2[int, error]) error {
			for _, err := range seq {
				if err != nil {
					return err // stops, as the contract requires
				}
			}
			return nil
		})
	})

	t.Run("a consumer that continues past an error is caught", func(t *testing.T) {
		t.Parallel()

		// Run the check against a deliberately non-conforming consumer inside
		// a nested test, and assert it FAILED. A harness that has never been
		// seen to fail has not been shown to work.
		got := coretest.CheckFatalErrorStops(func(seq iter.Seq2[int, error]) error {
			for range seq {
				continue // deliberately keeps reading past a fatal error
			}
			return nil
		})
		if len(got) == 0 {
			t.Error("the harness passed a consumer that ignored a fatal error")
		}
	})
}

// TestConformIteratorCatchesAGoroutineLeak proves the leak check is live
// rather than decorative.
func TestConformIteratorCatchesAGoroutineLeak(t *testing.T) {
	// Deliberately NOT parallel: goleak takes a process-global census, so a
	// sibling test's goroutines would be indistinguishable from the leak this
	// is trying to detect.

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	leaky := func(_ context.Context) (iter.Seq2[int, error], error) {
		// A prefetch goroutine that outlives the iteration — the realistic
		// shape of this bug in a streaming adapter.
		go func() { <-stop }()
		return func(yield func(int, error) bool) {
			for i := range 5 {
				if !yield(i, nil) {
					return
				}
			}
		}, nil
	}

	got := coretest.CheckIterator(leaky, coretest.LeakCheck())
	if len(got) == 0 {
		t.Error("the harness passed a producer that leaks a goroutine past the iteration")
	}
	t.Logf("violations reported: %v", got)
}

// TestConformIteratorCatchesAnUncancellableProducer proves the cancellation
// check is live.
func TestConformIteratorCatchesAnUncancellableProducer(t *testing.T) {
	t.Parallel()

	ignoresCtx := func(_ context.Context) (iter.Seq2[int, error], error) {
		return func(yield func(int, error) bool) {
			// Never checks ctx.Err(): burns budget after the user hit Ctrl-C.
			for i := range 5000 {
				if !yield(i, nil) {
					return
				}
			}
		}, nil
	}

	got := coretest.CheckIterator(ignoresCtx)
	if len(got) == 0 {
		t.Error("the harness passed a producer that ignores context cancellation")
	}
}

// TestConformIteratorRejectsATrivialProducer guards against a producer that
// passes every check by yielding nothing.
func TestConformIteratorRejectsATrivialProducer(t *testing.T) {
	t.Parallel()

	empty := func(_ context.Context) (iter.Seq2[int, error], error) {
		return func(_ func(int, error) bool) {}, nil
	}

	got := coretest.CheckIterator(empty)
	if len(got) == 0 {
		t.Error("the harness passed an empty producer; every check would be vacuous")
	}
}

// TestErrProbeIsDistinguishable confirms the injected error is identifiable,
// so a consumer under test cannot pass by swallowing it and returning some
// other error.
func TestErrProbeIsDistinguishable(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("reading assets: %w", coretest.ErrProbe)

	if !errors.Is(wrapped, coretest.ErrProbe) {
		t.Error("ErrProbe is not identifiable once wrapped, so a consumer under test " +
			"could pass by swallowing it and returning something else")
	}
	if errors.Is(wrapped, errors.New("coretest: injected fatal error")) {
		t.Error("ErrProbe matched an unrelated error with identical text")
	}
}
