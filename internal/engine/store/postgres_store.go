package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct {
	db *pgxpool.Pool
}

var ErrWorkspaceOwnerMismatch = errors.New("engine workspace belongs to a different Registry account")

func NewPostgresStore(db *pgxpool.Pool) Store {
	return &postgresStore{db: db}
}

func (s *postgresStore) LoadEngineInstallationID(ctx context.Context) (uuid.UUID, error) {
	var installationID uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT installation_id
		FROM fused_engine_installation
		WHERE singleton_key = 1
	`).Scan(&installationID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load Engine installation identity: %w", err)
	}
	return installationID, nil
}

func (s *postgresStore) BootstrapWorkspace(ctx context.Context, accountID uuid.UUID, name string) (uuid.UUID, error) {
	ownerID, err := s.getSingletonWorkspace(ctx)
	if err == nil {
		return s.finishWorkspaceBootstrap(ctx, accountID, ownerID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("load Engine workspace: %w", err)
	}
	if err := s.insertSingletonWorkspace(ctx, accountID, name); err != nil {
		return uuid.Nil, fmt.Errorf("create Engine workspace: %w", err)
	}
	// ON CONFLICT may mean a concurrent startup won the singleton insert.
	// Reloading the owner prevents a process authenticated as another Registry
	// account from accepting the winner's local workspace.
	ownerID, err = s.getSingletonWorkspace(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load created Engine workspace: %w", err)
	}
	return s.finishWorkspaceBootstrap(ctx, accountID, ownerID)
}

func (s *postgresStore) insertSingletonWorkspace(ctx context.Context, accountID uuid.UUID, name string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO fused_workspaces (name, account_id, slug, singleton_key)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (singleton_key) DO NOTHING
	`, name, accountID, accountID.String())
	return err
}

func (s *postgresStore) finishWorkspaceBootstrap(ctx context.Context, accountID, ownerID uuid.UUID) (uuid.UUID, error) {
	if err := validateWorkspaceOwner(accountID, ownerID); err != nil {
		return uuid.Nil, err
	}
	// Ownership must be established before bootstrap performs any local write;
	// a Registry identity for another account must not mutate this singleton
	// workspace as part of a failed startup.
	if err := s.ensureDefaultBucket(ctx); err != nil {
		return uuid.Nil, err
	}
	var workspaceID uuid.UUID
	if err := s.db.QueryRow(ctx, "SELECT id FROM fused_workspaces WHERE singleton_key = 1").Scan(&workspaceID); err != nil {
		return uuid.Nil, fmt.Errorf("fetch workspace ID: %w", err)
	}
	return workspaceID, nil
}

func (s *postgresStore) ensureDefaultBucket(ctx context.Context) error {
	query := `
		INSERT INTO fused_buckets (name, is_default)
		VALUES ('default', true)
		ON CONFLICT (name) DO NOTHING
	`
	_, err := s.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("ensure default bucket: %w", err)
	}
	return nil
}

// LoadDefaultBucketID is a point lookup for authorization policy resolution;
// it avoids loading every bucket and filtering in the HTTP layer.
func (s *postgresStore) LoadDefaultBucketID(ctx context.Context) (uuid.UUID, error) {
	var bucketID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT id FROM fused_buckets WHERE is_default = true LIMIT 1`).Scan(&bucketID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load default bucket ID: %w", err)
	}
	return bucketID, nil
}

func (s *postgresStore) getSingletonWorkspace(ctx context.Context) (uuid.UUID, error) {
	var ownerID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT account_id FROM fused_workspaces WHERE singleton_key = 1`).Scan(&ownerID)
	return ownerID, err
}

func validateWorkspaceOwner(expected, actual uuid.UUID) error {
	if expected == actual {
		return nil
	}
	return fmt.Errorf("%w: workspace owner is %s, license belongs to %s", ErrWorkspaceOwnerMismatch, actual, expected)
}

func (s *postgresStore) GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error) {
	err := s.VerifyWorkspaceOwner(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("workspace not found for account: %w", err)
	}
	version, err := s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, serviceID)
	if err != nil {
		return "", fmt.Errorf("no workspace service version found for service %s: %w", serviceID, err)
	}
	return version, nil
}

// GetLatestWorkspaceServiceVersionID returns exact latest workspace service version id through one app-scoped query or cache lookup.
func (s *postgresStore) GetLatestWorkspaceServiceVersionID(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (uuid.UUID, error) {
	err := s.VerifyWorkspaceOwner(ctx, accountID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("workspace not found for account: %w", err)
	}
	versionID, err := s.GetLatestWorkspaceServiceVersionIDByWorkspace(ctx, serviceID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no workspace service version found for service %s: %w", serviceID, err)
	}
	return versionID, nil
}

// appRuntimeSelectColumns is shared by GetAppRuntime/ListAppRuntimes/
// ListMCPAppsByAccount so their SELECT lists and Scan order can't drift
// apart from each other.
const appRuntimeSelectColumns = `a.account_id, a.app_id, f.owner_subject_id, f.owner_team_id,
fb.bucket_id, a.scope_schema_version, a.selections,
a.unified_definition_schema_version, a.unified_definitions,
a.unified_definition_hash, a.unified_codegen_descriptor_hash,
a.status, f.kind, f.display_name, a.version, a.config_key, a.created_at`

func scanAppRuntime(row pgx.Row) (*AppRuntime, error) {
	scope, _, err := scanAppRuntimeRow(row, false)
	return scope, err
}

func scanAppRuntimeWithTotal(row pgx.Row) (*AppRuntime, int, error) {
	return scanAppRuntimeRow(row, true)
}

// scanAppRuntimeRow maps the stable query column order into one Engine persistence value.
func scanAppRuntimeRow(row pgx.Row, includeTotal bool) (*AppRuntime, int, error) {
	var scope AppRuntime
	var bucketID *uuid.UUID
	var name *string
	var version, configKey *string
	var ownerSubjectID, ownerTeamID *uuid.UUID
	total := 0
	targets := []any{
		&scope.AccountID, &scope.AppID, &ownerSubjectID, &ownerTeamID,
		&bucketID, &scope.ScopeSchemaVersion, &scope.Selections,
		&scope.UnifiedDefinitionSchemaVersion, &scope.UnifiedDefinitions,
		&scope.UnifiedDefinitionHash, &scope.UnifiedCodegenDescriptorHash, &scope.Status,
		&scope.Kind, &name, &version, &configKey, &scope.CreatedAt,
	}
	if includeTotal {
		targets = append(targets, &total)
	}
	if err := row.Scan(targets...); err != nil {
		return nil, 0, err
	}
	applyAppRuntimeOptionals(&scope, ownerSubjectID, ownerTeamID, bucketID, name, version, configKey)
	return &scope, total, nil
}

func applyAppRuntimeOptionals(
	scope *AppRuntime,
	ownerSubjectID, ownerTeamID, bucketID *uuid.UUID,
	name, version, configKey *string,
) {
	if ownerSubjectID != nil {
		scope.OwnerSubjectID = *ownerSubjectID
	}
	if ownerTeamID != nil {
		scope.OwnerTeamID = *ownerTeamID
	}
	if bucketID != nil {
		scope.BucketID = *bucketID
	}
	if name != nil {
		scope.Name = *name
	}
	if version != nil {
		scope.Version = *version
	}
	if configKey != nil {
		scope.ConfigKey = *configKey
	}
}

func (s *postgresStore) GetAppRuntime(ctx context.Context, appID uuid.UUID) (*AppRuntime, error) {
	query := `
		SELECT ` + appRuntimeSelectColumns + `
		FROM fused_apps a
		JOIN fused_app_families f ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
		LEFT JOIN fused_app_family_buckets fb ON fb.app_family_id = f.app_family_id
		WHERE a.app_id = $1 AND a.status IN ('active', 'deprecated')
	`
	scope, err := scanAppRuntime(s.db.QueryRow(ctx, query, appID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAppRuntimeNotFound
		}
		return nil, err
	}
	// Exact runtime reads fail closed before callers can cache or execute a row
	// whose removed selection field decoded as a zero value.
	if _, err := models.DecodeAppSelections(scope.ScopeSchemaVersion, scope.Selections); err != nil {
		return nil, err
	}
	return scope, nil
}

func (s *postgresStore) ListAppRuntimes(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]*AppRuntime, error) {
	out := make(map[uuid.UUID]*AppRuntime)
	if len(appIDs) == 0 {
		return out, nil
	}
	// The bucket belongs to the family, so this batch projects it alongside each
	// exact app version without issuing a query per app.
	query := `
		SELECT ` + appRuntimeSelectColumns + `
		FROM fused_apps a
		JOIN fused_app_families f ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
		LEFT JOIN fused_app_family_buckets fb ON fb.app_family_id = f.app_family_id
		WHERE a.app_id = ANY($1::uuid[]) AND a.status IN ('active', 'deprecated')
	`
	rows, err := s.db.Query(ctx, query, appIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		scope, err := scanAppRuntime(rows)
		if err != nil {
			return nil, err
		}
		out[scope.AppID] = scope
	}
	return out, rows.Err()
}

// ListMCPAppsByAccount is the read side of the MCP servers list page: every
// kind='mcp' scope owned by accountID, newest first, paginated, plus the
// total count. The window aggregate keeps pagination to one bounded query.
func (s *postgresStore) ListMCPAppsByAccount(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]AppRuntime, int, error) {
	return s.ListAuthorizedMCPAppsByAccount(ctx, accountID, accesscontrol.AuthorizedScope{All: true}, limit, offset)
}

func (s *postgresStore) ListAuthorizedMCPAppsByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]AppRuntime, int, error) {
	return s.ListAuthorizedAppRuntimesByAccount(ctx, accountID, scope, AppKindMCP.String(), limit, offset)
}

func (s *postgresStore) ListAuthorizedAppRuntimesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind string, limit, offset int) ([]AppRuntime, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	kind, valid := normalizeAppKind(kind)
	if !valid {
		return nil, 0, ErrInvalidAppKind
	}
	query := `
		SELECT ` + appRuntimeSelectColumns + `, COUNT(*) OVER()
		FROM fused_apps a
		JOIN fused_app_families f ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
		LEFT JOIN fused_app_family_buckets fb ON fb.app_family_id = f.app_family_id
		WHERE a.account_id = $1 AND ($2 = '' OR f.kind = $2)
		  AND a.status IN ('active', 'deprecated')
		  AND ($3 OR a.app_family_id = ANY($4::uuid[]))
		ORDER BY a.created_at DESC
		LIMIT $5 OFFSET $6
	`
	rows, err := s.db.Query(ctx, query, accountID, kind, scope.All, scope.IDs, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	scopes := make([]AppRuntime, 0, limit)
	total := 0
	for rows.Next() {
		scope, count, err := scanAppRuntimeWithTotal(rows)
		if err != nil {
			return nil, 0, err
		}
		total = count
		scopes = append(scopes, *scope)
	}
	return scopes, total, rows.Err()
}

var _ AppRuntimePageRepository = (*postgresStore)(nil)

func (s *postgresStore) GetMCPAppByName(ctx context.Context, accountID uuid.UUID, name, version string) (*AppRuntime, error) {
	canonicalName, _, err := canonical.AppName(name)
	if err != nil {
		return nil, ErrAppRuntimeNotFound
	}
	query := `
		SELECT ` + appRuntimeSelectColumns + `
		FROM fused_apps a
		JOIN fused_app_families f ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
		LEFT JOIN fused_app_family_buckets fb ON fb.app_family_id = f.app_family_id
		WHERE a.account_id = $1 AND f.kind = 'mcp' AND f.canonical_name = $2
		  AND a.status IN ('active', 'deprecated')
	`
	args := []interface{}{accountID, canonicalName}
	if version != "" {
		query += ` AND a.version = $3`
		args = append(args, version)
	}
	query += ` ORDER BY a.created_at DESC LIMIT 1`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, ErrAppRuntimeNotFound
	}
	return scanAppRuntime(rows)
}

func (s *postgresStore) GetSDKAccountID(ctx context.Context, appID uuid.UUID) (uuid.UUID, error) {
	var accountID uuid.UUID
	err := s.db.QueryRow(ctx, "SELECT account_id FROM fused_apps WHERE app_id = $1 AND status IN ('active', 'deprecated')", appID).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, errors.New("sdk not found")
		}
		return uuid.Nil, fmt.Errorf("query sdk account error: %w", err)
	}
	return accountID, nil
}

// VerifyWorkspaceOwner returns nil if the Engine's singleton workspace
// belongs to the authenticated Registry account.
func (s *postgresStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	ownerID, err := s.getSingletonWorkspace(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("Engine workspace is not initialized")
		}
		return fmt.Errorf("VerifyWorkspaceOwner: %w", err)
	}
	if err := validateWorkspaceOwner(accountID, ownerID); err != nil {
		return err
	}
	return nil
}

// UpsertMCPSession merges lifecycle replay without moving the initial peer or regressing producer chronology.
func (s *postgresStore) UpsertMCPSession(ctx context.Context, session *models.MCPSession) error {
	query := `
		INSERT INTO fused_mcp_sessions
			(id, app_id, app_token_id, session_id, protocol_version, started_at, last_activity_at, ended_at, end_reason,
			 client_name, client_version, initial_client_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, NULLIF($12, '')::inet)
		ON CONFLICT (id) DO UPDATE SET
			app_token_id = COALESCE(fused_mcp_sessions.app_token_id, EXCLUDED.app_token_id),
			protocol_version = EXCLUDED.protocol_version,
			started_at = LEAST(fused_mcp_sessions.started_at, EXCLUDED.started_at),
			client_name = COALESCE(NULLIF(fused_mcp_sessions.client_name, ''), EXCLUDED.client_name),
			client_version = COALESCE(NULLIF(fused_mcp_sessions.client_version, ''), EXCLUDED.client_version),
			initial_client_ip = COALESCE(fused_mcp_sessions.initial_client_ip, EXCLUDED.initial_client_ip),
			last_activity_at = GREATEST(fused_mcp_sessions.last_activity_at, EXCLUDED.last_activity_at),
			ended_at = COALESCE(EXCLUDED.ended_at, fused_mcp_sessions.ended_at),
			end_reason = COALESCE(EXCLUDED.end_reason, fused_mcp_sessions.end_reason)
	`
	_, err := s.db.Exec(ctx, query, session.ID, session.AppID, nullableUUID(session.AppTokenID),
		session.SessionID, session.ProtocolVersion, session.StartedAt, session.LastActivityAt,
		session.EndedAt, session.EndReason, session.ClientName, session.ClientVersion, session.InitialClientIP)
	return err
}

// GetMCPAnalyticsDashboard uses SQL aggregation over canonical execution
// events. Session lifecycle remains a separate concern because a connection
// is not an execution and has different retention and update semantics.
func (s *postgresStore) GetMCPAnalyticsDashboard(ctx context.Context, appID uuid.UUID) (*models.MCPAnalyticsDashboard, error) {
	dashboard := &models.MCPAnalyticsDashboard{}

	totalsQuery := `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(AVG(latency_ms), 0)
		FROM fused_engine_execution_events WHERE app_id = $1 AND transport = 'mcp' AND execution_kind = 'physical'
	`
	if err := s.db.QueryRow(ctx, totalsQuery, appID).Scan(&dashboard.TotalRequests, &dashboard.FailedRequests, &dashboard.AverageLatencyMs); err != nil {
		return nil, fmt.Errorf("query mcp analytics totals: %w", err)
	}

	activeQuery := `SELECT COUNT(*) FROM fused_mcp_sessions WHERE app_id = $1 AND ended_at IS NULL`
	if err := s.db.QueryRow(ctx, activeQuery, appID).Scan(&dashboard.ActiveAgents); err != nil {
		return nil, fmt.Errorf("query mcp active agents: %w", err)
	}

	toolUsage, err := queryMCPToolUsage(ctx, s.db, appID)
	if err != nil {
		return nil, err
	}
	dashboard.ToolUsage = toolUsage

	serviceUsage, err := queryMCPServiceUsage(ctx, s.db, appID)
	if err != nil {
		return nil, err
	}
	dashboard.ServiceUsage = serviceUsage

	recentSessions, err := queryRecentMCPSessions(ctx, s.db, appID)
	if err != nil {
		return nil, err
	}
	dashboard.RecentSessions = recentSessions

	return dashboard, nil
}

// queryMCPToolUsage counts provider receipts, not logical wrappers that already own children.
func queryMCPToolUsage(ctx context.Context, db *pgxpool.Pool, appID uuid.UUID) ([]models.MCPToolUsage, error) {
	query := `
		SELECT endpoint_name, COUNT(*), COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(AVG(latency_ms), 0)
		FROM fused_engine_execution_events
		WHERE app_id = $1 AND transport = 'mcp' AND endpoint_name <> '' AND execution_kind = 'physical'
		GROUP BY endpoint_name
		ORDER BY COUNT(*) DESC
	`
	rows, err := db.Query(ctx, query, appID)
	if err != nil {
		return nil, fmt.Errorf("query mcp tool usage: %w", err)
	}
	defer rows.Close()

	var usage []models.MCPToolUsage
	for rows.Next() {
		var u models.MCPToolUsage
		if err := rows.Scan(&u.ToolName, &u.Count, &u.Failed, &u.AverageLatencyMs); err != nil {
			return nil, fmt.Errorf("scan mcp tool usage: %w", err)
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

// queryMCPServiceUsage preserves provider-only analytics after adding logical history.
func queryMCPServiceUsage(ctx context.Context, db *pgxpool.Pool, appID uuid.UUID) ([]models.MCPServiceUsage, error) {
	query := `
		SELECT COALESCE(workspace_service.service_name, event.service_id::text), COUNT(*),
			COUNT(*) FILTER (WHERE event.status = 'failed'), COALESCE(AVG(event.latency_ms), 0)
		FROM fused_engine_execution_events event
		LEFT JOIN fused_workspace_services workspace_service ON workspace_service.service_id = event.service_id
		WHERE event.app_id = $1 AND event.transport = 'mcp' AND event.service_id IS NOT NULL AND event.execution_kind = 'physical'
		GROUP BY COALESCE(workspace_service.service_name, event.service_id::text)
		ORDER BY COUNT(*) DESC
	`
	rows, err := db.Query(ctx, query, appID)
	if err != nil {
		return nil, fmt.Errorf("query mcp service usage: %w", err)
	}
	defer rows.Close()

	var usage []models.MCPServiceUsage
	for rows.Next() {
		var u models.MCPServiceUsage
		if err := rows.Scan(&u.ServiceName, &u.Count, &u.Failed, &u.AverageLatencyMs); err != nil {
			return nil, fmt.Errorf("scan mcp service usage: %w", err)
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

// queryRecentMCPSessions caps at 10 rows -- this backs a UI summary panel,
// not a full session history browser.
func queryRecentMCPSessions(ctx context.Context, db *pgxpool.Pool, appID uuid.UUID) ([]models.MCPSession, error) {
	query := `
		SELECT id, app_id, COALESCE(app_token_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       session_id, protocol_version, started_at, last_activity_at, ended_at, COALESCE(end_reason, ''),
		       client_name, client_version, COALESCE(host(initial_client_ip), '')
		FROM fused_mcp_sessions
		WHERE app_id = $1
		ORDER BY started_at DESC, id DESC
		LIMIT 10
	`
	rows, err := db.Query(ctx, query, appID)
	// A failed summary query must not appear as an empty session history.
	if err != nil {
		return nil, fmt.Errorf("query recent mcp sessions: %w", err)
	}
	defer rows.Close()

	return collectMCPSessionRows(rows)
}

// BatchCreateEngineExecutionEvents persists canonical provider and logical receipts in one bounded worker batch.
func (s *postgresStore) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	// Empty flushes should not acquire a database connection.
	if len(events) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	query := `
		INSERT INTO fused_engine_execution_events (
			id, trace_id, span_id, account_id, app_family_id, app_id, app_token_id, app_version, transport,
			provider_protocol, direction, service_id, service_version_id, operation_id, webhook_id,
			endpoint_name, external_id, event_name, http_method, request_path, environment,
			environment_source, provider_host, provider_http_status, provider_status_class, status,
			failure_reason, failure_category, failure_code, latency_ms, provider_latency_ms, attempt_count,
			auth_scheme_names, auth_scheme_types, auth_scheme_count, auth_selection_outcome,
			pagination_type, pagination_page_count, pagination_item_count, pagination_byte_count, pagination_stop_reason,
			rate_limit_decision, rate_limit_policy_count, rate_limit_scope_kinds, rate_limit_units,
			rate_limit_unit_totals, rate_limit_retry_outcome, rate_limit_header_outcome,
			request_bytes, response_bytes, verification_status, delivery_status, idempotency_key_hash,
			request_body_hash, idempotency_replayed, timings, started_at, ended_at, created_at,
			execution_kind, parent_execution_id, unified_target, execution_phase, unified_steps
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
			$21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40,
			$41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, $59,
			$60, $61, $62, $63, $64)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			failure_reason = EXCLUDED.failure_reason,
			failure_category = EXCLUDED.failure_category,
			failure_code = EXCLUDED.failure_code,
			latency_ms = EXCLUDED.latency_ms,
			attempt_count = EXCLUDED.attempt_count,
			auth_scheme_names = EXCLUDED.auth_scheme_names,
			auth_scheme_types = EXCLUDED.auth_scheme_types,
			auth_scheme_count = EXCLUDED.auth_scheme_count,
			auth_selection_outcome = EXCLUDED.auth_selection_outcome,
			pagination_type = EXCLUDED.pagination_type,
			pagination_page_count = EXCLUDED.pagination_page_count,
			pagination_item_count = EXCLUDED.pagination_item_count,
			pagination_byte_count = EXCLUDED.pagination_byte_count,
			pagination_stop_reason = EXCLUDED.pagination_stop_reason,
			rate_limit_decision = EXCLUDED.rate_limit_decision,
			rate_limit_policy_count = EXCLUDED.rate_limit_policy_count,
			rate_limit_scope_kinds = EXCLUDED.rate_limit_scope_kinds,
			rate_limit_units = EXCLUDED.rate_limit_units,
			rate_limit_unit_totals = EXCLUDED.rate_limit_unit_totals,
			rate_limit_retry_outcome = EXCLUDED.rate_limit_retry_outcome,
			rate_limit_header_outcome = EXCLUDED.rate_limit_header_outcome,
			request_bytes = EXCLUDED.request_bytes,
			response_bytes = EXCLUDED.response_bytes,
			verification_status = EXCLUDED.verification_status,
			delivery_status = EXCLUDED.delivery_status,
			ended_at = EXCLUDED.ended_at
	`
	for _, event := range events {
		// Validate before queuing so one malformed event cannot partially persist a batch.
		if err := validateExecutionEventIdentity(event); err != nil {
			return err
		}
		b.Queue(query,
			event.ID, event.TraceID, event.SpanID, nullableUUID(event.AccountID), nullableUUID(event.AppFamilyID),
			nullableUUID(event.AppID), nullableUUID(event.AppTokenID), event.AppVersion, event.Transport, event.ProviderProtocol, event.Direction,
			nullableUUID(event.ServiceID), event.ServiceVersionID, nullableUUID(event.OperationID), nullableUUID(event.WebhookID),
			event.EndpointName, event.ExternalID, event.EventName, event.HTTPMethod, event.RequestPath, event.Environment,
			event.EnvironmentSource, event.ProviderHost, event.ProviderHTTPStatus, event.ProviderStatusClass, event.Status,
			event.FailureReason, event.FailureCategory, event.FailureCode, event.LatencyMs, event.ProviderLatencyMs,
			event.AttemptCount, nonNilStrings(event.AuthSchemeNames), nonNilStrings(event.AuthSchemeTypes), event.AuthSchemeCount, event.AuthSelectionOutcome,
			event.PaginationType, event.PaginationPageCount, event.PaginationItemCount,
			event.PaginationByteCount, event.PaginationStopReason,
			event.RateLimitDecision, event.RateLimitPolicyCount, nonNilStrings(event.RateLimitScopeKinds), nonNilStrings(event.RateLimitUnits),
			nonNilInt64s(event.RateLimitUnitTotals), event.RateLimitRetryOutcome, event.RateLimitHeaderOutcome,
			event.RequestBytes, event.ResponseBytes, event.VerificationStatus, event.DeliveryStatus,
			event.IdempotencyKeyHash, event.RequestBodyHash, event.IdempotencyReplayed, event.Timings, event.StartedAt,
			event.EndedAt, event.CreatedAt,
			executionevent.Kind(event), nullableUUID(event.ParentExecutionID), event.UnifiedTarget, event.ExecutionPhase,
			append([]models.UnifiedExecutionStep{}, event.UnifiedSteps...),
		)
	}
	results := s.db.SendBatch(ctx, b)
	return results.Close()
}

func nonNilInt64s(values []int64) []int64 {
	if values == nil {
		return []int64{}
	}
	return values
}

// validateExecutionEventIdentity keeps all app-authenticated transports on the
// same exact family/app/version receipt invariant.
func validateExecutionEventIdentity(event models.EngineExecutionEvent) error {
	// Durable replay must apply the same bounded logical metadata rules as producers.
	if err := executionevent.ValidateUnifiedMetadata(event); err != nil {
		return err
	}
	// Webhooks retain their existing registration-scoped identity contract.
	if event.Transport != models.EngineExecutionTransportSDK && event.Transport != models.EngineExecutionTransportMCP &&
		event.Transport != models.EngineExecutionTransportREST {
		return nil
	}
	// App identity is mandatory even when a logical parent has no service identity.
	if event.AppFamilyID == uuid.Nil || event.AppID == uuid.Nil || strings.TrimSpace(event.AppVersion) == "" {
		return fmt.Errorf("%s execution event requires app family, app, and version identity", event.Transport)
	}
	// Match the persisted immutable version bound.
	if len([]rune(event.AppVersion)) > 128 {
		return errors.New("execution event app version exceeds 128 characters")
	}
	return nil
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func (s *postgresStore) DeleteEngineExecutionEventsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	result, err := s.db.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM fused_engine_execution_events
			WHERE started_at < $1
			ORDER BY started_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM fused_engine_execution_events event
		USING expired
		WHERE event.id = expired.id`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired execution events: %w", err)
	}
	return result.RowsAffected(), nil
}

type EngineExecutionFilter struct {
	ParentExecutionID uuid.UUID
	ReceiptRoots      bool
	AccountID         uuid.UUID
	ServiceID         uuid.UUID
	AppFamilyID       uuid.UUID
	AppID             uuid.UUID
	Transport         string
	Direction         string
	Status            string
	Limit             int
	Offset            int
	StartDate         *time.Time
	EndDate           *time.Time
}

// AppExecutionEventReader is optional so alternate Store implementations can
// adopt version-aware app activity without widening the core persistence contract.
type AppExecutionEventReader interface {
	ListEngineExecutionEventsByApp(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error)
}

type AppExecutionAnalyticsReader interface {
	GetEngineExecutionAnalyticsByApp(ctx context.Context, filter EngineExecutionFilter) (models.AppExecutionAnalytics, error)
}

// engineExecutionWhereClause keeps tenant/resource selection and accounting semantics in SQL.
func engineExecutionWhereClause(filter EngineExecutionFilter) (string, []any) {
	whereClause := "WHERE account_id = $1"
	args := []any{filter.AccountID}
	// App activity is family-scoped; service activity must never broaden to the workspace.
	if filter.AppFamilyID != uuid.Nil {
		whereClause += " AND app_family_id = $2"
		args = append(args, filter.AppFamilyID)
		// An exact app narrows the family when the caller did not request all versions.
		if filter.AppID != uuid.Nil {
			whereClause += " AND app_id = $3"
			args = append(args, filter.AppID)
		}
	} else if filter.ServiceID != uuid.Nil {
		// Service reads retain their exact resource predicate instead of relying
		// on application-side filtering after a broader account query.
		whereClause += " AND service_id = $2"
		args = append(args, filter.ServiceID)
	}
	// Keeping tenant and resource scope in the same SQL predicate prevents a
	// caller from receiving broad workspace data and filtering it in memory.
	argIdx := len(args) + 1
	// Optional dimensions stay database predicates to avoid loading unrelated receipts.
	if filter.Transport != "" {
		whereClause += fmt.Sprintf(" AND transport = $%d", argIdx)
		args = append(args, filter.Transport)
		argIdx++
	}
	// Inbound webhook and outbound execution histories remain independently selectable.
	if filter.Direction != "" {
		whereClause += fmt.Sprintf(" AND direction = $%d", argIdx)
		args = append(args, filter.Direction)
		argIdx++
	}
	// Status selection applies before pagination and counting.
	if filter.Status != "" {
		whereClause += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, filter.Status)
		argIdx++
	}
	// Bound the scan by the caller's requested interval.
	if filter.StartDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *filter.StartDate)
		argIdx++
	}
	// An omitted end keeps existing open-ended history behavior.
	if filter.EndDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *filter.EndDate)
	}
	return executionReceiptWhereClause(filter, whereClause, args)
}

func (s *postgresStore) ListEngineExecutionEventsByService(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filter.AppFamilyID = uuid.Nil
	filter.AppID = uuid.Nil
	return s.listEngineExecutionEvents(ctx, filter)
}

// ListEngineExecutionEventsByApp groups logical work without hiding physical receipts from service activity.
func (s *postgresStore) ListEngineExecutionEventsByApp(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filter.ServiceID = uuid.Nil
	filter.ReceiptRoots = true
	return s.listEngineExecutionEvents(ctx, filter)
}

// listEngineExecutionEvents pages before display joins, avoiding per-row service lookups.
func (s *postgresStore) listEngineExecutionEvents(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	var count int64
	// Count uses exactly the same scope as the page so parent grouping cannot skew pagination.
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM fused_engine_execution_events "+whereClause, args...).Scan(&count); err != nil {
		return nil, 0, err
	}

	argIdx := len(args) + 1
	// Page the canonical receipt rows before joining display metadata so query
	// cost remains bounded by the requested receipt limit rather than workspace size.
	query := `WITH event_page AS (
		SELECT * FROM fused_engine_execution_events ` + whereClause + fmt.Sprintf(" ORDER BY started_at DESC, id DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1) + `
	)
	SELECT event.id, COALESCE(event.trace_id, ''), COALESCE(event.span_id, ''), COALESCE(event.account_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(event.app_family_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(event.app_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(event.app_token_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(event.app_version, ''),
		event.transport, COALESCE(event.provider_protocol, ''), event.direction, COALESCE(event.service_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(event.service_version_id, ''),
		COALESCE(service.service_name, ''), COALESCE(service.service_slug, ''), COALESCE(version.version, ''),
		COALESCE(event.operation_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(event.webhook_id, '00000000-0000-0000-0000-000000000000'::uuid),
		event.endpoint_name, COALESCE(event.external_id, ''), COALESCE(event.event_name, ''), COALESCE(event.http_method, ''), COALESCE(event.request_path, ''), COALESCE(event.environment, ''),
		COALESCE(event.environment_source, ''), COALESCE(event.provider_host, ''), event.provider_http_status, COALESCE(event.provider_status_class, ''),
		event.status, COALESCE(event.failure_reason, ''), COALESCE(event.failure_category, ''), COALESCE(event.failure_code, ''), event.latency_ms, event.provider_latency_ms,
		event.attempt_count, event.auth_scheme_names, event.auth_scheme_types, event.auth_scheme_count, COALESCE(event.auth_selection_outcome, ''),
		COALESCE(event.pagination_type, ''), event.pagination_page_count, event.pagination_item_count, event.pagination_byte_count,
		COALESCE(event.pagination_stop_reason, ''), COALESCE(event.rate_limit_decision, ''), event.rate_limit_policy_count,
		event.rate_limit_scope_kinds, event.rate_limit_units, event.rate_limit_unit_totals,
		COALESCE(event.rate_limit_retry_outcome, ''), COALESCE(event.rate_limit_header_outcome, ''),
		event.request_bytes, event.response_bytes, COALESCE(event.verification_status, ''), COALESCE(event.delivery_status, ''),
		event.idempotency_replayed, COALESCE(event.timings, '{}'::jsonb), event.started_at, event.ended_at, event.created_at,
		event.execution_kind, COALESCE(event.parent_execution_id, '00000000-0000-0000-0000-000000000000'::uuid),
		event.unified_target, event.execution_phase, event.unified_steps
	FROM event_page event
	LEFT JOIN fused_workspace_services service ON service.service_id = event.service_id
	LEFT JOIN fused_workspace_service_versions version
		ON version.service_id = event.service_id AND version.service_version_id::text = event.service_version_id
	ORDER BY event.started_at DESC, event.id DESC`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.Query(ctx, query, args...)
	// A failed page read must not look like an empty history.
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	events := make([]models.EngineExecutionEvent, 0, filter.Limit)
	for rows.Next() {
		var event models.EngineExecutionEvent
		// Parent service identity is intentionally absent; the SELECT normalizes nullable UUIDs.
		if err := rows.Scan(
			&event.ID, &event.TraceID, &event.SpanID, &event.AccountID, &event.AppFamilyID, &event.AppID, &event.AppTokenID,
			&event.AppVersion, &event.Transport, &event.ProviderProtocol, &event.Direction, &event.ServiceID,
			&event.ServiceVersionID, &event.ServiceName, &event.ServiceSlug, &event.ServiceVersion,
			&event.OperationID, &event.WebhookID, &event.EndpointName,
			&event.ExternalID, &event.EventName, &event.HTTPMethod, &event.RequestPath, &event.Environment, &event.EnvironmentSource,
			&event.ProviderHost, &event.ProviderHTTPStatus, &event.ProviderStatusClass, &event.Status, &event.FailureReason,
			&event.FailureCategory, &event.FailureCode, &event.LatencyMs, &event.ProviderLatencyMs, &event.AttemptCount,
			&event.AuthSchemeNames, &event.AuthSchemeTypes, &event.AuthSchemeCount, &event.AuthSelectionOutcome,
			&event.PaginationType, &event.PaginationPageCount, &event.PaginationItemCount, &event.PaginationByteCount,
			&event.PaginationStopReason, &event.RateLimitDecision, &event.RateLimitPolicyCount,
			&event.RateLimitScopeKinds, &event.RateLimitUnits, &event.RateLimitUnitTotals,
			&event.RateLimitRetryOutcome, &event.RateLimitHeaderOutcome,
			&event.RequestBytes, &event.ResponseBytes, &event.VerificationStatus, &event.DeliveryStatus,
			&event.IdempotencyReplayed, &event.Timings, &event.StartedAt, &event.EndedAt, &event.CreatedAt,
			&event.ExecutionKind, &event.ParentExecutionID, &event.UnifiedTarget, &event.ExecutionPhase, &event.UnifiedSteps,
		); err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	// Never return a partial page after a cursor or transport failure.
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, count, nil
}

func (s *postgresStore) GetEngineExecutionAnalyticsByService(ctx context.Context, filter EngineExecutionFilter) (models.EngineExecutionAnalytics, error) {
	filter.AppFamilyID = uuid.Nil
	filter.AppID = uuid.Nil
	return s.getEngineExecutionAnalytics(ctx, filter)
}

func (s *postgresStore) GetEngineExecutionAnalyticsByApp(ctx context.Context, filter EngineExecutionFilter) (models.AppExecutionAnalytics, error) {
	filter.ServiceID = uuid.Nil
	summary, err := s.getEngineExecutionAnalytics(ctx, filter)
	if err != nil {
		return models.AppExecutionAnalytics{}, err
	}
	byService, err := s.getAppServiceExecutionBreakdown(ctx, filter)
	if err != nil {
		return models.AppExecutionAnalytics{}, err
	}
	return models.AppExecutionAnalytics{EngineExecutionAnalytics: summary, ByService: byService}, nil
}

func (s *postgresStore) getEngineExecutionAnalytics(ctx context.Context, filter EngineExecutionFilter) (models.EngineExecutionAnalytics, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	query := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status = 'success'),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COALESCE(AVG(latency_ms), 0),
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM fused_engine_execution_events ` + whereClause
	var analytics models.EngineExecutionAnalytics
	err := s.db.QueryRow(ctx, query, args...).Scan(
		&analytics.TotalCalls,
		&analytics.SuccessfulCalls,
		&analytics.FailedCalls,
		&analytics.AverageLatencyMs,
		&analytics.MedianLatencyMs,
		&analytics.P95LatencyMs,
	)
	return analytics, err
}

func (s *postgresStore) getAppServiceExecutionBreakdown(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionBreakdown, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	// Keeping grouping in SQL avoids loading individual receipts just to count
	// them in Go and makes the query count independent of bundled service count.
	query := `SELECT service_id::text, service_id::text, COUNT(*),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM fused_engine_execution_events ` + whereClause + `
		GROUP BY service_id ORDER BY COUNT(*) DESC`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]models.EngineExecutionBreakdown, 0)
	for rows.Next() {
		var item models.EngineExecutionBreakdown
		if err := rows.Scan(&item.Key, &item.Label, &item.TotalCalls, &item.FailedCalls, &item.P95LatencyMs); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) ListWebhookEventsByService(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, limit, offset int, startDate, endDate *time.Time) ([]models.WebhookEvent, int64, error) {
	whereClause := "WHERE account_id = $1 AND service_id = $2 AND transport = 'webhook'"
	args := []any{accountID, serviceID}
	argIdx := 3

	if eventName != "" {
		whereClause += fmt.Sprintf(" AND event_name = $%d", argIdx)
		args = append(args, eventName)
		argIdx++
	}
	if startDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *startDate)
		argIdx++
	}
	if endDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *endDate)
		argIdx++
	}

	countQuery := "SELECT COUNT(*) FROM fused_engine_execution_events " + whereClause
	var count int64
	if err := s.db.QueryRow(ctx, countQuery, args...).Scan(&count); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, account_id, service_id, COALESCE(external_id, ''), COALESCE(event_name, ''),
		COALESCE(failure_reason, ''), COALESCE(app_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(verification_status, ''), COALESCE(delivery_status, ''),
		COALESCE(environment, ''), latency_ms::integer, GREATEST(attempt_count - 1, 0), 0::double precision,
		request_bytes::integer, started_at FROM fused_engine_execution_events ` + whereClause + fmt.Sprintf(" ORDER BY started_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[models.WebhookEvent])
	if err != nil {
		return nil, 0, err
	}
	return events, count, nil
}

func (s *postgresStore) GetWebhookAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) (models.WebhookAnalytics, error) {
	whereClause := "WHERE account_id = $1 AND service_id = $2 AND transport = 'webhook'"
	args := []any{accountID, serviceID}
	argIdx := 3

	if eventName != "" {
		whereClause += fmt.Sprintf(" AND event_name = $%d", argIdx)
		args = append(args, eventName)
		argIdx++
	}
	if startDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *startDate)
		argIdx++
	}
	if endDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *endDate)
		argIdx++
	}

	var analytics models.WebhookAnalytics
	query := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE delivery_status IN ('success', 'delivered')),
		COUNT(*) FILTER (WHERE delivery_status = 'rejected'),
		COUNT(*) FILTER (WHERE delivery_status = 'failed')
		FROM fused_engine_execution_events ` + whereClause
	err := s.db.QueryRow(ctx, query, args...).Scan(
		&analytics.TotalIngested, &analytics.TotalDelivered, &analytics.TotalRejected, &analytics.TotalFailed,
	)
	return analytics, err
}

// GetIdempotentExecution looks up a cached response for (appID,
// idempotencyKeyHash). expires_at > NOW() makes TTL expiry a plain read-time
// filter -- expired rows just stop matching, no sweep job required for
// correctness (though one could trim storage over time as a follow-up).
func (s *postgresStore) GetIdempotentExecution(ctx context.Context, appID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	query := `
		SELECT id, app_id, idempotency_key_hash, request_body_hash, environment, response_body, response_status, response_media_family, created_at, expires_at
		FROM fused_engine_idempotency_keys
		WHERE app_id = $1 AND idempotency_key_hash = $2 AND expires_at > NOW()
	`
	var exec models.IdempotentExecution
	err := s.db.QueryRow(ctx, query, appID, idempotencyKeyHash).Scan(
		&exec.ID, &exec.AppID, &exec.IdempotencyKeyHash, &exec.RequestBodyHash,
		&exec.Environment, &exec.ResponseBody, &exec.ResponseStatus, &exec.ResponseMediaFamily, &exec.CreatedAt, &exec.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIdempotentExecutionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get idempotent execution: %w", err)
	}
	if requestBodyHash != "" && exec.RequestBodyHash != "" && exec.RequestBodyHash != requestBodyHash {
		return nil, ErrIdempotencyKeyConflict
	}
	return &exec, nil
}

// SaveIdempotentExecution caches a successful execution's response.
// ON CONFLICT DO NOTHING: if two concurrent requests with the same key both
// reach here, the first write wins and the second is a harmless no-op --
// both callers already have their (equivalent) response in hand from their
// own dispatch, this table only serves future lookups.
func (s *postgresStore) SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error {
	_, err := s.db.Exec(ctx, saveIdempotentExecutionQuery, idempotentExecutionInsertArgs(exec)...)
	return err
}

const saveIdempotentExecutionQuery = `
		INSERT INTO fused_engine_idempotency_keys
			(id, app_id, idempotency_key_hash, request_body_hash, environment, response_body, response_status, response_media_family, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9)
		ON CONFLICT (app_id, idempotency_key_hash) DO NOTHING
	`

// idempotentExecutionInsertArgs keeps INSERT argument order aligned with the persisted replay media contract.
func idempotentExecutionInsertArgs(exec *models.IdempotentExecution) []any {
	return []any{
		exec.ID,
		exec.AppID,
		exec.IdempotencyKeyHash,
		exec.RequestBodyHash,
		exec.Environment,
		exec.ResponseBody,
		exec.ResponseStatus,
		exec.ResponseMediaFamily,
		exec.ExpiresAt,
	}
}

func (s *postgresStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	query := `
		DELETE FROM fused_workspace_secrets 
		WHERE bucket_id = $1 
		AND service_id = $2 
		AND key_name = $3 
	`
	_, err := s.db.Exec(ctx, query, bucketID, serviceID, keyName)
	return err
}

func (s *postgresStore) ListSecretMeta(ctx context.Context, bucketID uuid.UUID) ([]WorkspaceSecretMeta, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1
	`
	rows, err := s.db.Query(ctx, query, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSecretMetas(rows)
}

const appBucketCredentialPresenceSQL = `
	WITH requirements AS (
		SELECT
			requirement.service_id,
			LOWER(REPLACE(BTRIM(requirement.auth_type), '-', '_')) AS auth_type,
			BTRIM(requirement.auth_name) AS auth_name,
			COALESCE(requirement.secret_keys, '[]'::jsonb) AS secret_keys
		FROM jsonb_to_recordset($2::jsonb) AS requirement(
			service_id uuid,
			auth_type text,
			auth_name text,
			secret_keys jsonb
		)
	)
	SELECT
		requirement.service_id,
		requirement.auth_type,
		requirement.auth_name,
		EXISTS (
			SELECT 1
			FROM fused_connect_configs config
			WHERE config.bucket_id = $1
			  AND config.service_id = requirement.service_id
			  AND config.enabled = TRUE
			  AND LOWER(REPLACE(BTRIM(config.auth_type), '-', '_')) = requirement.auth_type
			  AND BTRIM(config.auth_name) = requirement.auth_name
		) AS connected,
		COALESCE(ARRAY(
			SELECT requested_key.key_name
			FROM jsonb_array_elements_text(requirement.secret_keys) AS requested_key(key_name)
			WHERE EXISTS (
				SELECT 1
				FROM fused_workspace_secrets secret
				WHERE secret.bucket_id = $1
				  AND secret.service_id = requirement.service_id
				  AND secret.key_name = requested_key.key_name
			)
			ORDER BY requested_key.key_name
		), ARRAY[]::text[]) AS secret_keys
	FROM requirements requirement
	ORDER BY requirement.service_id, requirement.auth_type, requirement.auth_name`

// GetAppBucketCredentialPresence selects only the requested credential tuples
// and metadata. Keeping matching in SQL prevents a large bucket from being
// copied into Engine memory merely to answer one app plan.
func (s *postgresStore) GetAppBucketCredentialPresence(ctx context.Context, bucketID uuid.UUID, requirements []AppCredentialRequirement) ([]AppCredentialPresence, error) {
	if len(requirements) == 0 {
		return []AppCredentialPresence{}, nil
	}
	payload, err := json.Marshal(requirements)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, appBucketCredentialPresenceSQL, bucketID, payload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	presence := make([]AppCredentialPresence, 0, len(requirements))
	for rows.Next() {
		var item AppCredentialPresence
		if err := rows.Scan(&item.ServiceID, &item.AuthType, &item.AuthName, &item.Connected, &item.SecretKeys); err != nil {
			return nil, err
		}
		presence = append(presence, item)
	}
	return presence, rows.Err()
}

func (s *postgresStore) ListSecretMetaPage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]WorkspaceSecretMeta, int, error) {
	const credentialRows = `
		SELECT *, CASE
			WHEN LOWER(REPLACE(credential_type, '-', '_')) = 'basic' THEN REGEXP_REPLACE(key_name, '_(username|password)$', '')
			WHEN LOWER(REPLACE(credential_type, '-', '_')) IN ('mtls', 'mutualtls', 'mutual_tls') THEN REGEXP_REPLACE(key_name, '_(cert|key)$', '')
			ELSE key_name
		END AS family_key
		FROM fused_workspace_secrets WHERE bucket_id = $1`
	var total int
	countQuery := `SELECT COUNT(*) FROM (
		SELECT service_id, family_key, credential_type FROM (` + credentialRows + `) rows
		GROUP BY service_id, family_key, credential_type
	) credential_families`
	if err := s.db.QueryRow(ctx, countQuery, bucketID).Scan(&total); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	query := `
		WITH credential_rows AS (` + credentialRows + `)
		SELECT (ARRAY_AGG(id ORDER BY id))[1],  bucket_id, service_id, family_key,
		       credential_type, MAX(last_used_at), MIN(expires_at), MIN(created_at), MAX(updated_at),
		       ARRAY_AGG(key_name ORDER BY key_name)
		FROM credential_rows
		GROUP BY  bucket_id, service_id, family_key, credential_type
		ORDER BY MAX(updated_at) DESC, family_key ASC
		LIMIT $2 OFFSET $3
	`
	rows, err := s.db.Query(ctx, query, bucketID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	metas, err := collectSecretCredentialMetas(rows)
	return metas, total, err
}

func collectSecretCredentialMetas(rows pgx.Rows) ([]WorkspaceSecretMeta, error) {
	var metas []WorkspaceSecretMeta
	for rows.Next() {
		var meta WorkspaceSecretMeta
		if err := rows.Scan(
			&meta.ID, &meta.BucketID, &meta.ServiceID, &meta.KeyName, &meta.CredentialType,
			&meta.LastUsedAt, &meta.ExpiresAt, &meta.CreatedAt, &meta.UpdatedAt, &meta.KeyNames,
		); err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func collectSecretMetas(rows pgx.Rows) ([]WorkspaceSecretMeta, error) {
	var metas []WorkspaceSecretMeta
	for rows.Next() {
		var meta WorkspaceSecretMeta
		if err := rows.Scan(
			&meta.ID, &meta.BucketID, &meta.ServiceID, &meta.KeyName, &meta.CredentialType,
			&meta.LastUsedAt, &meta.ExpiresAt, &meta.CreatedAt, &meta.UpdatedAt,
		); err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func (s *postgresStore) ListSecretsForBucket(ctx context.Context, bucketID, serviceID uuid.UUID) ([]WorkspaceSecret, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2
	`
	rows, err := s.db.Query(ctx, query, bucketID, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

func (s *postgresStore) GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*WorkspaceSecret, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2 AND key_name = $3
	`
	var sec WorkspaceSecret
	err := s.db.QueryRow(ctx, query, bucketID, serviceID, keyName).Scan(
		&sec.ID, &sec.BucketID, &sec.ServiceID, &sec.KeyName, &sec.CredentialType,
		&sec.EncryptedDEK, &sec.EncryptedValue, &sec.LastUsedAt, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sec, nil
}

func (s *postgresStore) GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, error) {
	keyNames = uniqueSecretKeyNames(keyNames)
	if len(keyNames) == 0 {
		return nil, nil
	}
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)
	`
	rows, err := s.db.Query(ctx, query, bucketID, serviceID, keyNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

const firstCompleteSecretSetSQL = `
	WITH alternatives AS (
		SELECT ordinality, value
		FROM jsonb_array_elements($3::jsonb) WITH ORDINALITY
	), selected AS (
		SELECT value
		FROM alternatives candidate
		WHERE NOT EXISTS (
			SELECT 1
			FROM jsonb_array_elements_text(candidate.value->'required') required_key
			WHERE NOT EXISTS (
				SELECT 1 FROM fused_workspace_secrets secret
				WHERE secret.bucket_id = $1 AND secret.service_id = $2
				  AND secret.key_name = required_key
				  AND (secret.expires_at IS NULL OR secret.expires_at > NOW())
			)
		)
		ORDER BY ordinality
		LIMIT 1
	), selected_keys AS (
		SELECT jsonb_array_elements_text(
			COALESCE(value->'required', '[]'::jsonb) || COALESCE(value->'optional', '[]'::jsonb)
		) AS key_name
		FROM selected
	)
	SELECT secret.id, secret.bucket_id, secret.service_id, secret.key_name,
	       secret.credential_type, secret.encrypted_dek, secret.encrypted_value,
	       secret.last_used_at, secret.expires_at, secret.created_at, secret.updated_at
	FROM fused_workspace_secrets secret
	JOIN selected_keys selected_key ON selected_key.key_name = secret.key_name
	WHERE secret.bucket_id = $1 AND secret.service_id = $2
	  AND (secret.expires_at IS NULL OR secret.expires_at > NOW())
`

func (s *postgresStore) GetFirstCompleteSecretSet(ctx context.Context, bucketID, serviceID uuid.UUID, alternatives []SecretKeyAlternative) ([]WorkspaceSecret, error) {
	if len(alternatives) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(alternatives)
	if err != nil {
		return nil, fmt.Errorf("encode ordered secret alternatives: %w", err)
	}
	rows, err := s.db.Query(ctx, firstCompleteSecretSetSQL, bucketID, serviceID, payload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

// uniqueSecretKeyNames keeps exact-key queries compact without changing the
// caller's security boundary: every returned row still matches bucket+service.
func uniqueSecretKeyNames(keyNames []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keyNames))
	for _, keyName := range keyNames {
		if keyName == "" || seen[keyName] {
			continue
		}
		seen[keyName] = true
		out = append(out, keyName)
	}
	return out
}

func (s *postgresStore) ListSecretsForBuckets(ctx context.Context, bucketIDs []uuid.UUID, serviceID uuid.UUID) ([]WorkspaceSecret, error) {
	if len(bucketIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = ANY($1) AND service_id = $2
	`
	rows, err := s.db.Query(ctx, query, bucketIDs, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

type workspaceSecretRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanWorkspaceSecrets(rows workspaceSecretRows) ([]WorkspaceSecret, error) {
	var secrets []WorkspaceSecret
	for rows.Next() {
		sec, err := scanWorkspaceSecret(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, sec)
	}
	return secrets, rows.Err()
}

func scanWorkspaceSecret(rows workspaceSecretRows) (WorkspaceSecret, error) {
	var sec WorkspaceSecret
	err := rows.Scan(
		&sec.ID, &sec.BucketID, &sec.ServiceID,
		&sec.KeyName, &sec.CredentialType, &sec.EncryptedDEK, &sec.EncryptedValue,
		&sec.LastUsedAt, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt,
	)
	return sec, err
}
