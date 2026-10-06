//go:build unix

package host

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// removeFakeDocker answers the docker calls of PlanRemove/Execute from the
// environment (output per call kind) and logs every call to the returned file.
// A call whose arguments contain $FAIL_ON fails.
const removeFakeDocker = `echo "$*" >> "$DOCKER_LOG"
case "$*" in *"$FAIL_ON"*) if [ -n "$FAIL_ON" ]; then echo "boom: $*" >&2; exit 1; fi ;; esac
case "$*" in
  "ps -aq --filter label=devcontainer.local_folder="*) printf "$OUT_DEVCONTAINER" ;;
  "inspect --type container --format "*) printf "$OUT_INSPECT" ;;
  "inspect --format {{.State.Running}} "*) echo true ;;
  "ps -a --no-trunc --filter label=com.docker.compose.project="*) printf "$OUT_CONTAINERS" ;;
  "image inspect "*) printf "$OUT_TAGS" ;;
  "images --no-trunc --filter label=com.docker.compose.project="*) printf "$OUT_IMAGES" ;;
  "network ls --filter label=com.docker.compose.project="*) printf "$OUT_NETWORKS" ;;
  "volume ls --filter label=com.docker.compose.project="*) printf "$OUT_VOLUMES" ;;
esac
`

func setupRemoveFakeDocker(t *testing.T, env map[string]string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("DOCKER_LOG", logPath)
	for _, key := range []string{"OUT_DEVCONTAINER", "OUT_INSPECT", "OUT_CONTAINERS", "OUT_TAGS", "OUT_IMAGES", "OUT_NETWORKS", "OUT_VOLUMES", "FAIL_ON"} {
		t.Setenv(key, env[key])
	}
	fakeDocker(t, removeFakeDocker)
	return logPath
}

func dockerCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// mutatingCalls drops the read-only calls of the plan.
func mutatingCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, prefix := range []string{"stop", "rm ", "rmi ", "network rm", "volume rm"} {
			if strings.HasPrefix(c, prefix) {
				out = append(out, c)
			}
		}
	}
	return out
}

const (
	appID      = "2e7d99a944c1"
	appFullID  = "2e7d99a944c1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pgFullID   = "15052cb14cc2bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	uidImageID = "sha256:1174da09e509cccccccccccccccccccccccccccccccccccccccccccccccccccc"
	appImageID = "sha256:67d0eef301ceeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	oldImageID = "sha256:99990000aaaa0000000000000000000000000000000000000000000000000000"
	uidRef     = "vsc-scenebox-3ad2-uid:latest"
)

func composeEnv() map[string]string {
	return map[string]string{
		"OUT_DEVCONTAINER": appID + `\n`,
		"OUT_INSPECT":      uidImageID + `\t/scenebox-app-1\tscenebox\n`,
		// The devcontainer is listed again (full ID); a stopped postgres service too.
		"OUT_CONTAINERS": appFullID + `\tscenebox-app-1\n` + pgFullID + `\tscenebox-postgres-1\n`,
		"OUT_TAGS":       `["` + uidRef + `"]\n`,
		// The uid image inherits the project label; an old untagged build.
		"OUT_IMAGES":   uidImageID + `\t` + uidRef + `\n` + appImageID + `\tscenebox-app:latest\n` + oldImageID + `\t<none>:<none>\n`,
		"OUT_NETWORKS": `scenebox_default\n`,
		"OUT_VOLUMES":  `scenebox_pgdata\n`,
	}
}

func TestPlanRemoveCompose(t *testing.T) {
	setupRemoveFakeDocker(t, composeEnv())

	plan, err := PlanRemove("/src/scenebox", RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ComposeProject != "scenebox" || plan.ContainerID != appID || plan.ImageID != uidImageID {
		t.Fatalf("plan = %+v", plan)
	}
	wantContainers := []RemoveContainer{{ID: appID, Name: "scenebox-app-1"}, {ID: pgFullID, Name: "scenebox-postgres-1"}}
	if !reflect.DeepEqual(plan.Containers, wantContainers) {
		t.Fatalf("containers = %+v, want %+v", plan.Containers, wantContainers)
	}
	wantImages := []RemoveImage{{ID: uidImageID, Ref: uidRef}, {ID: appImageID, Ref: "scenebox-app:latest"}, {ID: oldImageID}}
	if !reflect.DeepEqual(plan.Images, wantImages) {
		t.Fatalf("images = %+v, want %+v", plan.Images, wantImages)
	}
	if !reflect.DeepEqual(plan.Networks, []string{"scenebox_default"}) || !reflect.DeepEqual(plan.Volumes, []string{"scenebox_pgdata"}) || plan.RemoveVolumes {
		t.Fatalf("networks = %q, volumes = %q, remove volumes = %v", plan.Networks, plan.Volumes, plan.RemoveVolumes)
	}

	want := "  containers: scenebox-app-1, scenebox-postgres-1\n" +
		"  images:     " + uidRef + ", scenebox-app:latest, 99990000aaaa\n" +
		"  networks:   scenebox_default\n" +
		"  Volumes are kept (use --volumes to remove them): scenebox_pgdata\n"
	if got := plan.Describe(); got != want {
		t.Fatalf("Describe() =\n%s\nwant\n%s", got, want)
	}
	plan.RemoveVolumes = true
	if got := plan.Describe(); !strings.Contains(got, "  volumes:    scenebox_pgdata\n") || strings.Contains(got, "kept") {
		t.Fatalf("Describe() with volumes =\n%s", got)
	}
}

func TestRemoveComposeKeepsVolumes(t *testing.T) {
	logPath := setupRemoveFakeDocker(t, composeEnv())

	plan, err := PlanRemove("/src/scenebox", RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Execute()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stop " + appID + " " + pgFullID,
		"rm " + appID,
		"rm " + pgFullID,
		"rmi " + uidRef,
		"rmi scenebox-app:latest",
		"rmi " + oldImageID,
		"network rm scenebox_default",
	}
	if got := mutatingCalls(dockerCalls(t, logPath)); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantLines := []string{
		"Removed containers: scenebox-app-1, scenebox-postgres-1",
		"Removed images: " + uidRef + ", scenebox-app:latest, 99990000aaaa",
		"Removed networks: scenebox_default",
		"Kept volumes (use --volumes to remove them): scenebox_pgdata",
	}
	if got := result.Lines(); !reflect.DeepEqual(got, wantLines) {
		t.Fatalf("lines = %q, want %q", got, wantLines)
	}
}

func TestRemoveComposeWithVolumes(t *testing.T) {
	logPath := setupRemoveFakeDocker(t, composeEnv())

	plan, err := PlanRemove("/src/scenebox", RemoveOptions{Volumes: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Execute()
	if err != nil {
		t.Fatal(err)
	}
	calls := mutatingCalls(dockerCalls(t, logPath))
	for _, want := range []string{"rm -v " + appID, "rm -v " + pgFullID, "volume rm scenebox_pgdata"} {
		if !contains(calls, want) {
			t.Fatalf("calls = %q, missing %q", calls, want)
		}
	}
	if !reflect.DeepEqual(result.Volumes, []string{"scenebox_pgdata"}) || result.KeptVolumes != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestRemoveComposeContinuesAfterFailure(t *testing.T) {
	env := composeEnv()
	env["FAIL_ON"] = "rmi scenebox-app:latest"
	setupRemoveFakeDocker(t, env)

	plan, err := PlanRemove("/src/scenebox", RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Execute()
	if err == nil || !strings.Contains(err.Error(), "boom: rmi scenebox-app:latest") {
		t.Fatalf("err = %v, want the failed rmi", err)
	}
	if !reflect.DeepEqual(result.Images, []string{uidRef, "99990000aaaa"}) || !reflect.DeepEqual(result.Networks, []string{"scenebox_default"}) {
		t.Fatalf("result = %+v", result)
	}
}

func TestPlanRemoveUntaggedDevcontainerImage(t *testing.T) {
	// A service image that is pulled (not built by Compose) gives the
	// devcontainer CLI's uid image no project label: it is removed by ID.
	env := composeEnv()
	env["OUT_TAGS"] = `[]\n`
	env["OUT_IMAGES"] = ""
	setupRemoveFakeDocker(t, env)

	plan, err := PlanRemove("/src/scenebox", RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []RemoveImage{{ID: uidImageID}}; !reflect.DeepEqual(plan.Images, want) {
		t.Fatalf("images = %+v, want %+v", plan.Images, want)
	}
}

func TestRemoveWithoutCompose(t *testing.T) {
	env := map[string]string{
		"OUT_DEVCONTAINER": "f89daca66bf9\n",
		"OUT_INSPECT":      `sha256:85fe33d211ea\t/distracted_jennings\t\n`,
	}
	logPath := setupRemoveFakeDocker(t, env)

	plan, err := PlanRemove("/src/dworm", RemoveOptions{Volumes: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ComposeProject != "" || plan.ContainerID != "f89daca66bf9" || plan.ImageID != "sha256:85fe33d211ea" || plan.RemoveVolumes {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := plan.Execute(); err != nil {
		t.Fatal(err)
	}
	calls := dockerCalls(t, logPath)
	for _, c := range calls {
		if strings.Contains(c, "com.docker.compose.project=") {
			t.Fatalf("non-Compose plan listed project resources: %q", calls)
		}
	}
	want := []string{"stop f89daca66bf9", "rm f89daca66bf9", "rmi sha256:85fe33d211ea"}
	if got := mutatingCalls(calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestRemoveWithoutComposeImageFailure(t *testing.T) {
	env := map[string]string{
		"OUT_DEVCONTAINER": "f89daca66bf9\n",
		"OUT_INSPECT":      `sha256:85fe33d211ea\t/distracted_jennings\t\n`,
		"FAIL_ON":          "rmi ",
	}
	setupRemoveFakeDocker(t, env)

	plan, err := PlanRemove("/src/dworm", RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Execute(); err == nil || !strings.Contains(err.Error(), "container removed but failed to remove image") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanRemoveWithoutContainer(t *testing.T) {
	setupRemoveFakeDocker(t, map[string]string{})
	if _, err := PlanRemove("/src/none", RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "no container found for /src/none") {
		t.Fatalf("err = %v", err)
	}
}
