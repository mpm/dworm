package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceFolderFromMounts(t *testing.T) {
	mounts := []containerMount{
		{Source: "/home/u/.claude", Destination: "/home/vscode/.claude"},
		{Source: "/home/u/projects", Destination: "/workspaces"},
		{Source: "/home/u/projects/app", Destination: "/workspaces/app-root"},
	}
	tests := []struct{ workspace, want string }{
		{"/home/u/projects/app", "/workspaces/app-root"},
		{"/home/u/projects/other", "/workspaces/other"},
		{"/home/u/projects", "/workspaces"},
		{"/home/u/elsewhere", ""},
		{"/home/u/projects-old", ""},
	}
	for _, tt := range tests {
		if got := workspaceFolderFromMounts(mounts, tt.workspace); got != tt.want {
			t.Errorf("workspaceFolderFromMounts(%q) = %q, want %q", tt.workspace, got, tt.want)
		}
	}
}

func TestResolveWorkspaceFolderUsesDockerInspect(t *testing.T) {
	fakeDocker(t, `echo '[{"Type":"bind","Source":"/src/app","Destination":"/workspaces/app"}]'`)
	if got := ResolveWorkspaceFolder("cid", "/src/app"); got != "/workspaces/app" {
		t.Fatalf("ResolveWorkspaceFolder = %q", got)
	}
}

func TestDevcontainerEnvironment(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "up"
		if rebuild {
			name = "rebuild"
		}
		for _, configured := range []bool{false, true} {
			mode := "inherited"
			if configured {
				mode = "configured"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("BUILDX_BAKE_ENTITLEMENTS_FS", "1")
				t.Setenv("DWORM_TEST_EMPTY", "inherited")
				t.Setenv("DWORM_TEST_KEEP", "keep")
				t.Setenv("DWORM_TEST_CAPTURE", filepath.Join(root, "capture"))
				if configured {
					data := "[devcontainer_env]\nBUILDX_BAKE_ENTITLEMENTS_FS = '0'\nDWORM_TEST_EMPTY = ''\nDWORM_TEST_LITERAL = '$(touch nope) $HOME'\n"
					if err := os.WriteFile(filepath.Join(root, ".dworm.config"), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(root, ".dworm.env"), []byte("DWORM_TEST_CONTAINER_ONLY=secret\n"), 0600); err != nil {
					t.Fatal(err)
				}
				dir := fakeDocker(t, `test "$BUILDX_BAKE_ENTITLEMENTS_FS" = 1 || exit 1
printf '/test-app\n'
`)
				script := `#!/bin/sh
set -eu
test "${DWORM_TEST_CONTAINER_ONLY-unset}" = unset
printf '%s\n' "$BUILDX_BAKE_ENTITLEMENTS_FS" "$DWORM_TEST_EMPTY" "$DWORM_TEST_KEEP" "${DWORM_TEST_LITERAL-unset}" "$@" > "$DWORM_TEST_CAPTURE"
printf '{"outcome":"success","containerId":"0123456789abcdef","remoteUser":"developer","remoteWorkspaceFolder":"/workspace"}\n'
`
				if err := os.WriteFile(filepath.Join(dir, "devcontainer"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				var info *ContainerInfo
				var err error
				if rebuild {
					info, err = DevcontainerRebuild(root, "/custom/devcontainer.json")
				} else {
					info, err = DevcontainerUp(context.Background(), root, "/custom/devcontainer.json", nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if info.ContainerName != "test-app" || info.RemoteUser != "developer" || info.WorkspaceDir != "/workspace" {
					t.Fatalf("container info = %+v", info)
				}
				data, err := os.ReadFile(os.Getenv("DWORM_TEST_CAPTURE"))
				if err != nil {
					t.Fatal(err)
				}
				want := "1\ninherited\nkeep\nunset\nup\n"
				if configured {
					want = "0\n\nkeep\n$(touch nope) $HOME\nup\n"
				}
				if rebuild {
					want += "--remove-existing-container\n"
				}
				want += "--workspace-folder\n" + root + "\n--config\n/custom/devcontainer.json\n"
				if string(data) != want {
					t.Fatalf("CLI environment/args = %q, want %q", data, want)
				}
				if os.Getenv("BUILDX_BAKE_ENTITLEMENTS_FS") != "1" || os.Getenv("DWORM_TEST_EMPTY") != "inherited" {
					t.Fatal("project settings changed the parent environment")
				}
			})
		}
	}
}

func TestDevcontainerRejectsInvalidConfigurationBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".dworm.config"), []byte("[devcontainer_env]\nBAD-NAME = 'value'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, rebuild := range []bool{false, true} {
		var err error
		if rebuild {
			_, err = DevcontainerRebuild(root, "")
		} else {
			_, err = DevcontainerUp(context.Background(), root, "", nil)
		}
		if err == nil || !strings.Contains(err.Error(), "devcontainer_env:") {
			t.Fatalf("rebuild=%v: error = %v, want invalid devcontainer_env", rebuild, err)
		}
	}
}
