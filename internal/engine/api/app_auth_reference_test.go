package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestParseAppAuthReferenceFusedNamespace keeps the reserved Fused-owned
// namespace distinct from the plain bucket namespace: only the fused form is
// marked managed, so a missing local credential can never silently select it.
func TestParseAppAuthReferenceFusedNamespace(t *testing.T) {
	managed, err := parseAppAuthReference("${fused.bucket.auth.gmail.oauth2}")
	if err != nil || !managed.Managed || managed.ServiceKey != "gmail" || managed.AuthName != "oauth2" {
		t.Fatalf("managed reference = %#v, %v", managed, err)
	}
	// The plain bucket form must stay unmanaged and keep its local meaning.
	local, err := parseAppAuthReference("${bucket.auth.gmail.oauth2}")
	if err != nil || local.Managed || local.ServiceKey != "gmail" || local.AuthName != "oauth2" {
		t.Fatalf("local reference = %#v, %v", local, err)
	}
	// A fused-like spelling without the exact prefix is not a managed reference.
	if _, err := parseAppAuthReference("${fused.bucket.auth.gmail.oauth2}extra"); err == nil {
		t.Fatal("expected trailing text to fail")
	}
	if _, err := parseAppAuthReference("${bucket.fused.auth.gmail.oauth2}"); err == nil {
		t.Fatal("expected reordered namespace to fail")
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

// TestResolveAppAuthReferenceSelectionPinsManagedSource proves the
// ${fused.bucket.auth...} form self-references the connecting service
// (unlike the plain form, which rejects that as pointless), needs no local
// bucket contract snapshot at all (the credential lives on the broker, not
// this bucket), and is exempt from the same-bucket invariant that only
// makes sense for a locally-stored credential. This is the exact function
// both SDK and MCP config apply share (createMCPConfigPlan calls the same
// resolveSDKSelections pipeline as SDK), so this one test covers both.
func TestResolveAppAuthReferenceSelectionPinsManagedSource(t *testing.T) {
	targetID := uuid.New()
	selection := models.SDKSelection{
		ServiceID: targetID, AuthType: "oauth", AuthName: "jiraOAuth",
		RequiredAuth: []models.SDKRequiredAuth{{AuthType: "oauth", AuthName: "jiraOAuth"}},
	}
	auth := &sdkAppAuthDoc{Type: "oauth", Name: "jiraOAuth", Ref: "${fused.bucket.auth.jira.jiraOAuth}"}
	services := map[string]store.WorkspaceService{"jira": {ServiceID: targetID, Version: "v1"}}
	// No contracts entry for targetID: a managed reference must not require
	// the local auth-contract snapshot the plain form needs to validate a
	// source's declared scheme.
	buckets := appBucketSet{Default: store.Bucket{ID: uuid.New(), Name: "default"}}
	if err := resolveAppAuthReferenceSelection(&selection, auth, services, nil, buckets); err != nil {
		t.Fatalf("resolve managed app auth reference: %v", err)
	}
	if !selection.ManagedAuth || selection.AuthRef != auth.Ref || selection.CredentialSourceServiceID != targetID ||
		selection.CredentialSourceAuthType != "oauth" || selection.CredentialSourceAuthName != "jiraOAuth" {
		t.Fatalf("resolved managed selection = %#v", selection)
	}

	// A scheme mismatch between the reference and the target must still be
	// rejected -- "managed" changes where the secret comes from, not whether
	// the reference has to name the right scheme.
	mismatched := selection
	mismatched.ManagedAuth, mismatched.CredentialSourceServiceID = false, uuid.Nil
	mismatchedAuth := &sdkAppAuthDoc{Type: "oauth", Name: "jiraOAuth", Ref: "${fused.bucket.auth.jira.otherScheme}"}
	if err := resolveAppAuthReferenceSelection(&mismatched, mismatchedAuth, services, nil, buckets); err == nil {
		t.Fatal("expected mismatched managed auth scheme to be rejected")
	}

	// A managed reference to a service the workspace has not enabled must be
	// rejected the same way an unmanaged one is.
	unknownAuth := &sdkAppAuthDoc{Type: "oauth", Name: "jiraOAuth", Ref: "${fused.bucket.auth.notenabled.jiraOAuth}"}
	if err := resolveAppAuthReferenceSelection(&selection, unknownAuth, map[string]store.WorkspaceService{}, nil, buckets); err == nil {
		t.Fatal("expected managed reference to a disabled service to be rejected")
	}
}

// TestManagedAuthReferencePlanReachesSDKAndMCPHTTP proves createMCPConfigPlan's
// claim ("MCP shares SDK selection/auth decisions rather than implementing an
// alternate planner") actually holds for ${fused.bucket.auth...}: both real
// HTTP plan handlers must pin the same managed selection from the same config
// shape, not just the unit-level resolver above.
func TestManagedAuthReferencePlanReachesSDKAndMCPHTTP(t *testing.T) {
	for _, test := range []struct{ kind, language, descriptionField string }{
		{"sdk", "typescript", ""},
		{"mcp", "", `,"description":"Find and manage Jira work."`},
	} {
		t.Run(test.kind, func(t *testing.T) {
			serviceID, versionID := uuid.New(), uuid.New()
			s := &workspaceTestStore{accountID: uuid.New(),
				workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "Jira", Version: "v1"}},
				workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1"}}},
			}
			registry := &appAuthMismatchRegistry{mockRegistryClient: &mockRegistryClient{
				slugIDs: map[string]uuid.UUID{"jira": serviceID},
				contractRevisions: map[string]sandbox.ServiceVersionRevision{
					serviceID.String() + "|v1": {ServiceID: serviceID, ServiceVersionID: versionID, Version: "v1", Revision: 1},
				},
			}, contracts: []sandbox.ServiceVersionExecutionAuthContract{executionAuthContract(serviceID,
				fusedobject.AuthConfigs{artifactOAuth("read")}, securedOperation("readIssue", "oauthAuth"))}}
			configStore := &mockConfigStore{}
			router := newControlTestRouter(s.accountID)
			router.Post("/sdk-config/plan", SDKConfigPlanHandler(configStore, s, registry))
			router.Post("/mcp-config/plan", MCPConfigPlanHandler(configStore, s, registry))
			body := fmt.Sprintf(`{"source_hash":"fixture","config_key":"%s:fixture:1.0.0","config":{"apiVersion":"fused/v1","kind":%q,"name":"fixture","version":"1.0.0"%s,"language":%q,"bucket":"default","services":{"jira":{"version":"v1","operations":["readIssue"],"auth":{"type":"oauth","name":"oauthAuth","ref":"${fused.bucket.auth.jira.oauthAuth}","managed_application_id":"729ea172-5512-4f37-b202-31084e2d2766"}}}}}`, test.kind, test.kind, test.descriptionField, test.language)
			request := httptest.NewRequest(http.MethodPost, "/"+test.kind+"-config/plan", strings.NewReader(body))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// A managed reference needs no bucket credential at all, so this must
			// plan clean -- no auth error, no "missing credential" readiness warning.
			if response.Code != http.StatusOK || configStore.createdPlan == nil {
				t.Fatalf("plan response = %d %s", response.Code, response.Body.String())
			}
			var payload struct {
				CredentialReadiness *appCredentialReadiness `json:"credential_readiness"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode plan response: %v", err)
			}
			if payload.CredentialReadiness != nil && len(payload.CredentialReadiness.MissingCredentials) != 0 {
				t.Fatalf("managed reference must not report missing local credentials: %#v", payload.CredentialReadiness)
			}
			var resolved struct {
				Selections []models.SDKSelection `json:"selections"`
			}
			if err := json.Unmarshal(configStore.createdPlan.ResolvedPayload, &resolved); err != nil || len(resolved.Selections) != 1 {
				t.Fatalf("resolved payload = %s / %v", configStore.createdPlan.ResolvedPayload, err)
			}
			selection := resolved.Selections[0]
			if !selection.ManagedAuth || selection.CredentialSourceServiceID != serviceID || selection.CredentialSourceAuthName != "oauthAuth" || selection.ManagedApplicationID != "729ea172-5512-4f37-b202-31084e2d2766" {
				t.Fatalf("%s: pinned selection = %#v", test.kind, selection)
			}
		})
	}
}
