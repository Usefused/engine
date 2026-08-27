package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/Usefused/engine/internal/engine/sandbox"
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

// importContractPreflighter is the read-only Registry client capability needed
// to admit the prospective runtime contract before publication.
type importContractPreflighter interface {
	PreflightImport(context.Context, []byte) (*sandbox.ImportContractPreflight, error)
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
	request, cancel, err := prepareImportApplyResponseWindow(w, r.WithContext(ctx))
	defer cancel()
	// A broken response deadline must fail before forwarding a possible mutation;
	// otherwise a predictable delivery failure would make commit recovery harder.
	if err != nil {
		writeImportResponseWindowFailure(rec, ctx)
		span.SetStatus(codes.Error, "import_response_deadline_unavailable")
		span.SetAttributes(attribute.String("failure_code", "import_response_deadline_unavailable"))
	} else {
		forwardPreparedImportApply(proxy, s, contractFetcher, rec, request, accountID)
	}
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

// forwardPreparedImportApply keeps publication and activation on one bounded
// request context while preserving the existing partial-outcome response path.
func forwardPreparedImportApply(proxy Forwarder, s store.Store, contractFetcher RuntimeContractFetcher, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx := r.Context()
	prepared, ok := prepareImportApplyPreflight(w, r, contractFetcher)
	// A failed preflight proves Registry publication was never attempted.
	if !ok {
		return
	}
	proxy.ForwardAndInspect(w, prepared, "", func(response *http.Response, body []byte) {
		audit := autoRegisterImportedService(ctx, s, contractFetcher, accountID, r.Header.Get("X-API-Key"), body)
		// Registry has committed, so Engine-local activation failure must replace
		// the success receipt with an authoritative partial outcome before write.
		if autoRegistrationSucceeded(audit) || audit.operationID == uuid.Nil {
			return
		}
		replaceProxyJSONResponse(response, http.StatusFailedDependency, importWorkspaceActivationFailure(audit, chimiddleware.GetReqID(ctx)))
	})
}

// prepareImportApplyPreflight buffers one tiny receipt, validates Registry's
// candidate in Engine, and adds only the resulting proof to the forwarded body.
func prepareImportApplyPreflight(w http.ResponseWriter, r *http.Request, contractFetcher RuntimeContractFetcher) (*http.Request, bool) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.import.preflight", trace.WithAttributes(
		attribute.String("user_action", "import.preflight"),
		attribute.String("phase", "engine_preflight"),
	))
	defer span.End()
	body, fields, receipt, err := readImportApplyReceipt(r.Body)
	// Malformed or oversized receipts fail before either Registry request can run.
	if err != nil {
		recordImportPreflightSpan(span, "request_rejected", "import_preflight_request_invalid", uuid.Nil)
		writeEngineImportPreflightFailure(w, ctx, http.StatusBadRequest, "import_preflight_request_invalid", "The import apply receipt is invalid.", false, uuid.Nil, "")
		return nil, false
	}
	preflighter, ok := contractFetcher.(importContractPreflighter)
	// Production always supplies the Registry client; a missing capability must
	// fail closed rather than restoring the old publish-first behavior.
	if !ok {
		recordImportPreflightSpan(span, "unavailable", "import_preflight_unavailable", receipt.PlanID)
		writeEngineImportPreflightFailure(w, ctx, http.StatusServiceUnavailable, "import_preflight_unavailable", "Engine import preflight is unavailable.", true, receipt.PlanID, importApplyRecovery(receipt))
		return nil, false
	}
	preflightBody := importApplyBodyWithoutProof(fields)
	preflight, err := preflighter.PreflightImport(ctx, preflightBody)
	if err != nil {
		// A terminal plan cannot mutate on replay, so Registry apply remains the
		// sole owner of its durable committed or failed receipt.
		if importPreflightAllowsTerminalReplay(err) {
			recordImportPreflightSpan(span, "terminal_replay", "", receipt.PlanID)
			return requestWithImportApplyBody(r, body), true
		}
		writeImportPreflightError(w, ctx, span, err, receipt)
		return nil, false
	}
	// A response for another plan is not authority to mutate this receipt.
	if preflight.OperationID != receipt.PlanID {
		recordImportPreflightSpan(span, "response_rejected", "import_preflight_identity_mismatch", receipt.PlanID)
		writeEngineImportPreflightFailure(w, ctx, http.StatusBadGateway, "import_preflight_identity_mismatch", "Registry returned preflight proof for a different import operation.", false, receipt.PlanID, importApplyRecovery(receipt))
		return nil, false
	}
	preparedBody, err := importApplyBodyWithProof(fields, preflight.ContractHash)
	// Encoding an already admitted bounded map should be infallible; keep the
	// mutation closed if that invariant is ever broken.
	if err != nil {
		recordImportPreflightSpan(span, "response_rejected", "import_preflight_proof_encoding_failed", receipt.PlanID)
		writeEngineImportPreflightFailure(w, ctx, http.StatusInternalServerError, "import_preflight_proof_encoding_failed", "Engine could not bind the import preflight proof.", false, receipt.PlanID, importApplyRecovery(receipt))
		return nil, false
	}
	span.SetAttributes(
		attribute.Int("contract.endpoint_count", len(preflight.Snapshot.Endpoints)),
		attribute.Int("contract.webhook_count", len(preflight.Snapshot.Webhooks)),
	)
	recordImportPreflightSpan(span, "accepted", "", receipt.PlanID)
	return requestWithImportApplyBody(r, preparedBody), true
}

type importApplyReceipt struct {
	PlanID     uuid.UUID
	ReviewHash string
}

// readImportApplyReceipt bounds and strictly admits the immutable identifiers
// while retaining unknown fields for forward-compatible proxying.
func readImportApplyReceipt(body io.ReadCloser) ([]byte, map[string]json.RawMessage, importApplyReceipt, error) {
	payload, err := readBoundedImportApplyBody(body)
	// Transport admission remains separate from JSON and identity validation so
	// each boundary stays small and reports one stable caller classification.
	if err != nil {
		return nil, nil, importApplyReceipt{}, err
	}
	fields, err := decodeImportApplyFields(payload)
	// No identity is parsed from a partial or multi-document request.
	if err != nil {
		return nil, nil, importApplyReceipt{}, err
	}
	receipt, err := parseImportApplyReceipt(fields)
	return payload, fields, receipt, err
}

// readBoundedImportApplyBody consumes one replayable receipt without allowing
// a truncated prefix to reach either Registry preflight or publication.
func readBoundedImportApplyBody(body io.ReadCloser) ([]byte, error) {
	// A missing stream cannot carry an immutable plan receipt.
	if body == nil {
		return nil, errors.New("import apply body is required")
	}
	defer body.Close() // The forwarded request receives an independent replayable reader below.
	payload, err := io.ReadAll(io.LimitReader(body, sandbox.MaxImportPreflightRequestBytes+1))
	// Crossing the shared limit must not forward a truncated JSON prefix.
	if err != nil || len(payload) == 0 || len(payload) > sandbox.MaxImportPreflightRequestBytes {
		return nil, errors.New("import apply body is unreadable or too large")
	}
	return payload, nil
}

// decodeImportApplyFields admits exactly one JSON object while retaining
// unknown fields for forward-compatible proxying after Engine adds its proof.
func decodeImportApplyFields(payload []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	// The apply contract is one object; scalars and arrays cannot carry named proof fields.
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("import apply body must be a JSON object")
	}
	// A second JSON document would not be covered by the Registry proof.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("import apply body contains trailing data")
	}
	return fields, nil
}

// parseImportApplyReceipt validates the immutable identifiers Engine uses to
// correlate Registry preview, publication, and exact retry recovery.
func parseImportApplyReceipt(fields map[string]json.RawMessage) (importApplyReceipt, error) {
	var planIDText, reviewHash string
	// Both fields are required before Engine can correlate a preflight response.
	if json.Unmarshal(fields["plan_id"], &planIDText) != nil || json.Unmarshal(fields["review_hash"], &reviewHash) != nil {
		return importApplyReceipt{}, errors.New("import apply receipt fields are invalid")
	}
	planID, err := uuid.Parse(planIDText)
	// Empty hashes and nil identities cannot bind the immutable Registry review.
	if err != nil || planID == uuid.Nil || strings.TrimSpace(reviewHash) == "" {
		return importApplyReceipt{}, errors.New("import apply receipt is incomplete")
	}
	return importApplyReceipt{PlanID: planID, ReviewHash: reviewHash}, nil
}

// importApplyBodyWithoutProof prevents a caller-supplied proof from entering
// Registry preflight while retaining every other apply field byte-semantically.
func importApplyBodyWithoutProof(fields map[string]json.RawMessage) []byte {
	clean := cloneImportApplyFields(fields)
	delete(clean, "preflight_hash")
	payload, _ := json.Marshal(clean)
	return payload
}

// importApplyBodyWithProof overwrites any caller value with the exact hash
// produced and admitted inside this Engine request.
func importApplyBodyWithProof(fields map[string]json.RawMessage, contractHash string) ([]byte, error) {
	prepared := cloneImportApplyFields(fields)
	hashJSON, err := json.Marshal(contractHash)
	// A Go string should always encode; returning the error preserves fail-closed behavior.
	if err != nil {
		return nil, err
	}
	prepared["preflight_hash"] = hashJSON
	return json.Marshal(prepared)
}

// cloneImportApplyFields keeps body reconstruction from mutating the admitted
// request map that terminal replay may still need unchanged.
func cloneImportApplyFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(fields)+1)
	for key, value := range fields {
		cloned[key] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

// requestWithImportApplyBody restores a replayable request for ReverseProxy
// after Engine consumed the original stream during preflight.
func requestWithImportApplyBody(r *http.Request, body []byte) *http.Request {
	prepared := r.Clone(r.Context())
	prepared.Body = io.NopCloser(bytes.NewReader(body))
	prepared.ContentLength = int64(len(body))
	prepared.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return prepared
}

// importPreflightAllowsTerminalReplay identifies only Registry's durable
// non-pending classification; every other preflight failure blocks apply.
func importPreflightAllowsTerminalReplay(err error) bool {
	upstream, response, ok := admittedImportPreflightHTTPError(err)
	// Only Registry's exact conflict response proves this plan can no longer mutate.
	if !ok || upstream.StatusCode != http.StatusConflict {
		return false
	}
	return response.Error.Code == "IMPORT_OPERATION_NOT_PENDING"
}

// admittedImportPreflightHTTPError accepts only Registry's nested, bounded
// public envelope so proxy or transport prose cannot cross Engine's API.
func admittedImportPreflightHTTPError(err error) (*sandbox.ImportPreflightHTTPError, workspaceConfigErrorResponse, bool) {
	var upstream *sandbox.ImportPreflightHTTPError
	// Local transport and validation failures do not contain Registry-owned recovery facts.
	if !errors.As(err, &upstream) || upstream.StatusCode < http.StatusBadRequest || upstream.StatusCode > 599 {
		return nil, workspaceConfigErrorResponse{}, false
	}
	var response workspaceConfigErrorResponse
	// A stable code is required before the original bounded envelope may be forwarded.
	if json.Unmarshal(upstream.Body, &response) != nil || strings.TrimSpace(response.Error.Code) == "" {
		return nil, workspaceConfigErrorResponse{}, false
	}
	return upstream, response, true
}

// writeImportPreflightError preserves typed Registry admission errors and maps
// Engine-local failures to one slim, secret-safe pre-commit envelope.
func writeImportPreflightError(w http.ResponseWriter, ctx context.Context, span trace.Span, err error, receipt importApplyReceipt) {
	upstream, response, admitted := admittedImportPreflightHTTPError(err)
	// Registry already owns typed provenance, review, and operation-state recovery semantics.
	if admitted {
		recordImportPreflightSpan(span, "registry_rejected", response.Error.Code, receipt.PlanID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstream.StatusCode)
		_, _ = w.Write(upstream.Body)
		return
	}
	// Deterministic Engine admission means Registry publication is proven absent.
	if errors.Is(err, sandbox.ErrImportRuntimeContractRejected) {
		slog.WarnContext(ctx, "Engine rejected prospective import runtime contract", slog.Any("error", err))
		recordImportPreflightSpan(span, "contract_rejected", "import_runtime_contract_rejected", receipt.PlanID)
		writeEngineImportPreflightFailure(w, ctx, http.StatusUnprocessableEntity, "import_runtime_contract_rejected", "Engine rejected the prospective runtime contract.", false, receipt.PlanID, "")
		return
	}
	slog.ErrorContext(ctx, "Engine import preflight failed", slog.Any("error", err))
	recordImportPreflightSpan(span, "unavailable", "import_preflight_unavailable", receipt.PlanID)
	writeEngineImportPreflightFailure(w, ctx, http.StatusServiceUnavailable, "import_preflight_unavailable", "Engine could not complete import preflight.", true, receipt.PlanID, importApplyRecovery(receipt))
}

// writeEngineImportPreflightFailure emits only proven pre-commit state and an
// exact retry command when the same immutable receipt is safe to retry.
func writeEngineImportPreflightFailure(w http.ResponseWriter, ctx context.Context, status int, code, message string, retryable bool, operationID uuid.UUID, recovery string) {
	operation := ""
	// Nil identity remains omitted rather than looking like a durable operation.
	if operationID != uuid.Nil {
		operation = operationID.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(workspaceConfigErrorResponse{Error: workspaceConfigErrorBody{
		Code: code, Message: message, Category: "precondition", Retryable: retryable,
		Phase: "engine_preflight", OperationID: operation, RequestID: chimiddleware.GetReqID(ctx),
		CommitState: "not_committed", Recovery: recovery,
	}})
}

// importApplyRecovery reconstructs the exact immutable receipt command without
// depending on local CLI state that may be unavailable during automation.
func importApplyRecovery(receipt importApplyReceipt) string {
	return "fused-cli import apply --plan-id '" + receipt.PlanID.String() + "' --review-hash '" + strings.ReplaceAll(receipt.ReviewHash, "'", "'\"'\"'") + "'"
}

// recordImportPreflightSpan keeps contract content and hashes out of telemetry
// while retaining the durable operation and stable outcome selectors.
func recordImportPreflightSpan(span trace.Span, outcome, code string, operationID uuid.UUID) {
	span.SetAttributes(attribute.String("outcome", outcome))
	// Empty codes are successful control states, not synthetic errors.
	if code != "" {
		span.SetAttributes(attribute.String("error_code", code))
	}
	// A non-nil plan ID is the only durable preflight correlation identity.
	if operationID != uuid.Nil {
		span.SetAttributes(attribute.String("operation_id", operationID.String()))
	}
	// Only failed outcomes should set the span error status.
	if outcome != "accepted" && outcome != "terminal_replay" {
		span.SetStatus(codes.Error, code)
	}
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
