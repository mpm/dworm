package endpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/mpm/dworm/internal/config"
)

var environmentDir = "/tmp/dworm"

// PublishEnvironment uses rename so readers always see one complete generation.
func PublishEnvironment(env map[string]string) error {
	if err := config.Validate(env); err != nil {
		return err
	}
	if err := os.MkdirAll(environmentDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(environmentDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("environment directory must be a private directory (0700)")
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(environmentDir, "environment.json"), data)
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".environment-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readEnvironment() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(environmentDir, "environment.json"))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return env, config.Validate(env)
}

func inheritedEnvironment() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, _ := strings.Cut(entry, "=")
		env[k] = v
	}
	return env
}

func jsonEnv(name string, value interface{}) {
	data, _ := json.Marshal(value)
	os.Setenv(name, string(data))
}

// RunWithEnvironment replaces this process, preserving signals and exit codes.
func RunWithEnvironment(overrides map[string]string, shell bool, args []string) error {
	if err := config.Validate(overrides); err != nil {
		return err
	}
	env, err := readEnvironment()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("missing command")
	}
	base := inheritedEnvironment()
	// A nested launcher starts a new independent shell state.
	for k := range base {
		if strings.HasPrefix(k, "_DWORM_") {
			delete(base, k)
		}
	}
	if shell {
		if err := os.MkdirAll(environmentDir, 0700); err != nil {
			return err
		}
		if err := atomicWrite(environmentDir+"/bashrc", []byte(bashRC)); err != nil {
			return err
		}
		jsonEnv("_DWORM_BASE", base)
		jsonEnv("_DWORM_OVERRIDES", overrides)
		jsonEnv("_DWORM_KEYS", config.Merge(env, overrides))
		args = []string{"/bin/bash", "--rcfile", environmentDir + "/bashrc"}
	}
	for k, v := range config.Merge(env, overrides) {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, os.Environ())
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// ShellEnvironment emits only validated assignments, with literal shell quoting.
func ShellEnvironment() (string, error) {
	env, err := readEnvironment()
	if err != nil {
		return "", err
	}
	var base, overrides, previous map[string]string
	for name, target := range map[string]*map[string]string{"_DWORM_BASE": &base, "_DWORM_OVERRIDES": &overrides, "_DWORM_KEYS": &previous} {
		if err := json.Unmarshal([]byte(os.Getenv(name)), target); err != nil {
			return "", fmt.Errorf("invalid shell environment state")
		}
	}
	if err := config.Validate(previous); err != nil {
		return "", err
	}
	if err := config.Validate(overrides); err != nil {
		return "", err
	}
	current := config.Merge(env, overrides)
	keys := make([]string, 0)
	for k := range config.Merge(previous, current) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, k := range keys {
		value, ok := current[k]
		if !ok {
			value, ok = base[k]
		}
		if ok {
			fmt.Fprintf(&out, "export %s=%s\n", k, shellQuote(value))
		} else {
			fmt.Fprintf(&out, "unset %s\n", k)
		}
	}
	data, _ := json.Marshal(current)
	fmt.Fprintf(&out, "export _DWORM_KEYS=%s\n", shellQuote(string(data)))
	return out.String(), nil
}

const bashRC = `# dworm-managed initialization; user configuration remains authoritative.
if [[ -f ~/.bashrc ]]; then source ~/.bashrc; fi
_dworm_reload_environment() {
    local status=$? updates
    if updates=$(/tmp/dworm_endpoint --shell-env); then
        eval "$updates"
    fi
    return "$status"
}
PROMPT_COMMAND+=(_dworm_reload_environment)
_dworm_reload_environment
`
