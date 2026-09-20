package managedauthclient

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// reconciliationVerifier models the Registry's verified installation identity for local broker integration tests.
type reconciliationVerifier struct {
	identity managedauthbroker.EnrollmentIdentity
}

// VerifyTicket binds every test enrollment to the same simulated Engine installation.
func (v reconciliationVerifier) VerifyTicket(context.Context, string) (managedauthbroker.EnrollmentIdentity, error) {
	return v.identity, nil
}

// reconciliationBroker drives real broker rotation while injecting response loss and bounded outages.
type reconciliationBroker struct {
	service       *managedauthbroker.Service
	enrollCalls   atomic.Int32
	refreshCalls  atomic.Int32
	failEnroll    atomic.Bool
	loseRefresh   atomic.Bool
	rejectRefresh atomic.Bool
	failRevoke    atomic.Bool
}

// Enroll counts remote issuance and can simulate an outage before any remote mutation.
func (b *reconciliationBroker) Enroll(ctx context.Context, ticket string) (string, string, int64, error) {
	b.enrollCalls.Add(1)
	// First-attempt failure must be recoverable without restarting the Engine.
	if b.failEnroll.Swap(false) {
		return "", "", 0, errors.New("fixture broker unavailable")
	}
	result, err := b.service.Enroll(ctx, ticket)
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, err
}

// Refresh can lose a committed response, reproducing the remote-success/local-failure recovery boundary.
func (b *reconciliationBroker) Refresh(ctx context.Context, token, ticket string) (string, string, int64, error) {
	b.refreshCalls.Add(1)
	// Authorization policy failures must not trigger automatic re-enrollment.
	if b.rejectRefresh.Load() {
		return "", "", 0, ErrBrokerRejected{StatusCode: 403}
	}
	result, err := b.service.Refresh(ctx, token, ticket)
	// Map the real broker's consumed-token result to its HTTP authentication contract.
	if errors.Is(err, managedauthbroker.ErrInvalidTicket) {
		return "", "", 0, ErrBrokerRejected{StatusCode: 401}
	}
	// Lose the response only after the broker has committed its rotation.
	if err == nil && b.loseRefresh.Swap(false) {
		return "", "", 0, errors.New("fixture response lost after rotation")
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, err
}

// reconciliationFixture keeps real encrypted persistence and remote token rotation while allowing failure injection.
func reconciliationFixture(t *testing.T) (*Service, *reconciliationBroker) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	// Replica and crash-window behavior needs real PostgreSQL transaction semantics.
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `DELETE FROM fused_managed_auth_preferences`)
	require.NoError(t, err)
	account := uuid.New()
	_, err = pool.Exec(t.Context(), `DELETE FROM fused_managed_auth_broker_credential`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_preferences`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_broker_credential`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_installations WHERE registry_account_id = $1`, account)
	})
	brokerService, err := managedauthbroker.NewService(managedauthbroker.NewStore(pool), reconciliationVerifier{identity: managedauthbroker.EnrollmentIdentity{AccountID: account, InstallationID: uuid.New(), ExpiresAt: time.Now().Add(5 * time.Minute)}})
	require.NoError(t, err)
	broker := &reconciliationBroker{service: brokerService}
	service, err := NewService(NewStore(pool), &countingTicketMinter{ticket: "fixture-ticket"}, broker, make([]byte, 32))
	require.NoError(t, err)
	return service, broker
}

// expireReconciliationCredential advances only local expiry, leaving the broker's real rotation rules intact.
func expireReconciliationCredential(t *testing.T, service *Service) Credential {
	t.Helper()
	cred, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.NoError(t, service.store.Save(t.Context(), cred.AccessToken, cred.RefreshToken, time.Now().Add(-time.Minute), cred.RefreshExpiresAt, service.masterKey))
	return cred
}

// TestReplicaReconciliationReusesCredentials proves process restarts and concurrent callers share one durable grant.
func TestReplicaReconciliationReusesCredentials(t *testing.T) {
	service, broker := reconciliationFixture(t)
	otherReplica, err := NewService(NewStore(service.store.pool), service.minter, broker, service.masterKey)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { results <- otherReplica.Enroll(t.Context()) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, broker.enrollCalls.Load())
	require.NoError(t, service.Enroll(t.Context()))
	require.EqualValues(t, 1, broker.enrollCalls.Load(), "restart must reuse persisted authority")
	expireReconciliationCredential(t, service)
	results = make(chan error, 8)
	for range 8 {
		wg.Go(func() { results <- otherReplica.EnsureFresh(t.Context()) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, broker.refreshCalls.Load())
	require.EqualValues(t, 1, broker.enrollCalls.Load())
}

// TestReconciliationRecoversLostResponse proves consumed remote refresh tokens recover without deleting local state.
func TestReconciliationRecoversLostResponse(t *testing.T) {
	service, broker := reconciliationFixture(t)
	broker.failEnroll.Store(true)
	require.Error(t, service.Enroll(t.Context()))
	require.NoError(t, service.Enroll(t.Context()), "an initial outage must be retryable")
	before := expireReconciliationCredential(t, service)
	broker.loseRefresh.Store(true)
	require.Error(t, service.EnsureFresh(t.Context()))
	retained, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.Equal(t, before.RefreshToken, retained.RefreshToken)
	require.NoError(t, service.EnsureFresh(t.Context()), "fresh Registry proof repairs a consumed remote token")
	after, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.NotEqual(t, before.RefreshToken, after.RefreshToken)
	require.Equal(t, StatusReady, service.Status(t.Context()))
	require.EqualValues(t, 3, broker.enrollCalls.Load())
}

// TestReconciliationRetainsCredentialOnPolicyFailure prevents a 403 from becoming an enrollment bypass.
func TestReconciliationRetainsCredentialOnPolicyFailure(t *testing.T) {
	service, broker := reconciliationFixture(t)
	require.NoError(t, service.Enroll(t.Context()))
	before := expireReconciliationCredential(t, service)
	require.Equal(t, StatusTemporarilyDown, service.Status(t.Context()))
	broker.rejectRefresh.Store(true)
	require.Error(t, service.EnsureFresh(t.Context()))
	after, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.Equal(t, before.RefreshToken, after.RefreshToken)
	require.EqualValues(t, 1, broker.enrollCalls.Load())
}

// TestReconciliationRecoversFailedLocalSave reproduces broker success followed by an actual PostgreSQL write failure.
func TestReconciliationRecoversFailedLocalSave(t *testing.T) {
	service, broker := reconciliationFixture(t)
	require.NoError(t, service.Enroll(t.Context()))
	before := expireReconciliationCredential(t, service)
	_, err := service.store.pool.Exec(t.Context(), `ALTER TABLE fused_managed_auth_broker_credential ADD CONSTRAINT fixture_reject_credential_save CHECK (false) NOT VALID`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = service.store.pool.Exec(context.Background(), `ALTER TABLE fused_managed_auth_broker_credential DROP CONSTRAINT IF EXISTS fixture_reject_credential_save`)
	})
	require.Error(t, service.EnsureFresh(t.Context()))
	retained, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.Equal(t, before.RefreshToken, retained.RefreshToken)
	_, err = service.store.pool.Exec(t.Context(), `ALTER TABLE fused_managed_auth_broker_credential DROP CONSTRAINT fixture_reject_credential_save`)
	require.NoError(t, err)
	require.NoError(t, service.EnsureFresh(t.Context()))
	after, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	require.NotEqual(t, before.RefreshToken, after.RefreshToken)
	require.EqualValues(t, 2, broker.enrollCalls.Load())
}

// Revoke exercises the same broker store used by production withdrawal.
func (b *reconciliationBroker) Revoke(ctx context.Context, token string) error {
	// Simulate a remote outage without changing the broker's current grant.
	if b.failRevoke.Load() {
		return errors.New("fixture revoke unavailable")
	}
	return b.service.Revoke(ctx, token)
}

// TestDisableSurvivesOutageAndRestart proves opt-out wins over startup auto-enrollment and pending credentials.
func TestDisableSurvivesOutageAndRestart(t *testing.T) {
	service, broker := reconciliationFixture(t)
	require.NoError(t, service.Enroll(t.Context()))
	before, err := service.store.Get(t.Context(), service.masterKey)
	require.NoError(t, err)
	broker.failRevoke.Store(true)
	require.NoError(t, service.Disable(t.Context()))
	require.Equal(t, StatusDisabled, service.Status(t.Context()))
	_, err = service.AccessToken(t.Context())
	require.ErrorIs(t, err, ErrDisabled)
	enabled, pending, err := service.store.State(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	require.True(t, pending)
	restarted, err := NewService(NewStore(service.store.pool), service.minter, broker, service.masterKey)
	require.NoError(t, err)
	require.Error(t, restarted.reconcile(t.Context(), true))
	require.EqualValues(t, 1, broker.enrollCalls.Load())
	broker.failRevoke.Store(false)
	require.ErrorIs(t, restarted.reconcile(t.Context(), true), ErrDisabled)
	_, err = service.store.Get(t.Context(), service.masterKey)
	require.ErrorIs(t, err, ErrNotEnrolled)
	_, err = broker.service.Refresh(t.Context(), before.RefreshToken, "fresh-ticket")
	require.ErrorIs(t, err, managedauthbroker.ErrInvalidTicket)
	require.NoError(t, restarted.Enroll(t.Context()))
	require.EqualValues(t, 2, broker.enrollCalls.Load())
	require.Equal(t, StatusReady, restarted.Status(t.Context()))
}

// TestDisableBeforeEnrollmentSurvivesStartup proves an early opt-out cannot be overwritten by capability discovery.
func TestDisableBeforeEnrollmentSurvivesStartup(t *testing.T) {
	service, broker := reconciliationFixture(t)
	require.NoError(t, service.Disable(t.Context()))
	require.ErrorIs(t, service.reconcile(t.Context(), true), ErrDisabled)
	require.Zero(t, broker.enrollCalls.Load())
	require.Equal(t, StatusDisabled, service.Status(t.Context()))
}
