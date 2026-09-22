//go:build !unix

package host

import "os/exec"

func configureEnvironmentProcess(cmd *exec.Cmd) {}
