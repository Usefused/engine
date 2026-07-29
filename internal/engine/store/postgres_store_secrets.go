package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

const upsertSecretSQL = `
	INSERT INTO fused_workspace_secrets
	(bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, expires_at)
	SELECT b.id, $2, $3, $4, $5, $6, $7
	FROM fused_buckets b
	WHERE b.id = $1
	ON CONFLICT ON CONSTRAINT uq_workspace_secrets
	DO UPDATE SET
		credential_type = EXCLUDED.credential_type,
		encrypted_dek = EXCLUDED.encrypted_dek,
		encrypted_value = EXCLUDED.encrypted_value,
		expires_at = EXCLUDED.expires_at,
		updated_at = NOW()
`

func (s *postgresStore) UpsertSecret(ctx context.Context, secret WorkspaceSecret) error {
	return execUpsertSecret(ctx, s.db, secret)
}

// UpsertSecrets commits a whole credential family in one transaction so a
// failed cert/key or username/password update cannot leave runtime auth split
// between old and new material.
func (s *postgresStore) UpsertSecrets(ctx context.Context, secrets []WorkspaceSecret) error {
	if len(secrets) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, secret := range secrets {
		if err := execUpsertSecret(ctx, tx, secret); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *postgresStore) DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error {
	if len(keyNames) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM fused_workspace_secrets WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)`, bucketID, serviceID, keyNames)
	return err
}

type secretExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// execUpsertSecret keeps the single-row statement shared between direct and
// transactional callers so conflict behavior stays identical across paths.
func execUpsertSecret(ctx context.Context, exec secretExec, secret WorkspaceSecret) error {
	// Bucket ownership is authoritative. Callers do not get to supply a
	// duplicate workspace identity that could disagree with the singleton Engine.
	tag, err := exec.Exec(ctx, upsertSecretSQL, secret.BucketID, secret.ServiceID, secret.KeyName, secret.CredentialType, secret.EncryptedDEK, secret.EncryptedValue, secret.ExpiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrBucketNotFound, secret.BucketID)
	}
	return nil
}
