package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestAuthRefreshCoordinatorPreservesOmittedRefreshToken verifies providers
// that rotate access only cannot erase the still-valid encrypted refresh grant.
func TestAuthRefreshCoordinatorPreservesOmittedRefreshToken(t *testing.T) {
	fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), time.Minute)
	client := authRefreshResponseClient(http.StatusOK, `{"access_token":"new-access","expires_in":3600}`)
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))

	refreshed, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err != nil {
		t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
	}
	if got := testDecryptAuthConnectionToken(t, fixture.masterKey, refreshed.EncryptedDEK, refreshed.EncryptedAccessToken); got != "new-access" {
		t.Fatalf("access token = %q, want new-access", got)
	}
	if got := testDecryptAuthConnectionToken(t, fixture.masterKey, refreshed.EncryptedDEK, refreshed.EncryptedRefreshToken); got != "old-refresh" {
		t.Fatalf("refresh token = %q, want preserved old-refresh", got)
	}
}

// TestAuthRefreshCoordinatorClearsOldExpiryForRotatedTokenWithoutTTL proves
// absolute metadata from an old refresh token cannot poison its replacement.
func TestAuthRefreshCoordinatorClearsOldExpiryForRotatedTokenWithoutTTL(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	oldRefreshExpiry := now.Add(30 * time.Minute)
	fixture.connection.RefreshTokenExpiresAt = &oldRefreshExpiry
	fixture.db.authConnection.RefreshTokenExpiresAt = &oldRefreshExpiry
	client := authRefreshResponseClient(http.StatusOK, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))

	refreshed, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err != nil {
		t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
	}
	if refreshed.RefreshTokenExpiresAt != nil {
		t.Fatalf("rotated token retained old expiry: %v", refreshed.RefreshTokenExpiresAt)
	}
}

// TestAuthRefreshCoordinatorPreservesExpiryForEchoedRefreshToken proves a
// provider echo is not mistaken for rotation when it omits TTL metadata.
func TestAuthRefreshCoordinatorPreservesExpiryForEchoedRefreshToken(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	oldRefreshExpiry := now.Add(30 * time.Minute)
	fixture.connection.RefreshTokenExpiresAt = &oldRefreshExpiry
	fixture.db.authConnection.RefreshTokenExpiresAt = &oldRefreshExpiry
	client := authRefreshResponseClient(http.StatusOK, `{"access_token":"new-access","refresh_token":"old-refresh","expires_in":3600}`)
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))

	refreshed, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err != nil {
		t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
	}
	if refreshed.RefreshTokenExpiresAt == nil || !refreshed.RefreshTokenExpiresAt.Equal(oldRefreshExpiry) {
		t.Fatalf("echoed refresh token expiry = %v, want %v", refreshed.RefreshTokenExpiresAt, oldRefreshExpiry)
	}
}

// TestAuthRefreshCoordinatorRejectsExpiringClaimBeforeProviderIO proves a
// worker cannot start a rotating-token exchange without completion headroom.
func TestAuthRefreshCoordinatorRejectsExpiringClaimBeforeProviderIO(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	var exchanges atomic.Int32
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"must-not-be-used"}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return now }), WithAuthRefreshHTTPClient(client))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, now, now.Add(4*time.Second))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if !errors.Is(err, ErrAuthRefreshFailed) || result.Outcome != AuthRefreshOutcomeTransientFailure || result.FailureCode != "refresh_lease_expired" {
		t.Fatalf("RefreshClaimedConnection() result=%#v error=%v", result, err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("expiring claim made %d provider calls", exchanges.Load())
	}
}

// TestAuthRefreshCoordinatorPreservesLeaseReserveForCompletion proves a slow
// CAS can finish after the provider-work deadline while the lease remains live.
func TestAuthRefreshCoordinatorPreservesLeaseReserveForCompletion(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	fixture.db.refreshCompleteDelay = 2 * time.Second
	client := &http.Client{Timeout: 50 * time.Millisecond, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"reserved-access","refresh_token":"reserved-refresh","expires_in":3600}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return now }), WithAuthRefreshHTTPClient(client))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, now, now.Add(6500*time.Millisecond))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if err != nil || result.Outcome != AuthRefreshOutcomeRefreshed {
		t.Fatalf("reserved completion result=%#v error=%v", result, err)
	}
}

// TestAuthRefreshCoordinatorUsesCompletionClockForLeaseCAS proves a provider
// response that arrives after lease expiry cannot persist rotated material.
func TestAuthRefreshCoordinatorUsesCompletionClockForLeaseCAS(t *testing.T) {
	startedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := startedAt
	fixture := newAuthRefreshCoordinatorFixture(t, startedAt, time.Minute)
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		clock = startedAt.Add(2 * time.Minute)
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"late-access","refresh_token":"late-refresh","expires_in":3600}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return clock }), WithAuthRefreshHTTPClient(client))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, startedAt, startedAt.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if !errors.Is(err, ErrAuthRefreshFailed) || result.Outcome != AuthRefreshOutcomeTransientFailure {
		t.Fatalf("late refresh result=%#v error=%v", result, err)
	}
	stored, loadErr := fixture.db.GetAuthConnectionByID(context.Background(), fixture.connection.ID)
	if loadErr != nil || stored == nil {
		t.Fatalf("GetAuthConnectionByID() connection=%v error=%v", stored != nil, loadErr)
	}
	if got := testDecryptAuthConnectionToken(t, fixture.masterKey, stored.EncryptedDEK, stored.EncryptedAccessToken); got != "old-access" {
		t.Fatalf("late provider response overwrote access token")
	}
}

// TestAuthConnectionNeedsRefreshUsesEarlierProviderExpiry proves foreground
// fallback watches refresh-token TTL even when the access token is long-lived.
func TestAuthConnectionNeedsRefreshUsesEarlierProviderExpiry(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	accessExpiry := now.Add(24 * time.Hour)
	refreshExpiry := now.Add(4 * time.Minute)
	connection := &store.AuthConnection{
		EncryptedRefreshToken: "encrypted", ExpiresAt: &accessExpiry, RefreshTokenExpiresAt: &refreshExpiry,
	}
	if !authConnectionNeedsRefresh(connection, now) {
		t.Fatal("earlier refresh-token expiry was not considered due")
	}
	connection.EncryptedRefreshToken = ""
	if authConnectionNeedsRefresh(connection, now) {
		t.Fatal("missing refresh material made a valid access token refresh early")
	}
}

// TestAuthRefreshCoordinatorLetsAccessOnlyGrantReachExpiry proves missing
// refresh material does not force reconnect while the access token is valid.
func TestAuthRefreshCoordinatorLetsAccessOnlyGrantReachExpiry(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	fixture.connection.EncryptedRefreshToken = ""
	fixture.db.authConnection.EncryptedRefreshToken = ""
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey, WithAuthRefreshClock(func() time.Time { return now }))

	connection, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err != nil || connection == nil {
		t.Fatalf("valid access-only result connection=%v error=%v", connection != nil, err)
	}
	if fixture.db.refreshClaimAttempts != 0 {
		t.Fatalf("valid access-only grant made %d refresh claims", fixture.db.refreshClaimAttempts)
	}

	expiredNow := now.Add(2 * time.Minute)
	coordinator = NewAuthRefreshCoordinator(fixture.db, fixture.masterKey, WithAuthRefreshClock(func() time.Time { return expiredNow }))
	connection, err = coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err == nil || connection != nil || fixture.db.authConnection.RefreshState != reconnectRequiredCode {
		t.Fatalf("expired access-only result connection=%v error=%v state=%q", connection != nil, err, fixture.db.authConnection.RefreshState)
	}
}

// TestAuthRefreshCoordinatorRechecksExpiryAfterFailedExchange proves request
// fallback never dispatches a token that expired while refresh was in flight.
func TestAuthRefreshCoordinatorRechecksExpiryAfterFailedExchange(t *testing.T) {
	startedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := startedAt
	fixture := newAuthRefreshCoordinatorFixture(t, startedAt, 20*time.Second)
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		clock = startedAt.Add(25 * time.Second)
		return authRefreshHTTPResponse(http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return clock }), WithAuthRefreshHTTPClient(client))

	connection, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err == nil || connection != nil {
		t.Fatalf("post-attempt expired result connection=%v error=%v", connection != nil, err)
	}
}

// TestAuthRefreshCoordinatorUsesVersionPinnedJSONOIDCContract proves the
// coordinator honors JSON/OIDC metadata loaded from the immutable snapshot.
func TestAuthRefreshCoordinatorUsesVersionPinnedJSONOIDCContract(t *testing.T) {
	fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), time.Minute)
	auth := refreshServiceMetadata("bearerAuth").AuthConfigs[0]
	auth.Type = "openIdConnect"
	fixture.connection.AuthType = "oidc"
	fixture.db.authConnection.AuthType = "oidc"
	fixture.db.connectConfig.AuthType = "openIdConnect"
	auth.TokenRequestMediaType = fusedobject.TokenRequestMediaTypeJSON
	fixture.db.serviceMetadata = &fusedobject.ServiceMetadata{AuthConfigs: fusedobject.AuthConfigs{auth}}
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Content-Type") != string(fusedobject.TokenRequestMediaTypeJSON) {
			t.Fatalf("Content-Type = %q, want JSON", request.Header.Get("Content-Type"))
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode JSON refresh request: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
			t.Fatalf("unexpected JSON refresh request: %#v", body)
		}
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"oidc-access","refresh_token":"oidc-refresh","expires_in":3600}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))

	refreshed, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
	if err != nil {
		t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
	}
	if got := testDecryptAuthConnectionToken(t, fixture.masterKey, refreshed.EncryptedDEK, refreshed.EncryptedAccessToken); got != "oidc-access" {
		t.Fatalf("OIDC access token = %q, want oidc-access (failure=%q)", got, fixture.db.authConnection.LastFailureCode)
	}
}

// TestAuthRefreshCoordinatorKeepsConsentedVersionAcrossSDKUpgrade proves a v2
// request refreshes a connection through its persisted v1 auth contract.
func TestAuthRefreshCoordinatorKeepsConsentedVersionAcrossSDKUpgrade(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	consentedVersionID := fixture.versionID
	requestVersionID := uuid.New()
	client := authRefreshResponseClient(http.StatusOK, `{"access_token":"v1-contract-access","refresh_token":"v1-contract-refresh","expires_in":3600}`)
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return now }), WithAuthRefreshHTTPClient(client))

	connection, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, requestVersionID)
	if err != nil {
		t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
	}
	if connection.ServiceVersionID != consentedVersionID {
		t.Fatalf("refreshed version = %s, want consented v1 %s", connection.ServiceVersionID, consentedVersionID)
	}
	if got := testDecryptAuthConnectionToken(t, fixture.masterKey, connection.EncryptedDEK, connection.EncryptedAccessToken); got != "v1-contract-access" {
		t.Fatalf("access token = %q, want v1-contract-access", got)
	}
}

// TestAuthRefreshCoordinatorContractSelectionFailsClosed proves an auth-name
// mismatch cannot fall back to another OAuth definition or make an HTTP call.
func TestAuthRefreshCoordinatorContractSelectionFailsClosed(t *testing.T) {
	fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), time.Minute)
	fixture.db.serviceMetadata = refreshServiceMetadata("differentAuth")
	var exchanges atomic.Int32
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"must-not-be-used"}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if !errors.Is(err, ErrAuthRefreshContractUnavailable) || result.Outcome != AuthRefreshOutcomeContractUnavailable {
		t.Fatalf("RefreshClaimedConnection() result=%#v error=%v", result, err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("mismatched auth contract made %d provider calls", exchanges.Load())
	}
}

// TestAuthRefreshCoordinatorRegistrationTypeMismatchFailsClosed proves a
// same-name OAuth/OIDC registration mismatch never reaches the provider.
func TestAuthRefreshCoordinatorRegistrationTypeMismatchFailsClosed(t *testing.T) {
	fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), time.Minute)
	fixture.db.connectConfig.AuthType = "oidc"
	var exchanges atomic.Int32
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"must-not-be-used"}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if !errors.Is(err, ErrAuthRefreshContractUnavailable) || result.Outcome != AuthRefreshOutcomeContractUnavailable {
		t.Fatalf("RefreshClaimedConnection() result=%#v error=%v", result, err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("mismatched registration type made %d provider calls", exchanges.Load())
	}
}

// TestAuthRefreshCoordinatorEmitsBoundedDecisionSpan proves every claimed
// branch ends with worker/request trigger, bounded outcome, and safe codes only.
func TestAuthRefreshCoordinatorEmitsBoundedDecisionSpan(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previousProvider)

	fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), time.Minute)
	fixture.db.serviceMetadata = refreshServiceMetadata("differentAuth")
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey, WithAuthRefreshClock(func() time.Time { return fixture.now }))
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}
	if _, err := coordinator.RefreshClaimedConnection(context.Background(), *claim); !errors.Is(err, ErrAuthRefreshContractUnavailable) {
		t.Fatalf("RefreshClaimedConnection() error = %v", err)
	}

	span := findAuthRefreshTestSpan(t, recorder.Ended())
	attributes := testSpanStringAttributes(span.Attributes())
	if attributes["auth.refresh.trigger"] != "worker" || attributes["auth.refresh.outcome"] != "contract_unavailable" || attributes["auth.refresh.failure_code"] != "refresh_contract_unavailable" {
		t.Fatalf("unexpected refresh decision attributes: %#v", attributes)
	}
	if span.Status().Code != codes.Error {
		t.Fatalf("refresh span status = %v, want error", span.Status().Code)
	}
	for _, forbidden := range []string{"auth_name", "end_user_ref", "token_url", "refresh_token", "access_token"} {
		if _, exists := attributes[forbidden]; exists {
			t.Fatalf("refresh span exposed forbidden attribute %q", forbidden)
		}
	}
}

// TestAuthRefreshCoordinatorEmitsRequestSkipDecisionSpans covers request paths
// that do not own a lease and therefore cannot rely on a claimed-exchange span.
func TestAuthRefreshCoordinatorEmitsRequestSkipDecisionSpans(t *testing.T) {
	tests := []struct {
		name          string
		expiresIn     time.Duration
		contended     bool
		retryDeferred bool
		claimMiss     bool
		wantResult    AuthRefreshOutcome
		wantCode      string
		wantStatus    codes.Code
	}{
		{name: "not due", expiresIn: time.Hour, wantResult: AuthRefreshOutcomeNotDue, wantStatus: codes.Ok},
		{name: "retry deferred", expiresIn: time.Minute, retryDeferred: true, wantResult: AuthRefreshOutcomeNotDue, wantCode: "refresh_retry_deferred", wantStatus: codes.Ok},
		{name: "lease contended", expiresIn: time.Minute, contended: true, wantResult: AuthRefreshOutcomeLeaseContended, wantCode: "refresh_lease_contended", wantStatus: codes.Ok},
		{name: "claim unavailable", expiresIn: time.Minute, claimMiss: true, wantResult: AuthRefreshOutcomeTransientFailure, wantCode: "refresh_claim_unavailable", wantStatus: codes.Error},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			previousProvider := otel.GetTracerProvider()
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			otel.SetTracerProvider(provider)
			defer otel.SetTracerProvider(previousProvider)

			now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			fixture := newAuthRefreshCoordinatorFixture(t, now, test.expiresIn)
			if test.contended {
				fixture.db.refreshLeaseToken = uuid.New()
				fixture.db.authConnection.LastRefreshAttemptAt = &now
			}
			if test.retryDeferred {
				retryNotBefore := now.Add(time.Minute)
				fixture.connection.RefreshRetryNotBefore = &retryNotBefore
				fixture.db.authConnection.RefreshRetryNotBefore = &retryNotBefore
			}
			fixture.db.forceRefreshClaimMiss = test.claimMiss
			coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
				WithAuthRefreshClock(func() time.Time { return now }),
				withAuthRefreshForegroundTiming(time.Second, time.Millisecond, time.Millisecond))
			if _, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID); err != nil {
				t.Fatalf("ensureForegroundConnectionFresh() error = %v", err)
			}

			span := findAuthRefreshTestSpan(t, recorder.Ended())
			attributes := testSpanStringAttributes(span.Attributes())
			if attributes["auth.refresh.trigger"] != "request" || attributes["auth.refresh.outcome"] != string(test.wantResult) || attributes["auth.refresh.failure_code"] != test.wantCode {
				t.Fatalf("unexpected request decision attributes: %#v", attributes)
			}
			if span.Status().Code != test.wantStatus {
				t.Fatalf("request skip span status = %v, want %v", span.Status().Code, test.wantStatus)
			}
		})
	}
}

// TestAuthRefreshCoordinatorClassifiesLostClaimWithoutDoubleSpan proves a late
// exchange observes the winner and reports contention on its existing span.
func TestAuthRefreshCoordinatorClassifiesLostClaimWithoutDoubleSpan(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previousProvider)

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fixture := newAuthRefreshCoordinatorFixture(t, now, time.Minute)
	claim, err := fixture.db.TryClaimAuthConnectionRefresh(context.Background(), fixture.connection.ID, fixture.versionID, now, now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("TryClaimAuthConnectionRefresh() claim=%v error=%v", claim != nil, err)
	}
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		fixture.db.refreshMu.Lock()
		winner := *fixture.db.authConnection
		winnerExpiresAt := now.Add(time.Hour)
		winner.ExpiresAt = &winnerExpiresAt
		fixture.db.authConnection = &winner
		fixture.db.refreshLeaseToken = uuid.New()
		fixture.db.refreshMu.Unlock()
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"late-access","refresh_token":"late-refresh","expires_in":3600}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
		WithAuthRefreshClock(func() time.Time { return now }), WithAuthRefreshHTTPClient(client))

	result, err := coordinator.RefreshClaimedConnection(context.Background(), *claim)
	if err != nil || result.Outcome != AuthRefreshOutcomeLeaseContended || result.FailureCode != "refresh_claim_lost" {
		t.Fatalf("RefreshClaimedConnection() result=%#v error=%v", result, err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("refresh spans = %d, want exactly one claimed span", len(spans))
	}
	attributes := testSpanStringAttributes(findAuthRefreshTestSpan(t, spans).Attributes())
	if attributes["auth.refresh.trigger"] != "worker" || attributes["auth.refresh.outcome"] != "lease_contended" {
		t.Fatalf("unexpected lost-claim attributes: %#v", attributes)
	}
}

// TestAuthRefreshCoordinatorTransientFailureHonorsAccessExpiry proves request
// fallback preserves a live token but never dispatches an expired credential.
func TestAuthRefreshCoordinatorTransientFailureHonorsAccessExpiry(t *testing.T) {
	tests := []struct {
		name       string
		expiresIn  time.Duration
		wantUsable bool
	}{
		{name: "still valid", expiresIn: time.Minute, wantUsable: true},
		{name: "expired", expiresIn: -time.Minute, wantUsable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthRefreshCoordinatorFixture(t, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), test.expiresIn)
			client := authRefreshResponseClient(http.StatusServiceUnavailable, `{"error":"temporarily_unavailable","detail":"must-not-leak"}`)
			coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
				WithAuthRefreshClock(func() time.Time { return fixture.now }), WithAuthRefreshHTTPClient(client))

			usable, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
			if test.wantUsable && (err != nil || usable == nil) {
				t.Fatalf("live token blocked: usable=%v err=%v", usable != nil, err)
			}
			if !test.wantUsable && (usable != nil || !errors.Is(err, ErrAuthRefreshFailed)) {
				t.Fatalf("expired token result: usable=%v err=%v, want sanitized failure", usable != nil, err)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-leak") {
				t.Fatalf("provider body leaked through error: %v", err)
			}
		})
	}
}

// TestAuthRefreshCoordinatorDeferredRetrySkipsProviderForLiveAndExpiredAccess
// proves request traffic honors persisted backoff without polling or HTTP I/O.
func TestAuthRefreshCoordinatorDeferredRetrySkipsProviderForLiveAndExpiredAccess(t *testing.T) {
	tests := []struct {
		name       string
		expiresIn  time.Duration
		wantAccess string
		wantErr    bool
	}{
		{name: "live token is returned immediately", expiresIn: time.Minute, wantAccess: "old-access"},
		{name: "expired token fails immediately", expiresIn: -time.Minute, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			fixture := newAuthRefreshCoordinatorFixture(t, now, test.expiresIn)
			retryNotBefore := now.Add(10 * time.Minute)
			fixture.connection.RefreshRetryNotBefore = &retryNotBefore
			fixture.db.authConnection.RefreshRetryNotBefore = &retryNotBefore
			var exchanges atomic.Int32
			client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				exchanges.Add(1)
				return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
			})}
			coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey,
				WithAuthRefreshClock(func() time.Time { return now }), WithAuthRefreshHTTPClient(client),
				withAuthRefreshForegroundTiming(time.Second, time.Millisecond, time.Millisecond))

			connection, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
			if test.wantErr {
				if connection != nil || !errors.Is(err, ErrAuthRefreshFailed) {
					t.Fatalf("expired deferred result: connection=%v error=%v", connection != nil, err)
				}
			} else if err != nil || connection == nil {
				t.Fatalf("live deferred result: connection=%v error=%v", connection != nil, err)
			}
			if connection != nil && testDecryptAuthConnectionToken(t, fixture.masterKey, connection.EncryptedDEK, connection.EncryptedAccessToken) != test.wantAccess {
				got := testDecryptAuthConnectionToken(t, fixture.masterKey, connection.EncryptedDEK, connection.EncryptedAccessToken)
				t.Fatalf("access token = %q, want %q", got, test.wantAccess)
			}
			if fixture.db.refreshClaimAttempts != 0 || exchanges.Load() != 0 {
				t.Fatalf("claims/exchanges = %d/%d, want 0/0", fixture.db.refreshClaimAttempts, exchanges.Load())
			}
		})
	}
}

// TestAuthRefreshCoordinatorForegroundContentionExchangesExactlyOnce proves a
// request waiting behind another lease reuses the winner's rotated token.
func TestAuthRefreshCoordinatorForegroundContentionExchangesExactlyOnce(t *testing.T) {
	fixture := newAuthRefreshCoordinatorFixture(t, time.Now().UTC(), time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	var exchanges atomic.Int32
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if exchanges.Add(1) == 1 {
			close(started)
		}
		<-release
		return authRefreshHTTPResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})}
	coordinator := NewAuthRefreshCoordinator(fixture.db, fixture.masterKey, WithAuthRefreshHTTPClient(client),
		withAuthRefreshForegroundTiming(10*time.Second, time.Second, time.Millisecond))

	type refreshCall struct {
		connection *store.AuthConnection
		err        error
	}
	results := make(chan refreshCall, 2)
	// call models one SDK request resolving the same due connected account.
	call := func() {
		connection, err := coordinator.ensureForegroundConnectionFresh(context.Background(), &fixture.connection, fixture.versionID)
		results <- refreshCall{connection: connection, err: err}
	}
	go call()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not reach provider")
	}
	go call()
	// Why: give the second request time to observe the held lease before the
	// provider response releases it; correctness still comes from store CAS.
	time.Sleep(10 * time.Millisecond)
	close(release)

	for range 2 {
		result := <-results
		if result.err != nil || result.connection == nil {
			t.Fatalf("contended refresh result: connection=%v err=%v", result.connection != nil, result.err)
		}
		if got := testDecryptAuthConnectionToken(t, fixture.masterKey, result.connection.EncryptedDEK, result.connection.EncryptedAccessToken); got != "new-access" {
			t.Fatalf("contended access token = %q, want new-access", got)
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want exactly 1", got)
	}
}

// TestProviderAuthorizationDiagnosticDoesNotRefreshOrReplay locks the decision
// that an unexpected provider 401 is recorded once and never invokes refresh.
func TestProviderAuthorizationDiagnosticDoesNotRefreshOrReplay(t *testing.T) {
	connectionID := uuid.New()
	mockStore := &resolverMockStore{}
	resolver := &secretResolver{db: mockStore}
	recorded, err := resolver.recordConnectedAuthFailure(context.Background(), map[string]any{
		"fused_connection_id": connectionID.String(),
	}, "provider_unauthorized")
	if err != nil || !recorded {
		t.Fatalf("recordConnectedAuthFailure() recorded=%v error=%v", recorded, err)
	}
	if mockStore.refreshLeaseToken != uuid.Nil {
		t.Fatalf("provider diagnostic unexpectedly acquired refresh lease %s", mockStore.refreshLeaseToken)
	}
	if mockStore.failureConnectionID != connectionID || mockStore.failureCode != "provider_unauthorized" {
		t.Fatalf("unexpected diagnostic id=%s code=%q", mockStore.failureConnectionID, mockStore.failureCode)
	}
}

// authRefreshCoordinatorFixture holds encrypted storage and immutable identity
// shared by coordinator tests without exposing plaintext through production APIs.
type authRefreshCoordinatorFixture struct {
	db         *resolverMockStore
	masterKey  []byte
	connection store.AuthConnection
	versionID  uuid.UUID
	now        time.Time
}

// newAuthRefreshCoordinatorFixture creates one due encrypted OAuth connection
// with a version-pinned contract and bucket client registration.
func newAuthRefreshCoordinatorFixture(t *testing.T, now time.Time, expiresIn time.Duration) authRefreshCoordinatorFixture {
	t.Helper()
	bucketID, serviceID, versionID := uuid.New(), uuid.New(), uuid.New()
	masterKey := []byte("12345678901234567890123456789012")
	connection := encryptedAuthConnection(t, masterKey, bucketID, serviceID, "user-1", "old-access", "old-refresh", now.Add(expiresIn))
	connection.ServiceVersionID = versionID
	config := encryptedResolverConnectConfig(t, masterKey, bucketID, serviceID)
	db := &resolverMockStore{authConnection: &connection, connectConfig: &config, serviceMetadata: refreshServiceMetadata("bearerAuth")}
	return authRefreshCoordinatorFixture{db: db, masterKey: masterKey, connection: connection, versionID: versionID, now: now}
}

// authRefreshResponseClient returns a deterministic token endpoint client.
func authRefreshResponseClient(status int, body string) *http.Client {
	return &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return authRefreshHTTPResponse(status, body), nil
	})}
}

// authRefreshHTTPResponse builds the minimal JSON HTTP response token parsing requires.
func authRefreshHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// findAuthRefreshTestSpan selects the coordinator span from any nested token spans.
func findAuthRefreshTestSpan(t *testing.T, spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == "engine.auth_connection.refresh" {
			return span
		}
	}
	t.Fatal("engine.auth_connection.refresh span was not ended")
	return nil
}

// testSpanStringAttributes projects string OTEL values for concise assertions.
func testSpanStringAttributes(attributes []attribute.KeyValue) map[string]string {
	result := make(map[string]string, len(attributes))
	for _, item := range attributes {
		if item.Value.Type() == attribute.STRING {
			result[string(item.Key)] = item.Value.AsString()
		}
	}
	return result
}
