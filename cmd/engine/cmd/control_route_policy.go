package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type routeResourceSource string

const (
	resourceFromWorkspace routeResourceSource = "workspace"
	resourceFromPath      routeResourceSource = "path"
	resourceFromQuery     routeResourceSource = "query"
)

type dynamicRequirementKind string

const (
	dynamicServiceCreate      dynamicRequirementKind = "service_create"
	dynamicBucketByName       dynamicRequirementKind = "bucket_by_name"
	dynamicSecretWrite        dynamicRequirementKind = "secret_write"
	dynamicWorkspaceApply     dynamicRequirementKind = "workspace_apply"
	dynamicConfigPlanAction   dynamicRequirementKind = "config_plan_action"
	dynamicWorkspacePlan      dynamicRequirementKind = "workspace_plan"
	dynamicDesiredConfigPlan  dynamicRequirementKind = "desired_config_plan"
	dynamicDesiredConfigApply dynamicRequirementKind = "desired_config_apply"
	dynamicSDKGenerate        dynamicRequirementKind = "sdk_generate"
	dynamicAppAccess          dynamicRequirementKind = "app_access"
	dynamicAppTokenAccess     dynamicRequirementKind = "app_token_access"
)

type controlRequirementResolver interface {
	ResolveControlRequirements(context.Context, accesscontrol.Actor, dynamicRequirementKind, map[string]string, *http.Request) ([]accesscontrol.Requirement, error)
}

type routeRequirement struct {
	permission   accesscontrol.Permission
	resourceType accesscontrol.ResourceType
	source       routeResourceSource
	pathParam    string
}

type controlRoutePolicy struct {
	method       string
	pattern      string
	subtree      bool
	requirements []routeRequirement
}

var dynamicControlRequirements = map[string]dynamicRequirementKind{
	http.MethodPost + " /workspace/services":                 dynamicServiceCreate,
	http.MethodDelete + " /workspace/buckets/{bucket_name}":  dynamicBucketByName,
	http.MethodPut + " /workspace/secrets":                   dynamicSecretWrite,
	http.MethodPut + " /workspace/secrets/bulk":              dynamicSecretWrite,
	http.MethodDelete + " /workspace/secrets":                dynamicSecretWrite,
	http.MethodPost + " /workspace/config/apply":             dynamicWorkspaceApply,
	http.MethodPost + " /workspace/config/plan":              dynamicWorkspacePlan,
	http.MethodPatch + " /config/plans/{plan_id}/actions":    dynamicConfigPlanAction,
	http.MethodPost + " /sdk-config/plan":                    dynamicDesiredConfigPlan,
	http.MethodPost + " /sdk-config/apply":                   dynamicDesiredConfigApply,
	http.MethodPost + " /mcp-config/plan":                    dynamicDesiredConfigPlan,
	http.MethodPost + " /mcp-config/apply":                   dynamicDesiredConfigApply,
	http.MethodPost + " /webhook-config/plan":                dynamicDesiredConfigPlan,
	http.MethodPost + " /webhook-config/apply":               dynamicDesiredConfigApply,
	http.MethodPost + " /sdks/generate":                      dynamicSDKGenerate,
	http.MethodPost + " /integrations/{service_id}/generate": dynamicSDKGenerate,
	http.MethodPost + " /apps/{app_id}/deprecate":            dynamicAppAccess,
	http.MethodPost + " /apps/{app_id}/undeprecate":          dynamicAppAccess,
	http.MethodDelete + " /apps/{app_id}/":                   dynamicAppAccess,
	http.MethodGet + " /sdks/{app_id}/download":              dynamicAppAccess,
	http.MethodPost + " /workspace/app-tokens":               dynamicAppTokenAccess,
	http.MethodDelete + " /workspace/app-tokens":             dynamicAppTokenAccess,
}

// These endpoints need a valid current identity but deliberately have no RBAC
// requirement: users must be able to inspect and revoke their own credential
// even after their workspace grants have been removed.
var authenticatedOnlyControlRoutes = map[string]struct{}{
	http.MethodGet + " /auth/whoami":      {},
	http.MethodPost + " /auth/cli/logout": {},
}

func workspaceRequirement(permission accesscontrol.Permission) routeRequirement {
	return routeRequirement{permission: permission, resourceType: accesscontrol.ResourceWorkspace, source: resourceFromWorkspace}
}

func pathRequirement(permission accesscontrol.Permission, resourceType accesscontrol.ResourceType, param string) routeRequirement {
	return routeRequirement{permission: permission, resourceType: resourceType, source: resourceFromPath, pathParam: param}
}

// controlRESTPolicies is the server-owned, machine-validated authorization
// contract for control-plane HTTP routes. Registry proxy routes are exact,
// even though their transport is mounted by prefix, so newly added Registry
// endpoints remain denied until this local policy is deliberately extended.
var controlRESTPolicies = []controlRoutePolicy{
	{http.MethodGet, "/auth/whoami", false, nil},
	{http.MethodPost, "/auth/cli/logout", false, nil},
	{http.MethodGet, "/audit/export", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAuditRead),
	}},
	{http.MethodPost, "/workspace/services/{service_id}/versions/{version_id}/refresh", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodDelete, "/workspace/services/{service_id}", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPost, "/workspace/services", false, nil},
	{http.MethodPut, "/workspace/buckets/{bucket_id}/values", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionBucketManage, accesscontrol.ResourceBucket, "bucket_id"),
	}},
	{http.MethodDelete, "/workspace/buckets/{bucket_id}/values", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionBucketManage, accesscontrol.ResourceBucket, "bucket_id"),
	}},
	{http.MethodPut, "/workspace/buckets/{bucket_id}/services/{service_id}/connect-config", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionCredentialsManage, accesscontrol.ResourceBucket, "bucket_id"),
		pathRequirement(accesscontrol.PermissionServiceConsume, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodGet, "/workspace/buckets/{bucket_id}/services/{service_id}/connect-config", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionCredentialsMetadataRead, accesscontrol.ResourceBucket, "bucket_id"),
		pathRequirement(accesscontrol.PermissionServiceRead, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPost, "/workspace/buckets/{bucket_id}/services/{service_id}/connect/sessions", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionConnectionManage, accesscontrol.ResourceBucket, "bucket_id"),
		pathRequirement(accesscontrol.PermissionServiceConsume, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodDelete, "/workspace/buckets/{bucket_id}/auth/connections/{connection_id}", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionConnectionManage, accesscontrol.ResourceBucket, "bucket_id"),
	}},
	{http.MethodPost, "/workspace/buckets", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionBucketManage),
	}},
	{http.MethodDelete, "/workspace/buckets/{bucket_name}", false, nil},
	{http.MethodPut, "/workspace/secrets", false, nil},
	{http.MethodPut, "/workspace/secrets/bulk", false, nil},
	{http.MethodDelete, "/workspace/secrets", false, nil},
	{http.MethodPost, "/workspace/app-tokens", false, nil},
	{http.MethodDelete, "/workspace/app-tokens", false, nil},

	{http.MethodPost, "/workspace/config/plan", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionWorkspaceRead),
	}},
	{http.MethodPost, "/workspace/config/apply", false, nil},
	{http.MethodPatch, "/config/plans/{plan_id}/actions", false, nil},
	{http.MethodPost, "/sdk-config/plan", false, nil},
	{http.MethodPost, "/sdk-config/apply", false, nil},
	{http.MethodPost, "/apps/{app_id}/deprecate", false, nil},
	{http.MethodPost, "/apps/{app_id}/undeprecate", false, nil},
	{http.MethodDelete, "/apps/{app_id}/", false, nil},
	{http.MethodPost, "/mcp-config/plan", false, nil},
	{http.MethodPost, "/mcp-config/apply", false, nil},
	{http.MethodPost, "/webhook-config/plan", false, nil},
	{http.MethodPost, "/webhook-config/apply", false, nil},

	{http.MethodPost, "/integrations/preview_openapi", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/import/plan", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/import/apply", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/start", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/respond", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/session/{session_id}/recover", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodPost, "/integrations/session/{session_id}/cancel", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodDelete, "/integrations/session/{session_id}", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueImport),
	}},
	{http.MethodGet, "/integrations/sessions/active", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueRead),
	}},
	{http.MethodGet, "/integrations/session/{session_id}", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueRead),
	}},
	{http.MethodGet, "/integrations/session/{session_id}/stream", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueRead),
	}},
	{http.MethodGet, "/integrations/{service_id}/versions/{version}/revision", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceRead, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPost, "/integrations/versions/revisions", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueRead),
	}},
	{http.MethodPost, "/integrations/versions/resolve", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionCatalogueRead),
	}},
	{http.MethodDelete, "/integrations/{service_id}", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/versions/{version}/deprecate", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/versions/{version}/public", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/versions/{version}/execution-policy", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/public", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/execution-policy", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPut, "/integrations/{service_id}/drift-watch", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPost, "/integrations/{service_id}/drift/{drift_id}/dismiss", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, "service_id"),
	}},
	{http.MethodPost, "/integrations/{service_id}/generate", false, []routeRequirement{
		pathRequirement(accesscontrol.PermissionServiceConsume, accesscontrol.ResourceService, "service_id"),
		workspaceRequirement(accesscontrol.PermissionAppCreate),
	}},
	{http.MethodGet, "/account", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAccountRead),
	}},
	{http.MethodGet, "/account/balance/stream", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionBillingRead),
	}},
	{http.MethodPut, "/account", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAccountManage),
	}},
	{http.MethodPost, "/account/regenerate-key", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAccountManage),
	}},
	{http.MethodPut, "/account/auto-topup", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionBillingManage),
	}},
	{http.MethodGet, "/credits/pricing", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionBillingRead),
	}},
	{http.MethodPost, "/leads", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAccountManage),
	}},
	{http.MethodPost, "/sdks/generate", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAppCreate),
	}},
	{http.MethodGet, "/sdks/{app_id}/download", false, nil},
	{http.MethodGet, "/sdks/job/{job_id}/stream", false, []routeRequirement{
		workspaceRequirement(accesscontrol.PermissionAppRead),
	}},
}

func controlAuthorizationMiddlewareWithAudit(authorizer accesscontrol.Authorizer, resolver controlRequirementResolver, recorder accesscontrol.AuditRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveControlAuthorizationRequest(w, r, next, authorizer, resolver, recorder)
		})
	}
}

func serveControlAuthorizationRequest(w http.ResponseWriter, r *http.Request, next http.Handler, authorizer accesscontrol.Authorizer, resolver controlRequirementResolver, recorder accesscontrol.AuditRecorder) {
	if classifyEngineRequest(r) != requestClassControl || isGraphQLControlPath(r.URL.Path) {
		next.ServeHTTP(w, r)
		return
	}
	started := time.Now()
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	if !ok {
		denyUnauthenticatedControlRequest(w, r, recorder, started)
		return
	}
	requirements, policy, resolved := resolveControlRESTPolicy(r, resolver)
	if !resolved || authorizer == nil {
		denyUnclassifiedControlRequest(w, r, recorder, actor, policy, started)
		return
	}
	if !authorizeControlRequirements(w, r, recorder, actor, authorizer, resolver, requirements, policy, started) {
		return
	}
	serveAuthorizedControlRequest(w, r, next, recorder, actor, requirements, policy)
}

func denyUnauthenticatedControlRequest(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, started time.Time) {
	finishControlAuthorization(w, r, started, "", 0, "unauthenticated")
	_ = recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, controlAuditAction(r.Method), r.URL.Path, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "unauthenticated"))
	accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
}

func denyUnclassifiedControlRequest(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, policy string, started time.Time) {
	finishControlAuthorization(w, r, started, policy, 0, "unclassified")
	_ = recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, nil, accesscontrol.AuditDenied, http.StatusForbidden, "policy_denied"))
	accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrPolicyDenied)
}

func authorizeControlRequirements(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, authorizer accesscontrol.Authorizer, resolver controlRequirementResolver, requirements []accesscontrol.Requirement, policy string, started time.Time) bool {
	err := accesscontrol.AuthorizeAll(r.Context(), authorizer, requirements...)
	outcome := "allowed"
	if err != nil {
		outcome = "denied"
	}
	finishControlAuthorization(w, r, started, policy, len(requirements), outcome)
	if err == nil {
		return true
	}
	err = enrichControlPermissionDenial(r.Context(), actor, authorizer, resolver, err)
	event := newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, requirements, accesscontrol.AuditDenied, http.StatusForbidden, "permission_denied")
	setAuditMissingRequirements(&event, accesscontrol.MissingRequirements(err))
	_ = recordControlAudit(r.Context(), recorder, event)
	accesscontrol.WriteAuthorizationError(w, err)
	return false
}

func serveAuthorizedControlRequest(w http.ResponseWriter, r *http.Request, next http.Handler, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement, policy string) {
	authorizedRequest := r.WithContext(accesscontrol.ContextWithRequiredPermissions(r.Context(), requirements))
	mutation := isControlMutation(r.Method)
	sensitiveRead := !mutation && requiresSensitiveReadAudit(requirements)
	if !mutation && !sensitiveRead {
		next.ServeHTTP(w, authorizedRequest)
		return
	}
	// Mutations and sensitive reads fail before execution when the durable audit
	// sink is unavailable; otherwise a successful user action could be
	// impossible to reconstruct during incident response.
	if !recordAuthorizedControlAttempt(w, r, recorder, actor, requirements, policy) {
		return
	}
	streamingRead := sensitiveRead && isStreamingControlRequest(r.URL.Path)
	writer := newAuditStatusWriter(w, mutation || (sensitiveRead && !streamingRead), false, mutation)
	if recovered, panicked := serveAuditedHandler(next, writer, authorizedRequest); panicked {
		recordAuthorizedControlPanic(r, recorder, actor, requirements, policy)
		panic(recovered)
	}
	finalizeAuthorizedControlAudit(r, recorder, actor, requirements, policy, sensitiveRead && !streamingRead, writer)
}

func isControlMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

func recordAuthorizedControlAttempt(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement, policy string) bool {
	preflight := newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, requirements, accesscontrol.AuditAttempted, 0, "attempted")
	if err := requireControlAudit(r.Context(), recorder, preflight); err != nil {
		slog.ErrorContext(r.Context(), "control mutation blocked because audit is unavailable", slog.Any("error", err))
		http.Error(w, `{"error":"audit service unavailable"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func recordAuthorizedControlPanic(r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement, policy string) {
	event := newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, requirements, accesscontrol.AuditFailed, http.StatusInternalServerError, "handler_panic")
	_ = recordControlAudit(r.Context(), recorder, event)
}

func finalizeAuthorizedControlAudit(r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement, policy string, failClosed bool, writer *auditStatusWriter) {
	status := writer.statusCode()
	outcome := controlAuditOutcome(status)
	reason := controlAuditReason(outcome, status)
	if writer.bodyOverflow {
		status, outcome, reason = http.StatusServiceUnavailable, accesscontrol.AuditFailed, "response_too_large"
	}
	event := newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, requirements, outcome, status, reason)
	err := recordControlAudit(r.Context(), recorder, event)
	finishAuditedResponse(writer, failClosed, err)
}

func isStreamingControlRequest(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(path, "/"), "/stream")
}

func requiresSensitiveReadAudit(requirements []accesscontrol.Requirement) bool {
	for _, requirement := range requirements {
		switch requirement.Permission {
		case accesscontrol.PermissionBucketValuesRead,
			accesscontrol.PermissionCredentialsMetadataRead,
			accesscontrol.PermissionAppTokensManage,
			accesscontrol.PermissionConnectionRead,
			accesscontrol.PermissionAccountRead,
			accesscontrol.PermissionBillingRead,
			accesscontrol.PermissionAuditRead,
			accesscontrol.PermissionAccessRead:
			return true
		}
	}
	return false
}

func firstControlRequirementResolver(resolvers []controlRequirementResolver) controlRequirementResolver {
	if len(resolvers) == 0 {
		return nil
	}
	return resolvers[0]
}

func isGraphQLControlPath(path string) bool {
	return path == "/graphql" || path == "/engine/graphql"
}

func resolveControlRESTPolicy(r *http.Request, resolver controlRequirementResolver) ([]accesscontrol.Requirement, string, bool) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	if !ok {
		return nil, "", false
	}
	for _, policy := range controlRESTPolicies {
		params, matched := matchControlRoute(policy, r.Method, r.URL.Path)
		if !matched {
			continue
		}
		requirements, valid := materializeRouteRequirements(policy.requirements, params, r.URL.Query(), actor.WorkspaceID)
		if !valid {
			continue
		}
		kind := dynamicControlRequirements[policy.method+" "+policy.pattern]
		if kind != "" {
			// Body- and database-derived requirements are part of the same all-or-
			// nothing authorization decision. Missing resolution is a denial, never
			// permission to run with only the static subset.
			if resolver == nil {
				return nil, policy.pattern, false
			}
			dynamic, err := resolver.ResolveControlRequirements(r.Context(), actor, kind, params, r)
			if err != nil {
				return nil, policy.pattern, false
			}
			requirements = append(requirements, dynamic...)
		}
		return requirements, policy.pattern, true
	}
	return nil, "", false
}

func matchControlRoute(policy controlRoutePolicy, method, path string) (map[string]string, bool) {
	if policy.method != method {
		return nil, false
	}
	if policy.subtree {
		return matchRoutePrefix(policy.pattern, path)
	}
	return matchRouteSegments(policy.pattern, path)
}

func matchRoutePrefix(pattern, path string) (map[string]string, bool) {
	patternParts := splitRoutePath(pattern)
	pathParts := splitRoutePath(path)
	if len(pathParts) < len(patternParts) {
		return nil, false
	}
	return matchRouteParts(patternParts, pathParts[:len(patternParts)])
}

func matchRouteSegments(pattern, path string) (map[string]string, bool) {
	patternParts := splitRoutePath(pattern)
	pathParts := splitRoutePath(path)
	if len(patternParts) != len(pathParts) {
		return nil, false
	}
	return matchRouteParts(patternParts, pathParts)
}

func matchRouteParts(patternParts, pathParts []string) (map[string]string, bool) {
	params := make(map[string]string)
	for i, patternPart := range patternParts {
		if strings.HasPrefix(patternPart, "{") && strings.HasSuffix(patternPart, "}") {
			params[strings.TrimSuffix(strings.TrimPrefix(patternPart, "{"), "}")] = pathParts[i]
			continue
		}
		if patternPart != pathParts[i] {
			return nil, false
		}
	}
	return params, true
}

func splitRoutePath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func materializeRouteRequirements(templates []routeRequirement, params map[string]string, query map[string][]string, workspaceID uuid.UUID) ([]accesscontrol.Requirement, bool) {
	requirements := make([]accesscontrol.Requirement, 0, len(templates))
	for _, template := range templates {
		resourceID := workspaceID
		if template.source == resourceFromPath {
			parsed, err := uuid.Parse(params[template.pathParam])
			if err != nil {
				// Invalid resource identities cannot be authorized safely. Leave
				// request-shape errors to the handler only after a valid policy exists.
				return nil, false
			}
			resourceID = parsed
		}
		if template.source == resourceFromQuery {
			parsed, err := uuid.Parse(firstQueryValue(query[template.pathParam]))
			if err != nil {
				return nil, false
			}
			resourceID = parsed
		}
		requirements = append(requirements, accesscontrol.Requirement{
			Permission: template.permission,
			Resource:   accesscontrol.ResourceRef{Type: template.resourceType, ID: resourceID},
		})
	}
	return requirements, true
}

func firstQueryValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func finishControlAuthorization(w http.ResponseWriter, r *http.Request, started time.Time, policy string, requirements int, outcome string) {
	w.Header().Add("Server-Timing", fmt.Sprintf("engine_authz;dur=%.3f", float64(time.Since(started).Microseconds())/1000))
	trace.SpanFromContext(r.Context()).AddEvent("engine.authorization.route", trace.WithAttributes(
		attribute.String("engine.authorization.policy", policy),
		attribute.String("engine.authorization.method", r.Method),
		attribute.Int("engine.authorization.requirements", requirements),
		attribute.String("engine.authorization.outcome", outcome),
	))
}

func validateControlRESTPolicies() error {
	seen := make(map[string]struct{}, len(controlRESTPolicies))
	for _, policy := range controlRESTPolicies {
		if err := validateControlRESTPolicy(policy, seen); err != nil {
			return err
		}
	}
	if err := validateDynamicPolicyReferences(seen); err != nil {
		return err
	}
	return validateAuthenticatedOnlyPolicyReferences(seen)
}

func validateControlRESTPolicy(policy controlRoutePolicy, seen map[string]struct{}) error {
	key := policy.method + " " + policy.pattern
	if _, duplicate := seen[key]; duplicate {
		return fmt.Errorf("duplicate control route policy: %s", key)
	}
	seen[key] = struct{}{}
	if err := validateControlRequirementMode(key, policy); err != nil {
		return err
	}
	if err := validateRouteRequirements(policy); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if policy.subtree && isRegistryProxyPolicy(policy.pattern) {
		return fmt.Errorf("Registry proxy policy must be exact: %s", key)
	}
	return nil
}

func validateControlRequirementMode(key string, policy controlRoutePolicy) error {
	_, authenticatedOnly := authenticatedOnlyControlRoutes[key]
	if authenticatedOnly && len(policy.requirements) != 0 {
		return fmt.Errorf("authenticated-only route has RBAC requirements: %s", key)
	}
	if len(policy.requirements) == 0 && dynamicControlRequirements[key] == "" && !authenticatedOnly {
		return fmt.Errorf("control route policy has no requirements: %s", key)
	}
	return nil
}

func isRegistryProxyPolicy(pattern string) bool {
	for _, prefix := range []string{"/integrations", "/account", "/credits", "/leads", "/sdks"} {
		if pattern == prefix || strings.HasPrefix(pattern, prefix+"/") {
			return true
		}
	}
	return false
}

func validateDynamicPolicyReferences(policies map[string]struct{}) error {
	for key := range dynamicControlRequirements {
		if _, ok := policies[key]; !ok {
			return fmt.Errorf("dynamic resolver has no route policy: %s", key)
		}
	}
	return nil
}

func validateAuthenticatedOnlyPolicyReferences(policies map[string]struct{}) error {
	for key := range authenticatedOnlyControlRoutes {
		if _, ok := policies[key]; !ok {
			return fmt.Errorf("authenticated-only route has no policy: %s", key)
		}
		if dynamicControlRequirements[key] != "" {
			return fmt.Errorf("authenticated-only route has dynamic requirements: %s", key)
		}
	}
	return nil
}

func validateRouteRequirements(policy controlRoutePolicy) error {
	for _, requirement := range policy.requirements {
		if err := validateRouteRequirement(policy.pattern, requirement); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteRequirement(pattern string, requirement routeRequirement) error {
	if err := accesscontrol.ValidatePermission(requirement.permission); err != nil {
		return err
	}
	if err := accesscontrol.ValidateResourceType(requirement.resourceType); err != nil {
		return err
	}
	switch requirement.source {
	case resourceFromWorkspace:
		if requirement.resourceType != accesscontrol.ResourceWorkspace {
			return fmt.Errorf("workspace source must use workspace resource type")
		}
	case resourceFromPath:
		if !strings.Contains(pattern, "{"+requirement.pathParam+"}") {
			return fmt.Errorf("path parameter %q is absent", requirement.pathParam)
		}
	case resourceFromQuery:
		if requirement.pathParam == "" {
			return fmt.Errorf("query parameter is required")
		}
	default:
		return fmt.Errorf("unknown resource source %q", requirement.source)
	}
	return nil
}
