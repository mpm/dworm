package protocol

import "encoding/json"

// EnvironmentCommand also supports containers started without an injected endpoint.
func EnvironmentCommand(overrides map[string]string, shell bool, command []string) []string {
	data, _ := json.Marshal(overrides)
	mode := "--with-env"
	if shell {
		mode = "--shell"
	}
	args := []string{"/bin/sh", "-c", `if [ -x /tmp/dworm_endpoint ]; then exec /tmp/dworm_endpoint "$@"; fi; shift 2; exec "$@"`, "dworm", mode, string(data)}
	return append(args, command...)
}
