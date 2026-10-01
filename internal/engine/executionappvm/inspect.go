package executionappvm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// InspectInProcess evaluates declarations only inside the already-confined worker.
func InspectInProcess(ctx context.Context, bundle []byte) (json.RawMessage, error) {
	program, err := compileCapabilityBundle(bundle)
	// Invalid source never reaches declaration evaluation or acquires a host bridge.
	if err != nil {
		return nil, errors.New("capability bundle is invalid")
	}
	return inspectCapabilityProgram(ctx, program)
}

// inspectCapabilityProgram validates the exact cached bytecode in a disposable runtime without host authority.
func inspectCapabilityProgram(ctx context.Context, program *goja.Program) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vm := goja.New()
	// Inline-only maps apply to dynamically evaluated source as well as the immutable bundle.
	vm.SetParserOptions(parser.WithSourceMapLoader(rejectExternalSourceMap))
	vm.SetMaxCallStackSize(512)
	stopInterrupt := context.AfterFunc(ctx, func() { vm.Interrupt("capability declaration timed out") })
	defer stopInterrupt()
	// No __fusedHost or Node objects exist here, so top-level code cannot reach provider or DB effects.
	if _, err := vm.RunProgram(program); err != nil {
		return nil, errors.New("capability declaration evaluation failed")
	}
	// Only a complete typed export may publish a manifest.
	if err := validateUnifiedAppDeclaration(vm); err != nil {
		return nil, err
	}
	return readUnifiedAppManifest(vm)
}

// validateUnifiedAppDeclaration requires the manifest to accompany one runnable typed execute export.
func validateUnifiedAppDeclaration(vm *goja.Runtime) error {
	value, err := vm.RunString(`Boolean(globalThis.FusedUnifiedApp &&
      typeof globalThis.FusedUnifiedApp.input?.parse === "function" &&
      typeof globalThis.FusedUnifiedApp.output?.parse === "function" &&
      typeof globalThis.FusedUnifiedApp.execute === "function")`)
	// A manifest without matching executable code cannot be attached to an app version.
	if err != nil || !value.ToBoolean() {
		return errors.New("unified app declaration is invalid")
	}
	return nil
}

// readUnifiedAppManifest accepts only bounded JSON published by the evaluated bundle.
func readUnifiedAppManifest(vm *goja.Runtime) (json.RawMessage, error) {
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
