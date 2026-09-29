package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authselector"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	restExecutionContentType = "application/json"
	maxRESTOperationBytes    = 512
	maxRESTSelectorBytes     = 256
)

type restExecutionRuntime interface {
	ConnectAppRuntime(context.Context, uuid.UUID) error
	DisconnectAppRuntime(uuid.UUID)
	ResolvePhysicalOperationByName(context.Context, uuid.UUID, string) (sandbox.ResolvedPhysicalOperation, bool, error)
	ValidateResolvedPhysicalSelectors(sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionSelectors) error
	ExecuteResolvedPhysicalJSON(context.Context, auth.RuntimeIdentity, sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error)
}

type restExecutionRequest struct {
	Operation  string                 `json:"operation"`
	Input      json.RawMessage        `json:"input"`
	Selector   *restExecutionSelector `json:"selector,omitempty"`
	Pagination *restPaginationIntent  `json:"pagination,omitempty"`
}

// restPaginationIntent mirrors the provider-neutral public control without accepting policy-owned limits.
type restPaginationIntent struct {
	MaxPages int `json:"max_pages"`
}

type restExecutionSelector struct {
	Environment string `json:"environment,omitempty"`
	EndUserRef  string `json:"end_user_ref,omitempty"`
	AuthType    string `json:"auth_type,omitempty"`
	AuthName    string `json:"auth_name,omitempty"`
	ResourceID  string `json:"resource_id,omitempty"`
}

type restExecutionPlan struct {
	kind      string
	physical  sandbox.ResolvedPhysicalOperation
	operation string
}

type restExecutionSuccess struct {
	AppID      string `json:"app_id"`
	Operation  string `json:"operation"`
	Kind       string `json:"kind"`
	StatusCode int    `json:"status_code,omitempty"`
	Results    any    `json:"results"`
}

type restExecutionErrorEnvelope struct {
	Error restExecutionErrorBody `json:"error"`
}

type restExecutionErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type restExecutionError struct {
	status  int
	code    string
	message string
	details any
}

// MountAppExecutionRoute mounts the only runtime REST execution endpoint; all
// other app lifecycle routes remain control-plane APIs.
func MountAppExecutionRoute(router chi.Router, server *EngineGRPCServer) {
	if router == nil || server == nil {
		return
	}
	router.Post("/v1/apps/{app_id}/executions", server.handleRESTExecution)
}

// handleRESTExecution authenticates one exact SDK or Unified App version and dispatches its selected operation.
func (s *EngineGRPCServer) handleRESTExecution(writer http.ResponseWriter, request *http.Request) {
	appID, requestErr := parseRESTAppID(chi.URLParam(request, "app_id"))
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	scope, identity, requestErr := s.authenticateRESTApp(request, appID)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	// Only the family-selected ready Unified App version accepts new invocations, including raw operations.
	if scope.Kind == store.AppKindUnifiedApp {
		if requestErr := s.admitUnifiedAppTraffic(request.Context(), appID); requestErr != nil {
			writeRESTExecutionError(writer, requestErr)
			return
		}
	}
	decoded, canonical, requestErr := decodeRESTExecutionRequest(writer, request)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	// Only Unified Apps can publish authored execute; every selected raw operation retains the existing path.
	if scope.Kind == store.AppKindUnifiedApp && decoded.Operation == "execute" {
		if !s.tryUnifiedAppRun(writer, request, scope, identity, decoded) {
			writeRESTExecutionError(writer, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "unified app bundle is unavailable"))
		}
		return
	}
	// Attached hosted capabilities use the same consumer authentication and durable executor.
	if s.tryAttachedUnifiedAppRun(writer, request, identity, decoded) { return }
	s.handleRawRESTExecution(writer, request, appID, scope, identity, decoded, canonical)
}

// handleRawRESTExecution preserves the existing selected physical and Unified operation dispatcher.
func (s *EngineGRPCServer) handleRawRESTExecution(writer http.ResponseWriter, request *http.Request, appID uuid.UUID, scope *store.AppRuntime, identity auth.RuntimeIdentity, decoded restExecutionRequest, canonical []byte) {
	if s.restRuntime == nil || s.restRuntime.ConnectAppRuntime(request.Context(), appID) != nil {
		writeRESTExecutionError(writer, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "app runtime is unavailable"))
		return
	}
	defer s.restRuntime.DisconnectAppRuntime(appID)
	plan, requestErr := s.classifyRESTExecution(request.Context(), scope, decoded)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	idempotencyKey, requestErr := restIdempotencyKey(request)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	ctx := sandbox.ContextWithRESTExecutionTransport(request.Context())
	response, requestErr := s.executeRESTPlan(ctx, scope, identity, decoded, canonical, plan, idempotencyKey)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	writeRESTExecutionJSON(writer, http.StatusOK, response)
}

// parseRESTAppID validates only public path syntax; token validation remains
// the authority for whether any syntactically valid app identity exists.
func parseRESTAppID(raw string) (uuid.UUID, *restExecutionError) {
	appID, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || appID == uuid.Nil {
		return uuid.Nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "app_id must be a valid UUID")
	}
	return appID, nil
}

// authenticateRESTApp accepts only one family execution bearer token and
// binds it to the exact SDK or Unified App runtime selected by the path.
func (s *EngineGRPCServer) authenticateRESTApp(request *http.Request, appID uuid.UUID) (*store.AppRuntime, auth.RuntimeIdentity, *restExecutionError) {
	token, err := restBearerToken(request)
	if err != nil {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusUnauthorized, "authentication_required", "a Bearer app token is required")
	}
	if s.tokenValidator == nil {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "app authentication is unavailable")
	}
	identity, err := s.tokenValidator.Validate(request.Context(), appID, token)
	if err != nil || identity.AppID != appID {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusUnauthorized, "authentication_failed", "app authentication failed")
	}
	scope, err := s.store.GetAppRuntime(request.Context(), appID)
	if err != nil || !validRESTAppScope(scope, identity, appID) {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusForbidden, "app_scope_unavailable", "app scope is unavailable")
	}
	// Authentication succeeds only for a runtime whose nested selection
	// contract is current; otherwise a removed field could widen execution.
	if _, err := models.DecodeAppSelections(scope.ScopeSchemaVersion, scope.Selections); err != nil {
		return nil, auth.RuntimeIdentity{}, newRESTExecutionError(http.StatusForbidden, "app_scope_unavailable", "app scope is unavailable")
	}
	return scope, identity, nil
}

// restBearerToken rejects duplicate, alternate-scheme, and whitespace-bearing
// credentials without ever copying token material into an error.
func restBearerToken(request *http.Request) (string, error) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", errors.New("Bearer token is required")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("Bearer token is invalid")
	}
	return token, nil
}

// validRESTAppScope requires one exact SDK or Unified App runtime with matching token kind.
func validRESTAppScope(scope *store.AppRuntime, identity auth.RuntimeIdentity, appID uuid.UUID) bool {
	return scope != nil && scope.AppID == appID && scope.AppID == identity.AppID &&
		scope.AccountID == identity.AccountID && scope.BucketID != uuid.Nil && scope.Kind == identity.Kind &&
		(scope.Kind == store.AppKindSDK || scope.Kind == store.AppKindUnifiedApp)
}

// decodeRESTExecutionRequest enforces one bounded canonical JSON document and
// rejects duplicate or unknown fields before runtime definitions select a kind.
func decodeRESTExecutionRequest(writer http.ResponseWriter, request *http.Request) (restExecutionRequest, []byte, *restExecutionError) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != restExecutionContentType {
		return restExecutionRequest{}, nil, newRESTExecutionError(http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be application/json")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, canonicaljson.MaxInputBytes))
	if err != nil || len(raw) == 0 {
		return restExecutionRequest{}, nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "request body must be one bounded JSON document")
	}
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return restExecutionRequest{}, nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "request body must be one bounded JSON document")
	}
	var decoded restExecutionRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return restExecutionRequest{}, nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "request body contains unsupported fields")
	}
	if err := validateRESTExecutionRequest(decoded); err != nil {
		return restExecutionRequest{}, nil, err
	}
	return decoded, canonical, nil
}

// validateRESTExecutionRequest checks physical input and routing fields before dispatch.
func validateRESTExecutionRequest(request restExecutionRequest) *restExecutionError {
	// Operation names must stay bounded before exact runtime lookup.
	if request.Operation == "" || len(request.Operation) > maxRESTOperationBytes || strings.TrimSpace(request.Operation) != request.Operation {
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "operation is required and must be bounded")
	}
	if len(request.Input) == 0 {
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "input is required")
	}
	if err := validateRESTSelector(request.Selector); err != nil {
		return err
	}
	if err := validateRESTPaginationIntent(request.Pagination); err != nil {
		return err
	}
	return nil
}

// validateRESTPaginationIntent applies the shared caller bound while retaining REST's stable error envelope.
func validateRESTPaginationIntent(value *restPaginationIntent) *restExecutionError {
	// Omitted intent retains the operation's automatic pagination behavior.
	if value == nil {
		return nil
	}
	if err := engine.ValidatePaginationIntent(&engine.PaginationIntent{MaxPages: value.MaxPages}); err != nil {
		return newRESTExecutionError(http.StatusBadRequest, "pagination_invalid", "pagination intent is invalid")
	}
	return nil
}

// validateRESTSelector applies the same bounded non-secret routing vocabulary
// before a selector reaches physical contract preflight.
func validateRESTSelector(selector *restExecutionSelector) *restExecutionError {
	if selector == nil {
		return nil
	}
	values := []string{selector.Environment, selector.EndUserRef, selector.AuthName, selector.ResourceID}
	for _, value := range values {
		if len(value) > maxRESTSelectorBytes || value != strings.TrimSpace(value) {
			return newRESTExecutionError(http.StatusBadRequest, "selector_invalid", "selector values must be bounded")
		}
	}
	// Only provider auth selectors recognized by physical execution may enter dispatch.
	if !validRESTAuthType(selector.AuthType) {
		return newRESTExecutionError(http.StatusBadRequest, "selector_invalid", "auth_type must be one of api_key, oauth, oidc, basic, bearer, or mtls")
	}
	if selector.ResourceID != "" {
		if _, err := uuid.Parse(selector.ResourceID); err != nil {
			return newRESTExecutionError(http.StatusBadRequest, "selector_invalid", "resource_id must be a UUID")
		}
	}
	return nil
}

// validRESTAuthType admits the physical routing vocabulary without accepting arbitrary auth mechanisms.
func validRESTAuthType(value string) bool {
	switch value {
	case "", "api_key", "oauth", "oidc", "basic", "bearer", "mtls":
		return true
	default:
		return false
	}
}

// classifyRESTExecution resolves only selected physical operations after graph retirement.
func (s *EngineGRPCServer) classifyRESTExecution(ctx context.Context, scope *store.AppRuntime, request restExecutionRequest) (restExecutionPlan, *restExecutionError) {
	physical, physicalFound, err := s.restRuntime.ResolvePhysicalOperationByName(ctx, scope.AppID, request.Operation)
	if errors.Is(err, sandbox.ErrPhysicalOperationAmbiguous) {
		return restExecutionPlan{}, newRESTExecutionError(http.StatusConflict, "operation_ambiguous", "operation resolves to multiple physical definitions")
	}
	if err != nil {
		return restExecutionPlan{}, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "physical operation definitions are unavailable")
	}
	// A removed graph name has no executable REST route even if its old definition remains stored.
	if !physicalFound {
		return restExecutionPlan{}, newRESTExecutionError(http.StatusNotFound, "operation_not_found", "operation is not defined for this app")
	}
	return restExecutionPlan{kind: "physical", physical: physical, operation: request.Operation}, nil
}

// restIdempotencyKey validates the bounded replay key supplied to physical execution.
func restIdempotencyKey(request *http.Request) (string, *restExecutionError) {
	values := request.Header.Values("Idempotency-Key")
	if len(values) > 1 {
		return "", newRESTExecutionError(http.StatusBadRequest, "invalid_request", "Idempotency-Key must be supplied once")
	}
	value := ""
	if len(values) == 1 {
		value = values[0]
	}
	if len(value) > maxRESTSelectorBytes || value != strings.TrimSpace(value) {
		return "", newRESTExecutionError(http.StatusBadRequest, "invalid_request", "Idempotency-Key must be bounded")
	}
	return value, nil
}

// executeRESTPlan uses the selected physical dispatcher for raw operations.
func (s *EngineGRPCServer) executeRESTPlan(ctx context.Context, scope *store.AppRuntime, identity auth.RuntimeIdentity, request restExecutionRequest, canonical []byte, plan restExecutionPlan, idempotencyKey string) (restExecutionSuccess, *restExecutionError) {
	return s.executeRESTPhysical(ctx, identity, request, canonical, plan, idempotencyKey)
}

// executeRESTPhysical validates its singular selector, binds full public
// intent to idempotency, and collects one successful bounded JSON response.
func (s *EngineGRPCServer) executeRESTPhysical(ctx context.Context, identity auth.RuntimeIdentity, request restExecutionRequest, canonical []byte, plan restExecutionPlan, idempotencyKey string) (restExecutionSuccess, *restExecutionError) {
	selectors := physicalRESTSelectors(request.Selector)
	// Typed auth corrections must reach the shared public projector instead of being collapsed into the legacy selector mask.
	if err := s.restRuntime.ValidateResolvedPhysicalSelectors(plan.physical, selectors); err != nil {
		return restExecutionSuccess{}, restErrorFromExecution(err)
	}
	pagination := runtimeRESTPaginationIntent(request.Pagination)
	if err := sandbox.ValidateResolvedPhysicalPaginationIntent(plan.physical, pagination); err != nil {
		return restExecutionSuccess{}, newRESTExecutionError(http.StatusBadRequest, "pagination_invalid", "pagination intent is incompatible with this operation")
	}
	input, err := decodeRESTInput(request.Input)
	if err != nil {
		return restExecutionSuccess{}, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "input must be a JSON object")
	}
	requestHash := restPhysicalRequestHash(request, canonical)
	result, err := s.restRuntime.ExecuteResolvedPhysicalJSON(ctx, identity, plan.physical, sandbox.PhysicalExecutionRequest{
		Params: input, Credentials: physicalRESTCredentials(request.Selector), Environment: selectors.Environment,
		IdempotencyKey: idempotencyKey, RequestBodyHash: requestHash, Pagination: pagination, Transport: models.EngineExecutionTransportREST,
	})
	if err != nil {
		return restExecutionSuccess{}, restErrorFromExecution(err)
	}
	return restExecutionSuccess{
		AppID: identity.AppID.String(), Operation: plan.operation, Kind: plan.kind,
		StatusCode: result.StatusCode, Results: []json.RawMessage{json.RawMessage(result.Body)},
	}, nil
}

// restPhysicalRequestHash leaves pagination for the shared physical hash binder while retaining every other public REST input.
func restPhysicalRequestHash(request restExecutionRequest, fallback []byte) string {
	// Requests without pagination already have the complete canonical replay identity.
	if request.Pagination == nil {
		digest := sha256.Sum256(fallback)
		return hex.EncodeToString(digest[:])
	}
	request.Pagination = nil
	encoded, err := json.Marshal(request)
	// Strict decoding makes marshal failure unreachable, but the original canonical request remains a safe deterministic fallback.
	if err != nil {
		digest := sha256.Sum256(fallback)
		return hex.EncodeToString(digest[:])
	}
	canonical, err := canonicaljson.Canonicalize(encoded)
	if err != nil {
		digest := sha256.Sum256(fallback)
		return hex.EncodeToString(digest[:])
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

// runtimeRESTPaginationIntent copies a validated REST control into the internal physical request.
func runtimeRESTPaginationIntent(value *restPaginationIntent) *engine.PaginationIntent {
	// Message absence must remain distinguishable from a present invalid zero value.
	if value == nil {
		return nil
	}
	return &engine.PaginationIntent{MaxPages: value.MaxPages}
}

// decodeRESTInput preserves JSON number precision by using json.Number until
// the canonical physical dispatcher performs its normal parameter mapping.
func decodeRESTInput(raw json.RawMessage) (map[string]any, error) {
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("physical input must be an object")
	}
	return input, nil
}

// physicalRESTSelectors maps only the five public non-secret routing fields to
// the canonical physical selector preflight DTO.
func physicalRESTSelectors(selector *restExecutionSelector) sandbox.PhysicalExecutionSelectors {
	if selector == nil {
		return sandbox.PhysicalExecutionSelectors{}
	}
	return sandbox.PhysicalExecutionSelectors{
		Environment: selector.Environment, EndUserRef: selector.EndUserRef,
		AuthType: selector.AuthType, AuthName: selector.AuthName, ResourceID: selector.ResourceID,
	}
}

// physicalRESTCredentials converts public selectors into reserved routing
// metadata only; arbitrary provider credentials are never accepted.
func physicalRESTCredentials(selector *restExecutionSelector) map[string]any {
	if selector == nil {
		return nil
	}
	credentials := make(map[string]any, 4)
	addRESTCredential(credentials, "fused_end_user_ref", selector.EndUserRef)
	addRESTCredential(credentials, "fused_auth_type", selector.AuthType)
	addRESTCredential(credentials, "fused_auth_name", selector.AuthName)
	addRESTCredential(credentials, "fused_resource_id", selector.ResourceID)
	return credentials
}

// addRESTCredential omits empty selectors so downstream default selection
// remains identical to the generated SDK path.
func addRESTCredential(credentials map[string]any, key, value string) {
	if value != "" {
		credentials[key] = value
	}
}

// restErrorFromExecution reduces internal failures to stable messages and
// never returns provider response bodies or raw error text.
func restErrorFromExecution(err error) *restExecutionError {
	if actionable := restActionableAuthError(err); actionable != nil {
		return actionable
	}
	if environment := restEnvironmentError(err); environment != nil {
		return environment
	}
	if physical := restPhysicalExecutionError(err); physical != nil {
		return physical
	}
	return restErrorFromStatus(err)
}

// restPhysicalExecutionError maps canonical physical replay, policy, media,
// and cancellation decisions without exposing raw provider errors.
func restPhysicalExecutionError(err error) *restExecutionError {
	switch {
	case errors.Is(err, store.ErrIdempotencyKeyConflict):
		return newRESTExecutionError(http.StatusConflict, "idempotency_conflict", "Idempotency-Key was reused for different input")
	case errors.Is(err, sandbox.ErrPhysicalOperationNotAllowed):
		return newRESTExecutionError(http.StatusForbidden, "operation_not_allowed", "token does not allow this operation")
	case errors.Is(err, sandbox.ErrPhysicalSelectorContract):
		return newRESTExecutionError(http.StatusBadRequest, "selector_invalid", "selector is incompatible with this operation")
	case errors.Is(err, sandbox.ErrPhysicalResponseNotJSON):
		return newRESTExecutionError(http.StatusBadGateway, "response_not_json", "provider response is not one JSON document")
	case errors.Is(err, sandbox.ErrPhysicalResponseTooLarge):
		return newRESTExecutionError(http.StatusBadGateway, "response_too_large", "provider response exceeds the REST JSON limit")
	case errors.Is(err, sandbox.ErrPhysicalResponseStatus):
		return newRESTExecutionErrorWithDetails(http.StatusBadGateway, "provider_error", "provider returned an unsuccessful response", restProviderHTTPStatusDetails(err))
	case errors.Is(err, context.DeadlineExceeded):
		return newRESTExecutionError(http.StatusGatewayTimeout, "execution_timeout", "execution timed out")
	case errors.Is(err, context.Canceled):
		return newRESTExecutionError(http.StatusGatewayTimeout, "execution_timeout", "execution was cancelled")
	default:
		return nil
	}
}

// restProviderHTTPStatusDetails exposes only a collector-owned numeric status,
// never provider headers, body content, or raw transport error text.
func restProviderHTTPStatusDetails(err error) map[string]any {
	var responseErr *sandbox.PhysicalResponseStatusError
	// Only the typed collector error proves that the number came from an HTTP status frame.
	if !errors.As(err, &responseErr) || responseErr.StatusCode < 100 || responseErr.StatusCode > 599 {
		return nil
	}
	return map[string]any{"provider_http_status": responseErr.StatusCode}
}

// restActionableAuthError projects only Engine-owned credential and connection
// routing fields needed for the caller to complete or repair authorization.
func restActionableAuthError(err error) *restExecutionError {
	var selector *authselector.NotFoundError
	// Invalid selector pairs stop before provider dispatch and can safely return only sanitized operation-local choices.
	if errors.As(err, &selector) {
		return newRESTExecutionErrorWithDetails(http.StatusBadRequest, selector.Code, selector.Message, map[string]any{
			"requested": selector.Requested, "available": selector.Available, "remediation": selector.Remediation,
		})
	}
	var missing *sandbox.CredentialMaterialMissingError
	// Static credential absence is proven before provider construction and can
	// safely expose only routing identity plus a value-free setup command.
	if errors.As(err, &missing) {
		return newRESTExecutionErrorWithDetails(http.StatusConflict, "bucket_credentials_missing", "provider credentials are not configured", map[string]any{
			"bucket_id": missing.BucketID, "service_id": missing.ServiceID,
			"auth_type": missing.AuthType, "auth_name": missing.AuthName, "command": missing.Command(),
		})
	}
	var connection *sandbox.ConnectionRequiredError
	if errors.As(err, &connection) {
		return newRESTExecutionErrorWithDetails(http.StatusConflict, "connection_required", "a provider connection is required", map[string]any{
			"bucket_id": connection.BucketID, "service_id": connection.ServiceID, "end_user_ref": connection.EndUserRef,
		})
	}
	var reconnect *sandbox.ReconnectRequiredError
	if errors.As(err, &reconnect) {
		return newRESTExecutionErrorWithDetails(http.StatusConflict, "reconnect_required", "the provider connection must be reauthorized", map[string]any{
			"bucket_id": reconnect.BucketID, "service_id": reconnect.ServiceID, "end_user_ref": reconnect.EndUserRef,
			"connection_id": reconnect.ConnectionID, "reason": reconnect.Reason,
		})
	}
	var resource *sandbox.ResourceSelectionRequiredError
	if errors.As(err, &resource) {
		return newRESTExecutionErrorWithDetails(http.StatusConflict, "resource_selection_required", "a provider resource must be selected", map[string]any{
			"bucket_id": resource.BucketID, "service_id": resource.ServiceID, "end_user_ref": resource.EndUserRef,
			"connection_id": resource.ConnectionID, "reason": resource.Reason,
		})
	}
	return nil
}

// restEnvironmentError preserves only the safe bounded environment choices
// already defined by the canonical runtime error contract.
func restEnvironmentError(err error) *restExecutionError {
	var unsupported *sandbox.EnvironmentNotSupportedError
	if errors.As(err, &unsupported) {
		return newRESTExecutionErrorWithDetails(http.StatusBadRequest, "environment_not_supported", "requested environment is not supported", map[string]any{
			"requested": unsupported.Requested, "available": unsupported.Available,
		})
	}
	var missing *sandbox.DefaultEnvironmentNotConfiguredError
	if errors.As(err, &missing) {
		return newRESTExecutionErrorWithDetails(http.StatusConflict, "default_environment_not_configured", "a default environment is not configured", map[string]any{
			"available": missing.Available,
		})
	}
	return nil
}

// restErrorFromStatus maps canonical gRPC admission decisions to REST without
// exposing their possibly contextual internal messages.
func restErrorFromStatus(err error) *restExecutionError {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execution request is invalid")
	case codes.PermissionDenied, codes.Unauthenticated:
		return newRESTExecutionError(http.StatusForbidden, "operation_not_allowed", "execution is not allowed")
	case codes.ResourceExhausted:
		return newRESTExecutionError(http.StatusTooManyRequests, "execution_limited", "execution limit was reached")
	case codes.FailedPrecondition, codes.Unavailable:
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "app runtime is unavailable")
	case codes.DeadlineExceeded:
		return newRESTExecutionError(http.StatusGatewayTimeout, "execution_timeout", "execution timed out")
	default:
		return newRESTExecutionError(http.StatusBadGateway, "execution_failed", "execution failed")
	}
}

// newRESTExecutionError constructs only server-owned status, code, and message
// values so caller/provider data cannot leak through the error envelope.
func newRESTExecutionError(statusCode int, code, message string) *restExecutionError {
	return &restExecutionError{status: statusCode, code: code, message: message}
}

// newRESTExecutionErrorWithDetails attaches only explicitly projected safe
// action metadata to the frozen error envelope.
func newRESTExecutionErrorWithDetails(statusCode int, code, message string, details any) *restExecutionError {
	return &restExecutionError{status: statusCode, code: code, message: message, details: details}
}

// writeRESTExecutionError writes the frozen bounded error envelope.
func writeRESTExecutionError(writer http.ResponseWriter, executionErr *restExecutionError) {
	if executionErr == nil {
		executionErr = newRESTExecutionError(http.StatusInternalServerError, "internal_error", "internal execution error")
	}
	writeRESTExecutionJSON(writer, executionErr.status, restExecutionErrorEnvelope{
		Error: restExecutionErrorBody{Code: executionErr.code, Message: executionErr.message, Details: executionErr.details},
	})
}

// writeRESTExecutionJSON emits one non-streaming JSON document for both
// success and error responses.
func writeRESTExecutionJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", restExecutionContentType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(statusCode)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
