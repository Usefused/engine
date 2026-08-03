package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestWorkspaceShareValidationOnlyAllowsBoundedResourceRoles(t *testing.T) {
	actor := MutationActor{SubjectID: uuid.New(), CredentialID: uuid.New(), RequestID: "workspace-share-test", TraceID: "0123456789abcdef0123456789abcdef"}
	for _, resourceType := range []accesscontrol.ResourceType{accesscontrol.ResourceBucket, accesscontrol.ResourceArtifact} {
		input := WorkspaceShareMutation{Resource: accesscontrol.ResourceRef{Type: resourceType, ID: uuid.New()}, Actor: actor}
		if err := validateWorkspaceShareMutation(input); err != nil {
			t.Fatalf("validate %s share: %v", resourceType, err)
		}
	}
	invalid := WorkspaceShareMutation{Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: uuid.New()}, Actor: actor}
	if err := validateWorkspaceShareMutation(invalid); !errors.Is(err, ErrInvalidWorkspaceShare) {
		t.Fatalf("service share error = %v, want ErrInvalidWorkspaceShare", err)
	}
	if role, ok := workspaceShareRole(accesscontrol.ResourceArtifact); !ok || role != accesscontrol.RoleArtifactUser {
		t.Fatalf("artifact share role = %q/%v", role, ok)
	}
}
