package bedrock

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak check for this package.
//
// docs/debt.md#18 requires the first adapter packages to install this, and the
// partner-cloud adapters are the next generation of the same obligation: an
// adapter holds a connection pool, a request timeout, and a rate limiter, all
// of which own goroutines that outlive the call that started them — a leak
// here is a run that never exits rather than a test that fails.
//
// VerifyTestMain rather than the shared per-test helper: goleak takes a
// process-global census, so a parallel sibling's goroutines are
// indistinguishable from a leak. Once per package is the only form that is not
// flaky, which is goleak's own recommendation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
