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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
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

type pendingReq struct {
	endpointName string
	serviceName  string
	startTime    time.Time
	// arguments is retained only until the response arrives so
	// enforceToolCallTimeout can match the request; it is never
	// written to analytics or telemetry storage.
	arguments map[string]any
}

type mcpSession struct {
	appID           string
	sessionID       string
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	cancel          context.CancelFunc
	pendingRequests map[string]pendingReq
	pendingMu       sync.Mutex
	idleTimer       *time.Timer
	injectedResp    chan string
	token           string

	// fixture is this session's own operation catalog, built at connect time
	// from the SDK's AppRuntime.Selections (mcp_session_fixture.go), scoping
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

// tokenCacheEntry holds the result of a successful TokenValidator.Validate call
// so the DB round-trip is skipped for repeated requests within the TTL window.
type tokenCacheEntry struct {
	accountID uuid.UUID
	expiry    time.Time
}

var tokenCache struct {
	sync.RWMutex
	m map[string]tokenCacheEntry
}

// activeExecutions tracks per-account in-flight sandbox executions for
// MaxSandboxConcurrency enforcement. A sync.Map is chosen because account
// count is unbounded and a coarse mutex would serialize unrelated accounts.
var activeExecutions struct {
	sync.Map // key: uuid.UUID.String() → *int64 (pointer so we can atomic increment)
}

func init() {
	mcpSessions.m = make(map[string]*mcpSession)
	tokenCache.m = make(map[string]tokenCacheEntry)
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
// currently registered across all SDK IDs.
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

func InitSandbox(r chi.Router, nc *messaging.NATSClient, appCfg *config.Config, cache ObjectCache, validator auth.TokenValidator, resolver SecretResolver, enginePort string) {
	initSharedSandboxes()

	cfg = appCfg
	globalNATSClient = nc
	globalObjectCache = cache
	globalDispatcher = engine.NewDispatcher()
	if local, ok := cache.(*LocalObjectCache); ok {
		if rateLimits := local.providerRateLimitStore(); rateLimits != nil {
			globalDispatcher = engine.NewDispatcherWithProviderRateLimits(rateLimits)
		}
	}
	globalTokenValidator = validator
	globalSecretResolver = resolver
	globalEnginePort = enginePort

	// Initialise rate limiters from config.
	rl := cfg.Sandbox.RateLimit
	initRateLimiters(rl.SSEConnectionsPerMinute, rl.SSEBurst, rl.MessagesPerMinute, rl.MessagesBurst)

	// Setup NATS subscriptions
	setupNATSSubscriptions(nc)

	// Background goroutine: evict expired token cache entries every 5 minutes.
	// Without this sweep the map grows without bound across the process lifetime.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			tokenCache.Lock()
			for key, entry := range tokenCache.m {
				if now.After(entry.expiry) {
					delete(tokenCache.m, key)
				}
			}
			tokenCache.Unlock()
		}
	}()

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})

	r.Get("/mcp/{id}/sse", mcpSseHandler)
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
// An OTEL thread is opened here for every user/agent-triggered execution.
// Because the user owns the MCP executor, full params are safe to record in spans
// for debugging and observability — credentials are the only exception and are
// NEVER written to spans, attrs, or log lines (sanitiseParams strips them first).
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
	executionStarted := time.Now()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.dispatch.execute", trace.WithAttributes(
		attribute.String("app.id", appID),
		attribute.String("endpoint_name", endpointName),
		attribute.Bool("idempotency_key_present", idempotencyKeyFromContext(ctx) != ""),
		attribute.Bool("request_body_hash_present", requestBodyHashFromContext(ctx) != ""),
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

	// 1-2. Validate the SDK's scope manifest and resolve endpointName within
	// it (governance layer 2; resolution only, no dispatcher mapping yet).
	// Split into resolveScopedEndpoint -- separation of concerns (scope
	// governance is a distinct step from dispatch) and keeps this function's
	// branching within the repo's complexity budget. See that function's doc
	// comment for the child span lifecycle it owns.
	resolutionStarted := time.Now()
	match, err := resolveScopedEndpoint(ctx, cache, span, appID, endpointName)
	if err != nil {
		return err
	}
	auditState.match = match
	ctx, cancelExecution, executionTimeoutMs := contextWithExecutionPolicyTimeout(ctx, match.service.TimeoutMs, span)
	defer cancelExecution()
	defer func() {
		err = normalizeExecutionTimeout(ctx, err, executionTimeoutMs)
	}()

	// 3. Map the resolved Fused endpoint to dispatcher inputs, then execute. The
	//    dispatcher applies auth from credentials and streams the response
	//    (SSE/pagination) back through stream.
	if dispatcher == nil {
		span.SetStatus(codes.Error, "dispatcher not initialized")
		return fmt.Errorf("engine dispatcher not initialized")
	}
	headersStarted := time.Now()
	params = paramsWithExecutionHeaders(params, idempotencyKeyFromContext(ctx), requestBodyHashFromContext(ctx))
	engine.MeasureExecutionTiming(ctx, "execution_headers_inject", headersStarted)
	objectMapStarted := time.Now()
	obj := fusedToIntegrationObject(match.service, match.endpoint)
	engine.MeasureExecutionTiming(ctx, "integration_object_map", objectMapStarted)

	// Idempotency cache-and-replay lives in idempotency_cache.go, split into two
	// steps (separation of concerns: "can we skip the vendor" vs "how do we
	// dispatch and remember the result" are different decisions with different
	// failure modes). A cache hit fully handles the response and this function
	// returns immediately; a lookup error (e.g. the key was reused with a
	// different body) fails the request without ever reaching the vendor.
	if replayed, replayErr := tryReplayFromIdempotencyCache(ctx, span, identity.AppID, obj, stream, &auditState); replayErr != nil {
		return replayErr
	} else if replayed {
		engine.MeasureExecutionTiming(ctx, "engine_resolution_total", resolutionStarted)
		return nil
	}

	// 4. Resolve Secrets (Workspace Defaults / SDK Overrides)
	credentials = withConnectedResourceRequirement(credentials, match.service.ConnectConfig)
	credentials, bucketVals, err := resolveMatchedExecutionCredentials(ctx, match, obj, identity.AppID, identity.AccountID, credentials)
	if err != nil {
		return err
	}
	ctx, err = contextWithProviderRateLimitIdentity(ctx, identity.AccountID, credentials)
	if err != nil {
		return err
	}

	engine.MeasureExecutionTiming(ctx, "engine_resolution_total", resolutionStarted)

	var runtimeResolution RuntimeEnvironmentResolution
	runtimeResolution, auditState.providerHTTPStatus, err = dispatchAndCache(ctx, dispatcher, match, obj, params, credentials, bucketVals, environment, stream, span, identity.AppID)
	auditState.selectedEnvironment = runtimeResolution.Environment
	auditState.environmentSource = runtimeResolution.Source
	auditState.providerHost = providerHost(runtimeResolution.BaseURL)
	if err != nil {
		err = normalizeExecutionTimeout(ctx, err, executionTimeoutMs)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if auditState.providerHTTPStatus >= http.StatusBadRequest {
		span.SetStatus(codes.Error, providerStatusError(auditState.providerHTTPStatus))
		return nil
	}
	if timings, ok := engine.ExecutionTimingsFromContext(ctx); ok {
		span.SetAttributes(timings.Attributes()...)
	}
	span.SetStatus(codes.Ok, "tool call dispatched")
	return nil
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
	return engine.WithProviderRateLimitIdentity(ctx, accountID, bucketID, connectionID), nil
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

func resolveTrackedExecutionIdentity(ctx context.Context, validator auth.TokenValidator, appID, token string, span trace.Span) (auth.RuntimeIdentity, func(), error) {
	identity, err := resolveExecutionIdentity(ctx, validator, appID, token)
	if err != nil {
		return auth.RuntimeIdentity{}, func() {}, err
	}
	if identity.AccountID == uuid.Nil {
		return identity, func() {}, nil
	}
	current, decrement := trackExecutionStart(identity.AccountID)
	limitErr := entitlement.CheckLimit(span, "sandbox_concurrency", current-1, entitlement.LiveEntitlement.Load().MaxSandboxConcurrency)
	if limitErr != nil {
		decrement()
		return auth.RuntimeIdentity{}, func() {}, limitErr
	}
	return identity, decrement, nil
}

func resolveMatchedExecutionCredentials(ctx context.Context, match *scopedEndpoint, obj *models.IntegrationObject, appID, accountID uuid.UUID, credentials map[string]any) (map[string]any, []store.BucketValue, error) {
	credentials = credentialsWithSelectionAuth(credentials, match.selection)
	request := CredentialRequest{
		AccountID: accountID, AppID: appID, ServiceID: match.service.ID,
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
func credentialsWithSelectionAuth(credentials map[string]any, selection models.SDKSelection) map[string]any {
	if selection.AuthType == "" && selection.AuthName == "" {
		return credentials
	}
	out := make(map[string]any, len(credentials)+2)
	for key, value := range credentials {
		out[key] = value
	}
	if credentialString(out, "fused_auth_type") == "" {
		out["fused_auth_type"] = selection.AuthType
	}
	if credentialString(out, "fused_auth_name") == "" {
		out["fused_auth_name"] = selection.AuthName
	}
	return out
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

// resolveScopedEndpoint validates the SDK's scope manifest and resolves
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

// recordSelectedAuthType records only the selected scheme name for audit/debug;
// credential values and key material stay out of telemetry.
func recordSelectedAuthType(span trace.Span, authType string) {
	if authType != "" {
		span.SetAttributes(attribute.String("selected_auth_type", authType))
	}
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
		return nil, fmt.Errorf("ScopeError: sdk scope not found")
	}

	var selections []models.SDKSelection
	if err := json.Unmarshal(selectionsJSON, &selections); err != nil {
		return nil, fmt.Errorf("ScopeError: invalid scope format")
	}

	return selections, nil
}

// scopedEndpoint is the result of resolving a tool name against an SDK's scope:
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

// findEndpointInScope resolves endpointName to an endpoint across the SDK's scoped
// providers. It returns nil when no provider in scope exposes the tool.
func findEndpointInScope(ctx context.Context, cache ObjectCache, appID string, selections []models.SDKSelection, endpointName string) (*scopedEndpoint, error) {
	for _, sel := range selections {
		serviceStarted := time.Now()
		fusedObj, err := cache.GetOrFetchServiceMetadata(ctx, appID, sel.ServiceID.String())
		engine.MeasureExecutionTiming(ctx, "service_metadata_resolution", serviceStarted)
		if err != nil {
			continue
		}

		endpointStarted := time.Now()
		ep, err := cache.GetEndpoint(ctx, appID, sel.ServiceID.String(), endpointName)
		engine.MeasureExecutionTiming(ctx, "endpoint_lookup", endpointStarted)
		if err != nil {
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

// snakeCase lowercases and replaces spaces with underscores, matching how
// endpoint names are surfaced as tool names.
func snakeCase(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), " ", "_")
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

// httpBlockPreloadScript is a Node.js --require preload that intercepts
// require() of raw HTTP client modules before any sandboxed MCP tool code
// runs, so outbound network access can only happen through the paths the
// sandbox explicitly allows.
const httpBlockPreloadScript = `'use strict';
// Fused sandbox — HTTP access is blocked in this environment.
// This preload intercepts require() before any user code runs.
const Module = require('module');
const _orig = Module._resolveFilename.bind(Module);
const BLOCKED = new Set([
  'http','node:http','https','node:https',
  'node-fetch','axios','got','cross-fetch',
  'undici','superagent','request','needle','node-fetch-native',
]);
Module._resolveFilename = function(req, parent, isMain, opts) {
  const base = req.split('/')[0]; // handle "axios/lib/…" paths
  if (BLOCKED.has(req) || BLOCKED.has(base))
    throw new Error('[Sandbox] HTTP modules are disabled. Got: ' + req);
  return _orig(req, parent, isMain, opts);
};
`

// writeHTTPBlockPreload keeps the defense-in-depth preload available to legacy
// sandbox execution paths while the current MCP runtime enforces network
// access inside its dependency-complete embedded bundle.
func writeHTTPBlockPreload(dir string) {
	path := filepath.Join(dir, "http-block-preload.cjs")
	if err := os.WriteFile(path, []byte(httpBlockPreloadScript), 0644); err != nil {
		slog.Error("Failed to write HTTP block preload script", slog.Any("error", err), slog.String("path", path))
	}
}

var legacySharedSandboxEntries = []string{"node_modules", "package.json", "package-lock.json"}

func removeLegacySharedSandboxDependencies(sharedDir string) error {
	for _, name := range legacySharedSandboxEntries {
		path := filepath.Join(sharedDir, name)
		if err := makeLegacyDependencyWritable(path); err != nil {
			return fmt.Errorf("make legacy sandbox dependency %s writable: %w", name, err)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove legacy sandbox dependency %s: %w", name, err)
		}
	}
	return nil
}

func makeLegacyDependencyWritable(root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	// Older Engines deliberately made the cache read-only. Restore only owner
	// write permission so the unprivileged Engine user can remove its own tree.
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// npm bin links need no permission change, and following an unexpected
		// symlink here could modify a target outside the retired cache tree.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()|0o200)
	})
}

// initSharedSandboxes removes the retired per-tenant dependency cache. The
// current MCP runtime and all of its npm dependencies are bundled by esbuild
// and embedded in the Engine binary, so retaining this cache only wastes PVC
// space. Exact known paths are removed to preserve any per-app directories.
func initSharedSandboxes() {
	sharedDir := sandboxesDir()
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		slog.Error("Failed to initialize sandbox data directory", slog.Any("error", err), slog.String("path", sharedDir))
		return
	}
	if err := removeLegacySharedSandboxDependencies(sharedDir); err != nil {
		slog.Warn("Failed to remove legacy sandbox dependencies", slog.Any("error", err))
	}

	// Keep this small compatibility asset current without installing packages.
	writeHTTPBlockPreload(sharedDir)
}
