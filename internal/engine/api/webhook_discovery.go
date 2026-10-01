package api

import (
	"context"
	"encoding/json"
	"os"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/secretref"
)

// webhookSigningSecretTarget returns navigation metadata only to actors allowed to inspect that bucket's credentials.
func webhookSigningSecretTarget(ctx context.Context, registration store.WorkspaceWebhook) map[string]interface{} {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	// Ordinary service readers must not learn credential locations from a URL listing.
	if !ok || registration.SecretBucketID == nil {
		return nil
	}
	resource := accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: *registration.SecretBucketID}
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor,
		accesscontrol.Requirement{Permission: accesscontrol.PermissionBucketRead, Resource: resource},
		accesscontrol.Requirement{Permission: accesscontrol.PermissionCredentialsMetadataRead, Resource: resource}); err != nil {
		return nil
	}
	ref, err := secretref.Parse(registration.SecretRef)
	// Invalid or non-secret references cannot be used to navigate to another credential.
	if err != nil || ref.Kind != secretref.KindSecret {
		return nil
	}
	return map[string]interface{}{"bucket_id": registration.SecretBucketID.String(), "key_name": ref.Key}
}

// webhookDiscoveryDestination exposes only the receiving address, never relay authority or secret references.
func webhookDiscoveryDestination(registration store.WorkspaceWebhook) (string, string) {
	var relay struct {
		Source json.RawMessage `json:"source"`
	}
	// A managed receiver pulls from its broker; its local ingress must never be advertised to providers.
	if len(registration.RelayConfig) > 0 {
		// Invalid persisted relay metadata cannot safely be advertised as direct ingress.
		if err := json.Unmarshal(registration.RelayConfig, &relay); err != nil {
			return "", "unknown"
		}
		// JSON null has no source; a populated source means managed delivery.
		if len(relay.Source) > 0 && string(relay.Source) != "null" {
			return "", "managed"
		}
	}
	// Preserve exact registered URLs used by provider signature policies.
	if registration.CallbackURL != "" {
		return registration.CallbackURL, "direct"
	}
	url, err := registeredCallbackURL(os.Getenv("FUSED_ENGINE_PUBLIC_URL"), registration.Slug)
	// Without a valid public base, callers may show the route but must not invent a provider URL.
	if err != nil {
		return "", "direct"
	}
	return url, "direct"
}
