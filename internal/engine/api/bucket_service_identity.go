package api

import (
	"context"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

// enrichBucketServiceIdentities resolves only readable service names, including services no longer activated locally.
func enrichBucketServiceIdentities(ctx context.Context, verifier ServiceVerifier, items []map[string]interface{}) []map[string]interface{} {
	scope, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
	// Credential metadata permission alone must not disclose Registry service identities.
	if err != nil {
		return items
	}
	ids := bucketServiceIdentityIDs(items, scope)
	metadata := fetchServiceCardMetadataForListing(ctx, verifier, apiKeyFromGraphQLContext(ctx), ids)
	return applyBucketServiceIdentities(items, metadata)
}

// bucketServiceIdentityIDs limits the batch to distinct, authorized, non-generic service IDs on this page.
func bucketServiceIdentityIDs(items []map[string]interface{}, scope accesscontrol.AuthorizedScope) []uuid.UUID {
	allowed := make(map[uuid.UUID]bool, len(scope.IDs))
	for _, id := range scope.IDs {
		allowed[id] = true
	}
	seen := make(map[uuid.UUID]bool)
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		raw, _ := item["service_id"].(string)
		id, err := uuid.Parse(raw)
		// Bucket-wide secrets have no service; duplicates and denied IDs need no Registry request.
		if err != nil || id == uuid.Nil || seen[id] || (!scope.All && !allowed[id]) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// applyBucketServiceIdentities preserves local names when Registry is unavailable or has removed a service.
func applyBucketServiceIdentities(items []map[string]interface{}, metadata map[uuid.UUID]sandbox.ServiceVisibility) []map[string]interface{} {
	for _, item := range items {
		raw, _ := item["service_id"].(string)
		id, err := uuid.Parse(raw)
		// Malformed or generic identifiers cannot have Registry display metadata.
		if err != nil || id == uuid.Nil {
			continue
		}
		identity, ok := metadata[id]
		// A failed lookup must leave durable local metadata intact.
		if !ok {
			continue
		}
		// Empty upstream names must not erase a known local label.
		if name := strings.TrimSpace(identity.Name); name != "" {
			item["service_name"] = name
		}
		item["service_slug"] = displaySlug(identity)
	}
	return items
}
