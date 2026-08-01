package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/store"
)

// isImportApplyPath reports whether r is the config-as-code commit request
// that gets the auto-register intercept (Task 3, engine_workspace_
// registration_plan.md) instead of RESTProxyHandler's normal uniform
// forward.
func isImportApplyPath(method, path string) bool {
	return method == http.MethodPost && path == "/integrations/import/apply"
}

// importApplyResponse mirrors the wire shape of the Registry's
// POST /integrations/import/apply response. Duplicating the small response
// shape keeps Engine coupled to the public JSON contract rather than Registry
// implementation types.
type importApplyResponse struct {
	ServiceID        string `json:"service_id"`
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	IsNewService     bool   `json:"is_new_service"`
	Version          string `json:"version"`
	ServiceVersionID string `json:"service_version_id"`
}

type autoRegistrationAudit struct {
	serviceID        uuid.UUID
	version          string
	serviceVersionID uuid.UUID
	outcome          string
	err              error
}

// forwardImportApplyWithAutoRegister mirrors forwardRESTMutationWithSpan's
// span setup, but proxies via ForwardAndInspect so autoRegisterImportedService
// can read what the Registry just applied.
func forwardImportApplyWithAutoRegister(proxy Forwarder, s store.Store, contractFetcher RuntimeContractFetcher, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.rest_mutation", trace.WithAttributes(
		attribute.String("user_action", "rest."+r.Method),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	rec := newStatusRecorder(w)
	proxy.ForwardAndInspect(rec, r.WithContext(ctx), "", func(body []byte) {
		autoRegisterImportedService(ctx, s, contractFetcher, accountID, r.Header.Get("X-API-Key"), body)
	})
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

// autoRegisterImportedService is Task 3's intercept
// (engine_workspace_registration_plan.md): after a successful
// import/apply, make the applied service usable in the caller's workspace
// immediately, with no second request needed.
//
// Gated on actual workspace-activation state (IsWorkspaceServiceEnabled), not
// IsNewService -- IsNewService only says whether the Registry row was just
// created, which isn't the same thing as "already activated in this
// workspace" (an existing Registry service may never have been activated in
// this Engine workspace).
//
// Failures here are logged and swallowed, never surfaced to the caller: a
// failure to auto-register must not turn an otherwise-successful
// import/apply into a client-visible error. The explicit "Add to Workspace"
// path remains available when automatic registration fails.
func autoRegisterImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, body []byte) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.auto_register_service")
	defer span.End()

	audit := registerImportedService(ctx, s, contractFetcher, accountID, apiKey, body)
	recordAutoRegistrationAudit(span, accountID, audit)
}

func registerImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, body []byte) autoRegistrationAudit {
	resp, audit, ok := decodeAutoRegistrationResponse(ctx, body)
	if !ok {
		return audit
	}
	return activateImportedService(ctx, s, contractFetcher, accountID, apiKey, resp, audit)
}

func decodeAutoRegistrationResponse(ctx context.Context, body []byte) (importApplyResponse, autoRegistrationAudit, bool) {
	var resp importApplyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.WarnContext(ctx, "auto-register: could not decode import/apply response", slog.Any("error", err))
		return resp, autoRegistrationAudit{outcome: "invalid_response", err: err}, false
	}
	if resp.ServiceID == "" {
		return resp, autoRegistrationAudit{outcome: "missing_service_id"}, false
	}
	serviceID, err := uuid.Parse(resp.ServiceID)
	if err != nil {
		slog.WarnContext(ctx, "auto-register: invalid service_id in import/apply response",
			slog.String("service_id", resp.ServiceID), slog.Any("error", err))
		return resp, autoRegistrationAudit{outcome: "invalid_service_id", err: err}, false
	}
	serviceVersionID, err := uuid.Parse(resp.ServiceVersionID)
	if err != nil || serviceVersionID == uuid.Nil {
		slog.WarnContext(ctx, "auto-register: invalid service_version_id in import/apply response",
			slog.String("service_version_id", resp.ServiceVersionID), slog.Any("error", err))
		return resp, autoRegistrationAudit{outcome: "invalid_service_version_id", err: err}, false
	}
	audit := autoRegistrationAudit{serviceID: serviceID, version: resp.Version, serviceVersionID: serviceVersionID}
	if resp.Version == "" {
		// AddWorkspaceServiceVersion requires a concrete version to pin to -- there's
		// nothing safe to activate without one.
		slog.WarnContext(ctx, "auto-register: import/apply response has no version, skipping",
			slog.String("service_id", resp.ServiceID))
		audit.outcome = "missing_version"
		return resp, audit, false
	}
	return resp, audit, true
}

func activateImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, resp importApplyResponse, audit autoRegistrationAudit) autoRegistrationAudit {
	if err := verifyWorkspaceActor(ctx, accountID); err != nil {
		slog.ErrorContext(ctx, "auto-register: resolve Engine workspace failed", slog.Any("error", err))
		audit.outcome, audit.err = "workspace_lookup_failed", err
		return audit
	}

	activated, err := s.IsWorkspaceServiceEnabled(ctx, audit.serviceID)
	if err != nil {
		slog.ErrorContext(ctx, "auto-register: IsWorkspaceServiceEnabled failed", slog.Any("error", err))
		audit.outcome, audit.err = "activation_check_failed", err
		return audit
	}
	if activated {
		if err := materializeRuntimeContractSnapshot(ctx, s, contractFetcher, accountID, audit.serviceID, audit.serviceVersionID, resp.Version, apiKey); err != nil {
			audit.outcome, audit.err = "contract_snapshot_failed", err
			return audit
		}
		if err := s.EnableWorkspaceServiceVersion(ctx, audit.serviceID, resp.Version, audit.serviceVersionID, accountID); err != nil {
			slog.ErrorContext(ctx, "auto-register: EnableWorkspaceServiceVersion failed", slog.Any("error", err))
			audit.outcome, audit.err = "version_activation_failed", err
			return audit
		}
		// Membership and version availability are separate: a later provider
		// version must become usable without rewriting the parent service row.
		audit.outcome = "version_enabled"
		return audit
	}

	if err := materializeRuntimeContractSnapshot(ctx, s, contractFetcher, accountID, audit.serviceID, audit.serviceVersionID, resp.Version, apiKey); err != nil {
		audit.outcome, audit.err = "contract_snapshot_failed", err
		return audit
	}
	if err := s.AddWorkspaceServiceVersion(ctx, audit.serviceID, "", resp.Version, audit.serviceVersionID, importedServiceName(resp), accountID); err != nil {
		slog.ErrorContext(ctx, "auto-register: AddWorkspaceServiceVersion failed", slog.Any("error", err))
		audit.outcome, audit.err = "activation_failed", err
		return audit
	}
	audit.outcome = "activated"
	return audit
}

func importedServiceName(resp importApplyResponse) string {
	if resp.Name != "" {
		return resp.Name
	}
	return resp.Slug
}

func recordAutoRegistrationAudit(span trace.Span, accountID uuid.UUID, audit autoRegistrationAudit) {
	span.SetAttributes(
		attribute.String("user_action", "workspace.auto_register_service"),
		attribute.String("account_id", accountID.String()),
		attribute.String("service_id", audit.serviceID.String()),
		attribute.String("service_version_id", audit.serviceVersionID.String()),
		attribute.String("service_version", audit.version),
		attribute.String("outcome", audit.outcome),
	)
	if audit.err != nil {
		span.RecordError(audit.err)
		span.SetStatus(codes.Error, audit.outcome)
	}
}
