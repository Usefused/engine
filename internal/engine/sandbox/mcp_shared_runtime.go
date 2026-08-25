package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mcpCallRequest preserves params as JSON until exact catalogue kind is known,
// avoiding numeric changes to Unified input while retaining physical map decoding.
type mcpCallRequest struct {
	OperationID string          `json:"operation_id"`
	Params      json.RawMessage `json:"params"`
}

// mcpUnifiedInvocation is the SDK-equivalent public call shape; transport
// identity and gRPC metadata remain Engine-owned and cannot be model-authored.
type mcpUnifiedInvocation struct {
	Input          json.RawMessage                       `json:"input"`
	Targets        []string                              `json:"targets"`
	Selectors      map[string]mcpUnifiedSelector         `json:"selectors,omitempty"`
	Pagination     map[string]mcpUnifiedPaginationIntent `json:"pagination,omitempty"`
	IdempotencyKey string                                `json:"idempotencyKey,omitempty"`
}

// mcpUnifiedSelector accepts only the generated TypeScript SDK's camelCase routing vocabulary.
type mcpUnifiedSelector struct {
	Environment string `json:"environment,omitempty"`
	EndUserRef  string `json:"endUserRef,omitempty"`
	AuthType    string `json:"authType,omitempty"`
	AuthName    string `json:"authName,omitempty"`
	ResourceID  string `json:"resourceId,omitempty"`
}

// mcpUnifiedPaginationIntent exposes only the caller-owned page ceiling.
type mcpUnifiedPaginationIntent struct {
	MaxPages uint32 `json:"maxPages"`
}

// mcpUnifiedResult is the exact generated-SDK all-settled target wire shape.
type mcpUnifiedResult struct {
	Target     string                `json:"target"`
	Status     string                `json:"status"`
	Data       json.RawMessage       `json:"data"`
	ErrorCode  *string               `json:"errorCode"`
	AuthAction *mcpUnifiedAuthAction `json:"authAction"`
}

// mcpUnifiedRollback is the exact generated-SDK compensation wire shape.
type mcpUnifiedRollback struct {
	Target      string                `json:"target"`
	Status      string                `json:"status"`
	ErrorCode   *string               `json:"errorCode"`
	TriggeredBy []string              `json:"triggeredBy"`
	AuthAction  *mcpUnifiedAuthAction `json:"authAction"`
}

// mcpUnifiedAuthAction omits optional recovery fields exactly as generated SDKs do.
type mcpUnifiedAuthAction struct {
	Action       string `json:"action"`
	BucketID     string `json:"bucketId"`
	ServiceID    string `json:"serviceId"`
	EndUserRef   string `json:"endUserRef"`
	ConnectionID string `json:"connectionId,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// mcpCallResponse mirrors callClient.ts's CallResponse -- exactly one of
// Result/Error is set. Result is left as json.RawMessage rather than a Go
// string so a JSON vendor response reaches the script as a parsed value,
// not a double-encoded string.
type mcpCallResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// mcpCallHandler is call()'s only server-side entrypoint (design doc,
// "No I/O outside call()" -- the shared runtime's sandboxed process has no
// other path to a vendor API). It is intentionally thin: resolve the
// session (only a live, already-authenticated MCP session may call this at
// all), resolve+validate the operation against the fixture (Guarding
// Against Hallucinated Calls), then hand off to the exact same
// engineExecuteCore path the gRPC edge uses (sandbox.go:
// EngineStreamExecuteFunc). No new dispatch or param-routing logic is
// introduced here -- that already exists in dispatcher.go and works the
// same way regardless of which caller flattened params into the map.
func mcpCallHandler(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := extractBearerToken(r)
	// Only a live session identifier may reach either catalogue namespace.
	if !ok {
		writeMCPCallResult(w, http.StatusUnauthorized, mcpCallResponse{Error: "Authorization header required"})
		return
	}

	sess, ok := lookupMCPSession(sessionID)
	// Session lookup preserves the established tenant boundary for both kinds.
	if !ok {
		writeMCPCallResult(w, http.StatusNotFound, mcpCallResponse{Error: "mcp session not found or expired"})
		return
	}

	var req mcpCallRequest
	// One malformed envelope receives the same bounded failure before dispatch.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: "invalid request body"})
		return
	}

	// Exact catalogue kind selects the adapter; invocation shape never grants
	// access to a private definition or changes physical dispatch behavior.
	if descriptor, unified := resolveSessionFixtureUnifiedOperation(sess, req.OperationID); unified {
		handleMCPUnifiedCall(w, r, sess, descriptor, req)
		return
	}

	op, ok := resolveSessionFixtureOperation(sess, req.OperationID)
	// Unknown names remain indistinguishable from entries outside this session.
	if !ok {
		// Tier-1 enforcement (design doc, Trust and Governance Model): an
		// operationId outside this server's registered set fails to resolve
		// here, before any credential or vendor is ever touched.
		slog.WarnContext(r.Context(), "mcp call() rejected: unregistered operationId",
			slog.String("operation_id", req.OperationID))
		writeMCPCallResult(w, http.StatusNotFound, mcpCallResponse{Error: fmt.Sprintf("unknown operationId %q", req.OperationID)})
		return
	}

	var params map[string]any
	// Physical calls retain their existing flat-map contract after the envelope
	// became raw JSON solely to preserve exact Unified input bytes.
	if len(req.Params) != 0 {
		// Present physical params must still be a valid JSON map before schema validation.
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: "invalid request body"})
			return
		}
	}
	// Existing physical schema validation and its client-visible errors remain unchanged.
	if err := validateCallParams(op, params); err != nil {
		slog.WarnContext(r.Context(), "mcp call() rejected: schema validation failed",
			slog.String("operation_id", req.OperationID))
		writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: err.Error()})
		return
	}

	result, err := dispatchMCPCall(r.Context(), sess, req.OperationID, params)
	// Physical provider failures retain their established response path.
	if err != nil {
		writeMCPCallResult(w, http.StatusBadGateway, mcpCallResponse{Error: err.Error()})
		return
	}

	writeMCPCallResult(w, http.StatusOK, mcpCallResponse{Result: result})
}

// resolveSessionFixtureUnifiedOperation keeps logical lookup inside the same
// immutable session fixture and fails closed when a stale session lacks it.
func resolveSessionFixtureUnifiedOperation(sess *mcpSession, operationID string) (*models.SDKUnifiedOperationDescriptor, bool) {
	// Missing fixture state cannot authorize a fallback to private definitions.
	if sess == nil || sess.fixture == nil {
		return nil, false
	}
	return sess.fixture.ResolveUnified(operationID)
}

// handleMCPUnifiedCall adapts one public invocation to the existing Engine
// ExecuteUnified method without owning compilation, authorization, or scheduling.
func handleMCPUnifiedCall(w http.ResponseWriter, r *http.Request, sess *mcpSession, descriptor *models.SDKUnifiedOperationDescriptor, request mcpCallRequest) {
	invocation, err := decodeMCPUnifiedInvocation(request.Params)
	// Malformed public options stop before trusted metadata or runtime state is attached.
	if err != nil {
		writeMCPCallResult(w, http.StatusBadRequest, mcpCallResponse{Error: "invalid Unified invocation"})
		return
	}
	selectors := make(map[string]*enginev1.ExecutionSelectors, len(invocation.Selectors))
	// Public target keys and their strict leaf values map without policy decisions.
	for target, value := range invocation.Selectors {
		selectors[target] = &enginev1.ExecutionSelectors{Environment: value.Environment, EndUserRef: value.EndUserRef, AuthType: value.AuthType, AuthName: value.AuthName, ResourceId: value.ResourceID}
	}
	pagination := make(map[string]*enginev1.PaginationIntent, len(invocation.Pagination))
	// Pagination remains independently keyed for canonical preflight validation.
	for target, value := range invocation.Pagination {
		pagination[target] = &enginev1.PaginationIntent{MaxPages: value.MaxPages}
	}
	ctx := ContextWithMCPExecutionTransport(r.Context())
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-app-id", sess.appID, "x-api-key", sess.token))
	// Initialization injects the existing method value; absence is a bounded server failure, not a fallback executor.
	if globalMCPUnifiedExecute == nil {
		writeMCPCallResult(w, http.StatusServiceUnavailable, mcpCallResponse{Error: "unified_execution_unavailable"})
		return
	}
	response, err := globalMCPUnifiedExecute(ctx, &enginev1.ExecuteUnifiedRequest{
		Operation: request.OperationID, Targets: invocation.Targets, InputJson: invocation.Input,
		TargetSelectors: selectors, TargetPagination: pagination, IdempotencyKey: invocation.IdempotencyKey,
	})
	// Runtime failures are collapsed to bounded codes so private definitions and provider errors never cross MCP.
	if err != nil {
		httpStatus, code := boundedMCPUnifiedError(err)
		writeMCPCallResult(w, httpStatus, mcpCallResponse{Error: code})
		return
	}
	result, httpStatus, code := projectMCPUnifiedResponse(descriptor, response)
	// Configured output failures remain bounded while successful output keeps its authored JSON shape.
	if code != "" {
		writeMCPCallResult(w, httpStatus, mcpCallResponse{Error: code})
		return
	}
	writeMCPCallResult(w, http.StatusOK, mcpCallResponse{Result: result})
}

// decodeMCPUnifiedInvocation preserves exact input JSON and supplies the same
// per-logical-call UUID default generated SDKs use when the caller omits a key.
func decodeMCPUnifiedInvocation(raw json.RawMessage) (mcpUnifiedInvocation, error) {
	var invocation mcpUnifiedInvocation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&invocation); err != nil {
		return invocation, err
	}
	// Exactly one document prevents ignored suffixes from escaping the audited request identity.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return invocation, fmt.Errorf("Unified invocation must contain one JSON document")
	}
	// Only absence defaults like the SDK; present whitespace must fail canonical
	// validation instead of being rewritten into a different request identity.
	if invocation.IdempotencyKey == "" {
		invocation.IdempotencyKey = uuid.NewString()
	}
	return invocation, nil
}

// boundedMCPUnifiedError maps only gRPC classes to stable public failures and
// deliberately excludes raw errors, operation names, selectors, and values.
func boundedMCPUnifiedError(err error) (int, string) {
	// Engine status class is enough for client recovery without exposing internals.
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest, "unified_request_invalid"
	case codes.Unauthenticated, codes.PermissionDenied:
		return http.StatusForbidden, "unified_execution_denied"
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, "unified_execution_limited"
	case codes.DeadlineExceeded, codes.Canceled:
		return http.StatusGatewayTimeout, "unified_execution_timeout"
	case codes.FailedPrecondition, codes.Unavailable:
		return http.StatusServiceUnavailable, "unified_execution_unavailable"
	default:
		return http.StatusBadGateway, "unified_execution_failed"
	}
}

// projectMCPUnifiedResponse returns configured output verbatim or the same
// camelCase all-settled fallback generated SDKs expose.
func projectMCPUnifiedResponse(descriptor *models.SDKUnifiedOperationDescriptor, response *enginev1.ExecuteUnifiedResponse) (json.RawMessage, int, string) {
	// A missing coordinator response is an Engine failure, never a successful null result.
	if response == nil {
		return nil, http.StatusBadGateway, "unified_execution_failed"
	}
	// Output mapping errors are Engine-owned bounded codes already validated by the executor.
	if response.OutputErrorCode != "" {
		return nil, http.StatusUnprocessableEntity, response.OutputErrorCode
	}
	outputConfigured := descriptor != nil && len(descriptor.OutputSchema) != 0
	// A configured output is authoritative and may never silently fall back to target data.
	if outputConfigured {
		if len(response.OutputJson) == 0 {
			return nil, http.StatusUnprocessableEntity, "output_unavailable"
		}
		return json.RawMessage(response.OutputJson), 0, ""
	}
	results := make([]mcpUnifiedResult, 0, len(response.Results))
	for _, item := range response.Results {
		results = append(results, projectMCPUnifiedResult(item))
	}
	rollbacks := make([]mcpUnifiedRollback, 0, len(response.RollbackResults))
	for _, item := range response.RollbackResults {
		rollbacks = append(rollbacks, projectMCPUnifiedRollback(item))
	}
	encoded, err := json.Marshal(struct {
		Results   []mcpUnifiedResult   `json:"results"`
		Rollbacks []mcpUnifiedRollback `json:"rollbacks"`
	}{Results: results, Rollbacks: rollbacks})
	// Projection has no cyclic values; failure is still bounded defensively.
	if err != nil {
		return nil, http.StatusBadGateway, "unified_projection_failed"
	}
	return encoded, 0, ""
}

// projectMCPUnifiedResult mirrors generated SDK normalization so malformed
// internal statuses and absent bounded codes cannot widen the public contract.
func projectMCPUnifiedResult(item *enginev1.UnifiedTargetResult) mcpUnifiedResult {
	result := mcpUnifiedResult{Target: item.GetTarget(), Status: "error", Data: json.RawMessage("null"), ErrorCode: optionalMCPString("execution_failed")}
	// Success alone may expose data; all other states redact provider output.
	if item.GetStatus() == "success" {
		result.Status, result.ErrorCode = "success", nil
		// Empty successful provider bodies become JSON null in every generated SDK.
		if len(item.GetDataJson()) != 0 {
			result.Data = json.RawMessage(item.GetDataJson())
		}
		return result
	}
	// Skipped targets use the SDK fallback and never present a recovery action.
	if item.GetStatus() == "skipped" {
		result.Status, result.ErrorCode = "skipped", optionalMCPString(defaultMCPCode(item.GetErrorCode(), "dependency_failed"))
		return result
	}
	result.ErrorCode = optionalMCPString(defaultMCPCode(item.GetErrorCode(), "execution_failed"))
	result.AuthAction = publicMCPAuthAction(item.GetAuthAction())
	return result
}

// projectMCPUnifiedRollback mirrors the SDK's two-state compensation result.
func projectMCPUnifiedRollback(item *enginev1.UnifiedRollbackResult) mcpUnifiedRollback {
	result := mcpUnifiedRollback{Target: item.GetTarget(), Status: "error", ErrorCode: optionalMCPString(defaultMCPCode(item.GetErrorCode(), "rollback_failed")), TriggeredBy: append([]string{}, item.GetTriggeredBy()...), AuthAction: publicMCPAuthAction(item.GetAuthAction())}
	// Successful compensation cannot carry a failure code or recovery action.
	if item.GetStatus() == "success" {
		result.Status, result.ErrorCode, result.AuthAction = "success", nil, nil
	}
	return result
}

// defaultMCPCode supplies the same bounded fallback used by generated SDKs.
func defaultMCPCode(value, fallback string) string {
	// Engine-provided bounded codes remain authoritative when present.
	if value != "" {
		return value
	}
	return fallback
}

// publicMCPAuthAction projects non-secret routing recovery fields with SDK camelCase names.
func publicMCPAuthAction(action *enginev1.UnifiedAuthAction) *mcpUnifiedAuthAction {
	// Absent or non-actionable guidance must remain JSON null, matching generated SDK fallbacks.
	if action == nil || action.GetAction() == "" {
		return nil
	}
	return &mcpUnifiedAuthAction{Action: action.Action, BucketID: action.BucketId, ServiceID: action.ServiceId, EndUserRef: action.EndUserRef, ConnectionID: action.ConnectionId, Reason: action.Reason}
}

// optionalMCPString preserves the SDK's null-vs-bounded-error-code distinction.
func optionalMCPString(value string) *string {
	// Successful targets and rollbacks do not manufacture an error code.
	if value == "" {
		return nil
	}
	return &value
}

// resolveSessionFixtureOperation requires the app-derived session
// fixture so one tenant can never observe a process-wide operation catalog.
func resolveSessionFixtureOperation(sess *mcpSession, operationID string) (*FixtureOperation, bool) {
	if sess == nil || sess.fixture == nil {
		return nil, false
	}
	return sess.fixture.Resolve(operationID)
}

// lookupMCPSession is a read-only accessor mirroring the RLock pattern used
// throughout mcp.go (e.g. mcpMessageHandler). Kept unexported: /mcp/call is
// the only caller outside mcp.go's own handlers.
func lookupMCPSession(sessionID string) (*mcpSession, bool) {
	mcpSessions.RLock()
	defer mcpSessions.RUnlock()
	sess, ok := mcpSessions.m[sessionID]
	return sess, ok
}

// extractBearerToken reads the session ID off the Authorization header.
// Shared-runtime sessions use their MCP sessionId as this bearer value (see
// callClient.ts) -- it is not a credential, only a lookup key into
// mcpSessions, the same non-secret identifier already used in the SSE
// transport's message URL (/mcp/message?sessionId=...).
func extractBearerToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

// dispatchMCPCall funnels one call() invocation through engineExecuteCore --
// the same credential-resolving, scope-enforcing, audited path the gRPC edge
// uses. Only non-secret user/resource selectors enter the credential envelope;
// globalSecretResolver resolves provider material server-side from the
// app scope and validated identity. The sandboxed Node process therefore
// never holds a provider credential. sess.token (not just sess.appID) remains
// required because engineExecuteCore resolves the account identity from it.
func dispatchMCPCall(ctx context.Context, sess *mcpSession, operationID string, params map[string]any) (json.RawMessage, error) {
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportMCP)
	ctx = contextWithMCPIdempotencyIdentity(ctx, params)

	buf := engine.NewBufferStream()
	err := engineExecuteCore(
		ctx, globalObjectCache, globalDispatcher, globalTokenValidator,
		sess.appID, sess.token, operationID, params, copyCredentialEnvelope(sess.authContext), "", buf,
	)
	if err != nil {
		return nil, err
	}
	return bufferToJSONResult(buf.Bytes())
}

// copyCredentialEnvelope prevents concurrent MCP calls from sharing a map
// that the secret resolver enriches with decrypted, request-local material.
func copyCredentialEnvelope(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// bufferToJSONResult forwards the vendor's response as a parsed JSON value
// when possible, so an execute script can do `const repos = await
// call(...); return repos.length` instead of having to JSON.parse a string
// itself. A non-JSON vendor response (e.g. plain text, or an empty body) is
// still returned rather than discarded or turned into an error -- as a JSON
// string -- so a shape mismatch is something the script/model can see and
// react to, not a silently eaten response.
func bufferToJSONResult(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	if json.Valid(raw) {
		return json.RawMessage(raw), nil
	}
	encoded, err := json.Marshal(string(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to encode vendor response: %w", err)
	}
	return json.RawMessage(encoded), nil
}

// writeMCPCallResult is this handler's own JSON writer rather than the
// package's existing writeError (mcp.go): writeError builds its body with
// fmt.Sprintf and does not escape the message, which breaks as soon as an
// error string contains a quote -- something this handler's validation
// errors do routinely (e.g. `missing required parameter "foo"`). Encoding
// through encoding/json avoids shipping a client-observable malformed-JSON
// bug instead of fixing it here.
func writeMCPCallResult(w http.ResponseWriter, status int, resp mcpCallResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
