package main

import (
	"log"
	"os"

	"github.com/mpm/dworm/internal/endpoint"
)

func main() {
	server := endpoint.NewServer()

	if err := server.Run(); err != nil {
		log.Printf("Endpoint error: %v", err)
		os.Exit(1)
	}
}
