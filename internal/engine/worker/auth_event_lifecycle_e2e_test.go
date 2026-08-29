package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/applifecycle"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/authevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

const (
	authLifecycleE2EBufSize           = 1024 * 1024
	authLifecycleE2EHash              = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	authLifecycleE2EMasterKey         = "12345678901234567890123456789012"
	authLifecycleE2EReconnectReceiver = "auth-lifecycle-reconnect-e2e"
	authLifecycleE2ERefreshReceiver   = "auth-lifecycle-refresh-e2e"
)

// authLifecycleE2EFixture contains only the identities that cross the stitched PostgreSQL, JetStream, and gRPC boundaries.
type authLifecycleE2EFixture struct {
	repository       store.Store
	accountID        uuid.UUID
	appFamilyID      uuid.UUID
	appID            uuid.UUID
	bucketID         uuid.UUID
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	connectionID     uuid.UUID
	executionToken   string
	now              time.Time
}

// authLifecycleE2EProviderRequest captures only the OAuth form fields needed to prove the real refresh exchange.
type authLifecycleE2EProviderRequest struct {
	method       string
	path         string
	grantType    string
	refreshToken string
	clientID     string
	clientSecret string
	parseFailed  bool
}

// authLifecycleE2EStoredRefresh is the bounded PostgreSQL projection needed to verify the winning rotation.
type authLifecycleE2EStoredRefresh struct {
	wrappedDEK       string
	encryptedAccess  string
	encryptedRefresh string
	state            string
	failureCode      string
	leaseToken       *uuid.UUID
	refreshedAt      *time.Time
	retryNotBefore   *time.Time
	expiresAt        *time.Time
}

// authLifecycleE2EProvider serves one deterministic token response while retaining the received grant for assertion.
type authLifecycleE2EProvider struct {
	requests chan authLifecycleE2EProviderRequest
}

// ServeHTTP captures the provider-facing refresh request before returning rotated token material.
func (provider *authLifecycleE2EProvider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	captured := authLifecycleE2EProviderRequest{method: request.Method, path: request.URL.Path}
	// Form parsing failure is retained as bounded test evidence instead of failing from the server goroutine.
	if err := request.ParseForm(); err != nil {
		captured.parseFailed = true
	} else {
		// Capturing only expected OAuth fields keeps unrelated headers and credentials out of failure output.
		captured.grantType = request.Form.Get("grant_type")
		captured.refreshToken = request.Form.Get("refresh_token")
		captured.clientID = request.Form.Get("client_id")
		captured.clientSecret = request.Form.Get("client_secret")
	}
	provider.requests <- captured
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`))
}

// TestAuthReconnectRequiredFlowsFromPostgresThroughSDKReceiver proves a committed refresh failure reaches the authenticated SDK stream.
func TestAuthReconnectRequiredFlowsFromPostgresThroughSDKReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// The single deadline bounds database setup, broker projection, and gRPC delivery together.
	t.Cleanup(cancel)
	pool := authLifecycleE2EPostgres(t, ctx)
	fixture := seedAuthLifecycleE2EFixture(t, ctx, pool)
	jetStream, receiver := startAuthLifecycleE2EHarness(
		t, ctx, pool, fixture, authevent.TypeReconnectRequired, authLifecycleE2EReconnectReceiver,
	)
	triggerMissingRefreshToken(t, ctx, pool, fixture)
	assertReconnectRequiredE2EDelivery(t, jetStream, receiver, fixture)
}

// TestAuthTokenRefreshedFlowsFromPostgresThroughSDKReceiver proves a successful provider rotation reaches and advances the authenticated SDK stream.
func TestAuthTokenRefreshedFlowsFromPostgresThroughSDKReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	// The shared deadline bounds real schema setup, provider exchange, broker projection, and SDK acknowledgement.
	t.Cleanup(cancel)
	pool := authLifecycleE2EPostgres(t, ctx)
	fixture := seedAuthLifecycleE2EFixture(t, ctx, pool)
	provider, requests := startAuthLifecycleE2EProvider(t)
	seedAuthLifecycleE2ERefreshSuccess(t, ctx, pool, fixture, provider.URL)
	jetStream, receiver := startAuthLifecycleE2EHarness(
		t, ctx, pool, fixture, authevent.TypeTokenRefreshed, authLifecycleE2ERefreshReceiver,
	)
	triggerSuccessfulRefresh(t, ctx, pool, fixture, provider.Client())
	assertAuthLifecycleE2EProviderRequest(t, requests)
	assertTokenRefreshedE2EDelivery(t, jetStream, receiver, fixture)
}

// authLifecycleE2EPostgres initializes one disposable schema on the explicitly configured PostgreSQL server.
func authLifecycleE2EPostgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	// An explicit database opt-in prevents an ordinary unit run from mutating a developer database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	admin, err := pgxpool.New(ctx, databaseURL)
	// The real integration cannot fall back to a mock when PostgreSQL is unavailable.
	if err != nil {
		t.Fatalf("connect auth lifecycle PostgreSQL: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := "engine_auth_lifecycle_e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	// Schema isolation lets this test run beside a developer Engine without touching its rows.
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create auth lifecycle schema: %v", err)
	}
	// Cleanup removes only the UUID-scoped schema created by this test.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A cleanup failure is reported because retained integration state can hide later isolation defects.
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop auth lifecycle schema: %v", err)
		}
	})
	parsed, err := url.Parse(databaseURL)
	// A valid PostgreSQL URL is required before the isolated search path can be attached.
	if err != nil || parsed.Scheme == "" {
		t.Fatal("DATABASE_URL must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	// Full Engine schema initialization is part of this integration boundary.
	if err != nil {
		t.Fatalf("initialize auth lifecycle schema: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedAuthLifecycleE2EFixture creates one active OAuth SDK and one expired access-only connection with exact app provenance.
func seedAuthLifecycleE2EFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) authLifecycleE2EFixture {
	t.Helper()
	fixture := authLifecycleE2EFixture{
		repository: store.NewPostgresStore(pool), accountID: uuid.New(), appFamilyID: uuid.New(), appID: uuid.New(),
		bucketID: uuid.New(), serviceID: uuid.New(), serviceVersionID: uuid.New(), connectionID: uuid.New(),
		now: time.Now().UTC().Truncate(time.Second),
	}
	teamID := uuid.New()
	selectionJSON := marshalAuthLifecycleE2ESelections(t, fixture)
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_workspaces (account_id, name, slug)
		VALUES ($1, 'Auth lifecycle E2E', $2)
	`, fixture.accountID, "auth-lifecycle-"+fixture.accountID.String())
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_teams (id, name, slug)
		VALUES ($1, 'Auth lifecycle owner', $2)
	`, teamID, "auth-lifecycle-owner-"+teamID.String())
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_buckets (id, name, is_default)
		VALUES ($1, $2, true)
	`, fixture.bucketID, "auth-lifecycle-"+fixture.bucketID.String())
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Auth lifecycle SDK', 'typescript', $4)
	`, fixture.appFamilyID, fixture.accountID, "auth-lifecycle-"+fixture.appFamilyID.String(), teamID)
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key, source_hash, capability_hash,
			 scope_schema_version, selections, status, activated_at)
		VALUES ($1, $2, $3, '1.0.0', $4, $5, $5, $6, $7, 'active', NOW())
	`, fixture.appID, fixture.appFamilyID, fixture.accountID, "sdk:auth-lifecycle:"+fixture.appID.String(), authLifecycleE2EHash, models.AppScopeSchemaVersion, selectionJSON)
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $2)
	`, fixture.appFamilyID, fixture.bucketID)
	expiresAt := fixture.now.Add(-time.Minute)
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		INSERT INTO fused_auth_connections
			(id, bucket_id, service_id, service_version_id, end_user_ref, created_by_app_id,
			 auth_type, auth_name, credential_source_service_id, credential_source_auth_type,
			 credential_source_auth_name, encrypted_dek, access_token, refresh_token,
			 token_type, scopes, scope_source, identity_claims, expires_at, refresh_state)
		VALUES ($1, $2, $3, $4, 'customer-42', $5,
		        'oauth2', 'jira', $3, 'oauth2', 'jira',
		        'fixture-envelope', 'fixture-ciphertext', NULL,
		        'Bearer', '{}', 'none', '{}', $6, 'ok')
	`, fixture.connectionID, fixture.bucketID, fixture.serviceID, fixture.serviceVersionID, fixture.appID, expiresAt)
	plaintext, _, err := applifecycle.New(fixture.repository).GenerateToken(ctx, applifecycle.GenerateTokenParams{
		AppFamilyID: fixture.appFamilyID, Name: "auth-lifecycle-e2e",
	})
	// The gRPC receiver must authenticate through the real family-token tables.
	if err != nil {
		t.Fatalf("generate auth lifecycle execution token: %v", err)
	}
	fixture.executionToken = plaintext
	return fixture
}

// seedAuthLifecycleE2ERefreshSuccess replaces the access-only row with valid encrypted rotation material and its exact immutable OAuth contract.
func seedAuthLifecycleE2ERefreshSuccess(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture authLifecycleE2EFixture, providerURL string) {
	t.Helper()
	wrappedDEK, encryptedTokens := encryptAuthLifecycleE2EValues(t, "old-access", "old-refresh")
	// The due row must carry realistic encryption so both decrypt-before-provider and encrypt-before-CAS execute.
	mustExecAuthLifecycleE2E(t, ctx, pool, `
		UPDATE fused_auth_connections
		SET encrypted_dek = $2,
		    access_token = $3,
		    refresh_token = $4,
		    expires_at = $5,
		    refresh_state = 'ok'
		WHERE id = $1
	`, fixture.connectionID, wrappedDEK, encryptedTokens[0], encryptedTokens[1], fixture.now.Add(-time.Minute))
	snapshotStore, ok := fixture.repository.(store.ServiceContractSnapshotStore)
	// The coordinator must load its OAuth definition through the real immutable snapshot store.
	if !ok {
		t.Fatal("PostgreSQL store does not implement service contract snapshots")
	}
	snapshot := store.ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 fixture.serviceID, ServiceVersionID: fixture.serviceVersionID,
		Version: "2026.8.29", Status: "active",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Name: "Auth lifecycle provider",
			AuthConfigs: fusedobject.AuthConfigs{{
				Name: "jira", Type: "oauth2",
				TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
				OAuth2Flows: fusedobject.OAuth2Flows{"authorizationCode": {
					AuthorizationURL: providerURL + "/authorize", TokenURL: providerURL + "/token", Scopes: map[string]string{},
				}},
			}},
		},
	}
	// Snapshot admission and persistence prove refresh uses the consent-time service version rather than a test stub.
	if _, err := snapshotStore.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("seed auth lifecycle service contract: %v", err)
	}
	clientIDKey, clientSecretKey, ok := credentialkeys.OAuthApplication("jira")
	// Exact generated key names keep application registration lookup on its production resolver path.
	if !ok {
		t.Fatal("derive auth lifecycle OAuth application keys")
	}
	credentialDEK, encryptedCredentials := encryptAuthLifecycleE2EValues(t, "client-id", "client-secret")
	secrets := []store.WorkspaceSecret{
		{WorkspaceSecretMeta: store.WorkspaceSecretMeta{
			BucketID: fixture.bucketID, ServiceID: fixture.serviceID, KeyName: clientIDKey, CredentialType: "oauth",
		}, EncryptedDEK: credentialDEK, EncryptedValue: encryptedCredentials[0]},
		{WorkspaceSecretMeta: store.WorkspaceSecretMeta{
			BucketID: fixture.bucketID, ServiceID: fixture.serviceID, KeyName: clientSecretKey, CredentialType: "oauth",
		}, EncryptedDEK: credentialDEK, EncryptedValue: encryptedCredentials[1]},
	}
	// A set-based family write prevents the provider client pair from becoming partially visible.
	if err := fixture.repository.UpsertSecrets(ctx, secrets); err != nil {
		t.Fatalf("seed auth lifecycle OAuth application: %v", err)
	}
}

// encryptAuthLifecycleE2EValues wraps one data key and encrypts a related test credential family exactly as Engine does.
func encryptAuthLifecycleE2EValues(t *testing.T, plaintexts ...string) (string, []string) {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK([]byte(authLifecycleE2EMasterKey))
	// A real envelope is required because the coordinator validates and unwraps persisted material.
	if err != nil {
		t.Fatalf("wrap auth lifecycle data key: %v", err)
	}
	encrypted := make([]string, 0, len(plaintexts))
	for _, plaintext := range plaintexts {
		ciphertext, err := store.EncryptWithDEK(dek, plaintext)
		// One encryption failure invalidates the entire related credential fixture.
		if err != nil {
			t.Fatalf("encrypt auth lifecycle value: %v", err)
		}
		encrypted = append(encrypted, ciphertext)
	}
	return wrappedDEK, encrypted
}

// marshalAuthLifecycleE2ESelections creates the exact immutable OAuth selection used by gRPC subscription authorization.
func marshalAuthLifecycleE2ESelections(t *testing.T, fixture authLifecycleE2EFixture) []byte {
	t.Helper()
	payload, err := json.Marshal(models.SDKSelections{{
		ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID,
		SchemaVersion: models.AppSelectionSchemaVersion, AuthType: "oauth2", AuthName: "jira",
	}})
	// Invalid fixture JSON cannot be allowed to masquerade as a subscription authorization failure.
	if err != nil {
		t.Fatalf("marshal auth lifecycle selections: %v", err)
	}
	return payload
}

// mustExecAuthLifecycleE2E persists one relational fixture statement or stops before the execution chain begins.
func mustExecAuthLifecycleE2E(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, arguments ...any) {
	t.Helper()
	// Fixture write failures are setup errors rather than lifecycle behavior.
	if _, err := pool.Exec(ctx, query, arguments...); err != nil {
		t.Fatalf("seed auth lifecycle fixture: %v", err)
	}
}

// startAuthLifecycleE2EProvider opens a real loopback HTTP server for the successful OAuth refresh grant.
func startAuthLifecycleE2EProvider(t *testing.T) (*httptest.Server, <-chan authLifecycleE2EProviderRequest) {
	t.Helper()
	requests := make(chan authLifecycleE2EProviderRequest, 1)
	provider := httptest.NewServer(&authLifecycleE2EProvider{requests: requests})
	// Provider shutdown is tied to the test so an exchange failure cannot retain a listener.
	t.Cleanup(provider.Close)
	return provider, requests
}

// startAuthLifecycleE2EHarness wires the canonical publisher, PostgreSQL projector, and authenticated SDK receiver for one event type.
func startAuthLifecycleE2EHarness(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	fixture authLifecycleE2EFixture,
	eventType authevent.Type,
	receiverName string,
) (nats.JetStreamContext, enginev1.EngineService_SubscribeWebhooksClient) {
	t.Helper()
	jetStream, natsClient := authEventWebhookTestJetStream(t)
	authevent.SetPublisher(authevent.NewPublisher(natsClient))
	// Global publication wiring is restored so unrelated package tests cannot inherit this broker.
	t.Cleanup(func() { authevent.SetPublisher(nil) })
	projector := startAuthLifecycleE2EProjector(t, ctx, fixture.repository, natsClient)
	// The projector drains before the broker fixture is torn down.
	t.Cleanup(func() { projector.Stop(context.Background()) })
	receiver := startAuthLifecycleE2EReceiver(t, ctx, pool, fixture, natsClient, eventType, receiverName)
	return jetStream, receiver
}

// startAuthLifecycleE2EProjector starts the production Postgres-backed app-family resolver over the real broker.
func startAuthLifecycleE2EProjector(t *testing.T, ctx context.Context, repository store.Store, natsClient *messaging.NATSClient) *AuthEventWebhookWorker {
	t.Helper()
	resolver, ok := repository.(store.AuthEventAppFamilyResolver)
	// The stitched test must use the real set-based PostgreSQL resolver rather than a family stub.
	if !ok {
		t.Fatal("PostgreSQL store does not implement auth event family resolution")
	}
	projector, err := StartAuthEventWebhookWorker(ctx, resolver, natsClient)
	// Projection startup must finish before the SDK receiver announces readiness.
	if err != nil {
		t.Fatalf("start auth lifecycle projector: %v", err)
	}
	return projector
}

// startAuthLifecycleE2EReceiver opens the production bidirectional gRPC receiver and waits for subscription readiness.
func startAuthLifecycleE2EReceiver(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	fixture authLifecycleE2EFixture,
	natsClient *messaging.NATSClient,
	eventType authevent.Type,
	receiverName string,
) enginev1.EngineService_SubscribeWebhooksClient {
	t.Helper()
	server := api.NewEngineGRPCServer(
		fixture.repository, nil, nil, store.NewPostgresConfigRepository(pool), natsClient,
		auth.NewTokenValidator(fixture.repository),
	)
	grpcServer := grpc.NewServer()
	enginev1.RegisterEngineServiceServer(grpcServer, server)
	listener := bufconn.Listen(authLifecycleE2EBufSize)
	// Serving on an in-memory listener retains real gRPC framing and metadata without opening a host port.
	go func() { _ = grpcServer.Serve(listener) }()
	// Server shutdown releases the receiver goroutines and durable delivery subscription.
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	// The in-memory dialer keeps the test hermetic while exercising the generated gRPC transport.
	dialer := func(context.Context, string) (net.Conn, error) { return listener.Dial() }
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	// A failed client channel cannot prove SDK receiver behavior.
	if err != nil {
		t.Fatalf("dial auth lifecycle gRPC receiver: %v", err)
	}
	// Client cleanup closes the bidirectional stream before the in-memory server stops.
	t.Cleanup(func() { _ = connection.Close() })
	streamContext := metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-api-key", fixture.executionToken,
		"x-app-id", fixture.appID.String(),
	))
	receiver, err := enginev1.NewEngineServiceClient(connection).SubscribeWebhooks(streamContext)
	// The production stream must open before its initial authorization frame is sent.
	if err != nil {
		t.Fatalf("open auth lifecycle SDK receiver: %v", err)
	}
	eventName := authLifecycleE2EEventName(t, fixture, eventType)
	err = receiver.Send(&enginev1.WebhookClientMessage{Payload: &enginev1.WebhookClientMessage_Subscribe{
		Subscribe: &enginev1.WebhookSubscribe{ReceiverName: receiverName, Events: []string{eventName}},
	}})
	// Subscription errors must be reported before the database transition is triggered.
	if err != nil {
		t.Fatalf("subscribe auth lifecycle SDK receiver: %v", err)
	}
	ready, err := receiver.Recv()
	// Readiness proves token, app runtime, OAuth selection, and JetStream consumer authorization all succeeded.
	if err != nil || ready.GetSubscribed() == nil {
		t.Fatalf("receive auth lifecycle subscription readiness: message=%v err=%v", ready != nil, err)
	}
	return receiver
}

// authLifecycleE2EEventName derives the same service-qualified enum value generated SDK clients subscribe to.
func authLifecycleE2EEventName(t *testing.T, fixture authLifecycleE2EFixture, eventType authevent.Type) string {
	t.Helper()
	eventName, ok := authevent.WebhookEventName(eventType)
	// The stitched receiver accepts only lifecycle types admitted by the canonical public mapping.
	if !ok {
		t.Fatalf("auth lifecycle event type %q has no SDK mapping", eventType)
	}
	return fixture.serviceID.String() + "." + eventName
}

// triggerMissingRefreshToken commits reconnect-required state through the real lease CAS before publication.
func triggerMissingRefreshToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture authLifecycleE2EFixture) {
	t.Helper()
	refreshStore, ok := fixture.repository.(store.AuthConnectionRefreshStore)
	// The coordinator must own the actual PostgreSQL refresh lease implementation in this test.
	if !ok {
		t.Fatal("PostgreSQL store does not implement auth refresh leases")
	}
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(ctx, fixture.connectionID, fixture.now, fixture.now.Add(time.Minute))
	// The expired access-only row must be durably leased before it can transition.
	if err != nil || claim == nil {
		t.Fatalf("claim auth lifecycle connection: claim=%v err=%v", claim != nil, err)
	}
	// A fixed clock keeps the lease and expiry decision deterministic across PostgreSQL and the coordinator.
	coordinator := sandbox.NewAuthRefreshCoordinator(
		fixture.repository, []byte(authLifecycleE2EMasterKey),
		sandbox.WithAuthRefreshClock(func() time.Time { return fixture.now }),
	)
	result, err := coordinator.RefreshClaimedConnection(ctx, *claim)
	assertMissingRefreshOutcome(t, result, err)
	assertCommittedReconnectState(t, ctx, pool, fixture.connectionID)
}

// triggerSuccessfulRefresh runs the production coordinator through provider exchange and the PostgreSQL lease CAS.
func triggerSuccessfulRefresh(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture authLifecycleE2EFixture, providerClient *http.Client) {
	t.Helper()
	refreshStore, ok := fixture.repository.(store.AuthConnectionRefreshStore)
	// The happy path must claim and complete through the real PostgreSQL lease implementation.
	if !ok {
		t.Fatal("PostgreSQL store does not implement auth refresh leases")
	}
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(ctx, fixture.connectionID, fixture.now, fixture.now.Add(time.Minute))
	// A due encrypted row must be exclusively leased before provider rotation starts.
	if err != nil || claim == nil {
		t.Fatalf("claim refreshable auth lifecycle connection: claim=%v err=%v", claim != nil, err)
	}
	providerClient.Timeout = 2 * time.Second
	// The loopback client keeps real HTTP serialization while bounding a broken provider independently from the test deadline.
	coordinator := sandbox.NewAuthRefreshCoordinator(
		fixture.repository, []byte(authLifecycleE2EMasterKey),
		sandbox.WithAuthRefreshClock(func() time.Time { return fixture.now }),
		sandbox.WithAuthRefreshHTTPClient(providerClient),
	)
	result, err := coordinator.RefreshClaimedConnection(ctx, *claim)
	// A valid grant and client registration must complete without a retry or reconnect decision.
	if err != nil || result.Outcome != sandbox.AuthRefreshOutcomeRefreshed || result.FailureCode != "" {
		t.Fatalf("successful auth refresh result=%#v err=%v", result, err)
	}
	assertCommittedRefreshState(t, ctx, pool, fixture)
}

// assertAuthLifecycleE2EProviderRequest proves the production OAuth client sent the expected refresh grant and application credentials.
func assertAuthLifecycleE2EProviderRequest(t *testing.T, requests <-chan authLifecycleE2EProviderRequest) {
	t.Helper()
	select {
	case request := <-requests:
		// Every provider-facing field must match the exact stored contract and encrypted credential inputs.
		if request.parseFailed || request.method != http.MethodPost || request.path != "/token" ||
			request.grantType != "refresh_token" || request.refreshToken != "old-refresh" ||
			request.clientID != "client-id" || request.clientSecret != "client-secret" {
			t.Fatal("OAuth refresh provider did not receive the expected grant")
		}
	case <-time.After(time.Second):
		// A missing provider request means the coordinator bypassed the real token exchange.
		t.Fatal("OAuth refresh provider did not receive a request")
	}
}

// assertCommittedRefreshState decrypts the PostgreSQL winner to prove rotation committed before event publication returned.
func assertCommittedRefreshState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture authLifecycleE2EFixture) {
	t.Helper()
	stored := readCommittedRefreshState(t, ctx, pool, fixture.connectionID)
	assertCommittedRefreshMetadata(t, stored, fixture.now)
	assertCommittedRefreshTokens(t, stored)
}

// readCommittedRefreshState loads only persisted rotation evidence from the exact connection row.
func readCommittedRefreshState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectionID uuid.UUID) authLifecycleE2EStoredRefresh {
	t.Helper()
	var stored authLifecycleE2EStoredRefresh
	// Direct relational inspection proves the event was not produced from an in-memory token response alone.
	if err := pool.QueryRow(ctx, `
		SELECT encrypted_dek, access_token, refresh_token, refresh_state, last_failure_code,
		       refresh_lease_token, last_refreshed_at, refresh_retry_not_before, expires_at
		FROM fused_auth_connections WHERE id = $1
	`, connectionID).Scan(
		&stored.wrappedDEK, &stored.encryptedAccess, &stored.encryptedRefresh, &stored.state, &stored.failureCode,
		&stored.leaseToken, &stored.refreshedAt, &stored.retryNotBefore, &stored.expiresAt,
	); err != nil {
		t.Fatalf("read committed refreshed auth connection: %v", err)
	}
	return stored
}

// assertCommittedRefreshMetadata verifies the CAS state, lease release, and deterministic scheduling fields.
func assertCommittedRefreshMetadata(t *testing.T, stored authLifecycleE2EStoredRefresh, now time.Time) {
	t.Helper()
	// The winning CAS must clear its lease and stale failure state while scheduling the next safe rotation.
	if stored.state != "ok" || stored.failureCode != "" || stored.leaseToken != nil || stored.refreshedAt == nil || stored.retryNotBefore == nil || !stored.retryNotBefore.After(now) {
		t.Fatalf("committed refreshed state=%q failure=%q lease=%v refreshed=%v retry=%v", stored.state, stored.failureCode, stored.leaseToken, stored.refreshedAt, stored.retryNotBefore)
	}
	// Provider expiry is persisted from the coordinator clock rather than from database or test wall time.
	if stored.expiresAt == nil || !stored.expiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("committed refreshed expiry=%v want=%v", stored.expiresAt, now.Add(time.Hour))
	}
}

// assertCommittedRefreshTokens decrypts both saved values so ciphertext-only updates cannot satisfy the happy path.
func assertCommittedRefreshTokens(t *testing.T, stored authLifecycleE2EStoredRefresh) {
	t.Helper()
	// Decryption of the stored winner proves both provider values were re-encrypted before the database commit.
	if decryptAuthLifecycleE2EToken(t, stored.wrappedDEK, stored.encryptedAccess) != "new-access" ||
		decryptAuthLifecycleE2EToken(t, stored.wrappedDEK, stored.encryptedRefresh) != "new-refresh" {
		t.Fatal("committed refreshed token material did not rotate")
	}
}

// decryptAuthLifecycleE2EToken verifies persisted ciphertext through the same envelope primitives used by Engine dispatch.
func decryptAuthLifecycleE2EToken(t *testing.T, wrappedDEK, ciphertext string) string {
	t.Helper()
	dek, err := store.UnwrapDEK([]byte(authLifecycleE2EMasterKey), wrappedDEK)
	// An unreadable data key means the stored refresh winner cannot be used by a later SDK execution.
	if err != nil {
		t.Fatalf("unwrap auth lifecycle data key: %v", err)
	}
	plaintext, err := store.DecryptWithDEK(dek, ciphertext)
	// A committed but unreadable token is not a successful refresh outcome.
	if err != nil {
		t.Fatalf("decrypt auth lifecycle token: %v", err)
	}
	return plaintext
}

// assertMissingRefreshOutcome verifies the coordinator exposes the bounded permanent-failure decision.
func assertMissingRefreshOutcome(t *testing.T, result sandbox.AuthRefreshResult, err error) {
	t.Helper()
	// Missing refresh material is a committed reconnect decision, not a retryable coordinator error.
	if err != nil || result.Outcome != sandbox.AuthRefreshOutcomeReconnectRequired || result.FailureCode != "refresh_token_missing" {
		t.Fatalf("refresh missing-token result=%#v err=%v", result, err)
	}
}

// assertCommittedReconnectState verifies publication returned only after the PostgreSQL lease transition committed.
func assertCommittedReconnectState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectionID uuid.UUID) {
	t.Helper()
	var state, failureCode string
	var leaseToken *uuid.UUID
	// Reading after publication returns proves the database state already crossed its commit boundary.
	if err := pool.QueryRow(ctx, `
		SELECT refresh_state, last_failure_code, refresh_lease_token
		FROM fused_auth_connections WHERE id = $1
	`, connectionID).Scan(&state, &failureCode, &leaseToken); err != nil {
		t.Fatalf("read committed auth lifecycle state: %v", err)
	}
	// The event is valid only when the permanent state and lease release are already durable.
	if state != "reconnect_required" || failureCode != "refresh_token_missing" || leaseToken != nil {
		t.Fatalf("committed auth lifecycle state=%q code=%q lease=%v", state, failureCode, leaseToken)
	}
}

// assertReconnectRequiredE2EDelivery validates the public reconnect event and acknowledges it through the production stream.
func assertReconnectRequiredE2EDelivery(t *testing.T, jetStream nats.JetStreamContext, receiver enginev1.EngineService_SubscribeWebhooksClient, fixture authLifecycleE2EFixture) {
	t.Helper()
	event := receiveAuthLifecycleE2EEvent(t, receiver, fixture, authevent.TypeReconnectRequired)
	payload := assertReconnectRequiredPayload(t, event.GetPayload(), "refresh_token_missing")
	assertAuthLifecycleE2EPayloadIdentity(t, payload, fixture)
	ackAuthLifecycleE2EEvent(t, jetStream, receiver, fixture, authLifecycleE2EReconnectReceiver, event)
}

// assertTokenRefreshedE2EDelivery validates the public healthy event and acknowledges it through the production stream.
func assertTokenRefreshedE2EDelivery(t *testing.T, jetStream nats.JetStreamContext, receiver enginev1.EngineService_SubscribeWebhooksClient, fixture authLifecycleE2EFixture) {
	t.Helper()
	event := receiveAuthLifecycleE2EEvent(t, receiver, fixture, authevent.TypeTokenRefreshed)
	payload := assertTokenRefreshedPayload(t, event.GetPayload())
	assertAuthLifecycleE2EPayloadIdentity(t, payload, fixture)
	ackAuthLifecycleE2EEvent(t, jetStream, receiver, fixture, authLifecycleE2ERefreshReceiver, event)
}

// receiveAuthLifecycleE2EEvent reads one projected event and verifies its generated service-qualified dispatch name.
func receiveAuthLifecycleE2EEvent(
	t *testing.T,
	receiver enginev1.EngineService_SubscribeWebhooksClient,
	fixture authLifecycleE2EFixture,
	eventType authevent.Type,
) *enginev1.WebhookEvent {
	t.Helper()
	message, err := receiver.Recv()
	// The SDK must observe the projected event within the shared test deadline.
	if err != nil || message.GetEvent() == nil {
		t.Fatalf("receive auth lifecycle SDK event: message=%v err=%v", message != nil, err)
	}
	event := message.GetEvent()
	wantEvent := authLifecycleE2EEventName(t, fixture, eventType)
	// Generated handlers dispatch on the established service-ID plus event-name enum value.
	if event.GetEvent() != wantEvent {
		t.Fatalf("auth lifecycle SDK event=%q want=%q", event.GetEvent(), wantEvent)
	}
	return event
}

// assertAuthLifecycleE2EPayloadIdentity checks the actionable public connection identity shared by every transition.
func assertAuthLifecycleE2EPayloadIdentity(t *testing.T, payload map[string]any, fixture authLifecycleE2EFixture) {
	t.Helper()
	// Public correlation identifies the affected connected user and exact consent contract without exposing credentials.
	if payload["connection_id"] != fixture.connectionID.String() || payload["service_id"] != fixture.serviceID.String() ||
		payload["service_version_id"] != fixture.serviceVersionID.String() || payload["end_user_ref"] != "customer-42" {
		t.Fatalf("auth lifecycle SDK payload identity=%#v", payload)
	}
}

// ackAuthLifecycleE2EEvent sends the generated receiver ACK and waits for the exact durable consumer to advance.
func ackAuthLifecycleE2EEvent(
	t *testing.T,
	jetStream nats.JetStreamContext,
	receiver enginev1.EngineService_SubscribeWebhooksClient,
	fixture authLifecycleE2EFixture,
	receiverName string,
	event *enginev1.WebhookEvent,
) {
	t.Helper()
	// ACK traverses the same client frame used by generated TypeScript and Python receivers.
	if err := receiver.Send(&enginev1.WebhookClientMessage{Payload: &enginev1.WebhookClientMessage_Ack{
		Ack: &enginev1.WebhookAck{EventId: event.GetId()},
	}}); err != nil {
		t.Fatalf("ack auth lifecycle SDK event: %v", err)
	}
	waitForAuthLifecycleE2EAck(t, jetStream, fixture, receiverName)
}

// waitForAuthLifecycleE2EAck proves the gRPC acknowledgement advanced the exact SDK receiver's durable consumer.
func waitForAuthLifecycleE2EAck(t *testing.T, jetStream nats.JetStreamContext, fixture authLifecycleE2EFixture, receiverName string) {
	t.Helper()
	durableName := fixture.accountID.String() + "-" + fixture.appFamilyID.String() + "-" + fixture.appID.String() + "-" + receiverName
	deadline := time.Now().Add(5 * time.Second)
	// The receiver loop applies client frames asynchronously, so broker state is polled within a strict bound.
	for time.Now().Before(deadline) {
		info, err := jetStream.ConsumerInfo("WEBHOOKS", durableName)
		// One acknowledged delivery proves the frame matched the pending event rather than being ignored.
		if err == nil && info.NumAckPending == 0 && info.AckFloor.Consumer >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("auth lifecycle SDK acknowledgement did not advance the durable consumer")
}
