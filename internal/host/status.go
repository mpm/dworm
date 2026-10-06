package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Status is the machine-readable status of a workspace (`dworm status --json`).
type Status struct {
	WorkspacePath string           `json:"workspace_path"`
	Container     *ContainerStatus `json:"container"`
	Up            UpStatus         `json:"up"`
	DwormVersion  string           `json:"dworm_version"`
}

// ContainerStatus describes the workspace's devcontainer.
type ContainerStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Running         bool   `json:"running"`
	RemoteUser      string `json:"remote_user"`
	WorkspaceFolder string `json:"workspace_folder"`
}

// UpStatus describes the workspace's instance. Only Running,
// EndpointConnected and Ports are present when no instance holds the
// workspace lock; Clients only when the instance answered op status.
type UpStatus struct {
	Running           bool        `json:"running"`
	PID               int         `json:"pid,omitempty"`
	StartedAt         *time.Time  `json:"started_at,omitempty"`
	State             string      `json:"state,omitempty"`
	Reason            string      `json:"reason,omitempty"`
	Mode              string      `json:"mode,omitempty"`
	EndpointConnected bool        `json:"endpoint_connected"`
	Reconnects        *int        `json:"reconnects,omitempty"`
	Clients           *int        `json:"clients,omitempty"`
	Ports             []StatePort `json:"ports"`
	ExecSocket        string      `json:"exec_socket,omitempty"`
	LogPath           string      `json:"log_path,omitempty"`
	DwormVersion      string      `json:"dworm_version,omitempty"`
}

// RunningInstance returns the state of the `dworm up` holding the workspace
// lock, or nil when none is running.
func RunningInstance(workspacePath string) (*InstanceState, error) {
	paths := InstancePathsFor(workspacePath)
	running, err := InstanceRunning(paths)
	if err != nil || !running {
		return nil, err
	}
	state, err := ReadInstanceState(paths.State)
	if err != nil {
		// Locked, but the state file is not written yet or unreadable.
		return &InstanceState{WorkspacePath: workspacePath}, nil
	}
	return state, nil
}

// WorkspaceFolder returns the in-container workspace folder: the one reported
// by a running `dworm up` for this container, else ResolveWorkspaceFolder.
func WorkspaceFolder(containerID, workspacePath string) string {
	if state, _ := RunningInstance(workspacePath); state != nil && state.WorkspaceFolder != "" && state.ContainerID == containerID {
		return state.WorkspaceFolder
	}
	return ResolveWorkspaceFolder(containerID, workspacePath)
}

// liveInstance returns the state of the running instance: live from op
// status when it answers (live = true), else from the state file.
func liveInstance(workspacePath string) (state *InstanceState, live bool, err error) {
	paths := InstancePathsFor(workspacePath)
	running, err := InstanceRunning(paths)
	if err != nil || !running {
		return nil, false, err
	}
	if state, err := QueryInstance(paths.Socket); err == nil {
		return state, true, nil
	}
	state, err = RunningInstance(workspacePath)
	return state, false, err
}

// GetStatus collects the workspace status. A missing container is not an error.
func GetStatus(workspacePath, dwormVersion string) (*Status, error) {
	status := &Status{WorkspacePath: workspacePath, DwormVersion: dwormVersion, Up: UpStatus{Ports: []StatePort{}}}

	instance, live, err := liveInstance(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("check dworm instance: %w", err)
	}
	if instance != nil {
		reconnects := instance.Reconnects
		status.Up = UpStatus{
			Running:           true,
			PID:               instance.PID,
			State:             instance.State,
			Reason:            instance.Reason,
			Mode:              instance.Mode,
			EndpointConnected: instance.EndpointConnected,
			Reconnects:        &reconnects,
			Ports:             instance.Ports,
			ExecSocket:        instance.ExecSocket,
			LogPath:           instance.LogPath,
			DwormVersion:      instance.DwormVersion,
		}
		if live {
			clients := instance.Clients
			status.Up.Clients = &clients
		}
		if !instance.StartedAt.IsZero() {
			status.Up.StartedAt = &instance.StartedAt
		}
		if status.Up.Ports == nil {
			status.Up.Ports = []StatePort{}
		}
	}

	container, err := inspectWorkspaceContainer(workspacePath)
	if err != nil || container == nil {
		return status, err
	}
	status.Container = container
	if instance != nil && strings.HasPrefix(instance.ContainerID, container.ID) {
		if instance.RemoteUser != "" {
			container.RemoteUser = instance.RemoteUser
		}
		if instance.WorkspaceFolder != "" {
			container.WorkspaceFolder = instance.WorkspaceFolder
		}
	}
	return status, nil
}

type containerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []containerMount `json:"Mounts"`
}

// inspectWorkspaceContainer finds the workspace's container, running or not.
func inspectWorkspaceContainer(workspacePath string) (*ContainerStatus, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", "ps", "-aq", "--filter", "label=devcontainer.local_folder="+workspacePath)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("query containers: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	ids := strings.Fields(stdout.String())
	if len(ids) == 0 {
		return nil, nil
	}

	output, err := exec.Command("docker", "inspect", "--type", "container", ids[0]).Output()
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", ids[0], err)
	}
	var inspected []containerInspect
	if err := json.Unmarshal(output, &inspected); err != nil || len(inspected) == 0 {
		return nil, fmt.Errorf("parse docker inspect output for %s", ids[0])
	}
	c := inspected[0]
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	return &ContainerStatus{
		ID:              id,
		Name:            strings.TrimPrefix(c.Name, "/"),
		Running:         c.State.Running,
		RemoteUser:      remoteUserFromMetadata(c.Config.Labels["devcontainer.metadata"], c.Config.User),
		WorkspaceFolder: workspaceFolderFromMounts(c.Mounts, workspacePath),
	}, nil
}

// remoteUserFromMetadata applies devcontainer metadata merge order: the last
// remoteUser wins, then the last containerUser, then the image user.
func remoteUserFromMetadata(metadata, imageUser string) string {
	var entries []struct {
		RemoteUser    string `json:"remoteUser"`
		ContainerUser string `json:"containerUser"`
	}
	if json.Unmarshal([]byte(metadata), &entries) != nil {
		return imageUser
	}
	remote, container := "", ""
	for _, e := range entries {
		if e.RemoteUser != "" {
			remote = e.RemoteUser
		}
		if e.ContainerUser != "" {
			container = e.ContainerUser
		}
	}
	switch {
	case remote != "":
		return remote
	case container != "":
		return container
	default:
		return imageUser
	}
}
