// Package version holds build-time metadata injected via -ldflags.
// GoReleaser populates these from GitVersion output (see .github/goreleaser.yaml).
package version

var (
	// Version is the semantic version (e.g. "0.23.0"). "dev" for local builds.
	Version = "dev"

	// GitCommit is the short commit hash.
	GitCommit = "none"

	// BuildTime is the commit date (ISO 8601).
	BuildTime = "unknown"
)

// String returns the version string for /healthz and log output.
func String() string {
	return Version
}
