package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// This file is the Engine-native GraphQL surface for MCP server management:
// list, deploy, kill (=deactivate), reactivate, delete, and analytics. It
// exists as its own schema/endpoint (mounted at POST /engine/graphql, see
// MountMCPGraphQLRoute) rather than folded into the existing POST /graphql,
// which is a pure Registry forward-proxy (graphql_proxy.go) with no
// resolvers of its own -- adding Engine-native fields there would mean
// teaching that proxy to selectively intercept certain operations instead of
// forwarding everything, a much larger and riskier change than a second
// endpoint.
//
// Resolvers here call the same unexported helpers the REST lifecycle
// handlers (sdk_lifecycle_handlers.go) already use -- activateArtifactScope,
// ensureArtifactScopeOwnedBy, sandbox.KillMCPSessionsForSDK, etc -- rather than
// reimplementing that logic, so the REST and GraphQL surfaces can't drift
// apart on what "kill" or "deploy" actually does. This file only owns
// request/response shape translation.

// mcpGraphQLContextKey namespaces this file's two context values
// (authenticated actor, inbound request) separately from the package's other
// context.WithValue usages (e.g. sandbox.go's "sdk-token"), which all use
// bare string keys -- an unexported type here avoids colliding with any of
// those by construction, at the one call site (MCPGraphQLHandler) that needs
// to be internally consistent, not interoperate with those other keys.
type mcpGraphQLContextKey string

const (
	mcpGraphQLActorContextKey mcpGraphQLContextKey = "mcp_graphql_actor"
	mcpGraphQLRequestKey      mcpGraphQLContextKey = "mcp_graphql_request"
	mcpGraphQLRevisionSinkKey mcpGraphQLContextKey = "mcp_graphql_revision_sink"
)

type authorizationRevisionSink interface {
	SetRevision(int64) bool
}

// mcpGraphQLActor is the authenticated caller, resolved once per request
// (mirroring resolveWorkspaceActor's single call per REST handler) rather
// than once per resolver -- a query touching multiple fields would otherwise
// re-validate the same API key repeatedly.
type mcpGraphQLActor struct {
	accountID uuid.UUID
}

func actorFromContext(ctx context.Context) (mcpGraphQLActor, error) {
	actor, ok := ctx.Value(mcpGraphQLActorContextKey).(mcpGraphQLActor)
	if !ok {
		return mcpGraphQLActor{}, fmt.Errorf("unauthenticated")
	}
	return actor, nil
}

func requestFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(mcpGraphQLRequestKey).(*http.Request)
	return r
}

// MountMCPGraphQLRoute registers the Engine-native MCP GraphQL endpoint.
func MountMCPGraphQLRoute(mux interface {
	Post(pattern string, handlerFn http.HandlerFunc)
}, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, masterKey []byte, revisionSinks ...authorizationRevisionSink) error {
	schema, err := newMCPGraphQLSchema(configStore, s, verifier, registryClient, masterKey)
	if err != nil {
		return fmt.Errorf("build mcp graphql schema: %w", err)
	}
	slugResolver, _ := registryClient.(sdkServiceSlugResolver)
	resources := graphQLAuthorizationResources{store: s, configStore: configStore, slugResolver: slugResolver, revisionSink: firstAuthorizationRevisionSink(revisionSinks)}
	mux.Post("/engine/graphql", mcpGraphQLHandler(schema, resources))
	return nil
}

// mcpGraphQLHandler consumes the Actor hydrated by the control middleware and
// authorizes the complete operation before graphql-go invokes any resolver.
func mcpGraphQLHandler(schema graphql.Schema, resourceResolvers ...graphQLAuthorizationResources) http.HandlerFunc {
	isDev := os.Getenv("FUSED_ENV") == "development"
	h := handler.New(&handler.Config{Schema: &schema, Pretty: isDev, GraphiQL: isDev})
	resources := firstGraphQLAuthorizationResources(resourceResolvers)

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authStart := time.Now()
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		authDur := time.Since(authStart)
		if !ok {
			setEngineGraphQLServerTiming(w.Header(), engineGraphQLTiming{auth: authDur, total: time.Since(start)})
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
			return
		}

		authorizationStart := time.Now()
		plan, err := authorizeEngineGraphQL(r, &schema, actor, resources, isDev)
		// Publish the exact resolved authorization plan to an outer audit
		// middleware even when authorization denies the operation.
		accesscontrol.CaptureRequiredPermissions(r.Context(), plan.requirements)
		accesscontrol.CaptureMissingPermissions(r.Context(), accesscontrol.MissingRequirements(err))
		authorizationDur := time.Since(authorizationStart)
		if err != nil {
			setEngineGraphQLServerTiming(w.Header(), engineGraphQLTiming{auth: authDur, authorize: authorizationDur, total: time.Since(start)})
			writeEngineGraphQLAuthorizationError(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), mcpGraphQLActorContextKey, mcpGraphQLActor{accountID: actor.AccountID})
		ctx = context.WithValue(ctx, mcpGraphQLRequestKey, r)
		if resources.revisionSink != nil {
			ctx = context.WithValue(ctx, mcpGraphQLRevisionSinkKey, resources.revisionSink)
		}
		ctx = context.WithValue(ctx, graphQLResolvedConnectionsContextKey{}, plan.resolvedConnections)
		// One auth-config cache per HTTP request so workspaceServices and
		// workspaceServicePage (or any future field needing the same
		// per-service auth options) share a single batched Registry call
		// instead of one each -- see fetchWorkspaceServiceAuthOptions.
		ctx = context.WithValue(ctx, mcpGraphQLAuthConfigCacheKey{}, newAuthConfigCache())

		execStart := time.Now()
		bw := newBufferedResponseWriter()
		h.ServeHTTP(bw, r.WithContext(ctx))
		if len(plan.deployments) > 0 {
			revisionLoader, _ := resources.store.(accesscontrol.AuthorizationRevisionLoader)
			if revisionLoader != nil && resources.revisionSink != nil {
				// A deployment can create its owner binding. Publish that revision
				// before the response allows an immediate follow-up request.
				syncAuthorizationRevision(ctx, revisionLoader, resources.revisionSink)
			}
		}
		execDur := time.Since(execStart)
		setEngineGraphQLServerTiming(bw.Header(), engineGraphQLTiming{
			auth:      authDur,
			authorize: authorizationDur,
			execute:   execDur,
			total:     time.Since(start),
		})
		bw.flushTo(w)
	}
}

func firstAuthorizationRevisionSink(values []authorizationRevisionSink) authorizationRevisionSink {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

type engineGraphQLTiming struct {
	auth      time.Duration
	authorize time.Duration
	execute   time.Duration
	total     time.Duration
}

func setEngineGraphQLServerTiming(header http.Header, timing engineGraphQLTiming) {
	parts := []string{serverTimingMetric("engine_auth", timing.auth)}
	if timing.authorize > 0 {
		parts = append(parts, serverTimingMetric("engine_authz", timing.authorize))
	}
	if timing.execute > 0 {
		parts = append(parts, serverTimingMetric("engine_graphql", timing.execute))
	}
	parts = append(parts, serverTimingMetric("engine_total", timing.total))
	header.Set("Server-Timing", strings.Join(parts, ", "))
}

func authorizeEngineGraphQL(r *http.Request, schema *graphql.Schema, actor accesscontrol.Actor, resources graphQLAuthorizationResources, allowIntrospection bool) (graphQLAuthorizationPlan, error) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.graphql.authorization")
	defer span.End()
	body, err := readAndRestoreBody(r)
	if err != nil {
		recordGraphQLAuthorizationSpan(span, graphQLAuthorizationPlan{}, "invalid")
		return graphQLAuthorizationPlan{}, fmt.Errorf("%w: read body", errInvalidGraphQLRequest)
	}
	plan, err := buildGraphQLAuthorizationPlanWithOptions(schema, body, actor.WorkspaceID, allowIntrospection)
	if err == nil {
		if limitErr := accesscontrol.ValidateAuditableRequirementCount(plan.requirements); limitErr != nil {
			err = fmt.Errorf("%w: %v", errInvalidGraphQLRequest, limitErr)
		}
	}
	if err == nil {
		// Static gates run before resource lookups so an obviously unauthorized
		// deployment cannot probe bucket names or trigger Registry resolution.
		err = authorizeGraphQLPlan(ctx, actor, plan)
	}
	if err == nil {
		err = resolveDynamicGraphQLResources(ctx, &plan, resources, actor.WorkspaceID, r.Header.Get("X-API-Key"))
	}
	if err == nil {
		if limitErr := accesscontrol.ValidateAuditableRequirementCount(plan.requirements); limitErr != nil {
			err = fmt.Errorf("%w: %v", errInvalidGraphQLRequest, limitErr)
		}
	}
	if err == nil {
		err = authorizeGraphQLPlan(ctx, actor, plan)
	}
	outcome := "allowed"
	if err != nil {
		outcome = "denied"
	}
	recordGraphQLAuthorizationSpan(span, plan, outcome)
	return plan, err
}

func resolveDynamicGraphQLResources(ctx context.Context, plan *graphQLAuthorizationPlan, resources graphQLAuthorizationResources, workspaceID uuid.UUID, apiKey string) error {
	deploymentRequirements, err := resources.resolveDeployments(ctx, workspaceID, plan.deployments, apiKey)
	if err != nil {
		return err
	}
	connectionRequirements, connections, err := resources.resolveConnections(ctx, plan.connections)
	if err != nil {
		return err
	}
	plan.mergeRequirements(deploymentRequirements)
	plan.mergeRequirements(connectionRequirements)
	plan.resolvedConnections = connections
	return nil
}

func firstGraphQLAuthorizationResources(values []graphQLAuthorizationResources) graphQLAuthorizationResources {
	if len(values) == 0 {
		return graphQLAuthorizationResources{}
	}
	return values[0]
}

func authorizeGraphQLPlan(ctx context.Context, actor accesscontrol.Actor, plan graphQLAuthorizationPlan) error {
	authorizer := accesscontrol.SnapshotAuthorizer{}
	if err := authorizer.CheckAll(ctx, actor, plan.requirements...); err != nil {
		return err
	}
	for _, request := range plan.scopes {
		scope, err := authorizer.Scope(ctx, actor, request.permission, request.resource)
		if err != nil {
			return err
		}
		if !scope.All && len(scope.IDs) == 0 {
			return &accesscontrol.PermissionDeniedError{Missing: []accesscontrol.Requirement{{
				Permission: request.permission,
				Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
			}}}
		}
	}
	return nil
}

func recordGraphQLAuthorizationSpan(span trace.Span, plan graphQLAuthorizationPlan, outcome string) {
	span.SetAttributes(
		attribute.String("engine.authorization.outcome", outcome),
		attribute.Int("engine.authorization.requirements", len(plan.requirements)),
		attribute.Int("engine.graphql.root_fields", plan.rootFields),
	)
}

func writeEngineGraphQLAuthorizationError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInvalidGraphQLRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_graphql_request"})
		return
	}
	accesscontrol.WriteAuthorizationError(w, err)
}

// ─── Types ──────────────────────────────────────────────────────────────────

var mcpServerType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPServer",
	Fields: graphql.Fields{
		"id":              &graphql.Field{Type: graphql.String},
		"name":            &graphql.Field{Type: graphql.String},
		"version":         &graphql.Field{Type: graphql.String},
		"config_key":      &graphql.Field{Type: graphql.String},
		"mcp_url":         &graphql.Field{Type: graphql.String},
		"execution_token": &graphql.Field{Type: graphql.String},
		"active":          &graphql.Field{Type: graphql.Boolean},
		"deactivated_at":  &graphql.Field{Type: graphql.String},
		"created_at":      &graphql.Field{Type: graphql.String},
	},
})

var mcpServerListType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPServerList",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(mcpServerType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var mcpToolUsageType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPToolUsage",
	Fields: graphql.Fields{
		"tool_name":       &graphql.Field{Type: graphql.String},
		"count":           &graphql.Field{Type: graphql.Int},
		"failed":          &graphql.Field{Type: graphql.Int},
		"average_latency": &graphql.Field{Type: graphql.Float},
	},
})

var mcpServiceUsageType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPServiceUsage",
	Fields: graphql.Fields{
		"service_name":    &graphql.Field{Type: graphql.String},
		"count":           &graphql.Field{Type: graphql.Int},
		"failed":          &graphql.Field{Type: graphql.Int},
		"average_latency": &graphql.Field{Type: graphql.Float},
	},
})

var mcpSessionSummaryType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPSessionSummary",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.String},
		"session_id": &graphql.Field{Type: graphql.String},
		"started_at": &graphql.Field{Type: graphql.String},
		"ended_at":   &graphql.Field{Type: graphql.String},
	},
})

var mcpAnalyticsDashboardType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPAnalyticsDashboard",
	Fields: graphql.Fields{
		"total_requests":  &graphql.Field{Type: graphql.Int},
		"failed_requests": &graphql.Field{Type: graphql.Int},
		"average_latency": &graphql.Field{Type: graphql.Float},
		"active_agents":   &graphql.Field{Type: graphql.Int},
		"tool_usage":      &graphql.Field{Type: graphql.NewList(mcpToolUsageType)},
		"service_usage":   &graphql.Field{Type: graphql.NewList(mcpServiceUsageType)},
		"recent_sessions": &graphql.Field{Type: graphql.NewList(mcpSessionSummaryType)},
	},
})

func newMCPGraphQLSchema(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, masterKey []byte) (graphql.Schema, error) {
	publicInsightReader := newPublicInsightReader(registryClient)
	query := graphql.NewObject(graphql.ObjectConfig{
		// The endpoint started with MCP operations, but it now owns broader
		// Engine reads like buckets and Connect auth. The root type name appears
		// in GraphQL validation errors, so keep it aligned with that surface.
		Name: "EngineQuery",
		Fields: graphql.Fields{
			"currentActorAccess":          currentActorAccessGraphQLField(),
			"artifact":                    artifactGraphQLField(s),
			"artifactServices":            artifactServicesGraphQLField(s),
			"artifacts":                   artifactsGraphQLField(s),
			"accessExplanation":           accessExplanationGraphQLField(s),
			"auditEvents":                 auditEventsGraphQLField(s),
			"artifactBuildSelectors":      artifactBuildSelectorsGraphQLField(s),
			"artifactOwningTeams":         artifactOwningTeamsGraphQLField(s),
			"users":                       usersGraphQLField(s),
			"user":                        userGraphQLField(s),
			"userEffectiveAccess":         userEffectiveAccessGraphQLField(s),
			"teamMembers":                 teamMembersGraphQLField(s),
			"teams":                       teamsGraphQLField(s),
			"team":                        teamGraphQLField(s),
			"workspaceShares":             workspaceSharesGraphQLField(s),
			"bucketReference":             bucketReferenceGraphQLField(s),
			"serviceReference":            serviceReferenceGraphQLField(s),
			"artifactReference":           artifactReferenceGraphQLField(s),
			"workspaceConnectionProfile":  workspaceConnectionProfileGraphQLField(s),
			"workspaceConnectConfigs":     workspaceConnectConfigsGraphQLField(s),
			"mcpServers":                  mcpServersField(s),
			"mcpServerByName":             mcpServerByNameField(s),
			"mcpAnalytics":                mcpAnalyticsField(s),
			"bucketSummaries":             bucketSummariesGraphQLField(s),
			"bucketSummary":               bucketSummaryGraphQLField(s),
			"bucketSummaryPage":           bucketSummaryPageGraphQLField(s),
			"bucketConnectSummary":        bucketConnectSummaryGraphQLField(s),
			"workspaceServices":           workspaceServicesGraphQLField(s, verifier),
			"workspaceServicePage":        workspaceServicePageGraphQLField(s, verifier),
			"workspaceWebhooks":           workspaceWebhooksGraphQLField(s),
			"webhookEvents":               webhookEventsGraphQLField(s),
			"webhookAnalytics":            webhookAnalyticsGraphQLField(s),
			"engineExecutionEvents":       engineExecutionEventsGraphQLField(s),
			"engineExecutionAnalytics":    engineExecutionAnalyticsGraphQLField(s),
			"workspaceExecutionAnalytics": workspaceExecutionAnalyticsGraphQLField(s),
			"publicServiceInsights":       publicServiceInsightsGraphQLField(s, publicInsightReader),
			"serviceConsumers":            serviceConsumersGraphQLField(s),
			"workspaceNotifications":      workspaceNotificationsGraphQLField(configStore, s, registryClient),
			"sdkTokens":                   sdkTokensGraphQLField(s),
			"sdkBuckets":                  sdkBucketsGraphQLField(s),
			"bucketSDKPage":               bucketSDKPageGraphQLField(s),
			"bucketServicePage":           bucketServicePageGraphQLField(s),
			"bucketValues":                bucketValuesGraphQLField(s),
			"bucketValuePage":             bucketValuePageGraphQLField(s),
			"secretMetas":                 secretMetasGraphQLField(s),
			"secretMetaPage":              secretMetaPageGraphQLField(s),
			"authConnections":             authConnectionsGraphQLField(s),
			"authConnectionPage":          authConnectionPageGraphQLField(s),
			"connectionResources":         connectionResourcesGraphQLField(s),
		},
	})
	mutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "EngineMutation",
		Fields: graphql.Fields{
			"createUser":                        createUserGraphQLField(s),
			"updateUser":                        updateUserGraphQLField(s),
			"suspendUser":                       suspendUserGraphQLField(s),
			"reactivateUser":                    reactivateUserGraphQLField(s),
			"addTeamMember":                     addTeamMemberGraphQLField(s),
			"removeTeamMember":                  removeTeamMemberGraphQLField(s),
			"issueUserCredential":               issueUserCredentialGraphQLField(s),
			"revokeUserCredential":              revokeUserCredentialGraphQLField(s),
			"createTeam":                        createTeamGraphQLField(s),
			"updateTeam":                        updateTeamGraphQLField(s),
			"archiveTeam":                       archiveTeamGraphQLField(s),
			"setTeamWorkspaceRole":              setTeamWorkspaceRoleGraphQLField(s),
			"grantTeamServiceAccess":            grantTeamServiceAccessGraphQLField(s),
			"revokeTeamServiceAccess":           revokeTeamServiceAccessGraphQLField(s),
			"grantTeamBucketAccess":             grantTeamBucketAccessGraphQLField(s),
			"revokeTeamBucketAccess":            revokeTeamBucketAccessGraphQLField(s),
			"grantTeamArtifactAccess":           grantTeamArtifactAccessGraphQLField(s),
			"revokeTeamArtifactAccess":          revokeTeamArtifactAccessGraphQLField(s),
			"grantWorkspaceBucketAccess":        grantWorkspaceBucketAccessGraphQLField(s),
			"revokeWorkspaceBucketAccess":       revokeWorkspaceBucketAccessGraphQLField(s),
			"grantWorkspaceArtifactAccess":      grantWorkspaceArtifactAccessGraphQLField(s),
			"revokeWorkspaceArtifactAccess":     revokeWorkspaceArtifactAccessGraphQLField(s),
			"setWorkspaceConnectionProfile":     setWorkspaceConnectionProfileGraphQLField(s, verifier, registryClient),
			"resetWorkspaceConnectionProfile":   resetWorkspaceConnectionProfileGraphQLField(s),
			"updateWorkspaceNotificationStatus": updateWorkspaceNotificationStatusGraphQLField(configStore),
			"deployMcpServer":                   deployMCPServerField(configStore, s, registryClient),
			"killMcpServer":                     killMCPServerField(s),
			"reactivateMcpServer":               reactivateMCPServerField(s),
			"deleteMcpServer":                   deleteMCPServerField(s),
			"upsertSecrets":                     upsertSecretsGraphQLField(s, masterKey),
			"deleteSecrets":                     deleteSecretsGraphQLField(s),
			"startConnectSession":               startConnectSessionGraphQLField(s, verifier, masterKey),
			"deleteAuthConnection":              deleteAuthConnectionGraphQLField(s),
			"setDefaultConnectionResource":      setDefaultConnectionResourceGraphQLField(s),
			"rediscoverConnectionResources":     rediscoverConnectionResourcesGraphQLField(s, verifier, masterKey),
			"refreshMissingServiceContracts":    refreshMissingServiceContractsGraphQLField(s, registryBatchRuntimeContractFetcher(registryClient)),
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: query, Mutation: mutation})
	if err != nil {
		return graphql.Schema{}, err
	}
	if err := validateGraphQLAuthorizationPolicy(&schema, engineGraphQLPolicy); err != nil {
		return graphql.Schema{}, err
	}
	return schema, nil
}

func registryBatchRuntimeContractFetcher(registryClient sandbox.RegistryClient) BatchRuntimeContractFetcher {
	fetcher, _ := registryClient.(BatchRuntimeContractFetcher)
	return fetcher
}

// ─── mcpServers(limit, offset) ─────────────────────────────────────────────

func mcpServersField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpServerListType,
		Args: graphql.FieldConfigArgument{
			"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 10},
			"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			limit, _ := p.Args["limit"].(int)
			offset, _ := p.Args["offset"].(int)
			if limit <= 0 {
				limit = 10
			}
			authorized, err := graphQLAuthorizedScope(p.Context, accesscontrol.PermissionArtifactRead, accesscontrol.ResourceArtifact)
			if err != nil {
				return nil, err
			}
			scopes, total, err := s.ListAuthorizedMCPScopesByAccount(p.Context, actor.accountID, authorized, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list mcp servers: %w", err)
			}
			items := make([]map[string]interface{}, 0, len(scopes))
			for _, scope := range scopes {
				items = append(items, mcpServerFields(requestFromContext(p.Context), scope))
			}
			return map[string]interface{}{"items": items, "total": total}, nil
		},
	}
}

func graphQLAuthorizedScope(ctx context.Context, permission accesscontrol.Permission, resource accesscontrol.ResourceType) (accesscontrol.AuthorizedScope, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return accesscontrol.AuthorizedScope{}, accesscontrol.ErrAuthenticationRequired
	}
	return (accesscontrol.SnapshotAuthorizer{}).Scope(ctx, actor, permission, resource)
}

// ─── mcpServerByName(name, version) ────────────────────────────────────────

func mcpServerByNameField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpServerType,
		Args: graphql.FieldConfigArgument{
			"name":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"version": &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.mcp_servers.get_by_name")
			defer span.End()

			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			name := p.Args["name"].(string)
			version := p.Args["version"].(string)

			scope, err := s.GetMCPScopeByName(ctx, actor.accountID, name, version)
			if err != nil {
				if errors.Is(err, store.ErrArtifactScopeNotFound) {
					// We return a strict error instead of null when an MCP is not found.
					// This maintains API consistency with the Registry's sdkByName behavior,
					// which explicitly errors so clients can cleanly catch the "not found" state
					// rather than handling a generic null object.
					if version != "" {
						return nil, fmt.Errorf("no MCP server found with name %s and version %s", name, version)
					}
					return nil, fmt.Errorf("no MCP server found with name %s", name)
				}
				return nil, fmt.Errorf("failed to fetch mcp server: %w", err)
			}
			return mcpServerFields(requestFromContext(ctx), *scope), nil
		},
	}
}

// mcpServerFields is the one place an store.ArtifactScope becomes the MCPServer
// GraphQL shape, shared by the list query and every mutation below that
// returns the server it just acted on -- one mapping, not five copies that
// could drift on which fields it exposes.
func mcpServerFields(r *http.Request, scope store.ArtifactScope) map[string]interface{} {
	return map[string]interface{}{
		"id":             scope.ArtifactID.String(),
		"name":           scope.Name,
		"version":        scope.Version,
		"config_key":     scope.ConfigKey,
		"mcp_url":        mcpURLForSDK(r, scope.ArtifactID),
		"active":         scope.DeactivatedAt == nil,
		"deactivated_at": formatOptionalTime(scope.DeactivatedAt),
		"created_at":     scope.CreatedAt.Format(mcpGraphQLTimeFormat),
	}
}

const mcpGraphQLTimeFormat = "2006-01-02T15:04:05Z07:00"

// formatOptionalTime returns "" for a nil timestamp (an active scope's
// DeactivatedAt) rather than a zero-time string, so the UI can treat an
// empty deactivated_at as "never deactivated" without parsing a sentinel
// date.
func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(mcpGraphQLTimeFormat)
}

// ─── mcpAnalytics(artifactId) ────────────────────────────────────────────────────

func mcpAnalyticsField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpAnalyticsDashboardType,
		Args: graphql.FieldConfigArgument{
			"artifactId": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			artifactID, err := uuid.Parse(p.Args["artifactId"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid artifactId")
			}
			if err := ensureArtifactScopeOwnedBy(p.Context, s, actor.accountID, artifactID); err != nil {
				return nil, err
			}
			dashboard, err := s.GetMCPAnalyticsDashboard(p.Context, artifactID)
			if err != nil {
				return nil, fmt.Errorf("load mcp analytics: %w", err)
			}
			return mcpAnalyticsDashboardFields(dashboard), nil
		},
	}
}

func mcpAnalyticsDashboardFields(d *models.MCPAnalyticsDashboard) map[string]interface{} {
	toolUsage := make([]map[string]interface{}, 0, len(d.ToolUsage))
	for _, u := range d.ToolUsage {
		toolUsage = append(toolUsage, map[string]interface{}{
			"tool_name": u.ToolName, "count": u.Count, "failed": u.Failed, "average_latency": u.AverageLatencyMs,
		})
	}
	serviceUsage := make([]map[string]interface{}, 0, len(d.ServiceUsage))
	for _, u := range d.ServiceUsage {
		serviceUsage = append(serviceUsage, map[string]interface{}{
			"service_name": u.ServiceName, "count": u.Count, "failed": u.Failed, "average_latency": u.AverageLatencyMs,
		})
	}
	sessions := make([]map[string]interface{}, 0, len(d.RecentSessions))
	for _, sess := range d.RecentSessions {
		endedAt := ""
		if sess.EndedAt != nil {
			endedAt = sess.EndedAt.Format(mcpGraphQLTimeFormat)
		}
		sessions = append(sessions, map[string]interface{}{
			"id": sess.ID.String(), "session_id": sess.SessionID,
			"started_at": sess.StartedAt.Format(mcpGraphQLTimeFormat), "ended_at": endedAt,
		})
	}
	return map[string]interface{}{
		"total_requests": d.TotalRequests, "failed_requests": d.FailedRequests,
		"average_latency": d.AverageLatencyMs, "active_agents": d.ActiveAgents,
		"tool_usage": toolUsage, "service_usage": serviceUsage, "recent_sessions": sessions,
	}
}

// ─── deployMcpServer / killMcpServer / reactivateMcpServer / deleteMcpServer ─

// deployMCPServerField accepts the complete declarative artifact and delegates
// to the same plan/apply services as CLI, avoiding a second selections-only
// creation contract.
func deployMCPServerField(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) *graphql.Field {
	return &graphql.Field{
		Type: mcpServerType,
		Args: graphql.FieldConfigArgument{
			"config":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(engineJSONType)},
			"owner_team": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(p.Args["config"])
			if err != nil {
				return nil, fmt.Errorf("invalid mcp config")
			}
			var doc sdkConfigDocument
			if err := decodeArtifactConfigJSON(raw, &doc); err != nil {
				return nil, fmt.Errorf("invalid mcp config")
			}
			if err := validateArtifactConfigDocument(doc, "mcp"); err != nil {
				return nil, err
			}
			ownerTeamSlug := strings.TrimSpace(graphQLArgString(p, "owner_team"))
			controlActor, ok := accesscontrol.ActorFromContext(p.Context)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			hash := sha256.Sum256(raw)
			configKey := fmt.Sprintf("mcp:%s:%s", doc.Name, doc.Version)
			request := requestFromContext(p.Context)
			apiKey := ""
			if request != nil {
				apiKey = request.Header.Get("X-API-Key")
			}
			planResult, err := createMCPConfigPlan(p.Context, configStore, s, registryClient, sdkPlanCall{
				apiKey: apiKey, accountID: actor.accountID, actor: controlActor,
				request: SDKConfigPlanRequest{ConfigKey: configKey, SourceHash: fmt.Sprintf("sha256:%x", hash), OwnerTeamSlug: ownerTeamSlug, Config: raw}, document: doc,
			})
			if err != nil {
				return nil, err
			}
			result, err := executeMCPConfigApply(p.Context, configStore, s, registryClient, sdkApplyCall{
				apiKey: apiKey, accountID: actor.accountID, actor: controlActor,
				planID: planResult.plan.ID, planRevision: planResult.plan.Revision, sourceHash: planResult.plan.SourceHash,
			})
			if err != nil {
				return nil, err
			}
			fields := mcpServerFields(request, result.Scope)
			fields["execution_token"] = result.ExecutionToken
			return fields, nil
		},
	}
}

func killMCPServerField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpServerType,
		Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			artifactID, err := uuid.Parse(p.Args["id"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid id")
			}
			if err := ensureArtifactScopeOwnedBy(p.Context, s, actor.accountID, artifactID); err != nil {
				return nil, err
			}
			if err := s.DeactivateSDK(p.Context, actor.accountID, artifactID); err != nil {
				return nil, sdkLifecycleStoreError(err, "failed to kill mcp server")
			}
			// See DeactivateSDKHandler's own comment: DeactivateSDK alone only
			// blocks *new* connections, live sessions must be force-killed here.
			sandbox.KillMCPSessionsForSDK(artifactID.String())
			scope, err := s.GetArtifactScope(p.Context, artifactID)
			if err != nil {
				return nil, fmt.Errorf("load killed mcp server: %w", err)
			}
			return mcpServerFields(requestFromContext(p.Context), *scope), nil
		},
	}
}

func reactivateMCPServerField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpServerType,
		Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			artifactID, err := uuid.Parse(p.Args["id"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid id")
			}
			if err := reactivateArtifactScope(p.Context, s, actor.accountID, artifactID); err != nil {
				return nil, err
			}
			scope, err := s.GetArtifactScope(p.Context, artifactID)
			if err != nil {
				return nil, fmt.Errorf("load reactivated mcp server: %w", err)
			}
			return mcpServerFields(requestFromContext(p.Context), *scope), nil
		},
	}
}

func deleteMCPServerField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			artifactID, err := uuid.Parse(p.Args["id"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid id")
			}
			if err := ensureArtifactScopeOwnedBy(p.Context, s, actor.accountID, artifactID); err != nil {
				return nil, err
			}
			if err := s.DeleteArtifactScope(p.Context, actor.accountID, artifactID); err != nil {
				return nil, fmt.Errorf("failed to delete mcp server: %w", err)
			}
			sandbox.KillMCPSessionsForSDK(artifactID.String())
			_ = sandbox.CleanupMCPSandboxDir(artifactID.String())
			return true, nil
		},
	}
}
