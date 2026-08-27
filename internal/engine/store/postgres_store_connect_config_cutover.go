package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	legacyConnectConfigCutoverVersion int64 = 1
	legacyConnectConfigCutoverName          = "20260827_oauth_application_secrets"
	legacyConnectConfigCutoverLock    int64 = 0x465553454F415554
)

// LegacyConnectConfigCutoverResult is the bounded aggregate safe for startup telemetry.
type LegacyConnectConfigCutoverResult struct {
	MigratedRows int
	SkippedRows  int
	BatchCount   int
	AlreadyDone  bool
}

// LegacyConnectConfigCutoverStore exposes the permanent master-key-aware startup gate.
type LegacyConnectConfigCutoverStore interface {
	MigrateLegacyConnectConfigs(context.Context, []byte, int) (LegacyConnectConfigCutoverResult, error)
}

type legacyConnectConfigRow struct {
	ID                    uuid.UUID
	BucketID              uuid.UUID
	ServiceID             uuid.UUID
	AuthType              string
	AuthName              string
	Enabled               bool
	EncryptedDEK          string
	EncryptedClientID     string
	EncryptedClientSecret string
}

// MigrateLegacyConnectConfigs moves encrypted app registrations in bounded batches before runtime starts.
func (s *postgresStore) MigrateLegacyConnectConfigs(ctx context.Context, masterKey []byte, batchSize int) (LegacyConnectConfigCutoverResult, error) {
	// A bounded positive batch is required so live upgrades cannot load the full table into memory.
	if batchSize < 1 || batchSize > 1000 {
		return LegacyConnectConfigCutoverResult{}, errors.New("legacy connect-config cutover batch size is invalid")
	}
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return LegacyConnectConfigCutoverResult{}, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, legacyConnectConfigCutoverLock); err != nil {
		return LegacyConnectConfigCutoverResult{}, err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, legacyConnectConfigCutoverLock)
	}()
	return migrateLegacyConnectConfigsLocked(ctx, conn, masterKey, batchSize)
}

// migrateLegacyConnectConfigsLocked drains the legacy table while the process-wide cutover lock is held.
func migrateLegacyConnectConfigsLocked(ctx context.Context, conn *pgxpool.Conn, masterKey []byte, batchSize int) (LegacyConnectConfigCutoverResult, error) {
	done, err := legacyConnectConfigCutoverApplied(ctx, conn)
	if err != nil {
		return LegacyConnectConfigCutoverResult{}, err
	}
	// Already-migrated databases still remove the empty compatibility table recreated by canonical schema setup.
	if done {
		if err := finalizeLegacyConnectConfigTable(ctx, conn, true, 0); err != nil {
			return LegacyConnectConfigCutoverResult{}, err
		}
		return LegacyConnectConfigCutoverResult{AlreadyDone: true}, nil
	}
	result := LegacyConnectConfigCutoverResult{}
	for {
		batch, err := migrateLegacyConnectConfigBatch(ctx, conn.Conn(), masterKey, batchSize)
		if err != nil {
			return LegacyConnectConfigCutoverResult{}, err
		}
		// A zero-sized exact batch proves all legacy rows have been drained.
		if batch.DrainedRows == 0 {
			break
		}
		result.MigratedRows += batch.MigratedRows
		result.SkippedRows += batch.SkippedRows
		result.BatchCount++
	}
	if err := finalizeLegacyConnectConfigTable(ctx, conn, false, result.MigratedRows); err != nil {
		return LegacyConnectConfigCutoverResult{}, err
	}
	return result, nil
}

// legacyConnectConfigCutoverApplied verifies both migration version and immutable name.
func legacyConnectConfigCutoverApplied(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (bool, error) {
	var versionExists, identityMatches bool
	err := conn.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM fused_sensitive_data_migrations WHERE version = $1),
		EXISTS (SELECT 1 FROM fused_sensitive_data_migrations WHERE version = $1 AND name = $2)`,
		legacyConnectConfigCutoverVersion, legacyConnectConfigCutoverName).Scan(&versionExists, &identityMatches)
	if err != nil {
		return false, err
	}
	// A reused version would make future live upgrades non-deterministic.
	if versionExists && !identityMatches {
		return false, errors.New("sensitive data migration identity mismatch")
	}
	return versionExists, nil
}

// migrateLegacyConnectConfigBatch decrypts, atomically writes, and removes one bounded keyset batch.
func migrateLegacyConnectConfigBatch(ctx context.Context, conn *pgx.Conn, masterKey []byte, batchSize int) (legacyConnectConfigBatchResult, error) {
	tx, err := conn.Begin(ctx)
	// Each page is its own atomic unit so a retry never observes partially migrated credentials.
	if err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id, bucket_id, service_id, auth_type, auth_name, enabled,
		encrypted_dek, encrypted_client_id, encrypted_client_secret
		FROM fused_connect_configs ORDER BY id LIMIT $1 FOR UPDATE`, batchSize)
	// Source row locks keep another cutover worker from selecting the same bounded page.
	if err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	configs, err := scanLegacyConnectConfigBatch(rows, batchSize)
	rows.Close()
	// Scan failure retains every source row because the transaction has not written targets.
	if err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	if err := resolveLegacyConnectConfigAuthNames(ctx, tx, configs); err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	secrets, configIDs, migratedRows, skippedRows, err := prepareLegacyConnectConfigBatch(masterKey, configs)
	// Any undecryptable row blocks the page so operators cannot lose its original ciphertext.
	if err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	// Both values for every registration are written together in one set-based statement.
	if err := execUpsertSecrets(ctx, tx, secrets); err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	// One exact-set delete keeps source removal atomic without issuing a query for every migrated registration.
	if _, err := tx.Exec(ctx, `DELETE FROM fused_connect_configs WHERE id = ANY($1::uuid[])`, configIDs); err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	// Commit follows target writes and source removal so restart recovery is idempotent.
	if err := tx.Commit(ctx); err != nil {
		return legacyConnectConfigBatchResult{}, err
	}
	return legacyConnectConfigBatchResult{DrainedRows: len(configs), MigratedRows: migratedRows, SkippedRows: skippedRows}, nil
}

type legacyConnectConfigBatchResult struct {
	DrainedRows  int
	MigratedRows int
	SkippedRows  int
}

// scanLegacyConnectConfigBatch keeps the live-upgrade memory bound explicit and enforced.
func scanLegacyConnectConfigBatch(rows pgx.Rows, batchSize int) ([]legacyConnectConfigRow, error) {
	configs := make([]legacyConnectConfigRow, 0, batchSize)
	for rows.Next() {
		var config legacyConnectConfigRow
		if err := rows.Scan(&config.ID, &config.BucketID, &config.ServiceID, &config.AuthType, &config.AuthName, &config.Enabled,
			&config.EncryptedDEK, &config.EncryptedClientID, &config.EncryptedClientSecret); err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

// prepareLegacyConnectConfigBatch decrypts one bounded page before its set-based transaction writes.
func prepareLegacyConnectConfigBatch(masterKey []byte, configs []legacyConnectConfigRow) ([]WorkspaceSecret, []uuid.UUID, int, int, error) {
	secrets := make([]WorkspaceSecret, 0, len(configs)*2)
	configIDs := make([]uuid.UUID, 0, len(configs))
	migratedRows := 0
	skippedRows := 0
	for _, config := range configs {
		// Disabled registrations and unresolved historical names stay inactive by not materializing executable secrets.
		if !config.Enabled || strings.TrimSpace(config.AuthName) == "" {
			configIDs = append(configIDs, config.ID)
			skippedRows++
			continue
		}
		migrated, err := migrateLegacyConnectConfigRow(masterKey, config)
		// A single invalid registration blocks its page instead of silently dropping user credentials.
		if err != nil {
			return nil, nil, 0, 0, err
		}
		secrets = append(secrets, migrated...)
		configIDs = append(configIDs, config.ID)
		migratedRows++
	}
	return secrets, configIDs, migratedRows, skippedRows, nil
}

type legacyConnectAuthNameRequest struct {
	ServiceID uuid.UUID `json:"service_id"`
	AuthType  string    `json:"auth_type"`
}

// legacyConnectAuthNameRequests selects only enabled historical rows whose scheme name can be safely backfilled.
func legacyConnectAuthNameRequests(configs []legacyConnectConfigRow) []legacyConnectAuthNameRequest {
	requests := make([]legacyConnectAuthNameRequest, 0, len(configs))
	for _, config := range configs {
		// Named and disabled rows need no metadata lookup; disabled rows are intentionally never activated.
		if config.Enabled && strings.TrimSpace(config.AuthName) == "" {
			requests = append(requests, legacyConnectAuthNameRequest{ServiceID: config.ServiceID, AuthType: canonicalApplicationAuthType(config.AuthType)})
		}
	}
	return requests
}

// resolveLegacyConnectConfigAuthNames backfills blank names only when enabled pinned versions expose one compatible family name.
func resolveLegacyConnectConfigAuthNames(ctx context.Context, tx pgx.Tx, configs []legacyConnectConfigRow) error {
	requests := legacyConnectAuthNameRequests(configs)
	// A page without eligible rows needs no metadata query or mutation.
	if len(requests) == 0 {
		return nil
	}
	resolved, err := queryLegacyConnectConfigAuthNames(ctx, tx, requests)
	// Migration must stop before mutating the page when its set-based metadata read fails.
	if err != nil {
		return err
	}
	for index := range configs {
		// Only a unique compatible pinned family may fill an historically blank selector.
		if configs[index].Enabled && strings.TrimSpace(configs[index].AuthName) == "" {
			configs[index].AuthName = resolved[configs[index].ServiceID]
		}
	}
	return nil
}

// queryLegacyConnectConfigAuthNames resolves unique compatible names for one migration page in a set-based query.
func queryLegacyConnectConfigAuthNames(ctx context.Context, tx pgx.Tx, requests []legacyConnectAuthNameRequest) (map[uuid.UUID]string, error) {
	payload, err := json.Marshal(requests)
	// A malformed internal request batch cannot safely reach the SQL resolver.
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH requested AS (
		SELECT service_id, auth_type FROM jsonb_to_recordset($1::jsonb) AS item(service_id uuid, auth_type text)
	), candidates AS (
		SELECT DISTINCT requested.service_id, auth ->> 'name' AS auth_name
		FROM requested
		JOIN fused_workspace_service_versions enabled ON enabled.service_id = requested.service_id
		JOIN fused_service_contract_snapshots snapshot
		  ON snapshot.service_id = enabled.service_id AND snapshot.service_version_id = enabled.service_version_id
		CROSS JOIN LATERAL jsonb_array_elements(snapshot.service_metadata -> 'auth_configs') auth
		WHERE COALESCE(auth ->> 'name', '') <> ''
		  AND CASE
			WHEN lower(replace(btrim(auth ->> 'type'), '-', '_')) IN ('oauth', 'oauth2') THEN 'oauth'
			WHEN lower(replace(btrim(auth ->> 'type'), '-', '_')) IN ('oidc', 'openidconnect', 'open_id_connect') THEN 'oidc'
			ELSE '' END = requested.auth_type
	)
	SELECT service_id, min(auth_name) FROM candidates GROUP BY service_id HAVING count(*) = 1`, payload)
	// Query failure leaves every historical row unchanged for a later retry.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resolved := make(map[uuid.UUID]string)
	// The bounded result contains at most one unique name per requested service.
	for rows.Next() {
		var serviceID uuid.UUID
		var authName string
		// Scan failure invalidates the whole batch so no partial mapping is applied.
		if err := rows.Scan(&serviceID, &authName); err != nil {
			return nil, err
		}
		resolved[serviceID] = authName
	}
	// Deferred driver errors are equivalent to an incomplete result batch.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return resolved, nil
}

// migrateLegacyConnectConfigRow preserves app values while discarding caller-owned redirect metadata.
func migrateLegacyConnectConfigRow(masterKey []byte, config legacyConnectConfigRow) ([]WorkspaceSecret, error) {
	credentialType := canonicalApplicationAuthType(config.AuthType)
	clientIDKey, clientSecretKey, ok := credentialkeys.OAuthApplication(config.AuthName)
	// Unknown families or unnamed schemes cannot be migrated without changing runtime identity.
	if credentialType == "" || !ok {
		return nil, errors.New("legacy connect config has invalid auth identity")
	}
	dek, err := UnwrapDEK(masterKey, config.EncryptedDEK)
	if err != nil {
		return nil, err
	}
	clientID, err := DecryptWithDEK(dek, config.EncryptedClientID)
	if err != nil {
		return nil, err
	}
	clientSecret, err := DecryptWithDEK(dek, config.EncryptedClientSecret)
	if err != nil {
		return nil, err
	}
	return encryptMigratedOAuthApplication(masterKey, config, credentialType, clientIDKey, clientSecretKey, clientID, clientSecret)
}

// encryptMigratedOAuthApplication creates independently wrapped rows matching normal secret-set writes.
func encryptMigratedOAuthApplication(masterKey []byte, config legacyConnectConfigRow, credentialType, clientIDKey, clientSecretKey, clientID, clientSecret string) ([]WorkspaceSecret, error) {
	inputs := []struct{ key, value string }{{clientIDKey, clientID}, {clientSecretKey, clientSecret}}
	secrets := make([]WorkspaceSecret, 0, len(inputs))
	for _, input := range inputs {
		wrappedDEK, dek, err := WrapDEK(masterKey)
		if err != nil {
			return nil, err
		}
		encrypted, err := EncryptWithDEK(dek, input.value)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, WorkspaceSecret{WorkspaceSecretMeta: WorkspaceSecretMeta{
			BucketID: config.BucketID, ServiceID: config.ServiceID, KeyName: input.key, CredentialType: credentialType,
		}, EncryptedDEK: wrappedDEK, EncryptedValue: encrypted})
	}
	return secrets, nil
}

// canonicalApplicationAuthType maps accepted stored spellings onto the two application-registration families.
func canonicalApplicationAuthType(value string) string {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", "_"))
	// Historical OAuth2 spellings retain the current public family name.
	if normalized == "oauth" || normalized == "oauth2" || normalized == "oauth2_authorization_code" {
		return "oauth"
	}
	// Historical OpenID spellings retain the OIDC family boundary.
	if normalized == "oidc" || normalized == "openidconnect" || normalized == "open_id_connect" {
		return "oidc"
	}
	return ""
}

// finalizeLegacyConnectConfigTable records completion only after proving no legacy rows remain.
func finalizeLegacyConnectConfigTable(ctx context.Context, conn *pgxpool.Conn, alreadyDone bool, migratedRows int) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	shouldDrop, err := validateLegacyConnectConfigTable(ctx, tx, alreadyDone)
	if err != nil {
		return err
	}
	// An absent table on an already-completed database is the true no-op path.
	if !shouldDrop {
		return tx.Commit(ctx)
	}
	// The immutable marker is inserted only on the first successful cutover.
	if !alreadyDone {
		if _, err := tx.Exec(ctx, `INSERT INTO fused_sensitive_data_migrations (version, name, rows_migrated) VALUES ($1, $2, $3)`, legacyConnectConfigCutoverVersion, legacyConnectConfigCutoverName, migratedRows); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DROP TABLE fused_connect_configs`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// validateLegacyConnectConfigTable proves the compatibility table is empty before its permanent removal.
func validateLegacyConnectConfigTable(ctx context.Context, tx pgx.Tx, alreadyDone bool) (bool, error) {
	var tableExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('fused_connect_configs') IS NOT NULL`).Scan(&tableExists); err != nil {
		return false, err
	}
	// An already-completed database need not recreate the historical source table.
	if alreadyDone && !tableExists {
		return false, nil
	}
	// The first application requires the source table established by the historical schema.
	if !tableExists {
		return false, errors.New("legacy connect config table is unavailable before cutover")
	}
	// An explicit exclusive lock closes the final count/drop race with an older Engine still serving config writes.
	if _, err := tx.Exec(ctx, `LOCK TABLE fused_connect_configs IN ACCESS EXCLUSIVE MODE`); err != nil {
		return false, err
	}
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM fused_connect_configs`).Scan(&remaining); err != nil {
		return false, err
	}
	// A completion marker must never coexist with new legacy data from a competing writer or older binary.
	if remaining != 0 {
		return false, errors.New("legacy connect config rows remain")
	}
	return true, nil
}
