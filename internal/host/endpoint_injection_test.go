package host

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectionCleansTemporaryPayload(t *testing.T) {
	for _, failure := range []string{"", "copy", "publish"} {
		t.Run("failure="+failure, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			// Successful startup exits immediately; yamux initialization does not
			// need an endpoint handshake. This isolates copying and staging.
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_CALLS"
case "$1" in
inspect) echo sha256:selected-image ;;
image) echo linux/arm64 ;;
cp) [ "$FAIL_STAGE" != copy ] ;;
exec)
  if [ "$3" = mv ]; then [ "$FAIL_STAGE" != publish ]; fi
  ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_CALLS", log)
			t.Setenv("FAIL_STAGE", failure)
			t.Setenv("TMPDIR", dir)
			e := NewEndpointManager("fixture-container", io.Discard)
			err := e.InjectAndStart()
			e.Close()
			if (err != nil) != (failure != "") {
				t.Fatalf("injection error: %v", err)
			}
			entries, _ := filepath.Glob(filepath.Join(dir, "dworm-endpoint-*"))
			if len(entries) != 0 {
				t.Fatalf("temporary payloads remain: %v", entries)
			}
			calls, _ := os.ReadFile(log)
			text := string(calls)
			if !strings.Contains(text, "image inspect --format {{.Os}}/{{.Architecture}} sha256:selected-image") {
				t.Fatal(text)
			}
			if !strings.Contains(text, "rm -f /tmp/dworm_endpoint.dworm-endpoint-") {
				t.Fatal("staging cleanup missing: " + text)
			}
			if failure == "" && !strings.Contains(text, "mv -f /tmp/dworm_endpoint.dworm-endpoint-") {
				t.Fatal("atomic publish missing: " + text)
			}
		})
	}
}
