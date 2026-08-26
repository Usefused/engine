package sandbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// staticAuthBindingStore makes any connected-account lookup observable for static-auth regressions.
type staticAuthBindingStore struct {
	*fixedBindingResolverStore
	connectionLookups int
}

// GetAuthConnection rejects user lookup because static credentials must come only from the bucket.
func (repository *staticAuthBindingStore) GetAuthConnection(context.Context, uuid.UUID, uuid.UUID, string, string) (*store.AuthConnection, error) {
	repository.connectionLookups++
	return nil, errors.New("unexpected connected-account lookup for static auth")
}

// GetAuthConnectionByIDForBuckets also rejects opaque connection lookup for static credentials.
func (repository *staticAuthBindingStore) GetAuthConnectionByIDForBuckets(context.Context, uuid.UUID, []uuid.UUID) (*store.AuthConnection, error) {
	repository.connectionLookups++
	return nil, errors.New("unexpected bound-connection lookup for static auth")
}

// TestSecretResolverNamedStaticAuthUsesBucketSecrets covers fixed MCP tokens and the shared dynamic SDK path.
func TestSecretResolverNamedStaticAuthUsesBucketSecrets(t *testing.T) {
	auths := []fusedobject.AuthConfig{
		{Name: "ApiKeyAuth", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "basicAuth", Type: "http", Scheme: "basic"},
		{Name: "bearerAuth", Type: "http", Scheme: "bearer"},
		{Name: "clientCertificate", Type: "mutualTLS"},
	}
	// Exercise both binding policies and name-only callers without special-casing any provider.
	for _, auth := range auths {
		for _, mode := range []store.AppTokenBindingMode{store.AppTokenBindingFixed, store.AppTokenBindingDynamic} {
			for _, selector := range []string{canonicalFusedAuthType(auth), ""} {
				// Each case owns its bucket and confirms selection metadata survives secret resolution.
				t.Run(auth.Name+"/"+string(mode)+"/"+selector, func(t *testing.T) {
					assertNamedStaticAuthUsesBucket(t, auth, mode, selector)
				})
			}
		}
	}
}

// assertNamedStaticAuthUsesBucket reproduces app-selected auth with unrelated end-user context and an OAuth alternative.
func assertNamedStaticAuthUsesBucket(t *testing.T, auth fusedobject.AuthConfig, mode store.AppTokenBindingMode, selector string) {
	t.Helper()
	masterKey := []byte("12345678901234567890123456789012")
	bucketID, serviceID := uuid.New(), uuid.New()
	base := &resolverMockStore{appRuntime: &store.AppRuntime{AppID: uuid.New(), BucketID: bucketID}}
	keys := staticSecretKeysForAuth(auth)
	// Encrypt every required static field so this exercises the real bucket decryption path.
	for _, key := range keys {
		base.secrets = append(base.secrets, staticAuthBindingSecret(t, masterKey, bucketID, serviceID, key))
	}
	repository := &staticAuthBindingStore{fixedBindingResolverStore: &fixedBindingResolverStore{resolverMockStore: base}}
	requirements := append(singleAuthRequirement(auth.Name), singleAuthRequirement("connectedOAuth")...)
	credentials := credentialsWithSelectionAuth(map[string]any{"fused_end_user_ref": "unrelated-user"}, models.SDKSelection{
		AuthType: selector, AuthName: auth.Name,
	}, requirements)
	resolved, _, err := NewSecretResolver(repository, masterKey).ResolveExecutionCredentials(context.Background(), CredentialRequest{
		AppID: base.appRuntime.AppID, TokenID: uuid.New(), BindingMode: mode,
		ServiceID: serviceID, AuthType: selector,
		Auths:        fusedobject.AuthConfigs{auth, {Name: "connectedOAuth", Type: "oauth2"}},
		Requirements: requirements, Passthrough: credentials,
	})
	// Static auth must succeed even though no connected-account binding exists.
	if err != nil {
		t.Fatalf("ResolveExecutionCredentials: %v", err)
	}
	assertStaticAuthBindingResult(t, repository, keys, credentials, resolved)
	// Retain both selectors for the dispatcher's independent auth-contract validation.
	if resolved["fused_auth_name"] != auth.Name || resolved["fused_auth_type"] != selector {
		t.Fatal("static auth selection was rewritten")
	}
}

// assertStaticAuthBindingResult verifies exact secret reads without connected lookup or mutation of caller input.
func assertStaticAuthBindingResult(t *testing.T, repository *staticAuthBindingStore, keys []string, input, resolved map[string]any) {
	t.Helper()
	// All static fields must be loaded while the caller's routing envelope stays credential-free.
	for _, key := range keys {
		if resolved[key] != "test-"+key || input[key] != nil {
			t.Fatal("static secret was not resolved exclusively into the execution envelope")
		}
	}
	// Only the selected scheme's exact secret set may be queried.
	if !reflect.DeepEqual(repository.getSecretsKeys, keys) || repository.getSecretsCalls != 1 || repository.listSecretsForBucketCalls != 0 {
		t.Fatal("static auth did not use one exact bucket secret-set lookup")
	}
	// Static selection must neither consult fixed bindings nor access a connected account.
	if repository.requestedToken != uuid.Nil || repository.connectionLookups != 0 || resolved["fused_connection_id"] != nil {
		t.Fatal("static auth entered connected-account routing")
	}
}

// staticAuthBindingSecret creates an encrypted, exact-bucket fixture without using real provider material.
func staticAuthBindingSecret(t *testing.T, masterKey []byte, bucketID, serviceID uuid.UUID, key string) store.WorkspaceSecret {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	// Fixture encryption failures must not masquerade as routing regressions.
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := store.EncryptWithDEK(dek, "test-"+key)
	// Reject incomplete fixtures before exercising the resolver.
	if err != nil {
		t.Fatal(err)
	}
	return store.WorkspaceSecret{
		WorkspaceSecretMeta: store.WorkspaceSecretMeta{
			ID: uuid.New(), BucketID: bucketID, ServiceID: serviceID, KeyName: key, CredentialType: "string",
		},
		EncryptedDEK: wrappedDEK, EncryptedValue: encrypted,
	}
}

// TestSelectedConnectedAuthNameValidatesNamedScheme keeps static bypasses tied to the operation's declared auth contract.
func TestSelectedConnectedAuthNameValidatesNamedScheme(t *testing.T) {
	auths := fusedobject.AuthConfigs{
		{Name: "ApiKeyAuth", Type: "apiKey"}, {Name: "oauthAuth", Type: "oauth2"},
		{Name: "oidcAuth", Type: "openIdConnect"}, {Name: "unusedAuth", Type: "apiKey"},
	}
	requirements := append(singleAuthRequirement("ApiKeyAuth"), singleAuthRequirement("oauthAuth")...)
	requirements = append(requirements, singleAuthRequirement("oidcAuth")...)
	cases := []struct {
		name, selector, want string
		wantError            bool
	}{
		{name: "ApiKeyAuth", selector: "api_key"},
		{name: "ApiKeyAuth"},
		{name: "oauthAuth", selector: "oauth", want: "oauthAuth"},
		{name: "oauthAuth", want: "oauthAuth"},
		{name: "oidcAuth", selector: "oidc", want: "oidcAuth"},
		{name: "oidcAuth", want: "oidcAuth"},
		{name: "unknownAuth", selector: "api_key", wantError: true},
		{name: "unusedAuth", selector: "api_key", wantError: true},
		{name: "oauthAuth", selector: "api_key", wantError: true},
		{name: "ApiKeyAuth", selector: "oauth", wantError: true},
		{name: "oauthAuth", selector: "oidc", wantError: true},
		{name: "ApiKeyAuth", selector: "unknown", wantError: true},
	}
	// Cover name/type agreement before either fixed binding or connected lookup can run.
	for _, tc := range cases {
		// Each selector pair is checked without a database or provider credential.
		t.Run(tc.name+"/"+tc.selector, func(t *testing.T) {
			got, err := selectedConnectedAuthName(map[string]any{
				"fused_auth_name": tc.name, "fused_auth_type": tc.selector,
			}, auths, requirements)
			// A declared static scheme is a no-op, not an invalid connected scheme.
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("selectedConnectedAuthName = %q, %v; want %q, error=%v", got, err, tc.want, tc.wantError)
			}
		})
	}
}

// TestSecretResolverNamedConnectedAuthStillRequiresFixedBinding protects both OAuth and OIDC from static-auth fallthrough.
func TestSecretResolverNamedConnectedAuthStillRequiresFixedBinding(t *testing.T) {
	// Both supported connected families must retain the same fail-closed token policy.
	for _, auth := range []fusedobject.AuthConfig{{Name: "oauthAuth", Type: "oauth2"}, {Name: "oidcAuth", Type: "openIdConnect"}} {
		// A valid named selector without its fixed binding must remain unexecutable.
		t.Run(auth.Name, func(t *testing.T) {
			repository := &fixedBindingResolverStore{resolverMockStore: &resolverMockStore{}}
			request := CredentialRequest{
				TokenID: uuid.New(), ServiceID: uuid.New(), BindingMode: store.AppTokenBindingFixed,
				Auths: fusedobject.AuthConfigs{auth}, Requirements: singleAuthRequirement(auth.Name),
			}
			err := (&secretResolver{db: repository}).applyAppTokenBinding(context.Background(), request, map[string]any{
				"fused_auth_name": auth.Name, "fused_auth_type": canonicalFusedAuthType(auth),
				"fused_end_user_ref": "caller-user", "fused_connection_id": uuid.NewString(),
			})
			// Caller connection selectors cannot substitute for the token's persisted binding.
			if !errors.Is(err, store.ErrAppTokenBindingInvalid) || repository.requestedToken != request.TokenID || repository.requestedAuth != auth.Name {
				t.Fatalf("fixed connected auth did not fail closed: %v", err)
			}
		})
	}
}

// TestSecretResolverNamedConnectedAuthUsesFixedBinding keeps named OAuth/OIDC routing pinned to the token rather than caller identity.
func TestSecretResolverNamedConnectedAuthUsesFixedBinding(t *testing.T) {
	// The same fixed-binding resolver owns both connected auth families.
	for _, authType := range []string{"oauth2", "openIdConnect"} {
		// Exercise the full encrypted connection path with an explicitly named selection.
		t.Run(authType, func(t *testing.T) {
			masterKey := []byte("12345678901234567890123456789012")
			bucketID, serviceID, tokenID := uuid.New(), uuid.New(), uuid.New()
			auth := fusedobject.AuthConfig{Name: "bearerAuth", Type: authType}
			connection := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "bound-user", "bound-token", "", time.Time{})
			connection.AuthType = canonicalFusedAuthType(auth)
			repository := &fixedBindingResolverStore{
				resolverMockStore: &resolverMockStore{
					appRuntime: &store.AppRuntime{AppID: uuid.New(), BucketID: bucketID}, authConnection: &connection,
				},
				binding: &store.AppTokenBinding{TokenID: tokenID, ServiceID: serviceID, AuthName: auth.Name, AuthConnectionID: connection.ID},
			}
			resolved, _, err := NewSecretResolver(repository, masterKey).ResolveExecutionCredentials(context.Background(), CredentialRequest{
				AppID: repository.appRuntime.AppID, TokenID: tokenID, ServiceID: serviceID, BindingMode: store.AppTokenBindingFixed,
				AuthType: canonicalFusedAuthType(auth), Auths: fusedobject.AuthConfigs{auth}, Requirements: singleAuthRequirement(auth.Name),
				Passthrough: map[string]any{
					"fused_auth_type": canonicalFusedAuthType(auth), "fused_auth_name": auth.Name,
					"fused_end_user_ref": "caller-user", "fused_connection_id": uuid.NewString(),
				},
			})
			// The selected named connection must decrypt successfully without falling through to bucket tokens.
			if err != nil {
				t.Fatalf("ResolveExecutionCredentials: %v", err)
			}
			// The binding's opaque connection identity must replace the caller's supplied connection.
			if resolved[auth.Name] != "bound-token" || resolved["fused_connection_id"] != connection.ID.String() || resolved["fused_end_user_ref"] != nil {
				t.Fatal("named connected auth did not use the authoritative fixed binding")
			}
			// Confirm the lookup stayed pinned to this token and service rather than a default connection.
			if repository.requestedToken != tokenID || repository.requestedService != serviceID || repository.requestedAuth != auth.Name {
				t.Fatal("named connected auth queried the wrong fixed-binding identity")
			}
		})
	}
}
