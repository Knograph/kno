package exec

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak over the whole suite: exec is an adapter whose entire
// contract is subprocess lifecycle, and a leaked process-control goroutine
// (a Cancel that never finished, a Wait abandoned) is the exact bug class
// this package exists to catch. VerifyTestMain, not a per-test check, because
// goleak's census is process-global and the suite runs in parallel.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fixture returns the path to a testdata script.
func fixture(t *testing.T, name string) string {
	t.Helper()
	return "testdata/" + name
}
