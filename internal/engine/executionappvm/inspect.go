package executionappvm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dop251/goja"
)

// InspectInProcess evaluates declarations only inside the already-confined worker.
func InspectInProcess(ctx context.Context, bundle []byte) (json.RawMessage, error) {
	// A missing or oversized script cannot enter the build-time interpreter.
	if len(bundle) == 0 || len(bundle) > MaxBundleBytes {
		return nil, errors.New("capability bundle is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vm := goja.New()
	vm.SetMaxCallStackSize(512)
	stopInterrupt := context.AfterFunc(ctx, func() { vm.Interrupt("capability declaration timed out") })
	defer stopInterrupt()
	program, err := goja.Compile("fused-capability-declarations.js", string(bundle), true)
	// Compile errors cannot be replaced by a caller-provided manifest.
	if err != nil {
		return nil, errors.New("capability bundle is invalid")
	}
	// No __fusedHost or Node objects exist here, so top-level code cannot reach provider or DB effects.
	if _, err := vm.RunProgram(program); err != nil {
		return nil, errors.New("capability declaration evaluation failed")
	}
	if err := validateExecutionAppDeclaration(vm); err != nil {
		return nil, err
	}
	return readExecutionAppManifest(vm)
}

// validateExecutionAppDeclaration requires the manifest to accompany one runnable typed execute export.
func validateExecutionAppDeclaration(vm *goja.Runtime) error {
	value, err := vm.RunString(`Boolean(globalThis.FusedExecutionApp &&
      typeof globalThis.FusedExecutionApp.input?.parse === "function" &&
      typeof globalThis.FusedExecutionApp.output?.parse === "function" &&
      typeof globalThis.FusedExecutionApp.execute === "function")`)
	// A manifest without matching executable code cannot be attached to an app version.
	if err != nil || !value.ToBoolean() {
		return errors.New("execution app declaration is invalid")
	}
	return nil
}

// readExecutionAppManifest accepts only bounded JSON published by the evaluated bundle.
func readExecutionAppManifest(vm *goja.Runtime) (json.RawMessage, error) {
	value, err := vm.RunString("JSON.stringify(globalThis.FusedExecutionManifest)")
	// Every bundle must publish one JSON manifest derived from its actual exports.
	if err != nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return nil, errors.New("capability manifest is unavailable")
	}
	manifest := []byte(value.String())
	// Manifest bytes are bounded independently of the compiled source size.
	if len(manifest) == 0 || len(manifest) > 1<<20 || !json.Valid(manifest) {
		return nil, errors.New("capability manifest is invalid")
	}
	return manifest, nil
}
