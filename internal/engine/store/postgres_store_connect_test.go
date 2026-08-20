package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var connectAuthTestMasterKey = []byte("12345678901234567890123456789012")

func TestPostgresStore_BucketAttachedConnectAuth(t *testing.T) {
	fixture := setupConnectAuthStore(t)

	t.Run("bucket names resolve in one exact batch", func(t *testing.T) {
		testGetBucketsByNames(t, fixture)
	})

	t.Run("config is upserted only for a bucket in the workspace", func(t *testing.T) {
		testConnectConfigOwnership(t, fixture)
	})

	t.Run("connections are reusable by bucket and isolated across buckets", func(t *testing.T) {
		testAuthConnectionsReusableByBucket(t, fixture)
	})

	t.Run("connect sessions are single lookup records with cleanup", func(t *testing.T) {
		testConnectSessionLifecycle(t, fixture)
	})

	t.Run("connect input completion is one-time and atomic", func(t *testing.T) {
		testConnectInputSessionLifecycle(t, fixture)
	})

	t.Run("connection resources reconcile and select without broad reads", func(t *testing.T) {
		testConnectionResourceLifecycle(t, fixture)
	})

	t.Run("callback token and exact resource commit atomically", func(t *testing.T) {
		testCallbackConnectionAtomicity(t, fixture)
	})

	t.Run("workspace profile layers resolve precedence and stay version/operation scoped", func(t *testing.T) {
		testWorkspaceConnectionProfileLifecycle(t, fixture)
	})
}

// TestPostgresStoreAuthConnectionRefreshLeases exercises cross-replica claims,
// CAS completion, retry throttling, recovery, and exact legacy version binding.
func TestPostgresStoreAuthConnectionRefreshLeases(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-refresh", versionID, "Refresh Service", fixture.accountID); err != nil {
		t.Fatalf("activate refresh fixture version: %v", err)
	}
	refreshStore := fixture.store.(AuthConnectionRefreshStore)
	now := time.Now().UTC().Truncate(time.Microsecond)
	early := upsertRefreshLeaseConnection(t, fixture, "refresh-early", versionID, now.Add(-2*time.Minute))
	later := upsertRefreshLeaseConnection(t, fixture, "refresh-later", versionID, now.Add(-time.Minute))
	_ = upsertRefreshLeaseConnection(t, fixture, "refresh-future", versionID, now.Add(2*time.Hour))
	legacy := upsertRefreshLeaseConnection(t, fixture, "refresh-legacy", uuid.Nil, now.Add(-time.Minute))

	first, second, leaseUntil := claimRefreshLeasePages(t, refreshStore, now, early.ID, later.ID)
	assertRefreshLeaseContention(t, refreshStore, first, versionID, now, leaseUntil)
	recovered := recoverExpiredRefreshLease(t, refreshStore, first, leaseUntil)
	completeRecoveredRefresh(t, refreshStore, first, recovered, leaseUntil)
	releaseWorkerRefreshTransiently(t, refreshStore, second, now)
	assertRefreshPassDoesNotReclaimCompleted(t, refreshStore, early.ID, now, leaseUntil)
	assertForegroundRetryAndReconnect(t, fixture, refreshStore, versionID, now)
	assertLegacyRefreshVersionBinding(t, refreshStore, legacy.ID, versionID, now)
	// The second page remains a valid independently leased claim, proving the
	// page-one worker did not serialize every due connection behind one lock.
	if second.LeaseToken == uuid.Nil || second.Connection.ID != later.ID {
		t.Fatalf("second refresh claim = %#v", second)
	}
}

// TestPostgresStoreAuthConnectionRefreshClaimsMissingRefreshToken verifies the
// managed worker can convert an expired access-only OAuth grant into reconnect.
func TestPostgresStoreAuthConnectionRefreshClaimsMissingRefreshToken(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-access-only", versionID, "Access Only Service", fixture.accountID); err != nil {
		t.Fatalf("activate access-only fixture version: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	encrypted := encryptConnectAuthValues(t, "expired-access-only")
	expiresAt := now.Add(-time.Minute)
	connection, err := fixture.store.UpsertAuthConnection(fixture.ctx, AuthConnection{
		BucketID: fixture.bucketA, ServiceID: fixture.serviceID, ServiceVersionID: versionID,
		EndUserRef: "worker-refresh-token-missing", AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0],
		TokenType: "Bearer", ExpiresAt: &expiresAt, RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("upsert access-only worker connection: %v", err)
	}
	validExpiresAt := now.Add(5 * time.Minute)
	valid, err := fixture.store.UpsertAuthConnection(fixture.ctx, AuthConnection{
		BucketID: fixture.bucketA, ServiceID: fixture.serviceID, ServiceVersionID: versionID,
		EndUserRef: "worker-refresh-token-missing-valid", AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0],
		TokenType: "Bearer", ExpiresAt: &validExpiresAt, RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("upsert valid access-only worker connection: %v", err)
	}
	refreshStore := fixture.store.(AuthConnectionRefreshStore)
	claims := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, now.Add(time.Minute), 10)
	if len(claims) != 1 || claims[0].Connection.ID != connection.ID || claims[0].Connection.EncryptedRefreshToken != "" {
		t.Fatalf("access-only worker claims = %#v", claims)
	}
	if containsAuthConnectionClaim(claims, valid.ID) {
		t.Fatalf("still-valid access-only connection was claimed early: %#v", claims)
	}
	marked, err := refreshStore.MarkAuthConnectionReconnectRequired(fixture.ctx, connection.ID, claims[0].LeaseToken, "refresh_token_missing", "trace-worker-missing", now.Add(time.Second))
	if err != nil || !marked {
		t.Fatalf("mark worker access-only reconnect required marked=%t err=%v", marked, err)
	}
}

// TestPostgresStoreAuthConnectionRefreshClaimsLegacyRetryableStates verifies
// v7 failed/expired rows are not stranded while reconnect_required stays final.
func TestPostgresStoreAuthConnectionRefreshClaimsLegacyRetryableStates(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-legacy-states", versionID, "Legacy State Service", fixture.accountID); err != nil {
		t.Fatalf("activate legacy refresh-state fixture version: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	failed := upsertRefreshLeaseConnectionState(t, fixture, "legacy-failed", versionID, now.Add(-2*time.Minute), "failed")
	expired := upsertRefreshLeaseConnectionState(t, fixture, "legacy-expired", versionID, now.Add(-time.Minute), "expired")
	reconnect := upsertRefreshLeaseConnectionState(t, fixture, "legacy-reconnect", versionID, now.Add(-3*time.Minute), "reconnect_required")
	refreshStore := fixture.store.(AuthConnectionRefreshStore)
	claims := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, now.Add(time.Minute), 10)
	if len(claims) != 2 || !containsAuthConnectionClaim(claims, failed.ID) || !containsAuthConnectionClaim(claims, expired.ID) || containsAuthConnectionClaim(claims, reconnect.ID) {
		t.Fatalf("legacy refresh-state claims = %#v", claims)
	}
}

// TestPostgresStoreAuthConnectionRefreshClaimsEarlierRefreshExpiry proves the
// worker schedules against the earlier provider-declared token deadline.
func TestPostgresStoreAuthConnectionRefreshClaimsEarlierRefreshExpiry(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-refresh-ttl", versionID, "Refresh TTL Service", fixture.accountID); err != nil {
		t.Fatalf("activate refresh TTL fixture version: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	connection := upsertRefreshLeaseConnection(t, fixture, "earlier-refresh-expiry", versionID, now.Add(24*time.Hour))
	refreshExpiry := now.Add(30 * time.Minute)
	connection.RefreshTokenExpiresAt = &refreshExpiry
	stored, err := fixture.store.UpsertAuthConnection(fixture.ctx, *connection)
	if err != nil || stored == nil {
		t.Fatalf("persist earlier refresh-token expiry connection=%v error=%v", stored != nil, err)
	}

	claims := claimRefreshPage(t, fixture.store.(AuthConnectionRefreshStore), now.Add(70*time.Minute), now, now, now.Add(time.Minute), 10)
	if len(claims) != 1 || claims[0].Connection.ID != stored.ID {
		t.Fatalf("earlier refresh-expiry claims = %#v", claims)
	}
}

// TestPostgresStoreAuthConnectionRefreshHonorsSuccessEligibility proves a
// later replica pass waits for the persisted post-success boundary.
func TestPostgresStoreAuthConnectionRefreshHonorsSuccessEligibility(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-success-boundary", versionID, "Success Boundary Service", fixture.accountID); err != nil {
		t.Fatalf("activate success-boundary fixture version: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	connection := upsertRefreshLeaseConnection(t, fixture, "success-boundary", versionID, now.Add(-time.Minute))
	refreshStore := fixture.store.(AuthConnectionRefreshStore)
	claims := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, now.Add(time.Minute), 10)
	if len(claims) != 1 || claims[0].Connection.ID != connection.ID {
		t.Fatalf("initial success-boundary claims = %#v", claims)
	}
	refreshedAt := now.Add(time.Second)
	nextEligibleAt := now.Add(20 * time.Minute)
	updated := refreshedAuthConnectionFixture(t, now.Add(30*time.Minute))
	updated.RefreshRetryNotBefore = &nextEligibleAt
	completeCurrentRefreshClaim(t, refreshStore, claims[0], updated, refreshedAt)

	assertStaggeredPassHonorsSuccessEligibility(t, refreshStore, connection.ID, refreshedAt)
	eligibleAt := nextEligibleAt.Add(time.Second)
	claims = claimRefreshPage(t, refreshStore, eligibleAt.Add(70*time.Minute), eligibleAt, eligibleAt, eligibleAt.Add(time.Minute), 10)
	if len(claims) != 1 || claims[0].Connection.ID != connection.ID {
		t.Fatalf("post-success-boundary claims = %#v", claims)
	}
}

// releaseWorkerRefreshTransiently makes retry eligibility elapse inside the
// same pass so the attempt watermark, rather than delay length, prevents reuse.
func releaseWorkerRefreshTransiently(t *testing.T, refreshStore AuthConnectionRefreshStore, claim AuthConnectionRefreshClaim, now time.Time) {
	t.Helper()
	released, err := refreshStore.ReleaseAuthConnectionRefresh(context.Background(), claim.Connection.ID, claim.LeaseToken, now.Add(2*time.Second), "provider_unavailable", "trace-worker", now.Add(time.Second))
	if err != nil || !released {
		t.Fatalf("release worker refresh transiently released=%t err=%v", released, err)
	}
}

// claimRefreshLeasePages proves ordered limit-one pages skip an already leased
// row and exclude future and unpinned legacy credentials from worker discovery.
func claimRefreshLeasePages(t *testing.T, refreshStore AuthConnectionRefreshStore, now time.Time, earlyID, laterID uuid.UUID) (AuthConnectionRefreshClaim, AuthConnectionRefreshClaim, time.Time) {
	t.Helper()
	leaseUntil := now.Add(time.Minute)
	first := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, leaseUntil, 1)
	second := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, leaseUntil, 1)
	third := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), now, now, leaseUntil, 10)
	if len(first) != 1 || first[0].Connection.ID != earlyID || len(second) != 1 || second[0].Connection.ID != laterID || len(third) != 0 {
		t.Fatalf("ordered refresh pages first=%#v second=%#v third=%#v", first, second, third)
	}
	if first[0].LeaseToken == second[0].LeaseToken {
		t.Fatal("each claimed connection must receive a unique lease token")
	}
	return first[0], second[0], leaseUntil
}

// claimRefreshPage wraps the worker claim with a focused failure message while
// retaining explicit pass and clock inputs for deterministic lease assertions.
func claimRefreshPage(t *testing.T, refreshStore AuthConnectionRefreshStore, cutoff, passStartedAt, now, leaseUntil time.Time, limit int) []AuthConnectionRefreshClaim {
	t.Helper()
	claims, err := refreshStore.ClaimAuthConnectionsForRefresh(context.Background(), cutoff, passStartedAt, now, leaseUntil, limit)
	if err != nil {
		t.Fatalf("ClaimAuthConnectionsForRefresh: %v", err)
	}
	return claims
}

// assertRefreshLeaseContention proves an SDK fallback cannot take a worker's
// live lease and that an exact version mismatch never rewrites stored identity.
func assertRefreshLeaseContention(t *testing.T, refreshStore AuthConnectionRefreshStore, claim AuthConnectionRefreshClaim, versionID uuid.UUID, now, leaseUntil time.Time) {
	t.Helper()
	contended, err := refreshStore.TryClaimAuthConnectionRefresh(context.Background(), claim.Connection.ID, versionID, now, leaseUntil)
	if err != nil || contended != nil {
		t.Fatalf("live lease contention claim=%#v err=%v", contended, err)
	}
	concurrentFallback, err := refreshStore.TryClaimAuthConnectionRefresh(context.Background(), claim.Connection.ID, uuid.New(), now, leaseUntil)
	if err != nil || concurrentFallback != nil {
		t.Fatalf("concurrent fallback version claim=%#v err=%v", concurrentFallback, err)
	}
}

// recoverExpiredRefreshLease advances the injected clock beyond lease expiry
// and verifies another worker can claim the earliest still-due connection.
func recoverExpiredRefreshLease(t *testing.T, refreshStore AuthConnectionRefreshStore, original AuthConnectionRefreshClaim, leaseUntil time.Time) AuthConnectionRefreshClaim {
	t.Helper()
	recoveryNow := leaseUntil.Add(time.Second)
	claims := claimRefreshPage(t, refreshStore, recoveryNow.Add(70*time.Minute), recoveryNow, recoveryNow, recoveryNow.Add(time.Minute), 1)
	if len(claims) != 1 || claims[0].Connection.ID != original.Connection.ID || claims[0].LeaseToken == original.LeaseToken {
		t.Fatalf("recovered refresh claim = %#v", claims)
	}
	return claims[0]
}

// completeRecoveredRefresh rejects the first worker's stale token, then stores
// rotated ciphertext and safe refresh timing under the recovered lease CAS.
func completeRecoveredRefresh(t *testing.T, refreshStore AuthConnectionRefreshStore, stale, recovered AuthConnectionRefreshClaim, leaseUntil time.Time) {
	t.Helper()
	refreshedAt := leaseUntil.Add(2 * time.Second)
	updated := refreshedAuthConnectionFixture(t, refreshedAt.Add(30*time.Minute))
	nextEligibleAt := refreshedAt.Add(20 * time.Minute)
	updated.RefreshRetryNotBefore = &nextEligibleAt
	assertStaleRefreshCompletionRejected(t, refreshStore, stale, updated, refreshedAt)
	connection := completeCurrentRefreshClaim(t, refreshStore, recovered, updated, refreshedAt)
	if connection.RefreshRetryNotBefore == nil || !connection.RefreshRetryNotBefore.Equal(nextEligibleAt) {
		t.Fatalf("successful refresh eligibility = %#v, want %v", connection.RefreshRetryNotBefore, nextEligibleAt)
	}
	duplicate, err := refreshStore.TryClaimAuthConnectionRefresh(context.Background(), recovered.Connection.ID, recovered.Connection.ServiceVersionID, refreshedAt, refreshedAt.Add(time.Minute))
	if err != nil || duplicate != nil {
		t.Fatalf("freshly completed foreground claim=%#v err=%v", duplicate, err)
	}
}

// assertStaggeredPassHonorsSuccessEligibility proves a later replica pass does
// not rotate a newly refreshed token until the durable success boundary.
func assertStaggeredPassHonorsSuccessEligibility(t *testing.T, refreshStore AuthConnectionRefreshStore, connectionID uuid.UUID, refreshedAt time.Time) {
	t.Helper()
	staggeredAt := refreshedAt.Add(5 * time.Minute)
	claims := claimRefreshPage(t, refreshStore, staggeredAt.Add(70*time.Minute), staggeredAt, staggeredAt, staggeredAt.Add(time.Minute), 10)
	if containsAuthConnectionClaim(claims, connectionID) {
		t.Fatalf("staggered replica reclaimed connection before success boundary: %#v", claims)
	}
}

// assertStaleRefreshCompletionRejected verifies an expired/replaced worker CAS
// cannot overwrite token material owned by the recovered lease.
func assertStaleRefreshCompletionRejected(t *testing.T, refreshStore AuthConnectionRefreshStore, stale AuthConnectionRefreshClaim, updated AuthConnection, refreshedAt time.Time) {
	t.Helper()
	if connection, completed, err := refreshStore.CompleteAuthConnectionRefresh(context.Background(), stale.Connection.ID, stale.LeaseToken, updated, refreshedAt); err != nil || completed || connection != nil {
		t.Fatalf("stale refresh completion connection=%#v completed=%t err=%v", connection, completed, err)
	}
}

// completeCurrentRefreshClaim stores a successful rotation and verifies its
// safe timing metadata before returning the reloaded connection.
func completeCurrentRefreshClaim(t *testing.T, refreshStore AuthConnectionRefreshStore, recovered AuthConnectionRefreshClaim, updated AuthConnection, refreshedAt time.Time) *AuthConnection {
	t.Helper()
	connection, completed, err := refreshStore.CompleteAuthConnectionRefresh(context.Background(), recovered.Connection.ID, recovered.LeaseToken, updated, refreshedAt)
	if err != nil || !completed || connection == nil || connection.LastRefreshedAt == nil || !connection.LastRefreshedAt.Equal(refreshedAt) {
		t.Fatalf("recovered refresh completion connection=%#v completed=%t err=%v", connection, completed, err)
	}
	return connection
}

// assertRefreshPassDoesNotReclaimCompleted locks the pass-start exclusion that
// prevents short-lived provider tokens from causing an infinite drain loop.
func assertRefreshPassDoesNotReclaimCompleted(t *testing.T, refreshStore AuthConnectionRefreshStore, completedID uuid.UUID, passStartedAt, leaseUntil time.Time) {
	t.Helper()
	now := leaseUntil.Add(3 * time.Second)
	claims := claimRefreshPage(t, refreshStore, now.Add(70*time.Minute), passStartedAt, now, now.Add(time.Minute), 10)
	if len(claims) != 0 || containsAuthConnectionClaim(claims, completedID) {
		t.Fatalf("same pass reclaimed attempted connections after completion %s: %#v", completedID, claims)
	}
}

// assertForegroundRetryAndReconnect proves a transient retry suppresses both
// near-expiry and expired requests until the persisted retry deadline elapses.
func assertForegroundRetryAndReconnect(t *testing.T, fixture connectAuthFixture, refreshStore AuthConnectionRefreshStore, versionID uuid.UUID, now time.Time) {
	t.Helper()
	connection := upsertRefreshLeaseConnection(t, fixture, "refresh-foreground", versionID, now.Add(5*time.Minute))
	claimAndReleaseForegroundRefresh(t, fixture, refreshStore, connection.ID, versionID, now)
	expired := assertForegroundRetryThrottleThenEligibility(t, fixture, refreshStore, connection.ID, versionID, now)
	markForegroundReconnectRequired(t, fixture, refreshStore, connection.ID, expired.LeaseToken, now)
	assertMissingRefreshTokenCanReconnect(t, fixture, refreshStore, versionID, now)
}

// claimAndReleaseForegroundRefresh records one transient failure and a future
// retry deadline while the still-valid access token remains usable.
func claimAndReleaseForegroundRefresh(t *testing.T, fixture connectAuthFixture, refreshStore AuthConnectionRefreshStore, connectionID, versionID uuid.UUID, now time.Time) {
	t.Helper()
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(fixture.ctx, connectionID, versionID, now, now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("foreground refresh claim=%#v err=%v", claim, err)
	}
	retryAt := now.Add(10 * time.Minute)
	released, err := refreshStore.ReleaseAuthConnectionRefresh(fixture.ctx, connectionID, claim.LeaseToken, retryAt, "provider_unavailable", "trace-bounded", now.Add(time.Second))
	if err != nil || !released {
		t.Fatalf("release transient refresh released=%t err=%v", released, err)
	}
}

// assertForegroundRetryThrottleThenEligibility proves the persisted deadline
// prevents tight loops even after expiry, then permits the next bounded retry.
func assertForegroundRetryThrottleThenEligibility(t *testing.T, fixture connectAuthFixture, refreshStore AuthConnectionRefreshStore, connectionID, versionID uuid.UUID, now time.Time) AuthConnectionRefreshClaim {
	t.Helper()
	throttled, err := refreshStore.TryClaimAuthConnectionRefresh(fixture.ctx, connectionID, versionID, now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil || throttled != nil {
		t.Fatalf("near-expiry retry throttle claim=%#v err=%v", throttled, err)
	}
	expiredThrottled, err := refreshStore.TryClaimAuthConnectionRefresh(fixture.ctx, connectionID, versionID, now.Add(6*time.Minute), now.Add(7*time.Minute))
	if err != nil || expiredThrottled != nil {
		t.Fatalf("expired retry throttle claim=%#v err=%v", expiredThrottled, err)
	}
	eligible, err := refreshStore.TryClaimAuthConnectionRefresh(fixture.ctx, connectionID, versionID, now.Add(11*time.Minute), now.Add(12*time.Minute))
	if err != nil || eligible == nil {
		t.Fatalf("post-retry claim=%#v err=%v", eligible, err)
	}
	return *eligible
}

// markForegroundReconnectRequired persists the provider's permanent grant
// rejection and verifies retry scheduling is cleared.
func markForegroundReconnectRequired(t *testing.T, fixture connectAuthFixture, refreshStore AuthConnectionRefreshStore, connectionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	marked, err := refreshStore.MarkAuthConnectionReconnectRequired(fixture.ctx, connectionID, leaseToken, "invalid_grant", "trace-reconnect", now.Add(11*time.Minute+time.Second))
	if err != nil || !marked {
		t.Fatalf("mark reconnect required marked=%t err=%v", marked, err)
	}
	stored, err := refreshStore.GetAuthConnectionByID(fixture.ctx, connectionID)
	if err != nil || stored == nil || stored.RefreshState != "reconnect_required" || stored.RefreshRetryNotBefore != nil {
		t.Fatalf("reconnect-required connection=%#v err=%v", stored, err)
	}
}

// assertMissingRefreshTokenCanReconnect proves an expired access-only grant
// can still be leased so the coordinator records a typed reconnect requirement.
func assertMissingRefreshTokenCanReconnect(t *testing.T, fixture connectAuthFixture, refreshStore AuthConnectionRefreshStore, versionID uuid.UUID, now time.Time) {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "access-without-refresh")
	expiresAt := now.Add(-time.Minute)
	connection, err := fixture.store.UpsertAuthConnection(fixture.ctx, AuthConnection{
		BucketID: fixture.bucketA, ServiceID: fixture.serviceID, ServiceVersionID: versionID,
		EndUserRef: "refresh-token-missing", AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0], TokenType: "Bearer",
		ExpiresAt: &expiresAt, RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("upsert access-only refresh connection: %v", err)
	}
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(fixture.ctx, connection.ID, versionID, now, now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim access-only expired connection=%#v err=%v", claim, err)
	}
	marked, err := refreshStore.MarkAuthConnectionReconnectRequired(fixture.ctx, connection.ID, claim.LeaseToken, "refresh_token_missing", "trace-missing", now.Add(time.Second))
	if err != nil || !marked {
		t.Fatalf("mark access-only reconnect required marked=%t err=%v", marked, err)
	}
}

// assertLegacyRefreshVersionBinding verifies an unpinned row is atomically
// bound from foreground exact metadata and never by background discovery.
func assertLegacyRefreshVersionBinding(t *testing.T, refreshStore AuthConnectionRefreshStore, connectionID, versionID uuid.UUID, now time.Time) {
	t.Helper()
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(context.Background(), connectionID, versionID, now, now.Add(time.Minute))
	if err != nil || claim == nil || claim.Connection.ServiceVersionID != versionID {
		t.Fatalf("legacy exact binding claim=%#v err=%v", claim, err)
	}
	retryAt := now.Add(time.Second)
	released, err := refreshStore.ReleaseAuthConnectionRefresh(context.Background(), connectionID, claim.LeaseToken, retryAt, "provider_unavailable", "trace-legacy", now.Add(500*time.Millisecond))
	if err != nil || !released {
		t.Fatalf("release legacy binding claim released=%t err=%v", released, err)
	}
	reused, err := refreshStore.TryClaimAuthConnectionRefresh(context.Background(), connectionID, uuid.New(), now.Add(2*time.Second), now.Add(time.Minute))
	if err != nil || reused == nil || reused.Connection.ServiceVersionID != versionID {
		t.Fatalf("legacy binding reuse claim=%#v err=%v", reused, err)
	}
}

// upsertRefreshLeaseConnection creates encrypted refreshable fixture material
// with a caller-selected expiry and optional exact service-version identity.
func upsertRefreshLeaseConnection(t *testing.T, fixture connectAuthFixture, endUserRef string, versionID uuid.UUID, expiresAt time.Time) *AuthConnection {
	return upsertRefreshLeaseConnectionState(t, fixture, endUserRef, versionID, expiresAt, "ok")
}

// upsertRefreshLeaseConnectionState creates a refresh fixture in one explicit
// legacy state while retaining the shared encrypted token construction.
func upsertRefreshLeaseConnectionState(t *testing.T, fixture connectAuthFixture, endUserRef string, versionID uuid.UUID, expiresAt time.Time, refreshState string) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "access-"+endUserRef, "refresh-"+endUserRef)
	connection, err := fixture.store.UpsertAuthConnection(fixture.ctx, AuthConnection{
		BucketID: fixture.bucketA, ServiceID: fixture.serviceID, ServiceVersionID: versionID,
		EndUserRef: endUserRef, AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0],
		EncryptedRefreshToken: encrypted.values[1], TokenType: "Bearer", ExpiresAt: &expiresAt, RefreshState: refreshState,
	})
	if err != nil {
		t.Fatalf("upsert refresh lease connection %q: %v", endUserRef, err)
	}
	return connection
}

// refreshedAuthConnectionFixture returns valid rotated ciphertext without
// copying identity or lease fields into completion persistence.
func refreshedAuthConnectionFixture(t *testing.T, expiresAt time.Time) AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "rotated-access", "rotated-refresh")
	return AuthConnection{
		AuthName: "oauth", EncryptedDEK: encrypted.dek,
		EncryptedAccessToken: encrypted.values[0], EncryptedRefreshToken: encrypted.values[1],
		TokenType: "Bearer", ScopeSource: "provider", ExpiresAt: &expiresAt, RefreshState: "ok",
	}
}

// testGetBucketsByNames verifies plan-time lookup returns requested workspace
// buckets without broad listing or cross-workspace rows.
func testGetBucketsByNames(t *testing.T, f connectAuthFixture) {
	t.Helper()
	buckets, err := f.store.GetBucketsByNames(f.ctx, []string{
		"connect-auth-prod-" + f.bucketA.String(), "missing-bucket",
	})
	if err != nil {
		t.Fatalf("GetBucketsByNames: %v", err)
	}
	if len(buckets) != 1 || buckets[0].ID != f.bucketA {
		t.Fatalf("exact bucket batch = %#v", buckets)
	}
}

// testWorkspaceConnectionProfileLifecycle covers the plan's verification
// matrix for workspace-scoped profiles: baseline vs override precedence
// resolved in SQL, reset deleting only the override, version/auth-type
// isolation, and one-transaction batch reconciliation.
func testWorkspaceConnectionProfileLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	profileStore, ok := f.store.(WorkspaceProfileStore)
	if !ok {
		t.Fatal("store does not implement workspace profile store")
	}
	versionID := uuid.New()
	if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-profile", versionID, "Profile Service", f.accountID); err != nil {
		t.Fatalf("activate profile version: %v", err)
	}
	baseline := seedWorkspaceProfileBaseline(t, f, profileStore, versionID)
	assertEffectiveProfileIsBaseline(t, f, profileStore, versionID, baseline)
	assertOverrideWinsOverBaseline(t, f, profileStore, versionID)
	assertWorkspaceBindingOperationScoping(t, f, versionID)
	assertWorkspaceProfileVersionAndAuthIsolation(t, f, profileStore, versionID)
	assertResetDeletesOnlyOverride(t, f, profileStore, versionID, baseline)
	assertBatchWorkspaceProfileReconcile(t, f, profileStore)
}

// seedWorkspaceProfileBaseline attaches the pinned Registry/Fused baseline
// layer -- this mirrors what activation does, independent of any bucket.
func seedWorkspaceProfileBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) WorkspaceConnectionProfile {
	t.Helper()
	registryProfileID := uuid.New()
	basePath := "base_url"
	revision := 1
	baseline := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		AuthType: "oauth", Layer: "baseline", RegistryProfileID: &registryProfileID,
		ProfileRevision: revision, ProfileHash: "baseline-hash-1", Provenance: "provider",
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	binding := WorkspaceConnectionBinding{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		SourceKind: "connection_resource", SourcePath: &basePath, TargetLocation: "base_url",
		Mode: "force", Provenance: "provider", SourceProfileRevision: &revision,
	}
	batchStore, ok := f.store.(WorkspaceProfileBatchStore)
	if !ok {
		t.Fatal("store does not implement workspace profile batch reconciliation")
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, []WorkspaceProfileReplacement{{Profile: baseline, Bindings: []WorkspaceConnectionBinding{binding}}}, nil); err != nil {
		t.Fatalf("seed baseline via reconcile: %v", err)
	}
	stored, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || stored == nil || stored.Layer != "baseline" {
		t.Fatalf("seeded baseline = %#v, err=%v", stored, err)
	}
	return *stored
}

// assertEffectiveProfileIsBaseline proves the effective read falls back to
// the baseline when no override row exists for the tuple.
func assertEffectiveProfileIsBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID, baseline WorkspaceConnectionProfile) {
	t.Helper()
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "baseline" || effective.ID != baseline.ID {
		t.Fatalf("effective profile without override = %#v, err=%v", effective, err)
	}
}

// assertOverrideWinsOverBaseline proves the SQL-resolved effective read
// prefers the override once one exists, without disturbing the baseline row.
func assertOverrideWinsOverBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) WorkspaceConnectionProfile {
	t.Helper()
	literal := "v1"
	override := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		AuthType: "oauth", ProfileRevision: 1, ProfileHash: "override-hash-1",
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	stored, err := profileStore.UpsertWorkspaceProfileOverride(f.ctx, override, []WorkspaceConnectionBinding{{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		SourceKind: "literal", LiteralValue: &literal, TargetLocation: "header", TargetName: "X-Version",
		Mode: "force", Provenance: "workspace", OperationIDs: []string{"getIssue"},
	}})
	if err != nil {
		t.Fatalf("UpsertWorkspaceProfileOverride: %v", err)
	}
	if stored.Layer != "override" || stored.Provenance != "workspace" || stored.RegistryProfileID != nil {
		t.Fatalf("stored override = %#v", stored)
	}
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "override" || effective.ID != stored.ID {
		t.Fatalf("effective profile with override present = %#v, err=%v", effective, err)
	}
	bindings, err := profileStore.ListWorkspaceProfileBindings(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || len(bindings) != 1 || bindings[0].LiteralValue == nil || *bindings[0].LiteralValue != literal {
		t.Fatalf("override bindings = %#v, err=%v", bindings, err)
	}
	return *stored
}

// assertWorkspaceBindingOperationScoping proves the execution-binding read
// filters by operation without loading the whole profile's binding set.
func assertWorkspaceBindingOperationScoping(t *testing.T, f connectAuthFixture, versionID uuid.UUID) {
	t.Helper()
	execStore, ok := f.store.(WorkspaceProfileStore)
	if !ok {
		t.Fatal("store does not implement workspace profile store")
	}
	bindings, err := execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, f.serviceID, versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 1 || bindings[0].TargetName != "X-Version" {
		t.Fatalf("getIssue-scoped bindings = %#v, err=%v", bindings, err)
	}
	bindings, err = execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, f.serviceID, versionID, "oauth", "otherOperation")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("expected operation-scoped binding to be excluded for a different operation, got %#v, err=%v", bindings, err)
	}
	// A different bucket in the same workspace resolves the same effective
	// bindings -- profiles are workspace-scoped, not bucket-scoped.
	bindings, err = execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketB, f.serviceID, versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 1 {
		t.Fatalf("expected sibling bucket in the same workspace to resolve the same bindings, got %#v, err=%v", bindings, err)
	}
}

// assertWorkspaceProfileVersionAndAuthIsolation proves a profile for one
// version/auth family never leaks into a lookup for another.
func assertWorkspaceProfileVersionAndAuthIsolation(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) {
	t.Helper()
	otherVersion, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, uuid.New(), "oauth")
	if err != nil || otherVersion != nil {
		t.Fatalf("unrelated version leaked a profile: %#v, err=%v", otherVersion, err)
	}
	otherAuth, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oidc")
	if err != nil || otherAuth != nil {
		t.Fatalf("unrelated auth family leaked a profile: %#v, err=%v", otherAuth, err)
	}
	bindings, err := profileStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, uuid.New(), versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("cross-service bindings = %#v, err=%v", bindings, err)
	}
}

// assertResetDeletesOnlyOverride is the "override survives" analog under the
// new model: resetting removes only the override row so the baseline --
// which was never touched -- immediately becomes the effective profile
// again, with its original revision/hash intact and no Registry call needed.
func assertResetDeletesOnlyOverride(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID, baseline WorkspaceConnectionProfile) {
	t.Helper()
	if err := profileStore.ResetWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth"); err != nil {
		t.Fatalf("ResetWorkspaceProfile: %v", err)
	}
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "baseline" || effective.ID != baseline.ID || effective.ProfileHash != baseline.ProfileHash {
		t.Fatalf("effective profile after reset = %#v, err=%v, want unchanged baseline %#v", effective, err, baseline)
	}
	bindings, err := profileStore.ListWorkspaceProfileBindings(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || len(bindings) != 1 || bindings[0].TargetLocation != "base_url" {
		t.Fatalf("bindings after reset should be the baseline's own rows, got %#v, err=%v", bindings, err)
	}
	// Resetting again is idempotent: no override remains to delete.
	if err := profileStore.ResetWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth"); err != nil {
		t.Fatalf("idempotent ResetWorkspaceProfile: %v", err)
	}
}

// assertBatchWorkspaceProfileReconcile proves multi-version replacements and
// exact deletes use the fixed-query transactional store path in one
// transaction, matching set-based reconciliation used by workspace apply.
func assertBatchWorkspaceProfileReconcile(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore) {
	t.Helper()
	batchStore, ok := f.store.(WorkspaceProfileBatchStore)
	if !ok {
		t.Fatal("store does not implement workspace profile batch reconciliation")
	}
	firstVersion, secondVersion := uuid.New(), uuid.New()
	for _, versionID := range []uuid.UUID{firstVersion, secondVersion} {
		if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-batch-"+versionID.String(), versionID, "Batch Service", f.accountID); err != nil {
			t.Fatalf("activate batch version: %v", err)
		}
	}
	first := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: firstVersion,
		AuthType: "oauth", Layer: "override", Provenance: "workspace",
		ProfileRevision: 1, ProfileHash: "batch-1", ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	literal := "batch"
	second := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: secondVersion,
		AuthType: "oauth", Layer: "override", Provenance: "workspace",
		ProfileRevision: 1, ProfileHash: "batch-2", ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	binding := WorkspaceConnectionBinding{
		ServiceID: f.serviceID, ServiceVersionID: secondVersion,
		SourceKind: "literal", LiteralValue: &literal, TargetLocation: "query", TargetName: "portal",
		Mode: "force", Provenance: "workspace",
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, []WorkspaceProfileReplacement{
		{Profile: first, Bindings: nil}, {Profile: second, Bindings: []WorkspaceConnectionBinding{binding}},
	}, nil); err != nil {
		t.Fatalf("batch replace profiles: %v", err)
	}
	profiles, err := profileStore.GetEffectiveWorkspaceProfiles(f.ctx, []WorkspaceProfileRef{
		{ServiceID: f.serviceID, ServiceVersionID: firstVersion, AuthType: "oauth"},
		{ServiceID: f.serviceID, ServiceVersionID: secondVersion, AuthType: "oauth"},
	})
	if err != nil || len(profiles) != 2 {
		t.Fatalf("batch profile lookup = %#v, err=%v", profiles, err)
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, nil, []WorkspaceProfileRef{
		{ServiceID: f.serviceID, ServiceVersionID: firstVersion, AuthType: "oauth"},
	}); err != nil {
		t.Fatalf("batch delete profile: %v", err)
	}
	removed, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, firstVersion, "oauth")
	if err != nil || removed != nil {
		t.Fatalf("batch profile delete left %#v, err=%v", removed, err)
	}
	kept, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, secondVersion, "oauth")
	if err != nil || kept == nil {
		t.Fatalf("batch delete removed an unrelated version: %#v, err=%v", kept, err)
	}
}

// testConnectionResourceLifecycle covers authoritative batch replacement,
// automatic sole default, explicit default, and connection-scoped lookup.
func testConnectionResourceLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	connection := upsertOAuthConnectionForUser(t, f, "resource_user")
	first := []ConnectionResource{{
		ConnectionID: connection.ID, BucketID: f.bucketA, ServiceID: f.serviceID,
		ProviderResourceID: "cloud-a", ResourceType: "jira_site", DisplayName: "Acme",
		BaseURL: "https://api.atlassian.com/ex/jira/cloud-a", MetadataJSON: []byte(`{"provider_resource_id":"cloud-a"}`),
	}}
	resources, err := f.store.ReconcileConnectionResources(f.ctx, connection.ID, first)
	if err != nil || len(resources) != 1 || !resources[0].IsDefault {
		t.Fatalf("first reconcile: resources=%#v err=%v", resources, err)
	}
	second := append(first, ConnectionResource{
		ConnectionID: connection.ID, BucketID: f.bucketA, ServiceID: f.serviceID,
		ProviderResourceID: "cloud-b", ResourceType: "jira_site", DisplayName: "Beta",
		BaseURL: "https://api.atlassian.com/ex/jira/cloud-b", MetadataJSON: []byte(`{}`),
	})
	resources, err = f.store.ReconcileConnectionResources(f.ctx, connection.ID, second)
	if err != nil || len(resources) != 2 {
		t.Fatalf("second reconcile: resources=%#v err=%v", resources, err)
	}
	selected, count, err := f.store.GetConnectionResourceForExecution(f.ctx, connection.ID, nil)
	if err != nil || selected == nil || selected.ProviderResourceID != "cloud-a" || count != 2 {
		t.Fatalf("default selection: selected=%#v count=%d err=%v", selected, count, err)
	}
	if _, err := f.store.SetDefaultConnectionResource(f.ctx, connection.ID, resources[1].ID); err != nil {
		t.Fatalf("SetDefaultConnectionResource: %v", err)
	}
	selected, _, err = f.store.GetConnectionResourceForExecution(f.ctx, connection.ID, nil)
	if err != nil || selected == nil || selected.ID != resources[1].ID {
		t.Fatalf("updated default selection: selected=%#v err=%v", selected, err)
	}
	resources, err = f.store.ReconcileConnectionResources(f.ctx, connection.ID, second[1:])
	if err != nil || len(resources) != 1 || resources[0].ProviderResourceID != "cloud-b" || !resources[0].IsDefault {
		t.Fatalf("authoritative removal: resources=%#v err=%v", resources, err)
	}
}

// testCallbackConnectionAtomicity proves reconnect rollback, exact resource
// replacement, and per-end-user isolation against PostgreSQL rather than an
// in-memory transaction model.
func testCallbackConnectionAtomicity(t *testing.T, f connectAuthFixture) {
	t.Helper()
	savedPrior := seedAtomicCallbackGrant(t, f)
	assertAtomicCallbackRollback(t, f)
	assertPostgresCallbackGrant(t, f, savedPrior.ID, "atomic_user_a", "prior-access", "prior-cloud")
	reconnectAtomicCallbackGrant(t, f, savedPrior.ID)
	assertPostgresCallbackGrant(t, f, savedPrior.ID, "atomic_user_a", "reconnected-access", "selected-cloud")
	savedSecond := connectSecondAtomicCallbackUser(t, f, savedPrior.ID)
	assertPostgresCallbackGrant(t, f, savedSecond.ID, "atomic_user_b", "second-user-access", "second-user-cloud")
	assertPostgresCallbackGrant(t, f, savedPrior.ID, "atomic_user_a", "reconnected-access", "selected-cloud")
}

// seedAtomicCallbackGrant creates the previous committed state used by both
// rollback and reconnect assertions.
func seedAtomicCallbackGrant(t *testing.T, f connectAuthFixture) *AuthConnection {
	t.Helper()
	connection := callbackAuthConnection(t, f, "atomic_user_a", "prior-access")
	resource := callbackConnectionResource(f, "prior-cloud")
	saved, rows, err := f.store.UpsertAuthConnectionAndReconcileResources(f.ctx, connection, []ConnectionResource{resource})
	// The initial atomic grant must persist both halves before rollback testing begins.
	if err != nil {
		t.Fatalf("seed callback grant: %v", err)
	}
	// One authoritative provider resource is the complete seeded routing set.
	if len(rows) != 1 {
		t.Fatalf("seed callback resources = %#v", rows)
	}
	return saved
}

// assertAtomicCallbackRollback injects an ownership failure after credential
// upsert has executed inside the transaction.
func assertAtomicCallbackRollback(t *testing.T, f connectAuthFixture) {
	t.Helper()
	connection := callbackAuthConnection(t, f, "atomic_user_a", "rejected-access")
	resource := callbackConnectionResource(f, "rejected-cloud")
	resource.BucketID = f.bucketB
	// Cross-bucket routing must reject the entire credential-and-resource transaction.
	if _, _, err := f.store.UpsertAuthConnectionAndReconcileResources(f.ctx, connection, []ConnectionResource{resource}); err == nil {
		t.Fatal("expected ownership failure to roll back callback transaction")
	}
}

// reconnectAtomicCallbackGrant replaces both halves of an existing grant and
// verifies the natural-key connection identity remains stable.
func reconnectAtomicCallbackGrant(t *testing.T, f connectAuthFixture, priorID uuid.UUID) {
	t.Helper()
	connection := callbackAuthConnection(t, f, "atomic_user_a", "reconnected-access")
	resource := callbackConnectionResource(f, "selected-cloud")
	saved, rows, err := f.store.UpsertAuthConnectionAndReconcileResources(f.ctx, connection, []ConnectionResource{resource})
	// Reconnect must atomically replace the prior grant without a partial result.
	if err != nil {
		t.Fatalf("reconnect callback grant: %v", err)
	}
	// Natural-key stability lets existing selectors continue to reference the connection.
	if saved.ID != priorID {
		t.Fatalf("reconnect connection ID = %s want %s", saved.ID, priorID)
	}
	// The returned routing set must contain only the authoritative replacement.
	if len(rows) != 1 || rows[0].ProviderResourceID != "selected-cloud" {
		t.Fatalf("reconnect callback resources = %#v", rows)
	}
}

// connectSecondAtomicCallbackUser proves the same service and bucket retain an
// independent natural key and resource set for another product end user.
func connectSecondAtomicCallbackUser(t *testing.T, f connectAuthFixture, priorID uuid.UUID) *AuthConnection {
	t.Helper()
	connection := callbackAuthConnection(t, f, "atomic_user_b", "second-user-access")
	resource := callbackConnectionResource(f, "second-user-cloud")
	saved, rows, err := f.store.UpsertAuthConnectionAndReconcileResources(f.ctx, connection, []ConnectionResource{resource})
	// A second end user must receive an independent atomic grant.
	if err != nil {
		t.Fatalf("second user callback grant: %v", err)
	}
	// Reusing the first user's ID would collapse the multi-user isolation boundary.
	if saved.ID == priorID {
		t.Fatal("second user reused the first user's connection")
	}
	// The second user receives only their own authoritative routing row.
	if len(rows) != 1 {
		t.Fatalf("second user callback resources = %#v", rows)
	}
	return saved
}

// callbackAuthConnection creates fresh encrypted token material for one
// callback natural key without sharing ciphertext across users or reconnects.
func callbackAuthConnection(t *testing.T, f connectAuthFixture, endUserRef, token string) AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, token)
	return AuthConnection{
		BucketID: f.bucketA, ServiceID: f.serviceID, ServiceVersionID: fixtureServiceVersionID(t, f), EndUserRef: endUserRef,
		AuthType: "oauth", AuthName: "oauth", EncryptedDEK: encrypted.dek,
		EncryptedAccessToken: encrypted.values[0], TokenType: "Bearer", RefreshState: "ok",
	}
}

// callbackConnectionResource creates one provider-selected route while leaving
// connection identity for the transactional store to assign after upsert.
func callbackConnectionResource(f connectAuthFixture, providerID string) ConnectionResource {
	return ConnectionResource{
		BucketID: f.bucketA, ServiceID: f.serviceID, ProviderResourceID: providerID,
		ResourceType: "jira_site", DisplayName: providerID,
		BaseURL:      "https://api.atlassian.com/ex/jira/" + providerID,
		MetadataJSON: []byte(`{"site_url":"https://tenant.atlassian.net"}`), IsActive: true,
	}
}

// assertPostgresCallbackGrant reloads both sides of a grant through public
// store methods so rollback assertions cannot pass on stale local values.
func assertPostgresCallbackGrant(t *testing.T, f connectAuthFixture, connectionID uuid.UUID, endUserRef, token, providerID string) {
	t.Helper()
	connection, err := f.store.GetAuthConnection(f.ctx, f.bucketA, f.serviceID, endUserRef, "oauth")
	if err != nil || connection == nil || connection.ID != connectionID {
		t.Fatalf("load callback connection: connection=%#v err=%v", connection, err)
	}
	if got := decryptConnectAuthValue(t, connection.EncryptedDEK, connection.EncryptedAccessToken); got != token {
		t.Fatalf("callback token = %q want %q", got, token)
	}
	resources, err := f.store.ListConnectionResources(f.ctx, connection.ID)
	if err != nil || len(resources) != 1 || resources[0].ProviderResourceID != providerID {
		t.Fatalf("callback resources = %#v want %q err=%v", resources, providerID, err)
	}
}

// upsertOAuthConnectionForUser creates an isolated connection key so resource
// tests do not depend on state created by sibling subtests.
func upsertOAuthConnectionForUser(t *testing.T, f connectAuthFixture, endUserRef string) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "resource-access")
	connection, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID: f.bucketA, ServiceID: f.serviceID, ServiceVersionID: fixtureServiceVersionID(t, f),
		EndUserRef: endUserRef, AuthType: "oauth", AuthName: "oauth", EncryptedDEK: encrypted.dek,
		EncryptedAccessToken: encrypted.values[0], TokenType: "Bearer", RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection resource user: %v", err)
	}
	return connection
}

func TestPostgresStoreConnectRejectsPlaintextAuthMaterial(t *testing.T) {
	s := &postgresStore{}
	_, err := s.UpsertConnectConfig(context.Background(), ConnectConfig{
		EncryptedDEK:          "dek",
		EncryptedClientID:     "client-id",
		EncryptedClientSecret: "client-secret",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected config plaintext rejection, got %v", err)
	}

	_, err = s.UpsertAuthConnection(context.Background(), AuthConnection{
		EncryptedDEK:         "dek",
		EncryptedAccessToken: "access-token",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected connection plaintext rejection, got %v", err)
	}

	_, err = s.CreateConnectSession(context.Background(), ConnectSession{
		EncryptedPKCEVerifier: "pkce-verifier",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected session plaintext rejection, got %v", err)
	}
}

type connectAuthFixture struct {
	ctx           context.Context
	cancel        context.CancelFunc
	pool          interface{ Close() }
	store         Store
	workspaceID   uuid.UUID
	bucketA       uuid.UUID
	bucketB       uuid.UUID
	serviceID     uuid.UUID
	appID         uuid.UUID
	appFamilyID   uuid.UUID
	ownerTeamID   uuid.UUID
	accountID     uuid.UUID
	ownsWorkspace bool
}

// setupConnectAuthStore keeps fixture ownership explicit so the same test is
// safe against both disposable CI databases and a running developer Engine.
func setupConnectAuthStore(t *testing.T) connectAuthFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Connect auth store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool := isolatedConnectAuthPool(t, ctx, dbURL)
	workspaceID, accountID, ownsWorkspace := connectAuthWorkspace(t, ctx, pool)
	fixture := connectAuthFixture{
		ctx:           ctx,
		cancel:        cancel,
		pool:          pool,
		store:         NewPostgresStore(pool),
		workspaceID:   workspaceID,
		bucketA:       uuid.New(),
		bucketB:       uuid.New(),
		serviceID:     uuid.New(),
		appID:         uuid.New(),
		appFamilyID:   uuid.New(),
		ownerTeamID:   seedAppOwnerTeam(t, ctx, pool),
		accountID:     accountID,
		ownsWorkspace: ownsWorkspace,
	}
	// This integration may run against a developer database, so clean up only
	// its UUID-scoped fixture instead of resetting shared Engine state.
	t.Cleanup(func() {
		cleanupConnectAuthFixture(pool, fixture)
		pool.Close()
		cancel()
	})
	seedConnectAuthFixture(t, pool, fixture)
	return fixture
}

// isolatedConnectAuthPool gives each integration test a UUID-named schema so
// failed prior runs and developer Engine rows cannot affect ordered assertions.
func isolatedConnectAuthPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect Connect auth test database: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := "engine_connect_auth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create isolated Connect auth schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated Connect auth schema: %v", err)
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("DATABASE_URL must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	if err != nil {
		t.Fatalf("initialize isolated Connect auth schema: %v", err)
	}
	return pool
}

// connectAuthWorkspace reuses the Engine singleton when present because
// creating a second workspace would violate the production schema by design.
func connectAuthWorkspace(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) (uuid.UUID, uuid.UUID, bool) {
	t.Helper()
	var workspaceID, accountID uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id, account_id FROM fused_workspaces LIMIT 1`).Scan(&workspaceID, &accountID)
	if err == nil {
		return workspaceID, accountID, false
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("load connect auth workspace: %v", err)
	}
	accountID = uuid.New()
	err = pool.QueryRow(ctx, `
		INSERT INTO fused_workspaces (name, account_id, slug, singleton_key) VALUES ($1, $2, $3, 1)
		RETURNING id
	`, "Connect Auth Workspace", accountID, "connect-auth-"+uuid.NewString()).Scan(&workspaceID)
	if err != nil {
		t.Fatalf("seed connect auth workspace: %v", err)
	}
	return workspaceID, accountID, true
}

// cleanupConnectAuthFixture preserves a reused workspace while relying on
// foreign keys to remove every connection row owned by the test buckets.
func cleanupConnectAuthFixture(db execer, fixture connectAuthFixture) {
	ctx := context.Background()
	_, _ = db.Exec(ctx, `DELETE FROM fused_apps WHERE app_id = $1`, fixture.appID)
	_, _ = db.Exec(ctx, `DELETE FROM fused_app_families WHERE app_family_id = $1`, fixture.appFamilyID)
	_, _ = db.Exec(ctx, `DELETE FROM fused_teams WHERE id = $1`, fixture.ownerTeamID)
	if fixture.ownsWorkspace {
		_, _ = db.Exec(ctx, `DELETE FROM fused_workspaces WHERE id = $1`, fixture.workspaceID)
		return
	}
	_, _ = db.Exec(ctx, `DELETE FROM fused_buckets WHERE id = $1 OR id = $2`, fixture.bucketA, fixture.bucketB)
	_, _ = db.Exec(ctx, `DELETE FROM fused_workspace_services WHERE service_id = $1`, fixture.serviceID)
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// seedConnectAuthFixture creates only UUID-addressable child rows, allowing
// cleanup to preserve any singleton workspace that predated the test.
func seedConnectAuthFixture(t *testing.T, db execer, f connectAuthFixture) {
	t.Helper()
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_buckets (id, name) VALUES ($1, $3), ($2, $4)
	`, f.bucketA, f.bucketB, "connect-auth-prod-"+f.bucketA.String(), "connect-auth-staging-"+f.bucketB.String()); err != nil {
		t.Fatalf("seed connect auth buckets: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Connect Auth SDK', 'typescript', $4)
	`, f.appFamilyID, f.accountID, "connect-auth-"+f.appFamilyID.String(), f.ownerTeamID); err != nil {
		t.Fatalf("seed connect auth app family: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($3, $1, $2, '1.0.0', $4, 'connect-auth', 'active')
	`, f.appFamilyID, f.accountID, f.appID, "sdk:connect-auth:"+f.appFamilyID.String()); err != nil {
		t.Fatalf("seed connect auth app runtime: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id) VALUES ($1, $2)
	`, f.appFamilyID, f.bucketA); err != nil {
		t.Fatalf("seed connect auth app bucket: %v", err)
	}
}

func testConnectConfigOwnership(t *testing.T, f connectAuthFixture) {
	t.Helper()
	cfg, err := f.store.UpsertConnectConfig(f.ctx, connectConfigForFixture(t, f))
	if err != nil {
		t.Fatalf("UpsertConnectConfig: %v", err)
	}
	if cfg.BucketID != f.bucketA || cfg.ServiceID != f.serviceID {
		t.Fatalf("unexpected connect config identity: %#v", cfg)
	}
	versionID := uuid.New()
	if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-connect", versionID, "Connect Service", f.accountID); err != nil {
		t.Fatalf("activate connect service: %v", err)
	}
	configs, err := f.store.ListConnectConfigsForService(f.ctx, f.serviceID)
	if err != nil || len(configs) != 1 || configs[0].BucketID != f.bucketA {
		t.Fatalf("ListConnectConfigsForService: configs=%#v err=%v", configs, err)
	}
	syncReader := f.store.(interface {
		ListWorkspaceConnectConfigs(context.Context) ([]WorkspaceConnectConfig, error)
	})
	exported, err := syncReader.ListWorkspaceConnectConfigs(f.ctx)
	if err != nil || len(exported) != 1 || exported[0].BucketName == "" {
		t.Fatalf("ListWorkspaceConnectConfigs: configs=%#v err=%v", exported, err)
	}

}

func connectConfigForFixture(t *testing.T, f connectAuthFixture) ConnectConfig {
	encrypted := encryptConnectAuthValues(t, "client-id-v1", "client-secret-v1")
	return ConnectConfig{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		AuthType:              "oauth",
		AuthName:              "oauth",
		Enabled:               true,
		EncryptedDEK:          encrypted.dek,
		EncryptedClientID:     encrypted.values[0],
		EncryptedClientSecret: encrypted.values[1],
		RedirectURI:           "https://engine.example.com/connect/callback",
	}
}

func testAuthConnectionsReusableByBucket(t *testing.T, f connectAuthFixture) {
	t.Helper()
	upsertConnectConfigForSummary(t, f)
	connA := upsertOAuthConnection(t, f)
	assertAuthConnectionFailureDiagnostic(t, f, connA)
	connA = assertReconnectUpsertReplacesConnection(t, f, connA)
	connB := upsertAPIKeyConnection(t, f)
	assertDifferentBucketConnections(t, connA, connB)
	assertBucketConnectionLookup(t, f, connA)
	assertConnectionListAndRefreshQuery(t, f, connA.ID, connB.ID)
	assertBucketConnectSummary(t, f)
}

// assertAuthConnectionFailureDiagnostic proves a provider failure updates only
// sanitized metadata and leaves the connection usable until Engine says otherwise.
func assertAuthConnectionFailureDiagnostic(t *testing.T, f connectAuthFixture, connection *AuthConnection) {
	t.Helper()
	failedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.store.RecordAuthConnectionFailure(f.ctx, connection.ID, "provider_unauthorized", "trace-123", failedAt); err != nil {
		t.Fatalf("RecordAuthConnectionFailure: %v", err)
	}
	found, err := f.store.GetAuthConnection(f.ctx, connection.BucketID, connection.ServiceID, connection.EndUserRef, connection.AuthName)
	if err != nil {
		t.Fatalf("GetAuthConnection after diagnostic: %v", err)
	}
	if found == nil || found.RefreshState != "ok" || found.LastFailureCode != "provider_unauthorized" || found.LastFailureTraceID != "trace-123" {
		t.Fatalf("unexpected connection diagnostic: %#v", found)
	}
}

// assertReconnectUpsertReplacesConnection proves callback storage for the same
// bucket/service/user restores one existing row rather than creating a second.
func assertReconnectUpsertReplacesConnection(t *testing.T, f connectAuthFixture, previous *AuthConnection) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "reconnected-access", "reconnected-refresh")
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	reconnected, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID: f.bucketA, ServiceID: f.serviceID, ServiceVersionID: fixtureServiceVersionID(t, f),
		EndUserRef: "user_123", CreatedByAppID: f.appID, AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0],
		EncryptedRefreshToken: encrypted.values[1], TokenType: "Bearer", ExpiresAt: &expiresAt, RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection reconnect: %v", err)
	}
	if reconnected.ID != previous.ID || reconnected.RefreshState != "ok" {
		t.Fatalf("reconnect must replace existing row and reset state: before=%#v after=%#v", previous, reconnected)
	}
	if reconnected.LastFailureCode != "" || reconnected.LastFailureAt != nil || reconnected.LastFailureTraceID != "" {
		t.Fatalf("reconnect must clear stale diagnostics: %#v", reconnected)
	}
	if decryptConnectAuthValue(t, reconnected.EncryptedDEK, reconnected.EncryptedAccessToken) != "reconnected-access" {
		t.Fatalf("reconnect did not replace encrypted access token")
	}
	return reconnected
}

func upsertConnectConfigForSummary(t *testing.T, f connectAuthFixture) {
	t.Helper()
	if _, err := f.store.UpsertConnectConfig(f.ctx, connectConfigForFixture(t, f)); err != nil {
		t.Fatalf("UpsertConnectConfig for summary: %v", err)
	}
}

// upsertOAuthConnection stores one expiring, exact-version OAuth credential
// used by bucket reuse and worker discovery assertions.
func upsertOAuthConnection(t *testing.T, f connectAuthFixture) *AuthConnection {
	t.Helper()
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	encrypted := encryptConnectAuthValues(t, "access-a", "refresh-a")
	conn, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		ServiceVersionID:      fixtureServiceVersionID(t, f),
		EndUserRef:            "user_123",
		CreatedByAppID:        f.appID,
		AuthType:              "oauth",
		AuthName:              "oauth",
		EncryptedDEK:          encrypted.dek,
		EncryptedAccessToken:  encrypted.values[0],
		EncryptedRefreshToken: encrypted.values[1],
		TokenType:             "Bearer",
		Scopes:                []string{"openid", "email"},
		Issuer:                "https://issuer.example.com",
		Subject:               "sub-123",
		IdentityClaims:        []byte(`{"email":"user@example.com"}`),
		ExpiresAt:             &expiresAt,
		RefreshState:          "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection bucket A: %v", err)
	}
	return conn
}

func upsertAPIKeyConnection(t *testing.T, f connectAuthFixture) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "access-b")
	conn, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID:             f.bucketB,
		ServiceID:            f.serviceID,
		EndUserRef:           "user_123",
		AuthType:             "api_key",
		AuthName:             "api_key",
		EncryptedDEK:         encrypted.dek,
		EncryptedAccessToken: encrypted.values[0],
		TokenType:            "Bearer",
		RefreshState:         "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection bucket B: %v", err)
	}
	return conn
}

func assertDifferentBucketConnections(t *testing.T, connA, connB *AuthConnection) {
	t.Helper()
	if connA.ID == connB.ID {
		t.Fatal("same end user/service in different buckets must produce separate connections")
	}
}

func assertBucketConnectionLookup(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	assertAuthConnectionByNaturalKey(t, f, connA)
	assertAuthConnectionBucketAccess(t, f, connA)
	assertCrossBucketDeleteBlocked(t, f, connA.ID)
}

func assertAuthConnectionByNaturalKey(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	found, err := f.store.GetAuthConnection(f.ctx, f.bucketA, f.serviceID, "user_123", "oauth")
	if err != nil {
		t.Fatalf("GetAuthConnection: %v", err)
	}
	if found == nil || found.ID != connA.ID || decryptConnectAuthValue(t, found.EncryptedDEK, found.EncryptedAccessToken) != "reconnected-access" {
		t.Fatalf("expected bucket A connection, got %#v", found)
	}
}

func assertAuthConnectionBucketAccess(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	allowed, err := f.store.GetAuthConnectionByIDForBuckets(f.ctx, connA.ID, []uuid.UUID{f.bucketA})
	if err != nil {
		t.Fatalf("GetAuthConnectionByIDForBuckets allowed: %v", err)
	}
	if allowed == nil || allowed.ID != connA.ID {
		t.Fatalf("expected connection through linked bucket, got %#v", allowed)
	}

	blocked, err := f.store.GetAuthConnectionByIDForBuckets(f.ctx, connA.ID, []uuid.UUID{f.bucketB})
	if err != nil {
		t.Fatalf("GetAuthConnectionByIDForBuckets blocked: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected cross-bucket connection lookup to be blocked, got %#v", blocked)
	}
}

func assertCrossBucketDeleteBlocked(t *testing.T, f connectAuthFixture, connID uuid.UUID) {
	t.Helper()
	if err := f.store.DeleteAuthConnection(f.ctx, f.bucketB, connID); !errors.Is(err, ErrAuthConnectionNotFound) {
		t.Fatalf("expected cross-bucket delete to be not found, got %v", err)
	}
}

// assertConnectionListAndRefreshQuery verifies authorized listing and the
// initial worker claim shape against OAuth versus API-key rows.
func assertConnectionListAndRefreshQuery(t *testing.T, f connectAuthFixture, connAID, connBID uuid.UUID) {
	t.Helper()
	connectionsByID, err := f.store.GetAuthConnectionsByIDs(f.ctx, []uuid.UUID{connAID, connBID, connAID})
	if err != nil {
		t.Fatalf("GetAuthConnectionsByIDs: %v", err)
	}
	if len(connectionsByID) != 2 || connectionsByID[connAID].ID != connAID || connectionsByID[connBID].ID != connBID {
		t.Fatalf("batched connections = %#v, want both requested IDs once", connectionsByID)
	}

	serviceFilter := f.serviceID
	listed, err := f.store.ListAuthConnections(f.ctx, f.bucketA, &serviceFilter, "user_123")
	if err != nil {
		t.Fatalf("ListAuthConnections: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != connAID {
		t.Fatalf("expected one bucket-filtered connection, got %#v", listed)
	}
	assertWorkerClaimsOAuthNotAPIKey(t, f, connAID, connBID)
}

// assertWorkerClaimsOAuthNotAPIKey verifies due discovery admits connected
// OAuth credentials without treating non-refreshable API keys as jobs.
func assertWorkerClaimsOAuthNotAPIKey(t *testing.T, f connectAuthFixture, connAID, connBID uuid.UUID) {
	t.Helper()
	refreshStore := f.store.(AuthConnectionRefreshStore)
	now := time.Now().UTC()
	refreshable, err := refreshStore.ClaimAuthConnectionsForRefresh(f.ctx, now.Add(5*time.Minute), now, now, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ClaimAuthConnectionsForRefresh: %v", err)
	}
	if !containsAuthConnectionClaim(refreshable, connAID) || containsAuthConnectionClaim(refreshable, connBID) {
		t.Fatalf("expected only OAuth connection with refresh token to need refresh, got %#v", refreshable)
	}
}

func assertBucketConnectSummary(t *testing.T, f connectAuthFixture) {
	t.Helper()
	summary, err := f.store.GetBucketConnectSummary(f.ctx, f.bucketA)
	if err != nil {
		t.Fatalf("GetBucketConnectSummary: %v", err)
	}
	if summary.BucketID != f.bucketA || summary.ConnectConfigCount != 1 || summary.ConnectedUserCount != 1 {
		t.Fatalf("unexpected bucket connect summary: %#v", summary)
	}
}

func testConnectSessionLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	session := createConnectSession(t, f)
	assertConnectSessionLookup(t, f, session)
	markConnectSessionUsed(t, f, session.StateHash)
	deleteExpiredConnectSession(t, f)
}

// createConnectSession persists one exact-version callback session for lookup,
// consumption, and expiry cleanup assertions.
func createConnectSession(t *testing.T, f connectAuthFixture) *ConnectSession {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "pkce-verifier")
	session, err := f.store.CreateConnectSession(f.ctx, ConnectSession{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		ServiceVersionID:      fixtureServiceVersionID(t, f),
		AuthType:              "oauth",
		AuthName:              "oauth",
		EndUserRef:            "user_456",
		StateHash:             "state-" + uuid.NewString(),
		NonceHash:             "nonce-hash",
		EncryptedDEK:          encrypted.dek,
		EncryptedPKCEVerifier: encrypted.values[0],
		CreatedByAppID:        f.appID,
		ReturnURL:             "https://app.example.com/oauth/done",
		RequestedScopes:       []string{"test"},
		ExpiresAt:             time.Now().UTC().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateConnectSession: %v", err)
	}
	return session
}

func assertConnectSessionLookup(t *testing.T, f connectAuthFixture, session *ConnectSession) {
	t.Helper()
	found, err := f.store.GetConnectSessionByStateHash(f.ctx, session.StateHash)
	if err != nil {
		t.Fatalf("GetConnectSessionByStateHash: %v", err)
	}
	if found == nil || found.BucketID != f.bucketA || found.EndUserRef != "user_456" {
		t.Fatalf("unexpected connect session: %#v", found)
	}
	if found.ReturnURL != "https://app.example.com/oauth/done" {
		t.Fatalf("expected return_url to round-trip, got %q", found.ReturnURL)
	}
	if decryptConnectAuthValue(t, found.EncryptedDEK, found.EncryptedPKCEVerifier) != "pkce-verifier" {
		t.Fatal("expected encrypted PKCE verifier to decrypt to fixture value")
	}
}

func markConnectSessionUsed(t *testing.T, f connectAuthFixture, stateHash string) {
	t.Helper()
	if err := f.store.MarkConnectSessionUsed(f.ctx, stateHash, time.Now().UTC()); err != nil {
		t.Fatalf("MarkConnectSessionUsed: %v", err)
	}
	used, err := f.store.GetConnectSessionByStateHash(f.ctx, stateHash)
	if err != nil {
		t.Fatalf("GetConnectSessionByStateHash after mark used: %v", err)
	}
	if used == nil || used.UsedAt == nil {
		t.Fatal("expected connect session to be marked used")
	}
	if err := f.store.MarkConnectSessionUsed(f.ctx, stateHash, time.Now().UTC()); !errors.Is(err, ErrConnectSessionUnavailable) {
		t.Fatalf("expected replayed connect session mark to fail, got %v", err)
	}
}

func deleteExpiredConnectSession(t *testing.T, f connectAuthFixture) {
	t.Helper()
	deleted, err := f.store.DeleteExpiredConnectSessions(f.ctx, time.Now().UTC().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredConnectSessions: %v", err)
	}
	if deleted == 0 {
		t.Fatal("expected expired session cleanup to delete at least one row")
	}
}

// testConnectInputSessionLifecycle verifies PostgreSQL owns the one-time
// transition from a hashed form token to an encrypted provider callback
// session, including replay denial and exact indexed lookup.
func testConnectInputSessionLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	now := time.Now().UTC()
	tokenHash := "input-" + uuid.NewString()
	pending, err := f.store.CreateConnectInputSession(f.ctx, ConnectInputSession{
		BucketID: f.bucketA, ServiceID: f.serviceID, AuthType: "oauth", AuthName: "oauth",
		ContractHash: "sha256:" + strings.Repeat("a", 64), EndUserRef: "user_input", TokenHash: tokenHash, CreatedByAppID: f.appID,
		ReturnURL: "https://app.example.com/oauth/done", ResourceInputJSON: []byte(`{"subdomain":"acme"}`),
		RequestedScopes: []string{"read"}, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateConnectInputSession: %v", err)
	}
	found, err := f.store.GetActiveConnectInputSessionByTokenHash(f.ctx, tokenHash)
	if err != nil || found == nil || found.ID != pending.ID || found.EndUserRef != "user_input" {
		t.Fatalf("GetActiveConnectInputSessionByTokenHash = %#v err=%v", found, err)
	}

	providerSession := postgresInputCompletionSession(t, f, now)
	mismatched := providerSession
	mismatched.EndUserRef = "different_user"
	if _, err := f.store.CompleteConnectInputSession(f.ctx, tokenHash, pending.ContractHash, now, mismatched); !errors.Is(err, ErrConnectSessionUnavailable) {
		t.Fatalf("identity-mismatched CompleteConnectInputSession error = %v", err)
	}
	created, err := f.store.CompleteConnectInputSession(f.ctx, tokenHash, pending.ContractHash, now, providerSession)
	if err != nil || created == nil {
		t.Fatalf("CompleteConnectInputSession = %#v err=%v", created, err)
	}
	if _, err := f.store.CompleteConnectInputSession(f.ctx, tokenHash, pending.ContractHash, now.Add(time.Second), providerSession); !errors.Is(err, ErrConnectSessionUnavailable) {
		t.Fatalf("replayed CompleteConnectInputSession error = %v", err)
	}
	stored, err := f.store.GetConnectSessionByStateHash(f.ctx, providerSession.StateHash)
	if err != nil || stored == nil || stored.EndUserRef != "user_input" {
		t.Fatalf("completed provider session = %#v err=%v", stored, err)
	}
	assertConcurrentConnectInputReplay(t, f, now.Add(time.Second))
}

// assertConcurrentConnectInputReplay proves the conditional UPDATE serializes
// racing browser submissions so exactly one callback session can be inserted.
func assertConcurrentConnectInputReplay(t *testing.T, f connectAuthFixture, now time.Time) {
	t.Helper()
	tokenHash := "concurrent-" + uuid.NewString()
	contractHash := "sha256:" + strings.Repeat("b", 64)
	_, err := f.store.CreateConnectInputSession(f.ctx, ConnectInputSession{
		BucketID: f.bucketA, ServiceID: f.serviceID, AuthType: "oauth", AuthName: "oauth",
		ContractHash: contractHash, EndUserRef: "user_input", TokenHash: tokenHash, CreatedByAppID: f.appID,
		ReturnURL: "https://app.example.com/oauth/done", ResourceInputJSON: []byte(`{"subdomain":"acme"}`),
		RequestedScopes: []string{"read"}, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateConnectInputSession for race: %v", err)
	}
	providerSession := postgresInputCompletionSession(t, f, now)
	results := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, completeErr := f.store.CompleteConnectInputSession(f.ctx, tokenHash, contractHash, now, providerSession)
			results <- completeErr
		}()
	}
	close(start)
	succeeded, unavailable := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrConnectSessionUnavailable) {
			unavailable++
		}
	}
	if succeeded != 1 || unavailable != 1 {
		t.Fatalf("concurrent completion outcomes success=%d unavailable=%d", succeeded, unavailable)
	}
}

// postgresInputCompletionSession builds callback material independently of the
// pending form row so the integration test detects accidental pre-authorisation
// state reuse or unencrypted verifier persistence.
func postgresInputCompletionSession(t *testing.T, f connectAuthFixture, now time.Time) ConnectSession {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "form-pkce-verifier")
	return ConnectSession{
		BucketID: f.bucketA, ServiceID: f.serviceID, ServiceVersionID: fixtureServiceVersionID(t, f), AuthType: "oauth", AuthName: "oauth",
		EndUserRef: "user_input", StateHash: "provider-" + uuid.NewString(), NonceHash: "nonce-hash",
		EncryptedDEK: encrypted.dek, EncryptedPKCEVerifier: encrypted.values[0], CreatedByAppID: f.appID,
		ReturnURL: "https://app.example.com/oauth/done", ResourceInputJSON: []byte(`{"subdomain":"acme"}`),
		RequestedScopes: []string{"read"}, ExpiresAt: now.Add(10 * time.Minute),
	}
}

// containsAuthConnectionClaim reports whether a claimed refresh page contains
// one connection ID without inspecting its private lease token.
func containsAuthConnectionClaim(claims []AuthConnectionRefreshClaim, id uuid.UUID) bool {
	for _, claim := range claims {
		if claim.Connection.ID == id {
			return true
		}
	}
	return false
}

// fixtureServiceVersionID resolves the exact active version seeded by the
// first fixture phase so every persisted callback credential is pinned.
func fixtureServiceVersionID(t *testing.T, f connectAuthFixture) uuid.UUID {
	t.Helper()
	versionID, err := f.store.GetLatestWorkspaceServiceVersionIDByWorkspace(f.ctx, f.serviceID)
	if err != nil {
		t.Fatalf("resolve fixture service version ID: %v", err)
	}
	return versionID
}

type encryptedConnectAuthValues struct {
	dek    string
	values []string
}

func encryptConnectAuthValues(t *testing.T, plaintexts ...string) encryptedConnectAuthValues {
	t.Helper()
	wrappedDEK, dek, err := WrapDEK(connectAuthTestMasterKey)
	if err != nil {
		t.Fatalf("wrap connect auth test DEK: %v", err)
	}
	encrypted := make([]string, 0, len(plaintexts))
	for _, plaintext := range plaintexts {
		ciphertext, err := EncryptWithDEK(dek, plaintext)
		if err != nil {
			t.Fatalf("encrypt connect auth test value: %v", err)
		}
		encrypted = append(encrypted, ciphertext)
	}
	return encryptedConnectAuthValues{dek: wrappedDEK, values: encrypted}
}

func decryptConnectAuthValue(t *testing.T, wrappedDEK, ciphertext string) string {
	t.Helper()
	dek, err := UnwrapDEK(connectAuthTestMasterKey, wrappedDEK)
	if err != nil {
		t.Fatalf("unwrap connect auth test DEK: %v", err)
	}
	plaintext, err := DecryptWithDEK(dek, ciphertext)
	if err != nil {
		t.Fatalf("decrypt connect auth test value: %v", err)
	}
	return plaintext
}
