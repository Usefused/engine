package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
)

// This file implements the three Engine-native SDK/MCP lifecycle endpoints:
// activate, deactivate, and delete. They live under the existing /sdk-config
// prefix (see config_routes.go) rather than under /sdks/*, which is reserved
// for the Registry-proxied SDK-generation surface (RESTProxyMountPaths
// forwards /sdks/* to the Registry with its path preserved -- mounting
// native Engine routes there would collide with that proxy).

type sdkActivateResponse struct {
	Status     string `json:"status"`
	ArtifactID string `json:"artifact_id"`
	MCPURL     string `json:"mcp_url"`
	AuthToken  string `json:"auth_token,omitempty"`
}

type sdkLifecycleResponse struct {
	Status     string `json:"status"`
	ArtifactID string `json:"artifact_id"`
}

// ActivateSDKHandler handles POST /sdk-config/{id}/activate.
//
// Scope creation belongs to SDK/MCP config apply because that path validates
// service contracts and pins auth policy. This lifecycle endpoint only
// re-enables a scope that has already passed that boundary.
func ActivateSDKHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_lifecycle.activate")
		defer span.End()

		accountID, err := resolveWorkspaceActor(ctx)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}

		artifactID, err := parseArtifactIDParam(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		span.SetAttributes(attribute.String("artifact_id", artifactID.String()))

		err = reactivateArtifactScope(ctx, s, accountID, artifactID)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "failed"))
			writeSDKConfigError(w, err)
			return
		}

		span.SetAttributes(attribute.String("outcome", "success"))
		writeJSON(w, sdkActivateResponse{
			Status:     "activated",
			ArtifactID: artifactID.String(),
			MCPURL:     mcpURLForSDK(r, artifactID),
		})
	}
}

// reactivateArtifactScope checks ownership before changing state so lifecycle
// commands can never create or adopt an unmanaged runtime identifier.
func reactivateArtifactScope(ctx context.Context, s store.Store, accountID, artifactID uuid.UUID) error {
	if err := ensureArtifactScopeOwnedBy(ctx, s, accountID, artifactID); err != nil {
		return err
	}
	if err := s.ReactivateSDK(ctx, accountID, artifactID); err != nil {
		return sdkLifecycleStoreError(err, "failed to activate sdk scope")
	}
	return nil
}

// ensureArtifactScopeOwnedBy checks a scope exists for artifactID and belongs to
// accountID without creating or modifying anything. Used by activate's
// reactivate-only shape (no selections given), where there's nothing to
// persist and the only question is "does the caller own something to
// reactivate" -- and by deactivate/delete, which must never act on a scope
// belonging to a different account.
func ensureArtifactScopeOwnedBy(ctx context.Context, s store.Store, accountID, artifactID uuid.UUID) error {
	existing, err := s.GetArtifactScope(ctx, artifactID)
	if err != nil {
		if errors.Is(err, store.ErrArtifactScopeNotFound) {
			return workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk scope not found"}
		}
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load sdk scope"}
	}
	if existing.AccountID != accountID {
		return workspaceConfigHTTPError{status: http.StatusForbidden, message: "sdk scope owner mismatch"}
	}
	return nil
}

// DeactivateSDKHandler handles POST /sdk-config/{id}/deactivate. Deactivating
// rejects new MCP session connections (LocalObjectCache.loadArtifactScope checks
// DeactivatedAt) and immediately tears down sessions already live for this
// artifactID, rather than waiting for them to expire naturally.
func DeactivateSDKHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_lifecycle.deactivate")
		defer span.End()

		accountID, err := resolveWorkspaceActor(ctx)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		artifactID, err := parseArtifactIDParam(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		span.SetAttributes(attribute.String("artifact_id", artifactID.String()))

		if err := ensureArtifactScopeOwnedBy(ctx, s, accountID, artifactID); err != nil {
			span.SetAttributes(attribute.String("outcome", "failed"))
			writeSDKConfigError(w, err)
			return
		}
		if err := s.DeactivateSDK(ctx, accountID, artifactID); err != nil {
			span.SetAttributes(attribute.String("outcome", "failed"))
			writeSDKConfigError(w, sdkLifecycleStoreError(err, "failed to deactivate sdk scope"))
			return
		}
		// DeactivateSDK's cachedStore wrapper already published a
		// engine.cache.invalidate.sdk_scope.* NATS message, so any node about
		// to accept a *new* connection will see the deactivation via
		// loadArtifactScope. That alone doesn't touch sessions already connected
		// on this node -- they never re-check loadArtifactScope after connecting --
		// so force-kill them directly.
		sandbox.KillMCPSessionsForSDK(artifactID.String())

		span.SetAttributes(attribute.String("outcome", "success"))
		writeJSON(w, sdkLifecycleResponse{Status: "deactivated", ArtifactID: artifactID.String()})
	}
}

// DeleteSDKHandler handles DELETE /sdk-config/{id}. Deleting an SDK/MCP scope
// is permanent: it kills every live session, deletes the fused_artifact_scopes
// row (which cascades to fused_artifact_tokens and fused_artifact_buckets via their
// artifact_id FKs -- see schema_engine.go -- so no separate token-revoke or
// bucket-unlink loop is needed here), then best-effort removes the sandbox's
// on-disk working directory.
func DeleteSDKHandler(s store.Store, proxy Forwarder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_lifecycle.delete")
		defer span.End()

		accountID, err := resolveWorkspaceActor(ctx)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		artifactID, err := parseArtifactIDParam(r)
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		span.SetAttributes(attribute.String("artifact_id", artifactID.String()))

		outcome, err := deleteSDKState(ctx, s, proxy, accountID, artifactID)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", outcome))
			writeSDKConfigError(w, err)
			return
		}

		sandbox.KillMCPSessionsForSDK(artifactID.String())
		if err := sandbox.CleanupMCPSandboxDir(artifactID.String()); err != nil {
			// Best-effort: the DB row (the source of truth for "does this
			// SDK/MCP exist") is already gone, so a leftover directory is a
			// disk-hygiene issue, not a correctness one.
			slog.WarnContext(ctx, "DeleteSDKHandler: failed to remove sandbox directory", slog.Any("error", err), slog.String("artifact_id", artifactID.String()))
		}

		span.SetAttributes(attribute.String("outcome", outcome))
		writeJSON(w, sdkLifecycleResponse{Status: "deleted", ArtifactID: artifactID.String()})
	}
}

func deleteSDKState(ctx context.Context, s store.Store, proxy Forwarder, accountID, artifactID uuid.UUID) (string, error) {
	scope, err := s.GetArtifactScope(ctx, artifactID)
	if errors.Is(err, store.ErrArtifactScopeNotFound) {
		if deleteRestoredArtifact(ctx, s, proxy, accountID, artifactID) {
			return "snapshot_deleted", nil
		}
		return "already_deleted", nil
	}
	if err != nil {
		return "failed", workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load sdk scope"}
	}
	if scope.AccountID != accountID {
		return "denied", workspaceConfigHTTPError{status: http.StatusForbidden, message: "sdk scope owner mismatch"}
	}
	if scope.Kind != "mcp" {
		if err := retireRegistrySDK(ctx, proxy, artifactID); err != nil {
			return "registry_retire_failed", err
		}
	}
	if err := s.DeleteArtifactScope(ctx, accountID, artifactID); err != nil {
		return "failed", workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to delete sdk scope"}
	}
	if snapshots, ok := s.(store.ArtifactSnapshotStore); ok {
		if err := snapshots.DeleteArtifactSnapshot(ctx, accountID, artifactID); err != nil {
			return "failed", workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to delete artifact snapshot"}
		}
	}
	return "success", nil
}

func deleteRestoredArtifact(ctx context.Context, s store.Store, proxy Forwarder, accountID, artifactID uuid.UUID) bool {
	snapshots, ok := s.(store.ArtifactSnapshotStore)
	if !ok {
		return false
	}
	if _, err := snapshots.GetArtifactSnapshot(ctx, accountID, artifactID); err != nil {
		return false
	}
	if err := retireRegistrySDK(ctx, proxy, artifactID); err != nil {
		return false
	}
	return snapshots.DeleteArtifactSnapshot(ctx, accountID, artifactID) == nil
}

func retireRegistrySDK(ctx context.Context, proxy Forwarder, artifactID uuid.UUID) error {
	if proxy == nil {
		return workspaceConfigHTTPError{status: http.StatusBadGateway, message: "registry lifecycle unavailable"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, "/sdks/"+artifactID.String(), nil)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to create registry lifecycle request"}
	}
	recorder := httptest.NewRecorder()
	proxy.Forward(recorder, request, "")
	if recorder.Code >= http.StatusOK && recorder.Code < http.StatusMultipleChoices || recorder.Code == http.StatusNotFound {
		return nil
	}
	return sdkProxyError{status: recorder.Code, body: recorder.Body.Bytes()}
}

func parseArtifactIDParam(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errors.New("invalid sdk id")
	}
	return id, nil
}

func sdkLifecycleStoreError(err error, fallbackMessage string) error {
	if errors.Is(err, store.ErrArtifactScopeNotFound) {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk scope not found"}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: fallbackMessage}
}

// mcpURLForSDK builds the public MCP SSE URL for artifactID from the incoming
// request's own host, mirroring how webhook display URLs are built
// elsewhere in this package (see appliedWorkspaceWebhook's doc comment) --
// the caller already knows which Engine host it just hit, so echoing that
// back keeps this correct behind any reverse proxy without a separate public
// base URL config knob.
func mcpURLForSDK(r *http.Request, artifactID uuid.UUID) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/mcp/%s/sse", scheme, r.Host, artifactID.String())
}
