package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/mtlsauth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookid"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/secretref"
)

const defaultWorkspaceConfigKey = "workspace"

type ConfigPlanRequest struct {
	ConfigKey  string          `json:"config_key"`
	SourceHash string          `json:"source_hash"`
	Config     json.RawMessage `json:"config"`
}

type ConfigApplyRequest struct {
	PlanID           string                              `json:"plan_id"`
	SourceHash       string                              `json:"source_hash"`
	AuthMaterials    map[string]workspaceAuthMaterial    `json:"auth_materials,omitempty"`
	ProfileMaterials map[string]workspaceConnectMaterial `json:"profile_materials,omitempty"`
	// BucketSecretMaterials carries the resolved values for
	// buckets.<name>.secrets.<key> $ENV refs, keyed by
	// workspaceBucketSecretMaterialKey(bucketName, key) -- same out-of-band
	// pattern as AuthMaterials: the plan/config only ever stores the $ENV
	// reference, never the resolved value, so this travels separately at
	// apply time, resolved locally by the CLI from its own environment.
	BucketSecretMaterials map[string]string `json:"bucket_secret_materials,omitempty"`
}

type workspaceAuthMaterial struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
	APIKey   string `json:"api_key"`
	Cert     string `json:"cert"`
	Key      string `json:"key"`
}

type workspaceConnectMaterial struct {
	ClientID      string            `json:"client_id"`
	ClientSecret  string            `json:"client_secret"`
	BindingValues map[string]string `json:"binding_values,omitempty"`
}

type workspaceConfigDocument struct {
	Kind         string                            `json:"kind"`
	Version      int                               `json:"version"`
	Services     map[string]workspaceConfigService `json:"services"`
	Buckets      map[string]workspaceConfigBucket  `json:"buckets,omitempty"`
	Deprecations []workspaceConfigDeprecation      `json:"deprecations,omitempty"`
}

type workspaceConfigBucket struct {
	ServiceConfig map[string]workspaceConfigBucketService `json:"service_config,omitempty"`
	// Secrets are generic, bucket-scoped named secrets ($ENV refs only, same
	// discipline as ServiceConfig.Auth/.Connect) -- not tied to any one
	// service, resolved via an explicit bucket.<name>.secret.<key> reference
	// rather than ambient SDK/artifact context (see
	// plans/plan-service-config-restructure.md item 4).
	Secrets map[string]string `json:"secrets,omitempty"`
}

type workspaceConfigBucketService struct {
	Auth *WorkspaceAuthConfig `json:"auth,omitempty"`
}

type workspaceConfigService struct {
	ServiceID   string `json:"service_id"`
	ServiceName string `json:"service_name,omitempty"`
	Public      *bool  `json:"public,omitempty"`
	// Versions is one entry per enabled version: its identity (Version, plus
	// the Engine-resolved ServiceVersionID once known), any per-version
	// override of Public/ExecutionPolicy, and the connection profiles scoped
	// to it. This replaces the old sibling resolved_versions/version_policies/
	// connection_profiles lists, each of which carried its own repeated
	// `version` field.
	Versions []workspaceConfigServiceVersion `json:"versions,omitempty"`
	// runtime_config.webhooks (RuntimeConfig) was removed with no backward
	// compatibility once kind: webhook shipped -- see
	// plans/plan-webhook-kind.md.
	// ExecutionPolicy carries rate-limit/retry settings and an optional public
	// flag. When public=true and the workspace owns the service, the Engine
	// publishes the settings to the Registry during apply so all SDK consumers
	// inherit these provider-declared limits. It is the default applied to
	// every version in Versions unless that version sets its own override.
	ExecutionPolicy *workspaceExecutionPolicy `json:"execution_policy,omitempty"`
}

// workspaceConfigServiceVersion mirrors cli/internal/configfile.WorkspaceServiceVersion
// in the Engine, the same way workspaceExecutionPolicy mirrors ExecutionPolicy.
type workspaceConfigServiceVersion struct {
	Version string `json:"version"`
	// ServiceVersionID is the Engine-resolved immutable ID for Version. At
	// plan time this may arrive empty (or absent) and get filled in by
	// resolveWorkspaceServiceVersions; the CLI then persists and replays the
	// resolved value at apply time so apply never needs a fresh registry
	// lookup that could drift under a reused version label.
	ServiceVersionID string `json:"service_version_id,omitempty"`
	// Public controls Registry-level visibility for just this version via
	// UpdateServiceVersionPublicStatus (owner only). Distinct from
	// ExecutionPolicy.Public, which controls whether this version's
	// rate_limit/retry are published, not whether the version itself is
	// visible.
	Public          *bool                     `json:"public,omitempty"`
	ExecutionPolicy *workspaceExecutionPolicy `json:"execution_policy,omitempty"`
	// ConnectionProfiles declares this version's routing-recipe intent,
	// independent of RuntimeConfig.Connect. Keeping profile intent and bucket
	// material as separate lists is what lets "Attach that baseline when its
	// service version is activated, independently of whether any bucket has
	// configured Connect credentials yet" and "Adding service-keyed material to
	// a bucket does not activate ... that service" both hold: neither list can
	// gate the other's resolution or reconciliation.
	ConnectionProfiles []workspaceConfigConnectionProfileIntent `json:"connection_profiles,omitempty"`
}

// workspaceExecutionPolicy mirrors cli/internal/configfile.ExecutionPolicy in
// the Engine. Keeping a separate type (instead of importing the CLI package)
// avoids a dependency inversion and lets the Engine evolve independently.
type workspaceExecutionPolicy struct {
	// Public, when true, publishes RateLimit, Retry, Pagination, and
	// WebhookConfig through the Registry publish API so downstream consumers
	// inherit these limits. Only valid for owned services; validated before
	// planning.
	Public      *bool            `json:"public,omitempty"`
	RateLimit   *rateLimitConfig `json:"rate_limit,omitempty"`
	Retry       *retryConfig     `json:"retry,omitempty"`
	RetryConfig *retryConfig     `json:"retry_config,omitempty"`
	// Pagination moved under execution_policy from the now-deleted
	// runtime_config.pagination (see plans/plan-service-config-restructure.md
	// item 1) -- it shares this same Public flag rather than having its own,
	// and travels the same service/version publish path as RateLimit/Retry.
	Pagination *paginationConfig `json:"pagination,omitempty"`
	// BaseURL declares an owner override for a wrong or missing spec-derived
	// base_url (plans/plan-service-config-restructure.md's base_url work
	// item). Like every other field here, it always takes local effect in
	// this workspace on apply regardless of Public (see
	// LocalObjectCache.applyExecutionPolicyOverride); Public additionally
	// publishes it into the provider contract, where it becomes every other
	// consumer's effective "base_url" too.
	BaseURL *string `json:"base_url,omitempty"`
	// EventExtractionPath and IncomingWebhookConfig are the provider's own
	// outbound webhook verification recipe
	// (plans/plan-service-config-restructure.md item 3) -- distinct from a
	// workspace's own webhook *registrations* (RuntimeConfig.Webhooks,
	// workspace-private, never published). The json tag matches the
	// existing incoming_webhook_config wire name exactly (not a new
	// "webhook_config" alias) since this same struct value is both decoded
	// from the CLI's config JSON and sent to the Registry publish API -- one
	// wire name has to satisfy both hops. IncomingWebhookConfig never carries
	// a secret -- only the verification mechanism -- which is what makes it
	// safe to publish under this same Public flag.
	EventExtractionPath   *string              `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *webhookVerifyConfig `json:"incoming_webhook_config,omitempty"`
	Reset                 bool                 `json:"reset,omitempty"`
}

type rateLimitConfig struct {
	Strategy          string `json:"strategy"`
	RequestsPerSecond int    `json:"requests_per_second"`
	RequestsPerMinute int    `json:"requests_per_minute"`
}

type retryConfig struct {
	Strategy   string `json:"strategy"`
	MaxRetries int    `json:"max_retries"`
	BackoffMs  int    `json:"backoff_ms"`
}

type paginationConfig struct {
	Type         string `json:"type"`
	RequestParam string `json:"request_param"`
	ResponsePath string `json:"response_path"`
}

// webhookVerifyConfig mirrors cli/internal/configfile.WebhookVerify --
// auth mechanism + where to find the signature, deliberately no secret field.
type webhookVerifyConfig struct {
	AuthType            string   `json:"auth_type,omitempty"`
	AuthLocation        string   `json:"auth_location,omitempty"`
	AuthKeyName         string   `json:"auth_key_name,omitempty"`
	SignatureHeader     string   `json:"signature_header,omitempty"`
	VerificationHeaders []string `json:"verification_headers,omitempty"`
}

// workspaceConfigConnectionProfileIntent is one auth_type routing decision,
// scoped to whichever workspaceConfigServiceVersion it's nested under --
// service_version identity comes from that parent, not from this struct.
// Resolved and Ambiguous are Engine-populated during plan resolution and
// rejected if a caller supplies them -- see rejectConfiguredResolvedProfiles.
type workspaceConfigConnectionProfileIntent struct {
	AuthType  string                     `json:"auth_type"`
	ProfileID string                     `json:"profile_id,omitempty"`
	Profile   *connectionprofile.Profile `json:"profile,omitempty"`
	Reset     bool                       `json:"reset,omitempty"`
	// Public, when true, publishes this connection profile to the Registry so
	// all consumers of the service see it as a baseline. Only valid for owned
	// services; validated before planning.
	Public   *bool                               `json:"public,omitempty"`
	Resolved *workspaceResolvedConnectionProfile `json:"resolved,omitempty"`
	// Ambiguous marks a tuple where several eligible public baselines exist and
	// no explicit profile_id was given. It is surfaced as a plan warning rather
	// than an error so one ambiguous tuple cannot block unrelated services or
	// versions from being planned/activated in the same request.
	Ambiguous bool `json:"ambiguous,omitempty"`
	// Reason is a safe, non-secret explanation surfaced in plan warnings when
	// Ambiguous is true (e.g. "multiple public connection profiles match").
	Reason string `json:"reason,omitempty"`
}

type WorkspaceAuthConfig struct {
	Bucket   string `json:"bucket,omitempty"`
	AuthType string `json:"auth_type"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Cert     string `json:"cert,omitempty"`
	Key      string `json:"key,omitempty"`
}

type InjectionConfig struct {
	Value    string `json:"value"`
	Location string `json:"location"`
	Name     string `json:"name"`
	Mode     string `json:"mode,omitempty"`
}

type workspaceResolvedConnectionProfile struct {
	ProfileID   string                    `json:"profile_id"`
	Revision    int                       `json:"revision"`
	ProfileHash string                    `json:"profile_hash"`
	Provenance  string                    `json:"provenance"`
	Config      connectionprofile.Profile `json:"config"`
}

type workspaceConfigDeprecation struct {
	ServiceID   string `json:"service_id"`
	Version     string `json:"version,omitempty"`
	EffectiveAt string `json:"effective_at"`
	Reason      string `json:"reason,omitempty"`
}

type workspaceDesiredState struct {
	Services             map[uuid.UUID]workspaceDesiredService
	BucketServiceConfigs []workspaceDesiredBucketServiceConfig
	// BucketSecrets is the normalized form of workspaceConfigBucket.Secrets --
	// generic, bucket-scoped (not service-scoped) named secret intents. Kept
	// as its own list rather than folded into BucketServiceConfigs because it
	// has no service dimension at all (see workspaceDesiredBucketSecret).
	BucketSecrets []workspaceDesiredBucketSecret
	Deprecations  map[uuid.UUID][]workspaceDeprecation
}

// workspaceDesiredBucketSecret is one buckets.<name>.secrets.<key> intent,
// normalized from workspaceConfigBucket.Secrets. EnvRef is validated but not
// resolved here -- resolution happens at apply time via
// ConfigApplyRequest.BucketSecretMaterials, out-of-band from the plan itself
// (same pattern as workspaceAuthMaterial).
type workspaceDesiredBucketSecret struct {
	BucketName string
	Key        string
	EnvRef     string
}

type workspaceDesiredService struct {
	Key             string
	ServiceID       uuid.UUID
	ServiceName     string
	Public          *bool
	Versions        []string
	VersionIDs      map[string]uuid.UUID
	ExecutionPolicy *workspaceExecutionPolicy
	// VersionPolicies is normalized from each workspaceConfigServiceVersion
	// entry's own Public/ExecutionPolicy override (present only when that
	// version set either one), with VersionID resolved the same way
	// ConnectionProfiles' Version is resolved to VersionID below.
	VersionPolicies []workspaceDesiredVersionPolicy
	// ConnectionProfiles is flattened from every workspaceConfigServiceVersion
	// entry's own nested ConnectionProfiles list. It is resolved and
	// reconciled by a code path that never consults RuntimeConfig.Connect,
	// keeping the two plans independent.
	ConnectionProfiles []workspaceDesiredConnectionProfile
}

// workspaceDesiredVersionPolicy is the normalized, version-pinned form of a
// workspaceConfigServiceVersion entry's own Public/ExecutionPolicy override.
type workspaceDesiredVersionPolicy struct {
	Version         string
	VersionID       uuid.UUID
	Public          *bool
	ExecutionPolicy *workspaceExecutionPolicy
}

// workspaceDesiredConnectionProfile is the normalized, version-pinned form of
// workspaceConfigConnectionProfileIntent.
type workspaceDesiredConnectionProfile struct {
	Version   string
	VersionID uuid.UUID
	AuthType  string
	ProfileID string
	Profile   *connectionprofile.Profile
	Reset     bool
	// Public, when true, publishes this profile to the Registry during apply.
	// Only set for owned services; non-owners are blocked during validation.
	Public    *bool
	Resolved  *workspaceResolvedConnectionProfile
	Ambiguous bool
	// Reason is a safe, non-secret explanation surfaced in plan warnings when
	// Ambiguous is true (e.g. "multiple public connection profiles match").
	Reason string
}

type workspaceDesiredBucketServiceConfig struct {
	BucketName string
	ServiceKey string
	ServiceID  uuid.UUID
	Auth       *WorkspaceAuthConfig
}

// explicit reports whether the workspace author named a specific profile
// (inline body or profile_id), as opposed to relying on automatic Registry
// selection. Only an explicit choice may replace an existing profile that
// omission would otherwise preserve.
func (p workspaceDesiredConnectionProfile) explicit() bool {
	return p.Profile != nil || strings.TrimSpace(p.ProfileID) != ""
}

// hasSource reports whether this intent has resolved to a profile body that
// can actually be written -- either an inline workspace body or an
// automatically/explicitly selected Registry snapshot.
func (p workspaceDesiredConnectionProfile) hasSource() bool {
	return p.Profile != nil || p.Resolved != nil
}

type workspaceDeprecation struct {
	ServiceID   uuid.UUID
	Version     string
	EffectiveAt string
	Reason      string
}

type workspaceManagedResources struct {
	Services []workspaceManagedService `json:"services"`
}

type workspaceManagedService struct {
	ServiceID string   `json:"service_id"`
	Versions  []string `json:"versions"`
}

type workspacePlanAction struct {
	ID                 string   `json:"id"`
	Type               string   `json:"type"`
	ServiceID          string   `json:"service_id"`
	ServiceName        string   `json:"service_name,omitempty"`
	Version            string   `json:"version,omitempty"`
	ServiceVersionID   string   `json:"service_version_id,omitempty"`
	FromVersion        string   `json:"from_version,omitempty"`
	ToVersion          string   `json:"to_version,omitempty"`
	Decision           string   `json:"decision,omitempty"`
	RequiresDecision   bool     `json:"requires_decision,omitempty"`
	ImpactedSDKConfigs []string `json:"impacted_sdk_configs,omitempty"`
	Recommendation     string   `json:"recommendation,omitempty"`
	SuggestedCommand   string   `json:"suggested_command,omitempty"`
	EffectiveAt        string   `json:"effective_at,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	Public             *bool    `json:"public,omitempty"`
	AuthType           string   `json:"auth_type,omitempty"`
	ProfileRevision    int      `json:"profile_revision,omitempty"`
	ProfileProvenance  string   `json:"profile_provenance,omitempty"`
	TargetLocation     string   `json:"target_location,omitempty"`
	TargetName         string   `json:"target_name,omitempty"`
	BindingSource      string   `json:"binding_source,omitempty"`
	// WillArchive is true when the service is owned by this workspace, meaning
	// removal will also soft-delete it from the Registry (not just deactivate
	// it locally). Surfaces this distinction in plan output so the user can see
	// "[archive from registry]" vs "[remove from workspace]" before confirming.
	WillArchive bool `json:"will_archive,omitempty"`
}

type workspacePlanSummary struct {
	Actions           []workspacePlanAction  `json:"actions"`
	UnmanagedServices []string               `json:"unmanaged_services"`
	Warnings          []workspacePlanWarning `json:"warnings,omitempty"`
	Blockers          []workspacePlanBlocker `json:"blockers,omitempty"`
}

type workspacePlanWarning struct {
	Code             string   `json:"code"`
	ServiceID        string   `json:"service_id"`
	Message          string   `json:"message"`
	SDKs             []string `json:"sdk_configs,omitempty"`
	SuggestedCommand string   `json:"suggested_command,omitempty"`
}

type workspacePlanBlocker struct {
	Code      string `json:"code"`
	ServiceID string `json:"service_id"`
	ActionID  string `json:"action_id"`
	Message   string `json:"message"`
}

type workspaceConfigHTTPError struct {
	status  int
	message string
}

func (e workspaceConfigHTTPError) Error() string { return e.message }

type ServiceSlugResolver interface {
	ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error)
}

type ServiceVisibilityResolver interface {
	FetchServiceVisibility(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) (map[uuid.UUID]sandbox.ServiceVisibility, error)
}

type ServiceVisibilityUpdater interface {
	UpdateServicePublic(ctx context.Context, serviceID uuid.UUID, isPublic bool, apiKey string) error
	// PublishServiceExecutionPolicy publishes the workspace execution policy
	// through the Registry API so downstream consumers inherit these
	// provider-declared limits. Only called for owned services. The policy
	// argument is marshalled to JSON as-is; callers pass
	// *workspaceExecutionPolicy but the interface accepts any to avoid a
	// cross-package unexported type dependency.
	PublishServiceExecutionPolicy(ctx context.Context, serviceID uuid.UUID, policy any, apiKey string) error
	// UpdateServiceVersionPublic is UpdateServicePublic's per-version sibling:
	// it sets is_public on just one version, independent of the service's own
	// visibility. Only called for owned services.
	UpdateServiceVersionPublic(ctx context.Context, serviceID uuid.UUID, version string, isPublic bool, apiKey string) error
	// PublishServiceVersionExecutionPolicy publishes execution policy scoped to
	// one provider version. Only called for owned services.
	PublishServiceVersionExecutionPolicy(ctx context.Context, serviceID uuid.UUID, version string, policy any, apiKey string) error
}

type ConnectionProfileResolver interface {
	FetchEligibleConnectionProfiles(context.Context, []sandbox.ConnectionProfileRef, string) ([]sandbox.ConnectionProfileRevision, error)
}

// ConnectionProfilePublisher publishes a workspace-declared connection profile
// (connection_profiles[*].public: true) to the Registry via setConnectionProfile
// so every consumer of the service can see it as a baseline. Only called for
// owned services (see validateWorkspacePublicIntent).
type ConnectionProfilePublisher interface {
	PublishConnectionProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, name string, profile connectionprofile.Profile, apiKey string) (*sandbox.ConnectionProfileRevision, error)
}

type ConnectionProfileContractResolver interface {
	FetchConnectionProfileContracts(context.Context, []uuid.UUID, string) ([]sandbox.ConnectionProfileContract, error)
}

// ServiceArchiver is the capability required to soft-delete a service from the
// Registry on behalf of its owner. Only the owner's Engine can issue the
// delete; other Engines that merely have the service activated can only remove
// it from their workspace (local deactivation).
type ServiceArchiver interface {
	ArchiveService(ctx context.Context, serviceID uuid.UUID, apiKey string) error
}

// ServiceVersionDeprecator marks a named version of a service as deprecated in
// the Registry. Only the owner can deprecate; non-owners cannot alter lifecycle
// status of versions they consume.
type ServiceVersionDeprecator interface {
	DeprecateServiceVersion(ctx context.Context, serviceID uuid.UUID, version, apiKey string) error
}

// WorkspaceConfigPlanHandler handles POST /workspace/config/plan.
func WorkspaceConfigPlanHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.workspace_config.plan")
		defer span.End()

		accountID, err := resolveWorkspaceActor(ctx)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.String("account_id", accountID.String()))

		req, err := decodeWorkspacePlanRequest(r)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		result, err := createWorkspaceConfigPlan(ctx, configStore, s, verifier, r.Header.Get("X-API-Key"), accountID, req)
		if err != nil {
			writeWorkspaceConfigError(w, err)
			return
		}

		span.SetAttributes(
			attribute.String("plan_id", result.plan.ID.String()),
			attribute.Int("required_permissions_count", result.requiredCount),
			attribute.String("outcome", "success"),
		)
		writeJSON(w, map[string]any{
			"plan_id":              result.plan.ID.String(),
			"config_key":           result.plan.ConfigKey,
			"source_hash":          result.plan.SourceHash,
			"base_generation":      result.plan.BaseGeneration,
			"required_permissions": result.plan.RequiredPermissions,
			"summary":              result.summary,
			"notifications":        result.notifications,
		})
	}
}

type workspaceConfigPlanResult struct {
	plan          *store.ConfigPlan
	summary       workspacePlanSummary
	notifications notificationInbox
	requiredCount int
}

func createWorkspaceConfigPlan(ctx context.Context, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, apiKey string, accountID uuid.UUID, req ConfigPlanRequest) (workspaceConfigPlanResult, error) {
	resolvedConfig, desired, err := resolveWorkspacePlanConfig(ctx, verifier, apiKey, req.Config)
	if err != nil {
		return workspaceConfigPlanResult{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	configKey := workspaceConfigKey(req.ConfigKey)
	currentState, err := configStore.GetConfigState(ctx, configKey)
	if err != nil {
		return workspaceConfigPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	currentWorkspace, err := loadCurrentWorkspaceState(ctx, s)
	if err != nil {
		return workspaceConfigPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load workspace state"}
	}
	previousManaged, err := parseManagedWorkspaceResources(currentState)
	if err != nil {
		return workspaceConfigPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "invalid managed workspace state"}
	}
	summary, err := buildWorkspacePlanSummary(ctx, configStore, s, verifier, apiKey, desired, currentWorkspace, previousManaged)
	if err != nil {
		return workspaceConfigPlanResult{}, err
	}
	return persistWorkspaceConfigPlan(ctx, configStore, accountID, req, configKey, resolvedConfig, desired, currentState, summary)
}

func persistWorkspaceConfigPlan(ctx context.Context, configStore store.ConfigRepository, accountID uuid.UUID, req ConfigPlanRequest, configKey string, resolvedConfig []byte, desired workspaceDesiredState, currentState *store.ConfigState, summary workspacePlanSummary) (workspaceConfigPlanResult, error) {
	actionsJSON, _ := json.Marshal(summary.Actions)
	blockersJSON, _ := json.Marshal(summary.Blockers)
	warningsJSON, _ := json.Marshal(summary.Warnings)
	requiredPermissions, requiredCount, err := workspacePlanRequiredPermissions(ctx, actionsJSON)
	if err != nil {
		return workspaceConfigPlanResult{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to compute required permissions"}
	}
	plan, err := configStore.CreateConfigPlan(ctx, store.CreateConfigPlanParams{
		ConfigKey: configKey, ConfigType: store.ConfigTypeWorkspace, SourceHash: req.SourceHash,
		BaseGeneration: currentGeneration(currentState), Actions: actionsJSON, ResolvedPayload: resolvedConfig,
		Blockers: blockersJSON, Warnings: warningsJSON, RequiredPermissions: requiredPermissions,
		CreatedBy: accountID, SupersedeExisting: true,
	})
	if err != nil {
		return workspaceConfigPlanResult{}, configPlanSaveHTTPError(err)
	}
	return workspaceConfigPlanResult{
		plan: plan, summary: summary, requiredCount: requiredCount,
		notifications: collectWorkspacePlanNotifications(ctx, configStore, workspaceServiceVersionsMap(desired)),
	}, nil
}

func configPlanSaveHTTPError(err error) workspaceConfigHTTPError {
	if errors.Is(err, store.ErrConfigPlanApplyInProgress) {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "config apply is in progress"}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to save plan"}
}

// collectWorkspacePlanNotifications gathers Engine-local notifications
// relevant to this workspace config's own service/version declarations --
// see plans/plan-service-changelog.md's "## Phase 4" for why kind: workspace
// was previously the one plan response with no notifications key at all,
// unlike SDK/MCP's collectSDKPlanNotifications. Deliberately Engine-local
// only (fused_workspace_notifications), not live Registry drift snapshots --
// WorkspaceConfigPlanHandler has no sandbox.RegistryClient dependency today
// (only ServiceVerifier), and threading one in to also fetch drift is a
// larger, separate change; flagged in the plan doc, not solved here.
func collectWorkspacePlanNotifications(ctx context.Context, configStore store.ConfigRepository, serviceVersions map[uuid.UUID][]string) notificationInbox {
	inbox := notificationInbox{}
	notifications, err := configStore.ListWorkspaceNotifications(ctx, store.WorkspaceNotificationStatusPending)
	if err != nil {
		inbox.Warnings = append(inbox.Warnings, "engine_notifications_unavailable")
		return inbox
	}
	inbox.Items = append(inbox.Items, filterWorkspaceEngineNotifications(notifications, serviceVersions)...)
	return inbox
}

func filterWorkspaceEngineNotifications(notifications []store.WorkspaceNotification, serviceVersions map[uuid.UUID][]string) []workspaceNotificationInboxItem {
	var items []workspaceNotificationInboxItem
	for _, notification := range notifications {
		if workspaceNotificationMatches(notification, serviceVersions) {
			items = append(items, workspaceNotificationInboxItems([]store.WorkspaceNotification{notification})...)
		}
	}
	return items
}

// workspaceNotificationMatches mirrors sdkNotificationMatches' service+version
// check (sdk_config_handlers.go) -- a workspace config has no single
// config_key of its own to fast-path against the way an SDK/MCP config does
// (ConfigKey on a workspace_service_removed/registry_* row lists the
// *impacted SDK configs*, never "workspace" itself), so this is purely the
// service+version match: any notification scoped to a service this
// workspace config declares, at either a version this config declares or
// the service-wide tier (notification.Version == "").
func workspaceNotificationMatches(notification store.WorkspaceNotification, serviceVersions map[uuid.UUID][]string) bool {
	if notification.ServiceID == nil {
		return false
	}
	versions, ok := serviceVersions[*notification.ServiceID]
	if !ok {
		return false
	}
	if notification.Version == "" {
		return true
	}
	for _, v := range versions {
		if v == notification.Version {
			return true
		}
	}
	return false
}

// workspaceServiceVersionsMap projects the resolved desired config down to
// just what workspaceNotificationMatches needs -- every service this
// workspace config declares, mapped to every version it declares for that
// service (a workspace service block can list several, unlike an SDK
// config's single pinned version).
func workspaceServiceVersionsMap(desired workspaceDesiredState) map[uuid.UUID][]string {
	out := make(map[uuid.UUID][]string, len(desired.Services))
	for _, svc := range desired.Services {
		out[svc.ServiceID] = svc.Versions
	}
	return out
}

// WorkspaceConfigApplyHandler handles POST /workspace/config/apply.
func WorkspaceConfigApplyHandler(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		thread := observability.ThreadFromContext(ctx)
		step := thread.Step("engine.workspace_config.apply")

		accountID, err := resolveWorkspaceActor(ctx)
		if err != nil {
			step.AddContext(map[string]any{"outcome": "unauthorized"}).Error(ctx, err)
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		step.AddContext(map[string]any{"account_id": accountID.String()})

		req, planID, err := decodeWorkspaceApplyRequest(r)
		if err != nil {
			step.AddContext(map[string]any{"outcome": "bad_request"}).Error(ctx, err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		planRevision, ok := AuthorizedPlanRevisionFromContext(ctx)
		if !ok {
			step.AddContext(map[string]any{"outcome": "authorization_snapshot_missing"}).Error(ctx, errors.New("authorized plan revision unavailable"))
			http.Error(w, `{"error":"authorized plan revision unavailable"}`, http.StatusForbidden)
			return
		}
		appliedWebhooks, err := executeWorkspaceConfigApply(ctx, configStore, s, verifier, workspaceApplyCall{
			apiKey:           r.Header.Get("X-API-Key"),
			accountID:        accountID,
			planID:           planID,
			planRevision:     planRevision,
			sourceHash:       req.SourceHash,
			masterKey:        masterKey,
			authMats:         req.AuthMaterials,
			profileMats:      req.ProfileMaterials,
			bucketSecretMats: req.BucketSecretMaterials,
		})
		if err != nil {
			step.AddContext(map[string]any{"outcome": "apply_failed"}).Error(ctx, err)
			writeWorkspaceConfigError(w, err)
			return
		}

		step.AddContext(map[string]any{"outcome": "success"}).Success(ctx)
		writeJSON(w, workspaceApplyResponse{
			Status:   "applied",
			PlanID:   planID.String(),
			Webhooks: appliedWebhookResponses(appliedWebhooks),
		})
	}
}

// workspaceApplyResponse is the apply endpoint's JSON body. Webhooks carries
// only the path segment (slug + service key), not a full URL -- the CLI
// already knows which Engine host it just called, so it builds the display
// URL itself rather than the Engine hardcoding a public base URL here.
type workspaceApplyResponse struct {
	Status   string                   `json:"status"`
	PlanID   string                   `json:"plan_id"`
	Webhooks []appliedWebhookResponse `json:"webhooks,omitempty"`
}

type appliedWebhookResponse struct {
	ServiceKey string `json:"service_key"`
	Label      string `json:"label"`
	Slug       string `json:"slug"`
}

func appliedWebhookResponses(applied []appliedWorkspaceWebhook) []appliedWebhookResponse {
	if len(applied) == 0 {
		return nil
	}
	out := make([]appliedWebhookResponse, len(applied))
	for i, w := range applied {
		out[i] = appliedWebhookResponse{ServiceKey: w.ServiceKey, Label: w.Label, Slug: w.Slug}
	}
	return out
}

type workspaceApplyCall struct {
	apiKey           string
	accountID        uuid.UUID
	planID           uuid.UUID
	planRevision     int
	sourceHash       string
	masterKey        []byte
	authMats         map[string]workspaceAuthMaterial
	profileMats      map[string]workspaceConnectMaterial
	bucketSecretMats map[string]string
}

func executeWorkspaceConfigApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	verifier ServiceVerifier,
	call workspaceApplyCall,
) ([]appliedWorkspaceWebhook, error) {
	plan, currentState, err := loadWorkspacePlanForApply(ctx, configStore, call)
	if err != nil {
		return nil, err
	}
	lease, err := configStore.ReserveConfigPlanApply(ctx, plan.ID, call.planRevision)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_apply_in_progress_or_revision_changed"}
	}
	leaseGuard := workspaceApplyLeaseGuard{configStore: configStore, planID: plan.ID, revision: call.planRevision, leaseID: lease.ID, releasable: true}
	defer leaseGuard.release()
	desired, previousManaged, err := workspaceApplyInputs(plan, currentState)
	if err != nil {
		return nil, err
	}
	if err := validateWorkspaceRemovalDecisions(plan, desired, previousManaged); err != nil {
		return nil, err
	}
	// Once external execution begins we finish independently of client
	// cancellation. Otherwise a dropped connection after an accepted Registry
	// mutation would release the lease and make a duplicate retry look safe.
	applyCtx, stopLease := workspaceApplyLeaseContext(ctx, configStore, plan.ID, call.planRevision, lease.ID)
	defer stopLease()
	// From the first Registry/local mutation onward, failures can represent a
	// partial or unknown outcome. Keep the reservation until crash-recovery
	// expiry unless the final database commit proves the apply completed.
	leaseGuard.releasable = false
	appliedWebhooks, err := applyWorkspaceConfig(applyCtx, s, verifier, call.apiKey, call.accountID, desired, previousManaged, call.authMats, call.profileMats, call.bucketSecretMats, call.masterKey)
	if err != nil {
		return nil, workspaceApplyError(ctx, err)
	}
	if err := applyWorkspaceRegistryActions(applyCtx, verifier, call, plan, currentState); err != nil {
		return nil, err
	}
	if err := applyWorkspacePolicyActions(applyCtx, s, verifier, call, plan, desired); err != nil {
		return nil, err
	}
	if err := createWorkspaceRemovalNotifications(applyCtx, configStore, call, plan); err != nil {
		return nil, err
	}
	if err := persistWorkspaceConfigApply(applyCtx, configStore, call, plan, desired, previousManaged, lease.ID); err != nil {
		return nil, err
	}
	leaseGuard.releasable = true
	return appliedWebhooks, nil
}

type workspaceApplyLeaseGuard struct {
	configStore store.ConfigRepository
	planID      uuid.UUID
	revision    int
	leaseID     uuid.UUID
	releasable  bool
}

func (guard *workspaceApplyLeaseGuard) release() {
	if guard.releasable {
		releaseWorkspaceApplyLease(guard.configStore, guard.planID, guard.revision, guard.leaseID)
	}
}

func applyWorkspaceRegistryActions(ctx context.Context, verifier ServiceVerifier, call workspaceApplyCall, plan *store.ConfigPlan, currentState *store.ConfigState) error {
	if err := archiveRemovedOwnedServices(ctx, verifier, call, plan); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: registry archive failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to archive owned services in registry"}
	}
	if err := applyDeprecationActions(ctx, verifier, call, plan, currentState); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: deprecation apply failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply deprecation actions"}
	}
	if err := applyWorkspaceVisibilityActions(ctx, verifier, call, plan); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: visibility apply failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply workspace service visibility"}
	}
	if err := applyWorkspaceVersionVisibilityActions(ctx, verifier, call, plan); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: version visibility apply failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply workspace service version visibility"}
	}
	return nil
}

func applyWorkspacePolicyActions(ctx context.Context, s store.Store, verifier ServiceVerifier, call workspaceApplyCall, plan *store.ConfigPlan, desired workspaceDesiredState) error {
	if err := applyWorkspaceExecutionPolicyPublishActions(ctx, verifier, call, plan, desired); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: execution policy publish failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to publish workspace execution policy"}
	}
	if err := applyWorkspaceVersionExecutionPolicyPublishActions(ctx, verifier, call, plan, desired); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: version execution policy publish failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to publish workspace version execution policy"}
	}
	if err := applyWorkspaceExecutionPolicyLocalActions(ctx, s, plan, desired); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: local execution policy apply failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply local workspace execution policy"}
	}
	if err := applyWorkspaceVersionExecutionPolicyLocalActions(ctx, s, plan, desired); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: local version execution policy apply failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply local workspace version execution policy"}
	}
	if err := applyWorkspaceConnectionProfilePublishActions(ctx, verifier, s, call, plan, desired); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: connection profile publish failed", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to publish workspace connection profile"}
	}
	return nil
}

const workspaceApplyExecutionTimeout = 10 * time.Minute

func workspaceApplyLeaseContext(parent context.Context, configStore store.ConfigRepository, planID uuid.UUID, revision int, leaseID uuid.UUID) (context.Context, context.CancelFunc) {
	return workspaceApplyLeaseContextWithTimeout(parent, configStore, planID, revision, leaseID, workspaceApplyExecutionTimeout)
}

func workspaceApplyLeaseContextWithTimeout(parent context.Context, configStore store.ConfigRepository, planID uuid.UUID, revision int, leaseID uuid.UUID, timeout time.Duration) (context.Context, context.CancelFunc) {
	return workspaceApplyLeaseContextWithTiming(parent, configStore, planID, revision, leaseID, timeout, 5*time.Minute, 5*time.Second)
}

func workspaceApplyLeaseContextWithTiming(parent context.Context, configStore store.ConfigRepository, planID uuid.UUID, revision int, leaseID uuid.UUID, timeout, renewInterval, renewTimeout time.Duration) (context.Context, context.CancelFunc) {
	// Preserve tracing/request values but not client cancellation once Registry
	// execution starts. A hard upper bound prevents abandoned downstream calls
	// from keeping an Engine goroutine and lease alive indefinitely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	go func() {
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(context.Background(), renewTimeout)
				_, err := configStore.RenewConfigPlanApply(renewCtx, planID, revision, leaseID)
				renewCancel()
				if err != nil {
					slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: apply lease lost", slog.Any("error", err))
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func releaseWorkspaceApplyLease(configStore store.ConfigRepository, planID uuid.UUID, revision int, leaseID uuid.UUID) {
	// Cleanup must survive request cancellation so a failed client connection
	// does not block a safe retry until the crash-recovery expiry.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := configStore.ReleaseConfigPlanApply(ctx, planID, revision, leaseID); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: release apply lease failed", slog.Any("error", err))
	}
}

func workspaceApplyError(ctx context.Context, err error) error {
	var httpErr workspaceConfigHTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: apply failed", slog.Any("error", err))
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to apply workspace config"}
}

func loadWorkspacePlanForApply(ctx context.Context, configStore store.ConfigRepository, call workspaceApplyCall) (*store.ConfigPlan, *store.ConfigState, error) {
	plan, err := configStore.GetConfigPlan(ctx, call.planID)
	if err != nil {
		return nil, nil, planFetchHTTPError(err)
	}
	if err := validateWorkspacePlanForApply(plan, call.sourceHash); err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
	}
	if call.planRevision <= 0 || plan.Revision != call.planRevision {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_revision_changed"}
	}
	currentState, err := configStore.GetConfigState(ctx, plan.ConfigKey)
	if err != nil {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch config state"}
	}
	if currentGeneration(currentState) != plan.BaseGeneration {
		return nil, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "plan_stale"}
	}
	return plan, currentState, nil
}

func workspaceApplyInputs(plan *store.ConfigPlan, currentState *store.ConfigState) (workspaceDesiredState, map[uuid.UUID]workspaceManagedService, error) {
	desired, err := parseWorkspaceConfig(plan.ResolvedPayload)
	if err != nil {
		return workspaceDesiredState{}, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid workspace config"}
	}
	if err := validateWorkspaceProfileResetApproved(desired, plan.Actions); err != nil {
		return workspaceDesiredState{}, nil, workspaceConfigHTTPError{status: http.StatusConflict, message: err.Error()}
	}
	previousManaged, err := parseManagedWorkspaceResources(currentState)
	if err != nil {
		return workspaceDesiredState{}, nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "invalid managed workspace state"}
	}
	return desired, previousManaged, nil
}

// validateWorkspaceProfileResetApproved requires destructive reset intent to
// appear in the approved action list before apply proceeds. Deleting a
// workspace override changes dispatch for every artifact that selects the
// tuple, so this must be an explicit, reviewed action -- see the plan's
// "Produce an explicit reset action before deleting an override." Unlike the
// old bucket-owned model, there is no bucket identity to pin here: a
// workspace profile has no bucket dimension at all.
func validateWorkspaceProfileResetApproved(desired workspaceDesiredState, rawActions json.RawMessage) error {
	expected := desiredProfileDetachActions(desired)
	if len(expected) == 0 {
		return nil
	}
	approved, err := approvedProfileDetachActions(rawActions)
	if err != nil {
		return errors.New("connection profile reset is missing from the approved plan")
	}
	for actionID, expectedAction := range expected {
		approvedAction, ok := approved[actionID]
		if !ok || !profileDetachActionMatches(approvedAction, expectedAction) {
			return errors.New("connection profile reset is missing from the approved plan")
		}
	}
	return nil
}

// approvedProfileDetachActions indexes only destructive actions after parsing
// the immutable plan payload; unrelated plan work cannot authorize deletion.
func approvedProfileDetachActions(rawActions json.RawMessage) (map[string]workspacePlanAction, error) {
	var actions []workspacePlanAction
	if err := json.Unmarshal(rawActions, &actions); err != nil {
		return nil, err
	}
	approved := make(map[string]workspacePlanAction, len(actions))
	for _, action := range actions {
		if action.Type == "detach_connection_profile" {
			approved[action.ID] = action
		}
	}
	return approved, nil
}

// desiredProfileDetachActions reuses plan rendering so apply validates the
// same non-secret routing tuple the user reviewed rather than a parallel
// shape. Detach is now expressed per-intent (workspaceDesiredConnectionProfile
// .Reset), so this filters the full per-service action list down to only the
// destructive ones instead of gating whole-service inclusion.
func desiredProfileDetachActions(desired workspaceDesiredState) map[string]workspacePlanAction {
	actions := make(map[string]workspacePlanAction)
	for _, service := range sortedDesiredServices(desired) {
		for _, action := range desiredConnectionProfileActions(service) {
			if action.Type == "detach_connection_profile" {
				actions[action.ID] = action
			}
		}
	}
	return actions
}

// profileDetachActionMatches prevents a saved action for another service,
// auth family, or resolved version from authorizing the requested deletion.
func profileDetachActionMatches(got, want workspacePlanAction) bool {
	return got.ID == want.ID &&
		got.Type == want.Type &&
		got.ServiceID == want.ServiceID &&
		got.Version == want.Version &&
		got.ServiceVersionID == want.ServiceVersionID &&
		got.AuthType == want.AuthType
}

func persistWorkspaceConfigApply(
	ctx context.Context,
	configStore store.ConfigRepository,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
	applyLeaseID uuid.UUID,
) error {
	managedJSON, _ := json.Marshal(managedResourcesAfterApply(desired, previousManaged))
	if _, err := configStore.ApplyConfigPlan(ctx, store.ApplyConfigPlanParams{
		PlanID:           call.planID,
		BaseGeneration:   plan.BaseGeneration,
		ExpectedRevision: call.planRevision,
		ApplyLeaseID:     applyLeaseID,
		State: store.UpsertConfigStateParams{
			ConfigKey:        plan.ConfigKey,
			ConfigType:       store.ConfigTypeWorkspace,
			SourceHash:       plan.SourceHash,
			DesiredState:     plan.ResolvedPayload,
			ManagedResources: managedJSON,
			UpdatedBy:        call.accountID,
		},
	}); err != nil {
		slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: ApplyConfigPlan error", slog.Any("error", err))
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "failed to atomically apply config plan"}
	}
	return nil
}

func decodeWorkspacePlanRequest(r *http.Request) (ConfigPlanRequest, error) {
	var req ConfigPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, errors.New("invalid request body")
	}
	if strings.TrimSpace(req.SourceHash) == "" {
		return req, errors.New("source_hash is required")
	}
	return req, nil
}

func resolveWorkspacePlanConfig(
	ctx context.Context,
	resolver any,
	apiKey string,
	raw json.RawMessage,
) (json.RawMessage, workspaceDesiredState, error) {
	doc, err := decodeWorkspaceConfigDocument(raw)
	if err != nil {
		return nil, workspaceDesiredState{}, err
	}
	if err := resolveWorkspaceServiceSlugs(ctx, resolver, apiKey, &doc); err != nil {
		return nil, workspaceDesiredState{}, err
	}
	if err := resolveWorkspaceServiceVersions(ctx, resolver, apiKey, &doc); err != nil {
		return nil, workspaceDesiredState{}, err
	}
	if err := resolveWorkspaceConnectionProfiles(ctx, resolver, apiKey, &doc); err != nil {
		return nil, workspaceDesiredState{}, err
	}
	parsedRaw, err := json.Marshal(doc)
	if err != nil {
		return nil, workspaceDesiredState{}, err
	}
	desired, err := parseWorkspaceConfig(parsedRaw)
	if err != nil {
		return nil, workspaceDesiredState{}, err
	}
	if _, err := workspaceProfileContracts(ctx, resolver, apiKey, desired); err != nil {
		return nil, workspaceDesiredState{}, err
	}
	resolvedRaw, err := json.Marshal(doc)
	return resolvedRaw, desired, err
}

// resolveWorkspaceConnectionProfiles performs one Registry batch before plan
// serialization so every automatic choice is visible and reproducible. It
// never returns an error for an ambiguous tuple: per "Leave profile state
// unresolved when several eligible baselines exist ... this must not block
// unrelated endpoints from being activated," ambiguity is recorded on the
// individual intent (Ambiguous/Reason) rather than aborting the whole batch,
// so unrelated services and versions in the same request still plan normally.
func resolveWorkspaceConnectionProfiles(ctx context.Context, resolver any, apiKey string, doc *workspaceConfigDocument) error {
	if err := rejectConfiguredResolvedProfiles(doc.Services); err != nil {
		return err
	}
	requests, targets, err := workspaceConnectionProfileRequests(doc.Services)
	if err != nil || len(requests) == 0 {
		// Either building the batch failed outright, or no intent in this
		// document actually needs Registry resolution (all inline profiles
		// or resets) -- either way there's no batch call to make.
		return err
	}
	profileResolver, ok := resolver.(ConnectionProfileResolver)
	if !ok {
		return errors.New("connection profile resolution is unavailable")
	}
	profiles, err := profileResolver.FetchEligibleConnectionProfiles(ctx, requests, apiKey)
	if err != nil {
		return err
	}
	return attachResolvedWorkspaceProfiles(doc, requests, targets, groupEligibleProfiles(profiles))
}

// rejectConfiguredResolvedProfiles reserves resolved snapshots for Engine;
// accepting them from source config would bypass Registry eligibility checks.
func rejectConfiguredResolvedProfiles(services map[string]workspaceConfigService) error {
	for key, service := range services {
		for _, version := range service.Versions {
			for _, intent := range version.ConnectionProfiles {
				// These two fields are only ever written by
				// attachResolvedWorkspaceProfiles during plan resolution --
				// an operator submitting either directly would be forging a
				// resolution outcome Registry never actually granted.
				if intent.Resolved != nil || intent.Ambiguous {
					return fmt.Errorf("service %q connection profile resolved/ambiguous fields are read-only", key)
				}
			}
		}
	}
	return nil
}

// workspaceProfileRequestTarget positionally links one Registry batch entry
// back to the exact intent slot it should populate -- two levels deep now
// that connection profiles are nested under their owning version entry.
type workspaceProfileRequestTarget struct {
	ServiceKey   string
	VersionIndex int
	ProfileIndex int
	Version      string
}

// attachResolvedWorkspaceProfiles pins the selected immutable revision beside
// each intent so apply can verify plan-time behavior has not drifted. Ambiguity
// is non-fatal, but an explicit ineligible profile_id is rejected because the
// operator made a concrete selection that Registry could not authorize.
func attachResolvedWorkspaceProfiles(doc *workspaceConfigDocument, requests []sandbox.ConnectionProfileRef, targets []workspaceProfileRequestTarget, grouped map[string][]sandbox.ConnectionProfileRevision) error {
	for index, target := range targets {
		service := doc.Services[target.ServiceKey]
		version := service.Versions[target.VersionIndex]
		intent := version.ConnectionProfiles[target.ProfileIndex]
		selection, err := selectWorkspaceConnectionProfile(intent.ProfileID, grouped[connectionProfileRefKey(requests[index])])
		if err != nil {
			return fmt.Errorf("service %q connection profile for version %s auth_type %s: %w", target.ServiceKey, target.Version, intent.AuthType, err)
		}
		if selection.Profile != nil {
			// A single unambiguous (or explicitly requested) profile was
			// found -- pin its immutable revision so apply can verify
			// nothing drifted between plan and apply.
			resolved := workspaceResolvedConnectionProfile{
				ProfileID: selection.Profile.ProfileID.String(), Revision: selection.Profile.Revision,
				ProfileHash: selection.Profile.ProfileHash, Provenance: selection.Profile.Provenance, Config: selection.Profile.Config,
			}
			intent.Resolved = &resolved
		} else if selection.Unresolved {
			// Multiple safe candidates matched with no explicit profile_id
			// to disambiguate -- record this in the plan as data for the
			// operator to resolve, not a hard failure.
			intent.Ambiguous = true
			intent.Reason = selection.Reason
		}
		// Neither branch: zero candidates matched, so the intent is left
		// untouched (no Resolved, not Ambiguous) -- apply surfaces that
		// absence on its own if the version actually needs a profile.
		version.ConnectionProfiles[target.ProfileIndex] = intent
		service.Versions[target.VersionIndex] = version
		doc.Services[target.ServiceKey] = service
	}
	return nil
}

// workspaceConnectionProfileRequests builds one deterministic batch across
// every intent needing Registry resolution (i.e. neither an inline profile
// body nor a reset) and keeps positional targets for attaching results. This
// is entirely independent of RuntimeConfig.Connect -- a service can request
// profile resolution with no bucket material declared at all. The owning
// version's ServiceVersionID is used directly (resolveWorkspaceServiceVersions
// already ran) instead of a separate resolved_versions lookup.
func workspaceConnectionProfileRequests(services map[string]workspaceConfigService) ([]sandbox.ConnectionProfileRef, []workspaceProfileRequestTarget, error) {
	keys := make([]string, 0, len(services))
	for key := range services {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	refs := make([]sandbox.ConnectionProfileRef, 0)
	targets := make([]workspaceProfileRequestTarget, 0)
	for _, key := range keys {
		service := services[key]
		for versionIndex, version := range service.Versions {
			for profileIndex, intent := range version.ConnectionProfiles {
				// An inline profile body needs no Registry lookup at all, and
				// a reset intent is clearing an override rather than
				// selecting one -- neither belongs in the resolution batch.
				if intent.Profile != nil || intent.Reset {
					continue
				}
				versionID, err := uuid.Parse(strings.TrimSpace(version.ServiceVersionID))
				if err != nil {
					// resolveWorkspaceServiceVersions must have already run
					// and written a real ID here -- an unparsable one means
					// this version's identity was never actually resolved.
					return nil, nil, fmt.Errorf("service %q connection profile: version %s has no resolved service_version_id", key, version.Version)
				}
				refs = append(refs, sandbox.ConnectionProfileRef{ServiceVersionID: versionID, AuthType: canonicalConnectAuthType(intent.AuthType)})
				targets = append(targets, workspaceProfileRequestTarget{ServiceKey: key, VersionIndex: versionIndex, ProfileIndex: profileIndex, Version: version.Version})
			}
		}
	}
	return refs, targets, nil
}

// groupEligibleProfiles indexes the batched response by exact version/auth
// identity so selection never scans or mixes another pinned contract.
func groupEligibleProfiles(profiles []sandbox.ConnectionProfileRevision) map[string][]sandbox.ConnectionProfileRevision {
	grouped := map[string][]sandbox.ConnectionProfileRevision{}
	for _, profile := range profiles {
		key := connectionProfileRefKey(sandbox.ConnectionProfileRef{ServiceVersionID: profile.ServiceVersionID, AuthType: profile.AuthType})
		grouped[key] = append(grouped[key], profile)
	}
	return grouped
}

// connectionProfileRefKey uses a separator that cannot occur in UUID or auth
// values, preventing ambiguous composite keys in plan-time indexes.
func connectionProfileRefKey(ref sandbox.ConnectionProfileRef) string {
	return ref.ServiceVersionID.String() + "\x00" + connectionprofile.CanonicalAuthType(ref.AuthType)
}

// workspaceProfileSelection is selectWorkspaceConnectionProfile's total
// result: exactly one of Profile or Unresolved describes the outcome, and
// neither is an error -- ambiguity is data, not a failure, so the caller can
// keep resolving every other tuple in the same batch.
type workspaceProfileSelection struct {
	Profile    *sandbox.ConnectionProfileRevision
	Unresolved bool
	Reason     string
}

// selectWorkspaceConnectionProfile honors explicit eligible identity and
// auto-selects only when exactly one safe public candidate exists. Ambiguous
// automatic selection is data for the plan; an ineligible explicit ID is an
// error because silently falling back would apply a different profile.
func selectWorkspaceConnectionProfile(requestedID string, profiles []sandbox.ConnectionProfileRevision) (workspaceProfileSelection, error) {
	requestedID = strings.TrimSpace(requestedID)
	if requestedID != "" {
		for index := range profiles {
			if profiles[index].ProfileID.String() == requestedID {
				return workspaceProfileSelection{Profile: &profiles[index]}, nil
			}
		}
		return workspaceProfileSelection{}, errors.New("selected public connection profile is not eligible")
	}
	// Exactly one candidate is the only unambiguous automatic selection.
	if len(profiles) == 1 {
		return workspaceProfileSelection{Profile: &profiles[0]}, nil
	}
	// Multiple safe candidates still require a user-visible plan choice.
	if len(profiles) > 1 {
		return workspaceProfileSelection{Unresolved: true, Reason: "multiple public connection profiles match; set profile_id explicitly"}, nil
	}
	return workspaceProfileSelection{}, nil
}

// decodeWorkspaceConfigDocument rejects absent or malformed source documents
// before any resolver can enrich them with server-controlled profile state.
func decodeWorkspaceConfigDocument(raw json.RawMessage) (workspaceConfigDocument, error) {
	var doc workspaceConfigDocument
	if len(raw) == 0 {
		return doc, errors.New("config is required")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, errors.New("config must be an object")
	}
	if doc.Services == nil {
		return doc, errors.New("config.services is required")
	}
	return doc, nil
}

func resolveWorkspaceServiceSlugs(ctx context.Context, resolver any, apiKey string, doc *workspaceConfigDocument) error {
	slugs := unresolvedWorkspaceServiceSlugs(doc.Services)
	if len(slugs) == 0 {
		return nil
	}
	slugResolver, ok := resolver.(ServiceSlugResolver)
	if !ok || slugResolver == nil {
		return errors.New("workspace service slug resolution is unavailable")
	}
	resolved, err := slugResolver.ResolveServiceIDsBySlugs(ctx, slugs, apiKey)
	if err != nil {
		return fmt.Errorf("resolve workspace service slugs: %w", err)
	}
	for _, slug := range slugs {
		id, ok := resolved[slug]
		if !ok || id == uuid.Nil {
			return fmt.Errorf("service slug %q was not found", slug)
		}
		service := doc.Services[slug]
		service.ServiceID = id.String()
		doc.Services[slug] = service
	}
	return nil
}

func resolveWorkspaceServiceVersions(ctx context.Context, resolver any, apiKey string, doc *workspaceConfigDocument) error {
	if len(doc.Services) == 0 {
		// Nothing to resolve -- an empty services map is a valid (if
		// pointless) plan, not an error.
		return nil
	}
	verifier, ok := resolver.(ServiceVerifier)
	if !ok || verifier == nil {
		// The concrete resolver passed in doesn't implement version
		// verification (e.g. a test double built for a narrower interface) --
		// fail clearly rather than silently skipping resolution.
		return errors.New("workspace service version resolution is unavailable")
	}
	refs := explicitWorkspaceVersionRefs(doc.Services)
	revisions, err := verifier.FetchServiceVersionRevisions(ctx, refs, apiKey)
	if err != nil {
		return fmt.Errorf("resolve workspace service versions: %w", err)
	}
	versionIDs := workspaceVersionIDMap(revisions)
	latestVersions, err := resolveLatestWorkspaceServiceVersions(ctx, verifier, apiKey, doc.Services)
	if err != nil {
		return err
	}
	for key, svc := range doc.Services {
		// No versions declared at all means "give me whatever Registry's
		// latest public version is right now" -- resolve and attach exactly
		// one version rather than treating an empty list as an error.
		if len(uniqueTrimmedServiceVersionNames(svc.Versions)) == 0 {
			if err := attachLatestWorkspaceServiceVersion(key, &svc, latestVersions); err != nil {
				return err
			}
		} else if err := attachResolvedWorkspaceVersions(key, &svc, versionIDs); err != nil {
			// One or more explicit versions were declared -- resolve each by
			// exact name instead of substituting the latest.
			return err
		}
		doc.Services[key] = svc
	}
	return nil
}

// uniqueTrimmedServiceVersionNames projects and dedupes the bare version
// names off svc.Versions, mirroring what uniqueTrimmed(svc.Versions) did
// back when Versions was still a flat []string.
func uniqueTrimmedServiceVersionNames(versions []workspaceConfigServiceVersion) []string {
	names := make([]string, 0, len(versions))
	for _, v := range versions {
		names = append(names, v.Version)
	}
	return uniqueTrimmed(names)
}

func explicitWorkspaceVersionRefs(services map[string]workspaceConfigService) []sandbox.ServiceVersionRef {
	var refs []sandbox.ServiceVersionRef
	for _, svc := range services {
		serviceID, err := uuid.Parse(strings.TrimSpace(svc.ServiceID))
		if err != nil {
			// No resolved ServiceID yet (slug resolution runs before this) --
			// nothing to batch for this service until that identity exists.
			continue
		}
		for _, version := range uniqueTrimmedServiceVersionNames(svc.Versions) {
			refs = append(refs, sandbox.ServiceVersionRef{ServiceID: serviceID, Version: version})
		}
	}
	return refs
}

func resolveLatestWorkspaceServiceVersions(ctx context.Context, verifier ServiceVerifier, apiKey string, services map[string]workspaceConfigService) (map[uuid.UUID]sandbox.ServiceVersionResolvedRef, error) {
	serviceIDs := workspaceServicesMissingVersions(services)
	if len(serviceIDs) == 0 {
		return map[uuid.UUID]sandbox.ServiceVersionResolvedRef{}, nil
	}
	resolved, err := verifier.FetchLatestServiceVersions(ctx, serviceIDs, apiKey)
	if err != nil {
		return nil, fmt.Errorf("resolve latest workspace service versions: %w", err)
	}
	return latestWorkspaceVersionMap(resolved), nil
}

func workspaceServicesMissingVersions(services map[string]workspaceConfigService) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(services))
	for _, svc := range services {
		// A service with explicit versions already declared doesn't need a
		// "latest" lookup -- only an empty Versions list does.
		if len(uniqueTrimmedServiceVersionNames(svc.Versions)) != 0 {
			continue
		}
		if id, err := uuid.Parse(strings.TrimSpace(svc.ServiceID)); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func latestWorkspaceVersionMap(resolved []sandbox.ServiceVersionResolvedRef) map[uuid.UUID]sandbox.ServiceVersionResolvedRef {
	out := make(map[uuid.UUID]sandbox.ServiceVersionResolvedRef, len(resolved))
	for _, ref := range resolved {
		out[ref.ServiceID] = ref
	}
	return out
}

func attachLatestWorkspaceServiceVersion(key string, svc *workspaceConfigService, latest map[uuid.UUID]sandbox.ServiceVersionResolvedRef) error {
	serviceID, _ := uuid.Parse(strings.TrimSpace(svc.ServiceID))
	ref := latest[serviceID]
	if ref.Version == "" || ref.ServiceVersionID == uuid.Nil {
		// Registry has nothing public to fall back to -- fail the plan
		// rather than silently leaving this service with zero versions.
		return fmt.Errorf("service %q has no latest public version", key)
	}
	// Replaces Versions outright (there was nothing here to preserve
	// overrides for -- an empty Versions list can't carry any).
	svc.Versions = []workspaceConfigServiceVersion{{Version: ref.Version, ServiceVersionID: ref.ServiceVersionID.String()}}
	return nil
}

// attachResolvedWorkspaceVersions rebuilds Versions in trimmed/deduped
// identity order while preserving each entry's own Public/ExecutionPolicy/
// ConnectionProfiles overrides (workspaceConfigServiceVersionsByName), only
// overwriting the identity fields (Version/ServiceVersionID) this resolution
// pass owns.
func attachResolvedWorkspaceVersions(key string, svc *workspaceConfigService, ids map[uuid.UUID]map[string]uuid.UUID) error {
	serviceID, _ := uuid.Parse(strings.TrimSpace(svc.ServiceID))
	names := uniqueTrimmedServiceVersionNames(svc.Versions)
	byName := workspaceConfigServiceVersionsByName(svc.Versions)
	resolved := make([]workspaceConfigServiceVersion, 0, len(names))
	for _, version := range names {
		versionID := ids[serviceID][version]
		if versionID == uuid.Nil {
			// The explicit version name doesn't exist for this service in
			// Registry -- fail the plan rather than silently dropping it.
			return fmt.Errorf("service %q version %s has no exact service_version_id", key, version)
		}
		// Start from the original entry so its Public/ExecutionPolicy/
		// ConnectionProfiles overrides survive; only identity is overwritten.
		entry := byName[version]
		entry.Version = version
		entry.ServiceVersionID = versionID.String()
		resolved = append(resolved, entry)
	}
	svc.Versions = resolved
	return nil
}

// workspaceConfigServiceVersionsByName indexes each version entry by its
// trimmed name (first entry wins on a user-repeated version) so a rebuild of
// Versions can carry forward Public/ExecutionPolicy/ConnectionProfiles
// without needing to touch them directly.
func workspaceConfigServiceVersionsByName(versions []workspaceConfigServiceVersion) map[string]workspaceConfigServiceVersion {
	byName := make(map[string]workspaceConfigServiceVersion, len(versions))
	for _, v := range versions {
		name := strings.TrimSpace(v.Version)
		// Duplicate version names should have already been rejected by
		// validation upstream, but if one somehow reaches here, keep the
		// first entry deterministically rather than letting later entries
		// silently clobber earlier overrides.
		if _, exists := byName[name]; exists {
			continue
		}
		byName[name] = v
	}
	return byName
}

func unresolvedWorkspaceServiceSlugs(services map[string]workspaceConfigService) []string {
	seen := map[string]bool{}
	for key, service := range services {
		if strings.TrimSpace(service.ServiceID) != "" {
			continue
		}
		if id, err := uuid.Parse(strings.TrimSpace(key)); err == nil {
			service.ServiceID = id.String()
			services[key] = service
			continue
		}
		slug := strings.TrimSpace(key)
		if slug != "" {
			seen[slug] = true
		}
	}
	slugs := make([]string, 0, len(seen))
	for slug := range seen {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

func decodeWorkspaceApplyRequest(r *http.Request) (ConfigApplyRequest, uuid.UUID, error) {
	var req ConfigApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, uuid.Nil, errors.New("invalid request body")
	}
	planID, err := uuid.Parse(req.PlanID)
	if err != nil {
		return req, uuid.Nil, errors.New("invalid plan_id")
	}
	return req, planID, nil
}

func parseWorkspaceConfig(raw json.RawMessage) (workspaceDesiredState, error) {
	var doc workspaceConfigDocument
	if len(raw) == 0 {
		return workspaceDesiredState{}, errors.New("config is required")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return workspaceDesiredState{}, errors.New("config must be an object")
	}
	if err := validateWorkspaceConfigDocument(doc); err != nil {
		return workspaceDesiredState{}, err
	}
	out := workspaceDesiredState{
		Services:     map[uuid.UUID]workspaceDesiredService{},
		Deprecations: map[uuid.UUID][]workspaceDeprecation{},
	}
	for key, svc := range doc.Services {
		desired, err := normalizeWorkspaceService(key, svc)
		if err != nil {
			return workspaceDesiredState{}, err
		}
		if _, exists := out.Services[desired.ServiceID]; exists {
			return workspaceDesiredState{}, fmt.Errorf("duplicate service_id %s", desired.ServiceID)
		}
		out.Services[desired.ServiceID] = desired
	}
	bucketConfigs, err := normalizeWorkspaceBuckets(doc.Buckets, doc.Services, out.Services)
	if err != nil {
		return workspaceDesiredState{}, err
	}
	out.BucketServiceConfigs = bucketConfigs

	bucketSecrets, err := normalizeWorkspaceBucketSecrets(doc.Buckets)
	if err != nil {
		return workspaceDesiredState{}, err
	}
	out.BucketSecrets = bucketSecrets
	if err := normalizeWorkspaceDeprecations(doc.Deprecations, out.Deprecations); err != nil {
		return workspaceDesiredState{}, err
	}
	return out, nil
}

// validateWorkspaceConfigDocument keeps top-level schema checks separate from
// normalization so the parser stays simple as service and bucket sections grow.
func validateWorkspaceConfigDocument(doc workspaceConfigDocument) error {
	if doc.Kind != "" && doc.Kind != "workspace" {
		return errors.New("config kind must be workspace")
	}
	if doc.Services == nil {
		return errors.New("config.services is required")
	}
	return nil
}

func normalizeWorkspaceService(key string, svc workspaceConfigService) (workspaceDesiredService, error) {
	rawID := strings.TrimSpace(svc.ServiceID)
	if rawID == "" {
		// resolveWorkspaceServiceSlugs writes ServiceID back onto the doc
		// keyed by slug -- but if that step was skipped (e.g. the key was
		// already a UUID), fall back to treating the map key itself as the ID.
		rawID = strings.TrimSpace(key)
	}
	serviceID, err := uuid.Parse(rawID)
	if err != nil {
		return workspaceDesiredService{}, fmt.Errorf("service %q requires a valid service_id", key)
	}
	if len(svc.Versions) == 0 {
		// Plan-time resolution (resolveWorkspaceServiceVersions) always
		// populates at least one version, even for an originally-empty list --
		// reaching normalize with none means that step never ran or failed
		// silently, so treat it as a hard error rather than a no-op service.
		return workspaceDesiredService{}, fmt.Errorf("service %q requires at least one version", key)
	}
	versions, versionIDs, err := normalizeWorkspaceServiceVersionIdentities(key, svc.Versions)
	if err != nil {
		return workspaceDesiredService{}, err
	}
	profiles, err := normalizeWorkspaceServiceConnectionProfiles(key, svc.Versions, versionIDs)
	if err != nil {
		return workspaceDesiredService{}, err
	}
	return workspaceDesiredService{
		Key:                key,
		ServiceID:          serviceID,
		ServiceName:        strings.TrimSpace(svc.ServiceName),
		Public:             svc.Public,
		Versions:           versions,
		VersionIDs:         versionIDs,
		ExecutionPolicy:    svc.ExecutionPolicy,
		VersionPolicies:    normalizeWorkspaceVersionPolicies(svc.Versions, versionIDs),
		ConnectionProfiles: profiles,
	}, nil
}

// normalizeWorkspaceServiceVersionIdentities validates and dedupes the
// service's enabled version names and resolves each to its Engine
// service_version_id, mirroring what resolvedWorkspaceVersionIDs +
// ensureResolvedWorkspaceVersions used to do against the separate
// resolved_versions sibling list -- identity now travels with each Versions
// entry directly instead.
func normalizeWorkspaceServiceVersionIdentities(key string, items []workspaceConfigServiceVersion) ([]string, map[string]uuid.UUID, error) {
	versions := make([]string, 0, len(items))
	versionIDs := map[string]uuid.UUID{}
	seen := map[string]bool{}
	for _, item := range items {
		version := strings.TrimSpace(item.Version)
		if version == "" {
			return nil, nil, fmt.Errorf("service %q versions entry requires a version", key)
		}
		if seen[version] {
			// Each version now owns its own overrides directly, so a repeat
			// would mean two conflicting sets of overrides for one identity --
			// reject rather than silently picking one.
			return nil, nil, fmt.Errorf("service %q has duplicate version %s", key, version)
		}
		seen[version] = true
		versions = append(versions, version)
		versionID, err := uuid.Parse(strings.TrimSpace(item.ServiceVersionID))
		if err != nil {
			// Plan-time resolution should have already written a real ID
			// here -- an unparsable one means apply is being run against an
			// unplanned or stale document.
			return nil, nil, fmt.Errorf("service %q version %s is missing service_version_id", key, version)
		}
		versionIDs[version] = versionID
	}
	return versions, versionIDs, nil
}

// normalizeWorkspaceVersionPolicies projects each version's own
// Public/ExecutionPolicy override (when it set either) into the flat
// per-version-policy list the rest of the pipeline already expects. A
// version can't reference an unenabled version or repeat itself anymore --
// nesting makes both cases structurally unrepresentable -- so this can no
// longer fail the way it once could against the separate version_policies
// sibling list.
func normalizeWorkspaceVersionPolicies(items []workspaceConfigServiceVersion, versionIDs map[string]uuid.UUID) []workspaceDesiredVersionPolicy {
	out := make([]workspaceDesiredVersionPolicy, 0, len(items))
	for _, item := range items {
		// A version with neither field set has nothing to override -- skip it
		// so the desired-state policy list only carries entries that
		// genuinely deviate from the service-level default, not one
		// boilerplate row per enabled version.
		if item.Public == nil && item.ExecutionPolicy == nil {
			continue
		}
		version := strings.TrimSpace(item.Version)
		out = append(out, workspaceDesiredVersionPolicy{
			Version: version, VersionID: versionIDs[version],
			Public: item.Public, ExecutionPolicy: item.ExecutionPolicy,
		})
	}
	return out
}

// normalizeWorkspaceBuckets attaches bucket-owned credential intent to already
// normalized service identities so apply never accepts material for a service
// the workspace config did not enable.
func normalizeWorkspaceBuckets(buckets map[string]workspaceConfigBucket, configured map[string]workspaceConfigService, services map[uuid.UUID]workspaceDesiredService) ([]workspaceDesiredBucketServiceConfig, error) {
	out := make([]workspaceDesiredBucketServiceConfig, 0)
	byKey := workspaceDesiredServicesByKey(services)
	for bucketName, bucket := range buckets {
		name := workspaceConnectBucketName(bucketName)
		for serviceKey, serviceConfig := range bucket.ServiceConfig {
			service := byKey[serviceKey]
			if service.ServiceID == uuid.Nil {
				return nil, fmt.Errorf("workspace bucket %q references unknown service %q", name, serviceKey)
			}
			if _, ok := configured[serviceKey]; !ok {
				return nil, fmt.Errorf("workspace bucket %q references unapproved service %q", name, serviceKey)
			}
			if err := validateWorkspaceAuthConfigIntent(serviceKey, serviceConfig.Auth); err != nil {
				return nil, err
			}
			out = append(out, workspaceDesiredBucketServiceConfig{BucketName: name, ServiceKey: serviceKey, ServiceID: service.ServiceID, Auth: serviceConfig.Auth})
		}
	}
	return out, nil
}

// normalizeWorkspaceBucketSecrets validates and flattens every bucket's
// generic Secrets map into the desired-state list, enforcing the same
// $ENV-only discipline as bucket Auth material (Secrets never carries a
// literal value past this point -- resolution happens out-of-band at apply
// time via ConfigApplyRequest.BucketSecretMaterials).
func normalizeWorkspaceBucketSecrets(buckets map[string]workspaceConfigBucket) ([]workspaceDesiredBucketSecret, error) {
	var out []workspaceDesiredBucketSecret
	for bucketName, bucket := range buckets {
		name := workspaceConnectBucketName(bucketName)
		for key, ref := range bucket.Secrets {
			if strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("workspace bucket %q has a secret with an empty key name", name)
			}
			if !isWorkspaceEnvRef(ref) {
				return nil, fmt.Errorf("workspace bucket %q secret %q requires a $ENV reference", name, key)
			}
			out = append(out, workspaceDesiredBucketSecret{BucketName: name, Key: key, EnvRef: ref})
		}
	}
	return out, nil
}

// workspaceDesiredServicesByKey prevents bucket material lookups from scanning
// all services for every bucket entry.
func workspaceDesiredServicesByKey(services map[uuid.UUID]workspaceDesiredService) map[string]workspaceDesiredService {
	byKey := make(map[string]workspaceDesiredService, len(services))
	for _, service := range services {
		byKey[service.Key] = service
	}
	return byKey
}

// normalizeWorkspaceConnectionProfileIntents validates and normalizes one
// service's independent profile intent list. It is deliberately decoupled
// from RuntimeConfig.Connect: a service can declare connection profile intent
// with no bucket material at all, and vice versa (Agreed Product Rules 11-12).
// normalizeWorkspaceServiceConnectionProfiles flattens each version's nested
// ConnectionProfiles into the single per-service list the rest of the
// pipeline already expects, injecting that version's own identity into each
// entry since an intent no longer carries its own `version` field -- it's
// implied by which workspaceConfigServiceVersion it's nested under.
func normalizeWorkspaceServiceConnectionProfiles(key string, items []workspaceConfigServiceVersion, versionIDs map[string]uuid.UUID) ([]workspaceDesiredConnectionProfile, error) {
	seen := map[string]bool{}
	out := make([]workspaceDesiredConnectionProfile, 0)
	for _, versionItem := range items {
		// Most enabled versions won't declare any profile intent at all --
		// skip straight past them rather than looping zero times below.
		if len(versionItem.ConnectionProfiles) == 0 {
			continue
		}
		version := strings.TrimSpace(versionItem.Version)
		for _, item := range versionItem.ConnectionProfiles {
			desired, err := normalizeWorkspaceConnectionProfileIntent(key, version, item, versionIDs)
			if err != nil {
				return nil, err
			}
			dedupeKey := desired.VersionID.String() + "\x00" + desired.AuthType
			if seen[dedupeKey] {
				// Two intents for the same version+auth_type would make "which
				// one actually applies" ambiguous -- reject outright.
				return nil, fmt.Errorf("service %q has duplicate connection_profiles for version %s auth_type %s", key, desired.Version, desired.AuthType)
			}
			seen[dedupeKey] = true
			out = append(out, desired)
		}
	}
	return out, nil
}

// normalizeWorkspaceConnectionProfileIntent enforces the same mutual
// exclusivity and structural rules as the old Connect-embedded profile intent
// (validateWorkspaceConnectProfileIntent), now scoped to one version/auth
// tuple instead of implicitly fanning across every version a service enables.
// version/versionIDs come from the owning workspaceConfigServiceVersion entry
// -- an intent itself no longer carries a `version` field, and nesting makes
// "references an unenabled version" structurally unrepresentable, so there's
// no equivalent of the old containsString check to run here anymore.
func normalizeWorkspaceConnectionProfileIntent(key, version string, item workspaceConfigConnectionProfileIntent, versionIDs map[string]uuid.UUID) (workspaceDesiredConnectionProfile, error) {
	authType := connectionprofile.CanonicalAuthType(item.AuthType)
	if !isSupportedConnectAuthType(authType) {
		return workspaceDesiredConnectionProfile{}, fmt.Errorf("service %q connection_profiles has unsupported auth_type", key)
	}
	if item.Reset && (item.Profile != nil || strings.TrimSpace(item.ProfileID) != "") {
		// Reset means "clear whatever override exists" -- combining it with a
		// concrete profile/profile_id would be a self-contradictory intent.
		return workspaceDesiredConnectionProfile{}, fmt.Errorf("service %q connection_profiles reset cannot include profile or profile_id", key)
	}
	// An inline profile body needs its own auth_type/shape validation; a bare
	// profile_id selection or a reset has nothing further to check here.
	if item.Profile != nil {
		if connectionprofile.CanonicalAuthType(item.Profile.AuthType) != authType {
			return workspaceDesiredConnectionProfile{}, fmt.Errorf("service %q connection profile auth_type must match its auth_type", key)
		}
		if err := connectionprofile.Validate(item.Profile, connectionprofile.Contract{}).Err(); err != nil {
			return workspaceDesiredConnectionProfile{}, fmt.Errorf("service %q connection profile: %w", key, err)
		}
	}
	return workspaceDesiredConnectionProfile{
		Version: version, VersionID: versionIDs[version], AuthType: authType,
		ProfileID: strings.TrimSpace(item.ProfileID), Profile: item.Profile, Reset: item.Reset,
		Public: item.Public,
		// Resolved/Ambiguous/Reason are Engine-populated during plan resolution
		// (attachResolvedWorkspaceProfiles) and must survive the round trip
		// through plan.ResolvedPayload back into workspaceDesiredState at apply
		// time, since apply never re-runs automatic selection itself.
		Resolved: item.Resolved, Ambiguous: item.Ambiguous, Reason: item.Reason,
	}, nil
}

func validateWorkspaceAuthConfigIntent(key string, auth *WorkspaceAuthConfig) error {
	if auth == nil {
		return nil
	}
	authType := canonicalWorkspaceStaticAuthType(auth.AuthType)
	switch authType {
	case "basic":
		return validateWorkspaceAuthEnvRefs(key, authType, auth.Username, auth.Password)
	case "api_key":
		return validateWorkspaceAuthEnvRefs(key, authType, auth.APIKey)
	case "mtls":
		return validateWorkspaceAuthEnvRefs(key, authType, auth.Cert, auth.Key)
	case "bearer", "oauth", "oidc":
		return validateWorkspaceAuthEnvRefs(key, authType, auth.Token)
	default:
		return fmt.Errorf("service %q auth has unsupported auth_type", key)
	}
}

// validateWorkspaceAuthEnvRefs rejects inline static credentials before they
// can be stored in config plans; apply receives resolved values out-of-band.
func validateWorkspaceAuthEnvRefs(key, authType string, values ...string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || !isWorkspaceEnvRef(value) {
			// Static auth is encrypted into bucket secrets during apply; config
			// state stores only env refs so secret rotation stays local/operator owned.
			return fmt.Errorf("service %q auth %s requires $ENV credential fields", key, authType)
		}
	}
	return nil
}

func resolveWorkspaceServiceVisibility(
	ctx context.Context,
	resolver any,
	apiKey string,
	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
) (map[uuid.UUID]sandbox.ServiceVisibility, error) {
	// Collect IDs that need visibility data:
	// (a) services with any public-flag intent -- service-level, execution
	//     policy, connection profile, or version_policies (ownership validation
	//     required for all of these; see workspaceServicesWithPublicIntent)
	// (b) previously-managed services no longer in desired (need ownership to
	//     label plan output as "[archive from registry]" vs "[remove from workspace]")
	seen := map[uuid.UUID]bool{}
	var serviceIDs []uuid.UUID
	for _, serviceID := range workspaceServicesWithPublicIntent(desired) {
		serviceIDs = append(serviceIDs, serviceID)
		seen[serviceID] = true
	}
	for serviceID := range previousManaged {
		if _, stillDesired := desired.Services[serviceID]; !stillDesired && !seen[serviceID] {
			serviceIDs = append(serviceIDs, serviceID)
			seen[serviceID] = true
		}
	}
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	visibilityResolver, ok := resolver.(ServiceVisibilityResolver)
	if !ok {
		return nil, errors.New("workspace service visibility resolution is unavailable")
	}
	visibility, err := visibilityResolver.FetchServiceVisibility(ctx, serviceIDs, apiKey)
	if err != nil {
		return nil, fmt.Errorf("resolve service visibility: %w", err)
	}
	return validateWorkspacePublicIntent(desired, visibility)
}

// workspaceServicesWithPublicIntent collects every service that has any
// public-flag intent (service-level, execution policy, or connection profile),
// so a single FetchServiceVisibility round trip covers all ownership checks.
func workspaceServicesWithPublicIntent(desired workspaceDesiredState) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{})
	for serviceID, svc := range desired.Services {
		if svc.Public != nil {
			seen[serviceID] = struct{}{}
			continue
		}
		if svc.ExecutionPolicy != nil && svc.ExecutionPolicy.Public != nil && *svc.ExecutionPolicy.Public {
			seen[serviceID] = struct{}{}
			continue
		}
		for _, p := range svc.ConnectionProfiles {
			if p.Public != nil && *p.Public {
				seen[serviceID] = struct{}{}
				break
			}
		}
		for _, vp := range svc.VersionPolicies {
			if vp.Public != nil {
				seen[serviceID] = struct{}{}
				break
			}
			if vp.ExecutionPolicy != nil && vp.ExecutionPolicy.Public != nil && *vp.ExecutionPolicy.Public {
				seen[serviceID] = struct{}{}
				break
			}
		}
	}
	out := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func validateWorkspacePublicIntent(
	desired workspaceDesiredState,
	visibility map[uuid.UUID]sandbox.ServiceVisibility,
) (map[uuid.UUID]sandbox.ServiceVisibility, error) {
	for serviceID, svc := range desired.Services {
		vis := visibility[serviceID]

		if svc.Public != nil {
			// Service-level public toggle: only the owner may publish the
			// service page to the Registry.
			if !vis.IsOwner {
				return nil, fmt.Errorf("service %s public can only be set for services owned by this workspace", serviceID)
			}
		}

		if svc.ExecutionPolicy != nil && svc.ExecutionPolicy.Public != nil && *svc.ExecutionPolicy.Public {
			// Publishing execution policy (rate_limit/retry) to the Registry
			// changes the defaults inherited by all consumers, so it is
			// strictly owner-only.
			if !vis.IsOwner {
				return nil, fmt.Errorf("service %s execution_policy.public can only be set for services owned by this workspace", serviceID)
			}
		}

		for i, p := range svc.ConnectionProfiles {
			if p.Public == nil || !*p.Public {
				continue
			}
			// Publishing a connection profile to the Registry means any
			// consumer of the service can adopt it as a baseline, so only
			// the owning workspace may do this.
			if !vis.IsOwner {
				return nil, fmt.Errorf("service %s connection_profiles[%d].public can only be set for services owned by this workspace", serviceID, i)
			}
		}

		for _, vp := range svc.VersionPolicies {
			if vp.Public != nil && !vis.IsOwner {
				return nil, fmt.Errorf("service %s version %s public can only be set for services owned by this workspace", serviceID, vp.Version)
			}
			if vp.ExecutionPolicy != nil && vp.ExecutionPolicy.Public != nil && *vp.ExecutionPolicy.Public && !vis.IsOwner {
				return nil, fmt.Errorf("service %s version %s execution_policy.public can only be set for services owned by this workspace", serviceID, vp.Version)
			}
		}
	}
	return visibility, nil
}

func buildWorkspacePlanSummary(
	ctx context.Context,
	configStore store.ConfigRepository,
	s store.Store,
	verifier ServiceVerifier,

	apiKey string,
	desired workspaceDesiredState,
	currentWorkspace map[uuid.UUID]currentWorkspaceService,
	previousManaged map[uuid.UUID]workspaceManagedService,
) (workspacePlanSummary, error) {
	sdkImpacts, err := store.WorkspaceSDKServiceImpacts(ctx, configStore, s)
	if err != nil {
		return workspacePlanSummary{}, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to inspect SDK usage"}
	}
	visibility, err := resolveWorkspaceServiceVisibility(ctx, verifier, apiKey, desired, previousManaged)
	if err != nil {
		return workspacePlanSummary{}, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	summary := planWorkspaceChanges(desired, currentWorkspace, previousManaged, sdkImpacts, visibility)
	if err := reconcileWorkspaceProfilePlanActions(ctx, s, desired, &summary); err != nil {
		return workspacePlanSummary{}, err
	}
	// Ambiguous tuples are surfaced as warnings, not blockers: the plan still
	// contains every other action (including unrelated attach/enable actions
	// for the same or other services), and this tuple simply has no
	// attach_connection_profile action until an explicit profile_id is set.
	summary.Warnings = append(summary.Warnings, ambiguousConnectionProfileWarnings(desired)...)
	sortWorkspacePlanSummary(&summary)
	return summary, nil
}

// ambiguousConnectionProfileWarnings reports every unresolved tuple in one
// pass so a caller can fix all of them at once instead of discovering them
// one plan-apply cycle at a time.
func ambiguousConnectionProfileWarnings(desired workspaceDesiredState) []workspacePlanWarning {
	var warnings []workspacePlanWarning
	for _, svc := range sortedDesiredServices(desired) {
		for _, intent := range svc.ConnectionProfiles {
			if !intent.Ambiguous {
				continue
			}
			warnings = append(warnings, workspacePlanWarning{
				Code: "connection_profile_ambiguous", ServiceID: svc.ServiceID.String(),
				Message: fmt.Sprintf("service %s version %s auth_type %s: %s", svc.ServiceID, intent.Version, intent.AuthType, intent.Reason),
			})
		}
	}
	return warnings
}

// reconcileWorkspaceProfilePlanActions suppresses plan actions that apply's
// omission rule will skip because an exact current profile already exists.
// Unlike the old bucket-owned model, this needs no bucket resolution at all:
// a workspace profile's identity is workspace + service + service_version +
// auth_type, so workspaceID alone is enough to build its lookup refs.
func reconcileWorkspaceProfilePlanActions(ctx context.Context, s store.Store, desired workspaceDesiredState, summary *workspacePlanSummary) error {
	return suppressPreservedAutomaticProfileActions(ctx, s, desired, summary)
}

type automaticWorkspaceProfileCandidate struct {
	Ref          store.WorkspaceProfileRef
	AttachID     string
	ServiceID    uuid.UUID
	Version      string
	BindingCount int
}

// suppressPreservedAutomaticProfileActions removes only actions that apply's
// omission rule will skip because an exact current profile already exists.
func suppressPreservedAutomaticProfileActions(ctx context.Context, s store.Store, desired workspaceDesiredState, summary *workspacePlanSummary) error {
	candidates := automaticWorkspaceProfileCandidates(desired)
	if len(candidates) == 0 {
		return nil
	}
	profileStore, err := engineProfileStore(s)
	if err != nil {
		return err
	}
	refs := automaticWorkspaceProfileRefs(candidates)
	current, err := profileStore.GetEffectiveWorkspaceProfiles(ctx, refs)
	if err != nil {
		return err
	}
	suppressed := preservedAutomaticProfileActionIDs(candidates, indexWorkspaceProfiles(current))
	summary.Actions = workspaceActionsWithoutIDs(summary.Actions, suppressed)
	return nil
}

// automaticWorkspaceProfileCandidates mirrors apply's implicit-selection rule
// and keeps explicit profile/profile_id replacements visible in the plan.
func automaticWorkspaceProfileCandidates(desired workspaceDesiredState) []automaticWorkspaceProfileCandidate {
	var candidates []automaticWorkspaceProfileCandidate
	for _, svc := range sortedDesiredServices(desired) {
		for _, intent := range svc.ConnectionProfiles {
			if intent.Reset || intent.explicit() || !intent.hasSource() {
				continue
			}
			candidates = append(candidates, automaticWorkspaceProfileCandidate{
				Ref:      store.WorkspaceProfileRef{ServiceID: svc.ServiceID, ServiceVersionID: intent.VersionID, AuthType: intent.AuthType},
				AttachID: workspaceActionID("attach_connection_profile", svc.ServiceID, intent.Version), ServiceID: svc.ServiceID,
				Version: intent.Version, BindingCount: len(intent.Resolved.Config.Bindings),
			})
		}
	}
	return candidates
}

// automaticWorkspaceProfileRefs projects the exact tuples consumed by the
// store's composite batch lookup without broad workspace profile loading.
func automaticWorkspaceProfileRefs(candidates []automaticWorkspaceProfileCandidate) []store.WorkspaceProfileRef {
	refs := make([]store.WorkspaceProfileRef, len(candidates))
	for index := range candidates {
		refs[index] = candidates[index].Ref
	}
	return refs
}

// preservedAutomaticProfileActionIDs includes an attachment and all binding
// actions because apply preserves that entire existing profile atomically.
func preservedAutomaticProfileActionIDs(candidates []automaticWorkspaceProfileCandidate, current map[string]*store.WorkspaceConnectionProfile) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, candidate := range candidates {
		if current[workspaceProfileRefKey(candidate.Ref)] == nil {
			continue
		}
		ids[candidate.AttachID] = struct{}{}
		for index := 0; index < candidate.BindingCount; index++ {
			ids[workspaceActionID("create_bucket_binding", candidate.ServiceID, candidate.Version, fmt.Sprint(index))] = struct{}{}
		}
	}
	return ids
}

// workspaceActionsWithoutIDs retains deterministic order while dropping only
// action IDs proven to be no-ops against current profile state.
func workspaceActionsWithoutIDs(actions []workspacePlanAction, suppressed map[string]struct{}) []workspacePlanAction {
	kept := make([]workspacePlanAction, 0, len(actions))
	for _, action := range actions {
		if _, remove := suppressed[action.ID]; !remove {
			kept = append(kept, action)
		}
	}
	return kept
}

func normalizeWorkspaceDeprecations(items []workspaceConfigDeprecation, out map[uuid.UUID][]workspaceDeprecation) error {
	for _, item := range items {
		deprecation, err := normalizeWorkspaceDeprecation(item)
		if err != nil {
			return err
		}
		out[deprecation.ServiceID] = append(out[deprecation.ServiceID], deprecation)
	}
	return nil
}

func normalizeWorkspaceDeprecation(item workspaceConfigDeprecation) (workspaceDeprecation, error) {
	serviceID, err := uuid.Parse(strings.TrimSpace(item.ServiceID))
	if err != nil {
		return workspaceDeprecation{}, errors.New("deprecation requires a valid service_id")
	}
	effectiveAt := strings.TrimSpace(item.EffectiveAt)
	if effectiveAt == "" {
		return workspaceDeprecation{}, fmt.Errorf("deprecation for service %s requires effective_at", serviceID)
	}
	if _, err := time.Parse("2006-01-02", effectiveAt); err != nil {
		return workspaceDeprecation{}, fmt.Errorf("deprecation for service %s requires effective_at as YYYY-MM-DD", serviceID)
	}
	return workspaceDeprecation{
		ServiceID:   serviceID,
		Version:     strings.TrimSpace(item.Version),
		EffectiveAt: effectiveAt,
		Reason:      strings.TrimSpace(item.Reason),
	}, nil
}

type currentWorkspaceService struct {
	ServiceID  uuid.UUID
	Version    string
	Versions   map[string]bool
	VersionIDs map[string]uuid.UUID
}

func loadCurrentWorkspaceState(ctx context.Context, s store.Store) (map[uuid.UUID]currentWorkspaceService, error) {
	services, err := s.ListWorkspaceServices(ctx, nil)
	if err != nil {
		return nil, err
	}
	// One batched lookup for every activated service's allowed versions,
	// instead of one ListWorkspaceServiceVersions call per service in the loop
	// below -- a workspace plan with N activated services used to cost N+1
	// queries just to describe "what's already there".
	allowedVersions, err := s.ListWorkspaceServiceVersionsForServices(ctx, workspaceServiceIDs(services))
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]currentWorkspaceService{}
	for _, svc := range services {
		out[svc.ServiceID] = currentWorkspaceService{
			ServiceID:  svc.ServiceID,
			Version:    svc.Version,
			Versions:   enabledVersionSet(allowedVersions[svc.ServiceID]),
			VersionIDs: enabledVersionIDSet(allowedVersions[svc.ServiceID]),
		}
	}
	return out, nil
}

func workspaceServiceIDs(services []store.WorkspaceService) []uuid.UUID {
	ids := make([]uuid.UUID, len(services))
	for i, svc := range services {
		ids[i] = svc.ServiceID
	}
	return ids
}

func enabledVersionSet(versions []store.WorkspaceServiceVersion) map[string]bool {
	set := map[string]bool{}
	for _, version := range versions {
		set[version.Version] = true
	}
	return set
}

func enabledVersionIDSet(versions []store.WorkspaceServiceVersion) map[string]uuid.UUID {
	set := map[string]uuid.UUID{}
	for _, version := range versions {
		set[version.Version] = version.ServiceVersionID
	}
	return set
}

func parseManagedWorkspaceResources(state *store.ConfigState) (map[uuid.UUID]workspaceManagedService, error) {
	if state == nil || len(state.ManagedResources) == 0 {
		return map[uuid.UUID]workspaceManagedService{}, nil
	}
	var managed workspaceManagedResources
	if err := json.Unmarshal(state.ManagedResources, &managed); err != nil {
		return nil, err
	}
	out := map[uuid.UUID]workspaceManagedService{}
	for _, svc := range managed.Services {
		serviceID, err := uuid.Parse(svc.ServiceID)
		if err != nil {
			return nil, err
		}
		svc.Versions = uniqueTrimmed(svc.Versions)
		out[serviceID] = svc
	}
	return out, nil
}

// planWorkspaceChanges combines desired additions, visibility, deprecations,
// profile actions, and managed removals into one ordered review surface.
func planWorkspaceChanges(
	desired workspaceDesiredState,
	current map[uuid.UUID]currentWorkspaceService,
	previousManaged map[uuid.UUID]workspaceManagedService,
	sdkImpacts map[uuid.UUID]map[uuid.UUID][]string,
	visibility map[uuid.UUID]sandbox.ServiceVisibility,
) workspacePlanSummary {
	var summary workspacePlanSummary
	summary.Actions = append(summary.Actions, desiredWorkspaceActions(desired, current)...)
	summary.Actions = append(summary.Actions, desiredWorkspaceVisibilityActions(desired, visibility)...)
	summary.Actions = append(summary.Actions, desiredVersionVisibilityActions(desired)...)
	summary.Actions = append(summary.Actions, desiredExecutionPolicyPublishActions(desired)...)
	summary.Actions = append(summary.Actions, desiredVersionExecutionPolicyPublishActions(desired)...)
	summary.Actions = append(summary.Actions, desiredExecutionPolicyLocalActions(desired)...)
	summary.Actions = append(summary.Actions, desiredVersionExecutionPolicyLocalActions(desired)...)
	summary.Actions = append(summary.Actions, desiredConnectionProfilePublishActions(desired)...)
	summary.Actions = append(summary.Actions, desiredDeprecationActions(desired, sdkImpacts, &summary)...)
	summary.Actions = append(summary.Actions, managedWorkspaceRemovalActions(desired, previousManaged, current, sdkImpacts, visibility, &summary)...)
	summary.UnmanagedServices = unmanagedWorkspaceServices(desired, current, previousManaged)
	sortWorkspacePlanSummary(&summary)
	return summary
}

// desiredWorkspaceActions emits deterministic service, version, and profile
// changes so apply never performs behavior omitted from the visible plan.
func desiredWorkspaceActions(desired workspaceDesiredState, current map[uuid.UUID]currentWorkspaceService) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		currentSvc, exists := current[serviceID]
		if !exists {
			actions = append(actions, workspacePlanAction{ID: workspaceActionID("add_service", serviceID), Type: "add_service", ServiceID: serviceID.String(), ServiceName: svc.ServiceName})
		}
		for _, version := range svc.Versions {
			if !currentSvc.Versions[version] {
				actions = append(actions, workspacePlanAction{
					ID: workspaceActionID("enable_service_version", serviceID, version), Type: "enable_service_version",
					ServiceID: serviceID.String(), Version: version, ServiceVersionID: svc.VersionIDs[version].String(),
				})
			}
		}
		actions = append(actions, desiredConnectionProfileActions(svc)...)
	}
	return actions
}

// desiredConnectionProfileActions describes each version attachment and safe
// binding structure without exposing literal or environment-resolved values.
// It operates purely on svc.ConnectionProfiles -- entirely independent of
// RuntimeConfig.Connect -- so an activation's profile action list is visible
// whether or not the service also declares bucket material. An ambiguous
// intent (Ambiguous==true) contributes no action here; it is reported as a
// plan warning instead (see ambiguousConnectionProfileWarnings) so it cannot
// block sibling actions in the same or other services.
func desiredConnectionProfileActions(svc workspaceDesiredService) []workspacePlanAction {
	var actions []workspacePlanAction
	for _, intent := range svc.ConnectionProfiles {
		if intent.Reset {
			actions = append(actions, workspacePlanAction{
				ID: workspaceActionID("detach_connection_profile", svc.ServiceID, intent.Version), Type: "detach_connection_profile",
				ServiceID: svc.ServiceID.String(), ServiceName: svc.ServiceName, Version: intent.Version,
				ServiceVersionID: intent.VersionID.String(), AuthType: intent.AuthType,
			})
			continue
		}
		if !intent.hasSource() {
			continue
		}
		profile := intent.Profile
		revision, provenance := 1, "workspace"
		if intent.Resolved != nil {
			revision, provenance = intent.Resolved.Revision, intent.Resolved.Provenance
			resolvedProfile := intent.Resolved.Config
			profile = &resolvedProfile
		}
		actions = append(actions, workspacePlanAction{
			ID:   workspaceActionID("attach_connection_profile", svc.ServiceID, intent.Version),
			Type: "attach_connection_profile", ServiceID: svc.ServiceID.String(), ServiceName: svc.ServiceName,
			Version: intent.Version, ServiceVersionID: intent.VersionID.String(),
			AuthType: intent.AuthType, ProfileRevision: revision, ProfileProvenance: provenance,
		})
		for index, binding := range profile.Bindings {
			actions = append(actions, workspacePlanAction{
				ID:   workspaceActionID("create_bucket_binding", svc.ServiceID, intent.Version, fmt.Sprint(index)),
				Type: "create_bucket_binding", ServiceID: svc.ServiceID.String(), ServiceName: svc.ServiceName,
				Version: intent.Version, ServiceVersionID: intent.VersionID.String(),
				TargetLocation: binding.Location, TargetName: binding.Name, BindingSource: safeBindingPlanSource(binding.Value),
			})
		}
	}
	return actions
}

// safeBindingPlanSource preserves structural source intent in plan output but
// redacts literal values that may contain workspace-specific material.
func safeBindingPlanSource(value string) string {
	expression, err := connectionprofile.ParseExpression(value)
	if err != nil {
		return "invalid"
	}
	switch expression.Kind {
	case connectionprofile.SourceEnvironment:
		return "$" + expression.EnvName
	case connectionprofile.SourceConnectionResource:
		return expression.Raw
	default:
		return "literal"
	}
}

// desiredWorkspaceVisibilityActions records ownership-authorized visibility
// changes separately from profile behavior so neither can imply the other.
func desiredWorkspaceVisibilityActions(
	desired workspaceDesiredState,
	visibility map[uuid.UUID]sandbox.ServiceVisibility,
) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		if svc.Public == nil || visibility[serviceID].IsPublic == *svc.Public {
			continue
		}
		actionType := workspaceVisibilityActionType(*svc.Public)
		actions = append(actions, workspacePlanAction{
			ID:          workspaceActionID(actionType, serviceID),
			Type:        actionType,
			ServiceID:   serviceID.String(),
			ServiceName: svc.ServiceName,
			Public:      svc.Public,
		})
	}
	return actions
}

func workspaceVisibilityActionType(isPublic bool) string {
	if isPublic {
		return "set_service_public"
	}
	return "set_service_private"
}

// desiredVersionVisibilityActions emits a set_service_version_public/private
// action for every version_policies entry with Public set. Unlike
// desiredWorkspaceVisibilityActions, this has no per-version current-state
// fetch to diff against (the Registry visibility fetch is service-scoped
// only), so the action is always emitted when declared -- matching
// desiredExecutionPolicyPublishActions' rationale below, since
// UpdateServiceVersionPublicStatus is idempotent.
func desiredVersionVisibilityActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		for _, vp := range svc.VersionPolicies {
			if vp.Public == nil {
				continue
			}
			actionType := workspaceVersionVisibilityActionType(*vp.Public)
			actions = append(actions, workspacePlanAction{
				ID:               workspaceActionID(actionType, serviceID, vp.Version),
				Type:             actionType,
				ServiceID:        serviceID.String(),
				ServiceName:      svc.ServiceName,
				Version:          vp.Version,
				ServiceVersionID: vp.VersionID.String(),
				Public:           vp.Public,
			})
		}
	}
	return actions
}

func workspaceVersionVisibilityActionType(isPublic bool) string {
	if isPublic {
		return "set_service_version_public"
	}
	return "set_service_version_private"
}

// desiredExecutionPolicyPublishActions emits a
// "publish_service_execution_policy" action whenever the workspace declares
// execution_policy.public=true for an owned service. The action is always
// emitted (not only on change) because Registry publishes are idempotent and
// Engine has no local mirror of the currently published provider policy.
func desiredExecutionPolicyPublishActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		ep := svc.ExecutionPolicy
		if ep == nil || ep.Public == nil || !*ep.Public {
			continue
		}
		actions = append(actions, workspacePlanAction{
			ID:          workspaceActionID("publish_service_execution_policy", serviceID),
			Type:        "publish_service_execution_policy",
			ServiceID:   serviceID.String(),
			ServiceName: svc.ServiceName,
			Public:      ep.Public,
		})
	}
	return actions
}

// desiredVersionExecutionPolicyPublishActions emits a
// "publish_service_version_execution_policy" action whenever the workspace
// declares version_policies[*].execution_policy.public=true for an owned
// service, mirroring desiredExecutionPolicyPublishActions at version scope.
func desiredVersionExecutionPolicyPublishActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		for _, vp := range svc.VersionPolicies {
			ep := vp.ExecutionPolicy
			if ep == nil || ep.Public == nil || !*ep.Public {
				continue
			}
			actions = append(actions, workspacePlanAction{
				ID:               workspaceActionID("publish_service_version_execution_policy", serviceID, vp.Version),
				Type:             "publish_service_version_execution_policy",
				ServiceID:        serviceID.String(),
				ServiceName:      svc.ServiceName,
				Version:          vp.Version,
				ServiceVersionID: vp.VersionID.String(),
				Public:           ep.Public,
			})
		}
	}
	return actions
}

// desiredExecutionPolicyLocalActions emits a "set_local_execution_policy" (or
// "reset_local_execution_policy", when ep.Reset is set) action whenever the
// workspace declares an execution_policy for a service, independent of
// Public. Unlike desiredExecutionPolicyPublishActions this is not gated on
// ownership or Public=true: a workspace can locally enforce rate_limit/
// retry_config/pagination/webhook config for a service it doesn't own, or
// hasn't published, the same way workspace connection profile overrides
// already work (see plans/plan-service-config-restructure.md's local-
// enforcement gap). Local and publish actions are independent and both may
// be emitted for the same service -- one governs this workspace's own
// enforcement, the other governs what every other consumer inherits.
func desiredExecutionPolicyLocalActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		ep := svc.ExecutionPolicy
		if ep == nil {
			continue
		}
		actionType := "set_local_execution_policy"
		if ep.Reset {
			actionType = "reset_local_execution_policy"
		}
		actions = append(actions, workspacePlanAction{
			ID:          workspaceActionID(actionType, serviceID),
			Type:        actionType,
			ServiceID:   serviceID.String(),
			ServiceName: svc.ServiceName,
		})
	}
	return actions
}

// desiredVersionExecutionPolicyLocalActions is
// desiredExecutionPolicyLocalActions' per-version sibling, mirroring
// desiredVersionExecutionPolicyPublishActions' scope.
func desiredVersionExecutionPolicyLocalActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		for _, vp := range svc.VersionPolicies {
			ep := vp.ExecutionPolicy
			if ep == nil {
				continue
			}
			actionType := "set_local_service_version_execution_policy"
			if ep.Reset {
				actionType = "reset_local_service_version_execution_policy"
			}
			actions = append(actions, workspacePlanAction{
				ID:               workspaceActionID(actionType, serviceID, vp.Version),
				Type:             actionType,
				ServiceID:        serviceID.String(),
				ServiceName:      svc.ServiceName,
				Version:          vp.Version,
				ServiceVersionID: vp.VersionID.String(),
			})
		}
	}
	return actions
}

// desiredConnectionProfilePublishActions emits a
// "publish_connection_profile" action for every profile intent that declares
// public=true on an owned service. Like the execution policy action, it is
// always emitted rather than diffed, since the Registry has no local mirror.
func desiredConnectionProfilePublishActions(desired workspaceDesiredState) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, svc := range desired.Services {
		for _, intent := range svc.ConnectionProfiles {
			if intent.Public == nil || !*intent.Public {
				continue
			}
			actions = append(actions, workspacePlanAction{
				ID:               workspaceActionID("publish_connection_profile", serviceID, intent.Version, intent.AuthType),
				Type:             "publish_connection_profile",
				ServiceID:        serviceID.String(),
				ServiceName:      svc.ServiceName,
				Version:          intent.Version,
				ServiceVersionID: intent.VersionID.String(),
				AuthType:         intent.AuthType,
				Public:           intent.Public,
			})
		}
	}
	return actions
}

func desiredDeprecationActions(
	desired workspaceDesiredState,
	sdkImpacts map[uuid.UUID]map[uuid.UUID][]string,
	summary *workspacePlanSummary,
) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, deprecations := range desired.Deprecations {
		desiredSvc, serviceConfigured := desired.Services[serviceID]
		if !serviceConfigured {
			continue
		}
		for _, deprecation := range deprecations {
			if deprecation.Version == "" {
				var allImpacted []string
				for _, impacts := range sdkImpacts[serviceID] {
					allImpacted = append(allImpacted, impacts...)
				}
				actions = append(actions, deprecateServiceAction(serviceID, deprecation, allImpacted, summary))
				continue
			}
			if containsString(desiredSvc.Versions, deprecation.Version) {
				actions = append(actions, deprecateVersionAction(serviceID, deprecation.Version, deprecation))
			}
		}
	}
	return actions
}

func managedWorkspaceRemovalActions(
	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
	current map[uuid.UUID]currentWorkspaceService,
	sdkImpacts map[uuid.UUID]map[uuid.UUID][]string,
	visibility map[uuid.UUID]sandbox.ServiceVisibility,
	summary *workspacePlanSummary,
) []workspacePlanAction {
	var actions []workspacePlanAction
	for serviceID, managed := range previousManaged {
		desiredSvc, stillManaged := desired.Services[serviceID]
		if !stillManaged {
			if deprecation, ok := serviceDeprecationDirective(desired, serviceID); ok {
				var allImpacted []string
				for _, impacts := range sdkImpacts[serviceID] {
					allImpacted = append(allImpacted, impacts...)
				}
				actions = append(actions, deprecateServiceAction(serviceID, deprecation, allImpacted, summary))
				continue
			}
			action := workspacePlanAction{
				ID:        workspaceActionID("remove_service", serviceID),
				Type:      "remove_service",
				ServiceID: serviceID.String(),
				// WillArchive tells the user that this Engine owns the service, so
				// confirming the apply will also soft-delete it from the Registry
				// (not just deactivate it locally). The label shown in plan output
				// will read "[archive from registry]" instead of "[remove from workspace]".
				WillArchive: visibility[serviceID].IsOwner,
			}
			var allImpacted []string
			for _, impacts := range sdkImpacts[serviceID] {
				allImpacted = append(allImpacted, impacts...)
			}
			actions = append(actions, attachServiceRemovalImpact(action, allImpacted, summary))
			continue
		}
		for _, version := range managed.Versions {
			if !containsString(desiredSvc.Versions, version) {
				if deprecation, ok := versionDeprecationDirective(desired, serviceID, version); ok {
					actions = append(actions, deprecateVersionAction(serviceID, version, deprecation))
					continue
				}
				action := workspacePlanAction{
					ID:        workspaceActionID("disable_service_version", serviceID, version),
					Type:      "disable_service_version",
					ServiceID: serviceID.String(),
					Version:   version,
				}
				// We need the service_version_id to check exact impact.
				versionID := current[serviceID].VersionIDs[version]
				actions = append(actions, attachVersionRemovalImpact(action, sdkImpacts[serviceID][versionID], summary))
			}
		}
	}
	return actions
}

func serviceDeprecationDirective(desired workspaceDesiredState, serviceID uuid.UUID) (workspaceDeprecation, bool) {
	for _, deprecation := range desired.Deprecations[serviceID] {
		if deprecation.Version == "" {
			return deprecation, true
		}
	}
	return workspaceDeprecation{}, false
}

func versionDeprecationDirective(desired workspaceDesiredState, serviceID uuid.UUID, version string) (workspaceDeprecation, bool) {
	for _, deprecation := range desired.Deprecations[serviceID] {
		if deprecation.Version == version {
			return deprecation, true
		}
	}
	return workspaceDeprecation{}, false
}

func deprecateServiceAction(serviceID uuid.UUID, deprecation workspaceDeprecation, impactedSDKs []string, summary *workspacePlanSummary) workspacePlanAction {
	action := workspacePlanAction{
		ID:               workspaceActionID("deprecate_service", serviceID),
		Type:             "deprecate_service",
		ServiceID:        serviceID.String(),
		EffectiveAt:      deprecation.EffectiveAt,
		Reason:           deprecation.Reason,
		SuggestedCommand: suggestedServiceDeprecationCommand(serviceID, deprecation.EffectiveAt),
	}
	if len(impactedSDKs) == 0 {
		return action
	}
	action.ImpactedSDKConfigs = append([]string(nil), impactedSDKs...)
	action.Recommendation = "SDK configs still use this service. The deprecation directive keeps the service active while consumers migrate."
	summary.Warnings = append(summary.Warnings, workspacePlanWarning{
		Code:             "sdk_uses_deprecated_service",
		ServiceID:        action.ServiceID,
		Message:          action.Recommendation,
		SDKs:             action.ImpactedSDKConfigs,
		SuggestedCommand: action.SuggestedCommand,
	})
	return action
}

func deprecateVersionAction(serviceID uuid.UUID, version string, deprecation workspaceDeprecation) workspacePlanAction {
	return workspacePlanAction{
		ID:               workspaceActionID("deprecate_version", serviceID, version),
		Type:             "deprecate_version",
		ServiceID:        serviceID.String(),
		Version:          version,
		EffectiveAt:      deprecation.EffectiveAt,
		Reason:           deprecation.Reason,
		SuggestedCommand: suggestedVersionDeprecationCommand(serviceID, version, deprecation.EffectiveAt),
	}
}

func attachServiceRemovalImpact(action workspacePlanAction, impactedSDKs []string, summary *workspacePlanSummary) workspacePlanAction {
	if len(impactedSDKs) == 0 {
		return action
	}
	action.RequiresDecision = true
	action.ImpactedSDKConfigs = append([]string(nil), impactedSDKs...)
	action.SuggestedCommand = suggestedServiceDeprecationCommandString(action.ServiceID, "YYYY-MM-DD")
	action.Recommendation = "SDK configs still use this service. Add a deprecation directive to the workspace config, or set decision=force_remove to accept breaking those SDKs."
	summary.Warnings = append(summary.Warnings, workspacePlanWarning{
		Code:             "sdk_uses_removed_service",
		ServiceID:        action.ServiceID,
		Message:          action.Recommendation,
		SDKs:             action.ImpactedSDKConfigs,
		SuggestedCommand: action.SuggestedCommand,
	})
	summary.Blockers = append(summary.Blockers, workspacePlanBlocker{
		Code:      "service_used_by_sdk",
		ServiceID: action.ServiceID,
		ActionID:  action.ID,
		Message:   "Explicit force_remove decision is required before removing a workspace service used by SDK configs.",
	})
	return action
}

func attachVersionRemovalImpact(action workspacePlanAction, impactedSDKs []string, summary *workspacePlanSummary) workspacePlanAction {
	if len(impactedSDKs) == 0 {
		return action
	}
	action.RequiresDecision = true
	action.ImpactedSDKConfigs = append([]string(nil), impactedSDKs...)
	action.SuggestedCommand = suggestedVersionDeprecationCommandString(action.ServiceID, action.Version, "YYYY-MM-DD")
	action.Recommendation = "SDK configs still use this version. Add a deprecation directive to the workspace config, or set decision=force_remove to accept breaking those SDKs."
	summary.Warnings = append(summary.Warnings, workspacePlanWarning{
		Code:             "sdk_uses_removed_version",
		ServiceID:        action.ServiceID,
		Message:          action.Recommendation,
		SDKs:             action.ImpactedSDKConfigs,
		SuggestedCommand: action.SuggestedCommand,
	})
	summary.Blockers = append(summary.Blockers, workspacePlanBlocker{
		Code:      "version_used_by_sdk",
		ServiceID: action.ServiceID,
		ActionID:  action.ID,
		Message:   "Explicit force_remove decision is required before removing a workspace service version used by SDK configs.",
	})
	return action
}

func suggestedServiceDeprecationCommand(serviceID uuid.UUID, effectiveAt string) string {
	return suggestedServiceDeprecationCommandString(serviceID.String(), effectiveAt)
}

func suggestedServiceDeprecationCommandString(serviceID, effectiveAt string) string {
	return fmt.Sprintf("fused-cli workspace service deprecate %s --at %s", serviceID, effectiveAt)
}

func suggestedVersionDeprecationCommand(serviceID uuid.UUID, version, effectiveAt string) string {
	return suggestedVersionDeprecationCommandString(serviceID.String(), version, effectiveAt)
}

func suggestedVersionDeprecationCommandString(serviceID, version, effectiveAt string) string {
	return fmt.Sprintf("fused-cli workspace service version deprecate %s %s --at %s", serviceID, version, effectiveAt)
}

func unmanagedWorkspaceServices(
	desired workspaceDesiredState,
	current map[uuid.UUID]currentWorkspaceService,
	previousManaged map[uuid.UUID]workspaceManagedService,
) []string {
	var unmanaged []string
	for serviceID := range current {
		if _, managed := previousManaged[serviceID]; !managed {
			if _, desiredNow := desired.Services[serviceID]; !desiredNow {
				unmanaged = append(unmanaged, serviceID.String())
			}
		}
	}
	return unmanaged
}

// appliedWorkspaceWebhook is one webhook registration created or refreshed by
// an apply, surfaced back to the caller (WorkspaceConfigApplyHandler) so the
// CLI can print each registration's URL. ServiceKey is the workspace YAML's
// own map key for the service (e.g. "github") -- already the human-chosen,
// URL-safe identifier for that service in this workspace, so it's reused
// directly as the URL's cosmetic "-{serviceSlug}" suffix rather than fetching
// a separate canonical slug from the Registry.
type appliedWorkspaceWebhook struct {
	ServiceKey string
	Label      string
	Slug       string
}

func applyWorkspaceConfig(
	ctx context.Context,
	s store.Store,
	verifier ServiceVerifier,
	apiKey string,
	accountID uuid.UUID,
	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
	authMats map[string]workspaceAuthMaterial,
	profileMats map[string]workspaceConnectMaterial,
	bucketSecretMats map[string]string,
	masterKey []byte,
) ([]appliedWorkspaceWebhook, error) {
	// Bucket-owned OAuth app registration (fused-cli connect <slug> set) and
	// workspace connection profiles are independent plans (see the plan's
	// "Workspace Plan And Apply"): profile reconciliation never consults
	// bucket material, and bucket material is no longer a workspace apply
	// concern at all -- it's an immediate admin action against its own
	// endpoint (connect_admin_handlers.go), not something this apply plans,
	// validates, or reconciles.
	profilePlan, err := prepareWorkspaceProfilePlan(ctx, s, verifier, apiKey, desired, profileMats)
	if err != nil {
		return nil, err
	}
	authSecrets, err := prepareWorkspaceAuthSecrets(ctx, s, verifier, apiKey, desired, authMats, masterKey)
	if err != nil {
		return nil, err
	}
	// Generic bucket secrets are a third independent plan, same reasoning as
	// the comment above: no service dimension at all, so they can't gate or
	// be gated by service-scoped auth material.
	bucketSecrets, err := prepareWorkspaceBucketSecrets(ctx, s, desired, bucketSecretMats, masterKey)
	if err != nil {
		return nil, err
	}
	// Reconciled once for the entire apply, not once per service -- see
	// reconcileWorkspaceProfilePlan's doc comment for why this is the one
	// database round trip the plan requires even when many services change.
	if err := reconcileWorkspaceProfilePlan(ctx, s, profilePlan); err != nil {
		return nil, err
	}
	if err := upsertWorkspaceBucketSecrets(ctx, s, bucketSecrets); err != nil {
		return nil, err
	}
	applied, err := upsertDesiredWorkspaceServices(ctx, s, verifier, apiKey, accountID, desired, authSecrets)
	if err != nil {
		return nil, err
	}
	if err := removePreviouslyManagedWorkspaceResources(ctx, s, desired, previousManaged); err != nil {
		return nil, err
	}
	return applied, nil
}

func upsertDesiredWorkspaceServices(
	ctx context.Context,
	s store.Store,
	verifier ServiceVerifier,
	apiKey string,
	accountID uuid.UUID,
	desired workspaceDesiredState,
	authSecrets map[string]workspaceAuthApplyPlan,
) ([]appliedWorkspaceWebhook, error) {
	// applied is always empty now -- kept in this function's return type only
	// because appliedWebhookResponses/the apply response wire shape still
	// expects one; webhook registration itself moved entirely to kind:
	// webhook (see the loop body below and plans/plan-webhook-kind.md).
	var applied []appliedWorkspaceWebhook
	for _, svc := range sortedDesiredServices(desired) {
		serviceName, err := verifiedWorkspaceServiceName(ctx, verifier, svc, apiKey)
		if err != nil {
			return nil, err
		}

		firstVersion := svc.Versions[0]
		if err := s.AddWorkspaceServiceVersion(ctx, svc.ServiceID, workspaceServiceSlug(svc.Key), firstVersion, svc.VersionIDs[firstVersion], serviceName, accountID); err != nil {
			return nil, err
		}

		_, svcSpan := otel.Tracer("engine").Start(ctx, "engine.workspace_config.service_upserted")
		svcSpan.SetAttributes(
			attribute.String("service_id", svc.ServiceID.String()),
		)
		svcSpan.End()

		for _, version := range svc.Versions[1:] {
			if err := s.EnableWorkspaceServiceVersion(ctx, svc.ServiceID, version, svc.VersionIDs[version], accountID); err != nil {
				return nil, err
			}
			_, verSpan := otel.Tracer("engine").Start(ctx, "engine.workspace_config.version_enabled")
			verSpan.SetAttributes(
				attribute.String("service_id", svc.ServiceID.String()),
				attribute.String("version", version),
			)
			verSpan.End()
		}

		// runtime_config.webhooks (and the upsertWorkspaceServiceWebhooks call
		// that used to run here) was removed with no backward compatibility --
		// webhook registration is kind: webhook's job now, entirely decoupled
		// from workspace apply (see plans/plan-webhook-kind.md and
		// webhook_config_handlers.go). `applied` stays declared/returned
		// (still part of this function's and the caller's signature) but is
		// never populated by this loop anymore.

		// Connection profile reconciliation is intentionally not here: it is
		// batched once across the whole apply by reconcileWorkspaceProfilePlan,
		// not once per service inside this loop (see applyWorkspaceConfig).
		if err := upsertPreparedWorkspaceAuthSecrets(ctx, s, svc, authSecrets); err != nil {
			return nil, err
		}
	}
	return applied, nil
}

func workspaceServiceSlug(key string) string {
	key = strings.TrimSpace(key)
	if _, err := uuid.Parse(key); err == nil {
		return ""
	}
	// Provider qualification is Registry routing metadata. Engine is scoped to
	// one Registry account, so persisting only the verified bare slug keeps the
	// local lookup stable without importing cross-account identity semantics.
	if strings.HasPrefix(key, "@") {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}
	return key
}

type workspaceAuthApplyPlan struct {
	BucketID  uuid.UUID
	ServiceID uuid.UUID
	AuthType  string
	Secrets   []store.WorkspaceSecret
}

// prepareWorkspaceAuthSecrets validates all static-auth bucket writes before
// workspace membership mutates, avoiding half-applied service config.
func prepareWorkspaceAuthSecrets(
	ctx context.Context,
	s store.Store,
	verifier ServiceVerifier,
	apiKey string,

	desired workspaceDesiredState,
	materials map[string]workspaceAuthMaterial,
	masterKey []byte,
) (map[string]workspaceAuthApplyPlan, error) {
	plans := map[string]workspaceAuthApplyPlan{}
	buckets := workspaceConnectBucketCache{}
	for _, item := range desired.BucketServiceConfigs {
		if item.Auth == nil {
			continue
		}
		plan, err := prepareWorkspaceAuthSecret(ctx, s, verifier, apiKey, desired.Services[item.ServiceID], item, materials, buckets, masterKey)
		if err != nil {
			return nil, err
		}
		plans[workspaceBucketMaterialKey(item.BucketName, item.ServiceKey)] = plan
	}
	return plans, nil
}

// prepareWorkspaceAuthSecret derives provider-specific secret keys from
// registry metadata so users configure "basic" instead of dispatcher key names.
func prepareWorkspaceAuthSecret(
	ctx context.Context,
	s store.Store,
	verifier ServiceVerifier,
	apiKey string,

	svc workspaceDesiredService,
	item workspaceDesiredBucketServiceConfig,
	materials map[string]workspaceAuthMaterial,
	buckets workspaceConnectBucketCache,
	masterKey []byte,
) (workspaceAuthApplyPlan, error) {
	bucket, err := resolveWorkspaceConnectBucket(ctx, s, item.BucketName, buckets)
	if err != nil {
		return workspaceAuthApplyPlan{}, err
	}
	authType := canonicalWorkspaceStaticAuthType(item.Auth.AuthType)
	auth, err := workspaceStaticAuthConfig(ctx, verifier, apiKey, svc, authType)
	if err != nil {
		return workspaceAuthApplyPlan{}, err
	}
	resolved, err := workspaceAuthConfigWithMaterial(svc, item, materials)
	if err != nil {
		return workspaceAuthApplyPlan{}, err
	}
	secrets, err := encryptedWorkspaceAuthSecrets(bucket.ID, svc.ServiceID, auth, &resolved, masterKey)
	if err != nil {
		return workspaceAuthApplyPlan{}, err
	}
	return workspaceAuthApplyPlan{BucketID: bucket.ID, ServiceID: svc.ServiceID, AuthType: authType, Secrets: secrets}, nil
}

// workspaceStaticAuthConfig fetches the selected service version's auth shape;
// config files intentionally name auth families, not provider scheme IDs.
func workspaceStaticAuthConfig(ctx context.Context, verifier ServiceVerifier, apiKey string, svc workspaceDesiredService, authType string) (fusedobject.AuthConfig, error) {
	if verifier == nil {
		return fusedobject.AuthConfig{}, fmt.Errorf("service %s auth metadata resolver is unavailable", svc.ServiceID)
	}
	metadata, err := verifier.FetchServiceMetadata(ctx, svc.ServiceID, svc.Versions[0])
	if err != nil {
		return fusedobject.AuthConfig{}, fmt.Errorf("fetch auth shape for service %s: %w", svc.ServiceID, err)
	}
	return selectWorkspaceStaticAuthConfig(metadata.AuthConfigs, authType)
}

// selectWorkspaceStaticAuthConfig enforces the one-auth-per-type MVP contract
// by choosing the first config matching the requested auth family.
func selectWorkspaceStaticAuthConfig(auths fusedobject.AuthConfigs, authType string) (fusedobject.AuthConfig, error) {
	for _, auth := range auths {
		if canonicalWorkspaceAuthConfigType(auth) == authType {
			return auth, nil
		}
	}
	return fusedobject.AuthConfig{}, fmt.Errorf("auth type %q is not configured for this service", authType)
}

// workspaceAuthConfigWithMaterial replaces shareable $ENV refs with the
// apply-time material sent by the CLI before encryption.
func workspaceAuthConfigWithMaterial(svc workspaceDesiredService, item workspaceDesiredBucketServiceConfig, materials map[string]workspaceAuthMaterial) (WorkspaceAuthConfig, error) {
	resolved := *item.Auth
	material := materials[workspaceBucketMaterialKey(item.BucketName, item.ServiceKey)]
	replaceWorkspaceAuthEnvRefs(&resolved, material)
	if err := validateWorkspaceAuthResolvedMaterial(svc.Key, &resolved); err != nil {
		return WorkspaceAuthConfig{}, err
	}
	return resolved, nil
}

// replaceWorkspaceAuthEnvRefs only resolves whole-field env refs; partial
// interpolation stays unsupported so config intent is auditable.
func replaceWorkspaceAuthEnvRefs(auth *WorkspaceAuthConfig, material workspaceAuthMaterial) {
	if isWorkspaceEnvRef(auth.Username) {
		auth.Username = material.Username
	}
	if isWorkspaceEnvRef(auth.Password) {
		auth.Password = material.Password
	}
	if isWorkspaceEnvRef(auth.Token) {
		auth.Token = material.Token
	}
	if isWorkspaceEnvRef(auth.APIKey) {
		auth.APIKey = material.APIKey
	}
	if isWorkspaceEnvRef(auth.Cert) {
		auth.Cert = material.Cert
	}
	if isWorkspaceEnvRef(auth.Key) {
		auth.Key = material.Key
	}
}

// validateWorkspaceAuthResolvedMaterial fails closed when apply callers omit
// material for a config that declared static auth secrets.
func validateWorkspaceAuthResolvedMaterial(key string, auth *WorkspaceAuthConfig) error {
	authType := canonicalWorkspaceStaticAuthType(auth.AuthType)
	switch authType {
	case "basic":
		return validateWorkspaceAuthResolvedFields(key, authType, auth.Username, auth.Password)
	case "api_key":
		return validateWorkspaceAuthResolvedFields(key, authType, auth.APIKey)
	case "mtls":
		return validateWorkspaceAuthResolvedFields(key, authType, auth.Cert, auth.Key)
	default:
		return validateWorkspaceAuthResolvedFields(key, authType, auth.Token)
	}
}

// validateWorkspaceAuthResolvedFields prevents empty bucket secrets, which are
// harder to debug later as provider 401s.
func validateWorkspaceAuthResolvedFields(key, authType string, values ...string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			// Apply receives resolved local material out-of-band; missing fields
			// indicate a stale/non-CLI caller and must not produce empty secrets.
			return fmt.Errorf("service %q auth %s material is missing", key, authType)
		}
	}
	return nil
}

// encryptedWorkspaceAuthSecrets turns one selected auth family into the exact
// secret rows the dispatcher already knows how to read.
func encryptedWorkspaceAuthSecrets(bucketID, serviceID uuid.UUID, auth fusedobject.AuthConfig, cfg *WorkspaceAuthConfig, masterKey []byte) ([]store.WorkspaceSecret, error) {
	keys, err := workspaceAuthSecretInputs(auth, cfg)
	if err != nil {
		return nil, err
	}
	secrets := make([]store.WorkspaceSecret, 0, len(keys))
	for _, key := range keys {
		secret, err := encryptedWorkspaceSecret(bucketID, serviceID, key.Name, key.Type, key.Value, masterKey)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

type workspaceAuthSecretInput struct {
	Name  string
	Type  string
	Value string
}

// workspaceAuthSecretInputs maps public auth families to dispatcher credential
// keys while keeping bucket storage service-scoped.
func workspaceAuthSecretInputs(auth fusedobject.AuthConfig, cfg *WorkspaceAuthConfig) ([]workspaceAuthSecretInput, error) {
	name := workspaceAuthCredentialName(auth)
	if name == "" {
		return nil, errors.New("selected auth config has no credential name")
	}
	switch canonicalWorkspaceStaticAuthType(cfg.AuthType) {
	case "basic":
		return []workspaceAuthSecretInput{{Name: name + "_username", Type: "basic", Value: cfg.Username}, {Name: name + "_password", Type: "basic", Value: cfg.Password}}, nil
	case "api_key":
		return []workspaceAuthSecretInput{{Name: name, Type: "apiKey", Value: cfg.APIKey}}, nil
	case "mtls":
		// Validate before encryption so config apply cannot save an unusable or
		// expired transport credential pair into the bucket.
		if _, err := mtlsauth.CertificatePair(cfg.Cert, cfg.Key, time.Now().UTC()); err != nil {
			return nil, err
		}
		return []workspaceAuthSecretInput{{Name: name + "_cert", Type: "mtls", Value: cfg.Cert}, {Name: name + "_key", Type: "mtls", Value: cfg.Key}}, nil
	default:
		return []workspaceAuthSecretInput{{Name: name, Type: canonicalWorkspaceStaticAuthType(cfg.AuthType), Value: cfg.Token}}, nil
	}
}

// encryptedWorkspaceSecret wraps each value with the Engine master key so
// plaintext auth material never lands in the workspace secret table.
func encryptedWorkspaceSecret(bucketID, serviceID uuid.UUID, keyName, credentialType, value string, masterKey []byte) (store.WorkspaceSecret, error) {
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return store.WorkspaceSecret{}, fmt.Errorf("wrap auth secret DEK: %w", err)
	}
	encryptedValue, err := store.EncryptWithDEK(dek, value)
	if err != nil {
		return store.WorkspaceSecret{}, fmt.Errorf("encrypt auth secret: %w", err)
	}
	return store.WorkspaceSecret{
		WorkspaceSecretMeta: store.WorkspaceSecretMeta{
			ID:             uuid.New(),
			BucketID:       bucketID,
			ServiceID:      serviceID,
			KeyName:        keyName,
			CredentialType: credentialType,
		},
		EncryptedDEK:   wrappedDEK,
		EncryptedValue: encryptedValue,
	}, nil
}

// prepareWorkspaceBucketSecrets resolves and encrypts every declared
// buckets.<name>.secrets.<key> intent before any workspace membership
// mutates, mirroring prepareWorkspaceAuthSecrets' validate-before-write
// ordering. Unlike auth secrets, these have no per-service loop to ride
// along with -- they're upserted once for the whole apply (see
// upsertWorkspaceBucketSecrets), the same way connection profiles are
// reconciled once rather than per service.
func prepareWorkspaceBucketSecrets(
	ctx context.Context,
	s store.Store,
	desired workspaceDesiredState,
	materials map[string]string,
	masterKey []byte,
) ([]store.WorkspaceSecret, error) {
	if len(desired.BucketSecrets) == 0 {
		return nil, nil
	}
	buckets := workspaceConnectBucketCache{}
	secrets := make([]store.WorkspaceSecret, 0, len(desired.BucketSecrets))
	for _, intent := range desired.BucketSecrets {
		bucket, err := resolveWorkspaceConnectBucket(ctx, s, intent.BucketName, buckets)
		if err != nil {
			return nil, err
		}
		value, ok := materials[workspaceBucketSecretMaterialKey(intent.BucketName, intent.Key)]
		if !ok || strings.TrimSpace(value) == "" {
			// Mirrors validateWorkspaceAuthResolvedMaterial's guard: an
			// unresolved $ENV ref at apply time means the CLI sent a plan
			// without its matching material, not a legitimately empty secret.
			return nil, fmt.Errorf("bucket %q secret %q material is missing", workspaceConnectBucketName(intent.BucketName), intent.Key)
		}
		secret, err := encryptedWorkspaceSecret(bucket.ID, uuid.Nil, secretref.KeyPrefix+intent.Key, "bucket_secret", value, masterKey)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// upsertWorkspaceBucketSecrets performs the already-validated writes and
// records only non-secret identifiers/counts for audit, mirroring
// upsertPreparedWorkspaceAuthSecrets' OTEL shape.
func upsertWorkspaceBucketSecrets(ctx context.Context, s store.Store, secrets []store.WorkspaceSecret) error {
	if len(secrets) == 0 {
		return nil
	}
	if err := s.UpsertSecrets(ctx, secrets); err != nil {
		return fmt.Errorf("upsert bucket secrets: %w", err)
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.bucket_secrets_upserted")
	span.SetAttributes(attribute.Int("secret_count", len(secrets)))
	span.End()
	return nil
}

// upsertPreparedWorkspaceAuthSecrets performs the already-validated writes and
// records only non-secret identifiers/counts for audit.
func upsertPreparedWorkspaceAuthSecrets(ctx context.Context, s store.Store, svc workspaceDesiredService, plans map[string]workspaceAuthApplyPlan) error {
	for _, plan := range plans {
		if plan.ServiceID != svc.ServiceID {
			continue
		}
		if err := s.UpsertSecrets(ctx, plan.Secrets); err != nil {
			return fmt.Errorf("upsert auth secrets for service %s: %w", svc.ServiceID, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.auth_secrets_upserted")
		span.SetAttributes(
			attribute.String("bucket_id", plan.BucketID.String()),
			attribute.String("service_id", svc.ServiceID.String()),
			attribute.String("auth_type", plan.AuthType),
			attribute.Int("secret_count", len(plan.Secrets)),
		)
		span.End()
	}
	return nil
}

// workspaceAuthCredentialName mirrors runtime credential resolution so config
// apply writes the same key names applyAuth reads at execution time.
func workspaceAuthCredentialName(auth fusedobject.AuthConfig) string {
	name := strings.TrimSpace(auth.Name)
	if name != "" {
		return name
	}
	if canonicalWorkspaceAuthConfigType(auth) == "bearer" || canonicalWorkspaceAuthConfigType(auth) == "oauth" || canonicalWorkspaceAuthConfigType(auth) == "oidc" {
		// Bearer-style auth without a scheme name still maps to the dispatcher
		// Authorization slot, matching runtime credential resolution.
		return "Authorization"
	}
	if canonicalWorkspaceAuthConfigType(auth) == "mtls" {
		// mTLS is a paired transport credential, so unnamed imports need a
		// stable prefix for cert/key lookup instead of a header slot.
		return "mtls"
	}
	return ""
}

// canonicalWorkspaceAuthConfigType converts OpenAPI http schemes into the auth
// families users select in workspace config.
func canonicalWorkspaceAuthConfigType(auth fusedobject.AuthConfig) string {
	if strings.EqualFold(auth.Type, "http") {
		return canonicalWorkspaceStaticAuthType(auth.Scheme)
	}
	return canonicalWorkspaceImportedAuthType(auth.Type)
}

// canonicalWorkspaceStaticAuthType accepts only the public workspace config
// vocabulary; imported provider spellings are normalized separately.
func canonicalWorkspaceStaticAuthType(authType string) string {
	normalized := strings.ToLower(strings.TrimSpace(authType))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "api_key", "oauth", "oidc", "basic", "bearer", "mtls":
		return normalized
	default:
		return normalized
	}
}

// canonicalWorkspaceImportedAuthType keeps registry/OpenAPI spellings private;
// workspace config should stay on the stable public auth vocabulary.
func canonicalWorkspaceImportedAuthType(authType string) string {
	normalized := strings.ToLower(strings.TrimSpace(authType))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "oauth2", "oauth2_authorization_code":
		// Registry/OpenAPI names stay private; workspace config users select
		// the public oauth family instead.
		return "oauth"
	case "mutualtls", "mutual_tls":
		return "mtls"
	default:
		return canonicalWorkspaceStaticAuthType(authType)
	}
}

type workspaceProfileReplacement = store.WorkspaceProfileReplacement

type workspaceVersionProfile struct {
	Version   string
	VersionID uuid.UUID
	// AuthType comes from the owning intent (workspaceDesiredConnectionProfile),
	// not from RuntimeConfig.Connect -- a profile's auth family is declared per
	// version/auth tuple, independent of whether bucket Connect material exists.
	AuthType string
	Profile  connectionprofile.Profile
	Resolved *workspaceResolvedConnectionProfile
}

// workspaceProfilePlan is the profile-only counterpart to
// workspaceConnectApplyPlan: it aggregates every workspace connection profile
// replacement/delete across the entire desired state so apply performs one
// ReconcileWorkspaceProfiles transaction per request instead of one per
// service. It intentionally carries no bucket/Connect fields -- see the
// plan's "Workspace Plan And Apply": profile and bucket-material desired
// state are two independent plans that must never gate or imply each other.
type workspaceProfilePlan struct {
	Replacements []workspaceProfileReplacement
	Deletes      []store.WorkspaceProfileRef
}

type workspaceConnectBucketCache map[string]*store.Bucket

// prepareWorkspaceProfilePlan resolves and validates every workspace
// connection profile intent across the whole desired state, independently of
// bucket-owned Connect material (Agreed Product Rules 11-12). It re-verifies
// pinned Registry revisions/hashes, validates every intent against its exact
// pinned contract in one batch, and returns the complete set of replacements
// and deletes for one transactional reconcile (reconcileWorkspaceProfilePlan)
// covering the entire apply.
func prepareWorkspaceProfilePlan(
	ctx context.Context,
	s store.Store,
	resolver any,
	apiKey string,

	desired workspaceDesiredState,
	materials map[string]workspaceConnectMaterial,
) (workspaceProfilePlan, error) {
	if err := verifyResolvedWorkspaceProfiles(ctx, resolver, apiKey, desired); err != nil {
		return workspaceProfilePlan{}, err
	}
	contracts, err := workspaceProfileContracts(ctx, resolver, apiKey, desired)
	if err != nil {
		return workspaceProfilePlan{}, err
	}
	profileStore, err := engineProfileStore(s)
	if err != nil {
		// Test doubles and older rollout stores may omit the profile capability;
		// only desired profile writes require it when there is nothing to reconcile.
		if hasWorkspaceProfileMutations(desired) {
			return workspaceProfilePlan{}, err
		}
		return workspaceProfilePlan{}, nil
	}
	current, err := currentWorkspaceProfilesByRef(ctx, profileStore, desired)
	if err != nil {
		return workspaceProfilePlan{}, err
	}
	var plan workspaceProfilePlan

	// Literal binding injections had only ever been sourced from bucket-owned
	// Connect.Injections (workspace.yaml's connect: block, now removed -- see
	// fused-cli connect <slug> set). prepareWorkspaceServiceProfilePlan still
	// accepts an injections list so a future non-bucket source can populate
	// it without another signature change, but there is none today.
	for _, svc := range sortedDesiredServices(desired) {
		if err := prepareWorkspaceServiceProfilePlan(svc, materials[svc.Key], nil, contracts, current, &plan); err != nil {
			return workspaceProfilePlan{}, err
		}
	}
	return plan, nil
}

// currentWorkspaceProfilesByRef reads every tuple's current effective profile
// in one batched query so per-service planning never issues its own read.
func currentWorkspaceProfilesByRef(ctx context.Context, profileStore store.WorkspaceProfileStore, desired workspaceDesiredState) (map[string]*store.WorkspaceConnectionProfile, error) {
	refs := workspaceProfileRefs(desired)
	currentProfiles, err := profileStore.GetEffectiveWorkspaceProfiles(ctx, refs)
	if err != nil {
		return nil, err
	}
	return indexWorkspaceProfiles(currentProfiles), nil
}

// reconcileWorkspaceProfilePlan writes every workspace profile replacement and
// delete gathered across the whole apply in one transaction. This is the one
// database round trip the plan requires even when many services change --
// see prepareWorkspaceProfilePlan and ReconcileWorkspaceProfiles's set-based
// implementation.
func reconcileWorkspaceProfilePlan(ctx context.Context, s store.Store, plan workspaceProfilePlan) error {
	if len(plan.Replacements) == 0 && len(plan.Deletes) == 0 {
		return nil
	}
	batchStore, ok := s.(store.WorkspaceProfileBatchStore)
	if !ok {
		return errors.New("connection profile batch store is unavailable")
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.connection_profiles_reconciled")
	defer span.End()
	if err := batchStore.ReconcileWorkspaceProfiles(ctx, plan.Replacements, plan.Deletes); err != nil {
		span.SetAttributes(attribute.String("outcome", "error"))
		return err
	}
	// Non-secret aggregate counts only -- no profile JSON, binding values, or
	// resource metadata, per the plan's OTEL requirements.
	span.SetAttributes(
		attribute.Int("replacement_count", len(plan.Replacements)),
		attribute.Int("delete_count", len(plan.Deletes)),
		attribute.Int("binding_count", workspaceReplacementBindingCount(plan.Replacements)),
		attribute.String("outcome", "reconciled"),
	)
	return nil
}

// workspaceProfileContracts performs one batch Registry read and validates all
// inline or resolved profiles against their exact pinned service versions.
func workspaceProfileContracts(ctx context.Context, resolver any, apiKey string, desired workspaceDesiredState) (map[uuid.UUID]connectionprofile.Contract, error) {
	versionIDs := workspaceProfileVersionIDs(desired)
	// A workspace without profiles does not require Registry contract capability.
	if len(versionIDs) == 0 {
		return map[uuid.UUID]connectionprofile.Contract{}, nil
	}
	contractResolver, ok := resolver.(ConnectionProfileContractResolver)
	if !ok {
		return nil, errors.New("connection profile contract validation is unavailable")
	}
	resolved, err := contractResolver.FetchConnectionProfileContracts(ctx, versionIDs, apiKey)
	if err != nil {
		return nil, err
	}
	contracts := make(map[uuid.UUID]connectionprofile.Contract, len(resolved))
	services := make(map[uuid.UUID]uuid.UUID, len(resolved))
	for _, item := range resolved {
		contracts[item.ServiceVersionID] = connectionprofile.Contract{
			AuthTypes: item.AuthTypes, Servers: item.Servers, Operations: item.Operations, Complete: true,
		}
		services[item.ServiceVersionID] = item.ServiceID
	}
	for _, service := range sortedDesiredServices(desired) {
		for _, selected := range workspaceVersionProfiles(service) {
			contract, found := contracts[selected.VersionID]
			// Contract identity must match both version and owning service.
			if !found || services[selected.VersionID] != service.ServiceID {
				return nil, fmt.Errorf("service %q version %s connection profile contract is unavailable", service.Key, selected.Version)
			}
			if err := connectionprofile.Validate(&selected.Profile, contract).Err(); err != nil {
				return nil, fmt.Errorf("service %q version %s connection profile: %w", service.Key, selected.Version, err)
			}
		}
	}
	return contracts, nil
}

// workspaceProfileVersionIDs includes every version that will receive a
// profile, because each version owns an independent validation contract.
func workspaceProfileVersionIDs(desired workspaceDesiredState) []uuid.UUID {
	values := make([]uuid.UUID, 0)
	seen := map[uuid.UUID]struct{}{}
	for _, service := range sortedDesiredServices(desired) {
		for _, selected := range workspaceVersionProfiles(service) {
			// Shared version references still produce one Registry batch entry.
			if _, ok := seen[selected.VersionID]; ok {
				continue
			}
			seen[selected.VersionID] = struct{}{}
			values = append(values, selected.VersionID)
		}
	}
	return values
}

// verifyResolvedWorkspaceProfiles re-fetches public identities at apply time so
// a changed Registry revision cannot silently alter an approved plan.
func verifyResolvedWorkspaceProfiles(ctx context.Context, resolver any, apiKey string, desired workspaceDesiredState) error {
	refs, expected := resolvedWorkspaceProfileRefs(desired)
	// Inline-only workspaces have no public revision to re-fetch.
	if len(refs) == 0 {
		return nil
	}
	profileResolver, ok := resolver.(ConnectionProfileResolver)
	if !ok {
		return errors.New("connection profile revision verification is unavailable")
	}
	profiles, err := profileResolver.FetchEligibleConnectionProfiles(ctx, refs, apiKey)
	if err != nil {
		return err
	}
	actual := map[string]sandbox.ConnectionProfileRevision{}
	for _, profile := range profiles {
		actual[resolvedProfileIdentityKey(profile.ProfileID.String(), profile.ServiceVersionID)] = profile
	}
	for identity, wanted := range expected {
		profile, ok := actual[identity]
		// Identity, revision, and content hash must all match the approved plan.
		if !ok || profile.Revision != wanted.Revision || profile.ProfileHash != wanted.ProfileHash {
			return errors.New("connection profile revision changed since plan; run plan again")
		}
	}
	return nil
}

// resolvedWorkspaceProfileRefs builds one verification batch plus the exact
// revision/hash expectations used to compare its response. It reads
// Resolved directly off each intent in svc.ConnectionProfiles -- not
// RuntimeConfig.Connect -- since profile resolution no longer requires a
// Connect declaration.
func resolvedWorkspaceProfileRefs(desired workspaceDesiredState) ([]sandbox.ConnectionProfileRef, map[string]workspaceResolvedConnectionProfile) {
	refs := make([]sandbox.ConnectionProfileRef, 0)
	expected := map[string]workspaceResolvedConnectionProfile{}
	for _, service := range sortedDesiredServices(desired) {
		for _, intent := range service.ConnectionProfiles {
			if intent.Resolved == nil {
				continue
			}
			refs = append(refs, sandbox.ConnectionProfileRef{ServiceVersionID: intent.VersionID, AuthType: intent.AuthType})
			expected[resolvedProfileIdentityKey(intent.Resolved.ProfileID, intent.VersionID)] = *intent.Resolved
		}
	}
	return refs, expected
}

// resolvedProfileIdentityKey includes the version so a result for one pinned
// contract cannot accidentally satisfy another version's apply-time check.
func resolvedProfileIdentityKey(profileID string, versionID uuid.UUID) string {
	return profileID + "\x00" + versionID.String()
}

// prepareWorkspaceServiceProfilePlan applies preservation and reset rules to
// one service's connection-profile intents. It iterates svc.ConnectionProfiles
// directly (one entry per version/auth tuple) rather than svc.RuntimeConfig.Connect
// -- a service can request profile resolution with no bucket material declared
// at all, and each tuple's explicit/reset intent is independent of the others.
func prepareWorkspaceServiceProfilePlan(svc workspaceDesiredService, material workspaceConnectMaterial, injections []InjectionConfig, contracts map[uuid.UUID]connectionprofile.Contract, current map[string]*store.WorkspaceConnectionProfile, plan *workspaceProfilePlan) error {
	for _, intent := range svc.ConnectionProfiles {
		ref := store.WorkspaceProfileRef{ServiceID: svc.ServiceID, ServiceVersionID: intent.VersionID, AuthType: intent.AuthType}
		currentProfile := current[workspaceProfileRefKey(ref)]
		// Only an explicit reset may remove the workspace's current routing profile.
		if intent.Reset {
			if currentProfile != nil {
				plan.Deletes = append(plan.Deletes, ref)
			}
			continue
		}
		if !intent.hasSource() {
			continue
		}
		// Omission preserves current state; automatic Registry selection remains
		// useful only for a tuple that has no profile yet.
		if currentProfile != nil && !intent.explicit() {
			continue
		}
		selected := workspaceVersionProfile{Version: intent.Version, VersionID: intent.VersionID, AuthType: intent.AuthType, Resolved: intent.Resolved}
		if intent.Resolved != nil {
			selected.Profile = intent.Resolved.Config
		} else {
			selected.Profile = *intent.Profile
		}
		if err := prepareWorkspaceProfile(svc, material, injections, selected, contracts[intent.VersionID], currentProfile, plan); err != nil {
			return err
		}
	}
	return nil
}

// hasWorkspaceProfileMutations distinguishes a missing rollout capability from
// a store required to persist an attachment or perform an explicit detach.
func hasWorkspaceProfileMutations(desired workspaceDesiredState) bool {
	for _, svc := range desired.Services {
		if hasWorkspaceProfile(svc) || workspaceProfileDetachRequested(svc) {
			return true
		}
	}
	return false
}

// workspaceProfileDetachRequested reports whether any of this service's
// connection-profile intents requests an explicit reset. Reset lives on
// workspaceConfigConnectionProfileIntent itself (per-version/auth tuple), not
// on RuntimeConfig.Connect, so detaching one tuple never implies detaching
// another or touching bucket material.
func workspaceProfileDetachRequested(svc workspaceDesiredService) bool {
	for _, intent := range svc.ConnectionProfiles {
		if intent.Reset {
			return true
		}
	}
	return false
}

// workspaceProfileRefs gathers exact profile tuples before the single store
// read used to prepare profile reconciliation. Every declared connection
// profile intent contributes a ref, independent of whether the service also
// declares bucket Connect material.
func workspaceProfileRefs(desired workspaceDesiredState) []store.WorkspaceProfileRef {
	refs := make([]store.WorkspaceProfileRef, 0)
	for _, svc := range sortedDesiredServices(desired) {
		for _, intent := range svc.ConnectionProfiles {
			refs = append(refs, store.WorkspaceProfileRef{
				ServiceID: svc.ServiceID, ServiceVersionID: intent.VersionID, AuthType: intent.AuthType,
			})
		}
	}
	return refs
}

// hasWorkspaceProfile treats inline and Engine-resolved profiles uniformly for
// validation, planning, and persistence decisions.
func hasWorkspaceProfile(svc workspaceDesiredService) bool {
	return len(workspaceVersionProfiles(svc)) > 0
}

// workspaceVersionProfiles projects svc.ConnectionProfiles (one entry per
// version/auth tuple) into the resolved-or-inline shape prepareWorkspaceProfile
// consumes, preferring a verified public Registry snapshot over an inline body
// when both are present on the same intent. A reset or sourceless intent
// contributes nothing here -- it is handled entirely by
// prepareWorkspaceServiceProfilePlan's own Reset branch.
func workspaceVersionProfiles(svc workspaceDesiredService) []workspaceVersionProfile {
	profiles := make([]workspaceVersionProfile, 0, len(svc.ConnectionProfiles))
	for _, intent := range svc.ConnectionProfiles {
		if intent.Reset || !intent.hasSource() {
			continue
		}
		if intent.Resolved != nil {
			profiles = append(profiles, workspaceVersionProfile{
				Version: intent.Version, VersionID: intent.VersionID, AuthType: intent.AuthType,
				Profile: intent.Resolved.Config, Resolved: intent.Resolved,
			})
			continue
		}
		profiles = append(profiles, workspaceVersionProfile{
			Version: intent.Version, VersionID: intent.VersionID, AuthType: intent.AuthType, Profile: *intent.Profile,
		})
	}
	return profiles
}

// indexWorkspaceProfiles makes local revision and reset lookup constant-time
// while retaining the exact composite ownership boundary from the SQL read.
func indexWorkspaceProfiles(profiles []store.WorkspaceConnectionProfile) map[string]*store.WorkspaceConnectionProfile {
	indexed := make(map[string]*store.WorkspaceConnectionProfile, len(profiles))
	for index := range profiles {
		ref := store.WorkspaceProfileRef{
			ServiceID: profiles[index].ServiceID, ServiceVersionID: profiles[index].ServiceVersionID, AuthType: profiles[index].AuthType,
		}
		indexed[workspaceProfileRefKey(ref)] = &profiles[index]
	}
	return indexed
}

// workspaceProfileRefKey includes every ownership dimension so two versions or
// auth families cannot collide in workspace reconciliation.
func workspaceProfileRefKey(ref store.WorkspaceProfileRef) string {
	return ref.ServiceID.String() + "\x00" + ref.ServiceVersionID.String() + "\x00" + ref.AuthType
}

// prepareWorkspaceProfile resolves environment values, validates the complete
// pinned contract, and builds an in-memory atomic replacement. The resulting
// layer depends on where the behavior came from: a verified public Registry
// snapshot (selected.Resolved != nil) is the pinned baseline that activation
// attaches; an inline intent.Profile is workspace-authored and always becomes
// the override, per applyResolvedProfileIdentity below. plan is the
// workspace-wide profile plan, not a per-bucket one -- this function never
// touches bucket Connect material.
func prepareWorkspaceProfile(svc workspaceDesiredService, material workspaceConnectMaterial, injections []InjectionConfig, selected workspaceVersionProfile, contract connectionprofile.Contract, current *store.WorkspaceConnectionProfile, plan *workspaceProfilePlan) error {
	compiledProfile, err := validatedInlineWorkspaceProfile(selected.Profile, material.BindingValues, svc, selected.AuthType, contract)
	if err != nil {
		return err
	}
	declarativeProfile := selected.Profile
	// The profile row is the exportable declaration; compiled binding rows own
	// apply-time values so a later sync never writes resolved $ENV data to YAML.
	declarativeProfile.AuthType = compiledProfile.AuthType
	snapshot, hash, err := profileSnapshot(declarativeProfile)
	if err != nil {
		return err
	}
	// Public snapshots must hash exactly as planned after canonical processing.
	if selected.Resolved != nil && selected.Resolved.ProfileHash != hash {
		return errors.New("connection profile payload does not match planned revision hash")
	}
	profile := store.WorkspaceConnectionProfile{
		ServiceID: svc.ServiceID, ServiceVersionID: selected.VersionID, AuthType: declarativeProfile.AuthType, Layer: "override",
		ProfileRevision: nextLocalProfileRevision(current), ProfileHash: hash,
		Provenance: "workspace", ProfileSnapshot: snapshot,
	}
	applyResolvedProfileIdentity(&profile, selected.Resolved)
	bindings, err := compileProfileBindings(compiledProfile, profile)
	if err != nil {
		return err
	}

	// Injections are dynamic values that must bypass the connection profile compilation.
	for _, inj := range injections {
		mode := inj.Mode
		if mode == "" {
			mode = "replace"
		}
		val := strings.TrimSpace(inj.Value)

		bindings = append(bindings, store.WorkspaceConnectionBinding{
			ServiceID:        svc.ServiceID,
			ServiceVersionID: profile.ServiceVersionID,
			SourceKind:       "literal",
			LiteralValue:     &val,
			TargetLocation:   inj.Location,
			TargetName:       inj.Name,
			Mode:             mode,
		})
	}

	plan.Replacements = append(plan.Replacements, workspaceProfileReplacement{Profile: profile, Bindings: bindings})
	return nil
}

// validatedInlineWorkspaceProfile resolves apply-time environment material and
// then reruns the complete pinned contract before persistence. authType comes
// from the owning intent (one per version/auth tuple), not from
// RuntimeConfig.Connect, since a profile no longer requires bucket material.
func validatedInlineWorkspaceProfile(configured connectionprofile.Profile, values map[string]string, svc workspaceDesiredService, authType string, contract connectionprofile.Contract) (connectionprofile.Profile, error) {
	profile, err := resolvedInlineWorkspaceProfile(configured, values)
	if err != nil {
		return connectionprofile.Profile{}, fmt.Errorf("service %s connection profile: %w", svc.Key, err)
	}
	profile.AuthType = connectionprofile.CanonicalAuthType(profile.AuthType)
	if profile.AuthType != authType {
		return connectionprofile.Profile{}, errors.New("connection profile auth_type must match its auth_type")
	}
	if err := connectionprofile.Validate(&profile, contract).Err(); err != nil {
		return connectionprofile.Profile{}, err
	}
	return profile, nil
}

// applyResolvedProfileIdentity preserves Registry provenance and promotes the
// row to the baseline layer only for verified public snapshots; inline
// workspace profiles retain local identity and the override layer instead.
func applyResolvedProfileIdentity(profile *store.WorkspaceConnectionProfile, resolved *workspaceResolvedConnectionProfile) {
	// Nil identifies an inline workspace profile, which keeps override/workspace provenance.
	if resolved == nil {
		return
	}
	profileID, err := uuid.Parse(resolved.ProfileID)
	// An invalid server-generated identity cannot be persisted as Registry provenance.
	if err != nil {
		return
	}
	profile.Layer = "baseline"
	profile.RegistryProfileID = &profileID
	profile.ProfileRevision = resolved.Revision
	profile.ProfileHash = resolved.ProfileHash
	profile.Provenance = resolved.Provenance
}

// resolvedInlineWorkspaceProfile works on a JSON copy so apply-time environment
// substitution cannot mutate the plan payload used for drift comparison.
func resolvedInlineWorkspaceProfile(profile connectionprofile.Profile, values map[string]string) (connectionprofile.Profile, error) {
	payload, err := json.Marshal(profile)
	if err != nil {
		return connectionprofile.Profile{}, err
	}
	var resolved connectionprofile.Profile
	if err := json.Unmarshal(payload, &resolved); err != nil {
		return connectionprofile.Profile{}, err
	}
	// Only environment expressions are materialized; literals and resource paths
	// remain structural inputs for runtime compilation.
	for index := range resolved.Bindings {
		expression, err := connectionprofile.ParseExpression(resolved.Bindings[index].Value)
		if err != nil {
			return connectionprofile.Profile{}, err
		}
		// Non-environment values already contain their final plan-safe form.
		if expression.Kind != connectionprofile.SourceEnvironment {
			continue
		}
		value, ok := values[expression.EnvName]
		// Missing local material blocks apply rather than storing an unresolved secret reference.
		if !ok {
			return connectionprofile.Profile{}, errors.New("profile binding environment value is missing at apply")
		}
		resolved.Bindings[index].Value = value
	}
	return resolved, nil
}

// isWorkspaceEnvRef lets Engine accept plan-safe `$ENV` placeholders while
// still rejecting literal client secrets before they can enter config state.
func isWorkspaceEnvRef(value string) bool {
	return workspaceEnvRefName(value) != ""
}

// workspaceEnvRefName intentionally recognizes only whole-value env refs; the
// CLI, not Engine, performs the actual local environment lookup at apply time.
func workspaceEnvRefName(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return validWorkspaceEnvName(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")))
	}
	if strings.HasPrefix(value, "$") {
		return validWorkspaceEnvName(strings.TrimPrefix(value, "$"))
	}
	return ""
}

// validWorkspaceEnvName avoids treating arbitrary strings as env placeholders,
// which keeps inline secret validation deterministic.
func validWorkspaceEnvName(name string) string {
	if name == "" {
		return ""
	}
	for i, r := range name {
		if validWorkspaceEnvNameRune(i, r) {
			continue
		}
		return ""
	}
	return name
}

// validWorkspaceEnvNameRune keeps env-name syntax boring on purpose: letters
// and underscore may start, digits may follow, and nothing invokes a shell.
func validWorkspaceEnvNameRune(index int, r rune) bool {
	return r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9'
}

// workspaceBucketMaterialKey aligns CLI and Engine apply payloads without
// exposing bucket credentials inside the plan's shareable desired state.
func workspaceBucketMaterialKey(bucketName, serviceKey string) string {
	return workspaceConnectBucketName(bucketName) + "\x00" + serviceKey
}

// workspaceBucketSecretMaterialKey is workspaceBucketMaterialKey's sibling for
// the generic bucket.<name>.secrets.<key> field -- same "\x00"-joined key
// shape, keyed by secret name instead of service key since these have no
// service dimension.
func workspaceBucketSecretMaterialKey(bucketName, key string) string {
	return workspaceConnectBucketName(bucketName) + "\x00" + key
}

// workspaceReplacementBindingCount reports aggregate, non-secret work in OTEL
// without recording binding values or provider metadata.
func workspaceReplacementBindingCount(replacements []workspaceProfileReplacement) int {
	total := 0
	for _, replacement := range replacements {
		total += len(replacement.Bindings)
	}
	return total
}

// resolveWorkspaceConnectBucket caches exact workspace/name lookups during one
// apply so multiple services sharing a bucket do not create N+1 reads.
func resolveWorkspaceConnectBucket(ctx context.Context, s store.Store, bucketName string, buckets workspaceConnectBucketCache) (*store.Bucket, error) {
	name := workspaceConnectBucketName(bucketName)
	if bucket := buckets[name]; bucket != nil {
		return bucket, nil
	}
	bucket, err := s.GetBucketByName(ctx, name)
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "bucket not found: " + name}
	}
	buckets[name] = bucket
	return bucket, nil
}

func workspaceConnectBucketName(bucketName string) string {
	name := strings.TrimSpace(bucketName)
	if name == "" {
		return "default"
	}
	return name
}

// prepareWorkspaceWebhookRegistration is deliberately persistence-free so
// batch artifact preparation can build every row without database calls in
// its service loop.
func prepareWorkspaceWebhookRegistration(serviceID, serviceVersionID uuid.UUID, label, secretRef string, secretBucketID *uuid.UUID, authShape fusedobject.IncomingWebhookConfig, eventExtractionPath, owningConfigKey string) (store.WorkspaceWebhook, error) {
	slug, err := webhookid.Generate()
	if err != nil {
		return store.WorkspaceWebhook{}, fmt.Errorf("generate webhook slug for %q: %w", label, err)
	}
	return store.WorkspaceWebhook{
		ServiceID:           serviceID,
		ServiceVersionID:    serviceVersionID,
		Label:               label,
		Slug:                slug,
		AuthType:            authShape.AuthType,
		AuthLocation:        authShape.AuthLocation,
		AuthKeyName:         authShape.AuthKeyName,
		SignatureHeader:     authShape.SignatureHeader,
		VerificationHeaders: authShape.VerificationHeaders,
		EventExtractionPath: eventExtractionPath,
		SecretRef:           secretRef,
		SecretBucketID:      secretBucketID,
		OwningConfigKey:     owningConfigKey,
	}, nil
}

func emitWebhookAppliedSpan(ctx context.Context, saved store.WorkspaceWebhook) {
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.webhook_upserted")
	span.SetAttributes(
		attribute.String("service_id", saved.ServiceID.String()),
		attribute.String("label", saved.Label),
		attribute.String("webhook_id", saved.ID.String()),
	)
	span.End()
}

// applyDeprecationActions pushes deprecation directives from the plan to the
// Registry. Two action types are handled:
//
//   - "deprecate_version": marks a specific version deprecated.
//   - "deprecate_service": the service itself is being deprecated — the workspace
//     yaml removed the service entry and added a deprecation block. We surface
//     this at the version level in the Registry by deprecating every version
//     listed in previousManaged. (The service row is not soft-deleted here;
//     that path is archiveRemovedOwnedServices for owned services.)
//
// currentState carries the previously-applied desired_state. We diff the
// incoming deprecations block against it so that re-running apply after a
// partial failure doesn't re-send deprecation calls that already landed — the
// Registry endpoint is idempotent too, but skipping avoids noisy OTEL spans
// and unnecessary latency on large workspaces.
func applyDeprecationActions(
	ctx context.Context,
	deprecator any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
	currentState *store.ConfigState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}

	versionDeprecator, ok := deprecator.(ServiceVersionDeprecator)
	if !ok {
		return errors.New("service version deprecation is unavailable")
	}

	// Build a set of (serviceID, version) pairs that were already deprecated in
	// the last-applied state so we can skip them. Parsing errors are ignored:
	// a bad last state means nothing was previously applied and we proceed.
	alreadyDeprecated := previouslyAppliedDeprecations(currentState)

	for _, action := range actions {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			continue
		}
		switch action.Type {
		case "deprecate_version":
			if action.Version == "" {
				continue
			}
			key := serviceID.String() + "|" + action.Version
			if alreadyDeprecated[key] {
				// This exact (service, version) deprecation was already applied and
				// persisted to fused_config_states.desired_state — skip the Registry
				// call to avoid redundant mutations on replay.
				continue
			}
			if err := versionDeprecator.DeprecateServiceVersion(ctx, serviceID, action.Version, call.apiKey); err != nil {
				return fmt.Errorf("applyDeprecationActions: deprecate version %s@%s: %w", serviceID, action.Version, err)
			}
			_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.version_deprecated")
			span.SetAttributes(
				attribute.String("service_id", serviceID.String()),
				attribute.String("version", action.Version),
				attribute.String("outcome", "deprecated"),
			)
			span.End()
		}
	}
	return nil
}

// previouslyAppliedDeprecations parses the last-applied desired_state and
// returns a set of "serviceID|version" keys for every deprecation directive
// that was already persisted. The set is keyed on the version-level pair; a
// service-level deprecation (Version == "") is intentionally excluded because
// we only track version-granularity deprecations here.
func previouslyAppliedDeprecations(state *store.ConfigState) map[string]bool {
	if state == nil || len(state.DesiredState) == 0 {
		return nil
	}
	// desired_state is the raw resolved config JSON — parse only the top-level
	// "deprecations" array, the same shape as workspaceConfigDocument.
	var doc struct {
		Deprecations []workspaceConfigDeprecation `json:"deprecations,omitempty"`
	}
	if err := json.Unmarshal(state.DesiredState, &doc); err != nil {
		return nil
	}
	out := make(map[string]bool, len(doc.Deprecations))
	for _, dep := range doc.Deprecations {
		serviceID, err := uuid.Parse(dep.ServiceID)
		if err != nil || dep.Version == "" {
			continue
		}
		out[serviceID.String()+"|"+dep.Version] = true
	}
	return out
}

// archiveRemovedOwnedServices soft-deletes services from the Registry when the
// workspace owner removes them from their config. Only owned services can be
// archived — non-owner workspaces can deactivate a service locally but cannot
// delete it from the Registry.
//
// We batch-fetch visibility for all removed service IDs in a single Registry
// call so ownership is determined without N+1 queries, then archive each owned
// service individually (deletions are rare and serialising them is intentional
// so a partial failure is easy to retry without double-deleting).
func archiveRemovedOwnedServices(
	ctx context.Context,
	archiver any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
) error {
	removedIDs := planRemovedServiceIDs(plan)
	if len(removedIDs) == 0 {
		return nil
	}

	visResolver, ok := archiver.(ServiceVisibilityResolver)
	if !ok {
		return errors.New("service visibility resolution is unavailable")
	}
	svcArchiver, ok := archiver.(ServiceArchiver)
	if !ok {
		return errors.New("service archiving is unavailable")
	}

	visibility, err := visResolver.FetchServiceVisibility(ctx, removedIDs, call.apiKey)
	if err != nil {
		return fmt.Errorf("archiveRemovedOwnedServices: fetch visibility: %w", err)
	}

	for _, serviceID := range removedIDs {
		vis, ok := visibility[serviceID]
		if !ok || !vis.IsOwner {
			// Not owned by this workspace — local removal only, no Registry delete.
			continue
		}
		if err := svcArchiver.ArchiveService(ctx, serviceID, call.apiKey); err != nil {
			return fmt.Errorf("archiveRemovedOwnedServices: archive service %s: %w", serviceID, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.service_archived")
		span.SetAttributes(
			attribute.String("service_id", serviceID.String()),
			attribute.String("outcome", "archived"),
		)
		span.End()
	}
	return nil
}

// planRemovedServiceIDs extracts the service IDs from all remove_service
// actions in a plan. These are the services the user explicitly dropped from
// their workspace config yaml.
func planRemovedServiceIDs(plan *store.ConfigPlan) []uuid.UUID {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return nil
	}
	var ids []uuid.UUID
	for _, action := range actions {
		if action.Type != "remove_service" {
			continue
		}
		id, err := uuid.Parse(action.ServiceID)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func applyWorkspaceVisibilityActions(
	ctx context.Context,
	updater any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
) error {
	actions, err := workspaceVisibilityActions(plan.Actions)
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}
	visibilityUpdater, ok := updater.(ServiceVisibilityUpdater)
	if !ok {
		return errors.New("workspace service public visibility updates are unavailable")
	}
	for _, action := range actions {
		if err := applyWorkspaceVisibilityAction(ctx, visibilityUpdater, call, action); err != nil {
			return err
		}
	}
	return nil
}

func workspaceVisibilityActions(raw json.RawMessage) ([]workspacePlanAction, error) {
	var actions []workspacePlanAction
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, err
	}
	out := make([]workspacePlanAction, 0, len(actions))
	for _, action := range actions {
		if action.Type == "set_service_public" || action.Type == "set_service_private" {
			out = append(out, action)
		}
	}
	return out, nil
}

func applyWorkspaceVisibilityAction(
	ctx context.Context,
	updater ServiceVisibilityUpdater,
	call workspaceApplyCall,
	action workspacePlanAction,
) error {
	serviceID, err := uuid.Parse(action.ServiceID)
	if err != nil {
		return err
	}
	isPublic := action.Type == "set_service_public"
	if action.Public != nil {
		isPublic = *action.Public
	}
	if err := updater.UpdateServicePublic(ctx, serviceID, isPublic, call.apiKey); err != nil {
		return err
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.service_visibility_updated")
	span.SetAttributes(
		attribute.String("service_id", serviceID.String()),
		attribute.Bool("public", isPublic),
	)
	span.End()
	return nil
}

// applyWorkspaceVersionVisibilityActions is applyWorkspaceVisibilityActions'
// per-version sibling: it applies every set_service_version_public/private
// action in the approved plan via UpdateServiceVersionPublic.
func applyWorkspaceVersionVisibilityActions(
	ctx context.Context,
	updater any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
) error {
	actions, err := workspaceVersionVisibilityActions(plan.Actions)
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}
	visibilityUpdater, ok := updater.(ServiceVisibilityUpdater)
	if !ok {
		return errors.New("workspace service version public visibility updates are unavailable")
	}
	for _, action := range actions {
		if err := applyWorkspaceVersionVisibilityAction(ctx, visibilityUpdater, call, action); err != nil {
			return err
		}
	}
	return nil
}

func workspaceVersionVisibilityActions(raw json.RawMessage) ([]workspacePlanAction, error) {
	var actions []workspacePlanAction
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, err
	}
	out := make([]workspacePlanAction, 0, len(actions))
	for _, action := range actions {
		if action.Type == "set_service_version_public" || action.Type == "set_service_version_private" {
			out = append(out, action)
		}
	}
	return out, nil
}

func applyWorkspaceVersionVisibilityAction(
	ctx context.Context,
	updater ServiceVisibilityUpdater,
	call workspaceApplyCall,
	action workspacePlanAction,
) error {
	serviceID, err := uuid.Parse(action.ServiceID)
	if err != nil {
		return err
	}
	isPublic := action.Type == "set_service_version_public"
	if action.Public != nil {
		isPublic = *action.Public
	}
	if err := updater.UpdateServiceVersionPublic(ctx, serviceID, action.Version, isPublic, call.apiKey); err != nil {
		return err
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.service_version_visibility_updated")
	span.SetAttributes(
		attribute.String("service_id", serviceID.String()),
		attribute.String("version", action.Version),
		attribute.Bool("public", isPublic),
	)
	span.End()
	return nil
}

// applyWorkspaceVersionExecutionPolicyPublishActions is
// applyWorkspaceExecutionPolicyPublishActions' per-version sibling: it
// executes all "publish_service_version_execution_policy" actions through the
// Registry publish API. As with the service-level equivalent, the desired
// state is consulted by service ID + version to retrieve the full policy
// payload.
func applyWorkspaceVersionExecutionPolicyPublishActions(
	ctx context.Context,
	updater any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}
	var publish []workspacePlanAction
	for _, a := range actions {
		if a.Type == "publish_service_version_execution_policy" {
			publish = append(publish, a)
		}
	}
	if len(publish) == 0 {
		return nil
	}
	svu, ok := updater.(ServiceVisibilityUpdater)
	if !ok {
		return errors.New("workspace version execution policy publishing is unavailable")
	}
	for _, action := range publish {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			return err
		}
		svc, ok := desired.Services[serviceID]
		if !ok {
			continue
		}
		var policy *workspaceExecutionPolicy
		for _, vp := range svc.VersionPolicies {
			if vp.Version == action.Version {
				policy = vp.ExecutionPolicy
				break
			}
		}
		if policy == nil {
			continue
		}
		if err := svu.PublishServiceVersionExecutionPolicy(ctx, serviceID, action.Version, policy, call.apiKey); err != nil {
			return fmt.Errorf("publish execution policy for service %s version %s: %w", serviceID, action.Version, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.version_execution_policy_published")
		span.SetAttributes(attribute.String("service_id", serviceID.String()), attribute.String("version", action.Version))
		span.End()
	}
	return nil
}

// applyWorkspaceExecutionPolicyPublishActions executes all
// "publish_service_execution_policy" actions in the approved plan through the
// Registry publish API. The desired state is consulted by service ID to
// retrieve the full policy payload — only the action type is stored in the
// plan, not the payload (to keep plan actions small and avoid stale payload on
// re-apply).
func applyWorkspaceExecutionPolicyPublishActions(
	ctx context.Context,
	updater any,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}
	var publish []workspacePlanAction
	for _, a := range actions {
		if a.Type == "publish_service_execution_policy" {
			publish = append(publish, a)
		}
	}
	if len(publish) == 0 {
		return nil
	}
	svu, ok := updater.(ServiceVisibilityUpdater)
	if !ok {
		return errors.New("workspace execution policy publishing is unavailable")
	}
	for _, action := range publish {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			return err
		}
		svc, ok := desired.Services[serviceID]
		if !ok || svc.ExecutionPolicy == nil {
			continue
		}
		if err := svu.PublishServiceExecutionPolicy(ctx, serviceID, svc.ExecutionPolicy, call.apiKey); err != nil {
			return fmt.Errorf("publish execution policy for service %s: %w", serviceID, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.execution_policy_published")
		span.SetAttributes(attribute.String("service_id", serviceID.String()))
		span.End()
	}
	return nil
}

// executionPolicyOverrideStore is the narrow surface
// applyWorkspaceExecutionPolicyLocalActions needs, reached via type assertion
// against store.Store -- the same rollout idiom
// secret_resolver.go's loadBucketBindings uses for WorkspaceProfileStore, and
// LocalObjectCache.applyExecutionPolicyOverride uses for the read side.
type executionPolicyOverrideStore interface {
	UpsertWorkspaceExecutionPolicyOverride(ctx context.Context, override store.WorkspaceExecutionPolicyOverride) (*store.WorkspaceExecutionPolicyOverride, error)
	ResetWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID uuid.UUID, serviceVersionID *uuid.UUID) error
	// GetEffectiveWorkspaceExecutionPolicyOverride is read here (not just
	// written) by upsertWorkspaceServiceWebhooks -- webhook ingress was the
	// one execution_policy consumer the local-override read path
	// (LocalObjectCache.applyExecutionPolicyOverride) never reached, since it
	// resolves its auth shape from the Registry once at apply time instead of
	// through LocalObjectCache.
	GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error)
}

// applyWorkspaceExecutionPolicyLocalActions executes all
// "set_local_execution_policy"/"reset_local_execution_policy" actions in the
// approved plan against Engine-owned workspace policy storage -- never the
// Registry. As with the publish actions, only the action identity is stored in
// the plan; the payload is re-derived from desired at apply time.
func applyWorkspaceExecutionPolicyLocalActions(
	ctx context.Context,
	s store.Store,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}
	var local []workspacePlanAction
	for _, a := range actions {
		if a.Type == "set_local_execution_policy" || a.Type == "reset_local_execution_policy" {
			local = append(local, a)
		}
	}
	if len(local) == 0 {
		return nil
	}
	overrideStore, ok := s.(executionPolicyOverrideStore)
	if !ok {
		return errors.New("workspace local execution policy is unavailable")
	}
	for _, action := range local {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			return err
		}
		if action.Type == "reset_local_execution_policy" {
			if err := overrideStore.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, nil); err != nil {
				return fmt.Errorf("reset local execution policy for service %s: %w", serviceID, err)
			}
			continue
		}
		svc, ok := desired.Services[serviceID]
		if !ok || svc.ExecutionPolicy == nil {
			continue
		}
		override := workspaceExecutionPolicyOverride(serviceID, nil, svc.ExecutionPolicy)
		if _, err := overrideStore.UpsertWorkspaceExecutionPolicyOverride(ctx, override); err != nil {
			return fmt.Errorf("set local execution policy for service %s: %w", serviceID, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.local_execution_policy_set")
		span.SetAttributes(attribute.String("service_id", serviceID.String()))
		span.End()
	}
	return nil
}

// applyWorkspaceVersionExecutionPolicyLocalActions is
// applyWorkspaceExecutionPolicyLocalActions' per-version sibling.
func applyWorkspaceVersionExecutionPolicyLocalActions(
	ctx context.Context,
	s store.Store,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}
	var local []workspacePlanAction
	for _, a := range actions {
		if a.Type == "set_local_service_version_execution_policy" || a.Type == "reset_local_service_version_execution_policy" {
			local = append(local, a)
		}
	}
	if len(local) == 0 {
		return nil
	}
	overrideStore, ok := s.(executionPolicyOverrideStore)
	if !ok {
		return errors.New("workspace local execution policy is unavailable")
	}
	for _, action := range local {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			return err
		}
		versionID, err := uuid.Parse(action.ServiceVersionID)
		if err != nil {
			return err
		}
		if action.Type == "reset_local_service_version_execution_policy" {
			if err := overrideStore.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, &versionID); err != nil {
				return fmt.Errorf("reset local execution policy for service %s version %s: %w", serviceID, action.Version, err)
			}
			continue
		}
		svc, ok := desired.Services[serviceID]
		if !ok {
			continue
		}
		var policy *workspaceExecutionPolicy
		for _, vp := range svc.VersionPolicies {
			if vp.Version == action.Version {
				policy = vp.ExecutionPolicy
				break
			}
		}
		if policy == nil {
			continue
		}
		override := workspaceExecutionPolicyOverride(serviceID, &versionID, policy)
		if _, err := overrideStore.UpsertWorkspaceExecutionPolicyOverride(ctx, override); err != nil {
			return fmt.Errorf("set local execution policy for service %s version %s: %w", serviceID, action.Version, err)
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.local_version_execution_policy_set")
		span.SetAttributes(attribute.String("service_id", serviceID.String()), attribute.String("version", action.Version))
		span.End()
	}
	return nil
}

// workspaceExecutionPolicyOverride converts the CLI-facing execution policy
// wire shape into the store's override row. Field names are 1:1 with
// fusedobject's wire types (see fusedobject.RateLimitConfig etc.) -- this is
// a type conversion, not a semantic mapping, mirroring how dispatch_map.go's
// mapPagination/mapRateLimit/mapRetryConfig convert the same shapes in the
// opposite direction for SDK dispatch.
func workspaceExecutionPolicyOverride(serviceID uuid.UUID, serviceVersionID *uuid.UUID, ep *workspaceExecutionPolicy) store.WorkspaceExecutionPolicyOverride {
	override := store.WorkspaceExecutionPolicyOverride{
		ServiceID:           serviceID,
		ServiceVersionID:    serviceVersionID,
		EventExtractionPath: ep.EventExtractionPath,
		BaseURL:             ep.BaseURL,
	}
	if ep.RateLimit != nil {
		override.RateLimit = &fusedobject.RateLimitConfig{
			Strategy:          ep.RateLimit.Strategy,
			RequestsPerSecond: ep.RateLimit.RequestsPerSecond,
			RequestsPerMinute: ep.RateLimit.RequestsPerMinute,
		}
	}
	// Retry and RetryConfig are two accepted input spellings for the same
	// field (see workspaceExecutionPolicy's json tags); Retry wins when both
	// are set so the value that publishes is also the value that takes local
	// effect.
	retry := ep.RetryConfig
	if ep.Retry != nil {
		retry = ep.Retry
	}
	if retry != nil {
		override.RetryConfig = &fusedobject.RetryConfig{
			Strategy:   retry.Strategy,
			MaxRetries: retry.MaxRetries,
			BackoffMs:  retry.BackoffMs,
		}
	}
	if ep.Pagination != nil {
		override.Pagination = &fusedobject.PaginationConfig{
			Type:         ep.Pagination.Type,
			RequestParam: ep.Pagination.RequestParam,
			ResponsePath: ep.Pagination.ResponsePath,
		}
	}
	if ep.IncomingWebhookConfig != nil {
		override.IncomingWebhookConfig = &fusedobject.IncomingWebhookConfig{
			AuthType:            ep.IncomingWebhookConfig.AuthType,
			AuthLocation:        ep.IncomingWebhookConfig.AuthLocation,
			AuthKeyName:         ep.IncomingWebhookConfig.AuthKeyName,
			SignatureHeader:     ep.IncomingWebhookConfig.SignatureHeader,
			VerificationHeaders: ep.IncomingWebhookConfig.VerificationHeaders,
		}
	}
	return override
}

// applyWorkspaceConnectionProfilePublishActions executes all
// "publish_connection_profile" actions in the approved plan by calling
// PublishConnectionProfile (Registry's setConnectionProfile mutation) for
// each one. Like applyWorkspaceExecutionPolicyPublishActions, only the action
// identity is stored in the plan -- the desired state is consulted by
// service/version/auth_type to retrieve the full profile body.
//
// public=true requires an explicit inline profile body (item.Profile): a
// profile_id-only reference has nothing new to publish, so that case is a
// hard error here rather than a silent skip.
//
// The profile's display name is derived automatically from the service name
// (mirroring the CLI's direct `fused service connection-profile set` command,
// which also uses the service's display name), since workspace.yaml has no
// separate name field for a connection profile stream.
func applyWorkspaceConnectionProfilePublishActions(
	ctx context.Context,
	updater any,
	s store.Store,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
) error {
	var actions []workspacePlanAction
	if len(plan.Actions) == 0 {
		return nil
	}
	if err := json.Unmarshal(plan.Actions, &actions); err != nil {
		return err
	}
	var publish []workspacePlanAction
	for _, a := range actions {
		if a.Type == "publish_connection_profile" {
			publish = append(publish, a)
		}
	}
	if len(publish) == 0 {
		return nil
	}
	publisher, ok := updater.(ConnectionProfilePublisher)
	if !ok {
		return errors.New("workspace connection profile publishing is unavailable")
	}
	// Best-effort: MarkWorkspaceProfilePublished is local bookkeeping for
	// `fused sync` round-tripping, not required for the Registry publish
	// itself to succeed, so its absence must not block apply.
	profileStore, _ := s.(store.WorkspaceProfileStore)
	for _, action := range publish {
		serviceID, err := uuid.Parse(action.ServiceID)
		if err != nil {
			return err
		}
		svc, ok := desired.Services[serviceID]
		if !ok {
			continue
		}
		intent, ok := findDesiredConnectionProfileIntent(svc, action.Version, action.AuthType)
		if !ok {
			continue
		}
		if intent.Profile == nil {
			return fmt.Errorf("publish connection profile for service %s version %s auth_type %s: public requires an explicit inline profile, not profile_id", serviceID, action.Version, action.AuthType)
		}
		versionID := intent.VersionID
		if versionID == uuid.Nil {
			versionID = svc.VersionIDs[action.Version]
		}
		if versionID == uuid.Nil {
			return fmt.Errorf("publish connection profile for service %s version %s: unresolved service_version_id", serviceID, action.Version)
		}
		if _, err := publisher.PublishConnectionProfile(ctx, serviceID, versionID, svc.ServiceName, *intent.Profile, call.apiKey); err != nil {
			return fmt.Errorf("publish connection profile for service %s version %s auth_type %s: %w", serviceID, action.Version, action.AuthType, err)
		}
		if profileStore != nil {
			if err := profileStore.MarkWorkspaceProfilePublished(ctx, serviceID, versionID, action.AuthType); err != nil {
				slog.ErrorContext(ctx, "WorkspaceConfigApplyHandler: mark connection profile published failed",
					slog.Any("error", err), slog.String("service_id", serviceID.String()))
			}
		}
		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.connection_profile_published")
		span.SetAttributes(
			attribute.String("service_id", serviceID.String()),
			attribute.String("version", action.Version),
			attribute.String("auth_type", action.AuthType),
		)
		span.End()
	}
	return nil
}

// findDesiredConnectionProfileIntent looks up the exact (version, auth_type)
// tuple's intent so the apply step can retrieve the full profile body that the
// stored plan action deliberately omits (see desiredConnectionProfilePublishActions).
func findDesiredConnectionProfileIntent(svc workspaceDesiredService, version, authType string) (workspaceDesiredConnectionProfile, bool) {
	for _, intent := range svc.ConnectionProfiles {
		if intent.Version == version && intent.AuthType == authType {
			return intent, true
		}
	}
	return workspaceDesiredConnectionProfile{}, false
}

func resolveWorkspaceDesiredVersionIDs(
	ctx context.Context,
	verifier ServiceVerifier,
	apiKey string,
	desired workspaceDesiredState,
) (map[uuid.UUID]map[string]uuid.UUID, error) {
	if verifier == nil {
		return nil, errors.New("service version resolver is required")
	}
	refs := workspaceVersionRefs(desired)
	revisions, err := verifier.FetchServiceVersionRevisions(ctx, refs, apiKey)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace service versions: %w", err)
	}
	ids := workspaceVersionIDMap(revisions)
	for _, ref := range refs {
		if ids[ref.ServiceID][ref.Version] == uuid.Nil {
			return nil, fmt.Errorf("service %s version %s has no exact service_version_id", ref.ServiceID, ref.Version)
		}
	}
	return ids, nil
}

func workspaceVersionRefs(desired workspaceDesiredState) []sandbox.ServiceVersionRef {
	refs := make([]sandbox.ServiceVersionRef, 0)
	for _, svc := range sortedDesiredServices(desired) {
		for _, version := range svc.Versions {
			refs = append(refs, sandbox.ServiceVersionRef{ServiceID: svc.ServiceID, Version: version})
		}
	}
	return refs
}

func workspaceVersionIDMap(revisions []sandbox.ServiceVersionRevision) map[uuid.UUID]map[string]uuid.UUID {
	ids := map[uuid.UUID]map[string]uuid.UUID{}
	for _, revision := range revisions {
		if ids[revision.ServiceID] == nil {
			ids[revision.ServiceID] = map[string]uuid.UUID{}
		}
		ids[revision.ServiceID][revision.Version] = revision.ServiceVersionID
	}
	return ids
}

func removePreviouslyManagedWorkspaceResources(
	ctx context.Context,
	s store.Store,

	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
) error {
	for serviceID, managed := range previousManaged {
		desiredSvc, keepService := desired.Services[serviceID]
		if !keepService {
			if err := removeManagedWorkspaceService(ctx, s, desired, serviceID); err != nil {
				return err
			}
			continue
		}
		if err := removeManagedWorkspaceVersions(ctx, s, desired, desiredSvc, managed); err != nil {
			return err
		}
	}
	return nil
}

func removeManagedWorkspaceService(
	ctx context.Context,
	s store.Store,

	desired workspaceDesiredState,
	serviceID uuid.UUID,
) error {
	if _, deprecated := serviceDeprecationDirective(desired, serviceID); deprecated {
		return nil
	}
	if err := s.RemoveWorkspaceService(ctx, serviceID); err != nil && !errors.Is(err, store.ErrWorkspaceServiceNotFound) {
		return err
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.service_removed")
	span.SetAttributes(
		attribute.String("service_id", serviceID.String()),
	)
	span.End()

	return nil
}

func removeManagedWorkspaceVersions(
	ctx context.Context,
	s store.Store,

	desired workspaceDesiredState,
	desiredSvc workspaceDesiredService,
	managed workspaceManagedService,
) error {
	for _, version := range managed.Versions {
		if shouldKeepManagedVersion(desired, desiredSvc, version) {
			continue
		}
		if err := s.DisableWorkspaceServiceVersion(ctx, desiredSvc.ServiceID, version); err != nil && !errors.Is(err, store.ErrWorkspaceServiceVersionNotFound) {
			return err
		}

		_, span := otel.Tracer("engine").Start(ctx, "engine.workspace_config.version_removed")
		span.SetAttributes(
			attribute.String("service_id", desiredSvc.ServiceID.String()),
			attribute.String("version", version),
		)
		span.End()
	}
	return nil
}

func shouldKeepManagedVersion(desired workspaceDesiredState, desiredSvc workspaceDesiredService, version string) bool {
	if containsString(desiredSvc.Versions, version) {
		return true
	}
	_, deprecated := versionDeprecationDirective(desired, desiredSvc.ServiceID, version)
	return deprecated
}

func validateWorkspaceRemovalDecisions(
	plan *store.ConfigPlan,
	desired workspaceDesiredState,
	previousManaged map[uuid.UUID]workspaceManagedService,
) error {
	actions, err := parseWorkspacePlanActions(plan.Actions)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid plan actions"}
	}
	blockedRemovals := blockedServiceRemovalActions(plan.Blockers)
	for serviceID, managed := range previousManaged {
		if desiredSvc, keep := desired.Services[serviceID]; !keep {
			actionID := workspaceActionID("remove_service", serviceID)
			if blockedRemovals[actionID] {
				action := actions[actionID]
				if action.Decision != "force_remove" {
					return workspaceConfigHTTPError{status: http.StatusConflict, message: "force_remove_required"}
				}
			}
		} else {
			for _, version := range managed.Versions {
				if !containsString(desiredSvc.Versions, version) {
					actionID := workspaceActionID("disable_service_version", serviceID, version)
					if blockedRemovals[actionID] {
						action := actions[actionID]
						if action.Decision != "force_remove" {
							return workspaceConfigHTTPError{status: http.StatusConflict, message: "force_remove_required"}
						}
					}
				}
			}
		}
	}
	return nil
}

func createWorkspaceRemovalNotifications(
	ctx context.Context,
	configStore store.ConfigRepository,
	call workspaceApplyCall,
	plan *store.ConfigPlan,
) error {
	actions, err := workspaceNotificationActions(plan.Actions)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid plan actions"}
	}
	for _, action := range actions {
		if err := createWorkspaceRemovalNotificationsForAction(ctx, configStore, call, plan.ID, action); err != nil {
			return err
		}
	}
	return nil
}

func workspaceNotificationActions(raw json.RawMessage) ([]workspacePlanAction, error) {
	actionMap, err := parseWorkspacePlanActions(raw)
	if err != nil {
		return nil, err
	}
	var actions []workspacePlanAction
	for _, action := range actionMap {
		if (action.Type == "remove_service" || action.Type == "disable_service_version") && action.Decision == "force_remove" && len(action.ImpactedSDKConfigs) > 0 {
			actions = append(actions, action)
		}
	}
	return actions, nil
}

func createWorkspaceRemovalNotificationsForAction(
	ctx context.Context,
	configStore store.ConfigRepository,
	call workspaceApplyCall,
	planID uuid.UUID,
	action workspacePlanAction,
) error {
	serviceID, err := uuid.Parse(action.ServiceID)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "invalid removal action service_id"}
	}
	// Deduplicate: create exactly ONE notification per removal action, regardless of how many SDK configs were impacted.
	if len(action.ImpactedSDKConfigs) > 0 {
		if _, err := configStore.CreateWorkspaceNotification(ctx, workspaceRemovalNotificationParams(call, planID, action, serviceID)); err != nil {
			return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to create workspace notification"}
		}
	}
	return nil
}

func workspaceRemovalNotificationParams(
	call workspaceApplyCall,
	planID uuid.UUID,
	action workspacePlanAction,
	serviceID uuid.UUID,
) store.CreateWorkspaceNotificationParams {
	metadata, _ := json.Marshal(map[string]string{
		"plan_id":   planID.String(),
		"action_id": action.ID,
	})
	notifType := store.WorkspaceNotificationTypeServiceRemoved
	msg := fmt.Sprintf("Workspace service %s was force removed while %d SDK configs still reference it.", serviceID, len(action.ImpactedSDKConfigs))
	if action.Type == "disable_service_version" {
		notifType = store.WorkspaceNotificationTypeVersionRemoved
		msg = fmt.Sprintf("Workspace service version %s@%s was force removed while %d SDK configs still reference it.", serviceID, action.Version, len(action.ImpactedSDKConfigs))
	}
	return store.CreateWorkspaceNotificationParams{
		Type:      notifType,
		Severity:  store.WorkspaceNotificationSeverityBreaking,
		ServiceID: &serviceID,
		Version:   action.Version,
		ConfigKey: strings.Join(action.ImpactedSDKConfigs, ", "),
		Message:   msg,
		Metadata:  metadata,
		CreatedBy: call.accountID,
	}
}

func blockedServiceRemovalActions(raw json.RawMessage) map[string]bool {
	var blockers []workspacePlanBlocker
	if len(raw) == 0 || json.Unmarshal(raw, &blockers) != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, blocker := range blockers {
		if blocker.Code == "service_used_by_sdk" || blocker.Code == "version_used_by_sdk" {
			out[blocker.ActionID] = true
		}
	}
	return out
}

func parseWorkspacePlanActions(raw json.RawMessage) (map[string]workspacePlanAction, error) {
	var actions []workspacePlanAction
	if len(raw) == 0 {
		return map[string]workspacePlanAction{}, nil
	}
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, err
	}
	out := map[string]workspacePlanAction{}
	for _, action := range actions {
		out[action.ID] = action
	}
	return out, nil
}

func verifiedWorkspaceServiceName(ctx context.Context, verifier ServiceVerifier, svc workspaceDesiredService, apiKey string) (string, error) {
	if verifier == nil {
		return svc.ServiceName, nil
	}
	name, _, _, _, err := verifier.VerifyServiceExists(ctx, svc.ServiceID, apiKey)
	if err != nil {
		if errors.Is(err, sandbox.ErrServiceNotFound) {
			return "", fmt.Errorf("service %s not found: %w", svc.ServiceID, err)
		}
		return "", fmt.Errorf("verify service %s: %w", svc.ServiceID, err)
	}
	if name != "" {
		return name, nil
	}
	return svc.ServiceName, nil
}

func managedResourcesAfterApply(desired workspaceDesiredState, previousManaged map[uuid.UUID]workspaceManagedService) workspaceManagedResources {
	managed := workspaceManagedResources{}
	retained := retainedDeprecatedManagedServices(desired, previousManaged)
	for _, svc := range sortedDesiredServices(desired) {
		managed.Services = append(managed.Services, workspaceManagedService{
			ServiceID: svc.ServiceID.String(),
			Versions:  managedVersionsAfterApply(desired, svc, previousManaged[svc.ServiceID]),
		})
	}
	managed.Services = append(managed.Services, retained...)
	sort.Slice(managed.Services, func(i, j int) bool {
		return managed.Services[i].ServiceID < managed.Services[j].ServiceID
	})
	return managed
}

func managedVersionsAfterApply(
	desired workspaceDesiredState,
	svc workspaceDesiredService,
	previous workspaceManagedService,
) []string {
	versions := append([]string(nil), svc.Versions...)
	for _, version := range previous.Versions {
		if containsString(versions, version) {
			continue
		}
		if _, deprecated := versionDeprecationDirective(desired, svc.ServiceID, version); deprecated {
			versions = append(versions, version)
		}
	}
	return versions
}

func retainedDeprecatedManagedServices(desired workspaceDesiredState, previousManaged map[uuid.UUID]workspaceManagedService) []workspaceManagedService {
	var retained []workspaceManagedService
	for serviceID, managed := range previousManaged {
		if _, keep := desired.Services[serviceID]; keep {
			continue
		}
		if _, deprecated := serviceDeprecationDirective(desired, serviceID); deprecated {
			retained = append(retained, managed)
		}
	}
	return retained
}

func validateWorkspacePlanForApply(plan *store.ConfigPlan, sourceHash string) error {
	if plan.Status == store.ConfigPlanStatusSuperseded {
		return errors.New("plan_superseded")
	}
	if plan.Status != store.ConfigPlanStatusPending {
		return errors.New("plan_stale")
	}
	if plan.ConfigType != store.ConfigTypeWorkspace {
		return errors.New("plan_type_mismatch")
	}
	if sourceHash != "" && sourceHash != plan.SourceHash {
		return errors.New("source_hash_mismatch")
	}
	return nil
}

// resolveWorkspaceActor returns the identity resolved once by the control
// middleware. It deliberately has no request or store fallback.
func resolveWorkspaceActor(ctx context.Context) (uuid.UUID, error) {
	return controlActorAccount(ctx)
}

func planFetchHTTPError(err error) error {
	if errors.Is(err, store.ErrConfigPlanNotFound) {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "plan not found"}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to fetch plan"}
}

func writeWorkspaceConfigError(w http.ResponseWriter, err error) {
	var httpErr workspaceConfigHTTPError
	if errors.As(err, &httpErr) {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, httpErr.message), httpErr.status)
		return
	}
	http.Error(w, `{"error":"workspace config error"}`, http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func workspaceConfigKey(raw string) string {
	if key := strings.TrimSpace(raw); key != "" {
		return key
	}
	return defaultWorkspaceConfigKey
}

func currentGeneration(state *store.ConfigState) int {
	if state == nil {
		return 0
	}
	return state.Generation
}

func uniqueTrimmed(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sortedDesiredServices(desired workspaceDesiredState) []workspaceDesiredService {
	out := make([]workspaceDesiredService, 0, len(desired.Services))
	for _, svc := range desired.Services {
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ServiceID.String() < out[j].ServiceID.String()
	})
	return out
}

func sortWorkspacePlanSummary(summary *workspacePlanSummary) {
	sort.Slice(summary.Actions, func(i, j int) bool {
		left, right := summary.Actions[i], summary.Actions[j]
		if left.ServiceID != right.ServiceID {
			return left.ServiceID < right.ServiceID
		}
		if left.Type != right.Type {
			return left.Type < right.Type
		}
		return left.Version < right.Version
	})
	sort.Strings(summary.UnmanagedServices)
}

func workspaceActionID(parts ...any) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case uuid.UUID:
			out = append(out, value.String())
		case string:
			out = append(out, value)
		}
	}
	return strings.Join(out, ":")
}

func uuidFromAny(value any) (uuid.UUID, bool) {
	raw, ok := value.(string)
	if !ok {
		return uuid.Nil, false
	}
	serviceID, err := uuid.Parse(raw)
	return serviceID, err == nil
}

type workspaceNotificationInboxItem struct {
	ID                  string               `json:"id"`
	Source              string               `json:"source"`
	Type                string               `json:"type"`
	Severity            string               `json:"severity"`
	Status              string               `json:"status"`
	ServiceID           string               `json:"service_id,omitempty"`
	Version             string               `json:"version,omitempty"`
	ConfigKey           string               `json:"config_key,omitempty"`
	Message             string               `json:"message"`
	IntegrationObjectID string               `json:"integration_object_id,omitempty"`
	WebhookObjectID     string               `json:"webhook_object_id,omitempty"`
	DetectedAt          string               `json:"detected_at,omitempty"`
	Diff                []models.DriftChange `json:"diff,omitempty"`
}

type workspaceNotificationInboxResponse struct {
	Items    []workspaceNotificationInboxItem `json:"items"`
	Warnings []string                         `json:"warnings"`
	// TotalCount and PendingCount are always computed, paginated or not, so
	// the UI has one consistent response shape either way -- without a
	// windowed fetch the client could count len(Items) itself, but that
	// stops being possible once a page is only a slice of the whole set.
	TotalCount   int `json:"total_count"`
	PendingCount int `json:"pending_count"`
}

// workspaceNotificationInbox is the UI's own read path (the only caller is
// workspaceNotificationsGraphQLField) -- it is deliberately NOT shared with
// CLI plan/apply's notification collection (collectWorkspacePlanNotifications,
// collectSDKPlanNotifications, etc., which each call ListWorkspaceNotifications
// with WorkspaceNotificationStatusPending directly). The two surfaces want
// different statuses on purpose: plan/apply should only ever nag about a row
// once, so it stops appearing the moment a human acknowledges OR dismisses it
// (see plans/plan-service-changelog.md's Phase 4 "cross-surface consequence"
// note). The UI's bell panel/banners implement the two-tier read/dismiss
// model instead: an acknowledged row must stay visible, just de-emphasized --
// only a dismissed row disappears. Fetching every status and filtering out
// dismissed here (rather than asking Postgres for status="pending") is what
// keeps that promise; asking for pending-only, like plan/apply does, would
// silently drop acknowledged rows from the UI the moment they're marked read.
//
// page/limit are optional pagination for the full notifications page (see
// plans/plan-service-changelog.md's Phase 4 pagination follow-up): limit<=0
// means "unbounded," preserving the exact behavior every existing caller
// (bell panel, contextual banners) already depends on, including the
// live-drift-snapshot enrichment below. A paginated call (limit>0) returns
// only Engine-local notifications for that page -- live drift snapshots are
// a separate, typically-small, always-fresh mechanism that pagination
// doesn't apply to, so they're only ever included on the unbounded path.
//
// unreadOnly narrows a paginated call to pending rows only, backing the
// notifications page's default "unread only" view -- ignored on the
// unbounded path, which always shows pending+acknowledged (the bell
// panel/banners have no read/unread toggle, just the two-tier
// acknowledge/dismiss model).
func workspaceNotificationInbox(ctx context.Context, configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient, apiKey string, page, limit int, unreadOnly, readOnly bool) (workspaceNotificationInboxResponse, error) {
	var notifications []store.WorkspaceNotification
	var err error
	paginated := limit > 0
	if paginated {
		if page < 1 {
			page = 1
		}
		notifications, err = configStore.ListUnresolvedWorkspaceNotificationsPage(ctx, limit, (page-1)*limit, unreadOnly, readOnly)
	} else {
		notifications, err = configStore.ListWorkspaceNotifications(ctx, "")
		notifications = unresolvedWorkspaceNotifications(notifications)
	}
	if err != nil {
		return workspaceNotificationInboxResponse{}, fmt.Errorf("list workspace notifications: %w", err)
	}

	inbox := workspaceNotificationInboxResponse{Items: workspaceNotificationInboxItems(notifications)}
	inbox.TotalCount, inbox.PendingCount = workspaceNotificationCounts(ctx, configStore, len(notifications))
	if paginated {
		if unreadOnly {
			inbox.TotalCount = inbox.PendingCount
		} else if readOnly {
			inbox.TotalCount = inbox.TotalCount - inbox.PendingCount
		}
	}

	if paginated {
		return inbox, nil
	}

	services, err := s.ListWorkspaceServices(ctx, nil)
	if err != nil {
		inbox.Warnings = append(inbox.Warnings, "failed_to_list_workspace_services")
		return inbox, nil
	}
	snapshots, err := registryClient.FetchDriftSnapshotsForServices(ctx, workspaceServiceIDs(services), apiKey)
	if err != nil {
		// Drift snapshots live in the Registry, so inbox reads should still
		// return Engine-local notifications when that enrichment is unavailable.
		slog.WarnContext(ctx, "failed to fetch drift snapshots for workspace", slog.Any("error", err))
		inbox.Warnings = append(inbox.Warnings, "registry_notifications_unavailable")
		return inbox, nil
	}
	for _, snap := range snapshots {
		inbox.Items = append(inbox.Items, registryDriftInboxItem(snap))
	}
	return inbox, nil
}

// workspaceNotificationCounts resolves total_count/pending_count via the
// dedicated COUNT queries, degrading to the already-fetched page's own
// length (never zero on a real error) if a count query fails -- a wrong
// page-count on the UI is a much smaller problem than a hard GraphQL error
// blowing up the whole inbox read over a non-critical count.
func workspaceNotificationCounts(ctx context.Context, configStore store.ConfigRepository, fallback int) (total, pending int) {
	total, err := configStore.CountUnresolvedWorkspaceNotifications(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to count unresolved workspace notifications", slog.Any("error", err))
		total = fallback
	}
	pending, err = configStore.CountPendingWorkspaceNotifications(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to count pending workspace notifications", slog.Any("error", err))
		pending = 0
	}
	return total, pending
}

// unresolvedWorkspaceNotifications keeps pending and acknowledged rows,
// dropping only dismissed ones -- dismissed is the two-tier read/dismiss
// model's one terminal, hide-for-good state (see workspaceNotificationInbox's
// doc comment above for why this can't just be a status="pending" query).
func unresolvedWorkspaceNotifications(notifications []store.WorkspaceNotification) []store.WorkspaceNotification {
	out := make([]store.WorkspaceNotification, 0, len(notifications))
	for _, notification := range notifications {
		if notification.Status != store.WorkspaceNotificationStatusDismissed {
			out = append(out, notification)
		}
	}
	return out
}

func workspaceNotificationInboxItems(notifications []store.WorkspaceNotification) []workspaceNotificationInboxItem {
	items := make([]workspaceNotificationInboxItem, 0, len(notifications))
	for _, notification := range notifications {
		items = append(items, workspaceNotificationInboxItem{
			ID:        "engine:" + notification.ID.String(),
			Source:    "engine",
			Type:      string(notification.Type),
			Severity:  string(notification.Severity),
			Status:    string(notification.Status),
			ServiceID: notificationServiceID(notification.ServiceID),
			Version:   notification.Version,
			ConfigKey: notification.ConfigKey,
			Message:   notification.Message,
		})
	}
	return items
}

func notificationServiceID(serviceID *uuid.UUID) string {
	if serviceID == nil {
		return ""
	}
	return serviceID.String()
}
