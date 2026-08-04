package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

var (
	ErrArtifactScopeNotFound        = errors.New("sdk scope not found")
	ErrSDKBucketImmutable           = errors.New("sdk bucket assignment is immutable")
	ErrBucketNotFound               = errors.New("bucket not found")
	ErrBucketBound                  = errors.New("bucket is bound to an artifact")
	ErrDefaultBucketProtected       = errors.New("default bucket cannot be deleted")
	ErrAuthConnectionNotFound       = errors.New("auth connection not found")
	ErrConnectSessionUnavailable    = errors.New("connect session not found or already used")
	ErrInvalidEncryptedAuthMaterial = errors.New("invalid encrypted auth material")

	// ErrIdempotentExecutionNotFound means there's no unexpired cached
	// response for the given (artifact_id, idempotency key) -- the caller should
	// dispatch to the vendor normally.
	ErrIdempotentExecutionNotFound = errors.New("idempotent execution not found")
	// ErrIdempotencyKeyConflict means the idempotency key was reused with a
	// different request body than the cached one -- the caller reused a key
	// for a logically different request and must use a new one.
	ErrIdempotencyKeyConflict = errors.New("idempotency key reused with a different request body")
)

type ArtifactScope struct {
	AccountID  uuid.UUID
	ArtifactID uuid.UUID
	// Exactly one owner is set. Subject ownership is the safe default derived
	// from the authenticated actor; team ownership is an explicit sharing
	// decision resolved from a stable team slug by the Engine.
	OwnerSubjectID     uuid.UUID
	OwnerTeamID        uuid.UUID
	BucketID           uuid.UUID
	Selections         []byte
	ScopeSchemaVersion int
	// DeactivatedAt is nil for an active SDK/MCP. Non-nil blocks new MCP
	// session connections (LocalObjectCache.loadArtifactScope checks this) --
	// live sessions already in flight are killed separately by the caller
	// that sets this, not by the presence of the field itself.
	DeactivatedAt *time.Time
	// Kind labels how this scope is meant to be connected to: "sdk" (default)
	// or "mcp". A scope's shape (selections+bucket) is identical either way --
	// this is a listing/UI distinction (see ListMCPScopesByAccount), not an
	// enforcement mechanism.
	Kind string
	// Name is an optional user-supplied label (CLI --name flag, or a
	// workspace config's name), surfaced on the MCP servers list page. Never
	// set by a reactivate-only activate call (persistArtifactScope isn't invoked
	// on that path), so it can be empty even for a scope in active use.
	Name string
	// Version and ConfigKey identify the immutable declaration that created a
	// runtime scope; they are metadata only and never contain credentials.
	Version   string
	ConfigKey string
	// CreatedAt is read-only, populated from the DB default -- never written
	// by SaveArtifactScope.
	CreatedAt time.Time
}

type SDKToken struct {
	ID         uuid.UUID
	ArtifactID uuid.UUID
	TokenHash  string
	Name       string
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

type Bucket struct {
	ID        uuid.UUID
	Name      string
	IsDefault bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type BucketSummary struct {
	Bucket
	SecretCount        int
	ValueCount         int
	ConnectedUserCount int
}

type BucketConnectSummary struct {
	BucketID           uuid.UUID
	ConnectConfigCount int
	ConnectedUserCount int
}

type BucketServiceSummary struct {
	ServiceID          uuid.UUID
	ServiceName        string
	SecretCount        int
	ValueCount         int
	ConnectConfigCount int
	ConnectedUserCount int
}

type BucketValue struct {
	ID         uuid.UUID
	BucketID   uuid.UUID
	ServiceID  uuid.UUID
	KeyName    string
	Location   string
	Value      string
	Mode       string
	SourceKind string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// WorkspaceConnectionProfile is one layer (baseline or override) of the
// workspace + service + service_version + auth_type profile identity. The
// effective profile is resolved in SQL (override wins over baseline when both
// exist) rather than by loading both layers into Go -- see
// plans/workspace_connection_profile_scope_plan.md, "Effective Profile Query".
type WorkspaceConnectionProfile struct {
	ID        uuid.UUID
	ServiceID uuid.UUID
	// ServiceVersionID pins the exact version this profile governs -- a
	// profile for one version never affects another, even for the same
	// service and auth type.
	ServiceVersionID uuid.UUID
	AuthType         string
	// Layer is "baseline" (pinned Registry/Fused publication) or "override"
	// (workspace-authored). Keeping them as separate rows, rather than one row
	// with an override flag, makes reset a targeted delete of the override
	// while the baseline stays in place as an always-available fallback.
	Layer             string
	RegistryProfileID *uuid.UUID
	ProfileRevision   int
	ProfileHash       string
	Provenance        string
	ProfileSnapshot   []byte
	// IsPublic records whether this profile was published to the Registry as a
	// public baseline (workspace.yaml connection_profiles[*].public: true).
	// Local bookkeeping only, so `fused sync` can round-trip the intent.
	IsPublic  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkspaceProfileRef identifies one workspace + service + service_version +
// auth_type tuple, independent of any bucket. This is the effective profile's
// full identity per the plan.
type WorkspaceProfileRef struct {
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	AuthType         string
}

// WorkspaceConnectionBinding is the compiled runtime representation of one
// profile expression. It has no BucketID and no LocallyOverridden flag: the
// parent profile's Layer alone determines precedence, and routing bindings
// follow the same workspace-scoped identity as the profile that owns them.
type WorkspaceConnectionBinding struct {
	ID                    uuid.UUID
	ServiceID             uuid.UUID
	ServiceVersionID      uuid.UUID
	ProfileID             uuid.UUID
	SourceKind            string
	LiteralValue          *string
	SourcePath            *string
	TargetLocation        string
	TargetName            string
	OperationIDs          []string
	Mode                  string
	Provenance            string
	SourceProfileRevision *int
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// WorkspaceProfileReplacement groups one profile layer with the bindings it
// owns so workspace apply can reconcile many versions in one transaction.
type WorkspaceProfileReplacement struct {
	Profile  WorkspaceConnectionProfile
	Bindings []WorkspaceConnectionBinding
}

// WorkspaceProfileStore is kept separate from Store while rollout is in
// progress, allowing existing narrow test doubles to remain focused. Runtime
// paths require this capability and never fall back to broad bucket reads.
type WorkspaceProfileStore interface {
	// UpsertWorkspaceProfileOverride creates or updates the workspace override
	// layer and replaces its bindings in one transaction. Callers never choose
	// between create and update -- editing an effective profile is always an
	// upsert of the override, per the plan's product rules.
	UpsertWorkspaceProfileOverride(ctx context.Context, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) (*WorkspaceConnectionProfile, error)
	// ResetWorkspaceProfile deletes only the override layer (and its bindings,
	// via FK cascade); the baseline, if any, is left untouched so it becomes
	// the new effective profile without a Registry call.
	ResetWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error
	// GetEffectiveWorkspaceProfile resolves the override-if-present-else-
	// baseline precedence in one SQL query.
	GetEffectiveWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) (*WorkspaceConnectionProfile, error)
	// GetEffectiveWorkspaceProfiles is GetEffectiveWorkspaceProfile's batched
	// sibling for workspace apply and multi-service UI/sync reads.
	GetEffectiveWorkspaceProfiles(ctx context.Context, refs []WorkspaceProfileRef) ([]WorkspaceConnectionProfile, error)
	// ListWorkspaceProfileBindings returns the effective profile's compiled
	// rows for admin/read views (no operation filtering).
	ListWorkspaceProfileBindings(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) ([]WorkspaceConnectionBinding, error)
	// ListWorkspaceBindingsForExecution filters by workspace, service, exact
	// version, auth type, and operation in one query -- the runtime dispatch
	// hot path. bucketID is accepted only to derive and enforce workspace
	// ownership through a join; it is not part of the profile identity.
	ListWorkspaceBindingsForExecution(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) ([]WorkspaceConnectionBinding, error)
	// MarkWorkspaceProfilePublished sets is_public on the effective profile row
	// (override if present, else baseline) after a successful Registry publish.
	// A no-op (not an error) if no row exists yet for the tuple.
	MarkWorkspaceProfilePublished(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error
}

// WorkspaceProfileBatchStore is required only by multi-version workspace
// apply; individual GraphQL edits retain the narrower WorkspaceProfileStore
// operation.
type WorkspaceProfileBatchStore interface {
	// ReconcileWorkspaceProfiles applies all replacements and deletes in one
	// transaction, keyed by workspace tuples rather than bucket tuples.
	ReconcileWorkspaceProfiles(ctx context.Context, replacements []WorkspaceProfileReplacement, deletes []WorkspaceProfileRef) error
}

// WorkspaceServiceVersionStatusStore exposes the exact activation check used
// by profile mutations without widening Store and every unrelated test double.
type WorkspaceServiceVersionStatusStore interface {
	IsWorkspaceServiceVersionActive(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (bool, error)
}

// WorkspaceServiceVersionLookupStore resolves one exact activated version.
// Refresh uses this instead of listing a service's versions and filtering in
// Go, because a pinned refresh must never accidentally float to latest.
type WorkspaceServiceVersionLookupStore interface {
	GetWorkspaceServiceVersion(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceServiceVersion, error)
}

// WorkspaceServiceVersionContractBackfillStore lists active version rows that
// still lack Engine-local runtime contracts. The query owns the anti-join so a
// rollout backfill never lists all activations and filters snapshots in Go.
type WorkspaceServiceVersionContractBackfillStore interface {
	ListWorkspaceServiceVersionsMissingContractSnapshots(ctx context.Context, limit int) ([]WorkspaceServiceVersion, error)
}

// WorkspaceExecutionPolicyOverride is a workspace-local declaration of
// rate_limit/retry_config/pagination/event_extraction_path/
// incoming_webhook_config for a service the workspace does not own or has not
// published. There is no Layer field here: a row existing always means
// "override" -- published provider values are read from the runtime contract
// snapshot and never duplicated into this table as a local baseline.
// ServiceVersionID nil scopes the row to the service default; set, it scopes
// to that one version.
type WorkspaceExecutionPolicyOverride struct {
	ID                    uuid.UUID
	ServiceID             uuid.UUID
	ServiceVersionID      *uuid.UUID
	RateLimit             *fusedobject.RateLimitConfig
	RetryConfig           *fusedobject.RetryConfig
	Pagination            *fusedobject.PaginationConfig
	EventExtractionPath   *string
	IncomingWebhookConfig *fusedobject.IncomingWebhookConfig
	// BaseURL is this workspace's local override for a wrong or missing
	// spec-derived base_url -- see LocalObjectCache.applyExecutionPolicyOverride,
	// which is where this takes effect.
	BaseURL   *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type WorkspaceExecutionPolicyRef struct {
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
}

type WorkspaceExecutionPolicyBatchStore interface {
	GetEffectiveWorkspaceExecutionPolicyOverrides(ctx context.Context, refs []WorkspaceExecutionPolicyRef) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error)
}

// WorkspaceExecutionPolicyStore is kept separate from Store for the same
// staged-rollout reason as WorkspaceProfileStore: the consolidated
// resolution point (and the plan-action wiring that writes overrides) can
// depend on this narrow surface without widening Store and every existing
// test double.
type WorkspaceExecutionPolicyStore interface {
	// UpsertWorkspaceExecutionPolicyOverride creates or updates the override row
	// for override.ServiceID (+ override.ServiceVersionID, if set). Callers
	// never choose between create and update -- editing a workspace's local
	// execution policy is always an upsert, same product rule as connection
	// profile overrides.
	UpsertWorkspaceExecutionPolicyOverride(ctx context.Context, override WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error)
	// GetEffectiveWorkspaceExecutionPolicyOverride resolves the
	// version-override-if-present-else-service-default precedence in one SQL
	// query, mirroring GetEffectiveWorkspaceProfile. Returns (nil, nil) when no
	// override exists at either tier -- the caller falls back to the runtime
	// contract snapshot in that case.
	GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceExecutionPolicyOverride, error)
	// ResetWorkspaceExecutionPolicyOverride deletes the override row at the
	// given tier (serviceVersionID nil targets the service-default row, set
	// targets that version's row). A no-op, not an error, if no row exists.
	ResetWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID uuid.UUID, serviceVersionID *uuid.UUID) error
}

type WorkspaceSecretMeta struct {
	ID             uuid.UUID
	BucketID       uuid.UUID
	ServiceID      uuid.UUID
	KeyName        string
	KeyNames       []string
	CredentialType string
	LastUsedAt     *time.Time
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type WorkspaceSecret struct {
	WorkspaceSecretMeta
	EncryptedDEK   string
	EncryptedValue string
}

type ConnectConfig struct {
	ID                    uuid.UUID
	BucketID              uuid.UUID
	ServiceID             uuid.UUID
	AuthType              string
	Enabled               bool
	EncryptedDEK          string
	EncryptedClientID     string
	EncryptedClientSecret string
	RedirectURI           string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// WorkspaceConnectConfig adds the bucket name needed by declarative config
// export without teaching API projection code how to join storage identities.
type WorkspaceConnectConfig struct {
	ConnectConfig
	BucketName string
}

// WorkspaceConnectSyncStore groups the two workspace-scoped exports used by
// declarative sync. Keeping them as one capability lets wrappers preserve the
// fixed-query contract without widening every focused Store test double.
type WorkspaceConnectSyncStore interface {
	ListWorkspaceConnectConfigs(ctx context.Context) ([]WorkspaceConnectConfig, error)
	// ListWorkspaceConnectProfiles returns the effective (override-if-present-
	// else-baseline) profile for every active service version in the
	// workspace, in one query. CLI sync (Batch 6) reads this independently of
	// bucket connect configs, since profiles are no longer bucket-owned.
	ListWorkspaceConnectProfiles(ctx context.Context) ([]WorkspaceConnectionProfile, error)
}

type AuthConnection struct {
	ID                    uuid.UUID
	BucketID              uuid.UUID
	ServiceID             uuid.UUID
	EndUserRef            string
	CreatedByArtifactID   uuid.UUID
	AuthType              string
	EncryptedDEK          string
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	EncryptedIDToken      string
	TokenType             string
	Scopes                []string
	ScopeSource           string
	Issuer                string
	Subject               string
	IdentityClaims        []byte
	ExpiresAt             *time.Time
	RefreshTokenExpiresAt *time.Time
	LastUsedAt            *time.Time
	RefreshState          string
	// Failure metadata is deliberately limited to stable codes and OTEL
	// correlation; raw provider responses and user identifiers do not belong here.
	LastFailureCode    string
	LastFailureAt      *time.Time
	LastFailureTraceID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ConnectSession struct {
	ID                    uuid.UUID
	BucketID              uuid.UUID
	ServiceID             uuid.UUID
	EndUserRef            string
	StateHash             string
	NonceHash             string
	EncryptedDEK          string
	EncryptedPKCEVerifier string
	CreatedByArtifactID   uuid.UUID
	ReturnURL             string
	ResourceInputJSON     []byte
	RequestedScopes       []string
	ExpiresAt             time.Time
	UsedAt                *time.Time
	CreatedAt             time.Time
}

// ConnectionResource is non-secret provider context discovered for one
// connected user. Provider tokens remain exclusively on AuthConnection.
type ConnectionResource struct {
	ID                 uuid.UUID
	ConnectionID       uuid.UUID
	BucketID           uuid.UUID
	ServiceID          uuid.UUID
	ProviderResourceID string
	ResourceType       string
	DisplayName        string
	BaseURL            string
	MetadataJSON       []byte
	Scopes             []string
	IsDefault          bool
	IsActive           bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Store interface {
	// BootstrapWorkspace initializes the Engine's one local workspace after a
	// successful Registry handshake. It is idempotent for the owning account
	// and returns ErrWorkspaceOwnerMismatch for any other account.
	BootstrapWorkspace(ctx context.Context, accountID uuid.UUID, name string) (uuid.UUID, error)
	// AddWorkspaceServiceVersion captures
	// the cached service name (for offline resilience) and the account that
	// triggered the add (for the compliance audit trail).
	AddWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, serviceSlug string, version string, serviceVersionID uuid.UUID, serviceName string, addedBy uuid.UUID) error
	EnableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string, serviceVersionID uuid.UUID, enabledBy uuid.UUID) error
	DisableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string) error
	ListWorkspaceServiceVersions(ctx context.Context, serviceID uuid.UUID) ([]WorkspaceServiceVersion, error)
	// ListWorkspaceServiceVersionsForServices is ListWorkspaceServiceVersions's batched
	// sibling: workspace/SDK config plan callers that need every enabled
	// service version call this once instead of looping
	// ListWorkspaceServiceVersions per service.
	ListWorkspaceServiceVersionsForServices(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID][]WorkspaceServiceVersion, error)
	GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error)
	// GetLatestWorkspaceServiceVersionID is GetLatestWorkspaceServiceVersion's
	// sibling returning the service_version_id UUID instead of the version
	// name -- callers that need to key a snapshot/cache lookup (which is
	// keyed by service_version_id, not name) use this one.
	GetLatestWorkspaceServiceVersionID(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (uuid.UUID, error)
	GetLatestWorkspaceServiceVersionByWorkspace(ctx context.Context, serviceID uuid.UUID) (string, error)
	GetLatestWorkspaceServiceVersionIDByWorkspace(ctx context.Context, serviceID uuid.UUID) (uuid.UUID, error)
	SaveArtifactScope(ctx context.Context, scope ArtifactScope) error
	GetArtifactScope(ctx context.Context, artifactID uuid.UUID) (*ArtifactScope, error)
	DeleteArtifactScope(ctx context.Context, accountID uuid.UUID, artifactID uuid.UUID) error
	// DeactivateSDK/ReactivateSDK toggle ArtifactScope.DeactivatedAt. Both are
	// idempotent -- deactivating an already-deactivated SDK (or reactivating
	// an already-active one) succeeds without error, since the caller is
	// declaring a desired end state, not applying a transition.
	DeactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error
	ReactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error
	GetSDKAccountID(ctx context.Context, artifactID uuid.UUID) (uuid.UUID, error)
	// ListMCPScopesByAccount is the read side of the MCP servers list page:
	// paginated kind='mcp' scopes for accountID, newest first, plus the total
	// count. Mirrors the Registry's removed sdks(target_type: "mcp") GraphQL
	// query, but scoped to Engine-native scopes (SaveArtifactScope's kind column)
	// instead of Registry-generated SDK rows.
	ListMCPScopesByAccount(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]ArtifactScope, int, error)
	ListAuthorizedMCPScopesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]ArtifactScope, int, error)

	// GetMCPScopeByName looks up a specific MCP server by name, and optionally
	// by version. If version is empty, it returns the most recently created one.
	GetMCPScopeByName(ctx context.Context, accountID uuid.UUID, name, version string) (*ArtifactScope, error)

	// GetMCPAnalyticsDashboard aggregates canonical execution events and MCP sessions
	// for one SDK into the shape the MCP analytics page renders (overall
	// totals, per-tool and per-service breakdowns, active session count, and
	// recent sessions).
	GetMCPAnalyticsDashboard(ctx context.Context, artifactID uuid.UUID) (*models.MCPAnalyticsDashboard, error)

	// SDK Token methods
	CreateSDKToken(ctx context.Context, artifactID uuid.UUID, tokenHash, name string) (*SDKToken, error)
	ListSDKTokens(ctx context.Context, artifactID uuid.UUID) ([]SDKToken, error)
	RevokeSDKToken(ctx context.Context, artifactID uuid.UUID, name string) error
	GetArtifactByToken(ctx context.Context, tokenHash string) (*ArtifactScope, error)
	ValidateToken(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error)

	// Bucket methods
	CreateBucket(ctx context.Context, name string, isDefault bool) (*Bucket, error)
	ListBuckets(ctx context.Context) ([]Bucket, error)
	GetBucketsByNames(ctx context.Context, names []string) ([]Bucket, error)
	ListBucketSummaries(ctx context.Context, limit, offset int) ([]BucketSummary, int, error)
	ListAuthorizedBucketSummaries(ctx context.Context, scope accesscontrol.AuthorizedScope, limit, offset int) ([]BucketSummary, int, error)
	GetBucketSummary(ctx context.Context, bucketID uuid.UUID) (*BucketSummary, error)
	GetBucket(ctx context.Context, bucketID uuid.UUID) (*Bucket, error)
	GetBucketByName(ctx context.Context, name string) (*Bucket, error)
	DeleteBucket(ctx context.Context, name string, authorizedBucketID uuid.UUID) error

	// SDK Bucket Link methods
	ListBucketsForSDK(ctx context.Context, artifactID uuid.UUID) ([]Bucket, error)
	ListAuthorizedBucketsForSDK(ctx context.Context, artifactID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]Bucket, error)
	ListArtifactScopesForBucket(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]ArtifactScope, int, error)
	ListAuthorizedArtifactScopesForBucket(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]ArtifactScope, int, error)

	// Bucket Value methods
	UpsertBucketValue(ctx context.Context, val BucketValue) error
	GetBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]BucketValue, error)
	ListBucketValues(ctx context.Context, bucketID uuid.UUID) ([]BucketValue, error)
	ListBucketValuePage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]BucketValue, int, error)
	ListBucketValuesForBuckets(ctx context.Context, bucketIDs []uuid.UUID) ([]BucketValue, error)
	DeleteBucketValue(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error

	// Workspace Secret methods
	UpsertSecret(ctx context.Context, secret WorkspaceSecret) error
	// UpsertSecrets is the transactional sibling for credential families that
	// must rotate together, such as mTLS cert/key and basic username/password.
	UpsertSecrets(ctx context.Context, secrets []WorkspaceSecret) error
	DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error
	DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error
	ListSecretMeta(ctx context.Context, bucketID uuid.UUID) ([]WorkspaceSecretMeta, error)
	ListSecretMetaPage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]WorkspaceSecretMeta, int, error)
	// ListSecretsForBucket loads the secrets for a specific bucket and service.
	ListSecretsForBucket(ctx context.Context, bucketID, serviceID uuid.UUID) ([]WorkspaceSecret, error)
	ListSecretsForBuckets(ctx context.Context, bucketIDs []uuid.UUID, serviceID uuid.UUID) ([]WorkspaceSecret, error)
	// GetSecret fetches a single secret by its natural key (bucket + service + key_name).
	// Intended for point lookups where artifact_id is irrelevant (e.g. webhook signing keys).
	GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*WorkspaceSecret, error)
	// GetSecrets fetches a bounded exact key set for hot-path auth resolution.
	GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, error)

	// Connect auth methods. User auth is bucket-attached rather than SDK-
	// attached so newly generated SDKs can reuse connected users by linking to
	// the same credential bucket.
	UpsertConnectConfig(ctx context.Context, cfg ConnectConfig) (*ConnectConfig, error)
	GetConnectConfig(ctx context.Context, bucketID, serviceID uuid.UUID) (*ConnectConfig, error)
	ListConnectConfigsForBucket(ctx context.Context, bucketID uuid.UUID) ([]ConnectConfig, error)
	ListConnectConfigsForService(ctx context.Context, serviceID uuid.UUID) ([]ConnectConfig, error)
	GetBucketConnectSummary(ctx context.Context, bucketID uuid.UUID) (*BucketConnectSummary, error)
	UpsertAuthConnection(ctx context.Context, conn AuthConnection) (*AuthConnection, error)
	GetAuthConnection(ctx context.Context, bucketID, serviceID uuid.UUID, endUserRef string) (*AuthConnection, error)
	GetAuthConnectionByIDForBuckets(ctx context.Context, id uuid.UUID, bucketIDs []uuid.UUID) (*AuthConnection, error)
	GetAuthConnectionsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AuthConnection, error)
	ListAuthConnections(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string) ([]AuthConnection, error)
	ListAuthConnectionsPage(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string, limit, offset int) ([]AuthConnection, int, error)
	DeleteAuthConnection(ctx context.Context, bucketID, id uuid.UUID) error
	TouchAuthConnectionLastUsed(ctx context.Context, id uuid.UUID, usedAt time.Time) error
	// RecordAuthConnectionFailure updates only sanitized operational metadata so
	// observing a provider response cannot overwrite connection credentials.
	RecordAuthConnectionFailure(ctx context.Context, id uuid.UUID, code, traceID string, failedAt time.Time) error
	ListAuthConnectionsNeedingRefresh(ctx context.Context, cutoff time.Time, limit int) ([]AuthConnection, error)
	CreateConnectSession(ctx context.Context, session ConnectSession) (*ConnectSession, error)
	GetConnectSessionByStateHash(ctx context.Context, stateHash string) (*ConnectSession, error)
	MarkConnectSessionUsed(ctx context.Context, stateHash string, usedAt time.Time) error
	DeleteExpiredConnectSessions(ctx context.Context, before time.Time) (int64, error)
	// ReconcileConnectionResources atomically upserts the latest authoritative
	// discovery result and deactivates resources the provider no longer returns.
	ReconcileConnectionResources(ctx context.Context, connectionID uuid.UUID, resources []ConnectionResource) ([]ConnectionResource, error)
	GetConnectionResourceForExecution(ctx context.Context, connectionID uuid.UUID, resourceID *uuid.UUID) (*ConnectionResource, int, error)
	ListConnectionResources(ctx context.Context, connectionID uuid.UUID) ([]ConnectionResource, error)
	SetDefaultConnectionResource(ctx context.Context, connectionID, resourceID uuid.UUID) (*ConnectionResource, error)

	// GetWorkspaceIDForAccount resolves the singleton workspace without side
	// effects and verifies that it belongs to the authenticated account.
	VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error

	// ListWorkspaceServices returns all services the workspace has added.
	// Used heavily internally when pagination is unnecessary.
	ListWorkspaceServices(ctx context.Context, names []string) ([]WorkspaceService, error)
	ListAuthorizedWorkspaceServices(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string) ([]WorkspaceService, error)

	// ListWorkspaceServicesPage returns a paginated slice of workspace services
	// along with the total count matching the names filter.
	ListWorkspaceServicesPage(ctx context.Context, names []string, limit, offset int) ([]WorkspaceService, int, error)
	ListAuthorizedWorkspaceServicesPage(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string, limit, offset int) ([]WorkspaceService, int, error)
	ResolveWorkspaceServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error)
	ListBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, search string, limit, offset int) ([]BucketServiceSummary, int, error)
	ListAuthorizedBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, search string, limit, offset int) ([]BucketServiceSummary, int, error)
	// RemoveWorkspaceService removes the workspace-service row.
	// Returns ErrWorkspaceServiceNotFound if no such row exists.
	RemoveWorkspaceService(ctx context.Context, serviceID uuid.UUID) error
	// IsWorkspaceServiceEnabled is a targeted single-row existence check -- not
	// ListWorkspaceServices's full fetch -- for callers that only need a yes/no
	// answer (the import/apply auto-register intercept, and the /sdks/generate
	// workspace gate; engine_workspace_registration_plan.md, Task 2).
	IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error)

	// GetWorkspaceWebhookBySlug is the Engine's webhook ingress lookup -- the
	// single indexed read that replaces the old NATS-to-Registry round trip.
	GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*WorkspaceWebhook, error)
	// ListWorkspaceWebhooks returns every registration a workspace holds for
	// one service, for CLI/visibility output.
	ListWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID) ([]WorkspaceWebhook, error)
	// WorkspaceWebhookOwnersByLabel resolves, in one query, which config_key
	// (if any) already owns the (service_id, label) pair for every service in
	// serviceIDs -- used by kind: webhook's plan step to detect a conflict
	// (another artifact already claiming this artifact's name for one of its
	// services) without
	// a query per service. A service_id absent from the returned map has no
	// existing registration for this label at all.
	WorkspaceWebhookOwnersByLabel(ctx context.Context, serviceIDs []uuid.UUID, label string) (map[uuid.UUID]string, error)

	UpsertMCPSession(ctx context.Context, session *models.MCPSession) error
	BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error
	DeleteEngineExecutionEventsBefore(ctx context.Context, before time.Time, limit int) (int64, error)
	ListEngineExecutionEventsByService(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error)
	GetEngineExecutionAnalyticsByService(ctx context.Context, filter EngineExecutionFilter) (models.EngineExecutionAnalytics, error)
	GetWorkspaceExecutionAnalytics(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time) (models.WorkspaceExecutionAnalytics, error)
	ListUnprojectedPublicInsightServiceIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error)
	ProjectPublicServiceInsightReports(ctx context.Context, reportableServiceIDs []uuid.UUID, before time.Time, eventLimit int) (int64, error)
	ListPendingPublicServiceInsightReports(ctx context.Context, limit int, now time.Time) ([]models.PublicServiceInsightReport, error)
	MarkPublicServiceInsightReportResults(ctx context.Context, results []models.PublicServiceInsightReportResult, at time.Time) error
	MarkPublicServiceInsightReportDeliveryFailure(ctx context.Context, reportIDs []uuid.UUID, errorCode string, at time.Time) error

	// Webhook activity is projected from the canonical execution-event table.
	// Both reads keep account and service scoping in SQL so tenant isolation is
	// enforced by the data access boundary, not by filtering returned rows.
	ListWebhookEventsByService(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, limit, offset int, startDate, endDate *time.Time) ([]models.WebhookEvent, int64, error)
	GetWebhookAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) (models.WebhookAnalytics, error)

	// GetIdempotentExecution looks up a cached response for (artifactID,
	// idempotencyKeyHash). Returns ErrIdempotentExecutionNotFound if there's
	// no unexpired row, or ErrIdempotencyKeyConflict if a row exists but its
	// stored request body hash doesn't match requestBodyHash (both non-empty).
	GetIdempotentExecution(ctx context.Context, artifactID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error)
	// SaveIdempotentExecution caches a successful execution's response for
	// later replay. Concurrent duplicate writes for the same key are
	// harmless: the first one to land wins, later ones are no-ops.
	SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error

	// GetServiceChangelogCursor returns the poll cursor for one service --
	// see plans/plan-service-changelog.md's "## Phase 2" for why this is one
	// row per service (an explicit, deliberate trade-off) rather than a
	// single global cursor. Returns the epoch (matching the column's own
	// DEFAULT 'epoch') if no row exists yet, so a newly activated service's
	// first-ever poll fetches its entire history instead of erroring.
	GetServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID) (time.Time, error)
	// UpsertServiceChangelogCursor advances one service's poll cursor.
	// Callers must pass the max registry_created_at among the rows actually
	// returned by that poll, never wall-clock time -- using wall-clock time
	// could skip a row whose async insert on the Registry side lands after
	// the poll but is timestamped before it (see the plan doc's race
	// explanation).
	UpsertServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID, lastCheckedAt time.Time) error
	// InsertServiceChangelogCacheEntries idempotently inserts fetched rows
	// into fused_service_changelog_cache, keyed on registry_changelog_id
	// (entry.ID) -- ON CONFLICT DO NOTHING, so re-fetching the same row after
	// a crash between insert and cursor advance never duplicates it.
	InsertServiceChangelogCacheEntries(ctx context.Context, entries []models.ServiceChangelogEntry) error
}
