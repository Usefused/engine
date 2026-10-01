package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// unifiedAppTrafficReader keeps public resolution on the persisted promotion pointer.
type unifiedAppTrafficReader interface {
	UnifiedAppTrafficTarget(context.Context, uuid.UUID) (uuid.UUID, error)
}

// authenticateRESTEndpoint accepts only family IDs for Unified Apps and exact version IDs for SDKs.
func (s *EngineGRPCServer) authenticateRESTEndpoint(request *http.Request, endpointID uuid.UUID) (*store.AppRuntime, auth.RuntimeIdentity, *restExecutionError) {
	appID, familyRoute, requestErr := s.resolveRESTEndpoint(request.Context(), endpointID)
	// Failed resolution never guesses a version from creation order.
	if requestErr != nil {
		return nil, auth.RuntimeIdentity{}, requestErr
	}
	scope, identity, requestErr := s.authenticateRESTApp(request, appID)
	// The ordinary validator still owns token scope, expiry and revocation.
	if requestErr != nil {
		return nil, auth.RuntimeIdentity{}, requestErr
	}
	// Exact Unified App IDs are deliberately not an alternative public execution route.
	if scope.Kind == store.AppKindUnifiedApp && !familyRoute {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusBadRequest, "app_family_id_required", "use the Unified App family ID in the execution URL")
	}
	// A corrupted family pointer must never route into another adapter or family.
	if familyRoute && (scope.Kind != store.AppKindUnifiedApp || identity.AppFamilyID != endpointID || scope.AppFamilyID != endpointID) {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusForbidden, "app_scope_unavailable", "app scope is unavailable")
	}
	return scope, identity, nil
}

// resolveRESTEndpoint reads deployment state on every call, outside the exact-version authorization cache.
func (s *EngineGRPCServer) resolveRESTEndpoint(ctx context.Context, endpointID uuid.UUID) (uuid.UUID, bool, *restExecutionError) {
	targets, ok := s.store.(unifiedAppTrafficReader)
	// Stores without Unified App support can still serve exact SDK requests.
	if !ok {
		return endpointID, false, nil
	}
	target, err := targets.UnifiedAppTrafficTarget(ctx, endpointID)
	// Only authoritative absence allows the ordinary SDK version lookup.
	if errors.Is(err, store.ErrAppFamilyNotFound) {
		return endpointID, false, nil
	}
	// Database failures and cleared deployments cannot revive an older sibling.
	if err != nil || target == uuid.Nil {
		return uuid.Nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "Unified App has no available traffic target")
	}
	return target, true, nil
}

// authorizedEndpointExecutionResult preserves historical reads under the same family URL after promotion.
func (s *EngineGRPCServer) authorizedEndpointExecutionResult(ctx context.Context, request *http.Request, scope *store.AppRuntime, endpointID, executionID uuid.UUID) (*store.ExecutionResult, *restExecutionError) {
	// SDK result authorization retains its exact-version boundary.
	if scope.Kind != store.AppKindUnifiedApp {
		return s.authorizedExecutionResult(ctx, request, scope.AccountID, scope.AppID, executionID)
	}
	hash, valid := executionReadHandleDigest(request)
	// A handle remains mandatory even when the caller holds the family execution token.
	if !valid {
		return nil, newRESTExecutionError(http.StatusNotFound, "execution_not_found", "execution not found")
	}
	results, ok := s.store.(store.FamilyExecutionResultStore)
	// Missing family-aware storage cannot fall back to a weaker unscoped lookup.
	if !ok {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution results are unavailable")
	}
	record, err := results.GetFamilyExecutionResult(ctx, scope.AccountID, endpointID, executionID)
	// Wrong families, missing records and incorrect handles disclose the same bounded error.
	if errors.Is(err, store.ErrExecutionResultNotFound) || (err == nil && (record == nil || subtle.ConstantTimeCompare([]byte(hash), []byte(record.ReadHandleHash)) != 1)) {
		return nil, newRESTExecutionError(http.StatusNotFound, "execution_not_found", "execution not found")
	}
	// Persistence failures cannot be mistaken for an authorized empty result.
	if err != nil {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution results are unavailable")
	}
	return record, nil
}
