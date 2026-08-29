package vertex

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks out of the package's tests: the token
// cache, the transport, and the exchange all hold goroutine-adjacent state,
// and a leak here is how a run's HTTP connections outlive it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
