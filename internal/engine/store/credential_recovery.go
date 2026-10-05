package store

import (
	"context"

	"github.com/google/uuid"
)

// CredentialRecoveryMetadataStore supplies readable command targets only after credential resolution fails.
type CredentialRecoveryMetadataStore interface {
	CredentialRecoveryNames(context.Context, uuid.UUID, uuid.UUID) (string, string, error)
}

// CredentialRecoveryNames uses workspace metadata without retrieving any credential values.
func (s *postgresStore) CredentialRecoveryNames(ctx context.Context, serviceID, bucketID uuid.UUID) (string, string, error) {
	var service, bucket string
	err := s.db.QueryRow(ctx, `SELECT
 COALESCE((SELECT service_slug FROM fused_workspace_services WHERE service_id = $1), ''),
 COALESCE((SELECT name FROM fused_buckets WHERE id = $2), '')`, serviceID, bucketID).Scan(&service, &bucket)
	// Preserve the original IDs if metadata cannot be read; recovery must never replace the execution failure.
	if err != nil {
		return "", "", err
	}
	// Ambiguous slugs must not direct an operator to configure another service.
	if service != "" {
		resolved, lookupErr := s.ResolveWorkspaceServiceIDsByKeys(ctx, []string{service})
		// A failed or ambiguous reverse lookup must keep the immutable command target.
		if lookupErr != nil || resolved[service] != serviceID {
			service = ""
		}
	}
	return service, bucket, nil
}

// CredentialRecoveryNames bypasses caches so recovery commands reflect current bucket names.
func (s *cachedStore) CredentialRecoveryNames(ctx context.Context, serviceID, bucketID uuid.UUID) (string, string, error) {
	metadata, ok := s.Store.(CredentialRecoveryMetadataStore)
	// Lightweight stores can retain stable UUID guidance without supporting presentation metadata.
	if !ok {
		return "", "", nil
	}
	return metadata.CredentialRecoveryNames(ctx, serviceID, bucketID)
}
