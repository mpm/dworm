package endpoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// WriteGitConfig writes the git configuration to ~/.gitconfig
func WriteGitConfig(content string) error {
	if content == "" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	gitconfigPath := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write git config: %w", err)
	}

	return nil
}

// ConfigureCredentialHelper adds the dworm credential helper to git config
func ConfigureCredentialHelper(helperPath string) error {
	cmd := exec.Command("git", "config", "--global", "credential.helper", helperPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure credential helper: %w\noutput: %s", err, string(output))
	}
	return nil
}

// CreateCredentialHelper creates the credential helper script
func CreateCredentialHelper(helperPath string) error {
	// The credential helper script invokes the endpoint binary in credential-helper mode
	script := `#!/bin/sh
# dworm git credential helper
# Forwards credential requests to host via dworm endpoint
exec /tmp/dworm_endpoint --credential-helper "$1"
`
	if err := os.WriteFile(helperPath, []byte(script), 0755); err != nil {
		return fmt.Errorf("failed to create credential helper script: %w", err)
	}
	return nil
}
