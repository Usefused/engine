package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

const upsertSecretsSQL = `
	WITH input AS (
		SELECT * FROM jsonb_to_recordset($1::jsonb) AS item(
			bucket_id uuid,
			service_id uuid,
			key_name text,
			credential_type text,
			encrypted_dek text,
			encrypted_value text,
			expires_at timestamptz
		)
	), inserted AS (
		INSERT INTO fused_workspace_secrets
		(bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, expires_at)
		SELECT bucket.id, input.service_id, input.key_name, input.credential_type,
		       input.encrypted_dek, input.encrypted_value, input.expires_at
		FROM input
		JOIN fused_buckets bucket ON bucket.id = input.bucket_id
		ON CONFLICT ON CONSTRAINT uq_workspace_secrets
		DO UPDATE SET
			credential_type = EXCLUDED.credential_type,
			encrypted_dek = EXCLUDED.encrypted_dek,
			encrypted_value = EXCLUDED.encrypted_value,
			expires_at = EXCLUDED.expires_at,
			updated_at = NOW()
		RETURNING 1
	)
	SELECT COUNT(*) FROM inserted
`

type bulkSecretInput struct {
	BucketID       uuid.UUID  `json:"bucket_id"`
	ServiceID      uuid.UUID  `json:"service_id"`
	KeyName        string     `json:"key_name"`
	CredentialType string     `json:"credential_type"`
	EncryptedDEK   string     `json:"encrypted_dek"`
	EncryptedValue string     `json:"encrypted_value"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

func (s *postgresStore) UpsertSecret(ctx context.Context, secret WorkspaceSecret) error {
	return execUpsertSecret(ctx, s.db, secret)
}

// UpsertSecrets commits a whole credential family in one transaction so a
// failed cert/key or username/password update cannot leave runtime auth split
// between old and new material.
func (s *postgresStore) UpsertSecrets(ctx context.Context, secrets []WorkspaceSecret) error {
	// Empty families are a no-op and should not allocate a transaction.
	if len(secrets) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	// Family writes require a transaction so count validation can roll back a partial authoritative-bucket join.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The shared bulk path keeps direct writes and migration writes on the same conflict semantics.
	if err := execUpsertSecrets(ctx, tx, secrets); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteSecrets removes one exact bucket credential family in a single statement.
func (s *postgresStore) DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error {
	return s.deleteWorkspaceSecrets(ctx, bucketID, serviceID, keyNames)
}

// DeleteSecret shares the exact bucket-scoped deletion path used by family removal.
func (s *postgresStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	return s.deleteWorkspaceSecrets(ctx, bucketID, serviceID, []string{keyName})
}

// deleteWorkspaceSecrets removes only explicit keys; immutable apps become unready until referenced source material is restored.
func (s *postgresStore) deleteWorkspaceSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) error {
	keyNames = uniqueSecretKeyNames(keyNames)
	// Empty admin selections are a no-op and must never widen into a bucket-wide delete.
	if len(keyNames) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `DELETE FROM fused_workspace_secrets WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)`, bucketID, serviceID, keyNames)
	return err
}

type secretExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type secretBatchExec interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// execUpsertSecrets writes a complete credential family with one set-based database statement.
func execUpsertSecrets(ctx context.Context, exec secretBatchExec, secrets []WorkspaceSecret) error {
	payload := make([]bulkSecretInput, 0, len(secrets))
	for _, secret := range secrets {
		payload = append(payload, bulkSecretInput{
			BucketID: secret.BucketID, ServiceID: secret.ServiceID, KeyName: secret.KeyName,
			CredentialType: secret.CredentialType, EncryptedDEK: secret.EncryptedDEK,
			EncryptedValue: secret.EncryptedValue, ExpiresAt: secret.ExpiresAt,
		})
	}
	encoded, err := json.Marshal(payload)
	// Encoding failures must stop before the statement can write only part of a family.
	if err != nil {
		return err
	}
	var written int
	// One statement is the no-N+1 boundary for all secrets in this credential family.
	if err := exec.QueryRow(ctx, upsertSecretsSQL, string(encoded)).Scan(&written); err != nil {
		return err
	}
	// A short write means at least one bucket was not authoritative, so the owning transaction must roll back every row.
	if written != len(secrets) {
		return fmt.Errorf("%w: credential family", ErrBucketNotFound)
	}
	return nil
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
