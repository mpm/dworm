package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/mpm/dworm/internal/endpoint"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--shell-env" {
		text, err := endpoint.ShellEnvironment()
		if err != nil {
			log.Printf("Environment reload: %v", err)
			os.Exit(1)
		}
		fmt.Print(text)
		return
	}
	if len(os.Args) >= 4 && (os.Args[1] == "--with-env" || os.Args[1] == "--shell") {
		var overrides map[string]string
		if err := json.Unmarshal([]byte(os.Args[2]), &overrides); err != nil {
			log.Print("Invalid environment overrides")
			os.Exit(1)
		}
		if err := endpoint.RunWithEnvironment(overrides, os.Args[1] == "--shell", os.Args[3:]); err != nil {
			log.Printf("Environment launcher: %v", err)
			os.Exit(1)
		}
		return
	}
	// Check for --credential-helper flag
	if len(os.Args) >= 3 && os.Args[1] == "--credential-helper" {
		action := os.Args[2]
		socketPath := "/tmp/dworm-git-credential.sock"
		if err := endpoint.RunCredentialHelper(socketPath, action); err != nil {
			log.Printf("Credential helper error: %v", err)
			os.Exit(1)
		}
		return
	}

	server := endpoint.NewServer()

	if err := server.Run(); err != nil {
		log.Printf("Endpoint error: %v", err)
		os.Exit(1)
	}
}
