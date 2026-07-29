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

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"

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
)

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
}, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, registryClient sandbox.RegistryClient, masterKey []byte) error {
	schema, err := newMCPGraphQLSchema(configStore, s, verifier, registryClient, masterKey)
	if err != nil {
		return fmt.Errorf("build mcp graphql schema: %w", err)
	}
	mux.Post("/engine/graphql", mcpGraphQLHandler(schema, s))
	return nil
}

// mcpGraphQLHandler wraps graphql-go's own handler with one auth check per
// request, mirroring resolveWorkspaceActor's use everywhere else in this
// package.
func mcpGraphQLHandler(schema graphql.Schema, s store.Store) http.HandlerFunc {
	isDev := os.Getenv("FUSED_ENV") == "development"
	h := handler.New(&handler.Config{Schema: &schema, Pretty: isDev, GraphiQL: isDev})

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		authStart := time.Now()
		accountID, err := resolveWorkspaceActor(r.Context(), s, r)
		authDur := time.Since(authStart)
		if err != nil {
			setEngineGraphQLServerTiming(w.Header(), engineGraphQLTiming{auth: authDur, total: time.Since(start)})
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), mcpGraphQLActorContextKey, mcpGraphQLActor{accountID: accountID})
		ctx = context.WithValue(ctx, mcpGraphQLRequestKey, r)
		// One auth-config cache per HTTP request so workspaceServices and
		// workspaceServicePage (or any future field needing the same
		// per-service auth options) share a single batched Registry call
		// instead of one each -- see fetchWorkspaceServiceAuthOptions.
		ctx = context.WithValue(ctx, mcpGraphQLAuthConfigCacheKey{}, newAuthConfigCache())

		execStart := time.Now()
		bw := newBufferedResponseWriter()
		h.ServeHTTP(bw, r.WithContext(ctx))
		execDur := time.Since(execStart)
		setEngineGraphQLServerTiming(bw.Header(), engineGraphQLTiming{
			auth:    authDur,
			execute: execDur,
			total:   time.Since(start),
		})
		bw.flushTo(w)
	}
}

type engineGraphQLTiming struct {
	auth    time.Duration
	execute time.Duration
	total   time.Duration
}

func setEngineGraphQLServerTiming(header http.Header, timing engineGraphQLTiming) {
	parts := []string{serverTimingMetric("engine_auth", timing.auth)}
	if timing.execute > 0 {
		parts = append(parts, serverTimingMetric("engine_graphql", timing.execute))
	}
	parts = append(parts, serverTimingMetric("engine_total", timing.total))
	header.Set("Server-Timing", strings.Join(parts, ", "))
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
	query := graphql.NewObject(graphql.ObjectConfig{
		// The endpoint started with MCP operations, but it now owns broader
		// Engine reads like buckets and Connect auth. The root type name appears
		// in GraphQL validation errors, so keep it aligned with that surface.
		Name: "EngineQuery",
		Fields: graphql.Fields{
			"workspaceConnectionProfile": workspaceConnectionProfileGraphQLField(s),
			"workspaceConnectConfigs":    workspaceConnectConfigsGraphQLField(s),
			"mcpServers":                 mcpServersField(s),
			"mcpServerByName":            mcpServerByNameField(s),
			"mcpAnalytics":               mcpAnalyticsField(s),
			"bucketSummaries":            bucketSummariesGraphQLField(s),
			"bucketSummary":              bucketSummaryGraphQLField(s),
			"bucketSummaryPage":          bucketSummaryPageGraphQLField(s),
			"bucketConnectSummary":       bucketConnectSummaryGraphQLField(s),
			"workspaceServices":          workspaceServicesGraphQLField(s, verifier),
			"workspaceServicePage":       workspaceServicePageGraphQLField(s, verifier),
			"workspaceWebhooks":          workspaceWebhooksGraphQLField(s),
			"webhookEvents":              webhookEventsGraphQLField(s),
			"webhookAnalytics":           webhookAnalyticsGraphQLField(s),
			"workspaceNotifications":     workspaceNotificationsGraphQLField(configStore, s, registryClient),
			"sdkTokens":                  sdkTokensGraphQLField(s),
			"sdkBuckets":                 sdkBucketsGraphQLField(s),
			"bucketSDKPage":              bucketSDKPageGraphQLField(s),
			"bucketServicePage":          bucketServicePageGraphQLField(s),
			"bucketValues":               bucketValuesGraphQLField(s),
			"bucketValuePage":            bucketValuePageGraphQLField(s),
			"secretMetas":                secretMetasGraphQLField(s),
			"secretMetaPage":             secretMetaPageGraphQLField(s),
			"authConnections":            authConnectionsGraphQLField(s),
			"authConnectionPage":         authConnectionPageGraphQLField(s),
			"connectionResources":        connectionResourcesGraphQLField(s),
		},
	})
	mutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "EngineMutation",
		Fields: graphql.Fields{
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
	return graphql.NewSchema(graphql.SchemaConfig{Query: query, Mutation: mutation})
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
			scopes, total, err := s.ListMCPScopesByAccount(p.Context, actor.accountID, limit, offset)
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
			"config": &graphql.ArgumentConfig{Type: graphql.NewNonNull(engineJSONType)},
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
			hash := sha256.Sum256(raw)
			configKey := fmt.Sprintf("mcp:%s:%s", doc.Name, doc.Version)
			request := requestFromContext(p.Context)
			apiKey := ""
			if request != nil {
				apiKey = request.Header.Get("X-API-Key")
			}
			planResult, err := createMCPConfigPlan(p.Context, configStore, s, registryClient, sdkPlanCall{
				apiKey: apiKey, accountID: actor.accountID,
				request: SDKConfigPlanRequest{ConfigKey: configKey, SourceHash: fmt.Sprintf("sha256:%x", hash), Config: raw}, document: doc,
			})
			if err != nil {
				return nil, err
			}
			result, err := executeMCPConfigApply(p.Context, configStore, s, registryClient, sdkApplyCall{
				apiKey: apiKey, accountID: actor.accountID,
				planID: planResult.plan.ID, sourceHash: planResult.plan.SourceHash,
			})
			if err != nil {
				return nil, err
			}
			scope, err := s.GetArtifactScope(p.Context, result.RuntimeID)
			if err != nil {
				return nil, fmt.Errorf("load deployed mcp server: %w", err)
			}
			fields := mcpServerFields(request, *scope)
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
