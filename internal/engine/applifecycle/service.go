// Package applifecycle provides the shared SDK/MCP lifecycle service.
// It enforces version immutability, capability-expansion detection,
// deprecation/deactivation, and family-token authorization.
//
// SDK and MCP share one lifecycle model; the only divergence is SDK package
// generation, which is handled by an adapter behind this service.
package applifecycle

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/capability"
)

var ErrTokenPolicyInvalid = errors.New("invalid app token policy")

// LifecycleOutcome is the bounded telemetry vocabulary shared by lifecycle
// operations and their transport adapters.
type LifecycleOutcome string

const (
	OutcomeUnauthorized     LifecycleOutcome = "unauthorized"
	OutcomeInvalid          LifecycleOutcome = "invalid"
	OutcomeFailed           LifecycleOutcome = "failed"
	OutcomeSuccess          LifecycleOutcome = "success"
	OutcomeConflict         LifecycleOutcome = "conflict"
	OutcomeExisting         LifecycleOutcome = "existing"
	OutcomeCreated          LifecycleOutcome = "created"
	OutcomeVersionImmutable LifecycleOutcome = "version_immutable"
	OutcomeNoop             LifecycleOutcome = "noop"
	OutcomeDeprecated       LifecycleOutcome = "deprecated"
	OutcomeActive           LifecycleOutcome = "active"
	OutcomeDeactivated      LifecycleOutcome = "deactivated"
	OutcomeRevoked          LifecycleOutcome = "revoked"
)

// Service orchestrates app-family and app-version operations. It is the
// single coordination point for SDK and MCP lifecycle; callers provide a
// kind-specific adapter for package generation (SDK) or runtime setup (MCP).
type Service struct {
	store Repository
}

// Repository is the lifecycle-owned persistence surface. Keeping it narrower
// than store.Store prevents lifecycle decisions from becoming coupled to
// unrelated workspace, analytics, or credential storage concerns.
type Repository interface {
	CreateOrGetAppFamily(context.Context, store.AppFamily) (*store.AppFamily, bool, error)
	PublishAppVersion(context.Context, store.App) (*store.App, bool, error)
	AssessAppCapabilityExpansion(context.Context, uuid.UUID, []string) (bool, int, error)
	DeprecateApp(context.Context, uuid.UUID, string, *time.Time) error
	UndeprecateApp(context.Context, uuid.UUID) error
	GetApp(context.Context, uuid.UUID) (*store.App, error)
	DeactivateAppVersion(context.Context, uuid.UUID, uuid.UUID) error
	CreateAppToken(context.Context, store.AppTokenIssue) (*store.AppTokenMetadata, error)
}

// ConfigPlanRepository is the atomic desired-state apply boundary. The SQL
// implementation owns the transaction; the lifecycle service owns the
// user-triggered operation and its safe telemetry.
type ConfigPlanRepository interface {
	ApplyAppConfigPlan(context.Context, store.ApplyAppConfigPlanParams) (*store.ApplyAppConfigPlanResult, error)
}

func New(s Repository) *Service {
	return &Service{store: s}
}

// ApplyConfigPlan finalizes one SDK or MCP version through the shared app
// lifecycle while preserving the repository's single PostgreSQL transaction.
func (svc *Service) ApplyConfigPlan(ctx context.Context, repository ConfigPlanRepository, params store.ApplyAppConfigPlanParams) (*store.ApplyAppConfigPlanResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.apply_config_plan")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.id", params.Scope.AppID.String()),
		attribute.String("app.kind", params.Scope.Kind.String()),
		attribute.String("app.version", params.Scope.Version),
	)
	if !params.Scope.Kind.Valid() {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return nil, store.ErrAppKindInvalid
	}
	result, err := repository.ApplyAppConfigPlan(ctx, params)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return nil, err
	}
	span.SetAttributes(
		attribute.String("outcome", string(OutcomeSuccess)),
		attribute.Bool("app.version_created", result.VersionCreated),
		attribute.Bool("app.token_created", result.TokenCreated),
	)
	return result, nil
}

// --- Family and version creation ---

// CreateOrGetFamily ensures an app family exists for the given identity.
// If the family already exists, it returns the existing one (idempotent).
// The caller must have already canonicalized the name.
func (svc *Service) CreateOrGetFamily(ctx context.Context, params CreateFamilyParams) (*store.AppFamily, bool, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.create_family")
	defer span.End()
	span.SetAttributes(attribute.String("app.kind", params.Kind.String()))
	if !params.Kind.Valid() {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return nil, false, store.ErrAppKindInvalid
	}

	family := store.AppFamily{
		AppFamilyID:    uuid.New(),
		AccountID:      params.AccountID,
		Kind:           params.Kind,
		CanonicalName:  params.CanonicalName,
		DisplayName:    params.DisplayName,
		TargetLanguage: params.TargetLanguage,
		OwnerSubjectID: params.OwnerSubjectID,
		OwnerTeamID:    params.OwnerTeamID,
	}
	result, created, err := svc.store.CreateOrGetAppFamily(ctx, family)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return nil, false, fmt.Errorf("create or get family: %w", err)
	}
	if !result.HasSameBinding(family) {
		span.SetAttributes(attribute.String("outcome", string(OutcomeConflict)))
		return nil, false, store.ErrAppOwnerMismatch
	}
	outcome := OutcomeExisting
	if created {
		outcome = OutcomeCreated
	}
	span.SetAttributes(
		attribute.String("app.family_id", result.AppFamilyID.String()),
		attribute.String("outcome", string(outcome)),
		attribute.Bool("family.created", created),
	)
	return result, created, nil
}

// PublishVersion creates a new immutable app version within a family.
// It enforces version immutability: same family+version+source_hash is a
// no-op; same family+version+different source_hash is rejected.
func (svc *Service) PublishVersion(ctx context.Context, params PublishVersionParams) (*PublishVersionResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.publish_version")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.kind", params.Kind.String()),
		attribute.String("app.version", params.Version),
	)
	if !params.Kind.Valid() {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return nil, store.ErrAppKindInvalid
	}

	capabilityKeys, capHash, err := capability.KeysAndHash(params.Selections)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return nil, fmt.Errorf("publish version: decode capabilities: %w", err)
	}

	app := store.App{
		AppID:              params.AppID,
		AppFamilyID:        params.AppFamilyID,
		AccountID:          params.AccountID,
		Version:            params.Version,
		ConfigKey:          params.ConfigKey,
		SourceHash:         params.SourceHash,
		CapabilityHash:     capHash,
		CapabilityKeys:     capabilityKeys,
		ScopeSchemaVersion: params.ScopeSchemaVersion,
		Selections:         params.Selections,
		GeneratorVersion:   params.GeneratorVersion,
		Status:             store.AppStatusActive,
		CreatedBy:          params.CreatedBy,
		ExpectedFamilyKind: params.Kind,
	}

	persisted, created, err := svc.store.PublishAppVersion(ctx, app)
	if err != nil {
		if errors.Is(err, store.ErrAppVersionImmutable) {
			span.SetAttributes(attribute.String("outcome", string(OutcomeVersionImmutable)))
		} else {
			span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		}
		return nil, err
	}
	if !created {
		span.SetAttributes(attribute.String("outcome", string(OutcomeNoop)), attribute.Bool("app.noop", true))
		return &PublishVersionResult{App: *persisted, NoOp: true}, nil
	}

	span.SetAttributes(
		attribute.String("outcome", string(OutcomeCreated)),
		attribute.Bool("app.created", true),
		attribute.String("app.id", app.AppID.String()),
	)
	return &PublishVersionResult{
		App:     *persisted,
		Created: true,
	}, nil
}

// CapabilityExpansion detects whether a new version expands the family's
// capability relative to any existing version. Called before publishing so
// plan output can show the diff.
func (svc *Service) CapabilityExpansion(ctx context.Context, appFamilyID uuid.UUID, selections []byte) (*CapabilityExpansionResult, error) {
	keys, err := capability.Keys(selections)
	if err != nil {
		return nil, fmt.Errorf("decode capabilities: %w", err)
	}
	expands, tokenCount, err := svc.store.AssessAppCapabilityExpansion(ctx, appFamilyID, keys)
	if err != nil {
		return nil, err
	}
	if !expands {
		tokenCount = 0
	}
	return &CapabilityExpansionResult{Expands: expands, AffectedTokenCount: tokenCount}, nil
}

// --- Lifecycle: deprecation & deactivation ---

// Deprecate marks an app version as deprecated with a user-facing message.
// Deprecated apps remain executable and downloadable.
func (svc *Service) Deprecate(ctx context.Context, appID uuid.UUID, message string, plannedDeactivationAt *time.Time) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.deprecate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	if err := svc.store.DeprecateApp(ctx, appID, message, plannedDeactivationAt); err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return fmt.Errorf("deprecate: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", string(OutcomeDeprecated)))
	return nil
}

// Undeprecate restores a deprecated app to active status.
func (svc *Service) Undeprecate(ctx context.Context, appID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.undeprecate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	if err := svc.store.UndeprecateApp(ctx, appID); err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return fmt.Errorf("undeprecate: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", string(OutcomeActive)))
	return nil
}

// Deactivate permanently removes an app version. Persistence writes the
// tombstone, ends MCP sessions, clears ephemeral execution state, and deletes
// the executable app atomically.
//
// This is irreversible: the tombstone prevents reusing the (family, version)
// pair.
func (svc *Service) Deactivate(ctx context.Context, appID uuid.UUID, actorID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.deactivate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	app, err := svc.store.GetApp(ctx, appID)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return fmt.Errorf("get app for deactivation: %w", err)
	}
	if err := svc.store.DeactivateAppVersion(ctx, appID, actorID); err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return fmt.Errorf("deactivate app: %w", err)
	}

	span.SetAttributes(
		attribute.String("app.family_id", app.AppFamilyID.String()),
		attribute.String("app.version", app.Version),
		attribute.String("outcome", string(OutcomeDeactivated)),
	)
	return nil
}

// --- Token management ---

// NewExecutionToken returns one cryptographically random family-scoped token
// and the hash that may be persisted. SDK and MCP use the same token shape
// because the family, not the runtime adapter, is the authorization boundary.
func NewExecutionToken() (plaintext, tokenHash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext = "fused-app-" + base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, auth.HashToken(plaintext), nil
}

// GenerateToken creates a new family-scoped token. It returns the plaintext
// exactly once; the caller is responsible for delivering it to the user.
func (svc *Service) GenerateToken(ctx context.Context, params GenerateTokenParams) (plaintext string, token *store.AppTokenMetadata, err error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.generate_token")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", params.AppFamilyID.String()))

	policy, err := resolveTokenPolicy(params.Allow, params.ExpiresIn, time.Now())
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return "", nil, err
	}
	span.SetAttributes(
		attribute.Bool("app.token.allow_all", policy.AllowAll),
		attribute.Bool("app.token.expiry_present", policy.ExpiresAt != nil),
	)
	bindingMode, err := resolveTokenBindingMode(params.BindingMode, params.Bindings)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeInvalid)))
		return "", nil, err
	}
	// Counts and mode are enough to debug issuance without copying connected-user
	// selectors or Engine-owned connection identifiers into telemetry.
	span.SetAttributes(
		attribute.String("app.token.binding_mode", string(bindingMode)),
		attribute.Int("app.token.binding_count", len(params.Bindings)),
	)

	plaintext, tokenHash, err := NewExecutionToken()
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return "", nil, fmt.Errorf("generate app token: %w", err)
	}

	tok, err := svc.store.CreateAppToken(ctx, store.AppTokenIssue{
		ID:                   uuid.New(),
		AppFamilyID:          params.AppFamilyID,
		TokenHash:            tokenHash,
		Name:                 params.Name,
		Policy:               policy,
		BindingMode:          bindingMode,
		Bindings:             params.Bindings,
		IssuedBySubjectID:    params.IssuedBySubjectID,
		IssuedByCredentialID: params.IssuedByCredentialID,
	})
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(OutcomeFailed)))
		return "", nil, fmt.Errorf("create app token: %w", err)
	}

	span.SetAttributes(
		attribute.String("outcome", string(OutcomeCreated)),
	)
	return plaintext, tok, nil
}

func resolveTokenBindingMode(mode store.AppTokenBindingMode, bindings []store.AppTokenBindingRequest) (store.AppTokenBindingMode, error) {
	if mode == "" {
		if len(bindings) > 0 {
			return store.AppTokenBindingFixed, nil
		}
		return store.AppTokenBindingDynamic, nil
	}
	if !mode.Valid() {
		return "", fmt.Errorf("%w: invalid binding_mode", ErrTokenPolicyInvalid)
	}
	if mode == store.AppTokenBindingDynamic && len(bindings) > 0 {
		return "", fmt.Errorf("%w: dynamic tokens cannot declare fixed bindings", ErrTokenPolicyInvalid)
	}
	if mode == store.AppTokenBindingFixed && len(bindings) == 0 {
		return "", fmt.Errorf("%w: fixed tokens require at least one binding", ErrTokenPolicyInvalid)
	}
	return mode, nil
}

// FullAccessTokenPolicy is the apply-time default. Keeping it in the lifecycle
// domain prevents SDK and MCP adapters from inventing different token defaults.
func FullAccessTokenPolicy() store.AppTokenPolicy {
	return store.AppTokenPolicy{AllowAll: true, AllowedOperations: []string{}}
}

func resolveTokenPolicy(allow []string, expiresIn *time.Duration, now time.Time) (store.AppTokenPolicy, error) {
	allowAll, operations, err := normalizeTokenAllow(allow)
	if err != nil {
		return store.AppTokenPolicy{}, err
	}
	policy := store.AppTokenPolicy{AllowAll: allowAll, AllowedOperations: operations}
	if expiresIn == nil {
		return policy, nil
	}
	if *expiresIn <= 0 {
		return store.AppTokenPolicy{}, fmt.Errorf("%w: expires_in must be positive", ErrTokenPolicyInvalid)
	}
	expiresAt := now.Add(*expiresIn).UTC()
	if !expiresAt.After(now) {
		return store.AppTokenPolicy{}, fmt.Errorf("%w: expires_in is out of range", ErrTokenPolicyInvalid)
	}
	policy.ExpiresAt = &expiresAt
	return policy, nil
}

func normalizeTokenAllow(allow []string) (bool, []string, error) {
	if allow == nil {
		return true, []string{}, nil
	}
	if len(allow) == 0 {
		return false, nil, fmt.Errorf("%w: allow must contain at least one operation or *", ErrTokenPolicyInvalid)
	}
	unique := make(map[string]struct{}, len(allow))
	for _, raw := range allow {
		operation := strings.TrimSpace(raw)
		if operation == "" {
			return false, nil, fmt.Errorf("%w: allow entries must not be empty", ErrTokenPolicyInvalid)
		}
		unique[operation] = struct{}{}
	}
	if _, wildcard := unique[store.AppTokenAllowAllWildcard]; wildcard {
		if len(unique) != 1 {
			return false, nil, fmt.Errorf("%w: * must be the only allow entry", ErrTokenPolicyInvalid)
		}
		return true, []string{}, nil
	}
	operations := make([]string, 0, len(unique))
	for operation := range unique {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	return false, operations, nil
}

// --- Helpers ---

// --- Request/response types ---

// CreateFamilyParams contains the data needed to create or look up an app family.
type CreateFamilyParams struct {
	AccountID      uuid.UUID
	Kind           store.AppKind
	CanonicalName  string
	DisplayName    string
	TargetLanguage string
	OwnerSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
}

// GenerateTokenParams is adapter-neutral. MCP exposes scope/expiry controls;
// SDK callers currently use the default policy through the same service.
type GenerateTokenParams struct {
	AppFamilyID          uuid.UUID
	Name                 string
	Allow                []string
	ExpiresIn            *time.Duration
	BindingMode          store.AppTokenBindingMode
	Bindings             []store.AppTokenBindingRequest
	IssuedBySubjectID    *uuid.UUID
	IssuedByCredentialID *uuid.UUID
}

// PublishVersionParams contains the immutable version data.
type PublishVersionParams struct {
	AppFamilyID        uuid.UUID
	AccountID          uuid.UUID
	AppID              uuid.UUID
	Kind               store.AppKind
	Version            string
	ConfigKey          string
	SourceHash         string
	Selections         []byte
	ScopeSchemaVersion int
	GeneratorVersion   string
	CreatedBy          uuid.UUID
}

// PublishVersionResult summarizes what happened during publish.
type PublishVersionResult struct {
	App     store.App
	Created bool
	NoOp    bool // same version + same source = idempotent no-op
}

// CapabilityExpansionResult describes whether a new version expands the
// family's capability.
type CapabilityExpansionResult struct {
	Expands            bool `json:"expands"`
	AffectedTokenCount int  `json:"affected_token_count,omitempty"`
}

// Ensure our errors wrap the store errors for callers that check with errors.Is.
var (
	ErrAppVersionImmutable = store.ErrAppVersionImmutable
	ErrAppTombstoneExists  = store.ErrAppTombstoneExists
	ErrAppNotFound         = store.ErrAppNotFound
	ErrAppFamilyNotFound   = store.ErrAppFamilyNotFound
)

// Compile-time check that canonical package is correctly used.
var _ = canonical.AppName
