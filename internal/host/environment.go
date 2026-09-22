package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/mpm/dworm/internal/config"
)

type limitedOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 512*1024 {
		b.cancel()
		return 0, fmt.Errorf("host environment output exceeds 512 KiB")
	}
	return b.buffer.Write(p)
}

func RunEnvironmentCommand(ctx context.Context, root string, cfg config.HostEnv, stderr io.Writer) (map[string]string, error) {
	if len(cfg.Command) == 0 {
		return map[string]string{}, nil
	}
	_, timeout, err := cfg.Durations()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	configureEnvironmentProcess(cmd)
	cmd.Dir, cmd.Env, cmd.Stderr = root, os.Environ(), stderr
	cmd.WaitDelay = time.Second
	out := limitedOutput{cancel: cancel}
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("host environment command failed: %w", err)
	}
	env, err := config.ParseEnv(out.buffer.Bytes())
	if err != nil {
		return nil, fmt.Errorf("host environment command: %w", err)
	}
	return env, nil
}

// RefreshEnvironment serializes executions; a failed run leaves the last snapshot intact.
func RefreshEnvironment(ctx context.Context, interval time.Duration, run func() error, report func(error)) {
	if interval == 0 {
		return
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := run(); err != nil && ctx.Err() == nil {
				report(err)
			}
		}
	}
}
