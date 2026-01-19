// Package version provides version information for dworm.
// Variables are set at build time via ldflags.
package version

import (
	"fmt"
	"runtime"
)

// These variables are set at build time via ldflags:
//
//	-X github.com/mpm/dworm/internal/version.Version=v1.0.0
//	-X github.com/mpm/dworm/internal/version.Commit=abc123
//	-X github.com/mpm/dworm/internal/version.Date=2024-01-01
var (
	// Version is the release version (e.g., "v1.0.0").
	// Empty or "dev" indicates a development build.
	Version = "dev"

	// Commit is the git commit hash.
	Commit = "unknown"

	// Date is the build date.
	Date = "unknown"
)

// Info returns a formatted version string suitable for display.
func Info() string {
	if Version == "dev" || Version == "" {
		return fmt.Sprintf("dworm dev (commit: %s, built: %s, %s/%s)",
			shortCommit(), Date, runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("dworm %s (commit: %s, built: %s, %s/%s)",
		Version, shortCommit(), Date, runtime.GOOS, runtime.GOARCH)
}

// Short returns a short version string (just the version or "dev").
func Short() string {
	if Version == "dev" || Version == "" {
		return "dev"
	}
	return Version
}

// IsDev returns true if this is a development build.
func IsDev() bool {
	return Version == "dev" || Version == ""
}

func shortCommit() string {
	if len(Commit) > 7 {
		return Commit[:7]
	}
	return Commit
}
