package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestManagedApplicationPersistence fences hosted forms and preserves exact publication identity through durable refresh claims.
func TestManagedApplicationPersistence(t *testing.T) {
	f := setupConnectAuthStore(t)
	application := uuid.NewString()
	input, err := f.store.CreateConnectInputSession(f.ctx, ConnectInputSession{ID: uuid.New(), BucketID: f.bucketA, ServiceID: f.serviceID, AuthType: "oauth", AuthName: "oauth", ManagedAuth: true, ManagedApplicationID: application, EndUserRef: "managed-user", RequestedScopes: []string{}, TokenHash: uuid.NewString(), ContractHash: "sha256:" + strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Minute)})
	require.NoError(t, err)
	saved, err := f.store.GetActiveConnectInputSessionByTokenHash(f.ctx, input.TokenHash)
	require.NoError(t, err)
	require.Equal(t, application, saved.ManagedApplicationID)
	callback := ConnectSession{ID: input.ID, BucketID: f.bucketA, ServiceID: f.serviceID, ServiceVersionID: fixtureServiceVersionID(t, f), AuthType: "oauth", AuthName: "oauth", ManagedAuth: true, ManagedApplicationID: uuid.NewString(), EndUserRef: input.EndUserRef, RequestedScopes: []string{}, StateHash: uuid.NewString(), RedirectURI: "https://consumer.example/callback", ExpiresAt: input.ExpiresAt}
	_, err = f.store.CompleteConnectInputSession(f.ctx, input.TokenHash, input.ContractHash, time.Now(), callback)
	require.ErrorIs(t, err, ErrConnectSessionUnavailable)
	callback.ManagedApplicationID = application
	completed, err := f.store.CompleteConnectInputSession(f.ctx, input.TokenHash, input.ContractHash, time.Now(), callback)
	require.NoError(t, err)
	require.Equal(t, application, completed.ManagedApplicationID)
	read, err := f.store.GetConnectSessionByStateHash(f.ctx, callback.StateHash)
	require.NoError(t, err)
	require.Equal(t, application, read.ManagedApplicationID)
	conn := upsertRefreshLeaseConnection(t, f, input.EndUserRef, callback.ServiceVersionID, time.Now().Add(-time.Minute))
	conn.ManagedAuth, conn.ManagedApplicationID = true, application
	conn, err = f.store.UpsertAuthConnection(f.ctx, *conn)
	require.NoError(t, err)
	require.Equal(t, application, conn.ManagedApplicationID)
	claim, err := f.store.(AuthConnectionRefreshStore).TryClaimAuthConnectionRefresh(f.ctx, conn.ID, time.Now(), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, application, claim.Connection.ManagedApplicationID)
}
