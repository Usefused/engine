package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// validateWebhookRelayConfig keeps remote input disjoint from provider-secret ingress.
func validateWebhookRelayConfig(service webhookConfigServiceDoc) error {
	// A nil relay field preserves the existing registration contract.
	if service.Relay == nil {
		return nil
	}
	// A malformed export or receiver selector is never silently ignored.
	if err := service.Relay.Validate(); err != nil {
		return fmt.Errorf("invalid webhook relay policy")
	}
	// Remote receivers must not masquerade as direct signed ingress or store provider signing secrets.
	if service.Relay.Source != nil && service.Secret != "" {
		return fmt.Errorf("remote webhook source cannot configure a signing secret")
	}
	return nil
}

// resolveWebhookSecretBindings extends the existing bucket permission snapshot with explicitly selected connection buckets.
func resolveWebhookSecretBindings(ctx context.Context, s store.Store, label string, resolved map[string]webhookResolvedService, names []string) (map[string]webhookSecretBinding, []store.Bucket, error) {
	bindings, buckets, err := resolveLocalWebhookSecretBindings(ctx, s, label, resolved, names)
	// Ordinary secret resolution must succeed before remote receiver scope is considered.
	if err != nil {
		return nil, nil, err
	}
	remote, err := resolveWebhookSourceBuckets(ctx, s, resolved, names)
	// An unavailable or mismatched connection cannot authorize a receiver.
	if err != nil {
		return nil, nil, err
	}
	// Source buckets are added to the same required bucket.use permissions as local references.
	for name, bucket := range remote {
		bindings[name] = webhookSecretBinding{BucketID: bucket.ID}
		buckets = append(buckets, bucket)
	}
	return bindings, buckets, nil
}

// resolveWebhookSourceBuckets resolves connection identities and bucket permissions in two bounded batched reads.
func resolveWebhookSourceBuckets(ctx context.Context, s store.Store, resolved map[string]webhookResolvedService, names []string) (map[string]store.Bucket, error) {
	var ids []uuid.UUID
	var bucketNames []string
	// Only explicit remote sources participate in connected credential lookup.
	for _, name := range names {
		r := resolved[name]
		// Local ingress keeps its existing secret-resolution path.
		if r.Relay == nil || r.Relay.Source == nil {
			continue
		}
		ids = append(ids, r.Relay.Source.ConnectionID)
		bucketNames = append(bucketNames, r.Relay.Source.Bucket)
	}
	result := map[string]store.Bucket{}
	// Ordinary webhook config incurs no extra database reads.
	if len(ids) == 0 {
		return result, nil
	}
	connections, err := s.GetAuthConnectionsByIDs(ctx, ids)
	// Query failure cannot be interpreted as permission to a missing connection.
	if err != nil {
		return nil, err
	}
	buckets, err := s.GetBucketsByNames(ctx, bucketNames)
	// Bucket names resolve once; immutable IDs are checked against each connection.
	if err != nil {
		return nil, err
	}
	byName := map[string]store.Bucket{}
	// This in-memory index contains only the batch explicitly named in the desired configuration.
	for _, bucket := range buckets {
		byName[bucket.Name] = bucket
	}
	return matchWebhookSourceBuckets(resolved, names, connections, byName)
}

// matchWebhookSourceBuckets prevents cross-bucket, cross-service and non-managed token reuse.
func matchWebhookSourceBuckets(resolved map[string]webhookResolvedService, names []string, connections map[uuid.UUID]store.AuthConnection, buckets map[string]store.Bucket) (map[string]store.Bucket, error) {
	result := map[string]store.Bucket{}
	// Each configured source must be backed by its exact local bucket-owned connection.
	for _, name := range names {
		r := resolved[name]
		// Non-remote registrations have no provider connection authorization to evaluate.
		if r.Relay == nil || r.Relay.Source == nil {
			continue
		}
		source := r.Relay.Source
		conn, ok := connections[source.ConnectionID]
		bucket := buckets[source.Bucket]
		// Managed app identity and local bucket membership are required independently of UUID knowledge.
		if !ok || bucket.ID == uuid.Nil || conn.BucketID != bucket.ID || conn.ServiceID != r.ServiceID || !conn.ManagedAuth {
			return nil, webhookrelay.ErrDenied
		}
		result[name] = bucket
	}
	return result, nil
}

// prepareRelayAwareWebhookRegistration uses one ordinary registration with an explicit transport role.
func prepareRelayAwareWebhookRegistration(r webhookResolvedService, doc webhookConfigDocument, binding webhookSecretBinding, shape webhookAuthShape, configKey string) (store.WorkspaceWebhook, error) {
	// Remote receivers do not create a provider-auth bypass at the public webhook endpoint.
	if r.Relay != nil && r.Relay.Source != nil {
		row, err := prepareWorkspaceWebhookRegistration(r.ServiceID, r.ServiceVersionID, doc.Name, "", nil, fusedobject.IncomingWebhookConfig{}, "", configKey, "")
		row.AuthType = "fused_remote"
		row.RelayConfig, _ = json.Marshal(r.Relay)
		return row, err
	}
	// A published relay must originate from the exact signed registration, never unsigned public ingress.
	if r.Relay != nil && r.Relay.Publish != nil && (webhookrelay.ValidateExportVerification(shape.Auth.SignaturePolicy) != nil || binding.BucketID == uuid.Nil) {
		return store.WorkspaceWebhook{}, webhookrelay.ErrDenied
	}
	// Shared policy validation keeps signature references pinned to this registration's local bucket.
	if err := validateSignaturePolicyBinding(shape.Auth.SignaturePolicy, binding.Reference, doc.CallbackBaseURL); err != nil {
		return store.WorkspaceWebhook{}, err
	}
	var bucketID *uuid.UUID
	// Local unsigned webhooks deliberately retain their existing absent-secret representation.
	if binding.BucketID != uuid.Nil {
		id := binding.BucketID
		bucketID = &id
	}
	row, err := prepareWorkspaceWebhookRegistration(r.ServiceID, r.ServiceVersionID, doc.Name, binding.Reference, bucketID, shape.Auth, shape.EventExtractionPath, configKey, doc.CallbackBaseURL)
	row.RelayConfig, _ = json.Marshal(r.Relay)
	return row, err
}
