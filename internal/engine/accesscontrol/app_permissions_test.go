package accesscontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestAppCreationPermissionsAreIsolated exercises every pair of types, not only positive names.
func TestAppCreationPermissionsAreIsolated(t *testing.T) {
	workspace := ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}
	for _, ownType := range []string{"sdk", "mcp", "api", "webhook"} {
		permission := AppPermission(ownType, PermissionAppCreate)
		snapshot, err := NewAuthorizationSnapshot(1, Grant{Permission: permission, Resource: workspace})
		// Fixture construction must use the same strict grant catalogue as production.
		if err != nil {
			t.Fatal(err)
		}
		actor := Actor{Authorization: snapshot}
		for _, targetType := range []string{"sdk", "mcp", "api", "webhook"} {
			err := (SnapshotAuthorizer{}).CheckAll(t.Context(), actor, Requirement{Permission: AppPermission(targetType, PermissionAppCreate), Resource: workspace})
			// Only the explicitly granted type may pass a workspace creation check.
			if (err == nil) != (ownType == targetType) {
				t.Errorf("%s grant creating %s: %v", ownType, targetType, err)
			}
		}
	}
	for _, legacy := range []Permission{PermissionAppRead, PermissionAppUse, PermissionAppCreate, PermissionAppManage, PermissionAppTokensManage} {
		// Legacy scope names cannot silently become all-type grants.
		if ValidatePermission(legacy) == nil {
			t.Errorf("legacy scope accepted: %s", legacy)
		}
	}
}

// TestAppConfigPermissionType verifies the shared SDK route cannot erase API delivery intent.
func TestAppConfigPermissionType(t *testing.T) {
	for _, test := range []struct{ route, document, want string }{
		{"sdk", `{"kind":"sdk"}`, "sdk"},
		{"sdk", `{"kind":"sdk","generate":false}`, "api"},
		{"mcp", `{"kind":"mcp"}`, "mcp"},
		{"webhook", `{"kind":"webhook"}`, "webhook"},
		{"mcp", `{"kind":"sdk"}`, ""},
		{"sdk", `{"generate":"false"}`, ""},
		{"unknown", `{}`, ""},
	} {
		got, err := AppTypeFromConfig(test.route, []byte(test.document))
		// Invalid documents must fail before any side effects or resource lookup.
		if got != test.want || (err != nil) != (test.want == "") {
			t.Errorf("%s %s = %q, %v", test.route, test.document, got, err)
		}
	}
}

// TestCredentialSnapshotsDoNotShareOAuthScopes reproduces the same-subject cache-widening defect.
func TestCredentialSnapshotsDoNotShareOAuthScopes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		workspace := ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}
		permissions := []Permission{PermissionAppMCPCreate, PermissionAppSDKCreate}
		// Both orders matter because the previous cache used whichever credential loaded last.
		if reverse {
			permissions[0], permissions[1] = permissions[1], permissions[0]
		}
		loader := &principalLoaderStub{principal: ControlPrincipal{
			WorkspaceID: workspace.ID, SubjectID: uuid.New(), CredentialID: uuid.New(), Revision: 1,
			CredentialSource: "oauth_client", EffectiveGrants: []Grant{{Permission: permissions[0], Resource: workspace}},
		}}
		authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
		_, err := authenticator.AuthenticateControlCredential(context.Background(), "narrow-first")
		// Initial authentication must establish the first independent cache entry.
		if err != nil {
			t.Fatal(err)
		}
		loader.principal.CredentialID = uuid.New()
		loader.principal.EffectiveGrants = []Grant{{Permission: permissions[1], Resource: workspace}}
		_, err = authenticator.AuthenticateControlCredential(context.Background(), "narrow-second")
		// The second credential shares a subject and revision but not its scope.
		if err != nil {
			t.Fatal(err)
		}
		for index, credential := range []string{"narrow-first", "narrow-second"} {
			actor, err := authenticator.AuthenticateControlCredential(context.Background(), credential)
			// Cache hits must preserve the credential-specific permission intersection.
			if err != nil {
				t.Fatal(err)
			}
			for target, permission := range permissions {
				err := (SnapshotAuthorizer{}).CheckAll(t.Context(), actor, Requirement{Permission: permission, Resource: workspace})
				// A credential must retain its own grant and deny the other credential's grant.
				if target == index && err != nil {
					t.Fatal(err)
				}
				// A cache hit must not inherit the sibling credential’s consent.
				if target != index && !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("credential scope leaked: %s grants %s: %v", credential, permission, err)
				}
			}
		}
	}
}
