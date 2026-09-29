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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const attachedAppOperationPrefix = "unified_app:"

// AttachedUnifiedApps exposes only the current consumer's admitted public contracts.
func (s *EngineGRPCServer) AttachedUnifiedApps(ctx context.Context, identity auth.RuntimeIdentity) ([]models.UnifiedAppBinding, error) {
	// A hosted app cannot become a recursive consumer or borrow another adapter's scope.
	if identity.Kind != store.AppKindSDK && identity.Kind != store.AppKindMCP {
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
	bindings, err := s.AttachedUnifiedApps(ctx, identity)
	if err != nil {
		return nil, err
	}
	var selected *models.UnifiedAppBinding
	for i := range bindings {
		if bindings[i].Alias == alias {
			selected = &bindings[i]
			break
		}
	}
	// Missing membership cannot fall back to a workspace-wide app lookup.
	if selected == nil {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	scope, err := s.store.GetAppRuntime(ctx, selected.AppID)
	// Account, source identity, deletion, and promotion remain live execution checks.
	if err != nil || scope == nil || scope.AccountID != identity.AccountID || scope.AppFamilyID != selected.AppFamilyID || scope.Kind != store.AppKindUnifiedApp || !scope.Status.Runnable() || scope.BundleDigest != selected.BundleDigest {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	if s.admitUnifiedAppTraffic(ctx, selected.AppID) != nil {
		return nil, errors.New("attached Unified App version is not receiving traffic")
	}
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, selected.AppID)
	if requestErr != nil || !found || bundle.SourceHash != selected.SourceHash {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	// Plan-time app.use delegates the target's selected provider scope; the caller's token ID retains attribution.
	delegated := auth.RuntimeIdentity{AccountID: identity.AccountID, AppFamilyID: scope.AppFamilyID, AppID: scope.AppID, TokenID: identity.TokenID, AppVersion: scope.Version, Kind: scope.Kind, Status: scope.Status, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	result, requestErr := s.executeCapabilityRun(ctx, capabilityRunSpec{transport: string(identity.Kind), identity: delegated, version: scope.Version, bundle: bundle, manifest: manifest, input: input, mode: "live"})
	if requestErr != nil {
		return nil, errors.New("attached Unified App execution unavailable")
	}
	return json.Marshal(result)
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
