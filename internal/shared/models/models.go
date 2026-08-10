package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"time"

	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/sashabaranov/go-openai"
)

// ─── Account & API Key ────────────────────────────────────────────────────────

type Account struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Provider is the account's public, URL-safe identifier (e.g.
	// "acme-inc") -- the top-level namespace segment in a service's public
	// URL (/integrations/<provider>/<slug>). Nil until CreateAccount
	// auto-generates one from Name (mirrors Service.Slug's own pattern).
	Provider            *string   `json:"provider,omitempty"`
	Email               string    `json:"email,omitempty"`
	CreditBalance       float64   `json:"credit_balance"`
	LastBillingSequence uint64    `json:"last_billing_sequence"`
	AutoTopupEnabled    bool      `json:"auto_topup_enabled"`
	AutoTopupThreshold  float64   `json:"auto_topup_threshold"`
	AutoTopupBundleID   string    `json:"auto_topup_bundle_id"`
	IsSuspended         bool      `json:"is_suspended"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const (
	RuntimeUsageReportingAggregate = "aggregate"

	EngineUsageMetricExecutionTotal   = "engine.execution.total"
	EngineUsageMetricExecutionSuccess = "engine.execution.success"
	EngineUsageMetricExecutionFailed  = "engine.execution.failed"
)

// RuntimeEntitlement is the Registry-issued commercial contract shape the
// Engine persists locally. New capability fields are forward-compatible:
// missing JSON fields deserialize to Go zero values, which Normalized()
// promotes to safe commercial-tier defaults.
type RuntimeEntitlement struct {
	EntitlementRevision          string    `json:"entitlement_revision,omitempty"`
	Plan                         string    `json:"plan"`
	HeartbeatRequired            bool      `json:"heartbeat_required"`
	UsageReporting               string    `json:"usage_reporting"`
	PublicServiceInsightsEnabled bool      `json:"public_service_insights_enabled"`
	HeartbeatIntervalSeconds     int       `json:"heartbeat_interval_seconds"`
	HeartbeatStaleAfterSeconds   int       `json:"heartbeat_stale_after_seconds"`
	RefreshedAt                  time.Time `json:"refreshed_at,omitempty"`

	// ─── Capability limits (nil = missing/unlimited, 0 = not allowed, positive = hard ceiling) ───
	MaxBuckets            *int `json:"max_buckets,omitempty"`
	MaxSDKFamilies        *int `json:"max_sdk_families,omitempty"`
	MaxMCPFamilies        *int `json:"max_mcp_families,omitempty"`
	MaxServices           *int `json:"max_services,omitempty"`
	MaxSandboxConcurrency *int `json:"max_sandbox_concurrency,omitempty"`

	// ─── Feature gates ───
	DriftMonitoringEnabled  bool `json:"drift_monitoring_enabled"`
	WebhookIngestionEnabled bool `json:"webhook_ingestion_enabled"`
	SSOEnabled              bool `json:"sso_enabled"`

	// ─── Data governance (nil = missing, 0 = explicitly disallowed) ───
	ExecutionRetentionDays *int `json:"execution_retention_days,omitempty"`
}

// DefaultRuntimeEntitlement keeps Engines compatible with older Registries
// that only know account/workspace handshake fields. Missing contract fields
// must fail toward the commercial baseline, not toward "no heartbeat".
// IntPtr returns a pointer to v. Used for pointer-typed entitlement limit fields.
func IntPtr(v int) *int { return &v }

// Capability defaults are "commercial = unlimited" so new fields never block
// an Engine that was working before they existed.
func DefaultRuntimeEntitlement() RuntimeEntitlement {
	return RuntimeEntitlement{
		Plan:                         "commercial",
		HeartbeatRequired:            true,
		UsageReporting:               RuntimeUsageReportingAggregate,
		PublicServiceInsightsEnabled: false,
		HeartbeatIntervalSeconds:     60,
		HeartbeatStaleAfterSeconds:   300,
		MaxBuckets:                   IntPtr(-1),
		MaxSDKFamilies:               IntPtr(-1),
		MaxMCPFamilies:               IntPtr(-1),
		MaxServices:                  IntPtr(-1),
		MaxSandboxConcurrency:        IntPtr(-1),
		DriftMonitoringEnabled:       true,
		WebhookIngestionEnabled:      false,
		SSOEnabled:                   false,
		ExecutionRetentionDays:       IntPtr(30),
	}
}

func (e RuntimeEntitlement) Normalized() RuntimeEntitlement {
	defaults := DefaultRuntimeEntitlement()
	// Drift monitoring is part of every plan. Normalizing it to true also
	// unlocks Engines that persisted the former Dev-plan false value.
	e.DriftMonitoringEnabled = true
	e = e.withRuntimeDefaults(defaults)
	return e.withMissingLimitDefaults(defaults)
}

func (e RuntimeEntitlement) withRuntimeDefaults(defaults RuntimeEntitlement) RuntimeEntitlement {
	if e.Plan == "" {
		e.Plan = defaults.Plan
	}
	if e.UsageReporting == "" {
		e.UsageReporting = defaults.UsageReporting
	}
	if e.HeartbeatIntervalSeconds <= 0 {
		e.HeartbeatIntervalSeconds = defaults.HeartbeatIntervalSeconds
	}
	if e.HeartbeatStaleAfterSeconds <= 0 {
		e.HeartbeatStaleAfterSeconds = defaults.HeartbeatStaleAfterSeconds
	}
	if e.RefreshedAt.IsZero() {
		e.RefreshedAt = time.Now().UTC()
	}
	return e
}

func (e RuntimeEntitlement) withMissingLimitDefaults(defaults RuntimeEntitlement) RuntimeEntitlement {
	// nil means the field was missing (older Registry or JSON zero value).
	// Default to unlimited (-1) so missing fields never accidentally block.
	e.MaxBuckets = entitlementLimitOrDefault(e.MaxBuckets, defaults.MaxBuckets)
	e.MaxSDKFamilies = entitlementLimitOrDefault(e.MaxSDKFamilies, defaults.MaxSDKFamilies)
	e.MaxMCPFamilies = entitlementLimitOrDefault(e.MaxMCPFamilies, defaults.MaxMCPFamilies)
	e.MaxServices = entitlementLimitOrDefault(e.MaxServices, defaults.MaxServices)
	e.MaxSandboxConcurrency = entitlementLimitOrDefault(e.MaxSandboxConcurrency, defaults.MaxSandboxConcurrency)
	e.ExecutionRetentionDays = entitlementLimitOrDefault(e.ExecutionRetentionDays, defaults.ExecutionRetentionDays)
	return e
}

func entitlementLimitOrDefault(value, fallback *int) *int {
	// An explicit zero blocks the capability, so only a missing pointer may
	// inherit the compatibility default.
	if value != nil {
		return value
	}
	return fallback
}

type EngineUsageIncrement struct {
	Metric        string
	BucketStart   time.Time
	BucketSeconds int
	Count         int64
}

type EngineUsageReport struct {
	ReportID      uuid.UUID `json:"report_id"`
	Metric        string    `json:"metric"`
	BucketStart   time.Time `json:"bucket_start"`
	BucketSeconds int       `json:"bucket_seconds"`
	Count         int64     `json:"count"`
}

const PublicServiceInsightSchemaVersion = 1

var PublicServiceInsightLatencyBounds = [...]int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

type PublicServiceInsightReport struct {
	ReportID             uuid.UUID `json:"report_id"`
	ServiceID            uuid.UUID `json:"service_id"`
	ServiceVersionID     uuid.UUID `json:"service_version_id"`
	RegistryObjectKind   string    `json:"registry_object_kind"`
	RegistryObjectID     uuid.UUID `json:"registry_object_id"`
	Direction            string    `json:"direction"`
	Transport            string    `json:"transport"`
	Outcome              string    `json:"outcome"`
	ProviderStatusClass  string    `json:"provider_status_class"`
	BucketStart          time.Time `json:"bucket_start"`
	BucketSeconds        int       `json:"bucket_seconds"`
	CallCount            int64     `json:"call_count"`
	TotalLatencyMsSum    int64     `json:"total_latency_ms_sum"`
	ProviderLatencyMsSum int64     `json:"provider_latency_ms_sum"`
	LatencyHistogram     []int64   `json:"latency_histogram"`
	RetryAttemptsSum     int64     `json:"retry_attempts_sum"`
}

type PublicServiceInsightReportResult struct {
	ReportID uuid.UUID `json:"report_id"`
	Accepted bool      `json:"accepted"`
	Reason   string    `json:"reason,omitempty"`
}

type PublicServiceInsightEligibility struct {
	ServiceID  uuid.UUID `json:"service_id"`
	Reportable bool      `json:"reportable"`
}

type PublicServiceInsightsQuery struct {
	ServiceID          uuid.UUID  `json:"service_id"`
	StartDate          time.Time  `json:"start_date"`
	EndDate            time.Time  `json:"end_date"`
	Granularity        string     `json:"granularity"`
	ServiceVersionID   *uuid.UUID `json:"service_version_id,omitempty"`
	RegistryObjectKind string     `json:"registry_object_kind,omitempty"`
	RegistryObjectID   *uuid.UUID `json:"registry_object_id,omitempty"`
	Transport          string     `json:"transport,omitempty"`
}

type PublicServiceInsightPoint struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	TotalCalls   int64   `json:"total_calls"`
	FailedCalls  int64   `json:"failed_calls"`
	P50LatencyMs float64 `json:"p50_latency_ms"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
}

type PublicServiceInsights struct {
	Source             string                      `json:"source"`
	GeneratedAt        time.Time                   `json:"generated_at"`
	DataThrough        *time.Time                  `json:"data_through,omitempty"`
	PartialData        bool                        `json:"partial_data"`
	TotalCalls         int64                       `json:"total_calls"`
	SuccessfulCalls    int64                       `json:"successful_calls"`
	FailedCalls        int64                       `json:"failed_calls"`
	P50LatencyMs       float64                     `json:"p50_latency_ms"`
	P95LatencyMs       float64                     `json:"p95_latency_ms"`
	TimeSeries         []PublicServiceInsightPoint `json:"time_series"`
	TopOperations      []PublicServiceInsightPoint `json:"top_operations"`
	VersionBreakdown   []PublicServiceInsightPoint `json:"version_breakdown"`
	TransportBreakdown []PublicServiceInsightPoint `json:"transport_breakdown"`
}

// ─── Service ──────────────────────────────────────────────────────────────────

type Server struct {
	URL         string                   `json:"url"`
	Description string                   `json:"description,omitempty"`
	Environment string                   `json:"environment,omitempty"`
	IsDefault   bool                     `json:"is_default,omitempty"`
	Variables   []serverrouting.Variable `json:"variables,omitempty"`
}

type Servers []Server

// ServiceProviderIdentity is the organisation that owns a Registry service.
// It is projected from the owning account and never persisted on services.
type ServiceProviderIdentity struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

type Service struct {
	ID               uuid.UUID `json:"id"`
	ServiceVersionID uuid.UUID `json:"service_version_id,omitempty"`
	Name             string    `json:"name"` // e.g., "Stripe"
	Description      string    `json:"description"`
	BaseURL          string    `json:"base_url"`
	RequestedVersion string    `json:"-" db:"-"`
	// ResolvedVersion/ResolvedVersionChecked cache the provider version whose
	// config fields (base_url, auth_configs, rate_limit, retry_config,
	// incoming_webhook_config, default_headers) should be exposed for this
	// request. Set once per request and reused across sibling field resolvers
	// so they share a single version lookup. Never persisted.
	ResolvedVersion        *ServiceVersion  `json:"-" db:"-"`
	ResolvedVersionChecked bool             `json:"-" db:"-"`
	Servers                Servers          `json:"servers,omitempty" db:"servers"`
	SourceID               uuid.UUID        `json:"source_id,omitempty"`
	AuthConfigs            AuthConfigs      `json:"auth_configs" db:"auth_config"`
	RateLimit              *RateLimitConfig `json:"rate_limit,omitempty"`
	RetryConfig            *RetryConfig     `json:"retry_config,omitempty"`
	TimeoutMs              *int             `json:"timeout_ms,omitempty"`
	// Pagination is the service-level execution_policy fallback used when an
	// endpoint has no spec-derived pagination of its own (see
	// plans/plan-service-config-restructure.md item 1). One value per service,
	// not per-endpoint -- deliberately simplified since per-endpoint pagination
	// still wins whenever the spec declared it.
	Pagination *PaginationConfig `json:"pagination,omitempty"`
	// BaseURLOverride is the owner-editable execution_policy override of the
	// spec-derived BaseURL above. nil means no override is published; consumers
	// of the "base_url" field should prefer this value over BaseURL when set.
	BaseURLOverride       *string                `json:"base_url_override,omitempty"`
	DefaultHeaders        DefaultHeaders         `json:"default_headers,omitempty"`
	EventExtractionPath   *string                `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *IncomingWebhookConfig `json:"incoming_webhook_config,omitempty"`
	RawWSDL               string                 `json:"raw_wsdl,omitempty"`
	Slug                  *string                `json:"slug,omitempty"`
	Category              *string                `json:"category,omitempty"`
	IsPublic              bool                   `json:"is_public"`
	WatchForDrift         bool                   `json:"watch_for_drift"`
	ImportWarnings        ImportWarnings         `json:"import_warnings,omitempty"`
	AccountID             uuid.UUID              `json:"account_id,omitempty"`
	// ProviderIdentity is attached in one batched account lookup for GraphQL
	// service results. Provider identity is shown for owned and foreign services
	// alike; ownership remains the separate IsOwner capability signal.
	ProviderIdentity *ServiceProviderIdentity `json:"-" db:"-"`
	Embedding        *pgvector.Vector         `json:"-"`
	IsOwner          bool                     `json:"is_owner"`
	DeletedAt        *time.Time               `json:"deleted_at,omitempty" db:"deleted_at"`
	CreatedAt        time.Time                `json:"created_at"`
	UpdatedAt        time.Time                `json:"updated_at"`
	Resources        []Resource               `json:"-"`
	Integrations     []IntegrationObject      `json:"-"`
	Webhooks         []WebhookObject          `json:"-"`
}

type ImportWarning struct {
	ID             string   `json:"id"`
	EndpointID     string   `json:"endpoint_id,omitempty"`
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	OperationID    string   `json:"operation_id,omitempty"`
	Reasons        []string `json:"reasons"`
	Recommendation string   `json:"recommendation,omitempty"`
	Source         string   `json:"source,omitempty"`
	CreatedAt      string   `json:"created_at,omitempty"`
}

type ImportWarnings []ImportWarning

// ServiceWithSources is used for aggregation queries
type ServiceWithSources struct {
	Service `bson:",inline"`
	Sources []Source `json:"sources"`
}

// ─── Resource ────────────────────────────────────────────────────────────────

type Resource struct {
	ID               uuid.UUID `json:"id"`
	AccountID        uuid.UUID `json:"account_id,omitempty"`
	ServiceID        uuid.UUID `json:"service_id"`
	Name             string    `json:"name"` // e.g., "payments"
	Description      string    `json:"description"`
	RequestedVersion string    `json:"-" db:"-"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ─── Component ───────────────────────────────────────────────────────────────

type Component struct {
	ID        uuid.UUID `json:"id"`
	ServiceID uuid.UUID `json:"service_id"`
	Name      string    `json:"name"`
	Schema    Schema    `json:"schema"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ─── Webhook Object ──────────────────────────────────────────────────────────

type WebhookObject struct {
	ID          uuid.UUID `json:"id"`
	ServiceID   uuid.UUID `json:"service_id"`
	Name        string    `json:"name"`
	Version     *string   `json:"version,omitempty"`
	Method      string    `json:"method"`
	Description string    `json:"description"`
	RequestBody *Schema   `json:"request_body,omitempty"`
	// ChannelName and Action identify the AsyncAPI channel and operation a
	// webhook came from -- the closest analog to an endpoint's method+path,
	// used to match a webhook across reimports. Always empty for
	// OpenAPI-sourced webhooks, since OpenAPI's `webhooks:` section has no
	// channel/action concept: matching code must treat an empty ChannelName
	// as ineligible for channel-based matching, not as a real, matchable
	// value shared by every OpenAPI webhook.
	ChannelName string    `json:"channel_name,omitempty"`
	Action      string    `json:"action,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type WebhookEvent struct {
	ID                 uuid.UUID  `json:"event_id"`
	AccountID          uuid.UUID  `json:"account_id"`
	ServiceID          uuid.UUID  `json:"service_id"`
	MsgID              string     `json:"msg_id"`
	EventType          string     `json:"event_type"`
	ErrorReason        string     `json:"error_reason"`
	SDKRecordID        *uuid.UUID `json:"sdk_record_id,omitempty"`
	VerificationStatus string     `json:"verification_status"`
	DeliveryStatus     string     `json:"delivery_status"`
	Environment        string     `json:"environment"`
	LatencyMs          int        `json:"latency_ms"`
	RetryCount         int        `json:"retry_count"`
	CreditsConsumed    float64    `json:"credits_consumed"`
	PayloadSize        int        `json:"payload_size"`
	CreatedAt          time.Time  `json:"timestamp"`
}

type WebhookAnalytics struct {
	TotalIngested  int64 `json:"total_ingested"`
	TotalDelivered int64 `json:"total_delivered"`
	TotalRejected  int64 `json:"total_rejected"`
	TotalFailed    int64 `json:"total_failed"`
}

// ─── Service Version ──────────────────────────────────────────────────────────

// PublishConfigPayload is the configuration used to publish a service_versions
// row. It is intentionally a strict subset of ServiceVersion: the repository
// owns ID/name/public state/timestamps, and the Engine strips secret fields
// before this payload is built.
type PublishConfigPayload struct {
	BaseURL               *string                `json:"base_url,omitempty"`
	AuthConfigs           AuthConfigs            `json:"auth_configs,omitempty"`
	RateLimit             *RateLimitConfig       `json:"rate_limit,omitempty"`
	RetryConfig           *RetryConfig           `json:"retry_config,omitempty"`
	Pagination            *PaginationConfig      `json:"pagination,omitempty"`
	EventExtractionPath   *string                `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *IncomingWebhookConfig `json:"incoming_webhook_config,omitempty"`
	DefaultHeaders        DefaultHeaders         `json:"default_headers,omitempty"`
	// Servers is the full multi-environment base URL list (e.g. separately
	// named "Production"/"Staging" entries) ServersForm.tsx lets an owner
	// build. BaseURL remains the resolved primary URL used by SDK generation.
	// Without this field, saves silently discard every server after the first.
	Servers Servers `json:"servers,omitempty"`
	// SourceSpecVersion mirrors the provider-declared version from the source.
	SourceSpecVersion *string               `json:"source_spec_version,omitempty"`
	ConnectConfig     *ServiceConnectConfig `json:"connect_config,omitempty"`
}

type ServiceVersion struct {
	ID          uuid.UUID         `json:"id"`
	ServiceID   uuid.UUID         `json:"service_id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	HeaderName  string            `json:"header_name,omitempty"`
	HeaderValue string            `json:"header_value,omitempty"`
	BaseURL     *string           `json:"base_url,omitempty"`
	AuthConfigs AuthConfigs       `json:"auth_configs,omitempty"`
	RateLimit   *RateLimitConfig  `json:"rate_limit,omitempty"`
	RetryConfig *RetryConfig      `json:"retry_config,omitempty"`
	Pagination  *PaginationConfig `json:"pagination,omitempty"`
	// BaseURLOverride mirrors Service.BaseURLOverride, scoped to this version.
	BaseURLOverride       *string                `json:"base_url_override,omitempty"`
	EventExtractionPath   *string                `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *IncomingWebhookConfig `json:"incoming_webhook_config,omitempty"`
	ConnectConfig         *ServiceConnectConfig  `json:"connect_config,omitempty"`
	DefaultHeaders        DefaultHeaders         `json:"default_headers,omitempty"`
	// Servers mirrors PublishConfigPayload.Servers -- see that field's
	// comment for why this exists alongside BaseURL instead of replacing it.
	Servers  Servers `json:"servers,omitempty"`
	IsPublic bool    `json:"is_public"`
	// Status is the published lifecycle: "public" or "deprecated".
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// SourceSpecVersion is the value a spec import's own declaration
	// (OpenAPI/AsyncAPI info.version, Postman's optional info.version)
	// claimed for this row -- deliberately separate from Name, which stays
	// server-generated/date-based (see openapi.EnsureServiceVersion's doc
	// comment for why the two are never conflated). A later CLI re-import
	// compares its spec's declared version against this field, not Name, to
	// decide whether it's describing the version already live or a new one.
	// nil for any version never populated by a spec import.
	SourceSpecVersion *string `json:"source_spec_version,omitempty"`
}

// ServiceConnectConfig declares how a connected user maps to provider-side
// tenant resources. It is versioned with auth because discovery must follow
// the same provider contract used for token exchange and request dispatch.
type ServiceConnectConfig = connectionprofile.Profile
type ResourceDiscoveryConfig = connectionprofile.ResourceDiscoveryConfig
type ResourceInputConfig = connectionprofile.ResourceInputConfig
type ResourceInputField = connectionprofile.ResourceInputField
type ConnectionBinding = connectionprofile.Binding

// Published lifecycle values for ServiceVersion.Status.
const (
	VersionStatusPublic     = "public"
	VersionStatusDeprecated = "deprecated"
)

// ImportPlan is the server-side record backing a non-interactive CLI
// `import plan` -> `import apply` call. Mirrors the shape of Engine's
// ConfigPlan (compute now, persist, read back once at apply, mark applied)
// but scoped to a Registry service import rather than workspace/SDK config
// -- Registry has no workspace concept to key a plan against, so this lives
// in the Registry's own store instead of reusing Engine's ConfigRepository.
// SourceContent carries the raw spec bytes forward so apply can replay the
// exact same parse without re-fetching a URL that might have changed since
// plan ran; SourceHash is apply's guard against the underlying spec content
// changing between the two calls (the same role the existing
// sdk-config/workspace-config plan's source_hash already plays).
type ImportPlan struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
	// AccountID scopes an apply lookup to the account that created the
	// plan -- a plan_id is never valid for any other account.
	AccountID uuid.UUID `json:"account_id"`
	// ServiceID is nil for a plan that would create a brand-new service --
	// there's nothing to point at until apply actually creates it.
	ServiceID     *uuid.UUID `json:"service_id,omitempty"`
	Slug          string     `json:"slug,omitempty"`
	Name          string     `json:"name"`
	IsNewService  bool       `json:"is_new_service"`
	SourceContent string     `json:"-"`
	SourceURL     string     `json:"source_url,omitempty"`
	SourceHash    string     `json:"source_hash"`
	TargetType    string     `json:"target_type,omitempty"`
	Category      string     `json:"category,omitempty"`
	IsPublic      bool       `json:"is_public"`
	// TargetVersion is the exact provider-declared version reviewed by the
	// user. Keeping it on the plan prevents apply from inventing or
	// re-resolving an identity after approval.
	TargetVersion string          `json:"target_version"`
	Action        ImportAction    `json:"action"`
	ContractDiff  json.RawMessage `json:"contract_diff,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// SpecificationImport is the complete, already-parsed contract handed to
// the repository's atomic apply boundary. Parsing and embeddings stay out of
// persistence; every database mutation stays inside one transaction.
type SpecificationImport struct {
	PlanID        uuid.UUID
	AccountID     uuid.UUID
	ServiceID     uuid.UUID
	Name          string
	Slug          string
	TargetVersion string
	Action        ImportAction
	SourceURL     string
	SourceContent string
	SourceHash    string
	TargetType    string
	Category      string
	IsPublic      bool
	ContractDiff  json.RawMessage
	Version       ServiceVersion
	Integrations  []IntegrationObject
	Webhooks      []WebhookObject
	Components    []Component
}

type ServiceVersionRevision struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	Revision         int       `json:"revision"`
	SourceHash       string    `json:"source_hash"`
	IsPublic         bool      `json:"is_public"`
}

type ServiceVersionAuthConfigs struct {
	ServiceID        uuid.UUID   `json:"service_id"`
	Version          string      `json:"version"`
	ServiceVersionID uuid.UUID   `json:"service_version_id"`
	AuthConfigs      AuthConfigs `json:"auth_configs"`
}

// ServiceVersionConnectionContract is the bounded Registry response used to
// validate local workspace profiles against exact pinned service versions.
type ServiceVersionConnectionContract struct {
	ServiceID        uuid.UUID                     `json:"service_id"`
	ServiceVersionID uuid.UUID                     `json:"service_version_id"`
	AuthTypes        []string                      `json:"auth_types"`
	Servers          []string                      `json:"servers"`
	Operations       []connectionprofile.Operation `json:"operations"`
}

type ServiceVersionRef struct {
	ServiceID uuid.UUID `json:"service_id"`
	Version   string    `json:"version"`
}

type ServiceVersionResolvedRef struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
}

type ImportAction string

const (
	ImportActionCreateService ImportAction = "create_service"
	ImportActionUpdateVersion ImportAction = "update_version"
	ImportActionCreateVersion ImportAction = "create_version"
)

// ImportPlan.Status values.
const (
	ImportPlanStatusPending = "pending"
	ImportPlanStatusApplied = "applied"
)

// WorkspaceUsageRef is the minimal identity of a workspace pinned to a
// service version being touched by a ModifyExisting import -- just enough
// for a CLI/CI log to name which workspace is affected, not a full
// fused_workspaces row.
type WorkspaceUsageRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// ComputeNextVersionName returns the next date-based version name for a
// service, given the set of version names that already exist for it. Names
// are server-generated, consumer-meaningful YYYY-MM-DD, not a sequential
// v1/v2/... scheme (which sorts wrong lexicographically once past v9). A
// same-day republish appends -2, -3, ... so two publishes on the same day
// never collide, walking forward from -2 to find the first free suffix
// (handles the unlikely case where an earlier suffix was freed up / never
// taken).
//
// Exported and kept here (rather than private to the repository package)
// so both the publish path (repository.PublishServiceConfiguration /
// PublishDraft) and the OpenAPI import path (openapi.ParseSpec's
// no-explicit-version fallback) share one naming scheme instead of each
// growing their own -- both packages already depend on models, so this is
// the natural shared home.
func ComputeNextVersionName(existing map[string]bool, now time.Time) string {
	base := now.Format("2006-01-02")
	if !existing[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !existing[candidate] {
			return candidate
		}
	}
}

// ─── Integration Object ──────────────────────────────────────────────────────

type IntegrationObject struct {
	ID                uuid.UUID        `json:"id"`
	StableKey         string           `json:"stable_key"`
	AccountID         uuid.UUID        `json:"account_id,omitempty"`
	ServiceID         uuid.UUID        `json:"service_id"`
	SourceID          uuid.UUID        `json:"source_id,omitempty"`
	Name              string           `json:"name"`
	Description       string           `json:"description"`
	ResourceID        uuid.UUID        `json:"resource_id,omitempty"`
	ResourceName      string           `json:"resource,omitempty"` // Used by agent payload
	Embedding         *pgvector.Vector `json:"-"`
	Version           string           `json:"version"`
	ServiceVersionIDs []uuid.UUID      `json:"service_version_ids,omitempty"`
	Status            string           `json:"status"` // "active" | "drifted" | "updating"
	SpecHash          string           `json:"spec_hash"`

	Method          string          `json:"method"`
	NormalizedPath  string          `json:"normalized_path"`
	Path            string          `json:"path"`
	Deprecated      bool            `json:"deprecated"`
	DeprecationDate *time.Time      `json:"deprecation_date,omitempty"`
	Parameters      Parameters      `json:"parameters"`
	RequestContent  *RequestContent `json:"request_content,omitempty"`
	Responses       Responses       `json:"responses"`
	GraphQLQuery    *string         `json:"graphql_query,omitempty"`
	// ProviderProtocol is the provider-facing wire contract, not the SDK/MCP
	// execution transport recorded by activity and usage events.
	ProviderProtocol string `json:"provider_protocol,omitempty"`
	// OperationKind preserves GraphQL query/mutation semantics beyond HTTP POST.
	OperationKind        string                   `json:"operation_kind,omitempty"`
	Pagination           *PaginationConfig        `json:"pagination,omitempty"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
	// IsSSE marks that this endpoint returns a Server-Sent Events stream.
	// The Engine will set Accept: text/event-stream and parse the SSE wire format.
	IsSSE bool `json:"is_sse,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	ProviderProtocolREST    = "rest"
	ProviderProtocolGraphQL = "graphql"
	OperationKindQuery      = "query"
	OperationKindMutation   = "mutation"
)

type PaginationConfig paginationpolicy.Config

type PaginationOverrides map[string]PaginationConfig

func (p PaginationOverrides) Value() (driver.Value, error) {
	return json.Marshal(p)
}

func (p *PaginationOverrides) Scan(value interface{}) error {
	return scanJSONB(value, p)
}

const PathEncodingPreserveSlashes = "preserve_slashes"

type Parameter struct {
	Name         string `json:"name"`
	In           string `json:"in"` // "query" | "path" | "header"
	Required     bool   `json:"required"`
	Type         string `json:"type"`
	Description  string `json:"description"`
	PathEncoding string `json:"path_encoding,omitempty"`
}

type Schema struct {
	Ref                  string            `json:"$ref,omitempty"`
	Type                 string            `json:"type,omitempty"`
	Format               string            `json:"format,omitempty"` // e.g. "binary" for file downloads
	Properties           map[string]Schema `json:"properties,omitempty"`
	Items                *Schema           `json:"items,omitempty"`
	AdditionalProperties *Schema           `json:"additional_properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	Example              any               `json:"example,omitempty"`
}

const (
	RequestSerializationJSON      = "json"
	RequestSerializationForm      = "form_urlencoded"
	RequestSerializationMultipart = "multipart"
	RequestSerializationRaw       = "raw"
	RequestBinaryEncodingBase64   = "base64"
)

// RequestContent is the reviewed provider request representation. Keeping the
// schema with its media type and serialization prevents downstream layers from
// independently guessing how an otherwise identical object must be encoded.
type RequestContent struct {
	MediaType        string                 `json:"media_type"`
	Serialization    string                 `json:"serialization"`
	Required         bool                   `json:"required,omitempty"`
	Schema           *Schema                `json:"schema,omitempty"`
	PayloadParameter string                 `json:"payload_parameter,omitempty"`
	BinaryEncoding   string                 `json:"binary_encoding,omitempty"`
	Parts            map[string]RequestPart `json:"parts,omitempty"`
}

type RequestPart struct {
	ContentType    string `json:"content_type,omitempty"`
	BinaryEncoding string `json:"binary_encoding,omitempty"`
}

type AuthConfig struct {
	Name              string                        `json:"name,omitempty"`   // Logical name from OpenAPI securitySchemes (e.g. "bearerAuth")
	Type              string                        `json:"type"`             // "apiKey", "http", "mutualTLS", "oauth2", "openIdConnect"
	Flow              string                        `json:"flow,omitempty"`   // OAuth2 grant type: "clientCredentials", "authorizationCode", "password", "implicit"
	Scheme            string                        `json:"scheme,omitempty"` // e.g., "bearer", "basic", "digest"
	BasicPasswordMode authrouting.BasicPasswordMode `json:"basic_password_mode,omitempty"`
	Location          string                        `json:"location,omitempty"`          // "header", "query", "cookie"
	KeyName           string                        `json:"key_name,omitempty"`          // e.g., "Authorization", "X-API-Key"
	TokenURL          string                        `json:"token_url,omitempty"`         // For oauth2
	AuthorizationURL  string                        `json:"authorization_url,omitempty"` // For oauth2 authorizationCode/implicit only
	OpenIdConnectUrl  string                        `json:"open_id_connect_url,omitempty"`
	Scopes            []string                      `json:"scopes,omitempty"`

	// Fused Auth: OAuth edge-case fields stored per AuthConfig (not per service, since a
	// service can have multiple auth configs and these settings are per-config).
	PKCERequired        bool              `json:"pkce_required,omitempty"`
	ScopesDelimiter     string            `json:"scopes_delimiter,omitempty"`    // default "space"; set to "comma" when the provider requires it
	TokenEndpointAuth   string            `json:"token_endpoint_auth,omitempty"` // "body" (default) | "basic"
	ExtraAuthParams     map[string]string `json:"extra_auth_params,omitempty"`
	ExtraTokenParams    map[string]string `json:"extra_token_params,omitempty"`
	RefreshTokenRotates bool              `json:"refresh_token_rotates,omitempty"`
}

type AuthConfigs []AuthConfig

type IncomingWebhookConfig struct {
	AuthType            string   `json:"auth_type"`               // "none", "static_token", "hmac_signature", "signature_header"
	AuthLocation        string   `json:"auth_location,omitempty"` // "header", "query"
	AuthKeyName         string   `json:"auth_key_name,omitempty"`
	SignatureHeader     string   `json:"signature_header,omitempty"`
	SigningSecret       string   `json:"signing_secret,omitempty"`       // Usually encrypted at rest
	VerificationHeaders []string `json:"verification_headers,omitempty"` // For signature_header: all required headers for PKI verification
}

type RateLimitConfig = ratelimitpolicy.Config

type RetryConfig struct {
	Strategy   string `json:"strategy"`
	MaxRetries int    `json:"max_retries"`
	BackoffMs  int    `json:"backoff_ms"`
}

// ExecutionPolicy is a read-side snapshot of one published execution-policy
// tier. It exists so the changelog hook can hold both a "before" and "after"
// value in memory at once to diff them; normal contract readers should keep
// using their existing projected fields.
type ExecutionPolicy struct {
	RateLimit             *RateLimitConfig
	Retry                 *RetryConfig
	TimeoutMs             *int
	Pagination            *PaginationConfig
	BaseURLOverride       *string
	EventExtractionPath   *string
	IncomingWebhookConfig *IncomingWebhookConfig
}

// ─── Source ───────────────────────────────────────────────────────────────────

// Source represents a fetched URL whose content produced a Service.
// The embedding is of the raw fetched content so the drift
// watcher can detect semantic changes via vector similarity — a cosine distance
// drop below DRIFT_SIMILARITY_THRESHOLD means the docs have changed
// meaningfully.
type Source struct {
	ID                 uuid.UUID        `json:"id"`
	URL                string           `json:"url"`
	ContentHash        string           `json:"content_hash"`
	Embedding          *pgvector.Vector `json:"-"`
	ServiceID          uuid.UUID        `json:"service_id"`
	LastFetchedAt      time.Time        `json:"last_fetched_at"`
	LastModifiedHeader string           `json:"last_modified_header"`
	ETagHeader         string           `json:"etag_header"`
	RawContent         string           `json:"-"`
	OpenAPISpec        string           `json:"openapi_spec,omitempty"`
	ImportMethod       string           `json:"import_method"`
	CreatedAt          time.Time        `json:"created_at"`
}

// ─── Drift Snapshot ───────────────────────────────────────────────────────────

type DriftSnapshot struct {
	ID                  uuid.UUID `json:"id"`
	SourceID            uuid.UUID `json:"source_id"`
	IntegrationObjectID uuid.UUID `json:"integration_object_id"`
	// WebhookObjectID identifies the webhook a snapshot is about, for
	// snapshots that describe a webhook rather than an endpoint. Exactly one
	// of IntegrationObjectID and WebhookObjectID is ever set on a given row,
	// never both. A pointer (unlike IntegrationObjectID) because it's
	// genuinely optional: nil for endpoint drift, set for webhook drift --
	// when set, IntegrationObjectID is left at its zero value and stored as
	// SQL NULL rather than the literal all-zero UUID (see nilIfZeroUUID in
	// the repository).
	WebhookObjectID *uuid.UUID   `json:"webhook_object_id,omitempty"`
	PreviousHash    string       `json:"previous_hash"`
	CurrentHash     string       `json:"current_hash"`
	Diff            DriftChanges `json:"diff"`
	DetectedAt      time.Time    `json:"detected_at"`
	Status          string       `json:"status"` // "pending" | "applied" | "dismissed"
	// ServiceID is populated only by service-scoped queries
	// (ListPendingDriftSnapshotsForServices and its GraphQL/Engine-client
	// callers), which resolve it via a join through integration_objects/
	// webhook_objects since drift_snapshots itself has no service_id column.
	// Left at its zero value by every other query -- callers that need to
	// attribute a batch of snapshots back to the service each one belongs to
	// (the workspace/SDK notification inbox) read this instead of tracking
	// serviceID separately alongside each snapshot.
	ServiceID uuid.UUID `json:"service_id,omitempty"`
}

type DriftChange struct {
	Field       string `json:"field"`
	OldValue    any    `json:"old_value"`
	NewValue    any    `json:"new_value"`
	Severity    string `json:"severity"` // "breaking" | "non-breaking"
	Description string `json:"description"`
}

// ServiceChangelogConfigType enumerates which publishable surface a
// service_changelog row is about. See plans/plan-service-changelog.md.
type ServiceChangelogConfigType string

const (
	ServiceChangelogConfigTypeVersion           ServiceChangelogConfigType = "version"
	ServiceChangelogConfigTypeExecutionPolicy   ServiceChangelogConfigType = "execution_policy"
	ServiceChangelogConfigTypeConnectionProfile ServiceChangelogConfigType = "connection_profile"
)

// ServiceChangelogType is the four-value classification from
// plan-service-changelog.md ("## changelog_type") -- not every value applies
// to every ConfigType; see that doc for exactly which combinations exist.
type ServiceChangelogType string

const (
	ServiceChangelogTypeNew        ServiceChangelogType = "new"
	ServiceChangelogTypeChanged    ServiceChangelogType = "changed"
	ServiceChangelogTypeDeprecated ServiceChangelogType = "deprecated"
	ServiceChangelogTypeRemoved    ServiceChangelogType = "removed"
)

// ServiceChangelogEntry is one row recording an owner-published change to a
// service's already-Registry-known data -- distinct from DriftSnapshot
// above, which tracks a live provider API diverging from what was imported.
// One row per publish event, not one row per changed field: full detail
// lives inside Diff.
//
// Version is nil for service-wide execution-policy changes and
// service-granularity `removed` rows; set for everything scoped to one provider
// version.
//
// Diff is nil for `new`/`deprecated`/`removed` rows (nothing to compare
// against). When set, its shape depends on ConfigType: []DriftChange for
// execution_policy/connection_profile, or drift.EndpointVersionDiff for
// version -- left as json.RawMessage here (rather than importing
// registry/drift's type) so this shared model package has no dependency on
// a Registry-internal package; callers decode based on ConfigType.
type ServiceChangelogEntry struct {
	ID            uuid.UUID                  `json:"id"`
	ServiceID     uuid.UUID                  `json:"service_id"`
	Version       *string                    `json:"version,omitempty"`
	ConfigType    ServiceChangelogConfigType `json:"config_type"`
	ChangelogType ServiceChangelogType       `json:"changelog_type"`
	Diff          json.RawMessage            `json:"diff,omitempty"`
	PlanID        *uuid.UUID                 `json:"plan_id,omitempty"`
	ConfigKey     *string                    `json:"config_key,omitempty"`
	CreatedBy     *uuid.UUID                 `json:"created_by,omitempty"`
	CreatedAt     time.Time                  `json:"created_at"`
}

type ServiceGenerationResult struct {
	Service         *Service            `json:"service"`
	Integrations    []IntegrationObject `json:"integrations"`
	Webhooks        []WebhookObject     `json:"webhooks,omitempty"`
	SourceURL       string              `json:"source_url,omitempty"`
	ImportMethod    string              `json:"import_method,omitempty"`
	ServiceVersions []ServiceVersion    `json:"service_versions,omitempty"`
}

// ─── Session ──────────────────────────────────────────────────────────────────

// Session holds the full state of one integration generation run.
type Session struct {
	ID                    string                   `json:"id"`
	ServiceName           string                   `json:"service_name"`
	ServiceSlug           string                   `json:"service_slug,omitempty"`
	TargetVersion         string                   `json:"target_version"`
	SourceURL             string                   `json:"source_url"`
	TargetServiceID       uuid.UUID                `json:"target_service_id,omitempty"`
	TargetResourceName    string                   `json:"target_resource_name,omitempty"`
	TargetType            string                   `json:"target_type"`
	Instructions          string                   `json:"instructions,omitempty"`
	Messages              ChatMessages             `json:"messages"`
	State                 string                   `json:"state"`
	PendingQuestion       *string                  `json:"pending_question,omitempty"`
	FetchedContent        string                   `json:"-"`
	AccountID             uuid.UUID                `json:"account_id"`
	Result                *ServiceGenerationResult `json:"result,omitempty"`
	AccumulatedPaths      AccumulatedPaths         `json:"accumulated_paths,omitempty"`
	AccumulatedComponents AccumulatedPaths         `json:"accumulated_components,omitempty"`
	ChunkIndex            int                      `json:"chunk_index"`
	Error                 string                   `json:"error,omitempty"`
	CreatedAt             time.Time                `json:"created_at"`
	UpdatedAt             time.Time                `json:"updated_at"`
}

const SDKDefinitionSchemaVersion = 1

type SDKSelection struct {
	ServiceID               uuid.UUID   `json:"service_id"`
	ServiceVersionID        uuid.UUID   `json:"service_version_id"`
	DefinitionSchemaVersion int         `json:"definition_schema_version,omitempty"`
	EndpointIDs             []uuid.UUID `json:"endpoint_ids"`
	OperationNames          []string    `json:"operation_names,omitempty"`
	WebhookIDs              []uuid.UUID `json:"webhook_ids,omitempty"`
	WebhookNames            []string    `json:"webhook_names,omitempty"`
	SelectAll               bool        `json:"select_all,omitempty"`
	// WebhookSelectAll is the webhook-only counterpart to SelectAll -- a
	// service can select every operation and only an explicit subset of
	// webhooks, or vice versa (see cli/internal/configfile's
	// App service configuration's WebhooksSelectAll and
	// plans/plan-webhook-kind.md), so
	// this can't reuse SelectAll's single flag. sdkWebhookMatchesSelection
	// (postgres_repository_sdk_generator.go) treats SelectAll and
	// WebhookSelectAll as equivalent for webhook matching -- either one means
	// "all webhooks" -- while only SelectAll matches operations.
	WebhookSelectAll bool `json:"webhook_select_all,omitempty"`
	// AuthType and AuthName pin dispatch to the scheme selected when the
	// app was planned. Runtime callers should not have to rediscover a
	// provider's security-scheme spelling or rely on Registry ordering.
	AuthType      string               `json:"auth_type,omitempty"`
	AuthName      string               `json:"auth_name,omitempty"`
	ConnectScopes []string             `json:"connect_scopes,omitempty"`
	Injections    []SDKInjectionConfig `json:"injections,omitempty"`
}

type SDKInjectionConfig struct {
	Location string `json:"location"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Mode     string `json:"mode,omitempty"`
}

const AppScopeSchemaVersion = 2

const SDKGeneratorVersion = "registry-generator-v1"

const (
	SDKGenerationStatusPending  = "pending"
	SDKGenerationStatusComplete = "complete"
	SDKGenerationStatusFailed   = "failed"
)

type SDKContractBinding struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	Revision         int       `json:"revision"`
	SourceHash       string    `json:"source_hash"`
}

type SDKGenerationRequest struct {
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Version          string               `json:"version"`
	AppFamilyID      uuid.UUID            `json:"app_family_id"`
	AppID            uuid.UUID            `json:"app_id"`
	SourceHash       string               `json:"source_hash"`
	GeneratorVersion string               `json:"generator_version"`
	IdempotencyKey   string               `json:"idempotency_key,omitempty"`
	Selections       []SDKSelection       `json:"selections"`
	IncludeMCP       bool                 `json:"include_mcp"`
	TargetType       string               `json:"target_type"`
	TargetLanguage   string               `json:"target_language"`
	SkipSandbox      bool                 `json:"skip_sandbox"`
	ContractBindings []SDKContractBinding `json:"contract_bindings,omitempty"`
}

type SDKGenerationResult struct {
	AppFamilyID        uuid.UUID     `json:"app_family_id"`
	AppID              uuid.UUID     `json:"app_id"`
	AccountID          uuid.UUID     `json:"account_id,omitempty"`
	EngineID           uuid.UUID     `json:"-"`
	SourceHash         string        `json:"-"`
	JobID              string        `json:"job_id"`
	Status             string        `json:"status"`
	ScopeSchemaVersion int           `json:"scope_schema_version"`
	GeneratorVersion   string        `json:"generator_version"`
	Selections         SDKSelections `json:"selections"`
	RequestHash        string        `json:"-"`
}

const SDKPackageLeaseBatchLimit = 500

// SDKPackageLeaseRenewal is the minimal identity the Registry needs to retain
// one generated SDK package. Runtime configuration stays on the Engine.
type SDKPackageLeaseRenewal struct {
	AppID       uuid.UUID `json:"app_id"`
	AppFamilyID uuid.UUID `json:"app_family_id"`
}

// Fused Auth (SDKAuthKey, SDKAuthConnection, SDKOAuthConfig, AuthWebhookAttempt
// -- the hosted Connect runtime and its SDK-generation-time keypair
// provisioning) has been fully removed. It solved a multi-tenant "many end
// users, each with their own connected account" problem this product doesn't
// have; the Engine's workspace secrets store covers the real use case.

// SDKAuthConnection, SDKOAuthConfig, and AuthWebhookAttempt (the hosted
// Connect runtime's per-end-user connection storage, OAuth client-secret
// storage, and webhook delivery log) were removed along with the handlers,
// repository methods, and refresh worker that were their only users.

// ─── MCP Analytics ────────────────────────────────────────────────────────────

type MCPSession struct {
	ID        uuid.UUID  `json:"id"`
	AppID     uuid.UUID  `json:"app_id"`
	SessionID string     `json:"session_id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// MCPToolUsage/MCPServiceUsage are the per-dimension breakdowns
// GetMCPAnalyticsDashboard groups canonical events by endpoint and service.
// Two distinct types rather than one generic
// "Name" struct so each carries the field name the MCP analytics UI page
// already expects (tool_name vs service_name) without a translation layer.
type MCPToolUsage struct {
	ToolName         string  `json:"tool_name"`
	Count            int64   `json:"count"`
	Failed           int64   `json:"failed"`
	AverageLatencyMs float64 `json:"average_latency"`
}

type MCPServiceUsage struct {
	ServiceName      string  `json:"service_name"`
	Count            int64   `json:"count"`
	Failed           int64   `json:"failed"`
	AverageLatencyMs float64 `json:"average_latency"`
}

// MCPAnalyticsDashboard is the read shape for one SDK's MCP analytics page:
// overall totals plus per-tool/per-service breakdowns and recent sessions.
// This is the Engine-native replacement for the Registry's old (dead-stub)
// mcpAnalytics GraphQL field -- the field names match what that field's
// consumer (ui/app/routes/integrations.mcp_.$id.analytics.tsx) already
// expects, so the UI's query shape didn't need to change, only where it's
// sent.
type MCPAnalyticsDashboard struct {
	TotalRequests    int64   `json:"total_requests"`
	FailedRequests   int64   `json:"failed_requests"`
	AverageLatencyMs float64 `json:"average_latency"`
	// ActiveAgents counts sessions with no EndedAt yet (fused_mcp_sessions) --
	// "agent" here means one live MCP client connection, not a distinct
	// human/bot identity, which the Engine has no way to observe.
	ActiveAgents   int64             `json:"active_agents"`
	ToolUsage      []MCPToolUsage    `json:"tool_usage"`
	ServiceUsage   []MCPServiceUsage `json:"service_usage"`
	RecentSessions []MCPSession      `json:"recent_sessions"`
}

// ─── Engine Execution Audit ──────────────────────────────────────────────────

const (
	EngineExecutionTransportSDK     = "sdk"
	EngineExecutionTransportMCP     = "mcp"
	EngineExecutionTransportWebhook = "webhook"

	EngineExecutionDirectionOutbound = "outbound"
	EngineExecutionDirectionInbound  = "inbound"

	EngineExecutionStatusSuccess = "success"
	EngineExecutionStatusFailed  = "failed"
)

// EngineExecutionEvent is the durable product receipt for an SDK, MCP, or webhook execution.
// Rich per-step diagnostics stay in OTEL; this row is intentionally
// compact so user-facing history and dependency checks do not depend on an
// observability backend being configured.
type EngineExecutionEvent struct {
	ID                     uuid.UUID `json:"id" db:"id"`
	TraceID                string    `json:"trace_id,omitempty" db:"trace_id"`
	SpanID                 string    `json:"span_id,omitempty" db:"span_id"`
	AccountID              uuid.UUID `json:"account_id,omitempty" db:"account_id"`
	AppFamilyID            uuid.UUID `json:"app_family_id,omitempty" db:"app_family_id"`
	AppID                  uuid.UUID `json:"app_id,omitempty" db:"app_id"`
	AppVersion             string    `json:"app_version,omitempty" db:"app_version"`
	Transport              string    `json:"transport" db:"transport"`
	ProviderProtocol       string    `json:"provider_protocol,omitempty" db:"provider_protocol"`
	Direction              string    `json:"direction" db:"direction"`
	ServiceID              uuid.UUID `json:"service_id,omitempty" db:"service_id"`
	ServiceVersionID       string    `json:"service_version_id" db:"service_version_id"`
	OperationID            uuid.UUID `json:"operation_id,omitempty" db:"operation_id"`
	WebhookID              uuid.UUID `json:"webhook_id,omitempty" db:"webhook_id"`
	EndpointName           string    `json:"endpoint_name" db:"endpoint_name"`
	ExternalID             string    `json:"external_id,omitempty" db:"external_id"`
	EventName              string    `json:"event_name,omitempty" db:"event_name"`
	HTTPMethod             string    `json:"http_method,omitempty" db:"http_method"`
	RequestPath            string    `json:"request_path,omitempty" db:"request_path"`
	Environment            string    `json:"environment,omitempty" db:"environment"`
	EnvironmentSource      string    `json:"environment_source,omitempty" db:"environment_source"`
	ProviderHost           string    `json:"provider_host,omitempty" db:"provider_host"`
	ProviderHTTPStatus     *int      `json:"provider_http_status,omitempty" db:"provider_http_status"`
	ProviderStatusClass    string    `json:"provider_status_class,omitempty" db:"provider_status_class"`
	Status                 string    `json:"status" db:"status"`
	FailureReason          string    `json:"failure_reason,omitempty" db:"failure_reason"`
	FailureCategory        string    `json:"failure_category,omitempty" db:"failure_category"`
	FailureCode            string    `json:"failure_code,omitempty" db:"failure_code"`
	LatencyMs              int64     `json:"latency_ms" db:"latency_ms"`
	ProviderLatencyMs      *int64    `json:"provider_latency_ms,omitempty" db:"provider_latency_ms"`
	AttemptCount           int       `json:"attempt_count" db:"attempt_count"`
	AuthSchemeNames        []string  `json:"auth_scheme_names,omitempty" db:"auth_scheme_names"`
	AuthSchemeTypes        []string  `json:"auth_scheme_types,omitempty" db:"auth_scheme_types"`
	AuthSchemeCount        int64     `json:"auth_scheme_count,omitempty" db:"auth_scheme_count"`
	AuthSelectionOutcome   string    `json:"auth_selection_outcome,omitempty" db:"auth_selection_outcome"`
	PaginationType         string    `json:"pagination_type,omitempty" db:"pagination_type"`
	PaginationPageCount    int64     `json:"pagination_page_count,omitempty" db:"pagination_page_count"`
	PaginationItemCount    int64     `json:"pagination_item_count,omitempty" db:"pagination_item_count"`
	PaginationByteCount    int64     `json:"pagination_byte_count,omitempty" db:"pagination_byte_count"`
	PaginationStopReason   string    `json:"pagination_stop_reason,omitempty" db:"pagination_stop_reason"`
	RateLimitDecision      string    `json:"rate_limit_decision,omitempty" db:"rate_limit_decision"`
	RateLimitPolicyCount   int64     `json:"rate_limit_policy_count,omitempty" db:"rate_limit_policy_count"`
	RateLimitScopeKinds    []string  `json:"rate_limit_scope_kinds,omitempty" db:"rate_limit_scope_kinds"`
	RateLimitUnits         []string  `json:"rate_limit_units,omitempty" db:"rate_limit_units"`
	RateLimitUnitTotals    []int64   `json:"rate_limit_unit_totals,omitempty" db:"rate_limit_unit_totals"`
	RateLimitRetryOutcome  string    `json:"rate_limit_retry_outcome,omitempty" db:"rate_limit_retry_outcome"`
	RateLimitHeaderOutcome string    `json:"rate_limit_header_outcome,omitempty" db:"rate_limit_header_outcome"`
	RequestBytes           int64     `json:"request_bytes,omitempty" db:"request_bytes"`
	ResponseBytes          int64     `json:"response_bytes,omitempty" db:"response_bytes"`
	VerificationStatus     string    `json:"verification_status,omitempty" db:"verification_status"`
	DeliveryStatus         string    `json:"delivery_status,omitempty" db:"delivery_status"`
	IdempotencyKeyHash     string    `json:"idempotency_key_hash,omitempty" db:"idempotency_key_hash"`
	RequestBodyHash        string    `json:"request_body_hash,omitempty" db:"request_body_hash"`
	// IdempotencyReplayed is true when this execution was served from the
	// idempotency cache (see IdempotentExecution) instead of calling the
	// vendor -- lets callers see cache-hit rate without a separate metric.
	IdempotencyReplayed bool      `json:"idempotency_replayed,omitempty" db:"idempotency_replayed"`
	Timings             []byte    `json:"timings,omitempty" db:"timings"`
	StartedAt           time.Time `json:"started_at" db:"started_at"`
	EndedAt             time.Time `json:"ended_at" db:"ended_at"`
	CreatedAt           time.Time `json:"created_at" db:"created_at"`
}

const EngineExecutionEventSchemaVersion = 4

// EngineExecutionEventEnvelope keeps the NATS contract versioned independently
// from the database schema so a malformed or future message can be rejected
// before it reaches persistence.
type EngineExecutionEventEnvelope struct {
	SchemaVersion int                  `json:"schema_version"`
	Event         EngineExecutionEvent `json:"event"`
}

// EngineExecutionAnalytics is the compact summary shown above a service's
// outbound call history. It deliberately mirrors the durable execution
// receipts instead of depending on an external observability backend.
type EngineExecutionAnalytics struct {
	TotalCalls       int64   `json:"total_calls"`
	SuccessfulCalls  int64   `json:"successful_calls"`
	FailedCalls      int64   `json:"failed_calls"`
	AverageLatencyMs float64 `json:"average_latency_ms"`
	MedianLatencyMs  float64 `json:"median_latency_ms"`
	P95LatencyMs     float64 `json:"p95_latency_ms"`
}

type EngineExecutionBreakdown struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	TotalCalls   int64   `json:"total_calls"`
	FailedCalls  int64   `json:"failed_calls"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
}

// AppExecutionAnalytics keeps app totals and service distribution
// on the canonical execution-event model rather than the legacy MCP tables.
type AppExecutionAnalytics struct {
	EngineExecutionAnalytics
	ByService []EngineExecutionBreakdown `json:"by_service"`
}

type EngineExecutionFailure struct {
	ID              uuid.UUID `json:"id"`
	ServiceID       uuid.UUID `json:"service_id"`
	ServiceName     string    `json:"service_name"`
	Operation       string    `json:"operation"`
	Transport       string    `json:"transport"`
	FailureCategory string    `json:"failure_category"`
	FailureCode     string    `json:"failure_code"`
	FailureReason   string    `json:"failure_reason"`
	LatencyMs       int64     `json:"latency_ms"`
	StartedAt       time.Time `json:"started_at"`
}

type WorkspaceExecutionAnalytics struct {
	EngineExecutionAnalytics
	ByService      []EngineExecutionBreakdown `json:"by_service"`
	ByTransport    []EngineExecutionBreakdown `json:"by_transport"`
	RecentFailures []EngineExecutionFailure   `json:"recent_failures"`
}

// ─── Idempotency Cache ──────────────────────────────────────────────────────

// IdempotencyTTL is how long a cached execution response stays valid, mirroring
// Stripe's idempotency key retention convention: long enough to cover a client
// reconnecting well after Engine's own bounded retry loop has finished (which
// completes in seconds), short enough to bound storage growth.
const IdempotencyTTL = 24 * time.Hour

// IdempotentExecution caches the final response of one Execute call, keyed by
// (app_id, idempotency key). A repeated call with the same key -- a client
// retry, a dropped-response reconnect, or overlap with Engine's own retry
// loop -- replays this row instead of re-hitting the vendor. RequestBodyHash
// guards against key reuse across two genuinely different requests: a
// mismatch is a caller error (ErrIdempotencyKeyConflict), not a cache hit.
// Only successful executions are cached -- see recordEngineExecutionAudit's
// caller in sandbox -- so a transient failure never gets "stuck" behind a
// replayed error for the TTL window.
type IdempotentExecution struct {
	ID                 uuid.UUID `db:"id"`
	AppID              uuid.UUID `db:"app_id"`
	IdempotencyKeyHash string    `db:"idempotency_key_hash"`
	RequestBodyHash    string    `db:"request_body_hash"`
	Environment        string    `db:"environment"`
	ResponseBody       []byte    `db:"response_body"`
	ResponseStatus     int       `db:"response_status"`
	CreatedAt          time.Time `db:"created_at"`
	ExpiresAt          time.Time `db:"expires_at"`
}

// ─── Lead (Waitlist) ──────────────────────────────────────────────────────────

type Lead struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Language  string    `json:"language"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// --- JSONB Scanners & Valuers ---

func scanJSONB(value interface{}, target interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		s, ok := value.(string)
		if !ok {
			return errors.New("type assertion to []byte or string failed")
		}
		b = []byte(s)
	}
	return json.Unmarshal(b, target)
}

type Responses map[string]Schema

func (r Responses) Value() (driver.Value, error)  { return json.Marshal(r) }
func (r *Responses) Scan(value interface{}) error { return scanJSONB(value, r) }

type DefaultHeaders map[string]string

func (d DefaultHeaders) Value() (driver.Value, error)  { return json.Marshal(d) }
func (d *DefaultHeaders) Scan(value interface{}) error { return scanJSONB(value, d) }

func (s Schema) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *Schema) Scan(value interface{}) error { return scanJSONB(value, s) }

func (r RequestContent) Value() (driver.Value, error)  { return json.Marshal(r) }
func (r *RequestContent) Scan(value interface{}) error { return scanJSONB(value, r) }

func (s Servers) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *Servers) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *AuthConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfigs) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *AuthConfigs) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s RetryConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *RetryConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (p PaginationConfig) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *PaginationConfig) Scan(value interface{}) error { return scanJSONB(value, p) }

func getEncryptionKey() []byte {
	if len(config.GlobalEncryptionKey) == 32 {
		return config.GlobalEncryptionKey
	}
	key := os.Getenv("ENCRYPTION_KEY")
	if len(key) >= 32 {
		return []byte(key[:32])
	}
	b := make([]byte, 32)
	copy(b, "fused-default-encrypt-key-32b")
	if len(key) > 0 {
		copy(b, key)
	}
	return b
}

func encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(getEncryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decrypt(ciphertextStr string) (string, error) {
	if ciphertextStr == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextStr)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(getEncryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, actualCiphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (i IncomingWebhookConfig) Value() (driver.Value, error) {
	if i.SigningSecret != "" {
		if _, err := decrypt(i.SigningSecret); err != nil {
			// Decryption failed -> it is plaintext! Let's encrypt it.
			encSecret, err := encrypt(i.SigningSecret)
			if err != nil {
				return nil, err
			}
			i.SigningSecret = encSecret
		}
	}
	return json.Marshal(i)
}

func (i *IncomingWebhookConfig) Scan(value interface{}) error {
	return scanJSONB(value, i)
}

func (p Parameter) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameter) Scan(value interface{}) error { return scanJSONB(value, p) }

type Parameters []Parameter

func (p Parameters) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameters) Scan(value interface{}) error { return scanJSONB(value, p) }

type AccumulatedPaths map[string]any

func (a AccumulatedPaths) Value() (driver.Value, error)  { return json.Marshal(a) }
func (a *AccumulatedPaths) Scan(value interface{}) error { return scanJSONB(value, a) }

type DriftChanges []DriftChange

func (d DriftChanges) Value() (driver.Value, error)  { return json.Marshal(d) }
func (d *DriftChanges) Scan(value interface{}) error { return scanJSONB(value, d) }

type SDKSelections []SDKSelection

func (s SDKSelections) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *SDKSelections) Scan(value interface{}) error { return scanJSONB(value, s) }

type ChatMessages []openai.ChatCompletionMessage

func (c ChatMessages) Value() (driver.Value, error)  { return json.Marshal(c) }
func (c *ChatMessages) Scan(value interface{}) error { return scanJSONB(value, c) }

func (r *ServiceGenerationResult) Value() (driver.Value, error) {
	if r == nil {
		return nil, nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (r *ServiceGenerationResult) Scan(value interface{}) error { return scanJSONB(value, r) }

type UUIDArray []uuid.UUID

func (u UUIDArray) Value() (driver.Value, error) {
	if len(u) == 0 {
		return "{}", nil
	}
	var sb strings.Builder
	sb.WriteRune('{')
	for i, val := range u {
		if i > 0 {
			sb.WriteRune(',')
		}
		sb.WriteString(val.String())
	}
	sb.WriteRune('}')
	return sb.String(), nil
}

func (u *UUIDArray) Scan(value interface{}) error {
	if value == nil {
		*u = nil
		return nil
	}
	var srcStr string
	switch v := value.(type) {
	case string:
		srcStr = v
	case []byte:
		srcStr = string(v)
	default:
		return fmt.Errorf("cannot scan type %T into UUIDArray", value)
	}

	if len(srcStr) < 2 || srcStr[0] != '{' || srcStr[len(srcStr)-1] != '}' {
		return fmt.Errorf("invalid array format: %q", srcStr)
	}

	content := srcStr[1 : len(srcStr)-1]
	if content == "" {
		*u = []uuid.UUID{}
		return nil
	}

	parts := strings.Split(content, ",")
	res := make([]uuid.UUID, len(parts))
	for i, part := range parts {
		parsed, err := uuid.Parse(strings.Trim(part, `"`))
		if err != nil {
			return fmt.Errorf("failed to parse uuid element: %w", err)
		}
		res[i] = parsed
	}
	*u = res
	return nil
}
