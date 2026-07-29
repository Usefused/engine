package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	EventExtractionPath string
	// SecretRef is a canonical "${bucket.<name>.secret.<key>}" reference (see
	// internal/shared/secretref) into the generic bucket-scoped named-secret
	// store -- never a literal value. Empty means no signing secret is
	// configured for this registration. Resolved at verification time, not
	// stored/decrypted again here.
	SecretRef string
	// OwningConfigKey is nil for a registration created the legacy way
	// (workspace apply's runtime_config.webhooks). A kind: webhook artifact
	// apply sets this to its own config_key (e.g. "webhook:team-x-webhooks")
	// so workspace apply's prune (PruneWorkspaceWebhooks) never deletes or
	// fights over a row it doesn't own, and so (service_id, label)
	// uniqueness across different kind: webhook artifacts can be enforced
	// as "this pair already belongs to config_key X" instead of a silent
	// overwrite -- see plans/plan-webhook-kind.md.
	OwningConfigKey *string
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

var ErrWorkspaceWebhookNotFound = errors.New("workspace webhook not found")

// UpsertWorkspaceWebhook creates or updates a webhook registration keyed on
// (service_id, label). Callers must populate webhook.Slug with
// a freshly generated candidate (webhookid.Generate()) even when updating an
// existing row -- the candidate is only used if this turns out to be a first
// insert. On conflict, the SET clause deliberately omits `slug`: an existing
// registration's URL must never change out from under whoever configured it
// on the provider's side, so a re-apply that only changes, say, the signing
// secret leaves the slug (and therefore the URL) untouched. The returned
// struct always reflects the row's true persisted state, including whichever
// slug actually won.
func (s *postgresStore) UpsertWorkspaceWebhook(ctx context.Context, webhook WorkspaceWebhook) (*WorkspaceWebhook, error) {
	// verification_headers is `text[] NOT NULL DEFAULT '{}'`; pgx encodes a
	// nil Go slice as SQL NULL (not an empty array), which would violate the
	// NOT NULL constraint for the common case of a service that declares no
	// verification headers at all. Coerce nil -> empty slice at the DB
	// boundary rather than pushing this footgun onto every caller.
	verificationHeaders := webhook.VerificationHeaders
	if verificationHeaders == nil {
		verificationHeaders = []string{}
	}
	row := s.db.QueryRow(ctx, upsertWorkspaceWebhookSQL,
		webhook.ServiceID, webhook.ServiceVersionID, webhook.Label, webhook.Slug,
		webhook.AuthType, webhook.AuthLocation, webhook.AuthKeyName,
		webhook.SignatureHeader, verificationHeaders, webhook.EventExtractionPath,
		webhook.SecretRef, webhook.OwningConfigKey,
	)
	saved, err := scanWorkspaceWebhook(row)
	if err != nil {
		return nil, fmt.Errorf("UpsertWorkspaceWebhook: %w", err)
	}
	return &saved, nil
}

// RemoveWorkspaceWebhook deletes a single registration by its identity
// (service, label) -- a renamed label in workspace YAML is
// intentionally delete-old-create-new, not an in-place rename, so a stale
// label reliably stops resolving instead of silently reusing another
// registration's row.
func (s *postgresStore) RemoveWorkspaceWebhook(ctx context.Context, serviceID uuid.UUID, label string) error {
	res, err := s.db.Exec(ctx,
		`DELETE FROM fused_workspace_webhooks WHERE service_id = $1 AND label = $2`,
		serviceID, label,
	)
	if err != nil {
		return fmt.Errorf("RemoveWorkspaceWebhook: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("RemoveWorkspaceWebhook: %w", ErrWorkspaceWebhookNotFound)
	}
	return nil
}

// PruneWorkspaceWebhooks deletes every *legacy* (owning_config_key IS NULL)
// registration for serviceID whose label is not in keepLabels, in one query,
// and returns the labels that were actually removed so the caller can emit
// one audit span per removal (matching the granularity
// removeManagedWorkspaceVersions already uses for version removal) without a
// separate list-then-loop-delete round trip. An empty keepLabels removes
// every legacy registration for the service -- used both when a re-apply
// drops all webhooks for a still-kept service and when the service itself is
// removed from the workspace.
//
// The owning_config_key IS NULL filter is deliberate, not incidental:
// workspace apply (runtime_config.webhooks) must never delete or fight over
// a registration a kind: webhook artifact created -- that artifact's own
// apply is the only thing that reconciles its rows. See this table's
// owning_config_key column comment (schema_engine.go) and
// plans/plan-webhook-kind.md.
func (s *postgresStore) PruneWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID, keepLabels []string) ([]string, error) {
	// A nil slice must not reach the query as-is: pgx encodes a nil []string
	// as SQL NULL, and `label = ANY(NULL)` evaluates to NULL rather than
	// false -- which a `WHERE NOT (...)` clause then treats as "exclude this
	// row," silently pruning nothing instead of everything. Normalizing to a
	// non-nil empty slice keeps "remove everything" and "remove everything
	// except these" the same query path with no NULL-comparison surprise.
	if keepLabels == nil {
		keepLabels = []string{}
	}
	rows, err := s.db.Query(ctx, pruneWorkspaceWebhooksSQL, serviceID, keepLabels)
	if err != nil {
		return nil, fmt.Errorf("PruneWorkspaceWebhooks: %w", err)
	}
	defer rows.Close()
	var removed []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("PruneWorkspaceWebhooks: scan: %w", err)
		}
		removed = append(removed, label)
	}
	return removed, rows.Err()
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

// PruneOwnedWorkspaceWebhooks deletes every registration owned by
// owningConfigKey whose service_id is not in keepServiceIDs, in one query --
// see the Store interface doc comment for why this is scoped by owning
// artifact rather than by a single service the way PruneWorkspaceWebhooks
// is.
func (s *postgresStore) PruneOwnedWorkspaceWebhooks(ctx context.Context, owningConfigKey string, keepServiceIDs []uuid.UUID) ([]uuid.UUID, error) {
	// Same nil-vs-empty-slice footgun as PruneWorkspaceWebhooks: pgx encodes
	// a nil []uuid.UUID as SQL NULL, and `service_id = ANY(NULL)` is NULL,
	// which a `WHERE NOT (...)` clause treats as "exclude this row" --
	// silently pruning nothing instead of everything when the artifact's
	// services map is emptied entirely. Normalize so "keep nothing" and
	// "keep these" share one query path.
	if keepServiceIDs == nil {
		keepServiceIDs = []uuid.UUID{}
	}
	rows, err := s.db.Query(ctx,
		`DELETE FROM fused_workspace_webhooks
		 WHERE owning_config_key = $1 AND NOT (service_id = ANY($2::uuid[]))
		 RETURNING service_id`,
		owningConfigKey, keepServiceIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("PruneOwnedWorkspaceWebhooks: %w", err)
	}
	defer rows.Close()
	var removed []uuid.UUID
	for rows.Next() {
		var serviceID uuid.UUID
		if err := rows.Scan(&serviceID); err != nil {
			return nil, fmt.Errorf("PruneOwnedWorkspaceWebhooks: scan: %w", err)
		}
		removed = append(removed, serviceID)
	}
	return removed, rows.Err()
}

// WorkspaceWebhookOwnersByLabel is one batched query (service_id = ANY($1))
// rather than one query per service, since a kind: webhook artifact's
// (service, name) uniqueness check needs an answer for every service it
// declares and label is the same constant (the artifact's own name) across
// all of them -- see this method's doc comment on the Store interface.
func (s *postgresStore) WorkspaceWebhookOwnersByLabel(ctx context.Context, serviceIDs []uuid.UUID, label string) (map[uuid.UUID]*string, error) {
	if len(serviceIDs) == 0 {
		return map[uuid.UUID]*string{}, nil
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
	owners := make(map[uuid.UUID]*string, len(serviceIDs))
	for rows.Next() {
		var serviceID uuid.UUID
		var owningConfigKey *string
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

func scanWorkspaceWebhook(row rowScanner) (WorkspaceWebhook, error) {
	return scanWorkspaceWebhookRows(row)
}

func scanWorkspaceWebhookRows(row rowScanner) (WorkspaceWebhook, error) {
	var w WorkspaceWebhook
	err := row.Scan(
		&w.ID, &w.ServiceID, &w.ServiceVersionID, &w.Label, &w.Slug,
		&w.AuthType, &w.AuthLocation, &w.AuthKeyName,
		&w.SignatureHeader, &w.VerificationHeaders, &w.EventExtractionPath,
		&w.SecretRef, &w.OwningConfigKey,
		&w.CreatedAt, &w.UpdatedAt,
	)
	return w, err
}

// scanWorkspaceWebhookWithAccount scans the one extra trailing column
// selectWorkspaceWebhookBySlugSQL adds on top of workspaceWebhookColumns.
func scanWorkspaceWebhookWithAccount(row rowScanner) (WorkspaceWebhook, error) {
	var w WorkspaceWebhook
	err := row.Scan(
		&w.ID, &w.ServiceID, &w.ServiceVersionID, &w.Label, &w.Slug,
		&w.AuthType, &w.AuthLocation, &w.AuthKeyName,
		&w.SignatureHeader, &w.VerificationHeaders, &w.EventExtractionPath,
		&w.SecretRef, &w.OwningConfigKey,
		&w.CreatedAt, &w.UpdatedAt, &w.AccountID,
	)
	return w, err
}

const workspaceWebhookColumns = `
	id, service_id, service_version_id,
	label, slug,
	auth_type, auth_location, auth_key_name,
	signature_header, verification_headers, event_extraction_path,
	secret_ref, owning_config_key,
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
		w.secret_ref, w.owning_config_key,
		w.created_at, w.updated_at,
		fused_workspaces.account_id
	FROM fused_workspace_webhooks w
	CROSS JOIN fused_workspaces
	WHERE w.slug = $1`

// pruneWorkspaceWebhooksSQL relies on `label = ANY($2::text[])` being false
// for every row when $2 is an empty array, so an empty keepLabels correctly
// deletes every legacy registration for the service in the same query path
// as a partial prune -- no separate "delete all" branch needed.
// owning_config_key IS NULL scopes this to legacy (workspace-apply-created)
// rows only -- see PruneWorkspaceWebhooks' doc comment.
const pruneWorkspaceWebhooksSQL = `
	DELETE FROM fused_workspace_webhooks
	WHERE service_id = $1 AND owning_config_key IS NULL AND NOT (label = ANY($2::text[]))
	RETURNING label`

// upsertWorkspaceWebhookSQL intentionally never assigns `slug` in the SET
// clause -- see UpsertWorkspaceWebhook's doc comment for why.
const upsertWorkspaceWebhookSQL = `
	INSERT INTO fused_workspace_webhooks (
		service_id, service_version_id, label, slug,
		auth_type, auth_location, auth_key_name, signature_header, verification_headers, event_extraction_path,
		secret_ref, owning_config_key
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9::text[], '{}'::text[]), $10, $11, $12)
	ON CONFLICT (service_id, label) DO UPDATE SET
		service_version_id = EXCLUDED.service_version_id,
		auth_type = EXCLUDED.auth_type,
		auth_location = EXCLUDED.auth_location,
		auth_key_name = EXCLUDED.auth_key_name,
		signature_header = EXCLUDED.signature_header,
		verification_headers = COALESCE(EXCLUDED.verification_headers, '{}'::text[]),
		event_extraction_path = EXCLUDED.event_extraction_path,
		secret_ref = EXCLUDED.secret_ref,
		owning_config_key = EXCLUDED.owning_config_key,
		updated_at = NOW()
	RETURNING` + workspaceWebhookColumns
