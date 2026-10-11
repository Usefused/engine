package mcpcatalog

import (
	"context"
	"errors"
	"github.com/Usefused/engine/internal/shared/features"
	"testing"
)

// TestDisabledProviderBoundary rejects direct callers before endpoint validation or any network traffic.
func TestDisabledProviderBoundary(t *testing.T) {
	t.Setenv(features.ImportedMCPEnvironment, "false")
	_, discoverErr := Discover(context.Background(), "invalid", "secret")
	_, invokeErr := Invoke(context.Background(), "invalid", "secret", "tools/call", nil)
	for _, err := range []error{discoverErr, invokeErr} {
		// A stable disabled error proves neither endpoint validation nor provider work ran.
		if !errors.Is(err, features.ErrImportedMCPDisabled) {
			t.Fatal(err)
		}
	}
}
