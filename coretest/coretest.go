// Package coretest provides the conformance harness every adapter must pass.
//
// The Ring-0 iterator contract is a set of rules that cannot be expressed in a
// Go signature: a yielded error is fatal, cleanup is deferred inside the
// closure, and the producer checks ctx before each yield. Rules that live only
// in documentation get followed unevenly, and the specific way they get
// followed unevenly here is dangerous — if one adapter skips malformed records
// while another halts, the denominator behind every confidence interval varies
// by adapter, invisibly.
//
// So the contract ships with an executable definition. Adapters call
// ConformIterator; a rule nobody can fail is not a rule.
package coretest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"go.uber.org/goleak"

	"github.com/knograph/kno/core"
)

// ErrProbe is the fatal error the harness feeds through producers under test.
var ErrProbe = errors.New("coretest: injected fatal error")

// IteratorFactory returns a fresh iterator for one conformance scenario.
//
// It must return a NEW iterator each call, over at least two items, so that
// breaking after the first is meaningful. Reusing a partially-consumed
// iterator would make several checks vacuous.
type IteratorFactory[T any] func(ctx context.Context) (iter.Seq2[T, error], error)

// CheckIterator runs the conformance checks and returns one message per
// violation, empty when the producer conforms.
//
// It returns findings rather than calling t.Error so the harness itself can be
// tested — a harness that has never been seen to fail has not been shown to
// work (docs/debt.md#16). ConformIterator is the thin reporting wrapper.
//
// What it checks:
//
//   - At least two items are produced, so the other checks are not vacuous.
//   - Context cancellation halts iteration promptly, rather than running to
//     completion and discarding the work.
//   - Early break does not leak goroutines — but ONLY when opts requests it.
//     See LeakCheck.
//
// What it deliberately does NOT check, so nobody mistakes a pass for more
// coverage than it gives:
//
//   - File-descriptor leaks. There is no portable way to observe from outside
//     whether a producer closed its file. Adapters assert that themselves with
//     CleanupProbe, which is why that helper exists.
//   - That a yielded error is fatal. Only the CONSUMER can honor that rule.
//     FatalErrorStopsIteration checks the consumer side.
//
// The goroutine-leak check is OPT-IN through LeakCheck because goleak takes a
// process-global census: with t.Parallel() siblings in the same test binary,
// goroutines belonging to other tests are live at snapshot time, which both
// masks real leaks and produces flakes tied to scheduling order. A flaky gate
// gets disabled rather than fixed, so it is off unless the caller can promise
// a quiet process.
func CheckIterator[T any](newIter IteratorFactory[T], opts ...Option) []string {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	var violations []string

	// 1. The producer must yield enough for the other checks to mean anything.
	seq, err := newIter(context.Background())
	if err != nil {
		return []string{fmt.Sprintf("opening the iterator failed: %v", err)}
	}
	var n int
	for _, err := range seq {
		if err != nil {
			violations = append(violations,
				fmt.Sprintf("unexpected error at item %d: %v", n, err))
			break
		}
		n++
	}
	if n < 2 {
		violations = append(violations, fmt.Sprintf(
			"produced %d items; the conformance checks need at least 2 to be meaningful", n))
	}

	// 2. Early break must not leave a goroutine behind.
	ignore := goleak.IgnoreCurrent()
	func() {
		seq, err := newIter(context.Background())
		if err != nil {
			violations = append(violations, fmt.Sprintf("reopening the iterator failed: %v", err))
			return
		}
		for range seq {
			break // the consumer loses interest immediately
		}
	}()
	if cfg.leakCheck {
		if err := goleak.Find(ignore); err != nil {
			violations = append(violations, fmt.Sprintf(
				"a goroutine outlived an early break; the producer must stop its work "+
					"when the consumer stops reading: %v", err))
		}
	}

	// 3. Cancellation must halt iteration rather than burn the rest of the run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seq, err = newIter(ctx)
	if err == nil {
		var seen int
		for range seq {
			seen++
			if seen > cancelPatience {
				violations = append(violations, fmt.Sprintf(
					"iteration continued past %d items after the context was cancelled; "+
						"the producer must check ctx.Err() before each yield", cancelPatience))
				break
			}
		}
	}

	return violations
}

// cancelPatience is how many post-cancellation yields the harness tolerates
// before calling a producer uncancellable. Small, but not 1: a producer may
// legitimately be mid-yield when cancellation lands.
const cancelPatience = 100

// Option configures the conformance checks.
type Option func(*config)

type config struct{ leakCheck bool }

// LeakCheck enables the goroutine-leak check.
//
// Only pass it from a test that does NOT call t.Parallel(), and in a package
// whose other tests are not running concurrently — goleak's census is
// process-global, so a parallel sibling's goroutines are indistinguishable
// from a leak.
//
// For broad coverage without that constraint, prefer goleak.VerifyTestMain in
// the adapter package's TestMain, which is goleak's own recommendation for
// parallel suites.
func LeakCheck() Option { return func(c *config) { c.leakCheck = true } }

// ConformIterator asserts that a producer honors the Ring-0 iterator contract.
//
// Adapters call this from their own tests. See CheckIterator for exactly what
// is and is not verified.
func ConformIterator[T any](t *testing.T, newIter IteratorFactory[T], opts ...Option) {
	t.Helper()
	for _, v := range CheckIterator(newIter, opts...) {
		t.Error(v)
	}
}

// CheckFatalErrorStops returns one message per violation of the consumer-side
// fatal-error rule, empty when the consumer conforms.
//
// The rule is a CONSUMER obligation: producers can yield an error, but only
// the consumer decides to stop. This drives the consumer with a producer that
// deliberately keeps yielding after an error, because that drift — one adapter
// halting, another skipping — is what would silently change the denominator
// behind every confidence interval.
func CheckFatalErrorStops(consume func(iter.Seq2[int, error]) error) []string {
	var violations []string

	const errorAt = 2
	var yielded int
	misbehaving := func(yield func(int, error) bool) {
		for i := range 10 {
			yielded++
			if i == errorAt {
				if !yield(0, ErrProbe) {
					return
				}
				continue // deliberately keeps going after a fatal error
			}
			if !yield(i, nil) {
				return
			}
		}
	}

	if err := consume(misbehaving); err == nil {
		violations = append(violations,
			"the consumer swallowed a fatal error; a yielded error must stop iteration "+
				"and propagate, or malformed records silently shrink the sample")
	}
	if past := yielded - (errorAt + 1); past > 0 {
		violations = append(violations, fmt.Sprintf(
			"the consumer read %d items past a fatal error; it must stop at the first one", past))
	}
	return violations
}

// FatalErrorStopsIteration asserts that a consumer honors the fatal-error rule.
func FatalErrorStopsIteration(t *testing.T, consume func(iter.Seq2[int, error]) error) {
	t.Helper()
	for _, v := range CheckFatalErrorStops(consume) {
		t.Error(v)
	}
}

// CheckEvalsDuplicateIDs returns one message per duplicate Case ID an Evals
// iterator yields, empty when the source conforms.
//
// The invariant behind docs/debt.md#45: every Evals adapter deduplicates Case
// IDs, so an in-run duplicate is fatal and named rather than counted twice.
// Counted twice, the in-memory count and the store's INSERT OR IGNORE diverge —
// a number and its denominator describing different populations. This check
// turns the per-adapter convention into a core property: a future Evals
// adapter that yields duplicates fails its conformance harness.
func CheckEvalsDuplicateIDs(cases iter.Seq2[*core.Case, error]) []string {
	var violations []string
	seen := make(map[string]struct{})
	for c, err := range cases {
		if err != nil {
			break
		}
		if c == nil {
			continue
		}
		if _, dup := seen[c.GetId()]; dup {
			violations = append(violations, fmt.Sprintf(
				"the Evals source yielded Case id %q twice; an in-run duplicate must be fatal, "+
					"or the in-memory counts and the store's dedup diverge", c.GetId()))
		}
		seen[c.GetId()] = struct{}{}
	}
	return violations
}

// EvalsDuplicateIDs asserts that an Evals iterator never yields a duplicate
// Case ID.
func EvalsDuplicateIDs(t *testing.T, cases iter.Seq2[*core.Case, error]) {
	t.Helper()
	for _, v := range CheckEvalsDuplicateIDs(cases) {
		t.Error(v)
	}
}

// CleanupProbe records whether Close was called.
//
// ConformIterator cannot see file descriptors from outside, so adapters inject
// one of these as their underlying resource and assert AssertClosed after an
// early break. That is the only reliable way to prove cleanup was deferred
// INSIDE the iterator closure, where an early break still runs it.
type CleanupProbe struct {
	closed bool
}

// Close marks the probe closed. Safe to call more than once.
func (p *CleanupProbe) Close() error {
	p.closed = true
	return nil
}

// Closed reports whether Close was called.
func (p *CleanupProbe) Closed() bool { return p.closed }

// AssertClosed fails the test if Close was never called.
func (p *CleanupProbe) AssertClosed(t *testing.T) {
	t.Helper()
	if !p.closed {
		t.Error("the underlying resource was never closed after an early break; " +
			"cleanup must be deferred INSIDE the iterator closure, not around it")
	}
}
