package cmd

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/apptokeninvalidation"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/cliauth"
	entitlementpkg "github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/managedauth"
	enginemiddleware "github.com/Usefused/engine/internal/engine/middleware"
	"github.com/Usefused/engine/internal/engine/ratelimitcoordinator"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/worker"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
)

var (
	port        string
	grpcHost    string
	grpcPort    string
	webhookPort string
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

// init registers only runtime controls owned by Engine; UI origin selection is
// absent because the bundled UI is served from this same HTTP listener.
func init() {
	RootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVar(&port, "port", "8081", "HTTP port for API and UI")
	startCmd.Flags().StringVar(&grpcHost, "grpc-host", "127.0.0.1", "gRPC listen host")
	startCmd.Flags().StringVar(&grpcPort, "grpc-port", "50051", "gRPC port for SDK connections")
	startCmd.Flags().StringVar(&webhookPort, "webhook-port", "", "Dedicated HTTP port for Webhook Ingress (optional)")
	startCmd.Flags().StringVar(&licenseKey, "license-key", "", "License Key for Registry handshake")
	startCmd.Flags().StringVar(&environment, "environment", "", "Deployment environment label (e.g. production, staging) -- attached to OTel traces/logs/metrics and echoed on /health (overrides FUSED_ENGINE_ENVIRONMENT env var when set; defaults to \"production\")")
}

func runEngine() {
	licenseSources := loadEngineEnv()
	licenseSources.Flag = licenseKey

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// OTEL observability
	observability.Init(ctx)
	defer observability.Close()
	observability.InitMetrics(ctx)
	defer observability.CloseMetrics(ctx)

	cfg, database, natsClient, rateLimitKV := initDependencies(ctx, config.WithEngineLicenseSources(licenseSources))
	defer natsClient.Close()

	applyEngineOverrides(cfg)
	recordEngineLicenseResolution(ctx, cfg.Engine.LicenseKeySource)
	envLicense := requireRegistryLicense(ctx)

	// ─── Engine Bootstrap ───
	postgresStore := store.NewPostgresStore(database)
	engineStore := store.NewCachedStore(postgresStore, natsClient)
	tokenValidator := auth.NewTokenValidator(engineStore)
	tokenRevoker, err := apptokeninvalidation.NewService(
		engineStore, tokenValidator, apptokeninvalidation.NewPublisher(natsClient),
	)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to initialize app-token invalidation", slog.String("error_code", "app_token_invalidation_init_failed"))
		os.Exit(1)
	}
	rateLimits, err := newProviderRateLimitCoordinator(rateLimitKV, postgresStore)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to initialize provider rate-limit coordination", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() { _ = rateLimits.Close() }()
	registryClient := sandbox.NewHTTPRegistryClient(cfg.Engine.RegistryEndpoint, envLicense)
	if err := configureRegistryEngineIdentity(ctx, engineStore, registryClient); err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to configure Engine installation identity", slog.Any("error", err))
		os.Exit(1)
	}

	entitlement, authorizationRevision := bootstrapRegistryIdentity(ctx, engineStore, registryClient, envLicense)
	// Load the persisted entitlement into the in-memory cache so runtime limit checks
	// (sandbox concurrency, bucket creation, service limits) read from memory instead
	// of the database on every request.
	var entitlementStore runtimeEntitlementStore
	if ets, ok := engineStore.(runtimeEntitlementStore); ok {
		entitlementStore = ets
		dbEnt, err := ets.GetRuntimeEntitlement(ctx)
		if err == nil {
			entitlement = dbEnt
			entitlementpkg.LiveEntitlement.Store(dbEnt)
		} else {
			slog.WarnContext(ctx, "Failed to load persisted entitlement into memory cache; using handshake value", slog.Any("error", err))
			entitlementpkg.LiveEntitlement.Store(entitlement)
		}
	} else {
		entitlementpkg.LiveEntitlement.Store(entitlement)
	}
	backgroundStore := newSerializedBackgroundStore(engineStore)
	engineWorkers := startEngineWorkers(ctx, engineStore, natsClient, cfg.Engine, tokenValidator)
	engineWorkers.providerRateLimits = startProviderRateLimitProjection(ctx, postgresStore, natsClient)
	engineWorkers.packageLeases = startSDKPackageLeaseRenewal(ctx, backgroundStore, registryClient)
	engineWorkers.publicInsights = startPublicServiceInsightReporting(ctx, backgroundStore, registryClient)
	controlAuthenticator := newControlAuthenticator(ctx, engineStore, authorizationRevision)
	startAuthorizationRevisionPolling(ctx, backgroundStore, controlAuthenticator)
	engineWorkers.usageCounter = startEngineUsageCounter(ctx, engineStore, entitlement)

	startEngineHeartbeat(ctx, registryClient, entitlement, entitlementStore)
	usageFlushWorker := startEngineUsageReporting(ctx, backgroundStore, registryClient, entitlement)
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

	// configStore only needs the database, so create it once for both the
	// changelog poller and the gRPC webhook subscription path.
	configStore := store.NewPostgresConfigRepository(database)
	// Start the service changelog poller: periodically fetches published service
	// changelogs from the Registry, caches them locally, and emits workspace-scoped
	// notifications only when the workspace's own configuration references a changed
	// service or endpoint.
	sandbox.StartServiceChangelogPoller(ctx, engineStore, configStore, registryClient, envLicense)

	localObjectCache := sandbox.NewLocalObjectCache(engineStore)
	subscribeCacheInvalidation(natsClient, localObjectCache)

	registryProxy := api.NewRegistryProxy(cfg.Engine.RegistryEndpoint, envLicense)
	masterKey := loadMasterKey(ctx)
	browserCookies, browserSessionService := newBrowserSessionService(ctx, engineStore, registryClient, controlAuthenticator, masterKey)
	managedLoginService := newManagedLoginService(ctx, engineStore, registryClient, controlAuthenticator, masterKey)
	cliLoginService := newCLILoginService(ctx, engineStore, controlAuthenticator)

	r := buildEngineRouter(engineRouterDeps{
		cfg:                cfg,
		natsClient:         natsClient,
		engineStore:        engineStore,
		registryClient:     registryClient,
		registryProxy:      registryProxy,
		localObjectCache:   localObjectCache,
		configStore:        configStore,
		masterKey:          masterKey,
		controlAuth:        controlAuthenticator,
		managedLogin:       managedLoginService,
		cliLogin:           cliLoginService,
		browserSession:     browserSessionService,
		browserCookies:     browserCookies,
		providerRateLimits: rateLimits,
		tokenValidator:     tokenValidator,
		appTokenRevoker:    tokenRevoker,
	})

	webhookSrv := startWebhookServer(ctx, r)
	srv := startEngineHTTPServer(ctx, r)
	grpcServer := startEngineGRPCServer(ctx, engineStore, registryClient, masterKey, configStore, natsClient, tokenValidator)

	waitForEngineShutdown(ctx, cancel, srv, webhookSrv, grpcServer)
}

func newProviderRateLimitCoordinator(kv nats.KeyValue, repository store.Store) (*ratelimitcoordinator.Coordinator, error) {
	recovery, ok := repository.(ratelimitcoordinator.RecoveryStore)
	if !ok {
		return nil, errors.New("provider rate-limit recovery store is unavailable")
	}
	return ratelimitcoordinator.New(kv, recovery)
}

func configureRegistryEngineIdentity(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.registry_identity.configure")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "engine"))

	identityStore, ok := engineStore.(store.EngineInstallationStore)
	if !ok {
		span.SetAttributes(attribute.String("outcome", "unsupported"))
		return errors.New("Engine store does not support installation identity")
	}
	installationID, err := identityStore.LoadEngineInstallationID(ctx)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "load_failed"))
		return err
	}
	runtimeInstanceID := uuid.New()
	if err := registryClient.ConfigureEngineIdentity(installationID, runtimeInstanceID); err != nil {
		span.SetAttributes(attribute.String("outcome", "configure_failed"))
		return err
	}
	span.SetAttributes(
		attribute.String("engine.installation_id", installationID.String()),
		attribute.String("engine.runtime_instance_id", runtimeInstanceID.String()),
		attribute.String("outcome", "configured"),
	)
	return nil
}

func loadEngineEnv() config.EngineLicenseSources {
	return loadEngineEnvFiles([]string{".env", "../.env"})
}

func loadEngineEnvFiles(paths []string) config.EngineLicenseSources {
	sources := config.EngineLicenseSources{Environment: os.Getenv("FUSED_LICENSE_KEY")}
	for _, path := range paths {
		values, err := godotenv.Read(path)
		if err != nil {
			continue
		}
		if err := godotenv.Load(path); err != nil {
			slog.Warn("Failed to load Engine environment file", slog.String("path", path), slog.Any("error", err))
			continue
		}
		sources.DotEnv = values["FUSED_LICENSE_KEY"]
		return sources
	}
	slog.Warn("No .env file found, reading from configuration and environment")
	return sources
}

// applyEngineOverrides projects resolved secrets into dependencies that still
// read process configuration, without creating a second UI configuration path.
func applyEngineOverrides(cfg *config.Config) {
	if cfg.Engine.LicenseKey == "" {
		_ = os.Unsetenv("FUSED_LICENSE_KEY")
	} else {
		_ = os.Setenv("FUSED_LICENSE_KEY", cfg.Engine.LicenseKey)
	}
	setEnvFromConfigIfMissing("FUSED_ENCRYPTION_KEY", cfg.EncryptionKey)
}

func recordEngineLicenseResolution(ctx context.Context, source string) {
	_, span := otel.Tracer("engine").Start(ctx, "engine.configuration.license.resolve")
	span.SetAttributes(
		attribute.String("license.source", source),
		attribute.Bool("license.configured", source != "missing"),
	)
	span.End()
}

func setEnvFromConfigIfMissing(name, value string) {
	if os.Getenv(name) == "" && value != "" {
		os.Setenv(name, value)
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
	appTokenInvalidations *apptokeninvalidation.Worker
	executionEvents       *worker.ExecutionEventWorker
	retention             *worker.ExecutionRetentionWorker
	publicInsights        *worker.PublicInsightWorker
	usageCounter          *worker.UsageCounterWorker
	packageLeases         *worker.SDKPackageLeaseWorker
	providerRateLimits    *worker.ProviderRateLimitProjectionWorker
}

func (w engineWorkers) Stop(ctx context.Context) {
	if w.appTokenInvalidations != nil {
		w.appTokenInvalidations.Stop()
	}
	if w.providerRateLimits != nil {
		w.providerRateLimits.Stop(ctx)
	}
	if w.usageCounter != nil {
		w.usageCounter.Stop(ctx)
	}
	if w.executionEvents != nil {
		w.executionEvents.Stop(ctx)
	}
	if w.retention != nil {
		w.retention.Stop(ctx)
	}
	if w.publicInsights != nil {
		w.publicInsights.Stop(ctx)
	}
	if w.packageLeases != nil {
		w.packageLeases.Stop(ctx)
	}
}

func startProviderRateLimitProjection(ctx context.Context, projectionStore store.Store, natsClient *messaging.NATSClient) *worker.ProviderRateLimitProjectionWorker {
	typed, ok := projectionStore.(interface {
		BatchUpsertProviderRateLimitStates(context.Context, []ratelimitpolicy.StateEnvelope) error
	})
	if !ok {
		slog.ErrorContext(ctx, "FATAL: Provider rate-limit projection store is unavailable")
		os.Exit(1)
	}
	projectionWorker, err := worker.StartProviderRateLimitProjectionWorker(ctx, typed, natsClient)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to start provider rate-limit projection", slog.Any("error", err))
		os.Exit(1)
	}
	return projectionWorker
}

type runtimeEntitlementStore interface {
	SaveRuntimeEntitlement(ctx context.Context, entitlement models.RuntimeEntitlement) error
	GetRuntimeEntitlement(ctx context.Context) (models.RuntimeEntitlement, error)
}

func startEngineWorkers(ctx context.Context, engineStore store.Store, natsClient *messaging.NATSClient, cfg config.EngineConfig, tokenInvalidator apptokeninvalidation.Invalidator) engineWorkers {
	appTokenInvalidations, err := apptokeninvalidation.StartWorker(natsClient.JS, tokenInvalidator)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to start app-token invalidation subscriber", slog.Any("error", err))
		os.Exit(1)
	}
	executionevent.SetPublisher(executionevent.NewPublisher(natsClient))
	executionEventWorker, err := worker.StartExecutionEventWorker(ctx, engineStore, natsClient)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to start execution event persistence", slog.Any("error", err))
		os.Exit(1)
	}
	sandbox.SetIdempotencyStore(engineStore)
	// Webhook ingress resolves slugs against the Engine's own table so
	// delivery keeps working even when Registry is only used as the control
	// plane for catalogue data.
	sandbox.SetWebhookConfigStore(engineStore)
	worker.StartMCPSessionWorker(ctx, engineStore, natsClient)
	retentionWorker := worker.StartDynamicExecutionRetentionWorker(ctx, engineStore, func() int {
		return engineExecutionRetentionDays(entitlementpkg.LiveEntitlement.Load(), cfg.ExecutionRetentionDays)
	}, cfg.ExecutionCleanupBatch)
	return engineWorkers{appTokenInvalidations: appTokenInvalidations, executionEvents: executionEventWorker, retention: retentionWorker}
}

func engineExecutionRetentionDays(entitlement models.RuntimeEntitlement, fallback int) int {
	if entitlement.ExecutionRetentionDays == nil {
		return fallback
	}
	return *entitlement.ExecutionRetentionDays
}

func startSDKPackageLeaseRenewal(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient) *worker.SDKPackageLeaseWorker {
	leaseStore, ok := engineStore.(worker.SDKPackageLeaseStore)
	if !ok {
		slog.WarnContext(ctx, "SDK package lease store unavailable; package retention renewal disabled")
		return nil
	}
	leaseWorker := worker.NewSDKPackageLeaseWorker(leaseStore, registryClient, worker.SDKPackageLeaseOptions{})
	// Start is non-blocking, but its goroutine completes the startup pass before
	// arming the periodic timer. Registry availability never gates Engine runtime.
	leaseWorker.Start(ctx)
	return leaseWorker
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

func bootstrapRegistryIdentity(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient, envLicense string) (models.RuntimeEntitlement, int64) {
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

	accessRepository, ok := engineStore.(accesscontrol.BootstrapRepository)
	if !ok {
		slog.ErrorContext(ctx, "FATAL: Engine store does not support access-control bootstrap")
		os.Exit(1)
	}
	accessBootstrap, err := accesscontrol.BootstrapOwner(ctx, accessRepository, accUUID, envLicense, handshake.OwnerEmail)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to initialize bootstrap Owner access", slog.Any("error", err))
		os.Exit(1)
	}
	ownedServices, err := sandbox.ReconcileOwnedServices(ctx, engineStore, registryClient, accUUID, envLicense)
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to reconcile Registry services into the Engine workspace", slog.Any("error", err))
		os.Exit(1)
	}
	entitlement := handshake.Entitlements.Normalized()
	if entitlementStore, ok := engineStore.(runtimeEntitlementStore); ok {
		if err := entitlementStore.SaveRuntimeEntitlement(ctx, entitlement); err != nil {
			slog.WarnContext(ctx, "Failed to persist Registry entitlement bundle", slog.Any("error", err))
			// Do not acknowledge a revision that is only present in memory. The
			// next heartbeat will advertise an empty/older revision and Registry
			// will resend the bundle until persistence succeeds.
			entitlement.EntitlementRevision = ""
		}
	}
	slog.InfoContext(ctx, "Successfully initialized Engine workspace and bootstrap Owner",
		slog.String("account", accountIDStr),
		slog.String("workspace", wsName),
		slog.String("plan", entitlement.Plan),
		slog.String("usage_reporting", entitlement.UsageReporting),
		slog.Int64("authorization_revision", accessBootstrap.Revision),
		slog.Int("owned_services_discovered", ownedServices.Discovered),
		slog.Int("owned_services_restored", ownedServices.Activated),
		slog.Int("owned_services_already_active", ownedServices.AlreadyActive),
	)
	return entitlement, accessBootstrap.Revision
}

func newControlAuthenticator(ctx context.Context, engineStore store.Store, revision int64) *accesscontrol.Authenticator {
	loader, ok := engineStore.(accesscontrol.PrincipalLoader)
	if !ok {
		slog.ErrorContext(ctx, "FATAL: Engine store does not support control authentication")
		os.Exit(1)
	}
	authenticator, err := accesscontrol.NewAuthenticator(loader, revision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "FATAL: Failed to initialize control authenticator", slog.Any("error", err))
		os.Exit(1)
	}
	return authenticator
}

func startAuthorizationRevisionPolling(ctx context.Context, engineStore store.Store, authenticator *accesscontrol.Authenticator) {
	loader, ok := engineStore.(accesscontrol.AuthorizationRevisionLoader)
	if !ok {
		slog.ErrorContext(ctx, "FATAL: Engine store does not support authorization revision loading")
		os.Exit(1)
	}
	go accesscontrol.PollAuthorizationRevisions(ctx, loader, authenticator, 5*time.Second, func(err error) {
		slog.WarnContext(ctx, "Failed to refresh authorization revision", slog.Any("error", err))
	})
}

func startEngineHeartbeat(ctx context.Context, registryClient *sandbox.HTTPRegistryClient, entitlement models.RuntimeEntitlement, entitlementStore runtimeEntitlementStore) {
	if !entitlement.HeartbeatRequired {
		slog.InfoContext(ctx, "Engine heartbeat disabled by Registry entitlement")
		return
	}
	interval := engineHeartbeatInterval(entitlement)
	entitlementpkg.MarkHeartbeatSuccess(time.Now())
	go func() {
		// Send immediately after bootstrap so Registry can verify the runtime
		// during startup; the ticker then proves the Engine stayed alive.
		sendEngineHeartbeat(ctx, registryClient, entitlementStore)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		leaseTicker := time.NewTicker(engineHeartbeatLeaseCheckInterval(entitlement))
		defer leaseTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sendEngineHeartbeat(ctx, registryClient, entitlementStore)
			case <-leaseTicker.C:
				current := entitlementpkg.LiveEntitlement.Load()
				staleAfter := time.Duration(current.HeartbeatStaleAfterSeconds) * time.Second
				if entitlementpkg.EvaluateHeartbeatLease(time.Now(), staleAfter) {
					slog.ErrorContext(ctx, "Engine heartbeat grace period expired. Restricting runtime operations.", slog.Duration("stale_after", staleAfter))
				}
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

func startPublicServiceInsightReporting(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient) *worker.PublicInsightWorker {
	reportStore, ok := engineStore.(worker.PublicInsightStore)
	if !ok {
		slog.WarnContext(ctx, "Public service insight store unavailable; reporting disabled")
		return nil
	}
	reportWorker := worker.NewPublicInsightWorker(reportStore, registryClient, worker.PublicInsightOptions{
		Interval:      engineDurationFromEnv("FUSED_PUBLIC_INSIGHT_INTERVAL", time.Minute),
		EngineVersion: Version, EngineBuildHash: BuildHash,
	})
	reportWorker.Start(ctx)
	return reportWorker
}

func sendEngineHeartbeat(ctx context.Context, registryClient *sandbox.HTTPRegistryClient, entitlementStore runtimeEntitlementStore) bool {
	applied := entitlementpkg.LiveEntitlement.Load()
	resp, err := registryClient.SendHeartbeat(ctx, Version, BuildHash, applied.Plan, applied.EntitlementRevision, time.Now())
	if err != nil {
		slog.WarnContext(ctx, "Failed to send Engine heartbeat", slog.Any("error", err))
		return false
	}
	entitlementpkg.MarkHeartbeatSuccess(time.Now())

	entitlementpkg.EngineSuspended.Store(resp.IsSuspended)
	if resp.IsSuspended {
		slog.ErrorContext(ctx, "Engine suspended by Registry. Halting runtime operations.")
	}

	if resp.PlanChanged && resp.Entitlements != nil {
		newEntitlement := sandbox.RuntimeEntitlementFromHandshake(resp.Entitlements)
		if err := persistHeartbeatEntitlement(ctx, entitlementStore, newEntitlement); err != nil {
			slog.WarnContext(ctx, "Failed to persist updated entitlement after plan change", slog.Any("error", err))
		}
	}

	slog.DebugContext(ctx, "Sent Engine heartbeat", slog.String("version", Version), slog.String("build_hash", BuildHash))
	return true
}

func persistHeartbeatEntitlement(ctx context.Context, entitlementStore runtimeEntitlementStore, newEntitlement models.RuntimeEntitlement) error {
	if entitlementStore == nil {
		return errors.New("runtime entitlement store is unavailable")
	}
	oldPlan := entitlementpkg.LiveEntitlement.Load().Plan
	if err := entitlementStore.SaveRuntimeEntitlement(ctx, newEntitlement); err != nil {
		return err
	}
	entitlementpkg.LiveEntitlement.Store(newEntitlement)
	slog.InfoContext(ctx, "Engine entitlement updated after plan change",
		slog.String("old_plan", oldPlan),
		slog.String("new_plan", newEntitlement.Plan))
	return nil
}

func engineHeartbeatInterval(entitlement models.RuntimeEntitlement) time.Duration {
	// Local env remains an operator break-glass override for unusual network
	// conditions, but the Registry contract is the normal source of truth.
	if os.Getenv("FUSED_ENGINE_HEARTBEAT_INTERVAL") != "" {
		return engineDurationFromEnv("FUSED_ENGINE_HEARTBEAT_INTERVAL", time.Minute)
	}
	return time.Duration(entitlement.Normalized().HeartbeatIntervalSeconds) * time.Second
}

func engineHeartbeatLeaseCheckInterval(entitlement models.RuntimeEntitlement) time.Duration {
	interval := time.Duration(entitlement.Normalized().HeartbeatStaleAfterSeconds) * time.Second / 4
	if interval > 5*time.Second {
		return 5 * time.Second
	}
	if interval < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	return interval
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
				localObjectCache.InvalidateAppRuntime(parts[4])
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
	cfg                *config.Config
	natsClient         *messaging.NATSClient
	engineStore        store.Store
	registryClient     *sandbox.HTTPRegistryClient
	registryProxy      api.Forwarder
	localObjectCache   sandbox.ObjectCache
	configStore        store.ConfigRepository
	masterKey          []byte
	controlAuth        *accesscontrol.Authenticator
	managedLogin       api.ManagedLoginService
	cliLogin           api.CLILoginService
	browserSession     api.BrowserSessionService
	browserCookies     *browserauth.CookieManager
	providerRateLimits store.ProviderRateLimitStore
	tokenValidator     auth.TokenValidator
	appTokenRevoker    api.AppTokenRevoker
}

// buildEngineRouter serves API and embedded UI on one origin, so cross-origin
// browser access is neither required nor enabled by Engine configuration.
func buildEngineRouter(deps engineRouterDeps) chi.Router {
	// Router
	r := chi.NewRouter()
	r.Use(discardInboundRequestID)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(enginemiddleware.LicenseEnforcement)
	// Embedded SPA assets are many small JS/CSS chunks; compressing here keeps
	// local Engine hosting close to what a CDN would normally do for the UI.
	// JSON stays uncompressed so proxied GraphQL does not add buffering while
	// we are diagnosing browser-side TTFB.
	r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "image/svg+xml"))

	// This middleware must run before proxy routes: paths such as /integrations
	// are both SPA routes and Registry API routes, so the request shape decides.
	uiFS := backend.GetUIFS()
	r.Use(api.EmbeddedUIMiddleware(uiFS))
	auditRecorder, _ := deps.engineStore.(accesscontrol.AuditRecorder)
	r.Use(controlActorMiddlewareWithAudit(deps.controlAuth, auditRecorder, deps.browserCookies))
	r.Use(controlGraphQLAuditMiddleware(auditRecorder))
	r.Use(controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, newControlRequirementResolver(deps.engineStore, deps.configStore), auditRecorder))
	api.MountBrowserSessionRoutes(r, deps.browserSession)
	api.MountManagedIdentityRoutes(r, deps.managedLogin, deps.browserCookies)
	api.MountCLILoginRoutes(r, deps.cliLogin, deps.browserSession)

	// Exact Engine-owned control routes must be registered before Registry
	// prefix mounts so /sdks/{app_id}/download resolves locally while generation
	// and job-stream routes under /sdks continue through the Registry proxy.
	registerNativeRESTControlRoutes(r, deps)
	registerProxyRoutesWithRuntimeContracts(r, deps.registryProxy, deps.engineStore, deps.registryClient)

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

	secretResolver := sandbox.NewSecretResolver(deps.engineStore, deps.masterKey)

	sandbox.InitSandbox(r, deps.natsClient, deps.cfg, deps.localObjectCache, deps.tokenValidator, secretResolver, deps.providerRateLimits, port)
	// SDK and MCP webhook delivery uses EngineGRPCServer.SubscribeWebhooks.
	// Engine-native MCP GraphQL surface (list/deploy/kill/reactivate/delete +
	// analytics) -- a distinct endpoint from POST /graphql, which is a pure
	// Registry forward-proxy with no resolvers of its own (graphql_proxy.go).
	if err := api.MountMCPGraphQLRoute(r, deps.configStore, deps.engineStore, deps.registryClient, deps.registryClient, deps.masterKey, deps.controlAuth); err != nil {
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

func newManagedLoginService(
	ctx context.Context,
	engineStore store.Store,
	registryClient *sandbox.HTTPRegistryClient,
	authenticator *accesscontrol.Authenticator,
	masterKey []byte,
) *managedauth.Service {
	identityStore, ok := engineStore.(store.ManagedIdentityStore)
	if !ok {
		slog.ErrorContext(ctx, "Managed login store is unavailable")
		return nil
	}
	service, err := managedauth.NewService(identityStore, registryClient, authenticator, masterKey)
	if err != nil {
		slog.ErrorContext(ctx, "Managed login service is unavailable")
		return nil
	}
	service.StartCleanupWorker(ctx, time.Minute)
	return service
}

func newBrowserSessionService(
	ctx context.Context,
	engineStore store.Store,
	registryClient *sandbox.HTTPRegistryClient,
	authenticator *accesscontrol.Authenticator,
	masterKey []byte,
) (*browserauth.CookieManager, *browserauth.Service) {
	cookies, err := browserauth.NewCookieManager(masterKey)
	if err != nil {
		slog.ErrorContext(ctx, "Browser session cookies are unavailable")
		return nil, nil
	}
	sessionStore, ok := engineStore.(store.BrowserSessionStore)
	if !ok {
		slog.ErrorContext(ctx, "Browser session store is unavailable")
		return cookies, nil
	}
	service, err := browserauth.NewService(sessionStore, authenticator, cookies, registryClient, masterKey)
	if err != nil {
		slog.ErrorContext(ctx, "Browser session service is unavailable")
		return cookies, nil
	}
	return cookies, service
}

func newCLILoginService(
	ctx context.Context,
	engineStore store.Store,
	authenticator *accesscontrol.Authenticator,
) *cliauth.Service {
	loginStore, ok := engineStore.(store.CLILoginStore)
	if !ok {
		slog.ErrorContext(ctx, "CLI login store is unavailable")
		return nil
	}
	service, err := cliauth.NewService(loginStore, authenticator)
	if err != nil {
		slog.ErrorContext(ctx, "CLI login service is unavailable")
		return nil
	}
	return service
}

// Request IDs are audit identifiers, so they must be generated inside Engine
// rather than copied from a caller-controlled header that may contain secrets.
func discardInboundRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(middleware.RequestIDHeader)
		next.ServeHTTP(w, r)
	})
}

func startWebhookServer(ctx context.Context, r chi.Router) *http.Server {
	if webhookPort == "" || webhookPort == port {
		sandbox.InitWebhookRoutes(r)
		return nil
	}

	wr := chi.NewRouter()
	wr.Use(middleware.RequestID)
	wr.Use(middleware.Recoverer)
	sandbox.InitWebhookRoutes(wr)

	webhookSrv := newWebhookHTTPServer(wr)
	go serveHTTPServer(ctx, webhookSrv, "Starting Dedicated Webhook Server", slog.String("port", webhookPort), "Webhook Server failed")
	return webhookSrv
}

func newWebhookHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":" + webhookPort,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
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

func startEngineGRPCServer(ctx context.Context, engineStore store.Store, registryClient *sandbox.HTTPRegistryClient, masterKey []byte, configStore store.ConfigRepository, natsClient *messaging.NATSClient, tokenValidator auth.TokenValidator) *grpc.Server {
	listenAddress := engineGRPCListenAddress(grpcHost, grpcPort)
	lis, err := net.Listen("tcp", listenAddress)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to listen for gRPC", slog.Any("error", err))
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(enginemiddleware.UnaryLicenseEnforcement),
		grpc.StreamInterceptor(enginemiddleware.StreamLicenseEnforcement),
	)
	// SubscribeWebhooks needs both dependencies to resolve the configured
	// attachment and bridge its durable JetStream consumer to the gRPC stream.
	enginev1.RegisterEngineServiceServer(grpcServer, api.NewEngineGRPCServer(engineStore, registryClient, masterKey, configStore, natsClient, tokenValidator))

	go serveGRPCServer(ctx, grpcServer, lis, listenAddress)
	return grpcServer
}

func engineGRPCListenAddress(host, port string) string {
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func serveGRPCServer(ctx context.Context, grpcServer *grpc.Server, lis net.Listener, listenAddress string) {
	slog.InfoContext(ctx, "Starting Engine gRPC Server", slog.String("address", listenAddress))
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

func initDependencies(ctx context.Context, options ...config.LoadOption) (*config.Config, *pgxpool.Pool, *messaging.NATSClient, nats.KeyValue) {
	cfg, err := config.Load(ConfigPath, options...)
	if err != nil {
		slog.WarnContext(ctx, "Failed to parse config file, using defaults", slog.Any("error", err))
	}

	database, err := db.InitEnginePostgres(ctx, engineDatabaseURL(cfg))
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
	rateLimitKV, err := natsClient.InitProviderRateLimitBucket()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to init provider rate-limit KV", slog.Any("error", err))
		os.Exit(1)
	}
	return cfg, database, natsClient, rateLimitKV
}
