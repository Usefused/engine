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
			APIVersion: "fused/v1",
			Kind:       "mcp",
			Name:       "jira-mcp",
			Version:    "1.0.0",
			Bucket:     "default",
			Services:   map[string]sdkConfigServiceDoc{"jira": svc},
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
	doc.Kind, doc.Language = store.AppKindMCP.String(), ""
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
		Services: map[string]sdkConfigServiceDoc{"okta": {Version: "2026-07-01", Operations: []string{"getUser"}}},
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

// TestEnforceMCPFamilyLimit guards the MaxMCPFamilies entitlement check added
// to executeMCPConfigApply.  It mirrors the SDK family limit pattern in
// sdk_config_handlers.go but uses a separate limit field so SDK and MCP
// counts are gated independently.
func TestEnforceMCPFamilyLimit(t *testing.T) {
	accountID := uuid.New()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
		MaxMCPFamilies: models.IntPtr(-1), // unlimited
	})
	defer entitlement.LiveEntitlement.Reset()

	t.Run("existing family allowed regardless of limit", func(t *testing.T) {
		s := &workspaceTestStore{
			accountID: accountID,
			appFamilies: map[string]store.AppFamily{
				accountID.String() + "\x00" + "mcp" + "\x00" + "jira": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "jira",
				},
			},
		}
		// set a very low limit
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(0),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "jira")
		if err != nil {
			t.Fatalf("existing family should be allowed even at zero limit: %v", err)
		}
	})

	t.Run("new family allowed when under limit", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(2),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "slack")
		if err != nil {
			t.Fatalf("new family under limit should be allowed: %v", err)
		}
	})

	t.Run("new family blocked at limit", func(t *testing.T) {
		s := &workspaceTestStore{
			accountID: accountID,
			appFamilies: map[string]store.AppFamily{
				accountID.String() + "\x00" + "mcp" + "\x00" + "github": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "github",
				},
				accountID.String() + "\x00" + "mcp" + "\x00" + "gitlab": {
					AccountID:     accountID,
					Kind:          "mcp",
					CanonicalName: "gitlab",
				},
			},
		}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(2),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "bitbucket")
		if err == nil {
			t.Fatal("expected block when at limit")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusForbidden {
			t.Fatalf("expected 403, got %#v", err)
		}
	})

	t.Run("zero limit blocks all new families", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(0),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "pagerduty")
		if err == nil {
			t.Fatal("expected block with zero limit")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusForbidden {
			t.Fatalf("expected 403, got %#v", err)
		}
	})

	t.Run("unlimited allows new families", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: models.IntPtr(-1),
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "asana")
		if err != nil {
			t.Fatalf("unlimited should allow: %v", err)
		}
	})

	t.Run("nil limit is treated as unlimited", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
			MaxMCPFamilies: nil,
		})
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "notion")
		if err != nil {
			t.Fatalf("nil limit (normalized to unlimited) should allow: %v", err)
		}
	})

	t.Run("invalid name returns 400", func(t *testing.T) {
		s := &workspaceTestStore{accountID: accountID}
		err := enforceMCPFamilyLimit(context.Background(), s, accountID, "")
		if err == nil {
			t.Fatal("expected error for empty name")
		}
		werr, ok := err.(workspaceConfigHTTPError)
		if !ok || werr.status != http.StatusBadRequest {
			t.Fatalf("expected 400, got %#v", err)
		}
	})

	// DB count-error paths are covered by integration tests against real
	// Store implementations; workspaceTestStore does not expose injected
	// count errors.
}
