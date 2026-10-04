package oauthprovider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// sharedCoreTestPool isolates the real schema so migration tests cannot mutate an existing workspace.
func sharedCoreTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	// Unit-only runs remain usable without a database; CI integration runs provide one explicitly.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := "oauth_core_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+identifier)
	require.NoError(t, err)
	// Cleanup outlives the test context and removes only this test's schema.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
		require.NoError(t, err)
	})
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestSharedCorePostgresLifecycle exercises the shared protocol server against Fused's actual store.
func TestSharedCorePostgresLifecycle(t *testing.T) {
	pool := sharedCoreTestPool(t)
	ctx := t.Context()
	repository := store.NewPostgresStore(pool)
	accountID := uuid.New()
	_, err := repository.BootstrapWorkspace(ctx, accountID, "Shared OAuth test")
	require.NoError(t, err)
	owner, err := accesscontrol.BootstrapOwner(ctx, repository.(accesscontrol.BootstrapRepository), accountID, "fsk_shared_oauth_test")
	require.NoError(t, err)
	actor := browserActor(t, "service.read")
	actor.SubjectID, actor.CredentialID = owner.SubjectID, owner.CredentialID
	provider, err := NewService(repository.(store.OAuthClientStore), &fakeRevisionSink{})
	require.NoError(t, err)
	loader := repository.(accesscontrol.PrincipalLoader)

	registration, err := provider.RegisterClient(ctx, RegisterClientRequest{
		Name: "Local integration", RedirectURI: "http://localhost:4321/callback", Scopes: []string{"service.read"},
	})
	require.NoError(t, err)
	// A partially elapsed client lifetime must cap both token grants and their advertised TTL.
	_, err = pool.Exec(ctx, `UPDATE fused_oauth_clients SET expires_at = NOW() + INTERVAL '7 minutes' WHERE client_id = $1`, registration.ClientID)
	require.NoError(t, err)
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	authorize := AuthorizeRequest{
		ClientID: registration.ClientID, RedirectURI: "http://localhost:4321/callback",
		ResponseType: "code", Scope: []string{"service.read"}, State: "round-trip-state",
		CodeChallenge: base64.RawURLEncoding.EncodeToString(digest[:]), CodeChallengeMethod: "S256",
	}
	consent, err := provider.Authorize(ctx, actor, authorize)
	require.NoError(t, err)
	require.True(t, consent.RequiresConsent)
	require.NotNil(t, consent.Client.ExpiresAt)
	redirect, err := provider.Consent(ctx, actor, ConsentRequest{
		ClientID: authorize.ClientID, RedirectURI: authorize.RedirectURI, Scope: authorize.Scope,
		State: authorize.State, CodeChallenge: authorize.CodeChallenge, CodeChallengeMethod: "S256",
	})
	require.NoError(t, err)
	callback, err := url.Parse(redirect)
	require.NoError(t, err)
	require.Equal(t, authorize.State, callback.Query().Get("state"))
	request := TokenRequest{
		GrantType: "authorization_code", ClientID: registration.ClientID, ClientSecret: registration.ClientSecret,
		Code: callback.Query().Get("code"), RedirectURI: authorize.RedirectURI, CodeVerifier: "wrong-verifier",
	}
	// A bad verifier must deny the exchange without burning the code.
	_, err = provider.Token(ctx, request)
	require.ErrorIs(t, err, ErrInvalidGrant)
	request.CodeVerifier = verifier
	tokens, err := provider.Token(ctx, request)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(tokens.AccessToken, accessTokenPrefix))
	require.True(t, strings.HasPrefix(tokens.RefreshToken, refreshTokenPrefix))
	require.Greater(t, tokens.ExpiresIn, int64(0))
	require.LessOrEqual(t, tokens.ExpiresIn, int64(420))
	principal, err := loader.LoadControlPrincipal(ctx, hashSecret(tokens.AccessToken))
	require.NoError(t, err)
	require.Equal(t, owner.SubjectID, principal.SubjectID)
	// Replaying a consumed authorization code must not mint a second grant.
	_, err = provider.Token(ctx, request)
	require.ErrorIs(t, err, ErrInvalidGrant)

	refresh := TokenRequest{GrantType: "refresh_token", ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, RefreshToken: tokens.RefreshToken}
	rotated, err := provider.Token(ctx, refresh)
	require.NoError(t, err)
	require.LessOrEqual(t, rotated.ExpiresIn, int64(420))
	// Reusing an old refresh token revokes every credential in its rotation family.
	_, err = provider.Token(ctx, refresh)
	require.ErrorIs(t, err, ErrInvalidGrant)
	refresh.RefreshToken = rotated.RefreshToken
	_, err = provider.Token(ctx, refresh)
	require.ErrorIs(t, err, ErrInvalidGrant)
	_, err = loader.LoadControlPrincipal(ctx, hashSecret(rotated.AccessToken))
	require.Error(t, err)

	// Existing consent permits a new grant, but disconnection revokes it immediately.
	approved, err := provider.Authorize(ctx, actor, authorize)
	require.NoError(t, err)
	require.False(t, approved.RequiresConsent)
	callback, err = url.Parse(approved.RedirectURL)
	require.NoError(t, err)
	request.Code = callback.Query().Get("code")
	tokens, err = provider.Token(ctx, request)
	require.NoError(t, err)
	require.NoError(t, provider.RevokeConnectedApp(ctx, actor, consent.Client.ID))
	_, err = loader.LoadControlPrincipal(ctx, hashSecret(tokens.AccessToken))
	require.Error(t, err)
	apps, err := provider.ListConnectedApps(ctx, actor)
	require.NoError(t, err)
	require.Empty(t, apps)

	// Expiry is enforced at lookup without waiting for the core cleanup worker.
	_, err = pool.Exec(ctx, `UPDATE fused_oauth_clients SET expires_at = NOW() - INTERVAL '1 second' WHERE client_id = $1`, registration.ClientID)
	require.NoError(t, err)
	_, err = provider.Authorize(ctx, actor, authorize)
	require.ErrorIs(t, err, ErrUnauthorizedClient)
	_, err = provider.Token(ctx, refresh)
	require.ErrorIs(t, err, ErrUnauthorizedClient)
}
