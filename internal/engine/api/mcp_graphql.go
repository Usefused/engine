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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/applifecycle"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// This file is the Engine-native GraphQL surface for MCP server management:
// list, deploy, shared app lifecycle, and analytics. It
// exists as its own schema/endpoint (mounted at POST /engine/graphql, see
// MountMCPGraphQLRoute) rather than folded into the existing POST /graphql,
// which is a pure Registry forward-proxy (graphql_proxy.go) with no
// resolvers of its own -- adding Engine-native fields there would mean
// teaching that proxy to selectively intercept certain operations instead of
// forwarding everything, a much larger and riskier change than a second
// endpoint.
//
// Lifecycle resolvers delegate to applifecycle.Service so SDK and MCP versions
// cannot drift on deprecation or irreversible deactivation semantics.

// mcpGraphQLContextKey prevents the authenticated actor, inbound request, and
// revision sink from colliding with context values owned by other boundaries.
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
}, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, masterKey []byte, redirectURI string, revisionSinks ...authorizationRevisionSink) error {
	schema, err := newMCPGraphQLSchema(configStore, s, verifier, registryClient, masterKey, redirectURI)
	// Do not mount a route whose authorization policy schema could not be constructed.
	if err != nil {
		return fmt.Errorf("build mcp graphql schema: %w", err)
	}
	// GraphQL preflight shares the canonical service-reference capability with workspace and webhook flows.
	slugResolver, _ := registryClient.(ServiceSlugResolver)
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
		// Missing actors use the common envelope before parsing any GraphQL document.
		if !ok {
			setEngineGraphQLServerTiming(w.Header(), engineGraphQLTiming{auth: authDur, total: time.Since(start)})
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired, r.Context())
			return
		}

		authorizationStart := time.Now()
		plan, err := authorizeEngineGraphQL(r, &schema, actor, resources, isDev)
		// Publish the exact resolved authorization plan to an outer audit
		// middleware even when authorization denies the operation.
		accesscontrol.CaptureRequiredPermissions(r.Context(), plan.requirements)
		accesscontrol.CaptureMissingPermissions(r.Context(), accesscontrol.MissingRequirements(err))
		authorizationDur := time.Since(authorizationStart)
		// Parse and policy denials must remain HTTP-level structured failures.
		if err != nil {
			setEngineGraphQLServerTiming(w.Header(), engineGraphQLTiming{auth: authDur, authorize: authorizationDur, total: time.Since(start)})
			writeEngineGraphQLAuthorizationError(w, r.Context(), err)
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
		err = resolveDynamicGraphQLResources(ctx, &plan, resources, actor.AccountID, actor.WorkspaceID, r.Header.Get("X-API-Key"))
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

func resolveDynamicGraphQLResources(ctx context.Context, plan *graphQLAuthorizationPlan, resources graphQLAuthorizationResources, accountID, workspaceID uuid.UUID, apiKey string) error {
	deploymentRequirements, err := resources.resolveDeployments(ctx, workspaceID, plan.deployments, apiKey)
	if err != nil {
		return err
	}
	deploymentRequirements, err = resources.resolveAppRequirements(ctx, accountID, deploymentRequirements)
	if err != nil {
		return err
	}
	connectionRequirements, connections, err := resources.resolveConnections(ctx, plan.connections)
	if err != nil {
		return err
	}
	appRequirements, err := resources.resolveApps(ctx, accountID, plan.apps)
	if err != nil {
		return err
	}
	plan.mergeRequirements(deploymentRequirements)
	plan.mergeRequirements(connectionRequirements)
	plan.mergeRequirements(appRequirements)
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

// writeEngineGraphQLAuthorizationError separates malformed documents from
// authenticated policy denials while retaining the common Engine envelope.
func writeEngineGraphQLAuthorizationError(w http.ResponseWriter, ctx context.Context, err error) {
	// Malformed GraphQL cannot be classified as an authorization denial.
	if errors.Is(err, errInvalidGraphQLRequest) {
		writeControlAPIError(w, ctx, http.StatusBadRequest, "invalid_graphql_request", "The GraphQL request is invalid.", "Correct the GraphQL document and retry.")
		return
	}
	accesscontrol.WriteAuthorizationError(w, err, ctx)
}

// ─── Types ──────────────────────────────────────────────────────────────────

var mcpTransportURLsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPTransportURLs",
	Fields: graphql.Fields{
		"streamable_http": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"sse":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var mcpServerType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPServer",
	Fields: graphql.Fields{
		"id":                &graphql.Field{Type: graphql.String},
		"name":              &graphql.Field{Type: graphql.String},
		"version":           &graphql.Field{Type: graphql.String},
		"config_key":        &graphql.Field{Type: graphql.String},
		"default_transport": &graphql.Field{Type: graphql.String},
		"transport_urls":    &graphql.Field{Type: mcpTransportURLsGraphQLType},
		"execution_token":   &graphql.Field{Type: graphql.String},
		"active":            &graphql.Field{Type: graphql.Boolean},
		"deactivated_at":    &graphql.Field{Type: graphql.String},
		"created_at":        &graphql.Field{Type: graphql.String},
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
		"id":                &graphql.Field{Type: graphql.String},
		"app_token_id":      &graphql.Field{Type: graphql.String},
		"session_id":        &graphql.Field{Type: graphql.String},
		"protocol_version":  &graphql.Field{Type: graphql.String},
		"started_at":        &graphql.Field{Type: graphql.String},
		"last_activity_at":  &graphql.Field{Type: graphql.String},
		"ended_at":          &graphql.Field{Type: graphql.String},
		"end_reason":        &graphql.Field{Type: graphql.String},
		"client_name":       &graphql.Field{Type: graphql.String},
		"client_version":    &graphql.Field{Type: graphql.String},
		"initial_client_ip": &graphql.Field{Type: graphql.String},
	},
})

var mcpTokenActivityType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPTokenActivity",
	Fields: graphql.Fields{
		"id":                   &graphql.Field{Type: graphql.String},
		"name":                 &graphql.Field{Type: graphql.String},
		"binding_mode":         &graphql.Field{Type: graphql.String},
		"status":               &graphql.Field{Type: graphql.String},
		"issued_by_subject_id": &graphql.Field{Type: graphql.String},
		"execution_count":      &graphql.Field{Type: graphql.Float},
		"session_count":        &graphql.Field{Type: graphql.Float},
		"created_at":           &graphql.Field{Type: graphql.String},
		"last_used_at":         &graphql.Field{Type: graphql.String},
		"terminated_at":        &graphql.Field{Type: graphql.String},
		"termination_reason":   &graphql.Field{Type: graphql.String},
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
		"token_activity":  &graphql.Field{Type: graphql.NewList(mcpTokenActivityType)},
	},
})

// newMCPGraphQLSchema keeps session history and execution Activity behind the shared Engine authorization surface.
func newMCPGraphQLSchema(configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, masterKey []byte, redirectURIs ...string) (graphql.Schema, error) {
	publicInsightReader := newPublicInsightReader(registryClient)
	packageDownloads, _ := registryClient.(sandbox.SDKPackageDownloadCountClient)
	query := graphql.NewObject(graphql.ObjectConfig{
		// The endpoint started with MCP operations, but it now owns broader
		// Engine reads like buckets and Connect auth. The root type name appears
		// in GraphQL validation errors, so keep it aligned with that surface.
		Name: "EngineQuery",
		Fields: graphql.Fields{
			"appScaffoldRequirements":     appScaffoldRequirementsGraphQLField(s),
			"currentActorAccess":          currentActorAccessGraphQLField(),
			"app":                         appGraphQLField(s, packageDownloads),
			"apps":                        appsGraphQLField(s, packageDownloads),
			"appVersions":                 appVersionsGraphQLField(s, packageDownloads),
			"appServices":                 appServicesGraphQLField(s),
			"accessExplanation":           accessExplanationGraphQLField(s),
			"auditEvents":                 auditEventsGraphQLField(s),
			"appBuildSelectors":           appBuildSelectorsGraphQLField(s),
			"appOwningTeams":              appOwningTeamsGraphQLField(s),
			"users":                       usersGraphQLField(s),
			"user":                        userGraphQLField(s),
			"userEffectiveAccess":         userEffectiveAccessGraphQLField(s),
			"teamMembers":                 teamMembersGraphQLField(s),
			"teams":                       teamsGraphQLField(s),
			"team":                        teamGraphQLField(s),
			"workspaceShares":             workspaceSharesGraphQLField(s),
			"bucketReference":             bucketReferenceGraphQLField(s),
			"serviceReference":            serviceReferenceGraphQLField(s),
			"appReference":                appReferenceGraphQLField(s),
			"appFamilyReference":          appFamilyReferenceGraphQLField(s),
			"workspaceConnectionProfile":  workspaceConnectionProfileGraphQLField(s),
			"workspaceConnectionProfiles": workspaceConnectionProfilesGraphQLField(s),
			"mcpServers":                  mcpServersField(s),
			"mcpServerByName":             mcpServerByNameField(s),
			"mcpAnalytics":                mcpAnalyticsField(s),
			"mcpSessions":                 mcpSessionsField(s),
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
			"appExecutionEvents":          appExecutionEventsGraphQLField(s),
			"appExecutionAnalytics":       appExecutionAnalyticsGraphQLField(s),
			"engineExecutionAnalytics":    engineExecutionAnalyticsGraphQLField(s),
			"workspaceExecutionAnalytics": workspaceExecutionAnalyticsGraphQLField(s),
			"publicServiceInsights":       publicServiceInsightsGraphQLField(s, publicInsightReader),
			"serviceConsumers":            serviceConsumersGraphQLField(s),
			"workspaceNotifications":      workspaceNotificationsGraphQLField(configStore, s, registryClient),
			"appTokens":                   appTokensGraphQLField(s),
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
			"grantTeamAppAccess":                grantTeamAppAccessGraphQLField(s),
			"revokeTeamAppAccess":               revokeTeamAppAccessGraphQLField(s),
			"grantWorkspaceBucketAccess":        grantWorkspaceBucketAccessGraphQLField(s),
			"revokeWorkspaceBucketAccess":       revokeWorkspaceBucketAccessGraphQLField(s),
			"grantWorkspaceAppAccess":           grantWorkspaceAppAccessGraphQLField(s),
			"revokeWorkspaceAppAccess":          revokeWorkspaceAppAccessGraphQLField(s),
			"setWorkspaceConnectionProfile":     setWorkspaceConnectionProfileGraphQLField(s, verifier, registryClient),
			"resetWorkspaceConnectionProfile":   resetWorkspaceConnectionProfileGraphQLField(s),
			"updateWorkspaceNotificationStatus": updateWorkspaceNotificationStatusGraphQLField(configStore),
			"deployMcpServer":                   deployMCPServerField(configStore, s, registryClient),
			"deprecateApp":                      deprecateAppGraphQLField(s),
			"undeprecateApp":                    undeprecateAppGraphQLField(s),
			"deactivateApp":                     deactivateAppGraphQLField(s),
			"upsertSecrets":                     upsertSecretsGraphQLField(s, masterKey),
			"deleteSecrets":                     deleteSecretsGraphQLField(s),
			"startConnectSession":               startConnectSessionGraphQLField(s, verifier, masterKey, firstRedirectURI(redirectURIs)),
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
			authorized, err := graphQLAuthorizedScope(p.Context, accesscontrol.PermissionAppRead, accesscontrol.ResourceApp)
			if err != nil {
				return nil, err
			}
			scopes, total, err := s.ListAuthorizedMCPAppsByAccount(p.Context, actor.accountID, authorized, limit, offset)
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

			scope, err := s.GetMCPAppByName(ctx, actor.accountID, name, version)
			if err != nil {
				if errors.Is(err, store.ErrAppRuntimeNotFound) {
					// A strict not-found error lets clients distinguish a missing local app
					// version from an empty optional field without consulting Registry state.
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

// mcpServerFields is the one place an store.AppRuntime becomes the MCPServer
// GraphQL shape, shared by the list query and every mutation below that
// returns the server it just acted on -- one mapping, not five copies that
// could drift on which fields it exposes.
func mcpServerFields(r *http.Request, scope store.AppRuntime) map[string]interface{} {
	active := scope.Status == "" || scope.Status.Runnable()
	transportURLs := mcpTransportURLsForApp(r, scope.AppID)
	return map[string]interface{}{
		"id":                scope.AppID.String(),
		"name":              scope.Name,
		"version":           scope.Version,
		"config_key":        scope.ConfigKey,
		"default_transport": mcpDefaultTransport,
		"transport_urls":    mcpTransportURLsGraphQLValue(transportURLs),
		"active":            active,
		"deactivated_at":    "",
		"created_at":        scope.CreatedAt.Format(mcpGraphQLTimeFormat),
	}
}

const mcpGraphQLTimeFormat = "2006-01-02T15:04:05Z07:00"

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(mcpGraphQLTimeFormat)
}

func optionalGraphQLUUIDValue(value uuid.UUID) interface{} {
	if value == uuid.Nil {
		return nil
	}
	return value.String()
}

// ─── mcpAnalytics(app_id) ────────────────────────────────────────────────────

func mcpAnalyticsField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpAnalyticsDashboardType,
		Args: graphql.FieldConfigArgument{
			"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			appID, err := uuid.Parse(p.Args["app_id"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid app_id")
			}
			app, err := appOwnedBy(p.Context, s, actor.accountID, appID)
			if err != nil {
				return nil, err
			}
			dashboard, err := s.GetMCPAnalyticsDashboard(p.Context, appID)
			if err != nil {
				return nil, fmt.Errorf("load mcp analytics: %w", err)
			}
			tokens, err := s.ListAppTokens(p.Context, app.AppFamilyID)
			if err != nil {
				return nil, fmt.Errorf("load mcp token activity: %w", err)
			}
			return mcpAnalyticsDashboardFields(dashboard, tokens), nil
		},
	}
}

// mcpAnalyticsDashboardFields shares the safe session projection with the paginated history endpoint.
func mcpAnalyticsDashboardFields(d *models.MCPAnalyticsDashboard, tokens []store.AppTokenMetadata) map[string]interface{} {
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
	sessions := mcpSessionHistoryFields(d.RecentSessions)
	return map[string]interface{}{
		"total_requests": d.TotalRequests, "failed_requests": d.FailedRequests,
		"average_latency": d.AverageLatencyMs, "active_agents": d.ActiveAgents,
		"tool_usage": toolUsage, "service_usage": serviceUsage, "recent_sessions": sessions,
		"token_activity": mcpTokenActivityFields(tokens),
	}
}

func mcpTokenActivityFields(tokens []store.AppTokenMetadata) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(tokens))
	for _, token := range tokens {
		// Activity intentionally omits raw allowlists and credential IDs. Audit
		// readers need lifecycle/use evidence, not another token-management API.
		items = append(items, map[string]interface{}{
			"id": token.ID.String(), "name": token.Name,
			"binding_mode": string(token.BindingMode), "status": string(token.Status),
			"issued_by_subject_id": formatOptionalGraphQLUUID(token.IssuedBySubjectID),
			"execution_count":      float64(token.ExecutionCount), "session_count": float64(token.SessionCount),
			"created_at": formatGraphQLTime(token.CreatedAt), "last_used_at": formatOptionalGraphQLTime(token.LastUsedAt),
			"terminated_at": formatOptionalGraphQLTime(token.TerminatedAt), "termination_reason": token.TerminationReason,
		})
	}
	return items
}

// ─── deployMcpServer and shared app lifecycle ────────────────────────────────

// deployMCPServerField accepts the complete declarative app config and delegates
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
			if err := decodeAppConfigJSON(raw, &doc); err != nil {
				return nil, fmt.Errorf("invalid mcp config")
			}
			if err := validateAppConfigDocument(doc, "mcp"); err != nil {
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

func deprecateAppGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{
			"app_id":                  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"message":                 &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"planned_deactivation_at": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, app, err := appLifecycleGraphQLTarget(p, s)
			if err != nil {
				return nil, err
			}
			message := strings.TrimSpace(p.Args["message"].(string))
			if message == "" {
				return nil, errors.New("message is required")
			}
			plannedAt, err := optionalGraphQLTime(p.Args["planned_deactivation_at"])
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app.deprecate")
			defer span.End()
			span.SetAttributes(attribute.String("actor.type", string(actor.Kind)), attribute.String("app.id", app.AppID.String()))
			if err := applifecycle.New(s).Deprecate(ctx, app.AppID, message, plannedAt); err != nil {
				recordAppLifecycleGraphQLError(span, err)
				return nil, fmt.Errorf("deprecate app: %w", err)
			}
			span.SetAttributes(attribute.String("outcome", "deprecated"))
			return true, nil
		},
	}
}

func undeprecateAppGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, app, err := appLifecycleGraphQLTarget(p, s)
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app.undeprecate")
			defer span.End()
			span.SetAttributes(attribute.String("actor.type", string(actor.Kind)), attribute.String("app.id", app.AppID.String()))
			if err := applifecycle.New(s).Undeprecate(ctx, app.AppID); err != nil {
				recordAppLifecycleGraphQLError(span, err)
				return nil, fmt.Errorf("undeprecate app: %w", err)
			}
			span.SetAttributes(attribute.String("outcome", "active"))
			return true, nil
		},
	}
}

func deactivateAppGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, app, err := appLifecycleGraphQLTarget(p, s)
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app.deactivate")
			defer span.End()
			span.SetAttributes(attribute.String("actor.type", string(actor.Kind)), attribute.String("app.id", app.AppID.String()))
			if err := applifecycle.New(s).Deactivate(ctx, app.AppID, actor.SubjectID); err != nil {
				recordAppLifecycleGraphQLError(span, err)
				return nil, fmt.Errorf("deactivate app: %w", err)
			}
			if app.GeneratorVersion == "" {
				sandbox.TerminateMCPSessionsForApp(app.AppID.String())
			}
			span.SetAttributes(attribute.String("outcome", "deactivated"))
			return true, nil
		},
	}
}

func recordAppLifecycleGraphQLError(span trace.Span, err error) {
	span.SetAttributes(attribute.String("outcome", "failed"))
	span.RecordError(err)
	span.SetStatus(codes.Error, "app lifecycle mutation failed")
}

func appLifecycleGraphQLTarget(p graphql.ResolveParams, s store.Store) (accesscontrol.Actor, *store.App, error) {
	actor, ok := accesscontrol.ActorFromContext(p.Context)
	if !ok {
		return accesscontrol.Actor{}, nil, accesscontrol.ErrAuthenticationRequired
	}
	appID, err := uuid.Parse(p.Args["app_id"].(string))
	if err != nil {
		return accesscontrol.Actor{}, nil, errors.New("invalid app_id")
	}
	app, err := appOwnedBy(p.Context, s, actor.AccountID, appID)
	return actor, app, err
}

func optionalGraphQLTime(value interface{}) (*time.Time, error) {
	text, _ := value.(string)
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil, errors.New("planned_deactivation_at must be RFC3339")
	}
	return &parsed, nil
}
