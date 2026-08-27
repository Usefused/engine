package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

// TestSetWorkspaceConnectionProfileValidatesAndMaterializesAtomically proves
// UI writes persist canonical defaults and compiled bindings together.
func TestSetWorkspaceConnectionProfileValidatesAndMaterializesAtomically(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	profileStore := &profileGraphQLStore{activeVersionID: versionID, serviceID: serviceID}
	verifier := &profileVerifier{metadata: &fusedobject.ServiceMetadata{
		ID: serviceID, ServiceVersionID: versionID,
		AuthConfigs: fusedobject.AuthConfigs{{Type: "oauth2"}},
	}}
	registry := &profileRegistryClient{operations: []fusedobject.Endpoint{{
		Name: "getAccessibleResources", Method: "GET",
	}}}
	ctx := profileGraphQLTestContext(accountID)
	args := map[string]interface{}{
		"service_id":         serviceID.String(),
		"service_version_id": versionID.String(), "version": "2026-07-01", "auth_type": "oauth",
		"profile": map[string]interface{}{
			"auth_type": "oauth",
			"resource_discovery": map[string]interface{}{
				"operation_id": "getAccessibleResources", "id_path": "$[*].id",
				"base_url_path": "$[*].url", "resource_type": "jira_site",
				"allowed_hosts": []interface{}{"api.atlassian.com"},
			},
			"bindings": []interface{}{map[string]interface{}{
				"value": "${resource.base_url}", "location": "base_url", "mode": "force",
			}},
		},
	}
	result, err := setWorkspaceConnectionProfile(graphql.ResolveParams{Context: ctx, Args: args}, profileStore, verifier, registry)
	if err != nil {
		t.Fatalf("setWorkspaceConnectionProfile: %v", err)
	}
	if result == nil || profileStore.profile == nil || len(profileStore.bindings) != 1 {
		t.Fatalf("profile was not materialized: result=%#v profile=%#v bindings=%#v", result, profileStore.profile, profileStore.bindings)
	}
	if profileStore.profile.Provenance != "workspace" || profileStore.profile.Layer != "override" || profileStore.bindings[0].SourceKind != "connection_resource" {
		t.Fatalf("materialized profile provenance/layer/source = %#v %#v", profileStore.profile, profileStore.bindings[0])
	}
	var snapshot connectionprofile.Profile
	if err := json.Unmarshal(profileStore.profile.ProfileSnapshot, &snapshot); err != nil {
		t.Fatalf("decode stored profile: %v", err)
	}
	if snapshot.ResourceDiscovery.AutoRun != "after_oauth_callback" || snapshot.ResourceDiscovery.Lifecycle != "authoritative" {
		t.Fatalf("stored profile was not normalized: %#v", snapshot.ResourceDiscovery)
	}
}

// TestSetWorkspaceConnectionProfileRejectsInactiveVersion proves an override
// cannot be written for a service version the workspace has not activated.
func TestSetWorkspaceConnectionProfileRejectsInactiveVersion(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	// activeVersionID left as uuid.Nil so the exact status lookup returns false.
	profileStore := &profileGraphQLStore{serviceID: serviceID}
	verifier := &profileVerifier{metadata: &fusedobject.ServiceMetadata{ID: serviceID, ServiceVersionID: versionID}}
	registry := &profileRegistryClient{}
	ctx := profileGraphQLTestContext(accountID)
	args := map[string]interface{}{
		"service_id": serviceID.String(), "service_version_id": versionID.String(),
		"version": "2026-07-01", "auth_type": "oauth",
		"profile": map[string]interface{}{"auth_type": "oauth", "bindings": []interface{}{}},
	}
	if _, err := setWorkspaceConnectionProfile(graphql.ResolveParams{Context: ctx, Args: args}, profileStore, verifier, registry); err == nil {
		t.Fatal("expected inactive service version to be rejected")
	}
}

// TestWorkspaceConnectionProfilesGraphQLFieldReturnsFlatProfiles protects CLI sync from credential-store coupling.
func TestWorkspaceConnectionProfilesGraphQLFieldReturnsFlatProfiles(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	profileStore := &profileGraphQLStore{profile: &store.WorkspaceConnectionProfile{
		ID: uuid.New(), ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "oauth",
		ProfileSnapshot: json.RawMessage(`{"auth_type":"oauth"}`),
	}}
	result, err := workspaceConnectionProfilesGraphQLField(profileStore).Resolve(graphql.ResolveParams{Context: profileGraphQLTestContext(accountID)})
	// Resolver failure would make the CLI sync regression reappear before projection assertions run.
	if err != nil {
		t.Fatalf("resolve workspace connection profiles: %v", err)
	}
	items, ok := result.([]map[string]interface{})
	// A flat row must carry both service identities so CLI can place it without bucket metadata.
	if !ok || len(items) != 1 || items[0]["service_id"] != serviceID.String() || items[0]["service_version_id"] != versionID.String() {
		t.Fatalf("unexpected workspace connection profile projection: %#v", result)
	}
}

func profileGraphQLTestContext(accountID uuid.UUID) context.Context {
	ctx := context.WithValue(context.Background(), mcpGraphQLActorContextKey, mcpGraphQLActor{accountID: accountID})
	return accesscontrol.ContextWithActor(ctx, accesscontrol.Actor{AccountID: accountID, WorkspaceID: uuid.New()})
}

type profileGraphQLStore struct {
	store.Store
	serviceID       uuid.UUID
	activeVersionID uuid.UUID
	profile         *store.WorkspaceConnectionProfile
	bindings        []store.WorkspaceConnectionBinding
}

// VerifyWorkspaceOwner supplies the owned workspace for resolver tests.
func (s *profileGraphQLStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

// IsWorkspaceServiceVersionActive models the targeted activation query used
// before an override write without manufacturing an in-memory version list.
func (s *profileGraphQLStore) IsWorkspaceServiceVersionActive(_ context.Context, serviceID, versionID uuid.UUID) (bool, error) {
	return serviceID == s.serviceID && versionID == s.activeVersionID && s.activeVersionID != uuid.Nil, nil
}

// UpsertWorkspaceProfileOverride captures both sides of the atomic materialization call.
func (s *profileGraphQLStore) UpsertWorkspaceProfileOverride(_ context.Context, profile store.WorkspaceConnectionProfile, bindings []store.WorkspaceConnectionBinding) (*store.WorkspaceConnectionProfile, error) {
	profile.ID = uuid.New()
	s.profile = &profile
	s.bindings = append([]store.WorkspaceConnectionBinding(nil), bindings...)
	return s.profile, nil
}

// ResetWorkspaceProfile simulates resetting a local workspace override.
func (s *profileGraphQLStore) ResetWorkspaceProfile(context.Context, uuid.UUID, uuid.UUID, string) error {
	s.profile = nil
	s.bindings = nil
	return nil
}

// GetEffectiveWorkspaceProfile returns the current test profile for revision calculation.
func (s *profileGraphQLStore) GetEffectiveWorkspaceProfile(context.Context, uuid.UUID, uuid.UUID, string) (*store.WorkspaceConnectionProfile, error) {
	return s.profile, nil
}

// GetEffectiveWorkspaceProfiles models the batch read used by workspace application.
func (s *profileGraphQLStore) GetEffectiveWorkspaceProfiles(context.Context, []store.WorkspaceProfileRef) ([]store.WorkspaceConnectionProfile, error) {
	if s.profile == nil {
		return nil, nil
	}
	return []store.WorkspaceConnectionProfile{*s.profile}, nil
}

// ListWorkspaceConnectProfiles supplies the bounded effective profile export used by declarative sync.
func (s *profileGraphQLStore) ListWorkspaceConnectProfiles(context.Context) ([]store.WorkspaceConnectionProfile, error) {
	// An absent fixture models a workspace with no exportable routing profiles.
	if s.profile == nil {
		return nil, nil
	}
	return []store.WorkspaceConnectionProfile{*s.profile}, nil
}

// ListWorkspaceProfileBindings returns the compiled rows captured by replacement.
func (s *profileGraphQLStore) ListWorkspaceProfileBindings(context.Context, uuid.UUID, uuid.UUID, string) ([]store.WorkspaceConnectionBinding, error) {
	return s.bindings, nil
}

// ListWorkspaceBindingsForExecution satisfies the execution-side store contract.
func (s *profileGraphQLStore) ListWorkspaceBindingsForExecution(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) ([]store.WorkspaceConnectionBinding, error) {
	return s.bindings, nil
}

// MarkWorkspaceProfilePublished satisfies WorkspaceProfileStore; this test
// store doesn't exercise the publish-to-Registry flow, so it's a no-op.
func (s *profileGraphQLStore) MarkWorkspaceProfilePublished(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

type profileVerifier struct {
	ServiceVerifier
	metadata *fusedobject.ServiceMetadata
}

// FetchServiceMetadata returns the pinned fixture contract.
func (v *profileVerifier) FetchServiceMetadata(context.Context, uuid.UUID, string) (*fusedobject.ServiceMetadata, error) {
	return v.metadata, nil
}

type profileRegistryClient struct {
	sandbox.RegistryClient
	operations []fusedobject.Endpoint
}

// FetchServiceOperations returns one bounded operation fixture.
func (c *profileRegistryClient) FetchServiceOperations(context.Context, uuid.UUID, uuid.UUID) ([]fusedobject.Endpoint, error) {
	return c.operations, nil
}
