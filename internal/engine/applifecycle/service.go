// Package applifecycle provides the shared SDK/MCP lifecycle service.
// It enforces version immutability, capability-expansion detection,
// deprecation/deactivation, and family-token authorization.
//
// SDK and MCP share one lifecycle model; the only divergence is SDK package
// generation, which is handled by an adapter behind this service.
package applifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonical"
	"github.com/Usefused/engine/internal/shared/capability"
)

// Service orchestrates app-family and app-version operations. It is the
// single coordination point for SDK and MCP lifecycle; callers provide a
// kind-specific adapter for package generation (SDK) or runtime setup (MCP).
type Service struct {
	store store.Store
}

func New(s store.Store) *Service {
	return &Service{store: s}
}

// --- Family and version creation ---

// CreateOrGetFamily ensures an app family exists for the given identity.
// If the family already exists, it returns the existing one (idempotent).
// The caller must have already canonicalized the name.
func (svc *Service) CreateOrGetFamily(ctx context.Context, params CreateFamilyParams) (*store.AppFamily, bool, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.create_family")
	defer span.End()

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
		return nil, false, fmt.Errorf("create or get family: %w", err)
	}
	if err := validateFamilyMatch(*result, family); err != nil {
		return nil, false, err
	}
	span.SetAttributes(attribute.Bool("family.created", created))
	return result, created, nil
}

func validateFamilyMatch(existing, requested store.AppFamily) error {
	if existing.TargetLanguage != requested.TargetLanguage ||
		existing.OwnerSubjectID != requested.OwnerSubjectID ||
		existing.OwnerTeamID != requested.OwnerTeamID {
		return fmt.Errorf("app family identity already exists with different language or owner")
	}
	return nil
}

// PublishVersion creates a new immutable app version within a family.
// It enforces version immutability: same family+version+source_hash is a
// no-op; same family+version+different source_hash is rejected.
func (svc *Service) PublishVersion(ctx context.Context, params PublishVersionParams) (*PublishVersionResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.publish_version")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.kind", params.Kind),
		attribute.String("app.version", params.Version),
	)

	capabilityKeys, err := capability.Keys(params.Selections)
	if err != nil {
		return nil, fmt.Errorf("publish version: decode capabilities: %w", err)
	}
	capHash := hashStrings(capabilityKeys)

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
		Status:             "active",
		CreatedBy:          params.CreatedBy,
	}

	persisted, created, err := svc.store.PublishAppVersion(ctx, app)
	if err != nil {
		if errors.Is(err, store.ErrAppVersionImmutable) {
			span.SetAttributes(attribute.String("outcome", "version_immutable"))
		}
		return nil, err
	}
	if !created {
		span.SetAttributes(attribute.Bool("app.noop", true))
		return &PublishVersionResult{App: *persisted, NoOp: true}, nil
	}

	span.SetAttributes(
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

	if err := svc.store.DeprecateApp(ctx, appID, message, plannedDeactivationAt); err != nil {
		return fmt.Errorf("deprecate: %w", err)
	}
	return nil
}

// Undeprecate restores a deprecated app to active status.
func (svc *Service) Undeprecate(ctx context.Context, appID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.undeprecate")
	defer span.End()

	if err := svc.store.UndeprecateApp(ctx, appID); err != nil {
		return fmt.Errorf("undeprecate: %w", err)
	}
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

	app, err := svc.store.GetApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("get app for deactivation: %w", err)
	}
	if err := svc.store.DeactivateAppVersion(ctx, appID, actorID); err != nil {
		return fmt.Errorf("deactivate app: %w", err)
	}

	span.SetAttributes(
		attribute.String("app.family_id", app.AppFamilyID.String()),
		attribute.String("app.version", app.Version),
	)
	return nil
}

// --- Authorization ---

// HashToken produces the SHA-256 hex digest of a plaintext token.
// The plaintext is returned to the caller exactly once; only the hash is
// stored and compared during authorization.
func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// AuthorizeCall is the one-query authorization for an SDK/MCP runtime call.
// It verifies the token hash against the app's family, loads the full
// AuthProjection, and returns it for use by the execution boundary.
//
// A deactivated app returns ErrAppNotFound (stable denial). A deprecated
// app returns the projection with AppStatus "deprecated" so the boundary
// can surface a warning.
func (svc *Service) AuthorizeCall(ctx context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.authorize")
	defer span.End()

	proj, err := svc.store.AuthorizeApp(ctx, appID, tokenHash)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return nil, err
	}
	return proj, nil
}

// --- Token management ---

// GenerateToken creates a new family-scoped token. It returns the plaintext
// exactly once; the caller is responsible for delivering it to the user.
func (svc *Service) GenerateToken(ctx context.Context, appFamilyID uuid.UUID, name string) (plaintext string, token *store.AppToken, err error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.applifecycle.generate_token")
	defer span.End()

	plaintext = "fused-app-" + uuid.NewString()
	tokenHash := HashToken(plaintext)

	tok, err := svc.store.CreateAppToken(ctx, appFamilyID, tokenHash, name)
	if err != nil {
		return "", nil, fmt.Errorf("create app token: %w", err)
	}

	span.SetAttributes(
		attribute.String("app.family_id", appFamilyID.String()),
		attribute.String("token.name", name),
	)
	return plaintext, tok, nil
}

// --- Helpers ---

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func hashStrings(values []string) string {
	data := []byte(strings.Join(values, "\n"))
	return hashBytes(data)
}

// --- Request/response types ---

// CreateFamilyParams contains the data needed to create or look up an app family.
type CreateFamilyParams struct {
	AccountID      uuid.UUID
	Kind           string
	CanonicalName  string
	DisplayName    string
	TargetLanguage string
	OwnerSubjectID uuid.UUID
	OwnerTeamID    uuid.UUID
}

// PublishVersionParams contains the immutable version data.
type PublishVersionParams struct {
	AppFamilyID        uuid.UUID
	AccountID          uuid.UUID
	AppID              uuid.UUID
	Kind               string
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
