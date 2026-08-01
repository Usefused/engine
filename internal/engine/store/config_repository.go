package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/secretref"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type ConfigType string

const (
	ConfigTypeWorkspace ConfigType = "workspace"
	ConfigTypeSDK       ConfigType = "sdk"
	ConfigTypeMCP       ConfigType = "mcp"
	ConfigTypeWebhook   ConfigType = "webhook"
)

type ConfigPlanStatus string

const (
	ConfigPlanStatusPending    ConfigPlanStatus = "pending"
	ConfigPlanStatusApplied    ConfigPlanStatus = "applied"
	ConfigPlanStatusSuperseded ConfigPlanStatus = "superseded"
	ConfigPlanStatusStale      ConfigPlanStatus = "stale"
	ConfigPlanStatusFailed     ConfigPlanStatus = "failed"
)

type WorkspaceNotificationStatus string

const (
	WorkspaceNotificationStatusPending      WorkspaceNotificationStatus = "pending"
	WorkspaceNotificationStatusAcknowledged WorkspaceNotificationStatus = "acknowledged"
	WorkspaceNotificationStatusDismissed    WorkspaceNotificationStatus = "dismissed"
)

type WorkspaceNotificationSeverity string

const (
	WorkspaceNotificationSeverityBreaking WorkspaceNotificationSeverity = "breaking"
	// WorkspaceNotificationSeverityNonBreaking reuses models.DriftChange's
	// own two-value vocabulary ("breaking" | "non-breaking") rather than a
	// graduated scale -- see plans/plan-service-changelog.md's "## Phase 3"
	// for why every diff shape in this system already speaks that
	// vocabulary. Added for Phase 3's changelog-derived notifications,
	// which (unlike the two existing types below) can genuinely be
	// non-breaking (e.g. a new version, or a deprecation warning).
	WorkspaceNotificationSeverityNonBreaking WorkspaceNotificationSeverity = "non-breaking"
)

type WorkspaceNotificationType string

const (
	WorkspaceNotificationTypeServiceRemoved WorkspaceNotificationType = "workspace_service_removed"
	WorkspaceNotificationTypeVersionRemoved WorkspaceNotificationType = "workspace_version_removed"

	// The registry_* types below are Phase 3's changelog-derived
	// notifications (plans/plan-service-changelog.md's "## Phase 3") --
	// deliberately distinct from the two workspace_* types above, which
	// only fire when the user runs apply and explicitly force_removes a
	// service/version their own config still references (a decision-audit
	// record). registry_* notifications are proactive discoveries from the
	// Registry changelog feed, surfaced before the user ever syncs; the
	// prefix keeps the two mechanisms visually and semantically separable
	// in the notification list.
	WorkspaceNotificationTypeRegistryVersionAdded             WorkspaceNotificationType = "registry_version_added"
	WorkspaceNotificationTypeRegistryVersionChanged           WorkspaceNotificationType = "registry_version_changed"
	WorkspaceNotificationTypeRegistryVersionDeprecated        WorkspaceNotificationType = "registry_version_deprecated"
	WorkspaceNotificationTypeRegistryVersionRemoved           WorkspaceNotificationType = "registry_version_removed"
	WorkspaceNotificationTypeRegistryExecutionPolicyChanged   WorkspaceNotificationType = "registry_execution_policy_changed"
	WorkspaceNotificationTypeRegistryConnectionProfileChanged WorkspaceNotificationType = "registry_connection_profile_changed"
)

var (
	ErrConfigKeyRequired           = errors.New("config key is required")
	ErrConfigHashRequired          = errors.New("source hash is required")
	ErrConfigTypeInvalid           = errors.New("config type must be workspace, sdk, mcp, or webhook")
	ErrConfigJSONInvalid           = errors.New("config JSON payload is invalid")
	ErrConfigJSONObjectRequired    = errors.New("config JSON payload must be an object")
	ErrConfigJSONArrayRequired     = errors.New("config JSON payload must be an array")
	ErrRequiredPermissions         = errors.New("required permissions must contain at least one valid permission scope")
	ErrOwnerTeamRequired           = errors.New("owner team is required for artifact plans")
	ErrOwnerTeamUnexpected         = errors.New("owner team is not valid for workspace plans")
	ErrConfigPlanNotFound          = errors.New("config plan not found")
	ErrConfigStateIdentityMismatch = errors.New("config type and owner team are immutable")
	ErrConfigOwnerTeamInactive     = errors.New("config owner team is not active")
	ErrConfigPlanRevisionMismatch  = errors.New("config plan revision changed")
	ErrConfigPlanApplyInProgress   = errors.New("config plan apply is in progress")
	// ErrWorkspaceNotificationStatusInvalid guards
	// UpdateWorkspaceNotificationStatus's only two valid targets -- 'pending'
	// is set exclusively at creation (CreateWorkspaceNotification), never a
	// transition target.
	ErrWorkspaceNotificationStatusInvalid = errors.New("workspace notification status must be acknowledged or dismissed")
	ErrWorkspaceNotificationNotFound      = errors.New("workspace notification not found")
	// ErrWorkspaceNotificationImmutable means the row exists but is already
	// 'dismissed' -- Phase 4 (plans/plan-service-changelog.md) has no
	// "undismiss" path, dismissed is a terminal state.
	ErrWorkspaceNotificationImmutable = errors.New("workspace notification is dismissed and cannot be changed")
)

type ConfigState struct {
	ID               uuid.UUID       `json:"id"`
	ConfigKey        string          `json:"config_key"`
	ConfigType       ConfigType      `json:"config_type"`
	OwnerTeamID      *uuid.UUID      `json:"owner_team_id,omitempty"`
	SourceHash       string          `json:"source_hash"`
	Generation       int             `json:"generation"`
	DesiredState     json.RawMessage `json:"desired_state"`
	ManagedResources json.RawMessage `json:"managed_resources"`
	LatestResourceID *uuid.UUID      `json:"latest_resource_id,omitempty"`
	UpdatedBy        uuid.UUID       `json:"updated_by"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type ConfigPlan struct {
	ID                  uuid.UUID        `json:"id"`
	ConfigKey           string           `json:"config_key"`
	ConfigType          ConfigType       `json:"config_type"`
	OwnerTeamID         *uuid.UUID       `json:"owner_team_id,omitempty"`
	SourceHash          string           `json:"source_hash"`
	BaseGeneration      int              `json:"base_generation"`
	Status              ConfigPlanStatus `json:"status"`
	Actions             json.RawMessage  `json:"actions"`
	DesiredState        json.RawMessage  `json:"desired_state"`
	ResolvedPayload     json.RawMessage  `json:"resolved_payload"`
	Blockers            json.RawMessage  `json:"blockers"`
	Warnings            json.RawMessage  `json:"warnings"`
	RequiredPermissions json.RawMessage  `json:"required_permissions"`
	Revision            int              `json:"revision"`
	CreatedBy           uuid.UUID        `json:"created_by"`
	CreatedAt           time.Time        `json:"created_at"`
	AppliedAt           *time.Time       `json:"applied_at,omitempty"`
	SupersededAt        *time.Time       `json:"superseded_at,omitempty"`
}

type WorkspaceNotification struct {
	ID        uuid.UUID                     `json:"id"`
	Type      WorkspaceNotificationType     `json:"type"`
	Severity  WorkspaceNotificationSeverity `json:"severity"`
	Status    WorkspaceNotificationStatus   `json:"status"`
	ServiceID *uuid.UUID                    `json:"service_id,omitempty"`
	Version   string                        `json:"version,omitempty"`
	ConfigKey string                        `json:"config_key,omitempty"`
	Message   string                        `json:"message"`
	Metadata  json.RawMessage               `json:"metadata"`
	CreatedBy uuid.UUID                     `json:"created_by"`
	// ResolvedBy is nil until the notification's first transition out of
	// 'pending' (see UpdateWorkspaceNotificationStatus) -- the account that
	// acknowledged or dismissed it. Engine has no per-human-user identity
	// below account level, so this attributes to the same granularity
	// CreatedBy already uses, not a distinct "which person" answer.
	ResolvedBy *uuid.UUID `json:"resolved_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type UpsertConfigStateParams struct {
	ConfigKey        string
	ConfigType       ConfigType
	OwnerTeamID      *uuid.UUID
	SourceHash       string
	DesiredState     json.RawMessage
	ManagedResources json.RawMessage
	LatestResourceID *uuid.UUID
	UpdatedBy        uuid.UUID
}

type CreateWorkspaceNotificationParams struct {
	Type      WorkspaceNotificationType
	Severity  WorkspaceNotificationSeverity
	ServiceID *uuid.UUID
	Version   string
	ConfigKey string
	Message   string
	Metadata  json.RawMessage
	CreatedBy uuid.UUID
}

type CreateConfigPlanParams struct {
	ConfigKey           string
	ConfigType          ConfigType
	OwnerTeamID         *uuid.UUID
	SourceHash          string
	BaseGeneration      int
	Actions             json.RawMessage
	DesiredState        json.RawMessage
	ResolvedPayload     json.RawMessage
	Blockers            json.RawMessage
	Warnings            json.RawMessage
	RequiredPermissions json.RawMessage
	CreatedBy           uuid.UUID
	SupersedeExisting   bool
}

// ApplyConfigPlanParams keeps the desired-state write and plan transition
// together so a failed apply cannot leave either record claiming success on
// its own.
type ApplyConfigPlanParams struct {
	State            UpsertConfigStateParams
	PlanID           uuid.UUID
	BaseGeneration   int
	ExpectedRevision int
	ApplyLeaseID     uuid.UUID
}

type ConfigPlanApplyLease struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

// ApplyArtifactConfigPlanParams makes runtime scope creation and config-plan
// finalization one database operation. TokenHash is generated by the API so
// the raw credential never crosses into persistence or telemetry.
type ApplyArtifactConfigPlanParams struct {
	Plan                 ApplyConfigPlanParams
	Scope                ArtifactScope
	AuthorizedBucketName string
	TokenHash            string
	TokenName            string
	Activate             bool
}

type ApplyArtifactConfigPlanResult struct {
	State        *ConfigState
	ScopeCreated bool
}

// ApplyWebhookConfigPlanParams places the complete Engine-owned webhook
// reconciliation behind one PostgreSQL transaction. Service metadata and
// secret references are resolved before this call; no external work runs
// while the owner-team and config rows are locked.
type ApplyWebhookConfigPlanParams struct {
	Plan           ApplyConfigPlanParams
	Registrations  []WorkspaceWebhook
	KeepServiceIDs []uuid.UUID
}

type ApplyWebhookConfigPlanResult struct {
	State         *ConfigState
	Registrations []WorkspaceWebhook
}

type ConfigRepository interface {
	GetConfigState(ctx context.Context, configKey string) (*ConfigState, error)
	GetConfigStatesByKeys(ctx context.Context, configKeys []string) (map[string]ConfigState, error)
	ResolveArtifactOwnerTeam(ctx context.Context, configKey string) (uuid.UUID, error)
	ListConfigStates(ctx context.Context, configType ConfigType) ([]ConfigState, error)
	UpsertConfigState(ctx context.Context, params UpsertConfigStateParams) (*ConfigState, error)
	CreateConfigPlan(ctx context.Context, params CreateConfigPlanParams) (*ConfigPlan, error)
	GetConfigPlan(ctx context.Context, planID uuid.UUID) (*ConfigPlan, error)
	ReplaceConfigPlanActions(ctx context.Context, planID uuid.UUID, actions, requiredPermissions json.RawMessage, actorID uuid.UUID) (*ConfigPlan, error)
	ReserveConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int) (*ConfigPlanApplyLease, error)
	RenewConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int, leaseID uuid.UUID) (*ConfigPlanApplyLease, error)
	ReleaseConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int, leaseID uuid.UUID) error
	ApplyConfigPlan(ctx context.Context, params ApplyConfigPlanParams) (*ConfigState, error)
	ApplyArtifactConfigPlan(ctx context.Context, params ApplyArtifactConfigPlanParams) (*ApplyArtifactConfigPlanResult, error)
	ApplyWebhookConfigPlan(ctx context.Context, params ApplyWebhookConfigPlanParams) (*ApplyWebhookConfigPlanResult, error)
	CreateWorkspaceNotification(ctx context.Context, params CreateWorkspaceNotificationParams) (*WorkspaceNotification, error)
	ListWorkspaceNotifications(ctx context.Context, status WorkspaceNotificationStatus) ([]WorkspaceNotification, error)
	// ListUnresolvedWorkspaceNotificationsPage, CountUnresolvedWorkspaceNotifications,
	// and CountPendingWorkspaceNotifications back the paginated
	// workspaceNotifications GraphQL page (see plans/plan-service-changelog.md's
	// Phase 4 pagination follow-up) -- additive alongside ListWorkspaceNotifications
	// above, which every existing unbounded caller keeps using unchanged.
	ListUnresolvedWorkspaceNotificationsPage(ctx context.Context, limit, offset int, pendingOnly, readOnly bool) ([]WorkspaceNotification, error)
	CountUnresolvedWorkspaceNotifications(ctx context.Context) (int, error)
	CountPendingWorkspaceNotifications(ctx context.Context) (int, error)
	// UpdateWorkspaceNotificationStatus is Phase 4's one status-transition
	// method covering all three valid edges of the small state machine
	// (pending->acknowledged, pending->dismissed, acknowledged->dismissed) --
	// see plans/plan-service-changelog.md's "## Phase 4" for why this is one
	// generic method rather than separate Acknowledge/Dismiss methods.
	UpdateWorkspaceNotificationStatus(ctx context.Context, id uuid.UUID, status WorkspaceNotificationStatus, resolvedBy uuid.UUID) (*WorkspaceNotification, error)
}

type postgresConfigRepository struct {
	db *pgxpool.Pool
}

func NewPostgresConfigRepository(db *pgxpool.Pool) ConfigRepository {
	return &postgresConfigRepository{db: db}
}

func (r *postgresConfigRepository) GetConfigState(ctx context.Context, configKey string) (*ConfigState, error) {
	if strings.TrimSpace(configKey) == "" {
		return nil, ErrConfigKeyRequired
	}

	row := r.db.QueryRow(ctx, `
		SELECT id, config_key, config_type, owner_team_id, source_hash, generation,
		       desired_state, managed_resources, latest_resource_id, updated_by,
		       created_at, updated_at
		FROM fused_config_states
		WHERE config_key = $1
	`, strings.TrimSpace(configKey))
	return scanConfigState(row)
}

func (r *postgresConfigRepository) GetConfigStatesByKeys(ctx context.Context, configKeys []string) (map[string]ConfigState, error) {
	states := make(map[string]ConfigState, len(configKeys))
	if len(configKeys) == 0 {
		return states, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, config_key, config_type, owner_team_id, source_hash, generation,
		       desired_state, managed_resources, latest_resource_id, updated_by, created_at, updated_at
		FROM fused_config_states WHERE config_key = ANY($1)
	`, configKeys)
	if err != nil {
		return nil, fmt.Errorf("GetConfigStatesByKeys: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		state, err := scanConfigState(rows)
		if err != nil {
			return nil, err
		}
		states[state.ConfigKey] = *state
	}
	return states, rows.Err()
}

// ResolveArtifactOwnerTeam gives every artifact kind, including webhooks that
// have no runtime scope row, one SQL-owned immutable ownership lookup.
func (r *postgresConfigRepository) ResolveArtifactOwnerTeam(ctx context.Context, configKey string) (uuid.UUID, error) {
	if strings.TrimSpace(configKey) == "" {
		return uuid.Nil, ErrConfigKeyRequired
	}
	var ownerTeamID uuid.UUID
	err := r.db.QueryRow(ctx, `
		SELECT owner_team_id FROM fused_config_states
		WHERE config_key = $1 AND config_type IN ('sdk', 'mcp', 'webhook')
	`, strings.TrimSpace(configKey)).Scan(&ownerTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrConfigPlanNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve artifact owner team: %w", err)
	}
	return ownerTeamID, nil
}

func (r *postgresConfigRepository) ListConfigStates(ctx context.Context, configType ConfigType) ([]ConfigState, error) {
	if !validConfigType(configType) {
		return nil, ErrConfigTypeInvalid
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, config_key, config_type, owner_team_id, source_hash, generation,
		       desired_state, managed_resources, latest_resource_id, updated_by,
		       created_at, updated_at
		FROM fused_config_states
		WHERE config_type = $1
		ORDER BY config_key
	`, configType)
	if err != nil {
		return nil, fmt.Errorf("ListConfigStates: query: %w", err)
	}
	defer rows.Close()

	var out []ConfigState
	for rows.Next() {
		state, err := scanConfigState(rows)
		if err != nil {
			return nil, fmt.Errorf("ListConfigStates: scan: %w", err)
		}
		out = append(out, *state)
	}
	return out, rows.Err()
}

func (r *postgresConfigRepository) UpsertConfigState(ctx context.Context, params UpsertConfigStateParams) (*ConfigState, error) {
	if err := validateStateParams(&params); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_state.upsert")
	defer span.End()
	span.SetAttributes(configAttrs(params.ConfigKey, params.ConfigType)...)

	return upsertConfigState(ctx, r.db, params)
}

// ApplyConfigPlan locks the current state generation before writing. This
// closes the race between an apply-time freshness check and final persistence
// without holding a transaction open during external generation work.
func (r *postgresConfigRepository) ApplyConfigPlan(ctx context.Context, params ApplyConfigPlanParams) (*ConfigState, error) {
	if params.ExpectedRevision <= 0 {
		return nil, ErrConfigPlanRevisionMismatch
	}
	if params.State.ConfigType == ConfigTypeWorkspace && params.ApplyLeaseID == uuid.Nil {
		return nil, ErrConfigPlanApplyInProgress
	}
	if err := validateConfigIdentity(params.State.ConfigKey, params.State.ConfigType, params.State.SourceHash); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.apply")
	defer span.End()
	span.SetAttributes(configAttrs(params.State.ConfigKey, params.State.ConfigType)...)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ApplyConfigPlan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	return applyConfigPlanTx(ctx, tx, params)
}

func applyConfigPlanTx(ctx context.Context, tx pgx.Tx, params ApplyConfigPlanParams) (*ConfigState, error) {
	if err := lockConfigGeneration(ctx, tx, params); err != nil {
		return nil, err
	}
	ownerTeamID, err := loadApplyOwnerTeam(ctx, tx, params)
	if err != nil {
		return nil, err
	}
	// Apply ownership is read from the locked plan. A request cannot replace
	// the team selected and authorized during planning.
	params.State.OwnerTeamID = ownerTeamID
	if err := validateStateParams(&params.State); err != nil {
		return nil, err
	}
	state, err := upsertConfigState(ctx, tx, params.State)
	if err != nil {
		return nil, err
	}
	if err := markConfigPlanApplied(ctx, tx, params); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ApplyConfigPlan: commit: %w", err)
	}
	return state, nil
}

// ApplyWebhookConfigPlan atomically publishes webhook registrations and the
// config state that owns them. The team row is locked before config state,
// matching ArchiveTeam's serialization order and preventing an archive from
// committing between the ownership check and state publication.
func (r *postgresConfigRepository) ApplyWebhookConfigPlan(ctx context.Context, params ApplyWebhookConfigPlanParams) (*ApplyWebhookConfigPlanResult, error) {
	if params.Plan.ExpectedRevision <= 0 {
		return nil, ErrConfigPlanRevisionMismatch
	}
	if err := validateWebhookApplyParams(&params); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.webhook_apply")
	defer span.End()
	span.SetAttributes(configAttrs(params.Plan.State.ConfigKey, ConfigTypeWebhook)...)
	span.SetAttributes(attribute.Int("webhook.registrations", len(params.Registrations)))

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ApplyWebhookConfigPlan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	return applyWebhookConfigPlanTx(ctx, tx, params)
}

func applyWebhookConfigPlanTx(ctx context.Context, tx pgx.Tx, params ApplyWebhookConfigPlanParams) (*ApplyWebhookConfigPlanResult, error) {
	if err := lockActiveWebhookOwnerTeam(ctx, tx, params.Plan); err != nil {
		return nil, err
	}
	if err := lockConfigGeneration(ctx, tx, params.Plan); err != nil {
		return nil, err
	}
	ownerTeamID, err := loadApplyOwnerTeam(ctx, tx, params.Plan)
	if err != nil {
		return nil, err
	}
	params.Plan.State.OwnerTeamID = ownerTeamID

	saved, err := reconcileWebhookRegistrations(ctx, tx, params)
	if err != nil {
		return nil, err
	}
	state, err := upsertConfigState(ctx, tx, params.Plan.State)
	if err != nil {
		return nil, err
	}
	if err := markConfigPlanApplied(ctx, tx, params.Plan); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ApplyWebhookConfigPlan: commit: %w", err)
	}
	return &ApplyWebhookConfigPlanResult{State: state, Registrations: saved}, nil
}

func validateWebhookApplyParams(params *ApplyWebhookConfigPlanParams) error {
	if params.Plan.State.ConfigType != ConfigTypeWebhook {
		return ErrConfigTypeInvalid
	}
	if err := validateStateParams(&params.Plan.State); err != nil {
		return err
	}
	identities := make(map[string]struct{}, len(params.Registrations))
	for i := range params.Registrations {
		registration := &params.Registrations[i]
		if err := validateWebhookRegistration(*registration, params.Plan.State.ConfigKey); err != nil {
			return err
		}
		if registration.VerificationHeaders == nil {
			registration.VerificationHeaders = []string{}
		}
		identity := registration.ServiceID.String() + "\x00" + registration.Label
		if _, duplicate := identities[identity]; duplicate {
			return ErrWorkspaceWebhookDuplicate
		}
		identities[identity] = struct{}{}
	}
	if params.KeepServiceIDs == nil {
		params.KeepServiceIDs = []uuid.UUID{}
	}
	return nil
}

func validateWebhookRegistration(registration WorkspaceWebhook, configKey string) error {
	if registration.OwningConfigKey != configKey {
		return ErrConfigStateIdentityMismatch
	}
	if registration.ServiceID == uuid.Nil || registration.ServiceVersionID == uuid.Nil {
		return ErrWorkspaceWebhookNotFound
	}
	if strings.TrimSpace(registration.Label) == "" || strings.TrimSpace(registration.Slug) == "" {
		return ErrWorkspaceWebhookNotFound
	}
	if err := validateWebhookSecretBinding(registration); err != nil {
		return err
	}
	return nil
}

func validateWebhookSecretBinding(registration WorkspaceWebhook) error {
	// A signing reference without its immutable bucket identity (or vice
	// versa) would make runtime verification ambiguous, so malformed rows are
	// rejected before the transaction touches registrations.
	if (strings.TrimSpace(registration.SecretRef) == "") != (registration.SecretBucketID == nil) {
		return ErrWorkspaceWebhookNotFound
	}
	if registration.SecretBucketID == nil {
		return nil
	}
	ref, err := secretref.Parse(registration.SecretRef)
	if *registration.SecretBucketID == uuid.Nil || err != nil || ref.Kind != secretref.KindSecret {
		return ErrWorkspaceWebhookNotFound
	}
	return nil
}

func lockActiveWebhookOwnerTeam(ctx context.Context, tx pgx.Tx, params ApplyConfigPlanParams) error {
	var ownerTeamID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT plan.owner_team_id
		FROM fused_config_plans plan
		WHERE plan.id = $1 AND plan.config_key = $2 AND plan.config_type = 'webhook'
		  AND plan.source_hash = $3 AND plan.base_generation = $4
		  AND plan.status = 'pending' AND plan.revision = $5
	`, params.PlanID, params.State.ConfigKey, params.State.SourceHash, params.BaseGeneration, params.ExpectedRevision).Scan(&ownerTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConfigPlanNotFound
	}
	if err != nil {
		return fmt.Errorf("ApplyWebhookConfigPlan: load owner team: %w", err)
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT status = 'active' FROM fused_teams WHERE id = $1 FOR UPDATE`, ownerTeamID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConfigOwnerTeamInactive
	}
	if err != nil {
		return fmt.Errorf("ApplyWebhookConfigPlan: lock owner team: %w", err)
	}
	if !active {
		return ErrConfigOwnerTeamInactive
	}
	return nil
}

func reconcileWebhookRegistrations(ctx context.Context, tx pgx.Tx, params ApplyWebhookConfigPlanParams) ([]WorkspaceWebhook, error) {
	saved, err := upsertWorkspaceWebhooks(ctx, tx, params.Registrations)
	if err != nil {
		return nil, fmt.Errorf("ApplyWebhookConfigPlan: upsert registration: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM fused_workspace_webhooks
		WHERE owning_config_key = $1 AND NOT (service_id = ANY($2::uuid[]))
	`, params.Plan.State.ConfigKey, params.KeepServiceIDs); err != nil {
		return nil, fmt.Errorf("ApplyWebhookConfigPlan: prune registrations: %w", err)
	}
	return saved, nil
}

// ApplyArtifactConfigPlan commits the runtime and its desired-state pointer in
// one transaction. SDK callers must carry the database lease acquired before
// Registry generation, so only that reviewed plan revision can finalize.
func (r *postgresConfigRepository) ApplyArtifactConfigPlan(ctx context.Context, params ApplyArtifactConfigPlanParams) (*ApplyArtifactConfigPlanResult, error) {
	if params.Plan.ExpectedRevision <= 0 {
		return nil, ErrConfigPlanRevisionMismatch
	}
	if params.Plan.State.ConfigType == ConfigTypeSDK && params.Plan.ApplyLeaseID == uuid.Nil {
		return nil, ErrConfigPlanApplyInProgress
	}
	if err := validateArtifactApplyParams(params); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.apply_artifact")
	defer span.End()
	span.SetAttributes(configAttrs(params.Plan.State.ConfigKey, params.Plan.State.ConfigType)...)
	span.SetAttributes(attribute.String("artifact_id", params.Scope.ArtifactID.String()))

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ApplyArtifactConfigPlan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return nil, err
	}
	result, err := applyArtifactConfigPlanTx(ctx, tx, &params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ApplyArtifactConfigPlan: commit: %w", err)
	}
	return result, nil
}

func applyArtifactConfigPlanTx(ctx context.Context, tx pgx.Tx, params *ApplyArtifactConfigPlanParams) (*ApplyArtifactConfigPlanResult, error) {
	if err := prepareArtifactApplyTx(ctx, tx, params); err != nil {
		return nil, err
	}
	created, err := persistArtifactRuntimeTx(ctx, tx, *params)
	if err != nil {
		return nil, err
	}
	state, err := upsertConfigState(ctx, tx, params.Plan.State)
	if err != nil {
		return nil, err
	}
	if err := markConfigPlanApplied(ctx, tx, params.Plan); err != nil {
		return nil, err
	}
	return &ApplyArtifactConfigPlanResult{State: state, ScopeCreated: created}, nil
}

func prepareArtifactApplyTx(ctx context.Context, tx pgx.Tx, params *ApplyArtifactConfigPlanParams) error {
	if err := lockConfigGeneration(ctx, tx, params.Plan); err != nil {
		return err
	}
	ownerTeamID, err := loadApplyOwnerTeam(ctx, tx, params.Plan)
	if err != nil {
		return err
	}
	if ownerTeamID == nil || *ownerTeamID != params.Scope.OwnerTeamID {
		return ErrArtifactOwnerTeamMismatch
	}
	if err := verifyAuthorizedArtifactBucketTx(ctx, tx, params.Scope.BucketID, params.AuthorizedBucketName); err != nil {
		return err
	}
	params.Plan.State.OwnerTeamID = ownerTeamID
	return validateStateParams(&params.Plan.State)
}

func verifyAuthorizedArtifactBucketTx(ctx context.Context, tx pgx.Tx, bucketID uuid.UUID, bucketName string) error {
	var present bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM fused_buckets WHERE id = $1 AND name = $2
		)
	`, bucketID, strings.TrimSpace(bucketName)).Scan(&present)
	if err != nil {
		return fmt.Errorf("ApplyArtifactConfigPlan: verify bucket identity: %w", err)
	}
	if !present {
		return ErrSDKBucketImmutable
	}
	return nil
}

func persistArtifactRuntimeTx(ctx context.Context, tx pgx.Tx, params ApplyArtifactConfigPlanParams) (bool, error) {
	created, err := saveArtifactScopeTx(ctx, tx, params.Scope, normalizedArtifactKind(params.Scope.Kind))
	if err != nil {
		return false, err
	}
	if created {
		if err := insertArtifactTokenTx(ctx, tx, params); err != nil {
			return false, err
		}
	}
	if params.Activate {
		if _, err := tx.Exec(ctx, `UPDATE fused_artifact_scopes SET deactivated_at = NULL WHERE artifact_id = $1`, params.Scope.ArtifactID); err != nil {
			return false, fmt.Errorf("ApplyArtifactConfigPlan: activate scope: %w", err)
		}
	}
	return created, nil
}

func validateArtifactApplyParams(params ApplyArtifactConfigPlanParams) error {
	if err := validateConfigIdentity(params.Plan.State.ConfigKey, params.Plan.State.ConfigType, params.Plan.State.SourceHash); err != nil {
		return err
	}
	if params.Plan.State.ConfigType != ConfigTypeSDK && params.Plan.State.ConfigType != ConfigTypeMCP {
		return ErrConfigTypeInvalid
	}
	if err := validateArtifactApplyScopeIdentity(params.Scope, params.AuthorizedBucketName); err != nil {
		return err
	}
	if strings.TrimSpace(params.TokenHash) == "" || strings.TrimSpace(params.TokenName) == "" {
		return errors.New("artifact token identity is required")
	}
	return nil
}

func validateArtifactApplyScopeIdentity(scope ArtifactScope, bucketName string) error {
	if scope.AccountID == uuid.Nil || scope.ArtifactID == uuid.Nil || scope.OwnerTeamID == uuid.Nil {
		return errors.New("artifact scope identity is required")
	}
	if scope.BucketID == uuid.Nil || strings.TrimSpace(bucketName) == "" {
		return errors.New("authorized artifact bucket identity is required")
	}
	return nil
}

func normalizedArtifactKind(kind string) string {
	if kind == "" {
		return "sdk"
	}
	return kind
}

func insertArtifactTokenTx(ctx context.Context, tx pgx.Tx, params ApplyArtifactConfigPlanParams) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_artifact_tokens (artifact_id, token_hash, name)
		VALUES ($1, $2, $3)
	`, params.Scope.ArtifactID, params.TokenHash, params.TokenName)
	if err != nil {
		return fmt.Errorf("ApplyArtifactConfigPlan: create token: %w", err)
	}
	return nil
}

func loadApplyOwnerTeam(ctx context.Context, tx pgx.Tx, params ApplyConfigPlanParams) (*uuid.UUID, error) {
	var ownerTeamID *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT owner_team_id FROM fused_config_plans
		WHERE id = $1 AND config_key = $2 AND config_type = $3
			AND source_hash = $4 AND base_generation = $5 AND status = 'pending'
			AND revision = $6
		FOR UPDATE
	`, params.PlanID, params.State.ConfigKey, params.State.ConfigType, params.State.SourceHash, params.BaseGeneration, params.ExpectedRevision).Scan(&ownerTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConfigPlanRevisionMismatch
	}
	if err != nil {
		return nil, fmt.Errorf("ApplyConfigPlan: load owner team: %w", err)
	}
	return ownerTeamID, nil
}

type configQueryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// upsertConfigState accepts both a pool and a transaction so standalone state
// reconciliation and atomic plan apply share exactly the same write contract.
func upsertConfigState(ctx context.Context, q configQueryRower, params UpsertConfigStateParams) (*ConfigState, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO fused_config_states (
			config_key, config_type, owner_team_id, source_hash, desired_state,
			managed_resources, latest_resource_id, updated_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (config_key) DO UPDATE SET
			source_hash = EXCLUDED.source_hash,
			generation = fused_config_states.generation + 1,
			desired_state = EXCLUDED.desired_state,
			managed_resources = EXCLUDED.managed_resources,
			latest_resource_id = EXCLUDED.latest_resource_id,
			updated_by = EXCLUDED.updated_by,
			updated_at = NOW()
		WHERE fused_config_states.config_type = EXCLUDED.config_type
		  AND fused_config_states.owner_team_id IS NOT DISTINCT FROM EXCLUDED.owner_team_id
		RETURNING id, config_key, config_type, owner_team_id, source_hash, generation,
		          desired_state, managed_resources, latest_resource_id, updated_by,
		          created_at, updated_at
	`, params.ConfigKey, params.ConfigType, params.OwnerTeamID, params.SourceHash, params.DesiredState,
		params.ManagedResources, params.LatestResourceID, params.UpdatedBy)
	state, err := scanConfigState(row)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, ErrConfigStateIdentityMismatch
	}
	return state, nil
}

// lockConfigGeneration serializes applies for one config key and fails if a
// newer desired state won while external work was running.
func lockConfigGeneration(ctx context.Context, tx pgx.Tx, params ApplyConfigPlanParams) error {
	var generation int
	err := tx.QueryRow(ctx, `
		SELECT generation
		FROM fused_config_states
		WHERE config_key = $1
		FOR UPDATE
	`, params.State.ConfigKey).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		generation = 0
	} else if err != nil {
		return fmt.Errorf("ApplyConfigPlan: lock state: %w", err)
	}
	if generation != params.BaseGeneration {
		return fmt.Errorf("ApplyConfigPlan: config generation changed")
	}
	return nil
}

// markConfigPlanApplied verifies the immutable plan identity inside the same
// transaction as the state write, preventing a stale or superseded plan from
// being finalized after the earlier HTTP-layer check.
func markConfigPlanApplied(ctx context.Context, tx pgx.Tx, params ApplyConfigPlanParams) error {
	result, err := tx.Exec(ctx, `
		UPDATE fused_config_plans
		SET status = 'applied', applied_at = NOW()
		WHERE id = $1 AND config_key = $2
		  AND config_type = $3 AND source_hash = $4 AND base_generation = $5
		  AND revision = $6
		  AND (config_type NOT IN ('workspace', 'sdk') OR apply_lease_id = $7)
		  AND status = 'pending'
	`, params.PlanID, params.State.ConfigKey, params.State.ConfigType,
		params.State.SourceHash, params.BaseGeneration, params.ExpectedRevision, nullableApplyLease(params.ApplyLeaseID))
	if err != nil {
		return fmt.Errorf("ApplyConfigPlan: mark applied: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrConfigPlanRevisionMismatch
	}
	return nil
}

func (r *postgresConfigRepository) CreateConfigPlan(ctx context.Context, params CreateConfigPlanParams) (*ConfigPlan, error) {
	if err := validatePlanParams(&params); err != nil {
		return nil, err
	}
	_, requiredCount, _ := normalizeRequiredPermissions(params.RequiredPermissions)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.create")
	defer span.End()
	attrs := append(configAttrs(params.ConfigKey, params.ConfigType), attribute.Int("required_permissions_count", requiredCount))
	span.SetAttributes(attrs...)

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("CreateConfigPlan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := validatePlanOwnerConsistency(ctx, tx, params); err != nil {
		return nil, err
	}

	if params.SupersedeExisting {
		if err := supersedePendingPlans(ctx, tx, params.ConfigKey); err != nil {
			return nil, err
		}
	}
	plan, err := insertConfigPlan(ctx, tx, params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("CreateConfigPlan: commit: %w", err)
	}
	return plan, nil
}

func validatePlanOwnerConsistency(ctx context.Context, tx pgx.Tx, params CreateConfigPlanParams) error {
	var existingType ConfigType
	var existingOwnerTeamID *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT config_type, owner_team_id FROM fused_config_states WHERE config_key = $1 FOR UPDATE
	`, params.ConfigKey).Scan(&existingType, &existingOwnerTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("CreateConfigPlan: lock current ownership: %w", err)
	}
	if existingType != params.ConfigType || !equalOptionalUUID(existingOwnerTeamID, params.OwnerTeamID) {
		return ErrArtifactOwnerTeamMismatch
	}
	return nil
}

func equalOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *postgresConfigRepository) GetConfigPlan(ctx context.Context, planID uuid.UUID) (*ConfigPlan, error) {
	row := r.db.QueryRow(ctx, selectConfigPlanSQL()+` WHERE id = $1`, planID)
	return scanConfigPlan(row)
}

func (r *postgresConfigRepository) ReplaceConfigPlanActions(ctx context.Context, planID uuid.UUID, actions, requiredPermissions json.RawMessage, actorID uuid.UUID) (*ConfigPlan, error) {
	normalized, err := normalizeJSONArray(actions)
	if err != nil {
		return nil, err
	}
	requiredPermissions, requiredCount, err := normalizeRequiredPermissions(requiredPermissions)
	if err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.actions_replace")
	defer span.End()
	span.SetAttributes(
		attribute.String("plan_id", planID.String()),
		attribute.String("actor_id", actorID.String()),
		attribute.Int("required_permissions_count", requiredCount),
	)

	row := r.db.QueryRow(ctx, `
		UPDATE fused_config_plans
		SET actions = $2, required_permissions = $3, revision = revision + 1
		WHERE id = $1 AND status = 'pending'
		  AND (apply_lease_id IS NULL OR apply_lease_expires_at <= NOW())
		RETURNING id, config_key, config_type, owner_team_id, source_hash, base_generation,
		          status, actions, desired_state, resolved_payload, blockers, warnings, required_permissions, revision,
		          created_by, created_at, applied_at, superseded_at
	`, planID, normalized, requiredPermissions)
	plan, err := scanConfigPlan(row)
	if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, ErrConfigPlanNotFound) {
		return plan, err
	}
	current, getErr := r.GetConfigPlan(ctx, planID)
	if getErr != nil {
		return nil, getErr
	}
	if current.Status == ConfigPlanStatusPending {
		return nil, ErrConfigPlanApplyInProgress
	}
	return nil, ErrConfigPlanNotFound
}

const (
	configPlanApplyLeaseTTL        = 15 * time.Minute
	configPlanApplyLeaseTTLSeconds = int64(configPlanApplyLeaseTTL / time.Second)
)

// ReserveConfigPlanApply pins the exact reviewed revision before any external
// workspace side effect. Expiry makes a crashed Engine recoverable without an
// operator clearing database state.
func (r *postgresConfigRepository) ReserveConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int) (*ConfigPlanApplyLease, error) {
	if expectedRevision <= 0 {
		return nil, ErrConfigPlanRevisionMismatch
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.apply_reserve")
	defer span.End()
	span.SetAttributes(attribute.String("user_action", "config.apply.reserve"), attribute.String("plan_id", planID.String()), attribute.Int("revision", expectedRevision))
	lease := &ConfigPlanApplyLease{ID: uuid.New()}
	// PostgreSQL owns both the eligibility check and expiry calculation so an
	// Engine host with a skewed clock cannot steal or prolong an apply lease.
	err := r.db.QueryRow(ctx, `
		UPDATE fused_config_plans
		SET apply_lease_id = $3,
		    apply_lease_expires_at = NOW() + ($4 * INTERVAL '1 second')
		WHERE id = $1 AND revision = $2 AND status = 'pending'
		  AND (apply_lease_id IS NULL OR apply_lease_expires_at <= NOW())
		RETURNING apply_lease_expires_at
	`, planID, expectedRevision, lease.ID, configPlanApplyLeaseTTLSeconds).Scan(&lease.ExpiresAt)
	if err == nil {
		return lease, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("ReserveConfigPlanApply: %w", err)
	}
	plan, getErr := r.GetConfigPlan(ctx, planID)
	if getErr != nil {
		return nil, getErr
	}
	if plan.Status == ConfigPlanStatusPending && plan.Revision == expectedRevision {
		return nil, ErrConfigPlanApplyInProgress
	}
	return nil, ErrConfigPlanRevisionMismatch
}

// RenewConfigPlanApply keeps a live apply fenced while a slow Registry call is
// in flight. Once a lease has expired it cannot be revived: a replacement may
// already have acquired the right to change the reviewed revision.
func (r *postgresConfigRepository) RenewConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int, leaseID uuid.UUID) (*ConfigPlanApplyLease, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.apply_renew")
	defer span.End()
	span.SetAttributes(attribute.String("user_action", "config.apply.renew"), attribute.String("plan_id", planID.String()), attribute.Int("revision", expectedRevision))
	lease := &ConfigPlanApplyLease{ID: leaseID}
	// Keep renewal on the same database clock used by the fencing predicate.
	err := r.db.QueryRow(ctx, `
		UPDATE fused_config_plans
		SET apply_lease_expires_at = NOW() + ($4 * INTERVAL '1 second')
		WHERE id = $1 AND revision = $2 AND status = 'pending'
		  AND apply_lease_id = $3 AND apply_lease_expires_at > NOW()
		RETURNING apply_lease_expires_at
	`, planID, expectedRevision, leaseID, configPlanApplyLeaseTTLSeconds).Scan(&lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConfigPlanRevisionMismatch
	}
	if err != nil {
		return nil, fmt.Errorf("RenewConfigPlanApply: %w", err)
	}
	return lease, nil
}

// ReleaseConfigPlanApply is intentionally idempotent. Callers release after
// pre-external failures or a proven final commit; once external execution has
// begun, failed/cancelled applies retain the lease until recovery expiry.
func (r *postgresConfigRepository) ReleaseConfigPlanApply(ctx context.Context, planID uuid.UUID, expectedRevision int, leaseID uuid.UUID) error {
	if expectedRevision <= 0 || leaseID == uuid.Nil {
		return nil
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.config_plan.apply_release")
	defer span.End()
	span.SetAttributes(attribute.String("user_action", "config.apply.release"), attribute.String("plan_id", planID.String()), attribute.Int("revision", expectedRevision))
	_, err := r.db.Exec(ctx, `
		UPDATE fused_config_plans
		SET apply_lease_id = NULL, apply_lease_expires_at = NULL
		WHERE id = $1 AND revision = $2 AND apply_lease_id = $3
	`, planID, expectedRevision, leaseID)
	if err != nil {
		return fmt.Errorf("ReleaseConfigPlanApply: %w", err)
	}
	return nil
}

func nullableApplyLease(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// workspaceNotificationDedupeMatchSQL is CreateWorkspaceNotification's
// dedupe predicate, shared by both halves of its WITH/UNION ALL query so
// the two can never drift apart. It has two branches:
//
//   - If the incoming metadata (the `$7::jsonb` parameter) carries a
//     registry_changelog_id key, dedupe purely on that value -- Phase 3's
//     changelog-derived notifications (plans/plan-service-changelog.md's
//     "## Phase 3") key on the changelog row's own already-unique ID, one
//     notification per row regardless of how many configs it impacts.
//   - Otherwise, fall through to the original plan_id+action_id match --
//     every caller before Phase 3 (the workspace_service_removed/
//     workspace_version_removed apply-plan-action flow) has no
//     registry_changelog_id key at all, so this branch is exactly the
//     dedupe behavior that existed before Phase 3, byte-for-byte.
//
// The jsonb `?` operator tests key existence, not value, so an incoming
// registry_changelog_id can never accidentally match an existing row that
// lacks the key (that row's `metadata->>'registry_changelog_id'` is SQL
// NULL, and NULL never equals anything).
const workspaceNotificationDedupeMatchSQL = `(
	(($7::jsonb) ? 'registry_changelog_id' AND metadata->>'registry_changelog_id' = ($7::jsonb)->>'registry_changelog_id')
	OR
	(NOT (($7::jsonb) ? 'registry_changelog_id')
		AND metadata->>'plan_id' = ($7::jsonb)->>'plan_id'
		AND COALESCE(metadata->>'action_id', '') = COALESCE(($7::jsonb)->>'action_id', ''))
)`

func (r *postgresConfigRepository) CreateWorkspaceNotification(ctx context.Context, params CreateWorkspaceNotificationParams) (*WorkspaceNotification, error) {
	if params.Metadata == nil {
		params.Metadata = json.RawMessage("{}")
	}
	if _, err := normalizeJSONObject(params.Metadata); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace_notification.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("user_action", "workspace.notification.create"),

		attribute.String("type", string(params.Type)),
		attribute.String("severity", string(params.Severity)),
		attribute.String("config_key", params.ConfigKey),
	)

	// dedupeMatch is shared by both halves of the WITH/UNION ALL below --
	// see workspaceNotificationDedupeMatchSQL's own doc comment for what it
	// does and why the registry_changelog_id branch is additive.
	row := r.db.QueryRow(ctx, `
		WITH new_row AS (
			INSERT INTO fused_workspace_notifications (
				type, severity, status, service_id, version,
				config_key, message, metadata, created_by
			)
			SELECT $1, $2, 'pending', $3, $4, $5, $6, $7, $8
			WHERE NOT EXISTS (
				SELECT 1 FROM fused_workspace_notifications
				WHERE type = $1
					  AND (service_id = $3 OR (service_id IS NULL AND $3 IS NULL))
					  AND COALESCE(version, '') = COALESCE($4, '')
					  AND status = 'pending'
					  AND `+workspaceNotificationDedupeMatchSQL+`
				)
				RETURNING id, type, severity, status, service_id,
			              version, config_key, message, metadata, created_by, resolved_by, created_at, updated_at
		)
		SELECT * FROM new_row
		UNION ALL
		SELECT id, type, severity, status, service_id,
		       version, config_key, message, metadata, created_by, resolved_by, created_at, updated_at
		FROM fused_workspace_notifications
		WHERE type = $1
			  AND (service_id = $3 OR (service_id IS NULL AND $3 IS NULL))
			  AND COALESCE(version, '') = COALESCE($4, '')
			  AND status = 'pending'
			  AND `+workspaceNotificationDedupeMatchSQL+`
			LIMIT 1
	`, params.Type, params.Severity, params.ServiceID, params.Version,
		params.ConfigKey, params.Message, params.Metadata, params.CreatedBy)
	return scanWorkspaceNotification(row)
}

func (r *postgresConfigRepository) ListWorkspaceNotifications(ctx context.Context, status WorkspaceNotificationStatus) ([]WorkspaceNotification, error) {
	var args []any
	query := `
		SELECT id, type, severity, status, service_id,
		       version, config_key, message, metadata, created_by, resolved_by, created_at, updated_at
		FROM fused_workspace_notifications`
	if status != "" {
		args = append(args, status)
		query += " WHERE status = $1"
	}
	// Pending rows sort before acknowledged ones (within the same created_at
	// ordering) so unread notifications float to the top wherever this
	// unbounded list is rendered without a dedicated read/unread toggle --
	// notably the bell panel/banners (see workspace_config_handlers.go's
	// workspaceNotificationInbox). Callers that pass an explicit status
	// (CLI plan/apply always passes "pending") are unaffected since every
	// row already shares one status.
	query += " ORDER BY (status = 'pending') DESC, created_at DESC"

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListWorkspaceNotifications: query: %w", err)
	}
	defer rows.Close()

	var out []WorkspaceNotification
	for rows.Next() {
		note, err := scanWorkspaceNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("ListWorkspaceNotifications: scan: %w", err)
		}
		out = append(out, *note)
	}
	return out, rows.Err()
}

// ListUnresolvedWorkspaceNotificationsPage is ListWorkspaceNotifications'
// paginated sibling, purpose-built for the UI's full notifications page (see
// plans/plan-service-changelog.md's Phase 4 pagination follow-up) -- a new,
// additive method rather than changing ListWorkspaceNotifications' own
// signature, so CLI plan/apply's unbounded pending-only fetch and the
// contextual banners'/bell panel's unbounded pending+acknowledged fetch are
// both untouched. "Unresolved" matches unresolvedWorkspaceNotifications'
// own definition (internal/engine/api/workspace_config_handlers.go):
// pending or acknowledged, never dismissed -- done here as `status <>
// 'dismissed'` directly in SQL rather than fetching everything and
// filtering in Go, since a paginated query needs the exclusion applied
// before LIMIT/OFFSET, not after.
//
// pendingOnly narrows the same query to status = 'pending' -- backs the
// notifications page's default "unread only" view (a checkbox opts into
// seeing acknowledged rows too, which passes pendingOnly=false). Within
// either mode, pending rows still sort before acknowledged ones so unread
// items float to the top when both are shown, matching
// ListWorkspaceNotifications' own ordering above.
func (r *postgresConfigRepository) ListUnresolvedWorkspaceNotificationsPage(ctx context.Context, limit, offset int, pendingOnly, readOnly bool) ([]WorkspaceNotification, error) {
	where := "status <> 'dismissed'"
	if pendingOnly {
		where = "status = 'pending'"
	} else if readOnly {
		where = "status = 'acknowledged'"
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, type, severity, status, service_id,
		       version, config_key, message, metadata, created_by, resolved_by, created_at, updated_at
		FROM fused_workspace_notifications
		WHERE `+where+`
		ORDER BY (status = 'pending') DESC, created_at DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("ListUnresolvedWorkspaceNotificationsPage: query: %w", err)
	}
	defer rows.Close()

	var out []WorkspaceNotification
	for rows.Next() {
		note, err := scanWorkspaceNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("ListUnresolvedWorkspaceNotificationsPage: scan: %w", err)
		}
		out = append(out, *note)
	}
	return out, rows.Err()
}

// CountUnresolvedWorkspaceNotifications and CountPendingWorkspaceNotifications
// back the paginated page's "total_count"/"pending_count" fields -- the
// client can no longer derive these by counting a fully-loaded array once
// the underlying fetch is windowed.
func (r *postgresConfigRepository) CountUnresolvedWorkspaceNotifications(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_notifications WHERE status <> 'dismissed'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountUnresolvedWorkspaceNotifications: %w", err)
	}
	return count, nil
}

func (r *postgresConfigRepository) CountPendingWorkspaceNotifications(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_notifications WHERE status = 'pending'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountPendingWorkspaceNotifications: %w", err)
	}
	return count, nil
}

// UpdateWorkspaceNotificationStatus is the write path Phase 4 adds on top of
// Phase 3's read-only notifications (plans/plan-service-changelog.md's
// "## Phase 4"). The WHERE clause enforces the one state-machine invariant
// that matters -- 'dismissed' is terminal, no "undismiss" -- directly in the
// UPDATE itself rather than in a separate check-then-write (avoiding a
// read-then-write race between two callers acting on the same notification
// at once). A zero-row result is ambiguous between "no such id" and
// "already dismissed", so workspaceNotificationUpdateFailureReason does one
// follow-up existence check purely to return the more useful of the two
// errors -- this only runs on the already-uncommon failure path, never on
// a successful update.
func (r *postgresConfigRepository) UpdateWorkspaceNotificationStatus(ctx context.Context, id uuid.UUID, status WorkspaceNotificationStatus, resolvedBy uuid.UUID) (*WorkspaceNotification, error) {
	if status != WorkspaceNotificationStatusAcknowledged && status != WorkspaceNotificationStatusDismissed {
		return nil, ErrWorkspaceNotificationStatusInvalid
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace_notification.update_status")
	defer span.End()
	span.SetAttributes(
		attribute.String("user_action", "workspace.notification.update_status"),
		attribute.String("notification_id", id.String()),
		attribute.String("status", string(status)),
	)

	row := r.db.QueryRow(ctx, `
		UPDATE fused_workspace_notifications
		SET status = $2, resolved_by = $3, updated_at = NOW()
		WHERE id = $1 AND status <> 'dismissed'
		RETURNING id, type, severity, status, service_id,
		          version, config_key, message, metadata, created_by, resolved_by, created_at, updated_at
	`, id, status, resolvedBy)
	note, err := scanWorkspaceNotification(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, r.workspaceNotificationUpdateFailureReason(ctx, id)
		}
		return nil, fmt.Errorf("UpdateWorkspaceNotificationStatus: %w", err)
	}
	return note, nil
}

func (r *postgresConfigRepository) workspaceNotificationUpdateFailureReason(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM fused_workspace_notifications WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("UpdateWorkspaceNotificationStatus: check existence: %w", err)
	}
	if !exists {
		return ErrWorkspaceNotificationNotFound
	}
	return ErrWorkspaceNotificationImmutable
}

func validateStateParams(params *UpsertConfigStateParams) error {
	if err := validateConfigIdentity(params.ConfigKey, params.ConfigType, params.SourceHash); err != nil {
		return err
	}
	params.ConfigKey = strings.TrimSpace(params.ConfigKey)
	params.SourceHash = strings.TrimSpace(params.SourceHash)
	var err error
	if params.DesiredState, err = normalizeJSONObject(params.DesiredState); err != nil {
		return err
	}
	params.ManagedResources, err = normalizeJSONObject(params.ManagedResources)
	if err != nil {
		return err
	}
	return validateConfigOwnerTeam(params.ConfigType, params.OwnerTeamID)
}

func validatePlanParams(params *CreateConfigPlanParams) error {
	if err := validateConfigIdentity(params.ConfigKey, params.ConfigType, params.SourceHash); err != nil {
		return err
	}
	params.ConfigKey = strings.TrimSpace(params.ConfigKey)
	params.SourceHash = strings.TrimSpace(params.SourceHash)
	var err error
	params.Actions, err = normalizeJSONArray(params.Actions)
	if err != nil {
		return err
	}
	if params.DesiredState, err = normalizeJSONObject(params.DesiredState); err != nil {
		return err
	}
	if params.ResolvedPayload, err = normalizeJSONObject(params.ResolvedPayload); err != nil {
		return err
	}
	if params.Blockers, err = normalizeJSONArray(params.Blockers); err != nil {
		return err
	}
	params.Warnings, err = normalizeJSONArray(params.Warnings)
	if err != nil {
		return err
	}
	if err := validateConfigOwnerTeam(params.ConfigType, params.OwnerTeamID); err != nil {
		return err
	}
	params.RequiredPermissions, _, err = normalizeRequiredPermissions(params.RequiredPermissions)
	return err
}

func validateConfigOwnerTeam(configType ConfigType, ownerTeamID *uuid.UUID) error {
	if configType == ConfigTypeWorkspace {
		if ownerTeamID != nil {
			return ErrOwnerTeamUnexpected
		}
		return nil
	}
	if ownerTeamID == nil || *ownerTeamID == uuid.Nil {
		return ErrOwnerTeamRequired
	}
	return nil
}

func normalizeRequiredPermissions(raw json.RawMessage) (json.RawMessage, int, error) {
	canonical, requirements, err := accesscontrol.NormalizeRequiredPermissions(raw)
	if err != nil || len(requirements) == 0 {
		return nil, 0, ErrRequiredPermissions
	}
	return canonical, len(requirements), nil
}

func validateConfigIdentity(configKey string, configType ConfigType, sourceHash string) error {
	if strings.TrimSpace(configKey) == "" {
		return ErrConfigKeyRequired
	}
	if strings.TrimSpace(sourceHash) == "" {
		return ErrConfigHashRequired
	}
	if !validConfigType(configType) {
		return ErrConfigTypeInvalid
	}
	return nil
}

// validConfigType centralizes the persisted enum so reads and writes cannot
// accidentally accept different config kinds as the product grows.
func validConfigType(configType ConfigType) bool {
	return configType == ConfigTypeWorkspace || configType == ConfigTypeSDK || configType == ConfigTypeMCP || configType == ConfigTypeWebhook
}

func normalizeJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(raw) {
		return nil, ErrConfigJSONInvalid
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '{' {
		return nil, ErrConfigJSONObjectRequired
	}
	return raw, nil
}

func normalizeJSONArray(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage("[]"), nil
	}
	if !json.Valid(raw) {
		return nil, ErrConfigJSONInvalid
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '[' {
		return nil, ErrConfigJSONArrayRequired
	}
	return raw, nil
}

func supersedePendingPlans(ctx context.Context, tx pgx.Tx, configKey string) error {
	var planID uuid.UUID
	var activelyLeased bool
	err := tx.QueryRow(ctx, `
		SELECT id, apply_lease_id IS NOT NULL AND apply_lease_expires_at > NOW()
		FROM fused_config_plans
		WHERE config_key = $1 AND status = 'pending'
		FOR UPDATE
	`, configKey).Scan(&planID, &activelyLeased)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("CreateConfigPlan: lock pending plan: %w", err)
	}
	if activelyLeased {
		return ErrConfigPlanApplyInProgress
	}
	_, err = tx.Exec(ctx, `
		UPDATE fused_config_plans
		SET status = 'superseded', superseded_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, planID)
	if err != nil {
		return fmt.Errorf("CreateConfigPlan: supersede pending plans: %w", err)
	}
	return nil
}

func insertConfigPlan(ctx context.Context, tx pgx.Tx, params CreateConfigPlanParams) (*ConfigPlan, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO fused_config_plans (
			config_key, config_type, owner_team_id, source_hash, base_generation,
			actions, desired_state, resolved_payload, blockers, warnings, required_permissions, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, config_key, config_type, owner_team_id, source_hash, base_generation,
		          status, actions, desired_state, resolved_payload, blockers, warnings, required_permissions, revision,
		          created_by, created_at, applied_at, superseded_at
	`, params.ConfigKey, params.ConfigType, params.OwnerTeamID, params.SourceHash, params.BaseGeneration,
		params.Actions, params.DesiredState, params.ResolvedPayload, params.Blockers, params.Warnings, params.RequiredPermissions, params.CreatedBy)
	plan, err := scanConfigPlan(row)
	if err != nil {
		return nil, fmt.Errorf("CreateConfigPlan: insert: %w", err)
	}
	return plan, nil
}

func scanConfigState(row pgx.Row) (*ConfigState, error) {
	var state ConfigState
	var latestResourceID *uuid.UUID
	if err := row.Scan(
		&state.ID, &state.ConfigKey, &state.ConfigType, &state.OwnerTeamID, &state.SourceHash,
		&state.Generation, &state.DesiredState, &state.ManagedResources, &latestResourceID,
		&state.UpdatedBy, &state.CreatedAt, &state.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan config state: %w", err)
	}
	state.LatestResourceID = latestResourceID
	return &state, nil
}

func scanConfigPlan(row pgx.Row) (*ConfigPlan, error) {
	var plan ConfigPlan
	var appliedAt, supersededAt sql.NullTime
	if err := row.Scan(
		&plan.ID, &plan.ConfigKey, &plan.ConfigType, &plan.OwnerTeamID, &plan.SourceHash,
		&plan.BaseGeneration, &plan.Status, &plan.Actions, &plan.DesiredState, &plan.ResolvedPayload,
		&plan.Blockers, &plan.Warnings, &plan.RequiredPermissions, &plan.Revision, &plan.CreatedBy, &plan.CreatedAt,
		&appliedAt, &supersededAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConfigPlanNotFound
		}
		return nil, fmt.Errorf("scan config plan: %w", err)
	}
	plan.AppliedAt = nullableTime(appliedAt)
	plan.SupersededAt = nullableTime(supersededAt)
	return &plan, nil
}

func scanWorkspaceNotification(row pgx.Row) (*WorkspaceNotification, error) {
	var note WorkspaceNotification
	if err := row.Scan(
		&note.ID, &note.Type, &note.Severity, &note.Status,
		&note.ServiceID, &note.Version, &note.ConfigKey, &note.Message, &note.Metadata,
		&note.CreatedBy, &note.ResolvedBy, &note.CreatedAt, &note.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan workspace notification: %w", err)
	}
	return &note, nil
}

func nullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func selectConfigPlanSQL() string {
	return `
		SELECT id, config_key, config_type, owner_team_id, source_hash, base_generation,
		       status, actions, desired_state, resolved_payload, blockers, warnings, required_permissions, revision,
		       created_by, created_at, applied_at, superseded_at
		FROM fused_config_plans`
}

func configAttrs(configKey string, configType ConfigType) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("user_action", "config."+string(configType)),

		attribute.String("config_key", configKey),
		attribute.String("config_type", string(configType)),
	}
}
