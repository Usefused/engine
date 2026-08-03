package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

func TestWorkspaceShareGraphQLListsAndMutatesBoundedAccess(t *testing.T) {
	bucketID := uuid.New()
	shareID := uuid.New()
	share := store.WorkspaceShare{ID: shareID, RoleSlug: accesscontrol.RoleBucketUser, RoleDisplayName: "Bucket user",
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}, ResourceDisplayName: "company", CreatedAt: time.Now()}
	testStore := &teamGraphQLTestStore{workspaceShares: []store.WorkspaceShare{share}, workspaceTotal: 1,
		workspaceGrant: store.WorkspaceShareMutationResult{Share: share, AuthorizationRevision: 7, Changed: true},
		referenceIDs:   map[string]uuid.UUID{"bucket:company": bucketID}}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead, accesscontrol.PermissionAccessManage)

	list := executeTeamGraphQL(t, testStore, actor, `query { workspaceShares(resource_type:BUCKET,limit:5,offset:0) { total items { resource_id role_slug } } }`)
	if list.Code != 200 || testStore.workspaceOptions.ResourceType == nil || *testStore.workspaceOptions.ResourceType != accesscontrol.ResourceBucket {
		t.Fatalf("workspace share list = %d/%s, options %#v", list.Code, list.Body.String(), testStore.workspaceOptions)
	}

	mutation := executeTeamGraphQL(t, testStore, actor, `mutation { grantWorkspaceBucketAccess(bucket_id:"company") { changed share { role_slug } } }`)
	if mutation.Code != 200 || !mutationChanged(t, mutation, "grantWorkspaceBucketAccess") {
		t.Fatalf("workspace share mutation = %d/%s", mutation.Code, mutation.Body.String())
	}
	if testStore.workspaceMutation.Resource.Type != accesscontrol.ResourceBucket || testStore.workspaceMutation.Resource.ID != bucketID || testStore.workspaceMutation.Actor.SubjectID != actor.SubjectID {
		t.Fatalf("workspace mutation = %#v", testStore.workspaceMutation)
	}
	if len(testStore.referenceQueries) != 1 || testStore.referenceQueries[0].Kind != store.ReferenceBucket {
		t.Fatalf("workspace reference queries = %#v", testStore.referenceQueries)
	}
}

func TestWorkspaceShareGraphQLRequiresAccessManage(t *testing.T) {
	testStore := &teamGraphQLTestStore{}
	actor := actorWithTeamPermissions(t, accesscontrol.PermissionAccessRead)
	response := executeTeamGraphQL(t, testStore, actor, `mutation { grantWorkspaceArtifactAccess(artifact_id:"`+uuid.NewString()+`") { changed } }`)
	if response.Code != 403 {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
}
