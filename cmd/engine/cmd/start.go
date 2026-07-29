package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"

	backend "github.com/Usefused/engine"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	enginemiddleware "github.com/Usefused/engine/internal/engine/middleware"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/worker"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/messaging"
	apimiddleware "github.com/Usefused/engine/internal/shared/middleware"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

var (
	port        string
	grpcPort    string
	webhookPort string
	uiURL       string
	licenseKey  string
	environment string
)

// healthResponse is the /health endpoint's JSON shape. Environment is a pure
// observability/UX label (Task 8) so the CLI can print a "you're talking to
// production" warning before a destructive workspace apply.
type healthResponse struct {
	Status      string `json:"status"`
	Plane       string `json:"plane"`
	Environment string `json:"environment"`
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Fused Engine",
	Run: func(cmd *cobra.Command, args []string) {
		// Purely an observability/UX label (Task 8) -- mirrors the
		// license-key precedent above, letting the shared observability
		// package (and anything else) just read FUSED_ENGINE_ENVIRONMENT
		// directly instead of threading a parameter through every Init
		// call. Only overrides the env var when --environment is
		// explicitly passed, so leaving it unset preserves whatever
		// FUSED_ENGINE_ENVIRONMENT (or the "production" default) already
		// resolves to.
		if cmd.Flags().Changed("environment") {
			os.Setenv(observability.EngineEnvironmentEnvVar, environment)
		}
		runEngine()
	},
}

func init() {
	RootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVar(&port, "port", "8081", "HTTP port for API and UI")
	startCmd.Flags().StringVar(&grpcPort, "grpc-port", "50051", "gRPC port for SDK connections")
	startCmd.Flags().StringVar(&webhookPort, "webhook-port", "", "Dedicated HTTP port for Webhook Ingress (optional)")
	startCmd.Flags().StringVar(&uiURL, "ui-url", "", "URL for the UI (overrides engine.yaml)")
	// Hide ui-url from help since it's mainly for local dev overrides
	startCmd.Flags().MarkHidden("ui-url")
	startCmd.Flags().StringVar(&licenseKey, "license-key", "", "License Key for Registry handshake")
	startCmd.Flags().StringVar(&environment, "environment", "", "Deployment environment label (e.g. production, staging) -- attached to OTel traces/logs/metrics and echoed on /health (overrides FUSED_ENGINE_ENVIRONMENT env var when set; defaults to \"production\")")
}

func runEngine() {
	loadEngineEnv()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// OTEL observability
	observability.Init(ctx)
	defer observability.Close()
	observability.InitMetrics(ctx)
	defer observability.CloseMetrics(ctx)

	cfg, database, natsClient := initDependencies(ctx)
	defer natsClient.Close()

	applyEngineOverrides(cfg)
	envLicense := requireRegistryLicense(ctx)

	// ─── Engine Bootstrap ───
	engineStore := store.NewCachedStore(store.NewPostgresStore(database), natsClient)
	registryClient := sandbox.NewHTTPRegistryClient(cfg.Engine.RegistryEndpoint, envLicense)

	engineWorkers := startEngineWorkers(ctx, engineStore, natsClient, cfg)

	entitlement := bootstrapRegistryIdentity(ctx, engineStore, registryClient, envLicense)
	engineWorkers.usageCounter = startEngineUsageCounter(ctx, engineStore, entitlement)
	startEngineHeartbeat(ctx, registryClient, entitlement)
	usageFlushWorker := startEngineUsageReporting(ctx, engineStore, registryClient, entitlement)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		usageFlushWorker.Stop(stopCtx)
	}()
	defer func() {
		// Drain user/agent-triggered local records before the usage reporter does
		// its own deferred Registry flush; otherwise the newest counters would wait
		// until the next Engine startup.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		engineWorkers.Stop(stopCtx)
	}()

	sandbox.StartMCPCleanupWorker(ctx, database)
	// configStore only needs database (already available), so it's created
	// here rather than down by MountConfigRoutes -- both the changelog
	// poller below and SDKWebSocketHandler further down need it.
	configStore := store.NewPostgresConfigRepository(database)
	// Phase 2 (capture) + Phase 3 (usage-index matching + notification) of
	// the service changelog system (plans/plan-service-changelog.md):
	// captures Registry-published changes into this Engine's own local
	// cache and, for each newly cached row, notifies this workspace only
	// when its own configuration actually uses what changed.
	sandbox.StartServiceChangelogPoller(ctx, engineStore, configStore, registryClient, envLicense)

	_ = sandbox.NewActivationManager(registryClient, engineStore)

	localObjectCache := sandbox.NewLocalObjectCache(engineStore, registryClient)
	subscribeCacheInvalidation(natsClient, localObjectCache)

	registryProxy := api.NewRegistryProxy(cfg.Engine.RegistryEndpoint)
	// localObjectCache, not registryClient directly, so rate_limit/retry_config
	// enforcement reads the cached runtime contract snapshot (falling back to
	// a live Registry call only when no snapshot exists yet) instead of
	// making a live Registry call on every single proxied request.
	runtimeEnforcer := enginemiddleware.NewRuntimeEnforcer(engineStore, localObjectCache)
	masterKey := loadMasterKey(ctx)

	r := buildEngineRouter(engineRouterDeps{
		cfg:              cfg,
		natsClient:       natsClient,
		engineStore:      engineStore,
		registryClient:   registryClient,
		registryProxy:    registryProxy,
		localObjectCache: localObjectCache,
		runtimeEnforcer:  runtimeEnforcer,
		configStore:      configStore,
		masterKey:        masterKey,
	})

	webhookSrv := startWebhookServer(ctx, r)
	srv := startEngineHTTPServer(ctx, r)
	grpcServer := startEngineGRPCServer(ctx, engineStore, registryClient, masterKey)

	waitForEngineShutdown(ctx, cancel, srv, webhookSrv, grpcServer)
}

func loadEngineEnv() {
	if err := godotenv.Load(".env"); err != nil {
		if err := godotenv.Load("../.env"); err != nil {
			slog.Warn("No .env file found, reading from environment")
		}
	}
}

func applyEngineOverrides(cfg *config.Config) {
	// Flags intentionally outrank engine.yaml so container launches can make
	// one-off operational overrides without mutating checked-in config.
	if uiURL != "" {
		cfg.UIURL = uiURL
	}
	if licenseKey != "" {
		os.Setenv("FUSED_LICENSE_KEY", licenseKey)
	}
}

func requireRegistryLicense(ctx context.Context) string {
	envLicense := os.Getenv("FUSED_LICENSE_KEY")
	if envLicense == "" {
		slog.ErrorContext(ctx, "FATAL: No FUSED_LICENSE_KEY provided. Booting in local mode has been removed.")
		os.Exit(1)
	}
	return envLicense
}

type engineWorkers struct {
	executionAudit *worker.ExecutionAuditWorker
	usageCounter   *worker.UsageCounterWorker
}

func (w engineWorkers) Stop(ctx context.Context) {
	if w.usageCounter != nil {
		w.usageCounter.Stop(ctx)
	}
	if w.executionAudit != nil {
		w.executionAudit.Stop(ctx)
	}
}

type runtimeEntitlementStore interface {
	SaveRuntimeEntitlement(ctx context.Context, entitlement models.RuntimeEntitlement) error
}

func startEngineWorkers(ctx context.Context, engineStore store.Store, natsClient *messaging.NATSClient, cfg *config.Config) engineWorkers {
	executionAuditWorker := worker.NewExecutionAuditWorker(engineStore, worker.ExecutionAuditOptions{})
	executionAuditWorker.Start(ctx)
	sandbox.SetExecutionAuditRecorder(executionAuditWorker)
	sandbox.SetIdempotencyStore(engineStore)
	// Webhook ingress resolves slugs against the Engine's own table so
	// delivery keeps working even when Registry is only used as the control
	// plane for catalogue data.
	sandbox.SetWebhookConfigStore(engineStore)
	worker.StartMCPAnalyticsWorker(ctx, engineStore, natsClient, cfg)
	worker.StartWebhookAnalyticsWorker(ctx, engineStore, natsClient)
	return engineWorkers{executionAudit: executionAuditWorker}
}

func startEngineUsageCounter(ctx context.Context, engineStore store.Store, entitlement models.RuntimeEntitlement) *worker.UsageCounterWorker {
	if entitlement.UsageReporting != models.RuntimeUsageReportingAggregate {
		// Recording is gated together with flushing so a non-aggregate Registry
		// entitlement does not quietly accumulate local accounting rows forever.
		sandbox.SetExecutionUsageRecorder(nil)
		slog.InfoContext(ctx, "Engine aggregate usage recording disabled by Registry entitlement", slog.String("mode", entitlement.UsageReporting))
		return nil
	}
	usageStore, ok := engineStore.(worker.RuntimeUsageCounterStore)
	if !ok {
		sandbox.SetExecutionUsageRecorder(nil)
		slog.WarnContext(ctx, "Engine usage counter store unavailable; aggregate usage reports disabled")
		return nil
	}
	usageCounterWorker := worker.NewUsageCounterWorker(usageStore, worker.UsageCounterOptions{})
	usageCounterWorker.Start(ctx)
	sandbox.SetExecutionUsageRecorder(usageCounterWorker)
	return usageCounterWorker
}

func bootstrapRegistryIdentity(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient, envLicense string) models.RuntimeEntitlement {
	slog.InfoContext(ctx, "FUSED_LICENSE_KEY present. Attempting Registry Handshake...")
	handshake, err := registryClient.HandshakeWithEntitlements(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to handshake with Registry using provided License Key", slog.Any("error", err))
		os.Exit(1)
	}
	accountIDStr, wsName := handshake.AccountID, handshake.WorkspaceName

	accUUID, err := uuid.Parse(accountIDStr)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Registry returned invalid account ID format", slog.Any("error", err))
		os.Exit(1)
	}

	// Registry owns account provisioning; Engine only mirrors the one
	// workspace it is licensed to serve, avoiding split-brain local accounts.
	_, err = engineStore.BootstrapWorkspace(ctx, accUUID, wsName)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to initialize singleton Engine workspace", slog.Any("error", err))
		os.Exit(1)
	}

	err = engineStore.BootstrapAPIKey(ctx, accUUID, envLicense)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to cache Registry-issued API key in local Postgres", slog.Any("error", err))
		os.Exit(1)
	}
	entitlement := handshake.Entitlements.Normalized()
	if entitlementStore, ok := engineStore.(runtimeEntitlementStore); ok {
		if err := entitlementStore.SaveRuntimeEntitlement(ctx, entitlement); err != nil {
			slog.WarnContext(ctx, "Failed to persist Registry entitlement bundle", slog.Any("error", err))
		}
	}
	slog.InfoContext(ctx, "Successfully initialized Engine workspace and API key",
		slog.String("account", accountIDStr),
		slog.String("workspace", wsName),
		slog.String("plan", entitlement.Plan),
		slog.String("usage_reporting", entitlement.UsageReporting),
	)
	return entitlement
}

func startEngineHeartbeat(ctx context.Context, registryClient *sandbox.HTTPRegistryClient, entitlement models.RuntimeEntitlement) {
	if !entitlement.HeartbeatRequired {
		slog.InfoContext(ctx, "Engine heartbeat disabled by Registry entitlement")
		return
	}
	interval := engineHeartbeatInterval(entitlement)
	go func() {
		// Send immediately after bootstrap so Registry can verify the runtime
		// during startup; the ticker then proves the Engine stayed alive.
		sendEngineHeartbeat(ctx, registryClient)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sendEngineHeartbeat(ctx, registryClient)
			}
		}
	}()
}

func startEngineUsageReporting(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient, entitlement models.RuntimeEntitlement) *worker.UsageReportFlushWorker {
	if entitlement.UsageReporting != models.RuntimeUsageReportingAggregate {
		slog.InfoContext(ctx, "Engine aggregate usage reporting disabled by Registry entitlement", slog.String("mode", entitlement.UsageReporting))
		return nil
	}
	reportStore, ok := engineStore.(worker.RuntimeUsageReportStore)
	if !ok {
		slog.WarnContext(ctx, "Engine usage report store unavailable; aggregate usage flushing disabled")
		return nil
	}
	flushWorker := worker.NewUsageReportFlushWorker(reportStore, registryClient, worker.UsageReportFlushOptions{
		Interval:        engineDurationFromEnv("FUSED_ENGINE_USAGE_FLUSH_INTERVAL", time.Minute),
		BatchLimit:      engineIntFromEnv("FUSED_ENGINE_USAGE_FLUSH_BATCH", 500),
		EngineVersion:   Version,
		EngineBuildHash: BuildHash,
	})
	flushWorker.Start(ctx)
	return flushWorker
}

func sendEngineHeartbeat(ctx context.Context, registryClient *sandbox.HTTPRegistryClient) {
	if err := registryClient.SendHeartbeat(ctx, Version, BuildHash, time.Now()); err != nil {
		slog.WarnContext(ctx, "Failed to send Engine heartbeat", slog.Any("error", err))
		return
	}
	slog.DebugContext(ctx, "Sent Engine heartbeat", slog.String("version", Version), slog.String("build_hash", BuildHash))
}

func engineHeartbeatInterval(entitlement models.RuntimeEntitlement) time.Duration {
	// Local env remains an operator break-glass override for unusual network
	// conditions, but the Registry contract is the normal source of truth.
	if os.Getenv("FUSED_ENGINE_HEARTBEAT_INTERVAL") != "" {
		return engineDurationFromEnv("FUSED_ENGINE_HEARTBEAT_INTERVAL", time.Minute)
	}
	return time.Duration(entitlement.Normalized().HeartbeatIntervalSeconds) * time.Second
}

func engineDurationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("Invalid duration env, using default", slog.String("env", name), slog.String("value", raw), slog.Any("error", err))
		return fallback
	}
	return parsed
}

func engineIntFromEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		slog.Warn("Invalid integer env, using default", slog.String("env", name), slog.String("value", raw), slog.Any("error", err))
		return fallback
	}
	return parsed
}

func subscribeCacheInvalidation(natsClient *messaging.NATSClient, localObjectCache *sandbox.LocalObjectCache) {
	if natsClient != nil && natsClient.Conn != nil {
		natsClient.Conn.Subscribe("engine.cache.invalidate.sdk_scope.>", func(m *nats.Msg) {
			parts := strings.Split(m.Subject, ".")
			if len(parts) == 5 {
				localObjectCache.InvalidateArtifactScope(parts[4])
			}
		})
	}
}

func loadMasterKey(ctx context.Context) []byte {
	masterKey, err := store.MasterKeyFromEnv()
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to load FUSED_ENCRYPTION_KEY", slog.Any("error", err))
		os.Exit(1)
	}
	return masterKey
}

type engineRouterDeps struct {
	cfg              *config.Config
	natsClient       *messaging.NATSClient
	engineStore      store.Store
	registryClient   *sandbox.HTTPRegistryClient
	registryProxy    api.Forwarder
	localObjectCache sandbox.ObjectCache
	runtimeEnforcer  *enginemiddleware.RuntimeEnforcer
	configStore      store.ConfigRepository
	masterKey        []byte
}

func buildEngineRouter(deps engineRouterDeps) chi.Router {
	// Router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(apimiddleware.CORS(deps.cfg.UIURL))
	// Embedded SPA assets are many small JS/CSS chunks; compressing here keeps
	// local Engine hosting close to what a CDN would normally do for the UI.
	// JSON stays uncompressed so proxied GraphQL does not add buffering while
	// we are diagnosing browser-side TTFB.
	r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "image/svg+xml"))

	// This middleware must run before proxy routes: paths such as /integrations
	// are both SPA routes and Registry API routes, so the request shape decides.
	uiFS := backend.GetUIFS()
	r.Use(api.EmbeddedUIMiddleware(uiFS))

	registerProxyRoutesWithRuntimeContracts(r, deps.registryProxy, deps.engineStore, deps.registryClient, deps.runtimeEnforcer)

	engineEnvironment := observability.EngineEnvironment()
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(deps.cfg.Engine.RegistryEndpoint)
		status := "ok"
		if err != nil || resp.StatusCode >= 500 {
			status = "degraded, serving from local copy"
		} else if resp != nil {
			resp.Body.Close()
		}
		// environment is a pure observability/UX label (Task 8) so the CLI
		// can warn "you're talking to production" before a destructive
		// workspace apply -- it has no bearing on health status itself.
		body, _ := json.Marshal(healthResponse{Status: status, Plane: "engine", Environment: engineEnvironment})
		w.Write(body)
	})

	tokenValidator := auth.NewTokenValidator(deps.engineStore)
	secretResolver := sandbox.NewSecretResolver(deps.engineStore, deps.masterKey)

	sandbox.InitSandbox(r, deps.natsClient, deps.cfg, deps.localObjectCache, tokenValidator, secretResolver, port)
	// configStore (constructed above, alongside the changelog poller) is
	// also needed here by SDKWebSocketHandler -- resolving a connecting
	// SDK/MCP's webhook_attachment label (see resolveWebhookAttachmentLabel
	// in websocket_handler.go) requires reading the same fused_config_states
	// table the config routes use.
	r.Get("/sdks/ws", api.SDKWebSocketHandler(deps.configStore, deps.engineStore, deps.natsClient, tokenValidator))
	r.Mount("/workspace", api.WorkspaceHandler(deps.engineStore, deps.registryClient, deps.masterKey))

	// Mount the config routes
	api.MountConfigRoutes(r, deps.configStore, deps.engineStore, deps.registryClient, deps.registryProxy, deps.registryClient, deps.masterKey)

	// Engine-native MCP GraphQL surface (list/deploy/kill/reactivate/delete +
	// analytics) -- a distinct endpoint from POST /graphql, which is a pure
	// Registry forward-proxy with no resolvers of its own (graphql_proxy.go).
	if err := api.MountMCPGraphQLRoute(r, deps.configStore, deps.engineStore, deps.registryClient, deps.registryClient, deps.masterKey); err != nil {
		slog.Error("failed to mount mcp graphql route", slog.Any("error", err))
		os.Exit(1)
	}

	// NotFound fallback for any path the router doesn't know about.
	// At this point browser navigations are already handled by the middleware
	// above, so this only serves unmatched API paths as 404.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	})
	return r
}

func startWebhookServer(ctx context.Context, r chi.Router) *http.Server {
	if webhookPort == "" || webhookPort == port {
		sandbox.InitWebhookRoutes(r)
		return nil
	}

	wr := chi.NewRouter()
	wr.Use(middleware.RequestID)
	wr.Use(middleware.RealIP)
	wr.Use(middleware.Recoverer)
	sandbox.InitWebhookRoutes(wr)

	webhookSrv := &http.Server{
		Addr:    ":" + webhookPort,
		Handler: wr,
	}
	go serveHTTPServer(ctx, webhookSrv, "Starting Dedicated Webhook Server", slog.String("port", webhookPort), "Webhook Server failed")
	return webhookSrv
}

func startEngineHTTPServer(ctx context.Context, r chi.Router) *http.Server {
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go serveHTTPServer(ctx, srv, "Engine Server starting", slog.String("addr", ":"+port), "Server failed")
	return srv
}

func serveHTTPServer(ctx context.Context, srv *http.Server, startMessage string, attr slog.Attr, errorMessage string) {
	slog.InfoContext(ctx, startMessage, attr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.ErrorContext(ctx, errorMessage, slog.Any("error", err))
		os.Exit(1)
	}
}

func startEngineGRPCServer(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient, masterKey []byte) *grpc.Server {
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to listen for gRPC", slog.Any("error", err))
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	enginev1.RegisterEngineServiceServer(grpcServer, api.NewEngineGRPCServer(engineStore, registryClient, masterKey))

	go serveGRPCServer(ctx, grpcServer, lis)
	return grpcServer
}

func serveGRPCServer(ctx context.Context, grpcServer *grpc.Server, lis net.Listener) {
	slog.InfoContext(ctx, "Starting Engine gRPC Server", slog.String("port", grpcPort))
	if err := grpcServer.Serve(lis); err != nil {
		slog.ErrorContext(ctx, "gRPC Server failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func waitForEngineShutdown(ctx context.Context, cancel context.CancelFunc, srv *http.Server, webhookSrv *http.Server, grpcServer *grpc.Server) {
	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.InfoContext(ctx, "Shutting down Engine...")

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(shutdownCtx, "Server forced to shutdown", slog.Any("error", err))
	}

	shutdownWebhookServer(shutdownCtx, webhookSrv)

	grpcServer.GracefulStop()
	slog.InfoContext(shutdownCtx, "Engine exited")
}

func shutdownWebhookServer(ctx context.Context, webhookSrv *http.Server) {
	if webhookSrv != nil {
		if err := webhookSrv.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "Webhook Server forced to shutdown", slog.Any("error", err))
		}
	}
}

func initDependencies(ctx context.Context) (*config.Config, *pgxpool.Pool, *messaging.NATSClient) {
	cfg, err := config.Load(ConfigPath)
	if err != nil {
		slog.WarnContext(ctx, "Failed to parse config file, using defaults", slog.Any("error", err))
	}

	databaseURL := os.Getenv("FUSED_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		databaseURL = cfg.Database.URL
	}

	database, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to connect to PostgreSQL", slog.Any("error", err))
		os.Exit(1)
	}

	natsClient, err := messaging.ConnectNATS()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to connect to NATS", slog.Any("error", err))
		os.Exit(1)
	}
	if err := natsClient.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
		slog.ErrorContext(ctx, "Failed to init WEBHOOKS stream", slog.Any("error", err))
		os.Exit(1)
	}
	if err := natsClient.InitStream(messaging.FusedEngineStream, messaging.FusedEngineStreamSubjects()); err != nil {
		slog.ErrorContext(ctx, "Failed to init FUSED_ENGINE_EVENTS stream", slog.Any("error", err))
		os.Exit(1)
	}
	slog.InfoContext(ctx, "Connected to NATS", slog.String("url", os.Getenv("NATS_URL")))

	return cfg, database, natsClient
}
