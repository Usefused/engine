package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestAppPlanRoutesRequireTheirOwnCreateScope checks the actual middleware and shared SDK/API route in every direction.
func TestAppPlanRoutesRequireTheirOwnCreateScope(t *testing.T) {
	workspaceID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	for _, grantedType := range []string{"sdk", "mcp", "api", "webhook"} {
		grants := artifactPlanSelectionGrants(serviceID, bucketID)
		grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.AppPermission(grantedType, accesscontrol.PermissionAppCreate), Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
		snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
		// The fixture grants exactly one creation type and the same dependencies for every target.
		if err != nil {
			t.Fatal(err)
		}
		actor := accesscontrol.Actor{WorkspaceID: workspaceID, SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "oauth_client", Authorization: snapshot}
		for _, targetType := range []string{"sdk", "mcp", "api", "webhook"} {
			kind, extra := targetType, ""
			// REST APIs must stay distinct despite sharing SDK configuration endpoints.
			if targetType == "api" {
				kind, extra = "sdk", `,"generate":false`
			}
			body := fmt.Sprintf(`{"config_key":"%s:typed:1.0.0","config":{"kind":"%s"%s,"bucket":"default","services":{"%s":{"version":"1.0.0"}}}}`, kind, kind, extra, acceptanceServiceName)
			status, calls := serveArtifactPlanRoute(t, "/"+kind+"-config/plan", serviceID, bucketID, nil, actor, body)
			// Cross-type denials must happen before the downstream handler executes.
			assertArtifactPlanHTTPDecision(t, status, calls, grantedType == targetType)
		}
	}
}

// TestAppApplyRoutesUseStoredType verifies a forged request label cannot alter the stored plan's scope.
func TestAppApplyRoutesUseStoredType(t *testing.T) {
	workspaceID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	dependencies := []accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
	}
	storedRequirements, err := accesscontrol.MarshalRequiredPermissions(dependencies)
	// Stored webhook dependencies must use the same canonical representation as actual plans.
	if err != nil {
		t.Fatal(err)
	}
	for _, grantedType := range []string{"sdk", "mcp", "api", "webhook"} {
		grants := []accesscontrol.Grant{{Permission: accesscontrol.AppPermission(grantedType, accesscontrol.PermissionAppCreate), Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}}
		for _, dependency := range dependencies {
			grants = append(grants, accesscontrol.Grant(dependency))
		}
		snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
		// Invalid fixture grants must not masquerade as a cross-type authorization denial.
		if err != nil {
			t.Fatal(err)
		}
		actor := accesscontrol.Actor{WorkspaceID: workspaceID, SubjectID: uuid.New(), CredentialID: uuid.New(), CredentialSource: "oauth_client", Authorization: snapshot}
		for _, targetType := range []string{"sdk", "mcp", "api", "webhook"} {
			kind, extra := targetType, ""
			// The stored delivery flag, not the shared route, distinguishes a REST API plan.
			if targetType == "api" {
				kind, extra = "sdk", `,"generate":false`
			}
			plan := &store.ConfigPlan{ID: uuid.New(), ConfigType: store.ConfigType(kind), ConfigKey: kind + ":typed:1.0.0", Revision: 1,
				DesiredState: []byte(fmt.Sprintf(`{"kind":"%s"%s}`, kind, extra)), RequiredPermissions: storedRequirements,
				ResolvedPayload: []byte(fmt.Sprintf(`{"bucket_id":"%s","selections":[{"service_id":"%s"}]}`, bucketID, serviceID)),
			}
			resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, &controlConfigRepositoryStub{plan: plan})
			calls := 0
			// Only an authorized apply may enter the mutation handler.
			handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
			request := httptest.NewRequest(http.MethodPost, "/"+kind+"-config/apply", strings.NewReader(fmt.Sprintf(`{"plan_id":"%s","kind":"%s"}`, plan.ID, grantedType)))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertArtifactPlanHTTPDecision(t, response.Code, calls, grantedType == targetType)
		}
	}
}
