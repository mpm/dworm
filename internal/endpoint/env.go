package endpoint

import (
	"os"
)

// SetEnvironment sets environment variables in the current process
func SetEnvironment(envVars map[string]string) error {
	for key, value := range envVars {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

// GetCurrentEnv returns a map of all current environment variables
func GetCurrentEnv() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				env[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return env
}
