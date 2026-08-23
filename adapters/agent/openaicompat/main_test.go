package openaicompat_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak check for this package.
//
// docs/debt.md#18 names the adapter packages specifically, and this is the
// first one. VerifyTestMain rather than coretest's opt-in per-test helper:
// goleak takes a process-global census, so a parallel sibling's goroutines are
// indistinguishable from a leak, which both masks real leaks and flakes on
// scheduling order. Once per package is the only form that is not flaky, and it
// is goleak's own recommendation.
//
// It has teeth here because this package holds a connection pool, request
// timeouts, and a rate limiter — three sources of goroutines that outlive the
// call that started them if anything is wired wrong.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
