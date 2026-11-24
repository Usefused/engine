package api

import (
	"context"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
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
	row, err := prepareRelayAwareWebhookRegistration(context.Background(), webhookResolvedService{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), Relay: relay}, webhookConfigDocument{Name: "remote"}, webhookSecretBinding{}, webhookAuthShape{}, "webhook:remote")
	require.NoError(t, err)
	require.Equal(t, "fused_remote", row.AuthType)
	require.Empty(t, row.SecretRef)
	require.Nil(t, row.SecretBucketID)
}

// TestWebhookRegistrationUsesEnginePublicURLWhenCallbackBaseURLOmitted verifies Engine settings supply callback URLs without per-webhook user input.
func TestWebhookRegistrationUsesEnginePublicURLWhenCallbackBaseURLOmitted(t *testing.T) {
	ctxWithPublicURL := context.WithValue(context.Background(), enginePublicURLContextKey{}, "https://eng.usefused.com")
	policy := &signaturepolicy.Config{
		Version: 2,
		Rules: []signaturepolicy.Rule{{
			Verification: signaturepolicy.Verification{
				Signature: &signaturepolicy.SignatureVerification{
					SecretRef:  "${bucket.default.secret.twilio}",
					Components: []signaturepolicy.InputComponent{{Kind: signaturepolicy.ComponentExactCallbackURL}},
				},
			},
		}},
	}
	shape := webhookAuthShape{Auth: fusedobject.IncomingWebhookConfig{SignaturePolicy: policy}}
	binding := webhookSecretBinding{Reference: "${bucket.default.secret.twilio}", BucketID: uuid.New()}
	resolved := webhookResolvedService{ServiceID: uuid.New(), ServiceVersionID: uuid.New()}

	// Omitting callback_base_url uses Engine's configured public URL and satisfies exact-callback-URL signature policies.
	row, err := prepareRelayAwareWebhookRegistration(ctxWithPublicURL, resolved, webhookConfigDocument{Name: "twilio-events"}, binding, shape, "webhook:twilio-events")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(row.CallbackURL, "https://eng.usefused.com/webhook/"))

	// An explicit callback_base_url override still takes precedence over Engine's default public URL.
	overrideRow, err := prepareRelayAwareWebhookRegistration(ctxWithPublicURL, resolved, webhookConfigDocument{Name: "twilio-events", CallbackBaseURL: "https://broker.example.com"}, binding, shape, "webhook:twilio-events")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(overrideRow.CallbackURL, "https://broker.example.com/webhook/"))

	// When neither Engine public URL nor callback_base_url is configured, exact-callback-URL policies still fail closed.
	emptyCtx := context.WithValue(context.Background(), enginePublicURLContextKey{}, "")
	_, err = prepareRelayAwareWebhookRegistration(emptyCtx, resolved, webhookConfigDocument{Name: "twilio-events"}, binding, shape, "webhook:twilio-events")
	require.Error(t, err)
	require.Contains(t, err.Error(), "engine.public_url")
}

type stubWebhookAvailabilityStore struct {
	store.Store
	snapshotPresent bool
	hasWebhooks     bool
}

// ServiceContractWebhookAvailability returns controlled snapshot/webhook availability for unit tests.
func (s *stubWebhookAvailabilityStore) ServiceContractWebhookAvailability(context.Context, uuid.UUID, uuid.UUID) (bool, bool, error) {
	return s.snapshotPresent, s.hasWebhooks, nil
}

// TestEnsureWebhookServicesHaveEventsRejectsZeroWebhookServices verifies Engine webhook planning rejects services with no webhook events.
func TestEnsureWebhookServicesHaveEventsRejectsZeroWebhookServices(t *testing.T) {
	resolved := map[string]webhookResolvedService{
		"rest-only": {ServiceID: uuid.New(), ServiceVersionID: uuid.New()},
	}
	// A persisted contract snapshot with zero webhooks must fail planning with a clear 400 error.
	err := ensureWebhookServicesHaveEvents(context.Background(), &stubWebhookAvailabilityStore{snapshotPresent: true, hasWebhooks: false}, resolved)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no webhook events")
	// Services declaring at least one webhook event pass validation.
	require.NoError(t, ensureWebhookServicesHaveEvents(context.Background(), &stubWebhookAvailabilityStore{snapshotPresent: true, hasWebhooks: true}, resolved))
	// Missing snapshot fixtures do not break legacy unit tests without contract rows.
	require.NoError(t, ensureWebhookServicesHaveEvents(context.Background(), &stubWebhookAvailabilityStore{snapshotPresent: false, hasWebhooks: false}, resolved))
}
