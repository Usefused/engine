package api

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestParseAppAuthReferenceEnforcesClosedGrammar keeps direct Engine clients
// on the same exact reference contract enforced by SDK/MCP config parsing.
func TestParseAppAuthReferenceEnforcesClosedGrammar(t *testing.T) {
	valid, err := parseAppAuthReference("${bucket.auth.gmail.oauth2}")
	if err != nil || valid.ServiceKey != "gmail" || valid.AuthName != "oauth2" {
		t.Fatalf("valid reference = %#v, %v", valid, err)
	}
	invalid := []string{
		" ${bucket.auth.gmail.oauth2}",
		"${bucket.auth.gmail.oauth2} ",
		"${bucket.auth.gmail.oauth2}}",
		"${bucket.auth.gmail.$oauth}",
		"${bucket.auth.gmail.oauth 2}",
		"${bucket.auth.gmail.oauth2.extra}",
	}
	// Every malformed form must fail before source identity is persisted.
	for _, value := range invalid {
		if _, err := parseAppAuthReference(value); err == nil {
			t.Fatalf("expected reference %q to fail", strings.ReplaceAll(value, "\n", "\\n"))
		}
	}
}

// TestAppAuthSourceContractSelectionsDeduplicatesCredentialOnlyServices proves source reuse never expands app operations or storage reads.
func TestAppAuthSourceContractSelectionsDeduplicatesCredentialOnlyServices(t *testing.T) {
	sourceID := uuid.New()
	doc := sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{
		"gmail": {Auth: &sdkAppAuthDoc{Type: "oauth", Name: "targetOAuth", Ref: "${bucket.auth.google.sourceOAuth}"}},
		"drive": {Auth: &sdkAppAuthDoc{Type: "oauth", Name: "driveOAuth", Ref: "${bucket.auth.google.sourceOAuth}"}},
	}}
	requests, err := appAuthSourceContractSelections(doc, map[string]store.WorkspaceService{
		"google": {ServiceID: sourceID, Version: "v1", ServiceVersionID: uuid.New()},
	})
	if err != nil {
		t.Fatalf("source contract selections: %v", err)
	}
	// One source registration shared by several targets remains one metadata-only batch member.
	if len(requests) != 1 || requests[0].ServiceID != sourceID || requests[0].Version != "v1" || requests[0].SelectAll || len(requests[0].OperationNames) != 0 {
		t.Fatalf("source contract selections = %#v", requests)
	}
}

// TestResolveAppAuthReferenceSelectionPinsExactCompatibleSource verifies immutable app metadata owns the reusable credential route.
func TestResolveAppAuthReferenceSelectionPinsExactCompatibleSource(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	selection := models.SDKSelection{
		ServiceID: targetID, AuthType: "oauth", AuthName: "targetOAuth",
		RequiredAuth: []models.SDKRequiredAuth{{AuthType: "oauth", AuthName: "targetOAuth"}},
	}
	auth := &sdkAppAuthDoc{Type: "oauth", Name: "targetOAuth", Ref: "${bucket.auth.google.sourceOAuth}"}
	services := map[string]store.WorkspaceService{"google": {ServiceID: sourceID, Version: "v1"}}
	contracts := map[string]sandbox.ServiceVersionExecutionAuthContract{
		executionAuthContractKey(sourceID, "v1", nil, false): {
			ServiceID: sourceID, Version: "v1",
			AuthConfigs: fusedobject.AuthConfigs{{Name: "sourceOAuth", Type: "oauth2"}},
		},
	}
	// Both services resolve to the same zero-value default bucket here, so the
	// same-bucket auth.ref invariant is satisfied trivially for this test.
	if err := resolveAppAuthReferenceSelection(&selection, auth, services, contracts, appBucketSet{}); err != nil {
		t.Fatalf("resolve app auth reference: %v", err)
	}
	if selection.AuthRef != auth.Ref || selection.CredentialSourceServiceID != sourceID || selection.CredentialSourceAuthType != "oauth" || selection.CredentialSourceAuthName != "sourceOAuth" {
		t.Fatalf("resolved selection = %#v", selection)
	}

	incompatible := selection
	incompatible.CredentialSourceServiceID = uuid.Nil
	contracts[executionAuthContractKey(sourceID, "v1", nil, false)] = sandbox.ServiceVersionExecutionAuthContract{
		ServiceID: sourceID, Version: "v1", AuthConfigs: fusedobject.AuthConfigs{{Name: "sourceOAuth", Type: "openIdConnect"}},
	}
	// Reusing a same-named registration across OAuth and OIDC families must fail before readiness or persistence.
	if err := resolveAppAuthReferenceSelection(&incompatible, auth, services, contracts, appBucketSet{}); err == nil {
		t.Fatal("expected incompatible source auth family to be rejected")
	}
}

// TestResolveAppAuthReferenceSelectionRejectsCrossBucketSource verifies the
// same-bucket-only invariant with two ACTUALLY different resolved buckets
// (not the trivial zero-value case used above): a target resolved through
// the family default bucket must not be allowed to rebase its credential
// onto a source resolved through a distinct per-service override bucket.
func TestResolveAppAuthReferenceSelectionRejectsCrossBucketSource(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	selection := models.SDKSelection{
		ServiceID: targetID, AuthType: "oauth", AuthName: "targetOAuth",
		RequiredAuth: []models.SDKRequiredAuth{{AuthType: "oauth", AuthName: "targetOAuth"}},
	}
	auth := &sdkAppAuthDoc{Type: "oauth", Name: "targetOAuth", Ref: "${bucket.auth.google.sourceOAuth}"}
	services := map[string]store.WorkspaceService{"google": {ServiceID: sourceID, Version: "v1"}}
	contracts := map[string]sandbox.ServiceVersionExecutionAuthContract{
		executionAuthContractKey(sourceID, "v1", nil, false): {
			ServiceID: sourceID, Version: "v1",
			AuthConfigs: fusedobject.AuthConfigs{{Name: "sourceOAuth", Type: "oauth2"}},
		},
	}
	defaultBucket := store.Bucket{ID: uuid.New(), Name: "default"}
	overrideBucket := store.Bucket{ID: uuid.New(), Name: "override"}
	// The target resolves through the family default; the source resolves
	// through a distinct per-service override -- a real cross-bucket pair.
	buckets := appBucketSet{Default: defaultBucket, Overrides: map[uuid.UUID]store.Bucket{sourceID: overrideBucket}}
	if err := resolveAppAuthReferenceSelection(&selection, auth, services, contracts, buckets); err == nil {
		t.Fatal("expected cross-bucket auth.ref to be rejected")
	}

	// Moving the source's override to match the target's bucket must let the
	// identical reference succeed, proving the check compares resolved
	// buckets rather than unconditionally rejecting any override present.
	sameBucket := appBucketSet{Default: defaultBucket, Overrides: map[uuid.UUID]store.Bucket{sourceID: defaultBucket}}
	if err := resolveAppAuthReferenceSelection(&selection, auth, services, contracts, sameBucket); err != nil {
		t.Fatalf("expected same-bucket auth.ref to succeed once buckets align: %v", err)
	}
}

