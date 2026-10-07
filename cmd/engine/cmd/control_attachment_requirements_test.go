package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type attachmentRequirementStore struct {
	*controlRequirementStoreStub
	store.UnifiedAppAttachmentStore
	bindings  []models.UnifiedAppBinding
	accountID uuid.UUID
	loads     int
}

// ResolveUnifiedAppBindings records the account boundary and counts batched attachment resolution.
func (s *attachmentRequirementStore) ResolveUnifiedAppBindings(_ context.Context, accountID uuid.UUID, _ map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	s.accountID = accountID
	s.loads++
	return s.bindings, nil
}

// TestReferenceOnlyConsumerAuthorization keeps hosted-only SDKs and MCPs deployable without bypassing dependency grants.
func TestReferenceOnlyConsumerAuthorization(t *testing.T) {
	for _, kind := range []string{"sdk", "mcp"} {
		// Both delivery adapters must cross the same plan and apply authorization boundary.
		t.Run(kind, func(t *testing.T) {
			accountID, workspaceID, bucketID, familyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			binding := models.UnifiedAppBinding{Alias: "checkout", Name: "Checkout", Version: "1.0.0", AppID: uuid.New(), AppFamilyID: familyID}
			stores := &attachmentRequirementStore{controlRequirementStoreStub: &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "default"}}}, bindings: []models.UnifiedAppBinding{binding}}
			desired := `{"kind":"` + kind + `","bucket":"default","unified_apps":{"checkout":{"name":"Checkout","version":"1.0.0"}}}`
			payload, err := json.Marshal(map[string]any{"bucket_id": bucketID, "unified_apps": stores.bindings})
			// Fixture encoding must succeed before testing policy behavior.
			if err != nil {
				t.Fatal(err)
			}
			plan := &store.ConfigPlan{ID: uuid.New(), ConfigType: store.ConfigType(kind), DesiredState: []byte(desired), ResolvedPayload: payload, Revision: 1}
			resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{plan: plan})
			actor := accesscontrol.Actor{AccountID: accountID, WorkspaceID: workspaceID, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}
			for _, phase := range []string{"plan", "apply"} {
				body := `{"config_key":"` + kind + `:checkout:1.0.0","config":` + desired + `}`
				policy := dynamicDesiredConfigPlan
				// Apply must use the immutable resolved bindings rather than names supplied by a caller.
				if phase == "apply" {
					body = `{"plan_id":"` + plan.ID.String() + `"}`
					policy = dynamicDesiredConfigApply
				}
				request := httptest.NewRequest(http.MethodPost, "/"+kind+"-config/"+phase, strings.NewReader(body))
				requirements, err := resolver.ResolveControlRequirements(context.Background(), actor, policy, nil, request)
				// Hosted-only scope is valid and must yield create, bucket, and dependency authority.
				if err != nil || len(requirements) != 3 {
					t.Fatalf("%s requirements=%v error=%v", phase, requirements, err)
				}
				var grants []accesscontrol.Grant
				for _, role := range accesscontrol.BuiltInRoles() {
					// Use the shipped Admin definition instead of inventing a permissive test actor.
					if role.Slug == accesscontrol.RoleAdmin {
						for _, permission := range role.Permissions {
							grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
						}
					}
				}
				actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(1, grants...)
				// An invalid fixture grant must not masquerade as a policy failure.
				if err != nil {
					t.Fatal(err)
				}
				// A workspace administrator can deploy the reference-only consumer in both phases.
				if err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirements...); err != nil {
					t.Fatalf("admin %s: %v", phase, err)
				}
				grants = nil
				for _, requirement := range requirements {
					// Withhold only hosted-app use to prove bucket and creation access cannot substitute for it.
					if requirement.Permission != accesscontrol.PermissionAppUnifiedAppUse {
						grants = append(grants, accesscontrol.Grant{Permission: requirement.Permission, Resource: requirement.Resource})
					}
				}
				actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(2, grants...)
				// Construct only valid scoped grants before checking the deliberate denial.
				if err != nil {
					t.Fatal(err)
				}
				err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirements...)
				missing := accesscontrol.MissingRequirements(err)
				// Denial must identify the exact attached family, rather than a generic empty-service policy error.
				if !errors.Is(err, accesscontrol.ErrPermissionDenied) || len(missing) != 1 || missing[0].Permission != accesscontrol.PermissionAppUnifiedAppUse || missing[0].Resource.ID != familyID {
					t.Fatalf("dependency denial=%v missing=%v", err, missing)
				}
			}
			// Plan resolves one account-scoped batch; apply consumes the stored identities without another lookup.
			if stores.loads != 1 || stores.accountID != accountID {
				t.Fatalf("attachment lookup count/account = %d/%v", stores.loads, stores.accountID)
			}
		})
	}
}

// TestStoredAttachmentRequirementsFailClosed keeps malformed and recursive dependencies from bypassing normal grants.
func TestStoredAttachmentRequirementsFailClosed(t *testing.T) {
	bucketID, serviceID := uuid.New(), uuid.New()
	valid := models.UnifiedAppBinding{AppID: uuid.New(), AppFamilyID: uuid.New()}
	for _, test := range []struct {
		name       string
		kind       store.ConfigType
		bindings   []models.UnifiedAppBinding
		bucket     uuid.UUID
		wantDenied bool
	}{
		{name: "mixed MCP", kind: store.ConfigTypeMCP, bindings: []models.UnifiedAppBinding{valid}, bucket: bucketID},
		{name: "missing family", kind: store.ConfigTypeMCP, bindings: []models.UnifiedAppBinding{{AppID: valid.AppID}}, bucket: bucketID, wantDenied: true},
		{name: "missing version", kind: store.ConfigTypeSDK, bindings: []models.UnifiedAppBinding{{AppFamilyID: valid.AppFamilyID}}, bucket: bucketID, wantDenied: true},
		{name: "recursive", kind: store.ConfigTypeUnifiedApp, bindings: []models.UnifiedAppBinding{valid}, bucket: bucketID, wantDenied: true},
		{name: "oversized", kind: store.ConfigTypeMCP, bindings: make([]models.UnifiedAppBinding, 17), bucket: bucketID, wantDenied: true},
		{name: "missing bucket", kind: store.ConfigTypeMCP, bindings: []models.UnifiedAppBinding{valid}, wantDenied: true},
	} {
		// Physical scope must never hide an invalid hosted attachment.
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"bucket_id": test.bucket, "unified_apps": test.bindings, "selections": []map[string]any{{"service_id": serviceID}}})
			// Test setup failures must not count as a successful authorization denial.
			if err != nil {
				t.Fatal(err)
			}
			requirements, err := storedDesiredConfigSelectionRequirements(&store.ConfigPlan{ConfigType: test.kind, ResolvedPayload: raw}, accesscontrol.PermissionServiceConsume, accesscontrol.PermissionBucketUse)
			// Only the fully resolved mixed consumer can proceed.
			if test.wantDenied {
				if !errors.Is(err, accesscontrol.ErrPolicyDenied) {
					t.Fatalf("expected denial, got %v", err)
				}
				return
			}
			// Mixed scope must include the provider, bucket, and exact attached family.
			if err != nil || len(requirements) != 3 || requirements[0].Resource.ID != valid.AppFamilyID || requirements[1].Resource.ID != serviceID || requirements[2].Resource.ID != bucketID {
				t.Fatalf("requirements=%v error=%v", requirements, err)
			}
		})
	}
}

// TestAttachmentPlanRejectsUnresolvedReferences prevents partial lookup results from granting a smaller dependency set.
func TestAttachmentPlanRejectsUnresolvedReferences(t *testing.T) {
	stores := &attachmentRequirementStore{controlRequirementStoreStub: &controlRequirementStoreStub{}}
	resolver := &storeBackedControlRequirementResolver{store: stores}
	refs := map[string]models.UnifiedAppReference{"checkout": {Name: "Checkout", Version: "1.0.0"}}
	_, err := resolver.desiredAttachmentRequirements(context.Background(), uuid.New(), store.ConfigTypeMCP, refs)
	// Missing account-scoped bindings cannot be treated as an empty, harmless selection.
	if !errors.Is(err, accesscontrol.ErrPolicyDenied) || stores.loads != 1 {
		t.Fatalf("error=%v loads=%d", err, stores.loads)
	}
}
