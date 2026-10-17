//go:build windows

package worker

import (
	"os/exec"
	"syscall"
)

// Windows has no unix process groups; the WaitDelay forced reap still
// bounds cmd.Run (job-object kill is out of scope — no fleet plugin runs
// on windows).
func sysProcAttrGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func cancelKillGroup(cmd *exec.Cmd) func() error {
	return nil // default Cancel (direct-child Kill) applies
}
