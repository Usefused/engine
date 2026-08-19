package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestAppKindValid(t *testing.T) {
	for _, kind := range []AppKind{AppKindSDK, AppKindMCP} {
		if !kind.Valid() {
			t.Fatalf("%q should be a valid app kind", kind)
		}
	}
	if AppKind("artifact").Valid() {
		t.Fatal("legacy artifact kind must not be accepted")
	}
}

func TestAppStatusRunnable(t *testing.T) {
	if AppStatusBuilding.Runnable() {
		t.Fatal("building app must not be runnable")
	}
	if !AppStatusActive.Runnable() || !AppStatusDeprecated.Runnable() {
		t.Fatal("active and deprecated apps must remain runnable")
	}
	if AppStatus("deactivated").Valid() {
		t.Fatal("deactivated is a tombstone outcome, not a persisted app status")
	}
}

func TestAppPersistenceRejectsInvalidDomainValuesBeforeSQL(t *testing.T) {
	repository := &postgresStore{}
	if _, _, err := repository.CreateOrGetAppFamily(context.Background(), AppFamily{Kind: AppKind("artifact")}); err != ErrAppKindInvalid {
		t.Fatalf("CreateOrGetAppFamily error = %v, want ErrAppKindInvalid", err)
	}
	if _, _, err := repository.PublishAppVersion(context.Background(), App{Status: AppStatus("deactivated")}); err != ErrAppStatusInvalid {
		t.Fatalf("PublishAppVersion error = %v, want ErrAppStatusInvalid", err)
	}
	if _, _, err := repository.PublishAppVersion(context.Background(), App{Status: AppStatusActive, ExpectedFamilyKind: AppKind("artifact")}); err != ErrAppKindInvalid {
		t.Fatalf("PublishAppVersion kind error = %v, want ErrAppKindInvalid", err)
	}
}

func TestAppFamilyBindingExcludesPresentationFields(t *testing.T) {
	ownerID := uuid.New()
	existing := AppFamily{TargetLanguage: "typescript", OwnerSubjectID: ownerID, DisplayName: "Old name"}
	requested := AppFamily{TargetLanguage: "typescript", OwnerSubjectID: ownerID, DisplayName: "New name"}
	if !existing.HasSameBinding(requested) {
		t.Fatal("display-name changes must not alter immutable family binding")
	}
	requested.TargetLanguage = "python"
	if existing.HasSameBinding(requested) {
		t.Fatal("language changes must alter immutable family binding")
	}
}

// TestImmutableAppVersionComparesEntireRuntimeScope proves private definitions,
// descriptor hashes, selections, and capabilities all participate in version immutability.
func TestImmutableAppVersionComparesEntireRuntimeScope(t *testing.T) {
	existing := App{
		SourceHash: "source", ConfigKey: "sdk:billing:1.0.0", CapabilityHash: "capability",
		ScopeSchemaVersion: 1, GeneratorVersion: "generator", Selections: []byte(`[{"a":1,"b":2}]`),
	}
	requested := existing
	requested.Selections = []byte(`[{"b":2,"a":1}]`)
	if !sameImmutableAppVersion(existing, requested) {
		t.Fatal("JSON object ordering must not change immutable scope identity")
	}
	requested.CapabilityHash = "expanded"
	if sameImmutableAppVersion(existing, requested) {
		t.Fatal("changed capabilities must reject an immutable-version retry")
	}
	requested = existing
	requested.UnifiedDefinitionHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if sameImmutableAppVersion(existing, requested) {
		t.Fatal("changed Unified definitions must reject an immutable-version retry")
	}
}

func TestAppKindMustMatchConfigType(t *testing.T) {
	for _, valid := range []struct {
		kind       AppKind
		configType ConfigType
	}{{AppKindSDK, ConfigTypeSDK}, {AppKindMCP, ConfigTypeMCP}} {
		if !appKindMatchesConfigType(valid.kind, valid.configType) {
			t.Fatalf("expected %s/%s to match", valid.kind, valid.configType)
		}
	}
	if appKindMatchesConfigType(AppKindSDK, ConfigTypeMCP) || appKindMatchesConfigType("", ConfigTypeSDK) {
		t.Fatal("empty or cross-kind config identity must fail before persistence")
	}
}
