package store

import (
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// TestOAuthDeploymentPostgresLifecycle verifies management isolation, family ceilings, rotation and release rollback credentials.
func TestOAuthDeploymentPostgresLifecycle(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, _ := bootstrapUserTest(t, ctx, repository, "deployment-oauth")
	family, app, bucket := uuid.New(), uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,delivery_mode,target_language,canonical_name,display_name,owner_subject_id) SELECT $1::uuid,account_id,'sdk','api','typescript','deployment-test','Deployment test',$2 FROM fused_workspaces WHERE singleton_key=1`, family, owner.SubjectID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status) SELECT $1::uuid,$2,account_id,'1.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, app, family)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_buckets(id,name)VALUES($1,'deployment-default')`, bucket)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_app_family_buckets(app_family_id,bucket_id)VALUES($1,$2)`, family, bucket)
	require.NoError(t, err)
	raw := []byte(`{"schema_version":1,"name":"deployment-test","version":"1.0.0","services":{"crm":{"version":"v1","operations":["list"]}}}`)
	expiry := time.Now().Add(time.Hour)
	registered, err := repository.RegisterOAuthClient(ContextWithRuntimeManifest(ctx, raw), OAuthClientRegistration{Name: "Deployment", ClientType: OAuthClientConfidential, ClientID: "foc_" + uuid.NewString(), ClientSecretHash: accesscontrol.HashControlCredential("secret"), RedirectURIs: []string{"http://127.0.0.1:1234/callback"}, AllowedScopes: []string{OAuthDeploymentScope}, ExpiresAt: &expiry})
	require.NoError(t, err)
	grant := OAuthRuntimeGrant{AppID: app, AppFamilyID: family, ManifestDigest: RuntimeManifestDigest(raw), DefaultBucketID: bucket, Buckets: []OAuthRuntimeBucket{{ID: bucket, Name: "deployment-default"}}}
	code, verifier := oauthTestCode(t, ContextWithRuntimeConsent(ctx, grant), repository, registered.Client, owner.SubjectID, registered.Client.RedirectURIs[0], []string{OAuthDeploymentScope}, time.Now().Add(time.Minute))
	issue, _, _ := oauthTestTokenIssue(time.Now())
	exchange := &RuntimeExchange{TokenHash: accesscontrol.HashControlCredential("frt_first-release")}
	_, err = repository.ExchangeOAuthAuthorizationCode(ContextWithRuntimeExchange(ctx, exchange), OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: registered.Client.ID, RedirectURI: registered.Client.RedirectURIs[0], CodeVerifier: verifier, Issue: issue}, time.Now())
	require.NoError(t, err)
	// Neither credential may authenticate the generic management surface.
	_, err = repository.LoadControlPrincipal(ctx, issue.AccessTokenHash)
	require.Error(t, err)
	_, err = repository.LoadControlPrincipal(ctx, exchange.TokenHash)
	require.Error(t, err)
	actor, loaded, manifest, err := repository.LoadOAuthDeployment(ctx, issue.AccessTokenHash)
	require.NoError(t, err)
	require.Equal(t, grant, loaded)
	require.Equal(t, raw, manifest)
	authorizer := accesscontrol.SnapshotAuthorizer{}
	require.NoError(t, authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppAPIManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: family}}))
	require.Error(t, authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppAPIManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: uuid.New()}}))
	require.Error(t, authorizer.CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppAPICreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}))
	// Refresh-token possession alone never authenticates the deploy endpoint.
	_, _, _, err = repository.LoadOAuthDeployment(ctx, issue.RefreshTokenHash)
	require.Error(t, err)
	next := grant
	next.AppID = uuid.New()
	next.ManifestDigest = "sha256:new-release"
	_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status) SELECT $1::uuid,$2,account_id,'2.0.0',$1::uuid::text,'test','active' FROM fused_workspaces WHERE singleton_key=1`, next.AppID, family)
	require.NoError(t, err)
	nextHash := accesscontrol.HashControlCredential("frt_next-release")
	wrong := next
	wrong.AppFamilyID = uuid.New()
	require.Error(t, repository.IssueOAuthDeploymentToken(ctx, issue.AccessTokenHash, wrong, nextHash))
	wrong = next
	wrong.DefaultBucketID = uuid.New()
	require.Error(t, repository.IssueOAuthDeploymentToken(ctx, issue.AccessTokenHash, wrong, nextHash))
	require.NoError(t, repository.IssueOAuthDeploymentToken(ctx, issue.AccessTokenHash, next, nextHash))
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, next.AppID, nextHash)
	require.NoError(t, err)
	_, err = repository.AuthorizeApp(ctx, next.AppID, exchange.TokenHash)
	require.Error(t, err)
	// Rotation carries the original ceiling rather than widening it to the last published version.
	rotated, _, _ := oauthTestTokenIssue(time.Now())
	_, err = repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{ClientID: registered.Client.ID, RefreshTokenHash: issue.RefreshTokenHash, Issue: rotated}, time.Now())
	require.NoError(t, err)
	_, loaded, _, err = repository.LoadOAuthDeployment(ctx, rotated.AccessTokenHash)
	require.NoError(t, err)
	require.Equal(t, grant, loaded)
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.NoError(t, err)
	// Live owner deactivation disables both management and execution without waiting for expiry.
	_, err = pool.Exec(ctx, `UPDATE fused_subjects SET status='suspended' WHERE id=$1`, owner.SubjectID)
	require.NoError(t, err)
	_, _, _, err = repository.LoadOAuthDeployment(ctx, rotated.AccessTokenHash)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_subjects SET status='active' WHERE id=$1`, owner.SubjectID)
	require.NoError(t, err)
	// RFC7009 disconnect identifies this client's grant by refresh token and revokes every release.
	require.NoError(t, repository.RevokeOAuthRuntimeToken(ctx, registered.Client.ClientID, rotated.RefreshTokenHash))
	_, _, _, err = repository.LoadOAuthDeployment(ctx, rotated.AccessTokenHash)
	require.Error(t, err)
	_, err = repository.AuthorizeApp(ctx, app, exchange.TokenHash)
	require.Error(t, err)
	_, err = repository.AuthorizeApp(ctx, next.AppID, nextHash)
	require.Error(t, err)
}
