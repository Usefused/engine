package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBridgeOnlyControlAuthorization reproduces the real plan/apply route denial without bypassing delegation grants.
func TestBridgeOnlyControlAuthorization(t *testing.T) {
	for _, kind := range []string{"sdk", "mcp", "unified_app"} {
		// Every app adapter must retain both ordinary app authority and local sharing authority.
		t.Run(kind, func(t *testing.T) {
			workspaceID, bucketID := uuid.New(), uuid.New()
			stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "default"}}}
			desired := `{"kind":"` + kind + `","generate":false,"bucket":"default","bridges":{"echo_device":{"pairing_id":"` + uuid.New().String() + `","profile_id":"echo-live"}}}`
			raw, err := json.Marshal(map[string]any{"bucket_id": bucketID, "bridges": []models.BridgeBinding{{WorkspaceID: workspaceID.String()}}})
			// Fixture serialization errors cannot count as authorization outcomes.
			if err != nil {
				t.Fatal(err)
			}
			plan := &store.ConfigPlan{ID: uuid.New(), ConfigType: store.ConfigType(kind), DesiredState: []byte(desired), ResolvedPayload: raw, Revision: 1}
			resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{plan: plan})
			actor := accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}
			for _, phase := range []string{"plan", "apply"} {
				body := `{"config_key":"` + kind + `:echo:1.0.0","config":` + desired + `}`
				policy := dynamicDesiredConfigPlan
				// Apply must use stored bindings, never another client-supplied Bridge scope.
				if phase == "apply" {
					body = `{"plan_id":"` + plan.ID.String() + `"}`
					policy = dynamicDesiredConfigApply
				}
				request := httptest.NewRequest(http.MethodPost, "/"+strings.ReplaceAll(kind, "_", "-")+"-config/"+phase, strings.NewReader(body))
				requirements, err := resolver.ResolveControlRequirements(context.Background(), actor, policy, nil, request)
				// Bridge-only scope must yield app, bucket, and workspace delegation authority.
				if err != nil || len(requirements) != 3 {
					t.Fatalf("%s requirements=%v error=%v", phase, requirements, err)
				}
				assertBridgeControlAuthority(t, actor, requirements)
			}
		})
	}
}

// assertBridgeControlAuthority proves the fix admits owners while denying identical callers without Bridge delegation.
func assertBridgeControlAuthority(t *testing.T, actor accesscontrol.Actor, requirements []accesscontrol.Requirement) {
	t.Helper()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, attachmentAdminGrants(actor.WorkspaceID)...)
	// Use the shipped role policy instead of an invented permissive authorizer.
	if err != nil {
		t.Fatal(err)
	}
	actor.Authorization = snapshot
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirements...); err != nil {
		t.Fatal(err)
	}
	var grants []accesscontrol.Grant
	for _, requirement := range requirements {
		// Withhold only Bridge delegation so normal app and bucket access cannot substitute for it.
		if requirement.Permission != accesscontrol.PermissionBridgeManage {
			grants = append(grants, accesscontrol.Grant{Permission: requirement.Permission, Resource: requirement.Resource})
		}
	}
	actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(2, grants...)
	// A malformed test snapshot must fail independently of the expected policy denial.
	if err != nil {
		t.Fatal(err)
	}
	err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirements...)
	missing := accesscontrol.MissingRequirements(err)
	// The denial identifies precisely the required workspace sharing grant.
	if !errors.Is(err, accesscontrol.ErrPermissionDenied) || len(missing) != 1 || missing[0].Permission != accesscontrol.PermissionBridgeManage || missing[0].Resource.ID != actor.WorkspaceID {
		t.Fatalf("denial=%v missing=%v", err, missing)
	}
}

// TestStoredBridgeControlScopeRejectsMalformedPlans ensures local scope cannot turn invalid stored plans into authorized ones.
func TestStoredBridgeControlScopeRejectsMalformedPlans(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     store.ConfigType
		bindings []models.BridgeBinding
	}{
		{"missing workspace", store.ConfigTypeSDK, []models.BridgeBinding{{}}},
		{"nil workspace", store.ConfigTypeSDK, []models.BridgeBinding{{WorkspaceID: uuid.Nil.String()}}},
		{"unsupported kind", store.ConfigTypeWebhook, []models.BridgeBinding{{WorkspaceID: uuid.NewString()}}},
		{"oversized", store.ConfigTypeSDK, make([]models.BridgeBinding, 101)},
	} {
		// Each invalid stored scope must fail closed even before the apply handler rechecks contracts.
		t.Run(test.name, func(t *testing.T) {
			_, err := storedAppAttachmentRequirements(test.kind, nil, test.bindings)
			if !errors.Is(err, accesscontrol.ErrPolicyDenied) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
