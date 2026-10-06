package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// composeProjectLabel is the label Docker Compose puts on the containers,
// networks, volumes, and built images of a project. The devcontainer CLI
// starts Compose-based devcontainers through Compose, so they carry it too;
// images built FROM a project image (the devcontainer CLI's `-uid` image)
// inherit it.
const composeProjectLabel = "com.docker.compose.project"

// RemoveOptions configures PlanRemove.
type RemoveOptions struct {
	// Volumes also removes a Compose project's volumes: the named volumes
	// Compose created for it and the containers' anonymous volumes. Without
	// Compose it has no effect.
	Volumes bool
}

// RemoveImage is an image `dworm remove` deletes: by Ref (repo:tag) when it
// has one, else by ID.
type RemoveImage struct {
	ID  string
	Ref string
}

func (i RemoveImage) target() string {
	if i.Ref != "" {
		return i.Ref
	}
	return i.ID
}

// String is the name shown to the user.
func (i RemoveImage) String() string {
	if i.Ref != "" {
		return i.Ref
	}
	id := strings.TrimPrefix(i.ID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// RemoveContainer is a container `dworm remove` deletes.
type RemoveContainer struct {
	ID   string
	Name string
}

// RemovePlan is what `dworm remove` deletes for a workspace, determined
// before anything is touched so the confirmation prompt can list it.
type RemovePlan struct {
	WorkspacePath string
	// ContainerID is the devcontainer (the first container labelled
	// devcontainer.local_folder=<workspace>), ImageID the image it runs.
	ContainerID string
	ImageID     string
	// ComposeProject is the devcontainer's Compose project; empty for a
	// single-container devcontainer, whose plan is only ContainerID and
	// ImageID. The other fields are only set for Compose.
	ComposeProject string
	// Containers are all containers of the project (all services, running
	// or stopped), the devcontainer first.
	Containers []RemoveContainer
	// Images are the devcontainer's image and every image built for the
	// project (labelled with it, e.g. `<project>-app`). Pulled images that
	// other services run (postgres, …) are kept.
	Images []RemoveImage
	// Networks are the networks Compose created for the project.
	Networks []string
	// Volumes are the named volumes Compose created for the project. They
	// are removed only when RemoveVolumes is set.
	Volumes       []string
	RemoveVolumes bool
}

// RemoveResult lists what Execute removed (Compose) and the volumes it kept.
type RemoveResult struct {
	Containers  []string
	Images      []string
	Networks    []string
	Volumes     []string
	KeptVolumes []string
}

// PlanRemove finds the workspace's devcontainer (running or stopped) and,
// when it belongs to a Compose project, all resources of that project.
func PlanRemove(workspacePath string, opts RemoveOptions) (*RemovePlan, error) {
	ids, err := dockerLines("ps", "-aq", "--filter", "label=devcontainer.local_folder="+workspacePath)
	if err != nil {
		return nil, fmt.Errorf("failed to find container: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no container found for %s", workspacePath)
	}
	plan := &RemovePlan{WorkspacePath: workspacePath, ContainerID: ids[0]}

	// Without the image or the project label, remove the container alone
	// (like a single-container devcontainer whose image cannot be found).
	info, err := dockerLines("inspect", "--type", "container", "--format",
		`{{.Image}}{{"\t"}}{{.Name}}{{"\t"}}{{index .Config.Labels "`+composeProjectLabel+`"}}`, plan.ContainerID)
	if err != nil || len(info) == 0 {
		return plan, nil
	}
	fields := strings.Split(info[0], "\t")
	plan.ImageID = fields[0]
	if len(fields) < 3 || fields[2] == "" {
		return plan, nil
	}
	plan.ComposeProject = fields[2]
	plan.RemoveVolumes = opts.Volumes
	if err := plan.findComposeResources(strings.TrimPrefix(fields[1], "/")); err != nil {
		return nil, fmt.Errorf("failed to list Compose project %s: %w", plan.ComposeProject, err)
	}
	return plan, nil
}

func (p *RemovePlan) findComposeResources(devcontainerName string) error {
	filter := "label=" + composeProjectLabel + "=" + p.ComposeProject

	// Containers: the devcontainer first, then the other services.
	p.Containers = []RemoveContainer{{ID: p.ContainerID, Name: devcontainerName}}
	lines, err := dockerLines("ps", "-a", "--no-trunc", "--filter", filter, "--format", "{{.ID}}\t{{.Names}}")
	if err != nil {
		return err
	}
	for _, line := range lines {
		id, name, _ := strings.Cut(line, "\t")
		if !strings.HasPrefix(id, p.ContainerID) && !strings.HasPrefix(p.ContainerID, id) {
			p.Containers = append(p.Containers, RemoveContainer{ID: id, Name: name})
		}
	}

	// Images: the devcontainer's image first (it is built FROM the project
	// image, so it has to go before it), by every tag it has.
	seen := map[string]bool{}
	addImage := func(img RemoveImage) {
		if !seen[img.target()] {
			seen[img.target()] = true
			p.Images = append(p.Images, img)
		}
	}
	if p.ImageID != "" {
		tags, err := dockerLines("image", "inspect", "--format", "{{json .RepoTags}}", p.ImageID)
		var refs []string
		if err == nil && len(tags) > 0 {
			json.Unmarshal([]byte(tags[0]), &refs)
		}
		for _, ref := range refs {
			addImage(RemoveImage{ID: p.ImageID, Ref: ref})
		}
		if len(refs) == 0 {
			addImage(RemoveImage{ID: p.ImageID})
		}
	}
	lines, err = dockerLines("images", "--no-trunc", "--filter", filter, "--format", "{{.ID}}\t{{.Repository}}:{{.Tag}}")
	if err != nil {
		return err
	}
	for _, line := range lines {
		id, ref, _ := strings.Cut(line, "\t")
		if strings.Contains(ref, "<none>") {
			ref = ""
		}
		addImage(RemoveImage{ID: id, Ref: ref})
	}

	if p.Networks, err = dockerLines("network", "ls", "--filter", filter, "--format", "{{.Name}}"); err != nil {
		return err
	}
	p.Volumes, err = dockerLines("volume", "ls", "--filter", filter, "--format", "{{.Name}}")
	return err
}

// Describe lists what the plan removes, one indented line per kind, for the
// confirmation prompt of a Compose project.
func (p *RemovePlan) Describe() string {
	var b strings.Builder
	line := func(kind string, names []string) {
		if len(names) > 0 {
			fmt.Fprintf(&b, "  %-11s %s\n", kind+":", strings.Join(names, ", "))
		}
	}
	containers := make([]string, len(p.Containers))
	for i, c := range p.Containers {
		containers[i] = c.Name
	}
	images := make([]string, len(p.Images))
	for i, img := range p.Images {
		images[i] = img.String()
	}
	line("containers", containers)
	line("images", images)
	line("networks", p.Networks)
	if p.RemoveVolumes {
		line("volumes", p.Volumes)
	} else if len(p.Volumes) > 0 {
		fmt.Fprintf(&b, "  Volumes are kept (use --volumes to remove them): %s\n", strings.Join(p.Volumes, ", "))
	}
	return b.String()
}

// Execute removes what the plan lists. A single-container devcontainer is
// stopped and removed, then its image. A Compose project is removed resource
// by resource: a failure is reported but does not stop the rest, and the
// result lists what was removed.
func (p *RemovePlan) Execute() (*RemoveResult, error) {
	if p.ComposeProject == "" {
		return &RemoveResult{}, p.removeContainerAndImage()
	}

	result := &RemoveResult{}
	var errs []error
	run := func(args ...string) bool {
		if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("docker %s: %w\noutput: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output))))
			return false
		}
		return true
	}

	// Stop all at once: docker stops them in parallel, and stopping a
	// stopped container succeeds.
	stopArgs := []string{"stop"}
	for _, c := range p.Containers {
		stopArgs = append(stopArgs, c.ID)
	}
	run(stopArgs...)
	for _, c := range p.Containers {
		args := []string{"rm", c.ID}
		if p.RemoveVolumes {
			args = []string{"rm", "-v", c.ID}
		}
		if run(args...) {
			result.Containers = append(result.Containers, c.Name)
		}
	}
	for _, img := range p.Images {
		if run("rmi", img.target()) {
			result.Images = append(result.Images, img.String())
		}
	}
	for _, network := range p.Networks {
		if run("network", "rm", network) {
			result.Networks = append(result.Networks, network)
		}
	}
	for _, volume := range p.Volumes {
		if !p.RemoveVolumes {
			result.KeptVolumes = append(result.KeptVolumes, volume)
		} else if run("volume", "rm", volume) {
			result.Volumes = append(result.Volumes, volume)
		}
	}
	return result, errors.Join(errs...)
}

// removeContainerAndImage removes a single-container devcontainer.
func (p *RemovePlan) removeContainerAndImage() error {
	// Stop if running
	if IsContainerRunning(p.ContainerID) {
		stopCmd := exec.Command("docker", "stop", p.ContainerID)
		if output, err := stopCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to stop container: %w\noutput: %s", err, string(output))
		}
	}

	// Remove container
	rmCmd := exec.Command("docker", "rm", p.ContainerID)
	if output, err := rmCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove container: %w\noutput: %s", err, string(output))
	}

	// Remove image
	if p.ImageID != "" {
		rmiCmd := exec.Command("docker", "rmi", p.ImageID)
		if output, err := rmiCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("container removed but failed to remove image: %w\noutput: %s", err, string(output))
		}
	}
	return nil
}

// Lines describes the result, one line per kind, for the log.
func (r *RemoveResult) Lines() []string {
	var lines []string
	add := func(format string, names []string) {
		if len(names) > 0 {
			lines = append(lines, fmt.Sprintf(format, strings.Join(names, ", ")))
		}
	}
	add("Removed containers: %s", r.Containers)
	add("Removed images: %s", r.Images)
	add("Removed networks: %s", r.Networks)
	add("Removed volumes: %s", r.Volumes)
	add("Kept volumes (use --volumes to remove them): %s", r.KeptVolumes)
	return lines
}

// dockerLines runs docker and returns its non-empty output lines.
func dockerLines(args ...string) ([]string, error) {
	output, err := exec.Command("docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
