package sandbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// PhysicalExecutionRequest contains in-flight provider inputs only. Callers
// must derive child idempotency identity on ctx before entering this boundary.
type PhysicalExecutionRequest struct {
	Params          map[string]any
	Credentials     map[string]any
	Environment     string
	IdempotencyKey  string
	RequestBodyHash string
	Pagination      *engine.PaginationIntent
	// Transport is a server-owned ingress label. Public callers cannot set it;
	// adapters use it to preserve REST across Unified child goroutines.
	Transport string
}

// ValidateResolvedPhysicalPaginationIntent checks request controls against the exact immutable operation before fanout begins.
func ValidateResolvedPhysicalPaginationIntent(operation ResolvedPhysicalOperation, intent *engine.PaginationIntent) error {
	// Omitted intent preserves the resolved operation's automatic pagination behavior.
	if intent == nil {
		return nil
	}
	// Invalid opaque handles must fail closed before their cached service metadata is read.
	if operation.match == nil || operation.match.service == nil || !operation.match.allowed {
		return engine.ErrPaginationIntentInvalid
	}
	object := fusedToIntegrationObject(operation.match.service, operation.match.endpoint)
	return engine.ValidatePaginationIntentPolicy(intent, object.Pagination)
}

// ErrPhysicalOperationNotAllowed is the stable pre-provider authorization
// decision exposed to in-process transport adapters.
var ErrPhysicalOperationNotAllowed = errors.New("ScopeError: operation not allowed by token")

type physicalResultValidator func() error

// ExecuteResolvedPhysicalJSON keeps collection and validation inside the
// physical accounting boundary, so collector failures are audited and counted
// as failures of this child execution.
func ExecuteResolvedPhysicalJSON(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	operation ResolvedPhysicalOperation,
	request PhysicalExecutionRequest,
) (result PhysicalExecutionResult, err error) {
	collector := newBoundedJSONResponseCollector()
	validate := func() error {
		var validationErr error
		result, validationErr = collector.Result()
		return validationErr
	}
	err = executeResolvedPhysicalBoundary(ctx, dispatcher, identity, operation, request, collector, validate)
	return result, err
}

// ExecuteResolvedPhysicalSuccess runs a compensation through the canonical
// physical boundary while accepting successful bodyless provider responses.
func ExecuteResolvedPhysicalSuccess(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	operation ResolvedPhysicalOperation,
	request PhysicalExecutionRequest,
) error {
	collector := &successfulResponseCollector{}
	return executeResolvedPhysicalBoundary(ctx, dispatcher, identity, operation, request, collector, collector.Result)
}

// executeResolvedPhysicalBoundary applies authorization, concurrency, tracing,
// and accounting exactly once around one pre-resolved provider dispatch.
func executeResolvedPhysicalBoundary(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	operation ResolvedPhysicalOperation,
	request PhysicalExecutionRequest,
	stream engine.ResponseStream,
	validate physicalResultValidator,
) (err error) {
	match, err := operation.matchForIdentity(identity)
	if err != nil {
		return err
	}
	ctx = preparePhysicalExecutionContext(ctx, request)
	executionStarted := time.Now()
	ctx, span := startPhysicalExecutionSpan(ctx, identity, match.endpoint.Name)
	defer span.End()
	decrement, err := trackAuthenticatedExecution(identity, span)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer decrement()
	auditState := executionAuditState{identity: identity, endpointName: match.endpoint.Name, startedAt: executionStarted, match: match}
	defer finalizePhysicalExecution(ctx, span, &auditState, &err)
	if err = authorizePhysicalOperation(identity, match.endpoint.Name, span); err != nil {
		return err
	}
	recordExecutionContractNegotiation(span, match.service.ExecutionContractEnvelope, nil)
	err = executeResolvedProviderOperation(ctx, dispatcher, identity, match, request, stream, span, &auditState, executionStarted)
	if err == nil && validate != nil {
		err = validate()
		if err != nil {
			span.SetStatus(codes.Error, executionFailureDescription(err, auditState.providerHTTPStatus))
		}
	}
	return err
}

// preparePhysicalExecutionContext attaches replay identity, timings, and an
// optional server-owned transport to one physical child context.
func preparePhysicalExecutionContext(ctx context.Context, request PhysicalExecutionRequest) context.Context {
	ctx = engine.ContextWithExecutionTimings(ctx, engine.NewExecutionTimings())
	requestHash := engine.BindPaginationIntentRequestHash(request.RequestBodyHash, request.Pagination)
	ctx = contextWithExecutionIdentity(ctx, request.IdempotencyKey, requestHash)
	ctx = engine.ContextWithPaginationIntent(ctx, request.Pagination)
	// Empty transport preserves the historical SDK default. Explicit REST is
	// carried on each child so scheduler goroutines cannot lose attribution.
	if request.Transport != "" {
		ctx = contextWithExecutionTransport(ctx, request.Transport)
	}
	return engine.ContextWithIdempotencyKeyPresent(ctx, strings.TrimSpace(request.IdempotencyKey) != "")
}

// matchForIdentity prevents an opaque operation resolved for one app from being replayed under another identity.
func (operation ResolvedPhysicalOperation) matchForIdentity(identity auth.RuntimeIdentity) (*scopedEndpoint, error) {
	if operation.match == nil || operation.match.service == nil || !operation.match.allowed {
		return nil, errors.New("resolved physical operation is invalid")
	}
	if identity.AppID == uuid.Nil || operation.appID != identity.AppID {
		return nil, errors.New("ScopeError: resolved physical operation belongs to another app")
	}
	return operation.match, nil
}

// startPhysicalExecutionSpan opens the bounded child span owned by one physical call.
func startPhysicalExecutionSpan(ctx context.Context, identity auth.RuntimeIdentity, endpointName string) (context.Context, trace.Span) {
	return otel.Tracer("engine").Start(ctx, "engine.dispatch.execute", trace.WithAttributes(
		attribute.String("app.id", identity.AppID.String()),
		attribute.String("endpoint_name", endpointName),
		attribute.Bool("idempotency_key_present", idempotencyKeyFromContext(ctx) != ""),
		attribute.Bool("request_body_hash_present", requestBodyHashFromContext(ctx) != ""),
		attribute.Int("execution.contract_version", 0),
		attribute.Int("execution.required_capabilities_count", 0),
		attribute.String("execution.contract_negotiation.outcome", "not_reached"),
	))
}

// authorizePhysicalOperation enforces token policy before the physical provider call begins.
func authorizePhysicalOperation(identity auth.RuntimeIdentity, endpointName string, span trace.Span) error {
	if identity.AllowsOperation(endpointName) {
		span.SetAttributes(attribute.String("authorization.outcome", "allowed"))
		return nil
	}
	span.SetAttributes(attribute.String("authorization.outcome", "denied"))
	err := ErrPhysicalOperationNotAllowed
	span.SetStatus(codes.Error, err.Error())
	return err
}

// finalizePhysicalExecution records one receipt and usage outcome even if the caller disconnects.
func finalizePhysicalExecution(ctx context.Context, span trace.Span, state *executionAuditState, execErr *error) {
	auditCtx := context.WithoutCancel(ctx)
	recordEngineExecutionAudit(auditCtx, span, *state, *execErr)
	recordEngineExecutionUsage(auditCtx, span, *state, *execErr)
}

// executeResolvedProviderOperation reuses the direct execution path after exact
// operation admission has already selected the service and endpoint.
func executeResolvedProviderOperation(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	match *scopedEndpoint,
	request PhysicalExecutionRequest,
	stream engine.ResponseStream,
	span trace.Span,
	auditState *executionAuditState,
	resolutionStarted time.Time,
) (err error) {
	ctx, cancelExecution, executionTimeoutMs := contextWithExecutionPolicyTimeout(ctx, match.service.TimeoutMs, span)
	defer cancelExecution()
	defer func() { err = normalizeExecutionTimeout(ctx, err, executionTimeoutMs) }()
	if dispatcher == nil {
		span.SetStatus(codes.Error, "dispatcher not initialized")
		return errors.New("engine dispatcher not initialized")
	}
	params, obj := prepareResolvedProviderInputs(ctx, match, request.Params)
	if replayed, replayErr := tryReplayFromIdempotencyCache(ctx, span, identity.AppID, obj, stream, auditState); replayErr != nil || replayed {
		engine.MeasureExecutionTiming(ctx, "engine_resolution_total", resolutionStarted)
		return replayErr
	}
	credentials, bucketValues, err := prepareResolvedProviderCredentials(ctx, identity, match, obj, request.Credentials)
	if err != nil {
		return err
	}
	ctx, err = contextWithProviderRateLimitIdentity(ctx, identity.AccountID, credentials)
	if err != nil {
		return err
	}
	engine.MeasureExecutionTiming(ctx, "engine_resolution_total", resolutionStarted)
	return dispatchResolvedProvider(ctx, dispatcher, identity, match, obj, params, credentials, bucketValues, request.Environment, stream, span, auditState, executionTimeoutMs)
}

// prepareResolvedProviderInputs injects replay headers and maps the exact endpoint to the dispatcher object.
func prepareResolvedProviderInputs(ctx context.Context, match *scopedEndpoint, params map[string]any) (map[string]any, *models.IntegrationObject) {
	headersStarted := time.Now()
	params = paramsWithExecutionHeaders(params, idempotencyKeyFromContext(ctx), requestBodyHashFromContext(ctx))
	engine.MeasureExecutionTiming(ctx, "execution_headers_inject", headersStarted)
	objectMapStarted := time.Now()
	object := fusedToIntegrationObject(match.service, match.endpoint)
	engine.MeasureExecutionTiming(ctx, "integration_object_map", objectMapStarted)
	return params, object
}

// prepareResolvedProviderCredentials adds resource requirements and resolves bucket or connected-auth credentials.
func prepareResolvedProviderCredentials(
	ctx context.Context,
	identity auth.RuntimeIdentity,
	match *scopedEndpoint,
	object *models.IntegrationObject,
	credentials map[string]any,
) (map[string]any, []store.BucketValue, error) {
	credentials = withConnectedResourceRequirement(credentials, match.service.ConnectConfig)
	return resolveMatchedExecutionCredentials(ctx, match, object, identity, credentials)
}

// dispatchResolvedProvider records resolved environment and HTTP status around the existing dispatcher and cache path.
func dispatchResolvedProvider(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	match *scopedEndpoint,
	object *models.IntegrationObject,
	params, credentials map[string]any,
	bucketValues []store.BucketValue,
	environment string,
	stream engine.ResponseStream,
	span trace.Span,
	auditState *executionAuditState,
	executionTimeoutMs int,
) error {
	resolution, status, err := dispatchAndCache(ctx, dispatcher, match, object, params, credentials, bucketValues, environment, stream, span, identity.AppID)
	// The MCP adapter can turn a pre-provider connected-auth miss into exact client-header guidance without changing SDK behavior.
	err = classifyMCPEndUserRefRequirement(ctx, identity, match.service.AuthConfigs, object.SecurityRequirements, credentials, err)
	auditState.providerHTTPStatus = status
	auditState.selectedEnvironment = resolution.Environment
	auditState.environmentSource = resolution.Source
	auditState.providerHost = providerHost(resolution.BaseURL)
	return finishExecutionDispatch(ctx, span, status, executionTimeoutMs, err)
}
