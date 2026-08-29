package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestValidateAppConfigDocument_MCPRejectsWebhookSelection locks in the
// MCP webhook-rejection rule at both entry points that can select webhooks on
// a service: the explicit Webhooks allowlist and the newer WebhooksSelectAll
// flag. WebhooksSelectAll was added alongside webhook_attachment without
// updating this check, so `webhooks_select_all: true` on an MCP service
// silently bypassed the "MCP cannot select webhooks" rule that CLI validation
// already enforces -- this test guards against that
// regressing again.
func TestValidateAppConfigDocument_MCPRejectsWebhookSelection(t *testing.T) {
	baseDoc := func(svc sdkConfigServiceDoc) sdkConfigDocument {
		return sdkConfigDocument{
			APIVersion:  "fused/v1",
			Kind:        "mcp",
			Name:        "jira-mcp",
			Version:     "1.0.0",
			Description: "Find and manage support issues in Jira.",
			Bucket:      "default",
			Services:    map[string]sdkConfigServiceDoc{"jira": svc},
		}
	}

	t.Run("explicit webhooks list rejected", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}, Webhooks: []string{"issue.created"}})
		err := validateAppConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error, got %v", err)
		}
	})

	t.Run("webhooks_select_all rejected", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}, WebhooksSelectAll: true})
		err := validateAppConfigDocument(doc, "mcp")
		if err == nil || !strings.Contains(err.Error(), "cannot select webhooks") {
			t.Fatalf("expected 'cannot select webhooks' error for webhooks_select_all, got %v", err)
		}
	})

	t.Run("plain operations selection is fine", func(t *testing.T) {
		doc := baseDoc(sdkConfigServiceDoc{Operations: []string{"getIssue"}})
		if err := validateAppConfigDocument(doc, "mcp"); err != nil {
			t.Fatalf("expected no error for an operations-only MCP service, got %v", err)
		}
	})
}

// TestValidateAppConfigDocumentMCPUsesSharedUnifiedContract proves MCP admits
// the SDK graph shape while avoiding package-only language namespace checks.
func TestValidateAppConfigDocumentMCPUsesSharedUnifiedContract(t *testing.T) {
	doc := decodeUnifiedDocument(t, `{"github":"createIssue"}`, ``, "typescript")
	doc.Kind, doc.Language, doc.Description = store.AppKindMCP.String(), "", "Create and coordinate GitHub issues."
	// Exact MCP operation identities do not become nested generated members, so
	// a prefix pair that remains invalid for SDK code generation is admissible.
	doc.UnifiedOperations["issues"] = doc.UnifiedOperations["issues.create"]
	// MCP keeps exact logical names, so SDK-only generated namespace collisions
	// must not reject this otherwise valid graph.
	if err := validateAppConfigDocument(doc, store.AppKindMCP.String()); err != nil {
		t.Fatalf("MCP should use shared Unified graph validation: %v", err)
	}
}

// TestResolveMCPEndpointIDsUsesOneSnapshotBatch verifies endpoint freezing has
// constant query count as the number of explicit service selections grows.
func TestResolveMCPEndpointIDsUsesOneSnapshotBatch(t *testing.T) {
	firstService, secondService := uuid.New(), uuid.New()
	firstVersion, secondVersion := uuid.New(), uuid.New()
	s := &workspaceTestStore{}
	selections := []models.SDKSelection{
		{ServiceID: firstService, ServiceVersionID: firstVersion, OperationNames: []string{"getIssue", "createIssue"}},
		{ServiceID: secondService, ServiceVersionID: secondVersion, OperationNames: []string{"notify"}},
	}
	resolved, err := resolveMCPEndpointIDs(t.Context(), s, selections)
	// A resolver failure would hide whether the batch query shape was preserved.
	if err != nil {
		t.Fatalf("resolveMCPEndpointIDs() error = %v", err)
	}
	if len(s.contractEndpointBatches) != 1 {
		t.Fatalf("snapshot query count = %d, want 1", len(s.contractEndpointBatches))
	}
	// Each exact operation receives one immutable endpoint ID from rows already
	// intersected by the database-facing resolver.
	if len(resolved[0].EndpointIDs) != 2 || len(resolved[1].EndpointIDs) != 1 {
		t.Fatalf("resolved endpoint IDs = %#v", resolved)
	}
}

// TestMCPConfigApplyPersistsCompiledUnifiedState exercises plan-to-apply
// handoff and proves MCP uses the existing immutable runtime fields unchanged.
func TestMCPConfigApplyPersistsCompiledUnifiedState(t *testing.T) {
	accountID, serviceID, serviceVersionID := uuid.New(), uuid.New(), uuid.New()
	actor := controlTestOwnerActor(accountID)
	ctx := accesscontrol.ContextWithActor(t.Context(), actor)
	s := &workspaceTestStore{
		accountID: accountID,
		workspaceServices: []store.WorkspaceService{{
			ServiceID: serviceID, ServiceName: "okta", Version: "2026-07-01",
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: "2026-07-01"}},
		},
	}
	revision := sandbox.ServiceVersionRevision{
		ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1,
	}
	registryClient := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": revision,
	}}
	configStore := &mockConfigStore{}
	doc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: store.AppKindMCP.String(), Name: "security", Version: "1.0.0", Bucket: "default",
		Description: "Look up identities and coordinate security workflows in Okta.",
		Services:    map[string]sdkConfigServiceDoc{"okta": {Version: "2026-07-01", Operations: []string{"getUser"}}},
		UnifiedOperations: map[string]sdkUnifiedOperationDoc{
			"security.lookup": {
				Input:    json.RawMessage(`{"type":"object"}`),
				Bindings: map[string]sdkUnifiedBindingDoc{"okta": {Operation: "getUser"}},
			},
		},
	}
	planResult, err := createMCPConfigPlan(ctx, configStore, s, registryClient, sdkPlanCall{
		apiKey: "fsk_test", accountID: accountID, actor: actor,
		request: SDKConfigPlanRequest{ConfigKey: "mcp:security:1.0.0", SourceHash: "sha256:test"}, document: doc,
	})
	// Plan must succeed before apply can prove byte-identical persistence.
	if err != nil {
		t.Fatalf("createMCPConfigPlan() error = %v", err)
	}
	result, err := executeMCPConfigApply(ctx, configStore, s, registryClient, sdkApplyCall{
		apiKey: "fsk_test", accountID: accountID, actor: actor,
		planID: planResult.plan.ID, planRevision: planResult.plan.Revision, sourceHash: planResult.plan.SourceHash,
	})
	// Apply is the user-triggered mutation boundary whose persisted scope is under test.
	if err != nil {
		t.Fatalf("executeMCPConfigApply() error = %v", err)
	}
	// Reaching the shared atomic repository proves MCP did not fork persistence.
	if result.RuntimeID == uuid.Nil || configStore.artifactApply == nil {
		t.Fatalf("MCP apply did not reach shared app persistence: %#v", result)
	}
	var planned appResolvedPayload
	// The stored payload is authoritative for both the public descriptor and private hashes.
	if err := json.Unmarshal(planResult.plan.ResolvedPayload, &planned); err != nil {
		t.Fatalf("decode plan payload: %v", err)
	}
	// MCP keeps the credential-free descriptor in the plan without a new persistence column.
	if planned.UnifiedOperations == nil || len(planned.UnifiedOperations.Operations) != 1 {
		t.Fatalf("credential-free Unified descriptor is absent from the applied plan: %#v", planned.UnifiedOperations)
	}
	// The authored server summary must survive plan and apply without being reconstructed from operation identifiers.
	if planned.Description != doc.Description || configStore.artifactApply.Scope.Description != doc.Description {
		t.Fatalf("MCP description did not survive plan/apply: plan=%q scope=%q", planned.Description, configStore.artifactApply.Scope.Description)
	}
	// Planning performs two bounded set-based reads regardless of selected row
	// count: one physical freeze and one shared Unified compile resolution.
	if len(s.contractEndpointBatches) != 2 {
		t.Fatalf("snapshot batch count = %d, want endpoint freeze plus shared compile", len(s.contractEndpointBatches))
	}
	scope := configStore.artifactApply.Scope
	// Apply must copy the compiler-owned bytes and hashes exactly; recomputing
	// or translating them here would create a second Unified contract.
	if scope.UnifiedDefinitionSchemaVersion != planned.UnifiedDefinitionSchemaVersion ||
		string(scope.UnifiedDefinitions) != string(planned.UnifiedDefinitions) ||
		scope.UnifiedDefinitionHash != planned.UnifiedDefinitionHash ||
		scope.UnifiedCodegenDescriptorHash != planned.UnifiedCodegenDescriptorHash {
		t.Fatalf("persisted Unified scope differs from plan: %#v", scope)
	}
}

// TestValidateAppConfigDocumentMCPRequiresBoundedDescription keeps server identity useful and bounded before planning.
func TestValidateAppConfigDocumentMCPRequiresBoundedDescription(t *testing.T) {
	doc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "mail", Version: "1.0.0", Bucket: "default",
		Services: map[string]sdkConfigServiceDoc{"gmail": {Operations: []string{"listMessages"}}},
	}
	// Missing prose would leave clients unable to choose the server before tool discovery.
	if err := validateAppConfigDocument(doc, "mcp"); err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("missing description error = %v", err)
	}
	doc.Description = strings.Repeat("a", models.MCPServerDescriptionMaxBytes+1)
	// Oversized identity text would consume unbounded initialize context.
	if err := validateAppConfigDocument(doc, "mcp"); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized description error = %v", err)
	}
}

// TestValidateMCPDesiredStateTreatsDescriptionAsImmutable requires a version bump when server identity prose changes.
func TestValidateMCPDesiredStateTreatsDescriptionAsImmutable(t *testing.T) {
	doc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "mail", Version: "1.0.0",
		Description: "Read and summarize email.", Services: map[string]sdkConfigServiceDoc{},
	}
	desired, err := canonicalAppState(doc)
	// The baseline state must be canonical before immutability can be evaluated.
	if err != nil {
		t.Fatalf("canonicalAppState() error = %v", err)
	}
	doc.Description = "Read, summarize, and send email."
	_, err = validateMCPDesiredState(doc, &store.ConfigState{DesiredState: desired})
	// A changed public server promise is a new immutable MCP version, even with identical operation scope.
	if err == nil || !strings.Contains(err.Error(), "app_version_immutable") {
		t.Fatalf("description mutation error = %v", err)
	}
}

func TestAppConfigVersionLengthBound(t *testing.T) {
	tooLong := "1.0.0-" + strings.Repeat("a", maxAppVersionLength)

	sdkDoc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "sdk", Name: "support", Version: tooLong,
		Language: "typescript", Bucket: "default", Services: map[string]sdkConfigServiceDoc{},
	}
	if err := validateSDKIdentity(sdkDoc); err == nil {
		t.Fatal("expected an overlong SDK version to be rejected")
	}

	mcpDoc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "mcp", Name: "support", Version: tooLong,
		Bucket: "default", Services: map[string]sdkConfigServiceDoc{},
	}
	if err := validateAppConfigDocument(mcpDoc, "mcp"); err == nil {
		t.Fatal("expected an overlong MCP version to be rejected")
	}
}

// TestValidateSDKIdentityRejectsMCPDescription mirrors the CLI cross-kind metadata boundary.
func TestValidateSDKIdentityRejectsMCPDescription(t *testing.T) {
	doc := sdkConfigDocument{
		APIVersion: "fused/v1", Kind: "sdk", Name: "support", Version: "1.0.0",
		Language: "typescript", Bucket: "default", Description: "Manage support work.",
	}
	// Engine must reject inert authored prose even when a client bypasses CLI validation.
	if err := validateSDKIdentity(doc); err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("SDK description error = %v", err)
	}
}

// TestEnforceMCPFamilyLimit guards the MaxMCPFamilies entitlement check added
// to executeMCPConfigApply.  It mirrors the SDK family limit pattern in
// sdk_config_handlers.go but uses a separate limit field so SDK and MCP
// counts are gated independently.
func TestEnforceMCPFamilyLimit(t *testing.T) {
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{"existing family allowed regardless of limit", testMCPExistingFamilyCapacity},
		{"dormant existing family must reacquire capacity", testMCPDormantFamilyCapacity},
		{"new family allowed when under limit", testMCPFamilyUnderLimit},
		{"new family blocked at limit", testMCPFamilyAtLimit},
		{"fully deactivated family does not consume capacity", testMCPDeactivatedFamilyCapacity},
		{"zero limit blocks all new families", testMCPZeroFamilyLimit},
		{"unlimited allows new families", testMCPUnlimitedFamilyLimit},
		{"nil limit is treated as unlimited", testMCPNilFamilyLimit},
		{"invalid name returns 400", testMCPInvalidFamilyName},
	}
	// Named cases keep each lifecycle invariant independently readable and below the complexity ceiling.
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

// testMCPExistingFamilyCapacity proves an already-counted family can add a version after a plan downgrade.
func testMCPExistingFamilyCapacity(t *testing.T) {
	accountID, existingFamilyID := uuid.New(), uuid.New()
	s := &workspaceTestStore{accountID: accountID,
		appFamilies: map[string]store.AppFamily{accountID.String() + "\x00mcp\x00jira": {AppFamilyID: existingFamilyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "jira"}},
		apps:        map[uuid.UUID]store.App{uuid.New(): {AppFamilyID: existingFamilyID, Status: store.AppStatusActive}},
	}
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(0)})
	// Existing invokable usage must not be charged twice.
	if err := enforceMCPFamilyLimit(context.Background(), s, accountID, "jira"); err != nil {
		t.Fatalf("existing family should be allowed even at zero limit: %v", err)
	}
}

// testMCPDormantFamilyCapacity proves retained identity cannot bypass a full quota.
func testMCPDormantFamilyCapacity(t *testing.T) {
	accountID, dormantFamilyID, activeFamilyID := uuid.New(), uuid.New(), uuid.New()
	s := &workspaceTestStore{accountID: accountID,
		appFamilies: map[string]store.AppFamily{
			accountID.String() + "\x00mcp\x00dormant": {AppFamilyID: dormantFamilyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "dormant"},
			accountID.String() + "\x00mcp\x00active":  {AppFamilyID: activeFamilyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "active"},
		},
		apps: map[uuid.UUID]store.App{uuid.New(): {AppFamilyID: activeFamilyID, Status: store.AppStatusActive}},
	}
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(1)})
	// Publishing a dormant family makes it invokable again and therefore needs capacity.
	if err := enforceMCPFamilyLimit(context.Background(), s, accountID, "dormant"); err == nil {
		t.Fatal("expected dormant family reactivation to require capacity")
	}
}

// testMCPFamilyUnderLimit proves a new family is admitted when one slot remains.
func testMCPFamilyUnderLimit(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(2)})
	// Empty usage leaves room for the requested server.
	if err := enforceMCPFamilyLimit(context.Background(), &workspaceTestStore{accountID: accountID}, accountID, "slack"); err != nil {
		t.Fatalf("new family under limit should be allowed: %v", err)
	}
}

// testMCPFamilyAtLimit verifies the plain-language error returned when all slots are occupied.
func testMCPFamilyAtLimit(t *testing.T) {
	accountID, githubFamilyID, gitlabFamilyID := uuid.New(), uuid.New(), uuid.New()
	s := &workspaceTestStore{accountID: accountID,
		appFamilies: map[string]store.AppFamily{
			accountID.String() + "\x00mcp\x00github": {AppFamilyID: githubFamilyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "github"},
			accountID.String() + "\x00mcp\x00gitlab": {AppFamilyID: gitlabFamilyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "gitlab"},
		},
		apps: map[uuid.UUID]store.App{
			uuid.New(): {AppFamilyID: githubFamilyID, Status: store.AppStatusActive},
			uuid.New(): {AppFamilyID: gitlabFamilyID, Status: store.AppStatusDeprecated},
		},
	}
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(2)})
	err := enforceMCPFamilyLimit(context.Background(), s, accountID, "bitbucket")
	// Full usage must return a typed quota error rather than pass admission.
	if err == nil {
		t.Fatal("expected block when at limit")
	}
	assertMCPFamilyLimitError(t, err)
}

// assertMCPFamilyLimitError verifies quota identity and first-use guidance without duplicating admission setup.
func assertMCPFamilyLimitError(t *testing.T, err error) {
	t.Helper()
	werr, ok := err.(workspaceConfigHTTPError)
	// Every public field must remain explicit enough for agents that do not know internal family terminology.
	if !ok || werr.status != http.StatusForbidden || werr.code != "mcp_family_limit_exceeded" || werr.category != "entitlement" || werr.message != "This workspace has reached its MCP server limit (2 of 2)." || werr.remediation != "Deactivate all active or deprecated versions of an unused MCP server, or upgrade the workspace plan, then retry." {
		t.Fatalf("unexpected MCP family limit error: %#v", err)
	}
}

// testMCPDeactivatedFamilyCapacity proves retained history does not occupy an MCP slot.
func testMCPDeactivatedFamilyCapacity(t *testing.T) {
	accountID, familyID := uuid.New(), uuid.New()
	s := &workspaceTestStore{accountID: accountID,
		appFamilies: map[string]store.AppFamily{accountID.String() + "\x00mcp\x00retained": {AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindMCP, CanonicalName: "retained"}},
		apps:        map[uuid.UUID]store.App{},
	}
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(1)})
	// One new server remains admissible after the old server's final version is gone.
	if err := enforceMCPFamilyLimit(context.Background(), s, accountID, "replacement"); err != nil {
		t.Fatalf("fully deactivated family should not consume capacity: %v", err)
	}
}

// testMCPZeroFamilyLimit proves an explicit zero remains quota policy rather than authorization denial.
func testMCPZeroFamilyLimit(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(0)})
	err := enforceMCPFamilyLimit(context.Background(), &workspaceTestStore{accountID: accountID}, accountID, "pagerduty")
	// Zero entitlement blocks every new server.
	if err == nil {
		t.Fatal("expected block with zero limit")
	}
	werr, ok := err.(workspaceConfigHTTPError)
	// The stable code must remain distinct from permission denial.
	if !ok || werr.status != http.StatusForbidden || werr.code != "mcp_family_limit_exceeded" {
		t.Fatalf("expected MCP quota error, got %#v", err)
	}
}

// testMCPUnlimitedFamilyLimit proves the explicit unlimited sentinel admits new servers.
func testMCPUnlimitedFamilyLimit(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: models.IntPtr(-1)})
	// Unlimited workspaces have no family admission ceiling.
	if err := enforceMCPFamilyLimit(context.Background(), &workspaceTestStore{accountID: accountID}, accountID, "asana"); err != nil {
		t.Fatalf("unlimited should allow: %v", err)
	}
}

// testMCPNilFamilyLimit proves an absent forward-compatible field retains unlimited behavior.
func testMCPNilFamilyLimit(t *testing.T) {
	accountID := uuid.New()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxMCPFamilies: nil})
	// Missing limits remain unlimited so older entitlement rows do not block new functionality.
	if err := enforceMCPFamilyLimit(context.Background(), &workspaceTestStore{accountID: accountID}, accountID, "notion"); err != nil {
		t.Fatalf("nil limit should allow: %v", err)
	}
}

// testMCPInvalidFamilyName proves malformed identity fails before quota lookup.
func testMCPInvalidFamilyName(t *testing.T) {
	accountID := uuid.New()
	err := enforceMCPFamilyLimit(context.Background(), &workspaceTestStore{accountID: accountID}, accountID, "")
	// Empty names cannot identify a target family.
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	werr, ok := err.(workspaceConfigHTTPError)
	// Identity validation remains a caller error rather than quota denial.
	if !ok || werr.status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %#v", err)
	}
}
