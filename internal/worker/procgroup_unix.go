//go:build !windows

package worker

import (
	"os/exec"
	"syscall"
)

// sysProcAttrGroup puts the plugin in its own process group.
func sysProcAttrGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// cancelKillGroup takes down the WHOLE process group on context cancel —
// Go's default Cancel kills only the direct child, and a stalled plugin
// shell (`git clone … | tail`) leaves pipe-holding grandchildren that
// outlive it (#689's zero-work wall runs). WaitDelay is the second belt:
// even if a grandchild escapes the group kill by holding the pipes, Wait
// returns after the grace instead of blocking the node forever.
func cancelKillGroup(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		// nil: exec reports ctx.Err() (DeadlineExceeded for a node
		// timeout) so the G8 attribution in the graph executor keeps
		// working — a non-nil return would shadow it with this error.
		return nil
	}
}
