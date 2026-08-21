package transport_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak check for this package.
//
// docs/debt.md#18 requires an adapter package to install this. The entry names
// the adapter packages, and this is not one — but it is where the goroutines
// actually live: idle connections in the pool, the limiter's timers, and the
// request timeout's own. Serving the entry's purpose as well as its letter.
//
// VerifyTestMain rather than the shared per-test helper: goleak takes a
// process-global census, so a parallel sibling's goroutines are
// indistinguishable from a leak. Once per package is the only form that is not
// flaky, which is goleak's own recommendation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
