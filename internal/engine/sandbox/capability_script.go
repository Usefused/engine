package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/executionappvm"
)

const (
	maxCapabilityBundleBytes = executionappvm.MaxBundleBytes
	maxCapabilityInputBytes  = executionappvm.MaxInputBytes
	maxCapabilityOutputBytes = executionappvm.MaxOutputBytes
	maxCapabilityHostCalls   = executionappvm.MaxHostCalls
	maxCapabilityParallel    = executionappvm.MaxParallel
	capabilityWallTime       = executionappvm.WallTime
)

// CapabilityScriptHost owns the only effects available to authored JavaScript.
type CapabilityScriptHost = executionappvm.Host

// RunCapabilityScript invokes the single authored execute in an isolated worker.
func RunCapabilityScript(ctx context.Context, bundle []byte, input json.RawMessage, host CapabilityScriptHost) (json.RawMessage, error) {
	control, err := NewCapabilityDeterminism()
	// Every live invocation needs fresh Engine-owned replay controls.
	if err != nil {
		return nil, err
	}
	return RunCapabilityScriptWithDeterminism(ctx, bundle, input, host, control)
}

// RunCapabilityScriptWithDeterminism pins JavaScript time and randomness across live and replay workers.
func RunCapabilityScriptWithDeterminism(ctx context.Context, bundle []byte, input json.RawMessage, host CapabilityScriptHost, control CapabilityDeterminism) (json.RawMessage, error) {
	// Caller input and controls are bounded before starting a child process.
	if host == nil || len(bundle) == 0 || len(bundle) > maxCapabilityBundleBytes || len(input) > maxCapabilityInputBytes || !json.Valid(input) {
		return nil, errors.New("capability invocation is invalid")
	}
	// Replay cannot silently use wall time or Goja's default random source.
	if !control.Valid() {
		return nil, errors.New("capability deterministic controls are invalid")
	}
	return runCapabilityProcess(ctx, capabilityProcessFrame{Kind: "run", Bundle: bundle, Input: input, Determinism: control}, host)
}

// runCapabilityScriptInProcess exposes the dependency-light interpreter only to package tests.
func runCapabilityScriptInProcess(ctx context.Context, bundle []byte, input json.RawMessage, host CapabilityScriptHost) (json.RawMessage, error) {
	return executionappvm.RunInProcess(ctx, bundle, input, host)
}

// runCapabilityScriptInProcessWithDeterminism exposes deterministic interpreter checks to package tests.
func runCapabilityScriptInProcessWithDeterminism(ctx context.Context, bundle []byte, input json.RawMessage, host CapabilityScriptHost, control CapabilityDeterminism) (json.RawMessage, error) {
	return executionappvm.RunInProcessWithDeterminism(ctx, bundle, input, host, control)
}
