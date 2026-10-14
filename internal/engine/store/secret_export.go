package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// exportableSecretTypeSQL excludes provider-managed connections and paired credentials even if bad data sets the flag.
const exportableSecretTypeSQL = `(LOWER(REPLACE(credential_type, '-', '_')) IN ('api_key', 'apikey') AND service_id <> '00000000-0000-0000-0000-000000000000'::uuid
 OR credential_type = 'bucket_secret' AND service_id = '00000000-0000-0000-0000-000000000000'::uuid AND key_name LIKE 'secret:%')`

// GetExportableSecret deliberately bypasses runtime caches and filters before loading any encrypted material.
func (s *postgresStore) GetExportableSecret(ctx context.Context, bucketID, secretID uuid.UUID) (*WorkspaceSecret, error) {
	var secret WorkspaceSecret
	err := s.db.QueryRow(ctx, `SELECT id, bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value
 FROM fused_workspace_secrets WHERE bucket_id = $1 AND id = $2 AND exportable AND `+exportableSecretTypeSQL, bucketID, secretID).Scan(
		&secret.ID, &secret.BucketID, &secret.ServiceID, &secret.KeyName, &secret.CredentialType, &secret.EncryptedDEK, &secret.EncryptedValue)
	// Missing, private, and unsupported secrets are indistinguishable to retrieval callers.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// Never return partial material after a failed read.
	if err != nil {
		return nil, err
	}
	secret.Exportable = true
	return &secret, nil
}

// SetSecretExportable changes policy without copying, decrypting, or replacing the stored credential value.
func (s *postgresStore) SetSecretExportable(ctx context.Context, bucketID, secretID uuid.UUID, exportable bool) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE fused_workspace_secrets SET exportable = $3, updated_at = NOW()
 WHERE bucket_id = $1 AND id = $2 AND `+exportableSecretTypeSQL, bucketID, secretID, exportable)
	return tag.RowsAffected() == 1, err
}
