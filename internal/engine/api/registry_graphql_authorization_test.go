package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestRegistryGraphQLAuthorizationCollectsAliasesAndFragments(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead, accesscontrol.PermissionArtifactRead)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	body := []byte(`{"query":"query Dashboard { catalogue: services { total } ...Artifacts } fragment Artifacts on Query { sdks { total } }","operationName":"Dashboard"}`)

	operation, err := authorizeRegistryGraphQLOperation(ctx, body)
	if err != nil || operation != "query" {
		t.Fatalf("operation/error = %q/%v", operation, err)
	}
}

func TestRegistryGraphQLAuthorizationIsAllOrNothing(t *testing.T) {
	actor := registryPolicyActor(t, accesscontrol.PermissionCatalogueRead)
	ctx, capture := accesscontrol.ContextWithRequiredPermissionsCapture(context.Background())
	ctx = accesscontrol.ContextWithActor(ctx, actor)
	body := []byte(`{"query":"query Dashboard { services { total } sdks { total } }"}`)

	_, err := authorizeRegistryGraphQLOperation(ctx, body)
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 1 || denied.Missing[0].Permission != accesscontrol.PermissionArtifactRead {
		t.Fatalf("denial = %#v, %v", denied, err)
	}
	requirements, captured := capture.RequiredPermissions()
	if !captured || len(requirements) != 2 {
		t.Fatalf("captured requirements = %#v/%t, want both root-field requirements", requirements, captured)
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

func TestRegistryGraphQLPolicyCoversCurrentRegistrySchema(t *testing.T) {
	if got := len(registryGraphQLQueryPolicies) - 1; got != 33 {
		t.Fatalf("classified Registry queries = %d, want 33", got)
	}
	if got := len(registryGraphQLMutationPolicies) - 1; got != 3 {
		t.Fatalf("classified Registry mutations = %d, want 3", got)
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
