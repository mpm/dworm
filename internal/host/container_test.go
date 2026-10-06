package host

import "testing"

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
