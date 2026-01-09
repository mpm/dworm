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

// DevcontainerUp starts a devcontainer and returns container info
func DevcontainerUp(workspacePath string) (*ContainerInfo, error) {
	cmd := exec.Command("devcontainer", "up", "--workspace-folder", workspacePath)

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

// GetContainerID gets the container ID for a workspace path
func GetContainerID(workspacePath string) (string, error) {
	// Use devcontainer to get container info
	cmd := exec.Command("devcontainer", "up", "--workspace-folder", workspacePath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var result struct {
		ContainerID string `json:"containerId"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", err
	}

	return result.ContainerID, nil
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
