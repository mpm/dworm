package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ContainerInfo holds information about a running devcontainer
type ContainerInfo struct {
	ContainerID   string
	ContainerName string
	WorkspaceDir  string
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

	containerID := strings.TrimSpace(stdout.String())
	if containerID == "" {
		return fmt.Errorf("no running container found for %s", workspacePath)
	}

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

	containerID := strings.TrimSpace(stdout.String())
	if containerID == "" {
		return "", fmt.Errorf("no running container found for %s", workspacePath)
	}

	return containerID, nil
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

	containerID := strings.TrimSpace(stdout.String())
	if containerID == "" {
		return fmt.Errorf("no container found for %s", workspacePath)
	}

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
