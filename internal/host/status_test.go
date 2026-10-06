//go:build unix

package host

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRemoteUserFromMetadata(t *testing.T) {
	tests := []struct{ metadata, image, want string }{
		{`[{"remoteUser":"a"},{"id":"x"},{"remoteUser":"vscode","containerUser":"root"}]`, "img", "vscode"},
		{`[{"containerUser":"node"}]`, "img", "node"},
		{`[]`, "img", "img"},
		{``, "img", "img"},
	}
	for _, tt := range tests {
		if got := remoteUserFromMetadata(tt.metadata, tt.image); got != tt.want {
			t.Errorf("remoteUserFromMetadata(%q) = %q, want %q", tt.metadata, got, tt.want)
		}
	}
}

const inspectFixture = `[{"Id":"2e7d99a944c1aaaaaaaa","Name":"/scenebox-app-1","State":{"Running":true},
 "Config":{"User":"root","Labels":{"devcontainer.metadata":"[{\"remoteUser\":\"vscode\"}]"}},
 "Mounts":[{"Source":"/src/scenebox","Destination":"/workspaces/scenebox"}]}]`

func TestGetStatusWithoutContainer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	fakeDocker(t, "exit 0\n")

	status, err := GetStatus("/src/scenebox", "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(status)
	want := `{"workspace_path":"/src/scenebox","container":null,"up":{"running":false,"endpoint_connected":false,"ports":[]},"dworm_version":"v0.7.0"}`
	if string(data) != want {
		t.Fatalf("status = %s\nwant      %s", data, want)
	}
}

func TestGetStatusWithContainerAndRunningUp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	fakeDocker(t, `case "$1" in ps) echo 2e7d99a944c1 ;; inspect) cat "$INSPECT_FIXTURE" ;; esac`)
	fixture := t.TempDir() + "/inspect.json"
	os.WriteFile(fixture, []byte(inspectFixture), 0600)
	t.Setenv("INSPECT_FIXTURE", fixture)

	status, err := GetStatus("/src/scenebox", "dev")
	if err != nil {
		t.Fatal(err)
	}
	c := status.Container
	if c == nil || c.ID != "2e7d99a944c1" || c.Name != "scenebox-app-1" || !c.Running || c.RemoteUser != "vscode" || c.WorkspaceFolder != "/workspaces/scenebox" {
		t.Fatalf("container = %+v", c)
	}
	if status.Up.Running {
		t.Fatal("up.running without a lock holder")
	}

	lock, err := AcquireInstanceLock("/src/scenebox")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := NewStateFile(lock.Paths.State, InstanceState{
		PID: 99, ContainerID: "2e7d99a944c1aaaaaaaa", WorkspaceFolder: "/workspaces/override",
		EndpointConnected: true, Ports: []StatePort{{Port: 3000, Address: "127.0.0.1", LocalPort: 3000}},
	}); err != nil {
		t.Fatal(err)
	}
	status, err = GetStatus("/src/scenebox", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Up.Running || status.Up.PID != 99 || !status.Up.EndpointConnected || len(status.Up.Ports) != 1 {
		t.Fatalf("up = %+v", status.Up)
	}
	if status.Container.WorkspaceFolder != "/workspaces/override" {
		t.Fatalf("workspace folder = %q, want the running instance's", status.Container.WorkspaceFolder)
	}
	if got := WorkspaceFolder("2e7d99a944c1aaaaaaaa", "/src/scenebox"); got != "/workspaces/override" {
		t.Fatalf("WorkspaceFolder = %q", got)
	}
	data, _ := json.Marshal(status)
	for _, field := range []string{`"pid":99`, `"started_at"`, `"ports":[{"port":3000,"address":"127.0.0.1","local_port":3000}]`} {
		if field == `"started_at"` {
			if strings.Contains(string(data), field) {
				t.Fatalf("zero started_at serialized: %s", data)
			}
			continue
		}
		if !strings.Contains(string(data), field) {
			t.Fatalf("status JSON missing %s: %s", field, data)
		}
	}
}
