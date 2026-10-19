//go:build windows

package sessionstore

// isMountPointOn: Windows builds have no RWX subPath mounts in this
// fleet — report false (honest degradation wording applies).
func isMountPointOn(dir, parent string) bool {
	return false
}
