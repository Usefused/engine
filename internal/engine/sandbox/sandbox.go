package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/mcpsession"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type mcpSession struct {
	metadataMu         sync.Mutex
	clientMetadata     mcpsession.Metadata
	clientInfoRecorded bool
	appID              string
	sessionID          string
	tokenID            uuid.UUID
	protocolVersion    string
	transport          string
	cmd                *exec.Cmd
	stdin              io.WriteCloser
	cancel             context.CancelFunc
	lifecycleCtx       context.Context
	requestMu          sync.Mutex
	pendingRequests    map[string]struct{}
	searchTelemetry    map[string]*mcpSearchObservation
	pendingMu          sync.Mutex
	idleTimer          *time.Timer
	responses          chan string
	token              string
	activityMu         sync.Mutex
	ended              bool // Guarded by activityMu so late activity cannot rearm a retired session's timer.
	lastActivityAt     time.Time

	// fixture is this session's own operation catalog, built at connect time
	// from the app version's AppRuntime.Selections (mcp_session_fixture.go), scoping
	// search_docs/call() to exactly what its owner selected. A missing fixture
	// fails closed instead of exposing a process-wide catalog.
	fixture *Fixture
	// authContext is supplied by the MCP client at session establishment and
	// never exposed as a tool argument. It mirrors the fused envelope an SDK
	// method would send while keeping identity/routing out of model-authored JS.
	authContext map[string]any
}

var globalNATSClient *messaging.NATSClient
var cfg *config.Config
var globalObjectCache ObjectCache

// globalDispatcher is the single vendor-call execution path (retries, SSE,
// pagination, auth injection). Wired in InitSandbox and shared by the gRPC edge
// and the MCP tool-call path.
var globalDispatcher *engine.Dispatcher
var globalTokenValidator auth.TokenValidator
var globalSecretResolver SecretResolver
var globalMCPUnifiedExecute MCPUnifiedExecuteFunc

// MCPUnifiedExecuteFunc is the existing Engine ExecuteUnified method value;
// injecting it avoids a second graph executor and keeps the package boundary acyclic.
type MCPUnifiedExecuteFunc func(context.Context, *enginev1.ExecuteUnifiedRequest) (*enginev1.ExecuteUnifiedResponse, error)

// SetSecretResolver replaces the process-wide resolver used by every physical
// execution path and returns the prior value for bounded fixture restoration.
// Startup and integrations use this boundary so direct and Unified calls cannot
// accidentally resolve credentials differently.
func SetSecretResolver(resolver SecretResolver) SecretResolver {
	previous := globalSecretResolver
	globalSecretResolver = resolver
	return previous
}

// globalEnginePort is this process's own HTTP port, handed to shared-runtime
// sessions (as FUSED_ENGINE_PORT, see mcp.go buildMCPCommand) so their
// call() implementation knows where to reach POST /mcp/call. It is not
// discoverable from inside this package otherwise -- the port is a CLI flag
// owned by cmd/engine/cmd/start.go, not part of *config.Config.
var globalEnginePort string

var mcpSessions struct {
	sync.RWMutex
	m map[string]*mcpSession
}

// activeExecutions tracks per-account in-flight sandbox executions for
// MaxSandboxConcurrency enforcement. A sync.Map is chosen because account
// count is unbounded and a coarse mutex would serialize unrelated accounts.
var activeExecutions struct {
	sync.Map // key: uuid.UUID.String() → *int64 (pointer so we can atomic increment)
}

func init() {
	mcpSessions.m = make(map[string]*mcpSession)
}

// executionCounter tracks a single account's in-flight executions.
type executionCounter struct {
	sync.Mutex
	count int
}

// trackExecutionStart increments the per-account active execution count and
// returns the new count plus a decrement function to call when execution ends.
// If the limit is exceeded the counter is not incremented.
func trackExecutionStart(accountID uuid.UUID) (current int, decrement func()) {
	key := accountID.String()
	val, _ := activeExecutions.LoadOrStore(key, &executionCounter{})
	c := val.(*executionCounter)
	c.Lock()
	c.count++
	current = c.count
	c.Unlock()
	return current, func() {
		c.Lock()
		c.count--
		remaining := c.count
		c.Unlock()
		if remaining <= 0 {
			activeExecutions.Delete(key)
		}
	}
}

// activeMCPSessionCount returns the total number of active MCP sessions
// currently registered across all immutable app versions.
func activeMCPSessionCount() int {
	mcpSessions.RLock()
	defer mcpSessions.RUnlock()
	return len(mcpSessions.m)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	if msg == "no rows in result set" {
		status = http.StatusNotFound
		msg = "resource not found"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, msg)))
}

// InitSandbox wires the process-owned physical and logical execution edges used by SDK and MCP transports.
func InitSandbox(r chi.Router, nc *messaging.NATSClient, appCfg *config.Config, cache ObjectCache, validator auth.TokenValidator, resolver SecretResolver, rateLimits store.ProviderRateLimitStore, enginePort string, unifiedExecute MCPUnifiedExecuteFunc) {
	cfg = appCfg
	globalNATSClient = nc
	globalObjectCache = cache
	globalDispatcher = engine.NewDispatcher()
	// A configured coordinator must remain on the one canonical provider path.
	if rateLimits != nil {
		globalDispatcher = engine.NewDispatcherWithProviderRateLimits(rateLimits)
	}
	globalTokenValidator = validator
	SetSecretResolver(resolver)
	globalEnginePort = enginePort
	globalMCPUnifiedExecute = unifiedExecute

	// Initialise rate limiters from config.
	rl := cfg.Sandbox.RateLimit
	initRateLimiters(rl.SSEConnectionsPerMinute, rl.SSEBurst, rl.MessagesPerMinute, rl.MessagesBurst)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})

	registerMCPRoutes(r)
}

func registerMCPRoutes(r chi.Router) {
	r.Get("/mcp/{id}/sse", mcpSseHandler)
	r.HandleFunc("/mcp/{id}", mcpStreamableHandler)
	r.Post("/mcp/message", mcpMessageHandler)
	r.Post("/mcp/call", mcpCallHandler)
}
func InitWebhookRoutes(r chi.Router) {
	r.Get("/webhook/{urlSlug}", webhookIngressHandler)
	r.Get("/webhook/{urlSlug}/{eventName}", webhookIngressHandler)
	r.Post("/webhook/{urlSlug}", webhookIngressHandler)
	r.Post("/webhook/{urlSlug}/{eventName}", webhookIngressHandler)
}

// observabilityStartFunc is the injectable factory for execution threads.
// Tests substitute a spy here; production wires the real OTEL adapter via
// observability.Start. Kept as a var (not an interface) to avoid adding an
// unnecessary abstraction layer for a single call site.
//
// Why injectable: engineExecuteCore must be fully testable without a live
// OTEL collector, and we want to assert that credentials are never placed
// in span refs/attrs.
var observabilityStartFunc = func(ctx context.Context, name, parentID string, tags ...string) (observability.Thread, error) {
	return observability.Start(ctx, name, parentID, tags...)
}

// EngineStreamExecuteFunc is the credential-aware, streaming execution entrypoint
// used by the gRPC edge (grpc.go). It is a package var so tests can substitute a
// mock. The default performs real scope-checked dispatch to the vendor.
var EngineStreamExecuteFunc = func(ctx context.Context, appID, token, endpointName string, params, credentials map[string]any, environment string, stream engine.ResponseStream) error {
	return engineExecuteCore(ctx, globalObjectCache, globalDispatcher, globalTokenValidator, appID, token, endpointName, params, credentials, environment, stream)
}

// engineExecuteCore is the single code path that performs a vendor call. Both the
// gRPC edge and the MCP path funnel through it: validate scope (governance
// layer 2), audit, then stream the vendor response via the dispatcher.
//
// An OTEL span is opened here for every user/agent-triggered execution. Raw
// parameters are intentionally absent: request values can contain secrets or
// provider payloads even when they are not named like credentials. Bounded
// contract, outcome, timing, and identity metadata provide the audit signal.
// Timing ownership is established here so non-gRPC adapters also produce complete receipts.
func engineExecuteCore(
	ctx context.Context,
	cache ObjectCache,
	dispatcher *engine.Dispatcher,
	validator auth.TokenValidator,
	appID, token, endpointName string,
	params, credentials map[string]any,
	environment string,
	stream engine.ResponseStream,
) (err error) {
	// gRPC already records ingress work; preserve its collector while admitting bare MCP contexts.
	if _, ok := engine.ExecutionTimingsFromContext(ctx); !ok {
		ctx = engine.ContextWithExecutionTimings(ctx, engine.NewExecutionTimings())
	}
	executionStarted := time.Now()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.execute", trace.WithAttributes(
		attribute.String("app.id", appID),
		attribute.String("endpoint_name", endpointName),
		attribute.Bool("idempotency_key_present", idempotencyKeyFromContext(ctx) != ""),
		attribute.Bool("request_body_hash_present", requestBodyHashFromContext(ctx) != ""),
		attribute.Int("execution.contract_version", 0),
		attribute.Int("execution.required_capabilities_count", 0),
		attribute.String("execution.contract_negotiation.outcome", "not_reached"),
	))
	defer span.End()

	identity, decrementActive, err := resolveTrackedExecutionIdentity(ctx, validator, appID, token, span)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer decrementActive()
	auditState := executionAuditState{
		identity:     identity,
		endpointName: endpointName,
		startedAt:    executionStarted,
	}
	defer func() {
		// A client or policy deadline must not suppress the durable audit event.
		// WithoutCancel preserves identity/timing/span values while removing only
		// the cancellation signal that has already done its execution work.
		auditCtx := context.WithoutCancel(ctx)
		recordEngineExecutionAudit(auditCtx, span, auditState, err)
		recordEngineExecutionUsage(auditCtx, span, auditState, err)
	}()
	if !identity.AllowsOperation(endpointName) {
		// Fixture filtering improves MCP discovery, but this shared boundary is
		// the authoritative authorization decision for both SDK and MCP calls.
		span.SetAttributes(attribute.String("authorization.outcome", "denied"))
		err = errors.New("ScopeError: operation not allowed by token")
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetAttributes(attribute.String("authorization.outcome", "allowed"))

	// 1-2. Validate the app version's scope manifest and resolve endpointName within
	// it (governance layer 2; resolution only, no dispatcher mapping yet).
	// Split into resolveScopedEndpoint -- separation of concerns (scope
	// governance is a distinct step from dispatch) and keeps this function's
	// branching within the repo's complexity budget. See that function's doc
	// comment for the child span lifecycle it owns.
	resolutionStarted := time.Now()
	match, err := resolveExecutionContractScopedEndpoint(ctx, cache, span, appID, endpointName)
	if err != nil {
		return err
	}
	auditState.match = match
	request := PhysicalExecutionRequest{Params: params, Credentials: credentials, Environment: environment}
	return executeResolvedProviderOperation(ctx, dispatcher, identity, match, request, stream, span, &auditState, resolutionStarted)
}

func resolveExecutionContractScopedEndpoint(ctx context.Context, cache ObjectCache, span trace.Span, appID, endpointName string) (*scopedEndpoint, error) {
	match, err := resolveScopedEndpoint(ctx, cache, span, appID, endpointName)
	if err != nil {
		if _, incompatible := fusedobject.ExecutionContractCompatibilityDetails(err); incompatible {
			recordExecutionContractNegotiation(span, fusedobject.ExecutionContractEnvelope{}, err)
		}
		return nil, err
	}
	err = fusedobject.ValidateExecutionContractEnvelope(match.service.ExecutionContractEnvelope)
	recordExecutionContractNegotiation(span, match.service.ExecutionContractEnvelope, err)
	if err != nil {
		return nil, err
	}
	return match, nil
}

// Final status projection is kept separate from governance and dispatch so
// adding a new provider outcome cannot make the execution boundary itself too
// complex to audit reliably.
func finishExecutionDispatch(ctx context.Context, span trace.Span, providerHTTPStatus, executionTimeoutMs int, dispatchErr error) error {
	// Failed attempts consumed time too; export the same collected stages before outcome-specific returns.
	if timings, ok := engine.ExecutionTimingsFromContext(ctx); ok {
		span.SetAttributes(timings.Attributes()...)
	}
	// Normalize policy expiry without losing the recorded provider work that preceded it.
	if dispatchErr != nil {
		dispatchErr = normalizeExecutionTimeout(ctx, dispatchErr, executionTimeoutMs)
		span.SetStatus(codes.Error, executionFailureDescription(dispatchErr, providerHTTPStatus))
		return dispatchErr
	}
	// A provider HTTP failure is an execution failure even when transport completed normally.
	if providerHTTPStatus >= http.StatusBadRequest {
		span.SetStatus(codes.Error, providerStatusError(providerHTTPStatus))
		return nil
	}
	span.SetStatus(codes.Ok, "tool call dispatched")
	return nil
}

func executionFailureDescription(dispatchErr error, providerHTTPStatus int) string {
	_, code := classifyExecutionFailure(dispatchErr, providerHTTPStatus)
	if code == "" {
		return "request_failed"
	}
	return code
}

func contextWithProviderRateLimitIdentity(ctx context.Context, accountID uuid.UUID, credentials map[string]any) (context.Context, error) {
	bucketID, err := optionalResolvedCredentialUUID(credentials, "fused_bucket_id")
	if err != nil {
		return nil, err
	}
	connectionID, err := optionalResolvedCredentialUUID(credentials, "fused_connection_id")
	if err != nil {
		return nil, err
	}
	metadata, err := resourceMetadataValues(credentials["fused_resource_metadata"])
	if err != nil {
		return nil, err
	}
	bindings := make(map[string]string, len(metadata)+1)
	for key, value := range metadata {
		bindings["connection."+key] = value
	}
	if resourceID := credentialString(credentials, "fused_resource_provider_id"); resourceID != "" {
		bindings["resource.id"] = resourceID
	}
	ctx = engine.WithProviderRateLimitIdentity(ctx, accountID, bucketID, connectionID)
	return engine.WithProviderQuotaBindings(ctx, bindings), nil
}

func optionalResolvedCredentialUUID(credentials map[string]any, key string) (uuid.UUID, error) {
	raw := credentialString(credentials, key)
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolved %s is invalid", key)
	}
	return id, nil
}

// resolveTrackedExecutionIdentity resolves tracked execution identity from immutable app scope before provider dispatch.
func resolveTrackedExecutionIdentity(ctx context.Context, validator auth.TokenValidator, appID, token string, span trace.Span) (auth.RuntimeIdentity, func(), error) {
	identity, err := resolveExecutionIdentity(ctx, validator, appID, token)
	if err != nil {
		return auth.RuntimeIdentity{}, func() {}, err
	}
	decrement, err := trackAuthenticatedExecution(identity, span)
	return identity, decrement, err
}

// trackAuthenticatedExecution charges the authenticated account concurrency counter and rolls it back on denial.
func trackAuthenticatedExecution(identity auth.RuntimeIdentity, span trace.Span) (func(), error) {
	if identity.AccountID == uuid.Nil {
		return func() {}, nil
	}
	current, decrement := trackExecutionStart(identity.AccountID)
	limitErr := entitlement.CheckLimit(span, "sandbox_concurrency", current-1, entitlement.LiveEntitlement.Load().MaxSandboxConcurrency)
	if limitErr != nil {
		decrement()
		return func() {}, limitErr
	}
	return decrement, nil
}

func resolveMatchedExecutionCredentials(ctx context.Context, match *scopedEndpoint, obj *models.IntegrationObject, identity auth.RuntimeIdentity, credentials map[string]any) (map[string]any, []store.BucketValue, error) {
	credentials = credentialsWithSelectionAuth(credentials, match.selection, obj.SecurityRequirements)
	request := CredentialRequest{
		AccountID: identity.AccountID, AppID: identity.AppID, TokenID: identity.TokenID,
		BindingMode: identity.BindingMode, ServiceID: match.service.ID,
		OperationID: obj.Name, Auths: match.service.AuthConfigs, Passthrough: credentials,
		Requirements: obj.SecurityRequirements,
	}
	serviceVersionID, err := uuid.Parse(match.serviceVersionID)
	if err != nil {
		return nil, nil, errors.New("resolved service version ID is invalid")
	}
	authType := requestedAuthType(credentials)
	request.ServiceVersionID = serviceVersionID
	request.AuthType = authType
	return resolveExecutionCredentials(ctx, request)
}

// credentialsWithSelectionAuth adds only non-secret routing metadata resolved
// during desired-config planning. Explicit SDK inputs still win for callers that
// intentionally select another declared scheme on a general-purpose SDK.
func credentialsWithSelectionAuth(credentials map[string]any, selection models.SDKSelection, requirements authrouting.Requirements) map[string]any {
	if credentialString(credentials, "fused_auth_type") != "" || credentialString(credentials, "fused_auth_name") != "" {
		return credentials
	}
	if requirementsPermitAnonymous(requirements) {
		return credentials
	}
	if selection.AuthType == "" && selection.AuthName == "" {
		return credentials
	}
	out := make(map[string]any, len(credentials)+2)
	for key, value := range credentials {
		out[key] = value
	}
	out["fused_auth_type"] = selection.AuthType
	out["fused_auth_name"] = selection.AuthName
	return out
}

func requirementsPermitAnonymous(requirements authrouting.Requirements) bool {
	for _, alternative := range requirements {
		if len(alternative.Schemes) == 0 {
			return true
		}
	}
	return false
}

// withConnectedResourceRequirement rebuilds the credential envelope without
// internal routing fields, then marks only service versions that declared
// x-fused-connect. This prevents SDK input from impersonating resolver output.
func withConnectedResourceRequirement(credentials map[string]any, config *fusedobject.ServiceConnectConfig) map[string]any {
	if credentials == nil && config == nil {
		return nil
	}
	clean := make(map[string]any, len(credentials)+1)
	for key, value := range credentials {
		if !isInternalResourceCredential(key) {
			clean[key] = value
		}
	}
	if config != nil {
		clean["fused_resource_required"] = "true"
	}
	return clean
}

// isInternalResourceCredential defines fields that may only be created after
// a connection-scoped database lookup; resourceId remains public by design.
func isInternalResourceCredential(key string) bool {
	switch key {
	case "fused_resource_required", "fused_connection_id", "fused_bucket_id", "fused_resource_base_url", "fused_resource_type", "fused_resource_provider_id", "fused_resource_metadata":
		return true
	default:
		return false
	}
}

// resolveExecutionIdentity keeps SDK ID parsing and token validation together
// because both determine the account/bucket boundary for the rest of execution.
func resolveExecutionIdentity(ctx context.Context, validator auth.TokenValidator, appID, token string) (auth.RuntimeIdentity, error) {
	uid, err := uuid.Parse(appID)
	if err != nil {
		return auth.RuntimeIdentity{}, fmt.Errorf("invalid app id format")
	}
	if validator == nil {
		return auth.RuntimeIdentity{AppID: uid}, nil
	}
	authStarted := time.Now()
	identity, err := validator.Validate(ctx, uid, token)
	engine.MeasureExecutionTiming(ctx, "auth", authStarted)
	return identity, err
}

// resolveExecutionCredentials is the single pre-dispatch merge point for
// bucket secrets, bucket values, and connected-auth tokens.
func resolveExecutionCredentials(ctx context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	if globalSecretResolver == nil || request.AccountID == uuid.Nil {
		return request.Passthrough, nil, nil
	}
	credsStarted := time.Now()
	// Generated SDKs should only need the stable user reference; the Engine can
	// infer the concrete auth config name from the service manifest it already
	// loaded for this execution.
	request.Passthrough = WithDefaultConnectedAuthName(request.Passthrough, request.Auths, request.Requirements)
	resolved, vals, err := resolveScopedCredentials(ctx, request)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve credentials: %w", err)
	}
	engine.MeasureExecutionTiming(ctx, "credentials_resolution", credsStarted)
	return resolved, vals, nil
}

func resolveScopedCredentials(ctx context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	return globalSecretResolver.ResolveExecutionCredentials(ctx, request)
}

// resolveScopedEndpoint validates the app version's scope manifest and resolves
// endpointName within it, opening and fully owning its own "engine.scope.enforce"
// child span (started and ended here, not left for the caller to manage) so
// this stays a single self-contained governance step. parentSpan only
// receives a status update on failure -- span ownership for the successful
// path continues in engineExecuteCore's outer span.
func resolveScopedEndpoint(ctx context.Context, cache ObjectCache, parentSpan trace.Span, appID, endpointName string) (*scopedEndpoint, error) {
	_, scopeSpan := otel.Tracer("engine").Start(ctx, "engine.scope.enforce")
	defer scopeSpan.End()

	scopeStarted := time.Now()
	selections, err := validateAndParseScope(ctx, cache, appID)
	engine.MeasureExecutionTiming(ctx, "sdk_scope_manifest_lookup", scopeStarted)
	if err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		parentSpan.SetStatus(codes.Error, "scope error")
		return nil, err
	}

	endpointStarted := time.Now()
	match, err := findEndpointInScope(ctx, cache, appID, selections, endpointName)
	engine.MeasureExecutionTiming(ctx, "endpoint_metadata", endpointStarted)
	if err != nil {
		scopeSpan.SetStatus(codes.Error, err.Error())
		parentSpan.SetStatus(codes.Error, "scope resolution error")
		return nil, err
	}
	if match == nil {
		observability.ScopeRejections.Add(ctx, 1)
		scopeSpan.SetStatus(codes.Error, "tool not in scope")
		parentSpan.SetStatus(codes.Error, "tool not in scope")
		return nil, fmt.Errorf("ScopeError: tool '%s' not found or not in scope", endpointName)
	}
	if !match.allowed {
		// Endpoint exists on the provider but isn't enabled for this SDK.
		observability.ScopeRejections.Add(ctx, 1)
		scopeSpan.SetStatus(codes.Error, "endpoint not allowed")
		parentSpan.SetStatus(codes.Error, "endpoint not allowed")
		return nil, fmt.Errorf("ScopeError: unauthorized access to endpoint '%s'", endpointName)
	}
	return match, nil
}

func dispatchRuntimeEnvironment(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	match *scopedEndpoint,
	obj *models.IntegrationObject,
	params, credentials map[string]any,
	bucketValues []store.BucketValue,
	environment string,
	stream engine.ResponseStream,
	span trace.Span,
) (RuntimeEnvironmentResolution, int, error) {
	environmentStarted := time.Now()
	srv, resolution, err := serviceForRuntimeEnvironment(match.service, environment, credentials, bucketValues)
	engine.MeasureExecutionTiming(ctx, "runtime_environment", environmentStarted)
	if err != nil {
		recordRuntimeEnvironmentAttrs(span, match, environment, "")
		return RuntimeEnvironmentResolution{}, 0, err
	}
	resolution, err = applyOperationRuntimeServer(match.service, srv, obj, resolution, credentials, bucketValues)
	if err != nil {
		recordRuntimeEnvironmentAttrs(span, match, environment, "")
		return RuntimeEnvironmentResolution{}, 0, err
	}
	resourceSource := selectedConnectedResourceSource(credentials)
	recordRuntimeEnvironmentAttrs(span, match, resolution.Environment, resolution.Source)
	if resourceSource != "" {
		span.SetAttributes(
			attribute.String("resource_resolution_status", "selected"),
			attribute.String("resource_resolution_source", resourceSource),
			attribute.String("resource_type", credentialString(credentials, "fused_resource_type")),
		)
	}
	status, err := dispatcher.ExecuteStream(ctx, srv, obj, params, credentials, bucketValues, stream)
	span.SetAttributes(attribute.Int("provider_http_status", status))
	if statusErr := engine.SendResponseStatus(stream, status); err == nil {
		err = statusErr
	}
	// Diagnostics are best-effort and never change provider response behavior;
	// an unexpired token's 401/403 remains information, not an automation trigger.
	recordProviderAuthFailure(ctx, status, credentials, span)
	return resolution, status, err
}

func providerStatusError(status int) string {
	return fmt.Sprintf("provider returned HTTP %d", status)
}

func providerHost(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// recordProviderAuthFailure writes only stable authorization codes and trace
// correlation, keeping provider bodies and end-user references out of logs.
func recordProviderAuthFailure(ctx context.Context, status int, credentials map[string]any, span trace.Span) {
	code := providerAuthFailureCode(status)
	// Successful and unrelated provider statuses need no connection write.
	if code == "" {
		return
	}
	recorder, ok := globalSecretResolver.(connectedAuthFailureRecorder)
	// Alternate/test resolvers may not own persistence and should retain their
	// existing behavior rather than failing execution for missing diagnostics.
	if !ok {
		span.SetAttributes(attribute.String("connection_diagnostic_status", "unavailable"))
		return
	}
	recorded, err := recorder.recordConnectedAuthFailure(ctx, credentials, code)
	if err != nil {
		span.SetAttributes(attribute.String("connection_diagnostic_status", "failed"))
		slog.WarnContext(ctx, "failed to record connected auth diagnostic", slog.String("failure_code", code), slog.Any("error", err))
		return
	}
	// Static credentials intentionally have no connection diagnostic row.
	if !recorded {
		span.SetAttributes(attribute.String("connection_diagnostic_status", "not_connected"))
		return
	}
	span.SetAttributes(attribute.String("connection_diagnostic_status", "recorded"))
}

// providerAuthFailureCode deliberately classifies status only; provider bodies
// may contain PII and are neither persisted nor copied into OTEL attributes.
func providerAuthFailureCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "provider_unauthorized"
	case http.StatusForbidden:
		return "provider_forbidden"
	default:
		return ""
	}
}

// selectedConnectedResourceSource reports routing context without applying it
// directly; only an explicit validated base_url binding may alter the URL.
func selectedConnectedResourceSource(credentials map[string]any) string {
	if credentialString(credentials, "fused_resource_id") == "" {
		return ""
	}
	return "connection_resource"
}

func recordRuntimeEnvironmentAttrs(span trace.Span, match *scopedEndpoint, environment, source string) {
	span.SetAttributes(
		attribute.String("service_id", match.service.ID.String()),
		attribute.String("service_version_id", match.serviceVersionID),
		attribute.String("selected_environment", environment),
		attribute.String("environment_resolution_source", source),
	)
}

func validateAndParseScope(ctx context.Context, cache ObjectCache, appID string) ([]models.SDKSelection, error) {
	_, selectionsJSON, err := cache.GetAppRuntime(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("ScopeError: app scope not found")
	}

	var selections []models.SDKSelection
	if err := json.Unmarshal(selectionsJSON, &selections); err != nil {
		return nil, fmt.Errorf("ScopeError: invalid scope format")
	}
	// ObjectCache implementations expose only the nested payload, so enforce
	// its exact schema again before any endpoint can be resolved or dispatched.
	if err := models.ValidateAppSelections(models.AppScopeSchemaVersion, selections); err != nil {
		return nil, fmt.Errorf("ScopeError: unsupported app selection schema")
	}

	return selections, nil
}

// scopedEndpoint is the result of resolving a tool name against an app version's scope:
// the provider object, the matched endpoint, and whether it is enabled for the
// SDK. This is pure resolution data — mapping to dispatcher types happens in the
// caller, keeping resolution and mapping as separate concerns.
type scopedEndpoint struct {
	service          *fusedobject.ServiceMetadata
	endpoint         fusedobject.Endpoint
	allowed          bool
	serviceVersionID string
	selection        models.SDKSelection
}

// findEndpointInScope resolves endpointName to an endpoint across the app's scoped
// providers. It returns nil when no provider in scope exposes the tool.
func findEndpointInScope(ctx context.Context, cache ObjectCache, appID string, selections []models.SDKSelection, endpointName string) (*scopedEndpoint, error) {
	for _, sel := range selections {
		serviceStarted := time.Now()
		fusedObj, err := cache.GetOrFetchServiceMetadata(ctx, appID, sel.ServiceID.String())
		engine.MeasureExecutionTiming(ctx, "service_metadata_resolution", serviceStarted)
		if err != nil {
			if _, incompatible := fusedobject.ExecutionContractCompatibilityDetails(err); incompatible {
				return nil, err
			}
			continue
		}

		endpointStarted := time.Now()
		ep, err := cache.GetEndpoint(ctx, appID, sel.ServiceID.String(), endpointName)
		engine.MeasureExecutionTiming(ctx, "endpoint_lookup", endpointStarted)
		if err != nil {
			if _, incompatible := fusedobject.ExecutionContractCompatibilityDetails(err); incompatible {
				return nil, err
			}
			// Not found in this service, try next
			continue
		}

		return &scopedEndpoint{
			service:          fusedObj,
			endpoint:         *ep,
			allowed:          endpointAllowed(sel, ep),
			serviceVersionID: selectionServiceVersionID(sel),
			selection:        sel,
		}, nil
	}
	return nil, nil
}

func serviceForRuntimeEnvironment(metadata *fusedobject.ServiceMetadata, environment string, credentials map[string]any, values []store.BucketValue) (*models.Service, RuntimeEnvironmentResolution, error) {
	resolution, err := resolveRuntimeEnvironment(metadata, environment)
	if err != nil {
		return nil, RuntimeEnvironmentResolution{}, err
	}
	resolution, err = resolveRuntimeServerTemplate(metadata, resolution, credentials, values)
	if err != nil {
		return nil, RuntimeEnvironmentResolution{}, err
	}
	srv := fusedToService(metadata)
	srv.BaseURL = resolution.BaseURL
	srv.ServiceBaseURL = resolution.BaseURL
	srv.ServerSource = "service"
	if resolution.Source == "connection_resource" {
		srv.ServerSource = "connection_resource"
	}
	return srv, resolution, nil
}

func selectionServiceVersionID(sel models.SDKSelection) string {
	if sel.ServiceVersionID == uuid.Nil {
		return ""
	}
	return sel.ServiceVersionID.String()
}

// endpointAllowed reports whether the endpoint is in the selection's enabled
// set (governance layer 2: the SDK may only call endpoints it was generated for).
func endpointAllowed(sel models.SDKSelection, ep *fusedobject.Endpoint) bool {
	if sel.SelectAll {
		return true
	}
	for _, id := range sel.EndpointIDs {
		if id == ep.ID {
			return true
		}
	}
	for _, name := range sel.OperationNames {
		if name == ep.Name {
			return true
		}
	}
	return false
}

// sanitiseParams removes known credential keys from a params map before the
// map is written to OTEL spans or analytics storage. This is the single
// enforcement point — all recording paths call through here rather than
// applying their own ad-hoc filtering.
func sanitiseParams(params map[string]any, extraKeys ...string) map[string]any {
	// Common credential keys that must never be persisted regardless of how
	// they arrive. These match what applyAuth injects and what SDK configs can
	// forward, plus frequent aliases.
	const credentialKeys = "Authorization,authorization,username,password,apiKey,api_key,apikey,x-api-key,token,access_token,refresh_token,secret,client_secret,client_id,bearer,auth,credential,cert,certificate,key,private_key,client_cert,client_key"
	banned := make(map[string]struct{})
	for _, k := range strings.Split(credentialKeys, ",") {
		banned[strings.ToLower(k)] = struct{}{}
	}
	for _, k := range extraKeys {
		if k != "" {
			banned[strings.ToLower(k)] = struct{}{}
		}
	}

	out := make(map[string]any, len(params))
	for k, v := range params {
		if _, skip := banned[strings.ToLower(k)]; !skip {
			out[k] = v
		}
	}
	return out
}

// credentialKeysFromAuthConfigs returns the KeyName values declared in a
// service's auth configs so callers can extend the sanitisation denylist with
// service-specific credential keys.
func credentialKeysFromAuthConfigs(auths models.AuthConfigs) []string {
	keys := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth.KeyName != "" {
			keys = append(keys, auth.KeyName)
		}
		if canonicalModelAuthType(auth) == "mtls" && auth.Name != "" {
			// mTLS credentials are normally in the credentials map; include the
			// pair keys here defensively so accidental params never hit telemetry.
			keys = append(keys, auth.Name+"_cert", auth.Name+"_key")
		}
	}
	return keys
}
