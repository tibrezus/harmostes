//go:build !windows

package sessionstore

import "syscall"

// isMountPointOn reports whether dir sits on a different device than
// its parent — an actual mount, not plain directories materialized on
// the container's ephemeral layer (#704 r7/r8).
func isMountPointOn(dir, parent string) bool {
	var di, pi syscall.Stat_t
	if err := syscall.Stat(dir, &di); err != nil {
		return false
	}
	if err := syscall.Stat(parent, &pi); err != nil {
		return false
	}
	return di.Dev != pi.Dev
}
