package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// dynamicOAuthTestClient registers an anonymous client with a deterministic lifetime bound.
func dynamicOAuthTestClient(t *testing.T, ctx context.Context, repository *postgresStore) OAuthClient {
	t.Helper()
	expiry := time.Now().UTC().Add(OAuthDynamicClientMaxTTL).Truncate(time.Microsecond)
	result, err := repository.RegisterOAuthClient(ctx, OAuthClientRegistration{
		Name: "Temporary client", ClientType: OAuthClientConfidential, ClientID: "foc_" + uuid.NewString(),
		ClientSecretHash: accesscontrol.HashControlCredential(uuid.NewString()),
		RedirectURIs:     []string{"http://localhost:4321/callback"}, AllowedScopes: []string{"service.read"}, ExpiresAt: &expiry,
	})
	// A fixture must use the same anonymous issuance boundary as a real client.
	if err != nil {
		t.Fatalf("register dynamic client: %v", err)
	}
	return result.Client
}

// grantDynamicOAuthTestClient creates consent and a code without bypassing quota enforcement.
func grantDynamicOAuthTestClient(ctx context.Context, repository *postgresStore, clientID, subjectID uuid.UUID) error {
	return repository.RecordOAuthConsentAndIssueCode(ctx,
		OAuthConsentGrant{ClientID: clientID, SubjectID: subjectID, GrantedScope: []string{"service.read"}},
		OAuthAuthorizationCodeIssue{ID: uuid.New(), ClientID: clientID, SubjectID: subjectID,
			RedirectURI: "http://localhost:4321/callback", Scope: []string{"service.read"},
			CodeHash: accesscontrol.HashControlCredential(uuid.NewString()), CodeChallenge: strings.Repeat("a", 43), ExpiresAt: time.Now().Add(time.Minute)},
	)
}

// TestPostgresOAuthDynamicClientQuota verifies concurrent grants, user isolation, and capacity recovery.
func TestPostgresOAuthDynamicClientQuota(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, actor := bootstrapUserTest(t, ctx, repository, "oauth-dynamic-quota")
	clients := make([]OAuthClient, MaxActiveOAuthDynamicClientsPerUser+2)
	// Register more anonymous clients than any one user can authorize.
	for index := range clients {
		clients[index] = dynamicOAuthTestClient(t, ctx, repository)
	}
	type outcome struct {
		client OAuthClient
		err    error
	}
	results := make(chan outcome, len(clients))
	start := make(chan struct{})
	// Race distinct clients for the same user quota.
	for _, client := range clients {
		// Release all approvals together to exercise the database serialization boundary.
		go func(client OAuthClient) {
			<-start
			results <- outcome{client, grantDynamicOAuthTestClient(ctx, repository, client.ID, owner.SubjectID)}
		}(client)
	}
	close(start)
	var accepted, rejected []OAuthClient
	// Drain every request before examining the committed result.
	for range clients {
		result := <-results
		// Exactly ten concurrent grants may commit; other requests must be policy denials.
		switch {
		case result.err == nil:
			accepted = append(accepted, result.client)
		case errors.Is(result.err, ErrOAuthDynamicClientLimit):
			rejected = append(rejected, result.client)
		default:
			t.Fatalf("concurrent grant: %v", result.err)
		}
	}
	// No interleaving may exceed or underfill the configured capacity.
	if len(accepted) != 10 || len(rejected) != 2 {
		t.Fatalf("accepted=%d rejected=%d", len(accepted), len(rejected))
	}
	// Re-consenting an existing client must not consume a second slot.
	if err := grantDynamicOAuthTestClient(ctx, repository, accepted[0].ID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	var deniedCodes int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_oauth_authorization_codes WHERE client_id=$1`, rejected[0].ID).Scan(&deniedCodes)
	// Quota rejection must roll back both consent and code issuance.
	if err != nil || deniedCodes != 0 {
		t.Fatalf("denied client codes=%d error=%v", deniedCodes, err)
	}
	// A failed approval must not reserve a slot through a leftover consent row.
	if _, found, err := repository.GetOAuthUserConsent(ctx, rejected[0].ID, owner.SubjectID); err != nil || found {
		t.Fatalf("rejected consent persisted: found=%v error=%v", found, err)
	}
	other := createActiveUserWithCredential(t, ctx, repository, actor, "other-oauth@example.com")
	// The quota belongs to each active user, not the whole Engine.
	if err := grantDynamicOAuthTestClient(ctx, repository, rejected[0].ID, other.UserID); err != nil {
		t.Fatal(err)
	}
	permanent, _ := oauthTestClient(t, ctx, repository, actor, "http://localhost:4321/callback", []string{"service.read"})
	// Admin-managed permanent integrations do not consume temporary-client capacity.
	if err := grantDynamicOAuthTestClient(ctx, repository, permanent.ID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	// Disconnect releases the user's slot without depending on the cleanup worker.
	if _, err := repository.RevokeOAuthConnectedApp(ctx, accepted[0].ID, owner.SubjectID, actor); err != nil {
		t.Fatal(err)
	}
	// A denied registration can be approved once a disconnect makes room.
	if err := grantDynamicOAuthTestClient(ctx, repository, rejected[0].ID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	// Natural expiry also frees capacity before cleanup has revoked the client.
	if _, err := pool.Exec(ctx, `UPDATE fused_oauth_clients SET expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, accepted[1].ID); err != nil {
		t.Fatal(err)
	}
	// Expired clients must no longer count toward the next approval.
	if err := grantDynamicOAuthTestClient(ctx, repository, rejected[1].ID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	replacement := dynamicOAuthTestClient(t, ctx, repository)
	// Revoking a client must make room even while its old consent row remains.
	if _, err := repository.RevokeOAuthClient(ctx, accepted[2].ID, actor); err != nil {
		t.Fatal(err)
	}
	// A revoked client must no longer count toward the next approval.
	if err := grantDynamicOAuthTestClient(ctx, repository, replacement.ID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	apps, err := repository.ListOAuthConnectedApps(ctx, owner.SubjectID)
	// The visible list contains ten dynamic clients plus the permanent integration.
	if err != nil || len(apps) != 11 {
		t.Fatalf("connected apps=%d error=%v", len(apps), err)
	}
}

// TestPostgresOAuthDynamicClientLifetime ensures neither fresh nor legacy tokens outlive their client.
func TestPostgresOAuthDynamicClientLifetime(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, owner, _ := bootstrapUserTest(t, ctx, repository, "oauth-dynamic-lifetime")
	client := dynamicOAuthTestClient(t, ctx, repository)
	now := time.Now().UTC()
	code, verifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, client.RedirectURIs[0], []string{"service.read"}, now.Add(time.Minute))
	issue, rawAccess, rawRefresh := oauthTestTokenIssue(now)
	grant, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(code), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: verifier, Issue: issue}, now)
	// Both persisted expiries must be clamped even though refresh normally lasts thirty days.
	if err != nil || !grant.AccessExpiresAt.Equal(*client.ExpiresAt) || !grant.RefreshExpiresAt.Equal(*client.ExpiresAt) {
		t.Fatalf("token deadline mismatch: %v", err)
	}
	rotatedIssue, _, rotatedRefresh := oauthTestTokenIssue(now.Add(time.Minute))
	rotated, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{RefreshTokenHash: accesscontrol.HashControlCredential(rawRefresh), ClientID: client.ID, Issue: rotatedIssue}, now.Add(time.Minute))
	// Rotation must retain the original absolute deadline instead of restarting an hour.
	if err != nil || !rotated.AccessExpiresAt.Equal(*client.ExpiresAt) || !rotated.RefreshExpiresAt.Equal(*client.ExpiresAt) {
		t.Fatalf("rotated deadline mismatch: %v", err)
	}
	// Simulate a token issued before deadline clamping was introduced.
	if _, err := pool.Exec(ctx, `UPDATE fused_oauth_tokens SET access_expires_at=NOW()+INTERVAL '2 hours' WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	principal, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess))
	// Authentication must give its cache the earlier client deadline for legacy tokens too.
	if err != nil || principal.ExpiresAt == nil || !principal.ExpiresAt.Equal(*client.ExpiresAt) {
		t.Fatalf("cached identity deadline: %v", err)
	}
	pendingCode, pendingVerifier := oauthTestCode(t, ctx, repository, client, owner.SubjectID, client.RedirectURIs[0], []string{"service.read"}, now.Add(time.Minute))
	// Expire only the client, leaving every token and code otherwise live.
	if _, err := pool.Exec(ctx, `UPDATE fused_oauth_clients SET expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, client.ID); err != nil {
		t.Fatal(err)
	}
	// An expired client cannot authenticate even before cleanup runs.
	if _, err := repository.LoadControlPrincipal(ctx, accesscontrol.HashControlCredential(rawAccess)); !errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		t.Fatalf("expired authentication: %v", err)
	}
	// The persistence boundary must also deny a code exchange racing expiry.
	if _, err := repository.ExchangeOAuthAuthorizationCode(ctx, OAuthCodeExchange{CodeHash: accesscontrol.HashControlCredential(pendingCode), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeVerifier: pendingVerifier, Issue: issue}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("expired code exchange: %v", err)
	}
	// A still-live refresh token cannot resurrect its expired client.
	if _, err := repository.RotateOAuthRefreshToken(ctx, OAuthRefreshExchange{RefreshTokenHash: accesscontrol.HashControlCredential(rotatedRefresh), ClientID: client.ID, Issue: issue}, now); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("expired refresh: %v", err)
	}
	// Pending consent cannot activate an expired registration either.
	if err := grantDynamicOAuthTestClient(ctx, repository, client.ID, owner.SubjectID); !errors.Is(err, ErrOAuthGrantDenied) {
		t.Fatalf("expired consent: %v", err)
	}
}

// TestPostgresOAuthDynamicRegistrationRequiresBoundedExpiry rejects permanent and overlong dynamic clients.
func TestPostgresOAuthDynamicRegistrationRequiresBoundedExpiry(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	_, _, _ = bootstrapUserTest(t, ctx, repository, "oauth-dynamic-expiry-validation")
	expired := time.Now().Add(-time.Minute)
	tooLong := time.Now().Add(2 * time.Hour)
	// Exercise each way an internal caller could request an unbounded lifetime.
	for _, expiry := range []*time.Time{nil, &expired, &tooLong} {
		_, err := repository.RegisterOAuthClient(ctx, OAuthClientRegistration{Name: "Invalid expiry", ClientType: OAuthClientConfidential, ClientID: uuid.NewString(), ClientSecretHash: "test-hash", RedirectURIs: []string{"http://localhost/callback"}, ExpiresAt: expiry})
		// Internal callers cannot bypass the public service's one-hour lifetime.
		if !errors.Is(err, ErrInvalidOAuthClient) {
			t.Fatalf("invalid expiry accepted: %v", err)
		}
	}
}
