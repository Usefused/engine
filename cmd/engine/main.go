package main

import "github.com/Usefused/engine/cmd/engine/cmd"

// main is the entrypoint for the Engine binary.
// By delegating immediately to the `cmd` package, we keep the root executable
// extremely thin. This allows the Cobra CLI framework to handle all routing, flag parsing,
// and execution logic, making the Engine fully testable and modular.
func main() {
	cmd.Execute()
}
