package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// maxMCPPhysicalResultBytes bounds both buffered provider bytes and the exact
// JSON value returned to the sandboxed child runtime.
const maxMCPPhysicalResultBytes = 1 << 20

// mcpPaginationIntentUnknown fails closed if a newer Engine reason reaches an older MCP adapter.
const mcpPaginationIntentUnknown = "mcp_pagination_intent_invalid: omit pagination, then use search_docs to confirm whether this operation supports a lower pagination.maxPages bound"

// mcpUnifiedPhysicalPaginationCode identifies one reviewed call-shape rejection before the Unified coordinator starts.
const mcpUnifiedPhysicalPaginationCode = "mcp_physical_pagination_not_allowed_for_unified"

// mcpCallRequest preserves params as JSON until exact catalogue kind is known,
// avoiding numeric changes to Unified input while retaining physical map decoding.
type mcpCallRequest struct {
	OperationID string                       `json:"operation_id"`
	Params      json.RawMessage              `json:"params"`
	Pagination  *mcpPhysicalPaginationIntent `json:"pagination,omitempty"`
}

// mcpPhysicalPaginationIntent mirrors the generated SDK's provider-neutral page ceiling without entering provider params.
type mcpPhysicalPaginationIntent struct {
	MaxPages int `json:"maxPages"`
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
	Result            json.RawMessage `json:"result,omitempty"`
	Error             string          `json:"error,omitempty"`
	Code              string          `json:"code,omitempty"`
	RecoveryAction    string          `json:"recovery_action,omitempty"`
	ExecuteRequest    string          `json:"execute_request,omitempty"`
	ProviderExecution string          `json:"provider_execution,omitempty"`
	AutomaticReplay   *bool           `json:"automatic_replay,omitempty"`
}

// mcpCallHandler is call()'s only server-side entrypoint (design doc,
// "No I/O outside call()" -- the shared runtime's sandboxed process has no
// other path to a vendor API). This boundary resolves the authenticated
// session and catalogue kind, keeps physical execution controls separate
// from provider params, validates the public shape, then hands the request to
// the same engineExecuteCore path used by gRPC. Provider routing and pagination
// policy remain owned by the canonical Dispatcher.
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
		automaticReplay := false
		writeMCPCallResult(w, http.StatusNotFound, mcpCallResponse{
			Error: "MCP bridge session is unavailable; reinitialize the MCP connection instead of inventing a session ID",
			Code:  "MCP_BRIDGE_SESSION_UNAVAILABLE", RecoveryAction: "reinitialize_connection",
			ExecuteRequest: "reformat_if_session_state_used", ProviderExecution: "not_started", AutomaticReplay: &automaticReplay,
		})
		return
	}
	callCtx, cancel := mcpSessionRequestContext(r.Context(), sess)
	defer cancel()
	r = r.WithContext(callCtx)

	var req mcpCallRequest
	statusCode, errorCode := decodeBoundedMCPCallRequest(w, r, &req)
	// Internal child-runtime calls share the public MCP payload budget before decoding.
	if statusCode != 0 {
		recordMCPCallLimit(r.Context(), sess, errorCode)
		writeMCPCallResult(w, statusCode, mcpCallResponse{Error: errorCode})
		return
	}

	// Exact catalogue kind selects the adapter; invocation shape never grants
	// access to a private definition or changes physical dispatch behavior.
	if descriptor, unified := resolveSessionFixtureUnifiedOperation(sess, req.OperationID); unified {
		handleMCPResolvedUnifiedCall(w, r, sess, descriptor, req)
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

	params, intent, statusCode, failure := decodeMCPPhysicalCallInput(op, req)
	// Provider input and execution controls must both be admitted before dispatch.
	if statusCode != 0 {
		slog.WarnContext(r.Context(), "mcp call() rejected: physical input validation failed",
			slog.String("operation_id", req.OperationID))
		writeMCPCallResult(w, statusCode, failure)
		return
	}

	result, err := dispatchMCPCall(r.Context(), sess, req.OperationID, params, intent)
	// Physical provider failures retain their established response path.
	if err != nil {
		statusCode, failure := boundedMCPPhysicalCallResponse(req.OperationID, err)
		recordMCPCallLimit(r.Context(), sess, failure.Error)
		writeMCPCallResult(w, statusCode, failure)
		return
	}

	writeMCPCallResult(w, http.StatusOK, mcpCallResponse{Result: result})
}

// handleMCPResolvedUnifiedCall prevents physical-only options from being ignored by the logical adapter.
func handleMCPResolvedUnifiedCall(w http.ResponseWriter, r *http.Request, sess *mcpSession, descriptor *models.SDKUnifiedOperationDescriptor, request mcpCallRequest) {
	// Unified pagination remains target-keyed inside params to preserve SDK-equivalent graph semantics.
	if request.Pagination != nil {
		publicOperationID := fmt.Sprintf("%q", request.OperationID)
		message := fmt.Sprintf("%s: operation %s is Unified; use call(%s, params) without physical pagination options and keep any target-keyed pagination inside params.pagination", mcpUnifiedPhysicalPaginationCode, publicOperationID, publicOperationID)
		writeMCPCallResult(w, http.StatusBadRequest, mcpCorrectArgumentsResponse(mcpUnifiedPhysicalPaginationCode, message))
		return
	}
	handleMCPUnifiedCall(w, r, sess, descriptor, request)
}

// mcpCorrectArgumentsResponse creates the one closed bridge recovery proven to stop before provider or coordinator execution.
func mcpCorrectArgumentsResponse(code, message string) mcpCallResponse {
	automaticReplay := false
	return mcpCallResponse{
		Error: message, Code: code, RecoveryAction: "correct_execute_arguments",
		ExecuteRequest: "correct_arguments", ProviderExecution: "not_started", AutomaticReplay: &automaticReplay,
	}
}

// decodeMCPPhysicalCallInput admits provider params and the Engine-owned pagination option as separate concerns.
func decodeMCPPhysicalCallInput(operation *FixtureOperation, request mcpCallRequest) (map[string]any, *engine.PaginationIntent, int, mcpCallResponse) {
	var params map[string]any
	// Physical params preserve their flat-map contract while Unified input remains exact raw JSON.
	if len(request.Params) != 0 {
		// A present physical payload must be a JSON object before schema validation.
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, nil, http.StatusBadRequest, mcpCallResponse{Error: "invalid request body"}
		}
	}
	// Canonical provider schema validation remains independent from execution policy controls.
	if err := validateCallParams(operation, params); err != nil {
		return nil, nil, http.StatusBadRequest, mcpCallResponse{Error: err.Error()}
	}
	intent, err := decodeMCPPhysicalPaginationIntent(request.Pagination)
	// Invalid public controls stop before exact Engine resolution can trigger provider work.
	if err != nil {
		statusCode, failure := boundedMCPPhysicalCallResponse(request.OperationID, err)
		return nil, nil, statusCode, failure
	}
	return params, intent, 0, mcpCallResponse{}
}

// decodeMCPPhysicalPaginationIntent converts the public camelCase option into the canonical bounded Engine control.
func decodeMCPPhysicalPaginationIntent(value *mcpPhysicalPaginationIntent) (*engine.PaginationIntent, error) {
	// Omission preserves the operation's complete reviewed pagination policy.
	if value == nil {
		return nil, nil
	}
	intent := &engine.PaginationIntent{MaxPages: value.MaxPages}
	// Shared validation keeps MCP aligned with REST, gRPC, and Unified callers.
	if err := engine.ValidatePaginationIntent(intent); err != nil {
		return nil, err
	}
	return intent, nil
}

// decodeBoundedMCPCallRequest admits one child-runtime envelope only when its
// complete encoded body fits the same fixed budget as external MCP messages.
func decodeBoundedMCPCallRequest(w http.ResponseWriter, r *http.Request, request *mcpCallRequest) (int, string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPMessageBodyBytes))
	// Oversized bodies receive a stable code distinct from malformed envelopes.
	if err != nil {
		var maxBytesError *http.MaxBytesError
		// Only the typed limit failure may be classified as a payload rejection.
		if errors.As(err, &maxBytesError) {
			return http.StatusRequestEntityTooLarge, "mcp_call_payload_too_large"
		}
		return http.StatusBadRequest, "invalid request body"
	}
	// Unmarshal requires exactly one JSON document and rejects trailing data.
	if err := json.Unmarshal(body, request); err != nil {
		return http.StatusBadRequest, "invalid request body"
	}
	return 0, ""
}

// mcpSessionRequestContext binds bridge calls and SSE streams to session cancellation,
// preserving caller deadlines without inventing a maximum age for active sessions.
func mcpSessionRequestContext(parent context.Context, sess *mcpSession) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	// Directly constructed test sessions may rely solely on the caller's cancellation.
	if sess.lifecycleCtx == nil {
		return ctx, cancel
	}
	stopCancellation := context.AfterFunc(sess.lifecycleCtx, cancel)
	// AfterFunc runs asynchronously, so an already-ended session must fail closed immediately.
	if sess.lifecycleCtx.Err() != nil {
		cancel()
	}
	// Removing the callback avoids retaining completed calls until session teardown.
	return ctx, func() {
		stopCancellation()
		cancel()
	}
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
func dispatchMCPCall(ctx context.Context, sess *mcpSession, operationID string, params map[string]any, pagination *engine.PaginationIntent) (json.RawMessage, error) {
	ctx = contextWithExecutionTransport(ctx, models.EngineExecutionTransportMCP)
	ctx = contextWithMCPPhysicalExecutionIdentity(ctx, params, pagination)

	buf := engine.NewBoundedBufferStream(maxMCPPhysicalResultBytes)
	err := engineExecuteCore(
		ctx, globalObjectCache, globalDispatcher, globalTokenValidator,
		sess.appID, sess.token, operationID, params, copyCredentialEnvelope(sess.authContext), "", buf,
	)
	// Canonical execution errors, including the bounded-stream sentinel, retain their identity.
	if err != nil {
		return nil, err
	}
	return bufferToBoundedJSONResult(buf.Bytes(), maxMCPPhysicalResultBytes)
}

// contextWithMCPPhysicalExecutionIdentity binds pagination to replay identity and the canonical Dispatcher context.
func contextWithMCPPhysicalExecutionIdentity(ctx context.Context, params map[string]any, pagination *engine.PaginationIntent) context.Context {
	ctx = contextWithMCPIdempotencyIdentity(ctx, params)
	requestHash := engine.BindPaginationIntentRequestHash(requestBodyHashFromContext(ctx), pagination)
	ctx = contextWithExecutionIdentity(ctx, idempotencyKeyFromContext(ctx), requestHash)
	return engine.ContextWithPaginationIntent(ctx, pagination)
}

// boundedMCPPhysicalCallError projects result limits and pagination decisions into stable, actionable MCP failures.
func boundedMCPPhysicalCallError(operationID string, err error) (int, string) {
	// Provider bodies crossing the result budget never expose partial content or wrapper text.
	if errors.Is(err, engine.ErrBufferStreamLimitExceeded) {
		return http.StatusBadGateway, "mcp_call_result_too_large"
	}
	var paginationErr *engine.PaginationIntentValidationError
	// Typed caller failures include only public request and effective-limit facts safe for exact correction guidance.
	if errors.As(err, &paginationErr) {
		return http.StatusBadRequest, boundedMCPPaginationIntentError(operationID, paginationErr)
	}
	// Typed pagination failures retain a stable code and bounded recovery guidance instead of a generic wrapper.
	if code := engine.PaginationFailureCode(err); code != "" {
		return boundedMCPPaginationFailure(code)
	}
	return http.StatusBadGateway, boundedMCPCallErrorMessage(err.Error())
}

// boundedMCPPhysicalCallResponse adds closed recovery fields only when typed validation proves provider dispatch never began.
func boundedMCPPhysicalCallResponse(operationID string, err error) (int, mcpCallResponse) {
	statusCode, message := boundedMCPPhysicalCallError(operationID, err)
	response := mcpCallResponse{Error: message}
	var paginationErr *engine.PaginationIntentValidationError
	// Provider and traversal errors retain their existing outcome-unknown envelope.
	if !errors.As(err, &paginationErr) {
		return statusCode, response
	}
	code, known := mcpPaginationIntentCode(paginationErr.Reason)
	// Unknown future reasons cannot inherit a recovery decision that has not been reviewed.
	if !known {
		return statusCode, response
	}
	return statusCode, mcpCorrectArgumentsResponse(code, message)
}

// mcpPaginationIntentCode maps reviewed pre-provider reasons onto stable public codes shared by prose and recovery fields.
func mcpPaginationIntentCode(reason engine.PaginationIntentErrorReason) (string, bool) {
	// Only known reasons may become machine-actionable bridge codes.
	switch reason {
	case engine.PaginationIntentInvalidValue:
		return "mcp_pagination_max_pages_invalid", true
	case engine.PaginationIntentNotSupported:
		return "mcp_pagination_not_supported", true
	case engine.PaginationIntentBoundNotLower:
		return "mcp_pagination_bound_not_lower", true
	default:
		return "", false
	}
}

// boundedMCPPaginationIntentError gives an agent an exact call() correction using only the resolved public operation ID.
func boundedMCPPaginationIntentError(operationID string, validationErr *engine.PaginationIntentValidationError) string {
	publicOperationID := fmt.Sprintf("%q", operationID)
	code, known := mcpPaginationIntentCode(validationErr.Reason)
	// Unknown future reasons cannot borrow wording or recovery from a reviewed branch.
	if !known {
		return mcpPaginationIntentUnknown
	}
	// Each reason receives a stable code so clients can recover without parsing prose.
	switch validationErr.Reason {
	case engine.PaginationIntentInvalidValue:
		// Pre-policy validation must not recommend a bound that a non-paginated operation would reject next.
		return fmt.Sprintf("%s: operation %s received an invalid pagination.maxPages; use a positive integer lower than pagination.engine_max_pages only when search_docs exact operationId detail reports pagination.caller_bound_supported=true for this operation, otherwise use call(%s, params) without a pagination option; never reuse another operation's bound", code, publicOperationID, publicOperationID)
	case engine.PaginationIntentNotSupported:
		// The two-argument form removes the unsupported Engine control while preserving provider parameters.
		return fmt.Sprintf("%s: operation %s is not paginated; use call(%s, params) without a pagination option", code, publicOperationID, publicOperationID)
	case engine.PaginationIntentBoundNotLower:
		// The effective limit determines whether any valid positive strict reduction exists.
		return boundedMCPPaginationBoundError(code, publicOperationID, validationErr.EngineMaxPages)
	default:
		return mcpPaginationIntentUnknown
	}
}

// boundedMCPPaginationBoundError explains whether a smaller positive caller bound exists under the Engine limit.
func boundedMCPPaginationBoundError(code, publicOperationID string, engineMaxPages int) string {
	// A one-page Engine policy has no positive strict reduction, so omission is the only valid reformat.
	if engineMaxPages <= 1 {
		return fmt.Sprintf("%s: operation %s has an Engine page limit of %d, so no lower positive pagination.maxPages exists; use call(%s, params) without a pagination option", code, publicOperationID, engineMaxPages, publicOperationID)
	}
	return fmt.Sprintf("%s: operation %s has an Engine page limit of %d; use pagination.maxPages between 1 and %d, or omit pagination", code, publicOperationID, engineMaxPages, engineMaxPages-1)
}

// boundedMCPPaginationFailure maps Engine-owned pagination codes to safe status and recovery guidance.
func boundedMCPPaginationFailure(code string) (int, string) {
	// Reviewed hard limits are actionable input/result-shape failures rather than bridge outages.
	switch code {
	case "max_pages":
		return http.StatusUnprocessableEntity, "mcp_pagination_max_pages: Engine reached the operation's page limit before provider pagination ended; narrow the query, increase the provider page size, or set a lower pagination.maxPages when partial results are sufficient"
	case "max_items":
		return http.StatusUnprocessableEntity, "mcp_pagination_max_items: Engine reached the operation's item limit before provider pagination ended; narrow the provider query"
	case "max_bytes":
		return http.StatusUnprocessableEntity, "mcp_pagination_max_bytes: Engine reached the operation's pagination byte limit; narrow the provider query or response fields"
	case "max_duration":
		return http.StatusGatewayTimeout, "mcp_pagination_max_duration: provider pagination exceeded the operation's execution duration; narrow the provider query"
	case "cycle":
		return http.StatusBadGateway, "mcp_pagination_cycle: the provider repeated a continuation value; retry only after the provider pagination state changes"
	default:
		return http.StatusBadGateway, "mcp_pagination_" + code + ": Engine could not safely continue provider pagination; review the operation pagination contract"
	}
}

// boundedMCPCallErrorMessage applies the result budget to failure text as well
// as success values without exposing any rejected provider or validation text.
func boundedMCPCallErrorMessage(message string) string {
	// Reject raw oversize before quoting can allocate an even larger error representation.
	if len(message) > maxMCPPhysicalResultBytes {
		return "mcp_call_result_too_large"
	}
	encoded, _ := json.Marshal(message)
	// Control characters and quotes can expand even a small raw failure on the JSON wire.
	if len(encoded) > maxMCPPhysicalResultBytes {
		return "mcp_call_result_too_large"
	}
	return message
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

// bufferToBoundedJSONResult caps the model-visible JSON after conversion
// because quoting a text response can expand beyond its provider byte count.
func bufferToBoundedJSONResult(raw []byte, maxBytes int) (json.RawMessage, error) {
	result, err := bufferToJSONResult(raw)
	// Encoding failures remain authoritative and never become limit failures.
	if err != nil {
		return nil, err
	}
	// The post-encoding check bounds the exact result placed in the call response.
	if len(result) > maxBytes {
		return nil, engine.ErrBufferStreamLimitExceeded
	}
	return result, nil
}

// writeMCPCallResult keeps the bridge's typed result-or-error envelope separate
// from the package's generic HTTP error shape. Failure text receives the same
// bounded result policy, so early validation paths cannot bypass the writer budget.
func writeMCPCallResult(w http.ResponseWriter, status int, resp mcpCallResponse) {
	resp.Error = boundedMCPCallErrorMessage(resp.Error)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
