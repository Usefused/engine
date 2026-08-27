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
	if err := resolveAppAuthReferenceSelection(&selection, auth, services, contracts); err != nil {
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
	if err := resolveAppAuthReferenceSelection(&incompatible, auth, services, contracts); err == nil {
		t.Fatal("expected incompatible source auth family to be rejected")
	}
}
