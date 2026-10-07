package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const attachedAppOperationPrefix = "unified_app:"

// AttachedUnifiedApps exposes only the current consumer's admitted public contracts.
func (s *EngineGRPCServer) AttachedUnifiedApps(ctx context.Context, identity auth.RuntimeIdentity) ([]models.UnifiedAppBinding, error) {
	// Only admitted execution adapters can read an immutable dependency scope.
	if identity.Kind != store.AppKindSDK && identity.Kind != store.AppKindMCP && identity.Kind != store.AppKindUnifiedApp {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	repository, ok := s.store.(store.UnifiedAppAttachmentStore)
	if !ok {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	bindings, err := repository.ReadUnifiedAppBindings(ctx, identity.AccountID, identity.AppID)
	if err != nil {
		return nil, err
	}
	allowed := make([]models.UnifiedAppBinding, 0, len(bindings))
	for _, binding := range bindings {
		// Token restrictions apply to each authored alias before any contract is advertised.
		if identity.AllowsOperation(attachedAppOperationPrefix + binding.Alias) {
			allowed = append(allowed, binding)
		}
	}
	return allowed, nil
}

// ExecuteAttachedUnifiedApp delegates one explicitly selected alias through the durable Unified App executor.
func (s *EngineGRPCServer) ExecuteAttachedUnifiedApp(ctx context.Context, identity auth.RuntimeIdentity, alias string, input json.RawMessage) (json.RawMessage, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.attached_execute")
	defer span.End()
	span.SetAttributes(attribute.String("app.consumer_id", identity.AppID.String()))
	// An alias is a named capability, never a user-supplied app ID or credential.
	if !unifiedAppAlias.MatchString(alias) || !identity.AllowsOperation(attachedAppOperationPrefix+alias) {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	selected, err := s.selectedUnifiedAppAttachment(ctx, identity, alias)
	// Missing membership cannot fall back to a workspace-wide app lookup.
	if err != nil {
		return nil, err
	}
	// Hosted callers retain a bounded ancestry before any child can acquire runtime capacity.
	if identity.Kind == store.AppKindUnifiedApp {
		ctx, err = descendUnifiedAppCall(ctx, identity.AppFamilyID, selected.AppFamilyID)
		// Recursion and exhausted budgets must fail before reading or starting the target runtime.
		if err != nil {
			return nil, err
		}
	}
	scope, bundle, manifest, err := s.attachedUnifiedAppTarget(ctx, identity.AccountID, *selected)
	// Target identity and traffic authority must remain live after dependency admission.
	if err != nil {
		return nil, err
	}
	// Plan-time app.use delegates the target's selected provider scope; the caller's token ID retains attribution.
	delegated := auth.RuntimeIdentity{AccountID: identity.AccountID, AppFamilyID: scope.AppFamilyID, AppID: scope.AppID, TokenID: identity.TokenID, AppVersion: scope.Version, Kind: scope.Kind, Status: scope.Status, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	result, requestErr := s.executeCapabilityRun(ctx, capabilityRunSpec{transport: string(identity.Kind), identity: delegated, version: scope.Version, bundle: bundle, manifest: manifest, input: input, mode: "live"})
	// Control-plane details stay private when durable child execution cannot start.
	if requestErr != nil {
		return nil, errors.New("attached Unified App execution unavailable")
	}
	return json.Marshal(result)
}

// selectedUnifiedAppAttachment reads only the authenticated consumer's admitted aliases.
func (s *EngineGRPCServer) selectedUnifiedAppAttachment(ctx context.Context, identity auth.RuntimeIdentity, alias string) (*models.UnifiedAppBinding, error) {
	bindings, err := s.AttachedUnifiedApps(ctx, identity)
	// Storage failure cannot fall back to discovery or caller-supplied identities.
	if err != nil {
		return nil, err
	}
	for i := range bindings {
		// Exact alias membership is the only route to a target version.
		if bindings[i].Alias == alias {
			return &bindings[i], nil
		}
	}
	return nil, sandbox.ErrMCPCapabilityUnavailable
}

// attachedUnifiedAppTarget rechecks account, immutable identity and promotion before starting the child.
func (s *EngineGRPCServer) attachedUnifiedAppTarget(ctx context.Context, accountID uuid.UUID, selected models.UnifiedAppBinding) (*store.AppRuntime, *store.UnifiedAppBundle, *unifiedAppManifest, error) {
	scope, err := s.store.GetAppRuntime(ctx, selected.AppID)
	// Deleted or cross-account runtime state cannot inherit the plan's earlier authority.
	if err != nil || !matchesAttachedUnifiedApp(scope, accountID, selected) {
		return nil, nil, nil, sandbox.ErrMCPCapabilityUnavailable
	}
	// Pinned references never follow a promoted successor implicitly.
	if s.admitUnifiedAppTraffic(ctx, selected.AppID) != nil {
		return nil, nil, nil, errors.New("attached Unified App version is not receiving traffic")
	}
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, selected.AppID)
	// Both the immutable source and its compiled declaration must still match the reviewed binding.
	if requestErr != nil || !found || bundle.SourceHash != selected.SourceHash {
		return nil, nil, nil, sandbox.ErrMCPCapabilityUnavailable
	}
	return scope, bundle, manifest, nil
}

// matchesAttachedUnifiedApp keeps the delegated scope tied to the consumer's exact account and bundle.
func matchesAttachedUnifiedApp(scope *store.AppRuntime, accountID uuid.UUID, selected models.UnifiedAppBinding) bool {
	return scope != nil && scope.AppID == selected.AppID && scope.AccountID == accountID && scope.AppFamilyID == selected.AppFamilyID && scope.Kind == store.AppKindUnifiedApp && scope.Status.Runnable() && scope.BundleDigest == selected.BundleDigest
}

// tryAttachedUnifiedAppRun keeps attachment calls on the authenticated consumer REST endpoint.
func (s *EngineGRPCServer) tryAttachedUnifiedAppRun(w http.ResponseWriter, r *http.Request, identity auth.RuntimeIdentity, decoded restExecutionRequest) bool {
	// Ordinary operations preserve their existing dispatcher and request semantics.
	if !strings.HasPrefix(decoded.Operation, attachedAppOperationPrefix) {
		return false
	}
	if requestErr := validateUnifiedAppRESTControls(decoded); requestErr != nil {
		writeRESTExecutionError(w, requestErr)
		return true
	}
	result, err := s.ExecuteAttachedUnifiedApp(r.Context(), identity, strings.TrimPrefix(decoded.Operation, attachedAppOperationPrefix), decoded.Input)
	// Do not leak dependency existence or route a rejected alias into physical execution.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(http.StatusConflict, "unified_app_unavailable", "attached Unified App is unavailable; check access and the version receiving traffic"))
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(result)
	return true
}
