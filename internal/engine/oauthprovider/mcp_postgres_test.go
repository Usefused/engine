package oauthprovider

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMCPSharedCorePostgres proves public registration, consent, PKCE, rotation, and revocation use the real protocol/store boundary.
func TestMCPSharedCorePostgres(t *testing.T) {
	pool := sharedCoreTestPool(t)
	ctx := t.Context()
	repository := store.NewPostgresStore(pool)
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	_, err := repository.BootstrapWorkspace(ctx, accountID, "MCP OAuth test")
	require.NoError(t, err)
	owner, err := accesscontrol.BootstrapOwner(ctx, repository.(accesscontrol.BootstrapRepository), accountID, "fsk_mcp_oauth_test")
	require.NoError(t, err)
	actor := browserActor(t)
	actor.SubjectID, actor.CredentialID = owner.SubjectID, owner.CredentialID
	_, err = pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,owner_subject_id) VALUES ($1,$2,'mcp','oauth-test','OAuth test',$3)`, familyID, accountID, owner.SubjectID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status) VALUES ($1,$2,$3,'1.0.0','oauth-test','test','active')`, appID, familyID, accountID)
	require.NoError(t, err)
	provider, err := NewService(repository.(store.OAuthClientStore), &fakeRevisionSink{})
	require.NoError(t, err)
	registration, err := provider.RegisterMCPClient(ctx, MCPRegistration{ClientName: "Public MCP test", RedirectURIs: []string{"https://client.example/callback"}, TokenEndpointAuthMethod: "none"})
	require.NoError(t, err)
	require.Empty(t, registration.ClientSecret)
	require.NotNil(t, registration.Client.ExpiresAt)
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	request := AuthorizeRequest{ClientID: registration.Client.ClientID, RedirectURI: registration.Client.RedirectURIs[0], ResponseType: "code", Scope: []string{store.OAuthMCPScope}, State: "mcp-state", CodeChallenge: base64.RawURLEncoding.EncodeToString(digest[:]), CodeChallengeMethod: "S256"}
	consent, err := provider.AuthorizeMCP(ctx, actor, request)
	require.NoError(t, err)
	require.True(t, consent.RequiresConsent)
	resource := "https://engine.example/mcp/" + familyID.String()
	redirect, err := provider.ConsentMCP(ctx, actor, ConsentRequest{ClientID: request.ClientID, RedirectURI: request.RedirectURI, Scope: request.Scope, State: request.State, CodeChallenge: request.CodeChallenge, CodeChallengeMethod: request.CodeChallengeMethod}, store.OAuthMCPGrant{Resource: resource, AppFamilyID: familyID, AppID: appID, Operations: []string{"readMessages"}})
	require.NoError(t, err)
	callback, err := url.Parse(redirect)
	require.NoError(t, err)
	require.Equal(t, request.State, callback.Query().Get("state"))
	// Approved dynamic clients must outlive the initial registration window, while every connection still prompts.
	consent, err = provider.AuthorizeMCP(ctx, actor, request)
	require.NoError(t, err)
	require.True(t, consent.RequiresConsent)
	require.Nil(t, consent.Client.ExpiresAt)
	tokenRequest := TokenRequest{GrantType: "authorization_code", ClientID: request.ClientID, RedirectURI: request.RedirectURI, Code: callback.Query().Get("code"), CodeVerifier: "wrong-verifier"}
	_, err = provider.TokenMCP(ctx, tokenRequest, resource)
	require.ErrorIs(t, err, ErrInvalidGrant)
	tokenRequest.CodeVerifier = verifier
	tokens, err := provider.TokenMCP(ctx, tokenRequest, resource)
	require.NoError(t, err)
	projection, err := repository.AuthorizeApp(ctx, appID, hashSecret(tokens.AccessToken))
	require.NoError(t, err)
	require.True(t, projection.TokenPolicy.AllowsOperation("readMessages"))
	require.False(t, projection.TokenPolicy.AllowsOperation("deleteMessages"))
	refresh := TokenRequest{GrantType: "refresh_token", ClientID: request.ClientID, RefreshToken: tokens.RefreshToken}
	rotated, err := provider.TokenMCP(ctx, refresh, resource)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, appID, hashSecret(rotated.AccessToken))
	require.NoError(t, err)
	// Disconnecting at the shared Connected apps surface must invalidate runtime use and refresh together.
	require.NoError(t, provider.RevokeConnectedApp(ctx, actor, registration.Client.ID))
	_, err = repository.AuthorizeApp(ctx, appID, hashSecret(rotated.AccessToken))
	require.Error(t, err)
	refresh.RefreshToken = rotated.RefreshToken
	_, err = provider.TokenMCP(ctx, refresh, resource)
	require.ErrorIs(t, err, ErrInvalidGrant)
}
