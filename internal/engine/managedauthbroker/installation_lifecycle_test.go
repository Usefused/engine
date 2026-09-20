package managedauthbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// installationTestStore isolates grant ownership while exercising the real schema and PostgreSQL constraints.
func installationTestStore(t *testing.T) (*Store, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	// Integration coverage is opt-in when a disposable PostgreSQL database is available.
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	accountID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_installations WHERE registry_account_id = $1`, accountID)
	})
	return NewStore(pool), pool, accountID
}

// enrollTestInstallation gives each simulated Engine its own Registry-verified identity under the same license account.
func enrollTestInstallation(t *testing.T, store *Store, accountID uuid.UUID) (*Service, TokenResponse) {
	t.Helper()
	service, err := NewService(store, fakeTicketVerifier{accountID: accountID, installationID: uuid.New()})
	require.NoError(t, err)
	tokens, err := service.Enroll(t.Context(), "fixture-ticket")
	require.NoError(t, err)
	return service, tokens
}

// TestInstallationIsolation proves enrollment, repair, and rotation never revoke a sibling Engine's grant.
func TestInstallationIsolation(t *testing.T) {
	store, _, accountID := installationTestStore(t)
	serviceA, firstA := enrollTestInstallation(t, store, accountID)
	_, firstB := enrollTestInstallation(t, store, accountID)
	rowA, err := store.GetByAccessToken(t.Context(), hashCredential(firstA.AccessToken))
	require.NoError(t, err)
	rowB, err := store.GetByAccessToken(t.Context(), hashCredential(firstB.AccessToken))
	require.NoError(t, err)
	require.NotEqual(t, rowA.EngineInstallationID, rowB.EngineInstallationID)
	secondA, err := serviceA.Enroll(t.Context(), "new-fixture-ticket")
	require.NoError(t, err)
	_, err = store.GetByAccessToken(t.Context(), hashCredential(firstA.AccessToken))
	require.ErrorIs(t, err, ErrTokenInvalid)
	_, err = serviceA.Refresh(t.Context(), firstA.RefreshToken, "fresh-ticket")
	require.ErrorIs(t, err, ErrInvalidTicket)
	rotated, err := serviceA.Refresh(t.Context(), secondA.RefreshToken, "fresh-ticket")
	require.NoError(t, err)
	_, err = store.GetByAccessToken(t.Context(), hashCredential(rotated.AccessToken))
	require.NoError(t, err)
	_, err = store.GetByAccessToken(t.Context(), hashCredential(firstB.AccessToken))
	require.NoError(t, err)
}

// TestFailedRotationPreservesCredential forces a real unique-index failure during replacement.
func TestFailedRotationPreservesCredential(t *testing.T) {
	store, _, accountID := installationTestStore(t)
	serviceA, firstA := enrollTestInstallation(t, store, accountID)
	_, firstB := enrollTestInstallation(t, store, accountID)
	err := store.RotateRefreshToken(t.Context(), hashCredential(firstA.RefreshToken), hashCredential(firstB.AccessToken), "unused-refresh-hash", EnrollmentIdentity{AccountID: accountID, InstallationID: serviceA.verifier.(fakeTicketVerifier).installationID}, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	require.Error(t, err)
	_, err = store.GetByAccessToken(t.Context(), hashCredential(firstA.AccessToken))
	require.NoError(t, err)
	_, err = serviceA.Refresh(t.Context(), firstA.RefreshToken, "fresh-ticket")
	require.NoError(t, err, "failed replacement must leave the original refresh token usable")
}

// TestConcurrentBrokerRefreshHasOneWinner exercises PostgreSQL's compare-and-swap under actual contention.
func TestConcurrentBrokerRefreshHasOneWinner(t *testing.T) {
	store, _, accountID := installationTestStore(t)
	service, first := enrollTestInstallation(t, store, accountID)
	var successes atomic.Int32
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := service.Refresh(t.Context(), first.RefreshToken, "fresh-ticket")
			// Only one request may consume the presented token; losers must not affect its replacement.
			if err == nil {
				successes.Add(1)
			} else {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.ErrorIs(t, err, ErrInvalidTicket)
	}
	require.EqualValues(t, 1, successes.Load())
}

// TestRegistryVerifierRejectsAccountOnlyIdentity prevents silent compatibility fallback to account-wide grants.
func TestRegistryVerifierRejectsAccountOnlyIdentity(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": uuid.NewString()})
	}))
	defer fixture.Close()
	_, err := NewRegistryTicketVerifier(fixture.URL, fixture.Client()).VerifyTicket(t.Context(), "fixture-ticket")
	require.ErrorIs(t, err, ErrInvalidTicket)
}

// TestLegacyAccountGrantMigration revokes only grants whose missing installation identity cannot be recovered safely.
func TestLegacyAccountGrantMigration(t *testing.T) {
	store, pool, accountID := installationTestStore(t)
	_, current := enrollTestInstallation(t, store, accountID)
	_, err := pool.Exec(t.Context(), `INSERT INTO fused_managed_auth_installations
 (registry_account_id, token_family_id, access_token_hash, refresh_token_hash, scope, access_expires_at, refresh_expires_at)
 VALUES ($1,$2,$3,$4,$5,NOW()+INTERVAL '1 hour',NOW()+INTERVAL '1 day')`, accountID, uuid.New(), uuid.NewString(), uuid.NewString(), managedAuthScope)
	require.NoError(t, err)
	// Re-running real startup migrations must revoke legacy rows and preserve installation-scoped rows.
	reopened, err := db.InitEnginePostgres(t.Context(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	reopened.Close()
	var legacyLive int
	err = pool.QueryRow(t.Context(), `SELECT count(*) FROM fused_managed_auth_installations WHERE registry_account_id=$1 AND engine_installation_id IS NULL AND revoked_at IS NULL`, accountID).Scan(&legacyLive)
	require.NoError(t, err)
	require.Zero(t, legacyLive)
	_, err = store.GetByAccessToken(t.Context(), hashCredential(current.AccessToken))
	require.NoError(t, err)
}

// fixedApprovalVerifier models a Registry approval deadline that cannot be renewed by delaying redemption.
type fixedApprovalVerifier struct{ identity EnrollmentIdentity }

// VerifyTicket returns a fixed proof so expiry and identity binding can be exercised independently of a clock sleep.
func (v fixedApprovalVerifier) VerifyTicket(context.Context, string) (EnrollmentIdentity, error) {
	return v.identity, nil
}

// TestApprovalDeadlineAndRevocation binds grants to current Registry approval and isolates explicit withdrawal.
func TestApprovalDeadlineAndRevocation(t *testing.T) {
	store, _, accountID := installationTestStore(t)
	identity := EnrollmentIdentity{AccountID: accountID, InstallationID: uuid.New(), ExpiresAt: time.Now().Add(30 * time.Second)}
	service, err := NewService(store, fixedApprovalVerifier{identity: identity})
	require.NoError(t, err)
	grant, err := service.Enroll(t.Context(), "approved-ticket")
	require.NoError(t, err)
	require.LessOrEqual(t, grant.ExpiresIn, int64(30))
	row, err := store.GetByAccessToken(t.Context(), hashCredential(grant.AccessToken))
	require.NoError(t, err)
	require.WithinDuration(t, identity.ExpiresAt, row.AccessExpiresAt, time.Millisecond)
	sibling, siblingGrant := enrollTestInstallation(t, store, accountID)
	_, err = sibling.Refresh(t.Context(), grant.RefreshToken, "sibling-ticket")
	require.ErrorIs(t, err, ErrInvalidTicket, "a sibling's Registry proof cannot renew this installation")
	require.NoError(t, service.Revoke(t.Context(), grant.RefreshToken))
	require.NoError(t, service.Revoke(t.Context(), grant.RefreshToken), "revocation acknowledgements can safely be retried")
	_, err = store.GetByAccessToken(t.Context(), hashCredential(grant.AccessToken))
	require.ErrorIs(t, err, ErrTokenInvalid)
	_, err = sibling.Refresh(t.Context(), siblingGrant.RefreshToken, "fresh-ticket")
	require.NoError(t, err)
	identity.ExpiresAt = time.Now().Add(-time.Second)
	service.verifier = fixedApprovalVerifier{identity: identity}
	_, err = service.Enroll(t.Context(), "expired-approval")
	require.ErrorIs(t, err, ErrInvalidTicket)
	_, err = service.Refresh(t.Context(), grant.RefreshToken, "expired-approval")
	require.ErrorIs(t, err, ErrInvalidTicket)
}
