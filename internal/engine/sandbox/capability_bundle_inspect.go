package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/executionappvm"
)

// InspectCapabilityBundle evaluates declarations in an OS-confined worker.
func InspectCapabilityBundle(ctx context.Context, bundle []byte) (json.RawMessage, error) {
	// Source size is bounded before the parent allocates a child protocol frame.
	if len(bundle) == 0 || len(bundle) > maxCapabilityBundleBytes {
		return nil, errors.New("capability bundle is invalid")
	}
	return runCapabilityProcess(ctx, capabilityProcessFrame{Kind: "inspect", Bundle: bundle}, nil)
}

// inspectCapabilityBundleInProcess exposes the same Goja inspector to package tests.
func inspectCapabilityBundleInProcess(ctx context.Context, bundle []byte) (json.RawMessage, error) {
	return executionappvm.InspectInProcess(ctx, bundle)
}
