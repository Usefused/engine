package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/store"
)

// isImportApplyPath reports whether r is the config-as-code commit request
// that needs post-publication workspace activation instead of uniform forwarding.
func isImportApplyPath(method, path string) bool {
	return method == http.MethodPost && path == "/integrations/import/apply"
}

// importApplyResponse mirrors the wire shape of the Registry's
// POST /integrations/import/apply response. Duplicating the small response
// shape keeps Engine coupled to the public JSON contract rather than Registry
// implementation types.
type importApplyResponse struct {
	Status           string `json:"status"`
	OperationID      string `json:"operation_id"`
	Phase            string `json:"phase"`
	CommitState      string `json:"commit_state"`
	ServiceID        string `json:"service_id"`
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	Version          string `json:"version"`
	ServiceVersionID string `json:"service_version_id"`
}

type autoRegistrationAudit struct {
	operationID      uuid.UUID
	serviceID        uuid.UUID
	serviceSlug      string
	version          string
	serviceVersionID uuid.UUID
	outcome          string
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
	proxy.ForwardAndInspect(rec, r.WithContext(ctx), "", func(response *http.Response, body []byte) {
		audit := autoRegisterImportedService(ctx, s, contractFetcher, accountID, r.Header.Get("X-API-Key"), body)
		// Registry has committed, so Engine-local activation failure must replace
		// the success receipt with an authoritative partial outcome before write.
		if autoRegistrationSucceeded(audit) || audit.operationID == uuid.Nil {
			return
		}
		replaceProxyJSONResponse(response, http.StatusFailedDependency, importWorkspaceActivationFailure(audit, chimiddleware.GetReqID(ctx)))
	})
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

// autoRegisterImportedService makes a successfully imported service usable in
// the caller's workspace immediately, with no second request needed.
//
// Gated on actual workspace-activation state (IsWorkspaceServiceEnabled), not
// publication novelty: an existing Registry service may never have been
// activated in this Engine workspace.
//
// Registry commit remains successful when activation fails, but the returned
// audit lets the proxy replace the stale success response with exact recovery.
func autoRegisterImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, body []byte) autoRegistrationAudit {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.auto_register_service")
	defer span.End()

	audit := registerImportedService(ctx, s, contractFetcher, accountID, apiKey, body)
	recordAutoRegistrationAudit(span, accountID, audit)
	return audit
}

// autoRegistrationSucceeded recognizes only outcomes that made the imported version usable in this workspace.
func autoRegistrationSucceeded(audit autoRegistrationAudit) bool {
	return audit.outcome == "activated" || audit.outcome == "version_enabled"
}

// importWorkspaceActivationFailure returns a bounded structured partial outcome after Registry commit.
func importWorkspaceActivationFailure(audit autoRegistrationAudit, requestID string) []byte {
	recovery := importWorkspaceActivationRecovery(audit)
	response := workspaceConfigErrorResponse{Error: workspaceConfigErrorBody{
		Code:        "import_workspace_activation_failed",
		Message:     "The service was published, but Engine could not add its imported version to this workspace.",
		Category:    "partial",
		Retryable:   false,
		Phase:       "workspace_activation",
		OperationID: audit.operationID.String(),
		RequestID:   requestID,
		CommitState: "committed",
		Recovery:    recovery,
		Remediation: "Run the exact recovery command; do not repeat the committed import.",
		Details:     importWorkspaceActivationDetails(audit),
	}}
	body, err := json.Marshal(response)
	// This envelope contains only strings and a fixed map, so failure indicates
	// an internal encoder defect; retain a safe fixed response in that case.
	if err != nil {
		return []byte(`{"error":{"code":"import_workspace_activation_failed","message":"The service was published, but workspace activation failed.","category":"partial","phase":"workspace_activation","commit_state":"committed"}}`)
	}
	return body
}

// importWorkspaceActivationDetails omits unavailable opaque identities instead
// of rendering nil UUIDs that could look like valid recovery targets.
func importWorkspaceActivationDetails(audit autoRegistrationAudit) map[string]any {
	details := map[string]any{"workspace_outcome": audit.outcome}
	// Service identity is useful only after Registry returned a concrete UUID.
	if audit.serviceID != uuid.Nil {
		details["service_id"] = audit.serviceID.String()
	}
	// Version identity must independently be present before the CLI exposes it.
	if audit.serviceVersionID != uuid.Nil {
		details["service_version_id"] = audit.serviceVersionID.String()
	}
	return details
}

// importWorkspaceActivationRecovery pins activation to the exact Registry service and imported version.
func importWorkspaceActivationRecovery(audit autoRegistrationAudit) string {
	// Without complete Registry identity, status is the only recovery that does
	// not guess which service or version committed.
	if audit.serviceID == uuid.Nil || audit.serviceVersionID == uuid.Nil {
		return "fused-cli import status " + audit.operationID.String()
	}
	slug := safeImportRecoveryToken(audit.serviceSlug, audit.serviceID.String())
	version := safeImportRecoveryToken(audit.version, "")
	parts := []string{
		"fused-cli workspace service add", quoteImportRecoveryArg(slug),
		"--service-id", quoteImportRecoveryArg(audit.serviceID.String()),
	}
	// An exact safe version avoids activating a different future provider version;
	// malformed remote text is omitted rather than copied into a command.
	if version != "" {
		parts = append(parts, "--version", quoteImportRecoveryArg(version))
	}
	return strings.Join(append(parts, "--apply"), " ")
}

// safeImportRecoveryToken admits the bounded Registry identity grammar used in copied shell recovery.
func safeImportRecoveryToken(value, fallback string) string {
	value = strings.TrimSpace(value)
	// Recovery values must stay compact and credential-free even though Registry
	// normally supplies canonical slugs and versions here.
	if value == "" || len(value) > 256 || strings.Contains(strings.ToLower(value), "fsk_") || strings.Contains(value, "://") {
		return fallback
	}
	for _, char := range value {
		// Shell quoting cannot make terminal controls safe to render or audit.
		if unicode.IsControl(char) {
			return fallback
		}
	}
	return value
}

// quoteImportRecoveryArg makes a reviewed recovery argument inert in POSIX-compatible shells.
func quoteImportRecoveryArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// registerImportedService separates response admission from workspace mutation
// so malformed Registry success cannot reach local activation calls.
func registerImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, body []byte) autoRegistrationAudit {
	resp, audit, ok := decodeAutoRegistrationResponse(ctx, body)
	// Admission failure already contains the stable audit outcome needed by the proxy.
	if !ok {
		return audit
	}
	return activateImportedService(ctx, s, contractFetcher, accountID, apiKey, resp, audit)
}

// decodeAutoRegistrationResponse admits only the Registry's complete durable
// commit proof and resolves the exact immutable service version for activation.
func decodeAutoRegistrationResponse(ctx context.Context, body []byte) (importApplyResponse, autoRegistrationAudit, bool) {
	resp, operationID, rejected, ok := decodeCommittedImportApplyResponse(ctx, body)
	// Commit-proof rejection already owns its precise stable audit outcome.
	if !ok {
		return resp, rejected, false
	}
	// A committed service UUID is required for local authorization and storage.
	if resp.ServiceID == "" {
		return resp, autoRegistrationAudit{operationID: operationID, outcome: "missing_service_id"}, false
	}
	serviceID, err := uuid.Parse(resp.ServiceID)
	// Registry identity is opaque, so Engine must reject rather than normalize malformed UUIDs.
	if err != nil {
		slog.WarnContext(ctx, "auto-register: invalid service_id in import/apply response",
			slog.String("service_id", resp.ServiceID), slog.Any("error", err))
		return resp, autoRegistrationAudit{operationID: operationID, outcome: "invalid_service_id"}, false
	}
	serviceVersionID, err := uuid.Parse(resp.ServiceVersionID)
	// A nil or malformed version UUID cannot pin activation to the committed artifact.
	if err != nil || serviceVersionID == uuid.Nil {
		slog.WarnContext(ctx, "auto-register: invalid service_version_id in import/apply response",
			slog.String("service_version_id", resp.ServiceVersionID), slog.Any("error", err))
		return resp, autoRegistrationAudit{operationID: operationID, serviceID: serviceID, outcome: "invalid_service_version_id"}, false
	}
	resp.Slug = strings.TrimSpace(resp.Slug)
	if resp.Slug == "" {
		// The Registry slug is the stable config identity used by SDK/MCP plans.
		// Activating without it would succeed here but fail later authorization
		// when a desired config refers to the imported service by slug.
		return resp, autoRegistrationAudit{operationID: operationID, serviceID: serviceID, serviceVersionID: serviceVersionID, outcome: "missing_slug"}, false
	}
	audit := autoRegistrationAudit{operationID: operationID, serviceID: serviceID, serviceSlug: resp.Slug, version: resp.Version, serviceVersionID: serviceVersionID}
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

// decodeCommittedImportApplyResponse validates the Registry's durable success
// proof independently from resolving workspace service identity.
func decodeCommittedImportApplyResponse(ctx context.Context, body []byte) (importApplyResponse, uuid.UUID, autoRegistrationAudit, bool) {
	var resp importApplyResponse
	// Invalid JSON cannot prove either commit identity or workspace target.
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.WarnContext(ctx, "auto-register: could not decode import/apply response", slog.Any("error", err))
		return resp, uuid.Nil, autoRegistrationAudit{outcome: "invalid_response"}, false
	}
	operationID, err := uuid.Parse(resp.OperationID)
	// Auto-registration may only follow the Registry's exact durable success
	// proof; malformed success responses remain the CLI's unknown-outcome case.
	if err != nil || resp.Status != "applied" || resp.Phase != "complete" || resp.CommitState != "committed" {
		return resp, uuid.Nil, autoRegistrationAudit{outcome: "invalid_commit_proof"}, false
	}
	return resp, operationID, autoRegistrationAudit{}, true
}

// activateImportedService materializes the exact imported contract before
// adding or extending its local workspace membership.
func activateImportedService(ctx context.Context, s store.Store, contractFetcher RuntimeContractFetcher, accountID uuid.UUID, apiKey string, resp importApplyResponse, audit autoRegistrationAudit) autoRegistrationAudit {
	// Workspace identity must be authoritative before any local membership read or write.
	if err := verifyWorkspaceActor(ctx, accountID); err != nil {
		slog.ErrorContext(ctx, "auto-register: resolve Engine workspace failed", slog.Any("error", err))
		audit.outcome = "workspace_lookup_failed"
		return audit
	}

	activated, err := s.IsWorkspaceServiceEnabled(ctx, audit.serviceID)
	// Unknown membership cannot safely select either the add or enable-version mutation.
	if err != nil {
		slog.ErrorContext(ctx, "auto-register: IsWorkspaceServiceEnabled failed", slog.Any("error", err))
		audit.outcome = "activation_check_failed"
		return audit
	}
	// Both membership paths require the same validated snapshot before exposing the version.
	if err := materializeRuntimeContractSnapshot(ctx, s, contractFetcher, accountID, audit.serviceID, audit.serviceVersionID, resp.Version, apiKey); err != nil {
		audit.outcome = "contract_snapshot_failed"
		return audit
	}
	// Existing membership needs only the newly imported immutable version enabled.
	if activated {
		// Enabling the version is the only workspace mutation after snapshot success.
		if err := s.EnableWorkspaceServiceVersion(ctx, audit.serviceID, resp.Version, audit.serviceVersionID, accountID); err != nil {
			slog.ErrorContext(ctx, "auto-register: EnableWorkspaceServiceVersion failed", slog.Any("error", err))
			audit.outcome = "version_activation_failed"
			return audit
		}
		// Membership and version availability are separate: a later provider
		// version must become usable without rewriting the parent service row.
		audit.outcome = "version_enabled"
		return audit
	}

	// The additive mutation keeps unrelated workspace services untouched.
	if err := s.AddWorkspaceServiceVersion(ctx, audit.serviceID, resp.Slug, resp.Version, audit.serviceVersionID, importedServiceName(resp), accountID); err != nil {
		slog.ErrorContext(ctx, "auto-register: AddWorkspaceServiceVersion failed", slog.Any("error", err))
		audit.outcome = "activation_failed"
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

// recordAutoRegistrationAudit emits only stable identity and outcome fields on
// the existing mutation span; raw local errors remain in owner-controlled logs.
func recordAutoRegistrationAudit(span trace.Span, accountID uuid.UUID, audit autoRegistrationAudit) {
	span.SetAttributes(
		attribute.String("user_action", "workspace.auto_register_service"),
		attribute.String("account_id", accountID.String()),
		attribute.String("service_id", audit.serviceID.String()),
		attribute.String("service_version_id", audit.serviceVersionID.String()),
		attribute.String("outcome", audit.outcome),
	)
	// Every non-success outcome is a failed local follow-up, even when Registry
	// publication already committed and the caller receives exact recovery.
	if !autoRegistrationSucceeded(audit) {
		span.SetStatus(codes.Error, audit.outcome)
	}
}
