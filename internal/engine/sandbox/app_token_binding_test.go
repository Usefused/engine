package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type fixedBindingResolverStore struct {
	*resolverMockStore
	binding          *store.AppTokenBinding
	requestedToken   uuid.UUID
	requestedService uuid.UUID
	requestedAuth    string
}

func (repository *fixedBindingResolverStore) GetAppTokenBinding(_ context.Context, tokenID, serviceID uuid.UUID, authName string) (*store.AppTokenBinding, error) {
	repository.requestedToken = tokenID
	repository.requestedService = serviceID
	repository.requestedAuth = authName
	return repository.binding, nil
}

func (repository *fixedBindingResolverStore) GetAuthConnectionByIDForBuckets(_ context.Context, id uuid.UUID, bucketIDs []uuid.UUID) (*store.AuthConnection, error) {
	connection := repository.authConnection
	if connection == nil || connection.ID != id || len(bucketIDs) != 1 || connection.BucketID != bucketIDs[0] {
		return nil, nil
	}
	copy := *connection
	return &copy, nil
}

func TestSecretResolverFixedTokenBindingOverridesCallerSelectors(t *testing.T) {
	masterKey := []byte("12345678901234567890123456789012")
	bucketID, serviceID, tokenID := uuid.New(), uuid.New(), uuid.New()
	connection := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "bound-user", "connected-token", "", time.Time{})
	resourceID := uuid.New()
	base := &resolverMockStore{
		appRuntime:     &store.AppRuntime{AppID: uuid.New(), BucketID: bucketID},
		authConnection: &connection,
		connectionResource: &store.ConnectionResource{
			ID: resourceID, ConnectionID: connection.ID, BucketID: bucketID,
			ServiceID: serviceID, ResourceType: "jira_site", BaseURL: "https://bound.example.test",
		},
	}
	repository := &fixedBindingResolverStore{resolverMockStore: base, binding: &store.AppTokenBinding{
		TokenID: tokenID, ServiceID: serviceID, AuthName: "bearerAuth",
		AuthConnectionID: connection.ID, ResourceID: &resourceID,
	}}
	credentials, _, err := NewSecretResolver(repository, masterKey).ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: base.appRuntime.AppID, TokenID: tokenID, BindingMode: store.AppTokenBindingFixed,
		ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("bearerAuth"),
		Passthrough: map[string]any{
			"fused_end_user_ref": "attacker-selected-user",
			"fused_resource_id":  uuid.NewString(),
		},
	})
	if err != nil {
		t.Fatalf("ResolveExecutionCredentials: %v", err)
	}
	assertFixedBindingCredentials(t, credentials, repository, tokenID, serviceID, connection.ID, resourceID)
}

func TestSecretResolverFixedTokenBindingFailsClosedWhenMissing(t *testing.T) {
	serviceID, tokenID := uuid.New(), uuid.New()
	repository := &fixedBindingResolverStore{resolverMockStore: &resolverMockStore{
		appRuntime: &store.AppRuntime{AppID: uuid.New(), BucketID: uuid.New()},
	}}
	_, _, err := NewSecretResolver(repository, []byte("12345678901234567890123456789012")).ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: repository.appRuntime.AppID, TokenID: tokenID, BindingMode: store.AppTokenBindingFixed,
		ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("bearerAuth"),
	})
	if !errors.Is(err, store.ErrAppTokenBindingInvalid) {
		t.Fatalf("missing fixed binding error = %v, want ErrAppTokenBindingInvalid", err)
	}
}

func TestSecretResolverRejectsRemovedSelectionFieldBeforeBindingLookup(t *testing.T) {
	serviceID, serviceVersionID, tokenID := uuid.New(), uuid.New(), uuid.New()
	repository := &fixedBindingResolverStore{resolverMockStore: &resolverMockStore{
		preserveInvalidRuntime: true,
		appRuntime: &store.AppRuntime{
			AppID: uuid.New(), BucketID: uuid.New(), ScopeSchemaVersion: models.AppScopeSchemaVersion,
			Selections: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","definition_schema_version":3}]`),
		},
	}}
	_, _, err := NewSecretResolver(repository, []byte("12345678901234567890123456789012")).ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: repository.appRuntime.AppID, TokenID: tokenID, BindingMode: store.AppTokenBindingFixed,
		ServiceID: serviceID, AuthType: "oauth",
		Auths:        fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2"}},
		Requirements: singleAuthRequirement("bearerAuth"),
	})
	if err == nil || repository.requestedToken != uuid.Nil {
		t.Fatalf("legacy selection error = %v, binding token = %s", err, repository.requestedToken)
	}
}

func assertFixedBindingCredentials(t *testing.T, credentials map[string]any, repository *fixedBindingResolverStore, tokenID, serviceID, connectionID, resourceID uuid.UUID) {
	t.Helper()
	if credentials["bearerAuth"] != "connected-token" || credentials["fused_connection_id"] != connectionID.String() {
		t.Fatalf("fixed connection credential was not selected: %#v", credentials)
	}
	if credentials["fused_resource_id"] != resourceID.String() || credentials["fused_resource_base_url"] != "https://bound.example.test" {
		t.Fatalf("fixed resource was not selected: %#v", credentials)
	}
	if _, leaked := credentials["fused_end_user_ref"]; leaked {
		t.Fatalf("caller end-user selector survived fixed binding: %#v", credentials)
	}
	if repository.requestedToken != tokenID || repository.requestedService != serviceID || repository.requestedAuth != "bearerAuth" {
		t.Fatalf("binding lookup identity = %s/%s/%q", repository.requestedToken, repository.requestedService, repository.requestedAuth)
	}
}
