//go:build !windows

package exec

import (
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a cancelled command gets to shut down after TERM
// before the group is killed outright.
const killGrace = 1 * time.Second

// waitDelay bounds the whole cancellation sequence.
//
// CommandContext force-kills the direct child if Cancel has not led to an
// exit within WaitDelay. This is the backstop behind killGroup; it should
// never fire, and it is what keeps Wait from hanging forever on a process
// that ignores both TERM and KILL.
const waitDelay = 30 * time.Second

// setpgid makes the command the leader of its own process group.
func setpgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the command's whole process group.
//
// TERM first, KILL after killGrace: a script that handles TERM gets a chance
// to clean up, and one that does not cannot outlive the deadline. The group,
// not the process — without Setpgid, killing the script leaves its children
// running (a background job survives its parent), and the caps and the resume
// semantics both assume the whole group dies with the Case.
//
// Killing a group that is already gone is a normal race, not an error: the
// signal is sent to -pgid, and ESRCH just means nothing is left.
func killGroup(cmd *exec.Cmd, grace time.Duration) {
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(grace)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
