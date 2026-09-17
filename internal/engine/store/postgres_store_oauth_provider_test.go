package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// oauthTestClient registers a fresh confidential OAuth client for tests,
// returning the sanitized client projection alongside the raw (unhashed)
// secret the caller will need to authenticate as that client.
func oauthTestClient(t *testing.T, ctx context.Context, repository *postgresStore, actor MutationActor, redirectURI string, scopes []string) (OAuthClient, string) {
	t.Helper()
	rawSecret := "raw-secret-" + uuid.NewString()
	result, err := repository.CreateOAuthClient(ctx, OAuthClientRegistration{
		Name: "Test Client " + uuid.NewString(), ClientType: OAuthClientConfidential,
		ClientID: "foc_" + uuid.NewString(), ClientSecretHash: accesscontrol.HashControlCredential(rawSecret),
		RedirectURIs: []string{redirectURI}, AllowedScopes: scopes, Actor: actor,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	return result.Client, rawSecret
}

// oauthTestCode records consent and issues a single-use authorization code
// for subjectID against client, returning the raw code and the PKCE verifier
// a legitimate token exchange must present.
func oauthTestCode(t *testing.T, ctx context.Context, repository *postgresStore, client OAuthClient, subjectID uuid.UUID, redirectURI string, scope []string, expiresAt time.Time) (rawCode, verifier string) {
	t.Helper()
	verifier = strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	rawCode = "raw-code-" + uuid.NewString()
	err := repository.RecordOAuthConsentAndIssueCode(ctx,
		OAuthConsentGrant{ClientID: client.ID, SubjectID: subjectID, GrantedScope: scope},
		OAuthAuthorizationCodeIssue{
			ID: uuid.New(), ClientID: client.ID, SubjectID: subjectID, RedirectURI: redirectURI, Scope: scope,
			CodeHash: accesscontrol.HashControlCredential(rawCode), CodeChallenge: challenge, ExpiresAt: expiresAt,
		})
	if err != nil {
		t.Fatalf("RecordOAuthConsentAndIssueCode: %v", err)
	}
	return rawCode, verifier
}

// oauthTestTokenIssue builds a fresh, uniquely hashed access/refresh token
// pair suitable for OAuthCodeExchange.Issue / OAuthRefreshExchange.Issue.
func oauthTestTokenIssue(now time.Time) (issue OAuthTokenIssue, rawAccess, rawRefresh string) {
	rawAccess = "raw-access-" + uuid.NewString()
	rawRefresh = "raw-refresh-" + uuid.NewString()
	return OAuthTokenIssue{
		AccessTokenHash: accesscontrol.HashControlCredential(rawAccess), RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh),
		AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(30 * 24 * time.Hour),
	}, rawAccess, rawRefresh
}

func TestPostgresOAuthClientLifecycleCascadesTokenRevocation(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-client-lifecycle")
	redirectURI := "https://app.example.com/callback"

	client, rawSecret := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	if !client.HasSecret {
		t.Fatal("confidential client registration did not record a secret")
	}

	fetched, secretHash, err := repository.GetOAuthClientByPublicID(ctx, client.ClientID)
	if err != nil || fetched.ID != client.ID || secretHash != accesscontrol.HashControlCredential(rawSecret) {
		t.Fatalf("GetOAuthClientByPublicID = %#v, %q, %v", fetched, secretHash, err)
	}

	now := time.Now().UTC()
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, _ := oauthTestTokenIssue(now)
	metadata, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now.Add(time.Second))
	if err != nil || metadata.ClientID != client.ID {
		t.Fatalf("ExchangeOAuthAuthorizationCode = %#v, %v", metadata, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess)); err != nil {
		t.Fatalf("issued access token does not yet authenticate: %v", err)
	}

	// Revoking the client must immediately cut off every token it ever
	// issued, not merely future authorize/token requests.
	revision, err := repository.RevokeOAuthClient(ctx, client.ID, actor)
	if err != nil || revision <= 0 {
		t.Fatalf("RevokeOAuthClient = %d, %v", revision, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked client's token still authenticates: %v", err)
	}
	if _, err := repository.RevokeOAuthClient(ctx, client.ID, actor); !errors.Is(err, ErrOAuthClientNotFound) {
		t.Fatalf("double revoke error = %v, want ErrOAuthClientNotFound", err)
	}
}

func TestPostgresOAuthAuthorizationCodeRejectsReplayAndTampering(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-code-attacks")
	redirectURI := "https://app.example.com/callback"
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()

	// Wrong redirect_uri and wrong PKCE verifier must both be denied without
	// consuming the code, so a legitimate retry with correct parameters still
	// succeeds afterward.
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, _, _ := oauthTestTokenIssue(now)
	codeHash := accesscontrol.HashControlCredential(rawCode)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: "https://attacker.example.com/callback", CodeVerifier: verifier, Issue: issue,
	}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("wrong redirect_uri error = %v, want ErrOAuthGrantDenied", err)
	}
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: strings.Repeat("w", 43), Issue: issue,
	}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("wrong PKCE verifier error = %v, want ErrOAuthGrantDenied", err)
	}
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("legitimate exchange after failed attempts: %v", err)
	}
	// The code is single-use: a second legitimate-looking exchange must fail
	// even though every parameter matches.
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("replayed code error = %v, want ErrOAuthGrantDenied", err)
	}

	expiredCode, expiredVerifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(-time.Second))
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(expiredCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: expiredVerifier, Issue: issue,
	}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("expired code error = %v, want ErrOAuthGrantDenied", err)
	}
}

func TestPostgresOAuthRefreshTokenRotationDetectsReuseAndRevokesFamily(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-refresh-reuse")
	redirectURI := "https://app.example.com/callback"
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()

	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	firstIssue, rawAccess1, rawRefresh1 := oauthTestTokenIssue(now)
	firstMetadata, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: firstIssue,
	}, now)
	if err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}

	secondIssue, rawAccess2, rawRefresh2 := oauthTestTokenIssue(now.Add(time.Minute))
	secondMetadata, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{
		RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh1), ClientID: client.ID, Issue: secondIssue,
	}, now.Add(time.Minute))
	if err != nil || secondMetadata.TokenFamilyID != firstMetadata.TokenFamilyID {
		t.Fatalf("RotateOAuthRefreshToken = %#v, %v, want family %s", secondMetadata, err, firstMetadata.TokenFamilyID)
	}
	// The rotated access token authenticates; the pre-rotation one is still
	// within its own natural expiry and untouched by an honest rotation.
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess2)); err != nil {
		t.Fatalf("rotated access token does not authenticate: %v", err)
	}

	// Presenting the already-consumed refresh token again is a replay signal
	// (stolen token or buggy client either way): it must deny the grant AND
	// cascade-revoke every token in the family, including the one just
	// rotated to moments ago.
	if _, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{
		RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh1), ClientID: client.ID, Issue: secondIssue,
	}, now.Add(2*time.Minute)); !errors.Is(err, ErrOAuthGrantReuseDetected) {
		t.Fatalf("refresh token reuse error = %v, want ErrOAuthGrantReuseDetected", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess1)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("pre-rotation access token still authenticates after reuse detection: %v", err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess2)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("rotated access token still authenticates after reuse detection: %v", err)
	}
	if _, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{
		RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh2), ClientID: client.ID, Issue: secondIssue,
	}, now.Add(3*time.Minute)); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("rotating the newer refresh token after family revocation error = %v, want ErrOAuthGrantDenied", err)
	}
}

func TestPostgresOAuthLoadControlPrincipalEnforcesTokenScopeCeiling(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-scope-ceiling")
	redirectURI := "https://app.example.com/callback"
	// The owner subject holds every permission workspace-wide, so this test
	// isolates the token-scope intersection: only "service.read" was ever
	// consented to, and the resolved principal must reflect exactly that,
	// not the owner's full permission set.
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, _ := oauthTestTokenIssue(now)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}

	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess))
	if err != nil {
		t.Fatalf("LoadControlPrincipal: %v", err)
	}
	if principal.CredentialSource != "oauth_client" || principal.SubjectID != owner.SubjectID {
		t.Fatalf("principal = %#v", principal)
	}
	sawServiceRead, sawOutOfScope := false, false
	for _, grant := range principal.EffectiveGrants {
		switch grant.Permission {
		case accesscontrol.PermissionServiceRead:
			sawServiceRead = true
		case accesscontrol.PermissionServiceManage, accesscontrol.PermissionAccountManage:
			sawOutOfScope = true
		}
	}
	if !sawServiceRead {
		t.Fatal("delegated token principal is missing the consented service.read grant")
	}
	if sawOutOfScope {
		t.Fatalf("delegated token principal leaked permissions beyond its consented scope: %#v", principal.EffectiveGrants)
	}
}

func TestPostgresOAuthConnectedAppRevocationRemovesConsentAndTokens(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-connected-apps")
	redirectURI := "https://app.example.com/callback"
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, _ := oauthTestTokenIssue(now)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}

	apps, err := repository.ListOAuthConnectedApps(ctx, owner.SubjectID)
	if err != nil || len(apps) != 1 || apps[0].ClientID != client.ID {
		t.Fatalf("ListOAuthConnectedApps = %#v, %v", apps, err)
	}

	revision, err := repository.RevokeOAuthConnectedApp(ctx, client.ID, owner.SubjectID, actor)
	if err != nil || revision <= 0 {
		t.Fatalf("RevokeOAuthConnectedApp = %d, %v", revision, err)
	}
	if _, found, err := repository.GetOAuthUserConsent(ctx, client.ID, owner.SubjectID); err != nil || found {
		t.Fatalf("consent still present after disconnect: found=%v, err=%v", found, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("token still authenticates after disconnect: %v", err)
	}
	if _, err := repository.RevokeOAuthConnectedApp(ctx, client.ID, owner.SubjectID, actor); !errors.Is(err, ErrOAuthConsentNotFound) {
		t.Fatalf("double disconnect error = %v, want ErrOAuthConsentNotFound", err)
	}
}

func TestPostgresOAuthRevokeTokenByHashIsIdempotentAndClientScoped(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-revoke-rfc7009")
	redirectURI := "https://app.example.com/callback"
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	other, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, _ := oauthTestTokenIssue(now)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}
	tokenHash := accesscontrol.HashControlCredential(rawAccess)

	// A different client presenting someone else's token hash must not be
	// able to revoke it (RFC 7009 scopes revocation to the presenting
	// client), and the outcome must look identical to "not found".
	if revoked, err := repository.RevokeOAuthTokenByHash(ctx, other.ID, tokenHash); err != nil || revoked {
		t.Fatalf("cross-client revoke = %v, %v, want false/nil", revoked, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, tokenHash); err != nil {
		t.Fatalf("token was revoked by the wrong client: %v", err)
	}

	if revoked, err := repository.RevokeOAuthTokenByHash(ctx, client.ID, tokenHash); err != nil || !revoked {
		t.Fatalf("RevokeOAuthTokenByHash = %v, %v, want true/nil", revoked, err)
	}
	if _, err := repository.LoadControlPrincipal(ctx, tokenHash); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
	// RFC 7009: revoking an already-revoked/unknown token must still report
	// success-shaped output (false, nil) rather than an error, so a client
	// cannot distinguish "already gone" from "never existed".
	if revoked, err := repository.RevokeOAuthTokenByHash(ctx, client.ID, tokenHash); err != nil || revoked {
		t.Fatalf("second revoke = %v, %v, want false/nil", revoked, err)
	}
}

func TestPostgresOAuthExpireArtifactsPurgesExpiredCodesAndTokens(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-expire-artifacts")
	redirectURI := "https://app.example.com/callback"
	client, _ := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()

	// An expired, never-exchanged authorization code.
	oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(-time.Hour))
	// A token pair whose refresh lifetime already elapsed.
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, _, rawRefresh := oauthTestTokenIssue(now)
	issue.RefreshExpiresAt = now.Add(-time.Minute)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}

	purged, err := repository.ExpireOAuthArtifacts(ctx, now, 100)
	if err != nil || purged < 2 {
		t.Fatalf("ExpireOAuthArtifacts = %d, %v, want >= 2", purged, err)
	}
	// The purged refresh token must no longer be rotatable.
	if _, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{
		RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh), ClientID: client.ID, Issue: issue,
	}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("rotate after purge error = %v, want ErrOAuthGrantDenied", err)
	}
}

func TestPostgresOAuthAuditNeverLeaksSecretMaterial(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-audit-leak")
	redirectURI := "https://app.example.com/callback"
	client, rawSecret := oauthTestClient(t, ctx, repository, actor, redirectURI, []string{"service.read"})
	now := time.Now().UTC()
	rawCode, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, redirectURI, []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, rawRefresh := oauthTestTokenIssue(now)
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{
		CodeHash: accesscontrol.HashControlCredential(rawCode), ClientID: client.ID, RedirectURI: redirectURI, CodeVerifier: verifier, Issue: issue,
	}, now); err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT metadata::text FROM fused_audit_events
		WHERE action IN ('oauth.client.create', 'oauth.token.issue', 'oauth.client.revoke')
	`)
	if err != nil {
		t.Fatalf("query OAuth audit events: %v", err)
	}
	defer rows.Close()
	secrets := []string{
		rawSecret, rawCode, rawAccess, rawRefresh, verifier,
		accesscontrol.HashControlCredential(rawSecret), accesscontrol.HashControlCredential(rawCode),
		accesscontrol.HashControlCredential(rawAccess), accesscontrol.HashControlCredential(rawRefresh),
	}
	checked := 0
	for rows.Next() {
		var metadata string
		if err := rows.Scan(&metadata); err != nil {
			t.Fatalf("scan OAuth audit metadata: %v", err)
		}
		checked++
		for _, secret := range secrets {
			if strings.Contains(metadata, secret) {
				t.Fatalf("OAuth audit metadata leaked secret material: %s", metadata)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no OAuth audit events were recorded to check")
	}
}
