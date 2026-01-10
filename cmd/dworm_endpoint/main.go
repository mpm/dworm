package main

import (
	"log"
	"os"

	"github.com/mpm/dworm/internal/endpoint"
)

func main() {
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
