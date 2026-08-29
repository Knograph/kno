//go:build windows

package exec

import (
	"os/exec"
	"time"
)

// Windows has no POSIX process groups, so the group-kill guarantee the Unix
// path gives is not available: killing the command kills the direct child,
// and a child it spawned can outlive the deadline. The env allowlist and the
// output caps still hold; the package doc records this as the one platform
// difference.

const (
	killGrace = 1 * time.Second
	waitDelay = 30 * time.Second
)

// setpgid is a no-op on Windows.
func setpgid(cmd *exec.Cmd) {}

// killGroup kills the direct child; there is no group to kill.
func killGroup(cmd *exec.Cmd, grace time.Duration) {
	_ = cmd.Process.Kill()
}
