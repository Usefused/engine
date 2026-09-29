package main

import (
	"os"

	"github.com/Usefused/engine/internal/engine/executionappvm"
)

// main runs one disposable, isolated Unified App bundle and exits with its protocol outcome.
func main() {
	// Only the Engine parent may start a worker with its sanitized protocol environment.
	if os.Getenv("FUSED_CAPABILITY_WORKER") != "1" {
		os.Exit(1)
	}
	os.Exit(executionappvm.RunWorker())
}
