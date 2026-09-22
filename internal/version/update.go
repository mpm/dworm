package version

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	apply "github.com/creativeprojects/go-selfupdate/update"
)

func releaseVersion(v string) bool {
	parsed, err := semver.StrictNewVersion(strings.TrimPrefix(v, "v"))
	return err == nil && parsed.Prerelease() == "" && !strings.Contains(v, "dirty")
}

func newUpdater(source selfupdate.Source, goos, arch string) (*selfupdate.Updater, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if (goos != "linux" && goos != "darwin") || (arch != "amd64" && arch != "arm64") {
		return nil, fmt.Errorf("unsupported host platform %s/%s; see https://github.com/%s/releases", goos, arch, GitHubRepo)
	}
	return selfupdate.NewUpdater(selfupdate.Config{
		Source: source, OS: goos, Arch: arch,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
		Filters:   []string{`^dworm_v[^/]+_` + goos + `_` + arch + `\.tar\.gz$`},
	})
}

func discover(ctx context.Context, up *selfupdate.Updater) (*selfupdate.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rel, found, err := up.DetectLatest(ctx, selfupdate.ParseSlug(GitHubRepo))
	if err != nil {
		return nil, fmt.Errorf("discover release: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("no stable release asset for this platform; see https://github.com/%s/releases", GitHubRepo)
	}
	if !releaseVersion(rel.Version()) {
		return nil, fmt.Errorf("release %s is not a stable version", rel.Version())
	}
	return rel, nil
}

// SelfUpdate updates the actual running executable, including symlink resolution.
func SelfUpdate(ctx context.Context, out io.Writer) error {
	if !releaseVersion(Version) {
		return fmt.Errorf("self-update requires a clean stable release build (running %q); install from https://github.com/%s/releases", Version, GitHubRepo)
	}
	up, err := newUpdater(nil, "", "")
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running executable: %w", err)
	}
	return updateExecutable(ctx, up, Version, exe, out)
}

func updateExecutable(ctx context.Context, up *selfupdate.Updater, current, exe string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rel, err := discover(ctx, up)
	if err != nil {
		return err
	}
	if !rel.GreaterThan(current) {
		fmt.Fprintf(out, "dworm %s is already current or newer (latest: v%s).\n", current, rel.Version())
		return nil
	}
	destination, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable %s: %w", exe, err)
	}
	if err := up.UpdateTo(ctx, rel, destination); err != nil {
		if rollback := apply.RollbackError(err); rollback != nil {
			return fmt.Errorf("update %s failed: %v; rollback failed: %v; restore manually from %s", destination, err, rollback, rel.URL)
		}
		return fmt.Errorf("update %s failed (check destination directory permissions): %w; release: %s", destination, err, rel.URL)
	}
	fmt.Fprintf(out, "Installed dworm v%s to %s. The next invocation uses the update.\n", rel.Version(), destination)
	return nil
}
