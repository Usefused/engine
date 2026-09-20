package api

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRemoteWebhookSourceRequiresExactBucketConnection rejects UUID knowledge without matching bucket and managed-service ownership.
func TestRemoteWebhookSourceRequiresExactBucketConnection(t *testing.T) {
	bucket, connection, service := uuid.New(), uuid.New(), uuid.New()
	r := map[string]webhookResolvedService{"service": {ServiceID: service, Relay: &webhookrelay.Config{Source: &webhookrelay.Source{Bucket: "owner", ConnectionID: connection, RegistrationID: uuid.New()}}}}
	connections := map[uuid.UUID]store.AuthConnection{connection: {BucketID: bucket, ServiceID: service, ManagedAuth: true}}
	buckets := map[string]store.Bucket{"owner": {ID: bucket, Name: "owner"}}
	result, err := matchWebhookSourceBuckets(r, []string{"service"}, connections, buckets)
	require.NoError(t, err)
	require.Equal(t, bucket, result["service"].ID)
	buckets["owner"] = store.Bucket{ID: uuid.New(), Name: "owner"}
	_, err = matchWebhookSourceBuckets(r, []string{"service"}, connections, buckets)
	require.Error(t, err)
	buckets["owner"] = store.Bucket{ID: bucket, Name: "owner"}
	conn := connections[connection]
	conn.ManagedAuth = false
	connections[connection] = conn
	_, err = matchWebhookSourceBuckets(r, []string{"service"}, connections, buckets)
	require.Error(t, err)
}

// TestRemoteWebhookSourceHasNoProviderSecretOrPublicIngress separates delegated trust from the provider's own signing key.
func TestRemoteWebhookSourceHasNoProviderSecretOrPublicIngress(t *testing.T) {
	relay := &webhookrelay.Config{Source: &webhookrelay.Source{Bucket: "owner", ConnectionID: uuid.New(), RegistrationID: uuid.New()}}
	require.Error(t, validateWebhookRelayConfig(webhookConfigServiceDoc{Relay: relay, Secret: "${bucket.owner.secret.signing}"}))
	row, err := prepareRelayAwareWebhookRegistration(webhookResolvedService{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Relay: relay}, webhookConfigDocument{Name: "remote"}, webhookSecretBinding{}, webhookAuthShape{}, "webhook:remote")
	require.NoError(t, err)
	require.Equal(t, "fused_remote", row.AuthType)
	require.Empty(t, row.SecretRef)
	require.Nil(t, row.SecretBucketID)
}
