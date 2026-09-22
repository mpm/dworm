package version

import (
	"context"
	"time"
)

const GitHubRepo = "mpm/dworm"

type CheckResult struct {
	Current, Latest, ReleaseURL string
	UpdateAvailable             bool
}

func CheckForUpdate() *CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return CheckForUpdateWithContext(ctx)
}

// Incidental checks must never interrupt a command or print from a goroutine.
func CheckForUpdateWithContext(ctx context.Context) *CheckResult {
	if !releaseVersion(Version) {
		return nil
	}
	up, err := newUpdater(nil, "", "")
	if err != nil {
		return nil
	}
	rel, err := discover(ctx, up)
	if err != nil {
		return nil
	}
	return &CheckResult{Current: Version, Latest: "v" + rel.Version(), ReleaseURL: rel.URL, UpdateAvailable: rel.GreaterThan(Version)}
}
