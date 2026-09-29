package api

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestExecutionBundleDigestPlanAdmission keeps compiler identity canonical before any plan can become immutable state.
func TestExecutionBundleDigestPlanAdmission(t *testing.T) {
	generate := false
	base := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "unified_app", Name: "capability-app", Version: "1.0.0",
		Language: "typescript", Generate: &generate, Bucket: "default", Services: map[string]sdkConfigServiceDoc{
			"crm": {Version: "v1", Operations: []string{"customers.create"}},
		},
	}
	valid := store.UnifiedAppBundleDigest([]byte("compiled capability"))
	for _, testCase := range []struct {
		name, language, digest                  string
		defaultGenerate, enabledGenerate bool
		wantErr                                 bool
	}{
		{name: "authored TypeScript", language: "typescript", digest: valid},
		{name: "missing compiler identity", language: "typescript", wantErr: true},
		{name: "uppercase digest", language: "typescript", digest: strings.ToUpper(valid), wantErr: true},
		{name: "bare digest", language: "typescript", digest: strings.TrimPrefix(valid, "sha256:"), wantErr: true},
		{name: "other language", language: "python", digest: valid, wantErr: true},
		{name: "omitted package fields", language: "typescript", digest: valid, defaultGenerate: true},
		{name: "explicit package", language: "typescript", digest: valid, enabledGenerate: true, wantErr: true},
	} {
		// Each independent input proves the plan boundary rejects an unpinnable code identity.
		t.Run(testCase.name, func(t *testing.T) {
			doc := base
			doc.Language = testCase.language
			doc.BundleDigest = testCase.digest
			// Unified Apps infer their TypeScript runtime and never generate an SDK package.
			if testCase.defaultGenerate {
				doc.Generate = nil
			}
			if testCase.enabledGenerate {
				enabled := true
				doc.Generate = &enabled
			}
			err := validateUnifiedAppConfigDocument(doc)
			// A hosted execute needs its own immutable compiler identity.
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validateSDKConfigDocument() error = %v, want error %t", err, testCase.wantErr)
			}
		})
	}
}

// TestExecutionBundleDigestRequiresServiceScope keeps authored code within reviewed provider authority.
func TestExecutionBundleDigestRequiresServiceScope(t *testing.T) {
	generate := false
	doc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "unified_app", Name: "greeting", Version: "1.0.0",
		Language: "typescript", Generate: &generate, Bucket: "default",
		BundleDigest: store.UnifiedAppBundleDigest([]byte("authored execute")),
	}
	// A pinned bundle cannot authorize unselected provider operations.
	if err := validateUnifiedAppConfigDocument(doc); err == nil {
		t.Fatal("service-free Unified App accepted")
	}
	doc.Kind = "sdk"
	// SDKs also retain their ordinary service-selection rule.
	if err := validateSDKConfigDocument(doc); err == nil {
		t.Fatal("raw SDK without services was accepted")
	}
}

// TestUnifiedAppRuntimeRejectsEmptyScope prevents a compiler digest from widening provider authority.
func TestUnifiedAppRuntimeRejectsEmptyScope(t *testing.T) {
	scope, err := appRuntimeForApply(persistAppRuntimeParams{
		bucketID: uuid.New(), bucketName: "default", kind: store.AppKindUnifiedApp,
		scopeSchemaVersion: models.AppScopeSchemaVersion,
		bundleDigest:       store.UnifiedAppBundleDigest([]byte("greeting")),
	})
	// Runtime publication requires at least one exact selected provider operation.
	if err == nil {
		t.Fatalf("empty Unified App scope accepted: %#v", scope)
	}
}
