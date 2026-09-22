// Package endpointbundle contains the Linux endpoints built before the host.
package endpointbundle

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed assets/linux_amd64/dworm_endpoint assets/linux_arm64/dworm_endpoint
var assets embed.FS

func Payload(platform string) ([]byte, error) {
	parts := strings.Split(strings.TrimSpace(platform), "/")
	if len(parts) != 2 || parts[0] != "linux" {
		return nil, fmt.Errorf("unsupported container platform %q (expected linux/amd64 or linux/arm64)", platform)
	}
	arch := parts[1]
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("unsupported container architecture %q", arch)
	}
	return assets.ReadFile("assets/linux_" + arch + "/dworm_endpoint")
}
