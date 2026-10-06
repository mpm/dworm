package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// ContainerInfo holds information about a running devcontainer
type ContainerInfo struct {
	ContainerID   string
	ContainerName string
	WorkspaceDir  string
	RemoteUser    string
}

// DevcontainerUp starts a devcontainer and returns container info.
// If configPath is non-empty, it is passed to devcontainer CLI as --config.
func DevcontainerUp(workspacePath, configPath string) (*ContainerInfo, error) {
	args := []string{"up", "--workspace-folder", workspacePath}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	cmd := exec.Command("devcontainer", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("devcontainer up failed: %w\nstderr: %s", err, stderr.String())
	}

	// Parse JSON output
	var result struct {
		Outcome            string `json:"outcome"`
		ContainerID        string `json:"containerId"`
		RemoteUser         string `json:"remoteUser"`
		RemoteWorkspaceDir string `json:"remoteWorkspaceFolder"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse devcontainer output: %w\noutput: %s", err, stdout.String())
	}

	if result.Outcome != "success" {
		return nil, fmt.Errorf("devcontainer up failed with outcome: %s", result.Outcome)
	}

	// Get container name
	name, err := getContainerName(result.ContainerID)
	if err != nil {
		name = result.ContainerID[:12] // Use short ID as fallback
	}

	return &ContainerInfo{
		ContainerID:   result.ContainerID,
		ContainerName: name,
		WorkspaceDir:  result.RemoteWorkspaceDir,
		RemoteUser:    result.RemoteUser,
	}, nil
}

// DevcontainerDown stops a devcontainer
func DevcontainerDown(workspacePath string) error {
	// Find container by label
	cmd := exec.Command("docker", "ps", "-q", "--filter",
		fmt.Sprintf("label=devcontainer.local_folder=%s", workspacePath))

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to find container: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return fmt.Errorf("no running container found for %s", workspacePath)
	}
	containerID := lines[0]

	// Stop the container
	stopCmd := exec.Command("docker", "stop", containerID)
	if output, err := stopCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to stop container: %w\noutput: %s", err, string(output))
	}

	return nil
}

// getContainerName gets the name of a container by ID
func getContainerName(containerID string) (string, error) {
	cmd := exec.Command("docker", "inspect", "--format", "{{.Name}}", containerID)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", err
	}

	name := strings.TrimSpace(stdout.String())
	name = strings.TrimPrefix(name, "/")
	return name, nil
}

// GetContainerID gets the container ID for a workspace path using Docker label query.
// This is a read-only operation that doesn't start or modify containers.
func GetContainerID(workspacePath string) (string, error) {
	// Query Docker for container with matching devcontainer label
	cmd := exec.Command("docker", "ps", "-q", "--filter",
		fmt.Sprintf("label=devcontainer.local_folder=%s", workspacePath))

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to query containers: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("no running container found for %s", workspacePath)
	}

	return lines[0], nil
}

// DevcontainerRemove stops and removes a devcontainer and optionally its image.
func DevcontainerRemove(workspacePath string, removeImage bool) error {
	// Find container (running or stopped)
	cmd := exec.Command("docker", "ps", "-aq", "--filter",
		fmt.Sprintf("label=devcontainer.local_folder=%s", workspacePath))

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to find container: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return fmt.Errorf("no container found for %s", workspacePath)
	}
	containerID := lines[0]

	// Get image ID before removing the container
	var imageID string
	if removeImage {
		inspectCmd := exec.Command("docker", "inspect", "--format", "{{.Image}}", containerID)
		var inspectOut bytes.Buffer
		inspectCmd.Stdout = &inspectOut
		if err := inspectCmd.Run(); err == nil {
			imageID = strings.TrimSpace(inspectOut.String())
		}
	}

	// Stop if running
	if IsContainerRunning(containerID) {
		stopCmd := exec.Command("docker", "stop", containerID)
		if output, err := stopCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to stop container: %w\noutput: %s", err, string(output))
		}
	}

	// Remove container
	rmCmd := exec.Command("docker", "rm", containerID)
	if output, err := rmCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove container: %w\noutput: %s", err, string(output))
	}

	// Remove image
	if removeImage && imageID != "" {
		rmiCmd := exec.Command("docker", "rmi", imageID)
		if output, err := rmiCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("container removed but failed to remove image: %w\noutput: %s", err, string(output))
		}
	}

	return nil
}

// DevcontainerRebuild removes the existing container and rebuilds it.
func DevcontainerRebuild(workspacePath, configPath string) (*ContainerInfo, error) {
	args := []string{"up", "--remove-existing-container", "--workspace-folder", workspacePath}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	cmd := exec.Command("devcontainer", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("devcontainer rebuild failed: %w\nstderr: %s", err, stderr.String())
	}

	var result struct {
		Outcome            string `json:"outcome"`
		ContainerID        string `json:"containerId"`
		RemoteUser         string `json:"remoteUser"`
		RemoteWorkspaceDir string `json:"remoteWorkspaceFolder"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse devcontainer output: %w\noutput: %s", err, stdout.String())
	}

	if result.Outcome != "success" {
		return nil, fmt.Errorf("devcontainer rebuild failed with outcome: %s", result.Outcome)
	}

	name, err := getContainerName(result.ContainerID)
	if err != nil {
		name = result.ContainerID[:12]
	}

	return &ContainerInfo{
		ContainerID:   result.ContainerID,
		ContainerName: name,
		WorkspaceDir:  result.RemoteWorkspaceDir,
		RemoteUser:    result.RemoteUser,
	}, nil
}

// IsContainerRunning checks if a container is running
func IsContainerRunning(containerID string) bool {
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", containerID)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return false
	}

	return strings.TrimSpace(stdout.String()) == "true"
}

// containerMount is the subset of `docker inspect` mount data dworm uses.
type containerMount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// ResolveWorkspaceFolder returns the in-container path of workspacePath by
// finding the bind mount that contains it (the devcontainer workspace mount,
// or a parent directory mounted by compose setups). It returns "" when no
// mount matches, so docker falls back to the image's working directory.
func ResolveWorkspaceFolder(containerID, workspacePath string) string {
	output, err := exec.Command("docker", "inspect", "--type", "container", "--format", "{{json .Mounts}}", containerID).Output()
	if err != nil {
		return ""
	}
	var mounts []containerMount
	if err := json.Unmarshal(output, &mounts); err != nil {
		return ""
	}
	return workspaceFolderFromMounts(mounts, workspacePath)
}

func workspaceFolderFromMounts(mounts []containerMount, workspacePath string) string {
	best, bestLen := "", -1
	for _, m := range mounts {
		if m.Source == "" || m.Destination == "" {
			continue
		}
		rel, err := filepath.Rel(m.Source, workspacePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if len(m.Source) > bestLen {
			best, bestLen = path.Join(m.Destination, filepath.ToSlash(rel)), len(m.Source)
		}
	}
	return best
}
