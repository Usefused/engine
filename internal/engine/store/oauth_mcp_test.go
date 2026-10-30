package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type oauthMCPFixture struct {
	ctx        context.Context
	repository *postgresStore
	pool       *pgxpool.Pool
	actor      MutationActor
	client     OAuthClient
	grant      OAuthMCPGrant
}

// TestOAuthMCPPostgresConnectionCatalogue excludes other buckets and inactive resources from consent choices.
func TestOAuthMCPPostgresConnectionCatalogue(t *testing.T) {
	f := newOAuthMCPFixture(t)
	fixture := appTokenPolicyFixture{ctx: f.ctx, pool: f.pool, repository: f.repository, familyID: f.grant.AppFamilyID, appID: f.grant.AppID}
	target := fixture.seedFixedBindingTarget(t)
	// The shared legacy fixture bypasses normal persistence; match the canonical type written by provider callbacks.
	_, err := f.pool.Exec(f.ctx, `UPDATE fused_auth_connections SET auth_type='oauth' WHERE bucket_id=$1`, target.bucketID)
	require.NoError(t, err)
	selections, err := json.Marshal([]models.SDKSelection{
		{ServiceID: target.serviceA, AuthType: "oauth", AuthName: "gmail"},
		{ServiceID: target.serviceB, RequiredAuth: []models.SDKRequiredAuth{{AuthType: "oauth", AuthName: "drive"}}},
	})
	require.NoError(t, err)
	_, err = f.pool.Exec(f.ctx, `UPDATE fused_apps SET selections=$2 WHERE app_id=$1`, f.grant.AppID, selections)
	require.NoError(t, err)
	choices, err := f.repository.OAuthMCPConnections(f.ctx, f.grant.AppID)
	require.NoError(t, err)
	require.Len(t, choices, 3)
	// Disabling a resource removes its authority even though the provider account still exists.
	_, err = f.pool.Exec(f.ctx, `UPDATE fused_connection_resources SET is_active=false WHERE id=$1`, target.resourceB)
	require.NoError(t, err)
	choices, err = f.repository.OAuthMCPConnections(f.ctx, f.grant.AppID)
	require.NoError(t, err)
	// No inactive resource identity may survive in a fresh account catalogue.
	for _, choice := range choices {
		require.Nil(t, choice.ResourceID)
	}
	// Moving the family's bucket must not expose connections retained in its former bucket.
	_, err = f.pool.Exec(f.ctx, `DELETE FROM fused_app_family_buckets WHERE app_family_id=$1`, f.grant.AppFamilyID)
	require.NoError(t, err)
	choices, err = f.repository.OAuthMCPConnections(f.ctx, f.grant.AppID)
	require.NoError(t, err)
	require.Empty(t, choices)
}

// TestOAuthMCPPostgresManualRevocation prevents refresh from undoing revocation through the execution-token UI.
func TestOAuthMCPPostgresManualRevocation(t *testing.T) {
	f := newOAuthMCPFixture(t)
	metadata, issue, refresh := f.issue(t)
	_, err := f.repository.RevokeAppToken(f.ctx, f.grant.AppFamilyID, "OAuth "+metadata.ID.String())
	require.NoError(t, err)
	_, err = f.repository.AuthorizeApp(f.ctx, f.grant.AppID, issue.AccessTokenHash)
	require.Error(t, err)
	next, _, _ := oauthTestTokenIssue(time.Now())
	_, err = f.repository.RotateOAuthRefreshToken(ContextWithOAuthMCP(f.ctx, f.grant.Resource, nil), OAuthRefreshExchange{ClientID: f.client.ID, RefreshTokenHash: accesscontrol.HashControlCredential(refresh), Issue: next}, time.Now())
	require.Error(t, err)
}

// TestOAuthMCPPostgresPinnedVersion keeps version-specific grants scoped even through shared execution adapters.
func TestOAuthMCPPostgresPinnedVersion(t *testing.T) {
	f := newOAuthMCPFixture(t)
	f.grant.Resource = "https://engine.example/mcp/" + f.grant.AppID.String()
	_, issue, _ := f.issue(t)
	siblingID := uuid.New()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status)
	 SELECT $1::uuid,$2,account_id,'2.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, siblingID, f.grant.AppFamilyID)
	require.NoError(t, err)
	_, err = f.repository.AuthorizeApp(f.ctx, f.grant.AppID, issue.AccessTokenHash)
	require.NoError(t, err)
	_, err = f.repository.AuthorizeApp(f.ctx, siblingID, issue.AccessTokenHash)
	require.Error(t, err)
}

// newOAuthMCPFixture creates an isolated workspace with a real owner, OAuth client, and runnable MCP family.
func newOAuthMCPFixture(t *testing.T) oauthMCPFixture {
	t.Helper()
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	t.Cleanup(cancel)
	t.Cleanup(pool.Close)
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "mcp-oauth")
	family, app := uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,owner_subject_id)
	 SELECT $1::uuid,account_id,'mcp',$1::uuid::text,'Consent test',$2 FROM fused_workspaces WHERE singleton_key=1`, family, owner.SubjectID)
	// A real family is necessary to exercise foreign keys and canonical runtime authorization.
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status)
	 SELECT $1::uuid,$2,account_id,'1.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, app, family)
	// Fixture persistence or grant mutation must succeed before authority can be asserted.
	if err != nil {
		t.Fatal(err)
	}
	client, _ := oauthTestClient(t, ctx, repository, actor, "https://client.example/callback", []string{OAuthMCPScope})
	grant := OAuthMCPGrant{Resource: "https://engine.example/mcp/" + family.String(), AppFamilyID: family, AppID: app, Operations: []string{"readMessages"}}
	return oauthMCPFixture{ctx, repository, pool, actor, client, grant}
}

// issue obtains a real single-use code and exchanges it with its exact audience and PKCE verifier.
func (f oauthMCPFixture) issue(t *testing.T) (OAuthTokenMetadata, OAuthTokenIssue, string) {
	t.Helper()
	ctx := ContextWithOAuthMCP(f.ctx, f.grant.Resource, &f.grant)
	code, verifier := oauthTestCode(t, ctx, f.repository, f.client, f.actor.SubjectID, "https://client.example/callback", []string{OAuthMCPScope}, time.Now().Add(time.Minute))
	issue, _, refresh := oauthTestTokenIssue(time.Now())
	metadata, err := f.repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: f.client.ID, RedirectURI: "https://client.example/callback", CodeVerifier: verifier, Issue: issue}, time.Now())
	// Token exchange must atomically create both OAuth and executable projections.
	if err != nil {
		t.Fatal(err)
	}
	return metadata, issue, refresh
}

// TestOAuthMCPPostgresLifecycle proves scope isolation, immutable refresh, and family-wide revocation.
func TestOAuthMCPPostgresLifecycle(t *testing.T) {
	f := newOAuthMCPFixture(t)
	metadata, issue, refresh := f.issue(t)
	assertOAuthMCPProjection(t, f, metadata, issue)
	// Runtime OAuth must never authenticate the management API.
	if _, err := f.repository.LoadControlPrincipal(f.ctx, issue.AccessTokenHash); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("management access: %v", err)
	}
	next, _, _ := oauthTestTokenIssue(time.Now())
	request := OAuthRefreshExchange{ClientID: f.client.ID, RefreshTokenHash: accesscontrol.HashControlCredential(refresh), Issue: next}
	// Another resource cannot consume the refresh token.
	if _, err := f.repository.RotateOAuthRefreshToken(ContextWithOAuthMCP(f.ctx, "https://engine.example/mcp/"+uuid.NewString(), nil), request, time.Now()); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("wrong audience: %v", err)
	}
	rotated, err := f.repository.RotateOAuthRefreshToken(ContextWithOAuthMCP(f.ctx, f.grant.Resource, nil), request, time.Now())
	// Refresh must preserve the original grant identity.
	if err != nil || rotated.TokenFamilyID != metadata.TokenFamilyID {
		t.Fatalf("refresh: %+v %v", rotated, err)
	}
	projection, err := f.repository.AuthorizeApp(f.ctx, f.grant.AppID, next.AccessTokenHash)
	// Refresh must preserve the exact action ceiling.
	if err != nil || projection.TokenPolicy.AllowsOperation("deleteMessages") {
		t.Fatalf("refreshed policy: %+v %v", projection, err)
	}
	// Reusing the consumed refresh token must persist revocation despite returning a protocol error.
	if _, err := f.repository.RotateOAuthRefreshToken(ContextWithOAuthMCP(f.ctx, f.grant.Resource, nil), request, time.Now()); !errors.Is(err, ErrOAuthGrantReuseDetected) {
		t.Fatalf("refresh replay: %v", err)
	}
	// Revocation and permission removal must take effect on the next authorization.
	if _, err := f.repository.AuthorizeApp(f.ctx, f.grant.AppID, next.AccessTokenHash); err == nil {
		t.Fatal("replayed family still executes")
	}
}

// assertOAuthMCPProjection checks the exact approved policy before exercising rotation and revocation.
func assertOAuthMCPProjection(t *testing.T, f oauthMCPFixture, metadata OAuthTokenMetadata, issue OAuthTokenIssue) {
	t.Helper()
	projection, err := f.repository.AuthorizeApp(f.ctx, f.grant.AppID, issue.AccessTokenHash)
	// Only the approved action is executable, and even static-only grants reject dynamic connected selectors.
	if err != nil || projection.TokenPolicy.AllowAll || !projection.TokenPolicy.AllowsOperation("readMessages") || projection.TokenPolicy.AllowsOperation("deleteMessages") || projection.BindingMode != AppTokenBindingFixed || projection.OAuthGrantID != metadata.TokenFamilyID {
		t.Fatalf("unexpected runtime projection: %+v, %v", projection, err)
	}
}

// TestOAuthMCPPostgresCodeAudienceAndPermissions proves failed exchange is non-consuming and live RBAC remains authoritative.
func TestOAuthMCPPostgresCodeAudienceAndPermissions(t *testing.T) {
	f := newOAuthMCPFixture(t)
	ctx := ContextWithOAuthMCP(f.ctx, f.grant.Resource, &f.grant)
	code, verifier := oauthTestCode(t, ctx, f.repository, f.client, f.actor.SubjectID, "https://client.example/callback", []string{OAuthMCPScope}, time.Now().Add(time.Minute))
	issue, _, _ := oauthTestTokenIssue(time.Now())
	request := OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: f.client.ID, RedirectURI: "https://client.example/callback", CodeVerifier: verifier, Issue: issue}
	// The management issuer lacks a resource and must not consume an MCP code.
	if _, err := f.repository.ExchangeOAuthAuthorizationCode(f.ctx, request, time.Now()); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("unbound exchange: %v", err)
	}
	// A rejected audience must leave the original code available to its legitimate caller.
	if _, err := f.repository.ExchangeOAuthAuthorizationCode(ctx, request, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A successfully exchanged code must remain single-use.
	if _, err := f.repository.ExchangeOAuthAuthorizationCode(ctx, request, time.Now()); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("code replay: %v", err)
	}
	// Removing the user's delegation rights invalidates the token immediately without waiting for expiry.
	_, err := f.pool.Exec(f.ctx, `DELETE FROM fused_role_bindings WHERE subject_type='subject' AND subject_id=$1`, f.actor.SubjectID)
	// Fixture persistence or grant mutation must succeed before authority can be asserted.
	if err != nil {
		t.Fatal(err)
	}
	// Revocation and permission removal must take effect on the next authorization.
	if _, err := f.repository.AuthorizeApp(f.ctx, f.grant.AppID, issue.AccessTokenHash); err == nil {
		t.Fatal("removed user's rights still execute")
	}
}

// TestOAuthMCPPostgresFixedRefresh preserves an exact connected account after the original execution token expires.
func TestOAuthMCPPostgresFixedRefresh(t *testing.T) {
	f := newOAuthMCPFixture(t)
	bindingsFixture := appTokenPolicyFixture{ctx: f.ctx, pool: f.pool, repository: f.repository, familyID: f.grant.AppFamilyID, appID: f.grant.AppID}
	target := bindingsFixture.seedFixedBindingTarget(t)
	f.grant.Connections = []OAuthMCPConnectionBinding{{ServiceID: target.serviceA, AuthName: "gmail", ConnectionID: target.connectionA}, {ServiceID: target.serviceB, AuthName: "drive", ConnectionID: target.connectionB, ResourceID: &target.resourceB}}
	metadata, _, refresh := f.issue(t)
	// Expiry cleanup may remove the old executable token before a client performs its refresh.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM fused_app_tokens WHERE id=$1`, metadata.ID); err != nil {
		t.Fatal(err)
	}
	next, _, _ := oauthTestTokenIssue(time.Now())
	rotated, err := f.repository.RotateOAuthRefreshToken(ContextWithOAuthMCP(f.ctx, f.grant.Resource, nil), OAuthRefreshExchange{ClientID: f.client.ID, RefreshTokenHash: accesscontrol.HashControlCredential(refresh), Issue: next}, time.Now())
	// Fixture persistence or grant mutation must succeed before authority can be asserted.
	if err != nil {
		t.Fatal(err)
	}
	bindingsFixture.assertResolvedBinding(t, rotated.ID, target.serviceA, "gmail", target.connectionA, nil)
	bindingsFixture.assertResolvedBinding(t, rotated.ID, target.serviceB, "drive", target.connectionB, &target.resourceB)
	// Disconnecting the client cuts off the refreshed execution credential.
	if _, err := f.repository.RevokeOAuthConnectedApp(f.ctx, f.client.ID, f.actor.SubjectID, f.actor); err != nil {
		t.Fatal(err)
	}
	// Revocation and permission removal must take effect on the next authorization.
	if _, err := f.repository.AuthorizeApp(f.ctx, f.grant.AppID, next.AccessTokenHash); err == nil {
		t.Fatal("disconnected client still executes")
	}
}
