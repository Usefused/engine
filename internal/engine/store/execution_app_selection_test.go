package store

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestValidateAppApplySelectionsRejectsEmptyScope keeps compiler identity separate from provider authority.
func TestValidateAppApplySelectionsRejectsEmptyScope(t *testing.T) {
	digest := UnifiedAppBundleDigest([]byte("authored execute"))
	base := AppRuntime{Kind: AppKindSDK, ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: []byte(`[]`), BundleDigest: digest}
	for _, testCase := range []struct {
		name, digest string
		kind         AppKind
		selections   []byte
		wantError    bool
	}{
		{name: "authored Unified App", kind: AppKindUnifiedApp, digest: digest, selections: []byte(`[]`), wantError: true},
		{name: "SDK carrying digest", kind: AppKindSDK, digest: digest, selections: []byte(`[]`), wantError: true},
		{name: "raw SDK", kind: AppKindSDK, selections: []byte(`[]`), wantError: true},
		{name: "MCP", kind: AppKindMCP, digest: digest, selections: []byte(`[]`), wantError: true},
		{name: "missing selection field", kind: AppKindUnifiedApp, digest: digest, selections: nil, wantError: true},
		{name: "null selection field", kind: AppKindUnifiedApp, digest: digest, selections: []byte(`null`), wantError: true},
	} {
		// Publication never infers provider authority from an absent or wrong-kind scope.
		t.Run(testCase.name, func(t *testing.T) {
			scope := base
			scope.Kind, scope.BundleDigest, scope.Selections = testCase.kind, testCase.digest, testCase.selections
			err := validateAppApplySelections(scope)
			// A digest never substitutes for an exact selected provider operation.
			if (err != nil) != testCase.wantError {
				t.Fatalf("validateAppApplySelections() error = %v, want error %t", err, testCase.wantError)
			}
		})
	}
}
