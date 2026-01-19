package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// GitHubRepo is the repository path for version checks.
	GitHubRepo = "mpm/dworm"

	// checkTimeout is the timeout for the GitHub API request.
	checkTimeout = 5 * time.Second
)

// githubRelease represents the relevant fields from the GitHub releases API.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// CheckResult contains the result of a version check.
type CheckResult struct {
	// Current is the currently running version.
	Current string

	// Latest is the latest available version from GitHub.
	Latest string

	// UpdateAvailable is true if a newer version is available.
	UpdateAvailable bool

	// ReleaseURL is the URL to the latest release page.
	ReleaseURL string
}

// CheckForUpdate queries GitHub for the latest release and compares it
// to the current version. Returns nil if the check fails (network error, etc.)
// or if running a development version.
func CheckForUpdate() *CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	return CheckForUpdateWithContext(ctx)
}

// CheckForUpdateWithContext is like CheckForUpdate but accepts a context.
func CheckForUpdateWithContext(ctx context.Context) *CheckResult {
	// Skip check for development builds
	if IsDev() {
		return nil
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", GitHubRepo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "dworm/"+Short())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil
	}

	if release.TagName == "" {
		return nil
	}

	result := &CheckResult{
		Current:    Version,
		Latest:     release.TagName,
		ReleaseURL: release.HTMLURL,
	}

	result.UpdateAvailable = isNewer(release.TagName, Version)

	return result
}

// isNewer returns true if latest is a newer version than current.
// Both versions should be in the format "vX.Y.Z" or "X.Y.Z".
func isNewer(latest, current string) bool {
	// Normalize: remove leading "v"
	latest = strings.TrimPrefix(latest, "v")
	current = strings.TrimPrefix(current, "v")

	latestParts := parseVersion(latest)
	currentParts := parseVersion(current)

	if latestParts == nil || currentParts == nil {
		// Fall back to string comparison if parsing fails
		return latest > current
	}

	for i := 0; i < 3; i++ {
		if latestParts[i] > currentParts[i] {
			return true
		}
		if latestParts[i] < currentParts[i] {
			return false
		}
	}

	return false
}

// parseVersion parses a version string like "1.2.3" into [major, minor, patch].
// Returns nil if parsing fails.
func parseVersion(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) < 1 {
		return nil
	}

	result := make([]int, 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		// Handle pre-release suffixes like "1.2.3-beta"
		numStr := strings.Split(parts[i], "-")[0]
		var n int
		if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
			return nil
		}
		result[i] = n
	}

	return result
}
