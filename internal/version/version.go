// Package version contains build information injected by the release build.
package version

import "fmt"

var (
	// Version is the semantic release version or dev for local builds.
	Version = "dev"
	// Commit is the source revision used for the build.
	Commit = "unknown"
	// BuildDate is the RFC3339 release build time.
	BuildDate = "unknown"
)

// String returns deterministic human-readable build information.
func String() string {
	return fmt.Sprintf("tama-link %s (commit %s, built %s)", Version, Commit, BuildDate)
}
