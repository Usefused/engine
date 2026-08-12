package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WorkspaceWebhook is one Engine-owned webhook ingress registration. A
// workspace can hold multiple registrations for the same service (Label is
// the identity a re-apply matches on, e.g. "repo-a", "staging") -- labels
// are not an event filter, just a way for a team to tell their own
// registrations apart. AuthType/AuthLocation/AuthKeyName/SignatureHeader/
// VerificationHeaders/EventExtractionPath are denormalized from the
// service's IncomingWebhookConfig at apply time (see
// upsertDesiredWorkspaceServices) rather than looked up per inbound request,
// so ingress resolves a request with one indexed read instead of a call out
// to fetch the service's auth shape.
type WorkspaceWebhook struct {
	ID                  uuid.UUID
	ServiceID           uuid.UUID
	ServiceVersionID    uuid.UUID
	Label               string
	Slug                string
	AuthType            string
	AuthLocation        string
	AuthKeyName         string
	SignatureHeader     string
	VerificationHeaders []string
	SignaturePolicy     *signaturepolicy.Config
	CallbackURL         string
	EventExtractionPath string
	// SecretRef is a canonical "${bucket.<name>.secret.<key>}" reference (see
	// internal/shared/secretref) into the generic bucket-scoped named-secret
	// store -- never a literal value. Empty means no signing secret is
	// configured for this registration. Runtime parses only its key while the
	// immutable bucket identity below controls lookup.
	SecretRef string
	// SecretBucketID is resolved when the webhook is planned/applied and is
	// the only bucket identity runtime secret lookup trusts. SecretRef's name
	// is descriptive input, never a runtime lookup key.
	SecretBucketID *uuid.UUID
	// OwningConfigKey is the immutable kind: webhook config_key that owns this
	// registration. Clean-cutover persistence has no unowned registration form.
	OwningConfigKey string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// AccountID is only populated by GetWorkspaceWebhookBySlug (the ingress
	// lookup) -- it's resolved via a join against fused_workspaces rather
	// than stored on this table, since a workspace's owning account can
	// change identity concerns without touching every webhook row, and the
	// ingress path needs it (NATS subject, analytics) without a second
	// round trip. Zero value (uuid.Nil) on every other read path.
	AccountID uuid.UUID
}

var (
	ErrWorkspaceWebhookNotFound      = errors.New("workspace webhook not found")
	ErrWorkspaceWebhookOwnerConflict = errors.New("workspace webhook is owned by another config")
	ErrWorkspaceWebhookDuplicate     = errors.New("workspace webhook batch contains a duplicate service identity")
)

type workspaceWebhookQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type workspaceWebhookBatchRow struct {
	ServiceID           uuid.UUID               `json:"service_id"`
	ServiceVersionID    uuid.UUID               `json:"service_version_id"`
	Label               string                  `json:"label"`
	Slug                string                  `json:"slug"`
	AuthType            string                  `json:"auth_type"`
	AuthLocation        string                  `json:"auth_location"`
	AuthKeyName         string                  `json:"auth_key_name"`
	SignatureHeader     string                  `json:"signature_header"`
	VerificationHeaders []string                `json:"verification_headers"`
	EventExtractionPath string                  `json:"event_extraction_path"`
	SignaturePolicy     *signaturepolicy.Config `json:"signature_policy"`
	CallbackURL         string                  `json:"callback_url"`
	SecretRef           string                  `json:"secret_ref"`
	SecretBucketID      *uuid.UUID              `json:"secret_bucket_id"`
	OwningConfigKey     string                  `json:"owning_config_key"`
}

// upsertWorkspaceWebhooks sends the complete reconciliation batch to
// PostgreSQL once. The final SELECT restores input ordering because callers
// use the returned rows to build a deterministic apply response.
func upsertWorkspaceWebhooks(ctx context.Context, q workspaceWebhookQueryer, registrations []WorkspaceWebhook) ([]WorkspaceWebhook, error) {
	if len(registrations) == 0 {
		return []WorkspaceWebhook{}, nil
	}
	payload, err := marshalWorkspaceWebhookBatch(registrations)
	if err != nil {
		return nil, fmt.Errorf("UpsertWorkspaceWebhooks: encode batch: %w", err)
	}
	rows, err := q.Query(ctx, upsertWorkspaceWebhooksSQL, string(payload))
	if err != nil {
		return nil, fmt.Errorf("UpsertWorkspaceWebhooks: query: %w", err)
	}
	defer rows.Close()
	saved := make([]WorkspaceWebhook, 0, len(registrations))
	for rows.Next() {
		webhook, scanErr := scanWorkspaceWebhookRows(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("UpsertWorkspaceWebhooks: scan: %w", scanErr)
		}
		saved = append(saved, webhook)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("UpsertWorkspaceWebhooks: rows: %w", err)
	}
	// ON CONFLICT deliberately skips a row owned by another config. A short
	// result therefore has the same owner-conflict meaning as QueryRow's
	// pgx.ErrNoRows in the single-registration path.
	if len(saved) != len(registrations) {
		return nil, ErrWorkspaceWebhookOwnerConflict
	}
	return saved, nil
}

func marshalWorkspaceWebhookBatch(registrations []WorkspaceWebhook) ([]byte, error) {
	batch := make([]workspaceWebhookBatchRow, len(registrations))
	for i, registration := range registrations {
		headers := registration.VerificationHeaders
		if headers == nil {
			headers = []string{}
		}
		batch[i] = workspaceWebhookBatchRow{
			ServiceID: registration.ServiceID, ServiceVersionID: registration.ServiceVersionID,
			Label: registration.Label, Slug: registration.Slug,
			AuthType: registration.AuthType, AuthLocation: registration.AuthLocation,
			AuthKeyName: registration.AuthKeyName, SignatureHeader: registration.SignatureHeader,
			VerificationHeaders: headers, EventExtractionPath: registration.EventExtractionPath,
			SignaturePolicy: registration.SignaturePolicy, CallbackURL: registration.CallbackURL,
			SecretRef: registration.SecretRef, SecretBucketID: registration.SecretBucketID,
			OwningConfigKey: registration.OwningConfigKey,
		}
	}
	return json.Marshal(batch)
}

// GetWorkspaceWebhookBySlug is the Engine's ingress-path lookup -- the one
// query an inbound webhook request needs, hitting the unique index on slug.
// It joins fused_workspaces in the same query (rather than a second round
// trip) purely to resolve AccountID, which downstream ingress code (NATS
// subject, analytics payloads) still needs -- see WorkspaceWebhook.AccountID.
func (s *postgresStore) GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*WorkspaceWebhook, error) {
	row := s.db.QueryRow(ctx, selectWorkspaceWebhookBySlugSQL, slug)
	webhook, err := scanWorkspaceWebhookWithAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("GetWorkspaceWebhookBySlug: %w", ErrWorkspaceWebhookNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("GetWorkspaceWebhookBySlug: %w", err)
	}
	return &webhook, nil
}

// ListWorkspaceWebhooks returns every registration a workspace holds for one
// service, ordered by label for stable CLI/UI output.
func (s *postgresStore) ListWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID) ([]WorkspaceWebhook, error) {
	rows, err := s.db.Query(ctx,
		selectWorkspaceWebhookSQL+" WHERE service_id = $1 ORDER BY label",
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("ListWorkspaceWebhooks: query: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceWebhook
	for rows.Next() {
		webhook, err := scanWorkspaceWebhookRows(rows)
		if err != nil {
			return nil, fmt.Errorf("ListWorkspaceWebhooks: scan: %w", err)
		}
		out = append(out, webhook)
	}
	return out, rows.Err()
}

// WorkspaceWebhookOwnersByLabel is one batched query (service_id = ANY($1))
// rather than one query per service, since a webhook configuration's
// (service, name) uniqueness check needs an answer for every service it
// declares and label is the same constant (the configuration's name) across
// all of them -- see this method's doc comment on the Store interface.
func (s *postgresStore) WorkspaceWebhookOwnersByLabel(ctx context.Context, serviceIDs []uuid.UUID, label string) (map[uuid.UUID]string, error) {
	if len(serviceIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT service_id, owning_config_key FROM fused_workspace_webhooks
		 WHERE service_id = ANY($1::uuid[]) AND label = $2`,
		serviceIDs, label,
	)
	if err != nil {
		return nil, fmt.Errorf("WorkspaceWebhookOwnersByLabel: %w", err)
	}
	defer rows.Close()
	owners := make(map[uuid.UUID]string, len(serviceIDs))
	for rows.Next() {
		var serviceID uuid.UUID
		var owningConfigKey string
		if err := rows.Scan(&serviceID, &owningConfigKey); err != nil {
			return nil, fmt.Errorf("WorkspaceWebhookOwnersByLabel: scan: %w", err)
		}
		owners[serviceID] = owningConfigKey
	}
	return owners, rows.Err()
}

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query),
// letting the single- and multi-row paths share one scan implementation.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkspaceWebhookRows(row rowScanner) (WorkspaceWebhook, error) {
	var w WorkspaceWebhook
	var signaturePolicy []byte
	err := row.Scan(
		&w.ID, &w.ServiceID, &w.ServiceVersionID, &w.Label, &w.Slug,
		&w.AuthType, &w.AuthLocation, &w.AuthKeyName,
		&w.SignatureHeader, &w.VerificationHeaders, &w.EventExtractionPath,
		&signaturePolicy, &w.CallbackURL,
		&w.SecretRef, &w.SecretBucketID, &w.OwningConfigKey,
		&w.CreatedAt, &w.UpdatedAt,
	)
	return w, scanSignaturePolicy(signaturePolicy, err, &w)
}

// scanWorkspaceWebhookWithAccount scans the one extra trailing column
// selectWorkspaceWebhookBySlugSQL adds on top of workspaceWebhookColumns.
func scanWorkspaceWebhookWithAccount(row rowScanner) (WorkspaceWebhook, error) {
	var w WorkspaceWebhook
	var signaturePolicy []byte
	err := row.Scan(
		&w.ID, &w.ServiceID, &w.ServiceVersionID, &w.Label, &w.Slug,
		&w.AuthType, &w.AuthLocation, &w.AuthKeyName,
		&w.SignatureHeader, &w.VerificationHeaders, &w.EventExtractionPath,
		&signaturePolicy, &w.CallbackURL,
		&w.SecretRef, &w.SecretBucketID, &w.OwningConfigKey,
		&w.CreatedAt, &w.UpdatedAt, &w.AccountID,
	)
	return w, scanSignaturePolicy(signaturePolicy, err, &w)
}

func scanSignaturePolicy(raw []byte, scanErr error, webhook *WorkspaceWebhook) error {
	if scanErr != nil || len(raw) == 0 || string(raw) == "null" {
		return scanErr
	}
	return json.Unmarshal(raw, &webhook.SignaturePolicy)
}

const workspaceWebhookColumns = `
	id, service_id, service_version_id,
	label, slug,
	auth_type, auth_location, auth_key_name,
	signature_header, verification_headers, event_extraction_path,
	signature_policy, callback_url,
	secret_ref, secret_bucket_id, owning_config_key,
	created_at, updated_at`

const selectWorkspaceWebhookSQL = `SELECT` + workspaceWebhookColumns + ` FROM fused_workspace_webhooks`

// selectWorkspaceWebhookBySlugSQL is the ingress-path variant: same columns
// plus fused_workspaces.account_id via a cross join, so a single indexed read
// resolves everything the inbound handler needs (Code Requirements: no N+1
// queries). The cross join is safe because fused_workspaces holds exactly one
// row -- Engine is mono-workspace (CHECK (singleton_key = 1)) -- so this adds
// no fan-out, just the one column ingress needs from the one workspace row.
const selectWorkspaceWebhookBySlugSQL = `SELECT
		w.id, w.service_id, w.service_version_id,
		w.label, w.slug,
		w.auth_type, w.auth_location, w.auth_key_name,
		w.signature_header, w.verification_headers, w.event_extraction_path,
		w.signature_policy, w.callback_url,
		w.secret_ref, w.secret_bucket_id, w.owning_config_key,
		w.created_at, w.updated_at,
		fused_workspaces.account_id
	FROM fused_workspace_webhooks w
	CROSS JOIN fused_workspaces
	WHERE w.slug = $1`

// upsertWorkspaceWebhooksSQL expands one JSON parameter inside PostgreSQL,
// preserving immutable ownership; rows owned by a different config are
// omitted and detected by the result count.
const upsertWorkspaceWebhooksSQL = `
	WITH input AS (
		SELECT entry.ordinality,
			(entry.value->>'service_id')::uuid AS service_id,
			(entry.value->>'service_version_id')::uuid AS service_version_id,
			entry.value->>'label' AS label,
			entry.value->>'slug' AS slug,
			entry.value->>'auth_type' AS auth_type,
			entry.value->>'auth_location' AS auth_location,
			entry.value->>'auth_key_name' AS auth_key_name,
			entry.value->>'signature_header' AS signature_header,
			ARRAY(SELECT jsonb_array_elements_text(entry.value->'verification_headers')) AS verification_headers,
			entry.value->>'event_extraction_path' AS event_extraction_path,
			entry.value->'signature_policy' AS signature_policy,
			entry.value->>'callback_url' AS callback_url,
			entry.value->>'secret_ref' AS secret_ref,
			(entry.value->>'secret_bucket_id')::uuid AS secret_bucket_id,
			entry.value->>'owning_config_key' AS owning_config_key
		FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY AS entry(value, ordinality)
	), ranked AS (
		SELECT input.*,
			first_value(slug) OVER webhook_identity AS first_slug,
			first_value(owning_config_key) OVER webhook_identity AS first_owner,
			row_number() OVER (PARTITION BY service_id, label ORDER BY ordinality DESC) AS reverse_ordinality
		FROM input
		WINDOW webhook_identity AS (
			PARTITION BY service_id, label ORDER BY ordinality
			ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
		)
	), candidates AS (
		SELECT * FROM ranked WHERE reverse_ordinality = 1
	), ownership_conflict AS (
		SELECT 1
		FROM input
		JOIN candidates USING (service_id, label)
		LEFT JOIN fused_workspace_webhooks existing USING (service_id, label)
		WHERE input.owning_config_key <> (
			CASE WHEN existing.service_id IS NULL THEN candidates.first_owner ELSE existing.owning_config_key END
		)
		LIMIT 1
	), upserted AS (
		INSERT INTO fused_workspace_webhooks (
			service_id, service_version_id, label, slug,
			auth_type, auth_location, auth_key_name, signature_header, verification_headers, event_extraction_path,
			signature_policy, callback_url,
			secret_ref, secret_bucket_id, owning_config_key
		)
		SELECT service_id, service_version_id, label, first_slug,
			auth_type, auth_location, auth_key_name, signature_header, verification_headers, event_extraction_path,
			signature_policy, callback_url,
			secret_ref, secret_bucket_id, owning_config_key
		FROM candidates
		WHERE NOT EXISTS (SELECT 1 FROM ownership_conflict)
		ORDER BY ordinality
		ON CONFLICT (service_id, label) DO UPDATE SET
			service_version_id = EXCLUDED.service_version_id,
			auth_type = EXCLUDED.auth_type,
			auth_location = EXCLUDED.auth_location,
			auth_key_name = EXCLUDED.auth_key_name,
			signature_header = EXCLUDED.signature_header,
			verification_headers = EXCLUDED.verification_headers,
			event_extraction_path = EXCLUDED.event_extraction_path,
			signature_policy = EXCLUDED.signature_policy,
			callback_url = EXCLUDED.callback_url,
			secret_ref = EXCLUDED.secret_ref,
			secret_bucket_id = EXCLUDED.secret_bucket_id,
			updated_at = NOW()
		WHERE fused_workspace_webhooks.owning_config_key = EXCLUDED.owning_config_key
		RETURNING ` + workspaceWebhookColumns + `
	)
	SELECT upserted.id, input.service_id, input.service_version_id,
		input.label, upserted.slug,
		input.auth_type, input.auth_location, input.auth_key_name,
		input.signature_header, input.verification_headers, input.event_extraction_path,
		input.signature_policy, input.callback_url,
		input.secret_ref, input.secret_bucket_id, input.owning_config_key,
		upserted.created_at, upserted.updated_at
	FROM upserted
	JOIN input USING (service_id, label)
	ORDER BY input.ordinality`
