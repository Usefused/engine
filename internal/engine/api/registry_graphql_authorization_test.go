package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestRegistryGraphQLAuthorizationCollectsAliasesAndFragments(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	body := []byte(`{"query":"query Dashboard { catalogue: services { total } ...Catalogue } fragment Catalogue on Query { globalServiceAnalytics { total_services } }","operationName":"Dashboard"}`)

	operation, err := authorizeRegistryGraphQLOperation(ctx, body)
	if err != nil || operation != "query" {
		t.Fatalf("operation/error = %q/%v", operation, err)
	}
}

func TestRegistryGraphQLAuthorizationIsAllOrNothing(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueManage)
	ctx, capture := accesscontrol.ContextWithRequiredPermissionsCapture(context.Background())
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	body := []byte(`{"query":"mutation Dashboard { updateServicePublic setConnectionProfile }"}`)

	_, err := authorizeRegistryGraphQLOperation(ctx, body)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 2 {
		t.Fatalf("denial = %#v, %v", denied, err)
	}
	requirements, captured := capture.RequiredPermissions()
	if !captured || len(requirements) != 3 {
		t.Fatalf("captured requirements = %#v/%t, want all root-field requirements", requirements, captured)
	}
}

func TestRegistryGraphQLAuthorizationUsesSelectedMutation(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	body := []byte(`{"query":"query Read { services { total } } mutation Publish { updateServicePublic(serviceId: \"x\", isPublic: true) }","operationName":"Publish"}`)

	_, err := authorizeRegistryGraphQLOperation(ctx, body)
	if !errors.Is(err, accesscontrol.ErrPermissionDenied) {
		t.Fatalf("error = %v, want permission denied", err)
	}
}

func TestRegistryGraphQLAuthorizationCapturesOnlyActualMissingRequirement(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionServiceManage)
	ctx, capture := accesscontrol.ContextWithRequiredPermissionsCapture(context.Background())
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	body := []byte(`{"query":"mutation { setConnectionProfile }"}`)
	_, err := authorizeRegistryGraphQLOperation(ctx, body)
	if !errors.Is(err, accesscontrol.ErrPermissionDenied) {
		t.Fatalf("error = %v, want permission denied", err)
	}
	missing, ok := accesscontrol.MissingPermissionsFromContext(ctx)
	if !ok || len(missing) != 1 || missing[0].Permission != accesscontrol.PermissionCredentialsManage {
		t.Fatalf("missing requirements = %#v/%v", missing, ok)
	}
	requested, ok := capture.RequiredPermissions()
	if !ok || len(requested) != 2 || requested[0].Permission != accesscontrol.PermissionServiceManage {
		t.Fatalf("requested requirements = %#v/%v", requested, ok)
	}
}

func TestRegistryGraphQLAuthorizationFailsClosedForUnknownRoot(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.AllPermissions()...)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	_, err := authorizeRegistryGraphQLOperation(ctx, []byte(`{"query":"{ futureUnclassifiedField }"}`))
	if err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("error = %v, want unclassified policy error", err)
	}
}

// TestRegistryGraphQLAuthorizationClassifiesDiscoveryAndRuntimeContractRoots keeps pre-enable catalogue discovery on the reviewed read permission.
func TestRegistryGraphQLAuthorizationClassifiesDiscoveryAndRuntimeContractRoots(t *testing.T) {
	// Batched SDK/workspace initialization must discover candidates before any workspace service grant exists.
	roots := []string{"endpointByName", "serviceCandidatesByRefs", "serviceRuntimeContracts", "serviceVersionExecutionAuthContracts", "serviceWebhookMetadata", "serviceVersionImportIdentities"}

	for _, root := range roots {
		t.Run(root, func(t *testing.T) {
			body := []byte(`{"query":"query { ` + root + ` }"}`)
			t.Run("catalogue read", func(t *testing.T) {
				actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
				ctx := accesscontrol.ContextWithActor(context.Background(), actor)
				operation, err := authorizeRegistryGraphQLOperation(ctx, body)
				if err != nil || operation != "query" {
					t.Fatalf("operation/error = %q/%v", operation, err)
				}
			})
			t.Run("permission denied", func(t *testing.T) {
				actor := registryPolicyActor(t)
				ctx := accesscontrol.ContextWithActor(context.Background(), actor)
				_, err := authorizeRegistryGraphQLOperation(ctx, body)
				if !errors.Is(err, accesscontrol.ErrPermissionDenied) {
					t.Fatalf("error = %v, want permission denied", err)
				}
			})
		})
	}
}

func TestRegistryGraphQLAuthorizationRejectsUnclassifiedRootKinds(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	actor := registryPolicyActor(t, accesscontrol.AllPermissions()...)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	tests := []struct {
		name  string
		query string
	}{
		{name: "unknown query", query: `query { futureUnclassifiedField }`},
		{name: "mutation root in query", query: `query { updateServicePublic }`},
		{name: "query root in mutation", query: `mutation { serviceRuntimeContracts }`},
		{name: "unknown mutation", query: `mutation { futureUnclassifiedMutation }`},
		{name: "introspection", query: `query { __schema { queryType { name } } }`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"query":` + strconv.Quote(test.query) + `}`)
			_, err := authorizeRegistryGraphQLOperation(ctx, body)
			if err == nil || !strings.Contains(err.Error(), "unclassified") {
				t.Fatalf("error = %v, want unclassified policy error", err)
			}
		})
	}
}

func registryPolicyActor(t *testing.T, permissions ...accesscontrol.Permission) accesscontrol.Actor {
	t.Helper()
	workspaceID := uuid.New()
	grants := make([]accesscontrol.Grant, 0, len(permissions))
	for _, permission := range permissions {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatal(err)
	}
	return accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
}
