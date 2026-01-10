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
