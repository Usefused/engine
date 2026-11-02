package store

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRuntimeManifestRegistrationBoundary prevents unbound or mixed-scope provisioning clients.
func TestRuntimeManifestRegistrationBoundary(t *testing.T) {
	raw := []byte(`{"schema_version":1}`)
	_, err := runtimeRegistrationManifest(t.Context(), []string{OAuthRuntimeScope})
	require.Error(t, err)
	_, err = runtimeRegistrationManifest(ContextWithRuntimeManifest(t.Context(), raw), []string{"service.read"})
	require.Error(t, err)
	result, err := runtimeRegistrationManifest(ContextWithRuntimeManifest(t.Context(), raw), []string{OAuthRuntimeScope})
	require.NoError(t, err)
	require.Equal(t, raw, result)
}

// TestOAuthRuntimePostgresLifecycle proves byte binding, atomic exchange, version pinning, and irreversible disconnect.
func TestOAuthRuntimePostgresLifecycle(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "runtime-oauth")
	family, app := uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,delivery_mode,target_language,canonical_name,display_name,owner_subject_id) SELECT $1::uuid,account_id,'sdk','api','typescript',$1::uuid::text,'Runtime test',$2 FROM fused_workspaces WHERE singleton_key=1`, family, owner.SubjectID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status) SELECT $1::uuid,$2,account_id,'1.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, app, family)
	require.NoError(t, err)
	raw := []byte(`{"schema_version":1,"name":"runtime","version":"1.0.0","services":{"crm":{"version":"v1","operations":["list"]}}}`)
	expiry := time.Now().Add(30 * time.Minute)
	registered, err := repository.RegisterOAuthClient(ContextWithRuntimeManifest(ctx, raw), OAuthClientRegistration{Name: "Runtime test", ClientType: OAuthClientConfidential, ClientID: "foc_" + uuid.NewString(), ClientSecretHash: accesscontrol.HashControlCredential("secret"), RedirectURIs: []string{"http://127.0.0.1:1234/callback"}, AllowedScopes: []string{OAuthRuntimeScope}, ExpiresAt: &expiry})
	require.NoError(t, err)
	stored, err := repository.OAuthRuntimeManifest(ctx, registered.Client.ClientID)
	require.NoError(t, err)
	require.Equal(t, raw, stored)
	bucket := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, bucket, "runtime-default")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_app_family_buckets(app_family_id,bucket_id) VALUES($1,$2)`, family, bucket)
	require.NoError(t, err)
	grant := OAuthRuntimeGrant{AppID: app, AppFamilyID: family, ManifestDigest: RuntimeManifestDigest(raw), DefaultBucketID: bucket, Buckets: []OAuthRuntimeBucket{{ID: bucket, Name: "runtime-default"}}}
	code, verifier := oauthTestCode(t, ContextWithRuntimeConsent(ctx, grant), repository, registered.Client, owner.SubjectID, registered.Client.RedirectURIs[0], []string{OAuthRuntimeScope}, time.Now().Add(time.Minute))
	client, _, err := repository.GetOAuthClientByPublicID(ctx, registered.Client.ClientID)
	require.NoError(t, err)
	require.Nil(t, client.ExpiresAt)
	issue, _, _ := oauthTestTokenIssue(time.Now())
	request := OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: verifier, Issue: issue}
	// The management issuer cannot consume the code or turn it into a broad OAuth credential.
	_, err = repository.ExchangeOAuthAuthorizationCode(ctx, request, time.Now())
	require.ErrorIs(t, err, ErrOAuthGrantDenied)
	exchange := &RuntimeExchange{TokenHash: accesscontrol.HashControlCredential("frt_runtime-fixture")}
	metadata, err := repository.ExchangeOAuthAuthorizationCode(ContextWithRuntimeExchange(ctx, exchange), request, time.Now())
	require.NoError(t, err)
	require.Equal(t, grant, exchange.Grant)
	projection, err := repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.NoError(t, err)
	require.Nil(t, projection.TokenPolicy.ExpiresAt)
	// Neither the independent runtime secret nor the discarded OAuth token can authenticate management.
	_, err = repository.LoadControlPrincipal(ctx, exchange.TokenHash)
	require.Error(t, err)
	_, err = repository.LoadControlPrincipal(ctx, issue.AccessTokenHash)
	require.Error(t, err)
	// The same family cannot use this credential for a later, broader immutable version.
	sibling := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status) SELECT $1::uuid,$2,account_id,'2.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, sibling, family)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, sibling, exchange.TokenHash)
	require.Error(t, err)
	// Family-wide bucket changes must not silently alter an older version's approved default.
	replacement := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, replacement, "replacement")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_app_family_buckets SET bucket_id=$2 WHERE app_family_id=$1`, family, replacement)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_app_family_buckets SET bucket_id=$2 WHERE app_family_id=$1`, family, bucket)
	require.NoError(t, err)
	// Code replay cannot create another runtime credential.
	_, err = repository.ExchangeOAuthAuthorizationCode(ContextWithRuntimeExchange(ctx, &RuntimeExchange{TokenHash: accesscontrol.HashControlCredential("frt_replay")}), request, time.Now())
	require.ErrorIs(t, err, ErrOAuthGrantDenied)
	// OAuth access expiry does not terminate the separately approved runtime credential.
	_, err = pool.Exec(ctx, `UPDATE fused_oauth_tokens SET access_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, metadata.ID)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.NoError(t, err)
	_, err = repository.RevokeOAuthConnectedApp(ctx, client.ID, owner.SubjectID, actor)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.Error(t, err)
	// A fresh consent does not revive the previous credential.
	nextCode, nextVerifier := oauthTestCode(t, ContextWithRuntimeConsent(ctx, grant), repository, client, owner.SubjectID, client.RedirectURIs[0], []string{OAuthRuntimeScope}, time.Now().Add(time.Minute))
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.Error(t, err)
	nextIssue, _, _ := oauthTestTokenIssue(time.Now())
	nextExchange := &RuntimeExchange{TokenHash: accesscontrol.HashControlCredential("frt_reconnected")}
	_, err = repository.ExchangeOAuthAuthorizationCode(ContextWithRuntimeExchange(ctx, nextExchange), OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(nextCode), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: nextVerifier, Issue: nextIssue}, time.Now())
	require.NoError(t, err)
	// Presenting the raw credential through a different client cannot revoke its owner.
	require.NoError(t, repository.RevokeOAuthRuntimeToken(ctx, "foc_other", nextExchange.TokenHash))
	_, err = repository.AuthorizeApp(ctx, app, nextExchange.TokenHash)
	require.NoError(t, err)
	// A pending code shares the same consent and must be retired on controller disconnect.
	pendingCode, pendingVerifier := oauthTestCode(t, ContextWithRuntimeConsent(ctx, grant), repository, client, owner.SubjectID, client.RedirectURIs[0], []string{OAuthRuntimeScope}, time.Now().Add(time.Minute))
	require.NoError(t, repository.RevokeOAuthRuntimeToken(ctx, client.ClientID, nextExchange.TokenHash))
	var consents int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_oauth_user_consents WHERE client_id=$1 AND subject_id=$2`, client.ID, owner.SubjectID).Scan(&consents))
	require.Zero(t, consents, "controller disconnect releases the dynamic-client consent quota slot")
	pendingIssue, _, _ := oauthTestTokenIssue(time.Now())
	_, err = repository.ExchangeOAuthAuthorizationCode(ContextWithRuntimeExchange(ctx, &RuntimeExchange{TokenHash: accesscontrol.HashControlCredential("frt_disconnected-code")}), OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(pendingCode), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: pendingVerifier, Issue: pendingIssue}, time.Now())
	require.ErrorIs(t, err, ErrOAuthGrantDenied)
	require.NoError(t, repository.RevokeOAuthRuntimeToken(ctx, client.ClientID, nextExchange.TokenHash))
	_, err = repository.AuthorizeApp(ctx, app, nextExchange.TokenHash)
	require.Error(t, err)
}
