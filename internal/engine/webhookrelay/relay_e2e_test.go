package webhookrelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testcontract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	brokerDB, consumerDB                                                        *pgxpool.Pool
	brokerJS, consumerJS                                                        nats.JetStreamContext
	broker                                                                      *webhookrelay.Broker
	client                                                                      *webhookrelay.Client
	receiver                                                                    *webhookrelay.Receiver
	service, version, registration, localRegistration, connection, installation uuid.UUID
	account                                                                     uuid.UUID
	key                                                                         []byte
	serverURL                                                                   string
}
type staticToken string

// AccessToken models the existing installation credential while retaining real broker authentication.
func (s staticToken) AccessToken(context.Context) (string, error) { return string(s), nil }

type providerCatalog struct{ url string }

// GetProviderApp supplies a reviewed HTTPS provider fixture; the real OAuth exchange and proof extraction run unchanged.
func (p providerCatalog) GetProviderApp(context.Context, uuid.UUID, string, []byte, ...string) (managedauthbroker.ProviderApp, error) {
	return managedauthbroker.ProviderApp{ClientID: "test-client", ClientSecret: "test-secret", Auth: fusedobject.AuthConfig{Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost}, Flow: fusedobject.OAuth2FlowContract{TokenURL: p.url}}, nil
}

// TestRemoteWebhookDelivery proves authorized HTTP transfer between isolated Engine databases and durable streams.
func TestRemoteWebhookDelivery(t *testing.T) {
	f := newFixture(t)
	_, err := f.client.Subscribe(t.Context(), f.registration, uuid.New(), "unexchanged-token")
	require.Error(t, err)
	otherID := seedInstallation(t, f.brokerDB, "other-installation")
	other, err := webhookrelay.NewClient(f.serverURL, staticToken("other-installation"), nil)
	require.NoError(t, err)
	_, err = other.Subscribe(t.Context(), f.registration, uuid.New(), "provider-access")
	require.Error(t, err)
	require.NoError(t, f.receiver.Step(t.Context()))
	var subscription uuid.UUID
	require.NoError(t, f.consumerDB.QueryRow(t.Context(), `SELECT subscription_id FROM fused_webhook_receivers`).Scan(&subscription))
	require.NotEqual(t, uuid.Nil, subscription)
	_, err = other.Pull(t.Context(), subscription)
	require.Error(t, err)
	f.publish(t, "foreign-resource", "event-foreign")
	f.publish(t, "team-verified", "event-one")
	require.NoError(t, f.receiver.Step(t.Context()))
	assertMessageCount(t, f.consumerJS, 1)
	msg, err := f.consumerJS.GetMsg("WEBHOOKS", 1)
	require.NoError(t, err)
	require.Equal(t, "broker-verified", msg.Header.Get("X-Fused-Webhook-Provenance"))
	require.Contains(t, msg.Subject, f.account.String())
	// Repeated provider identity must not invoke downstream subscribers twice after durable receipt storage.
	f.publish(t, "team-verified", "event-one")
	require.NoError(t, f.receiver.Step(t.Context()))
	assertMessageCount(t, f.consumerJS, 1)
	// Recreating the worker retains the broker cursor and local deduplication state.
	restarted := &webhookrelay.Receiver{DB: f.consumerDB, JS: f.consumerJS, Client: f.client, Key: f.key}
	f.publish(t, "team-verified", "event-two")
	require.NoError(t, restarted.Step(t.Context()))
	assertMessageCount(t, f.consumerJS, 2)
	require.Error(t, other.Ack(t.Context(), subscription, "forged"))
	require.Error(t, f.client.Ack(t.Context(), subscription, "forged"))
	_, err = f.broker.Proof.Authorize(t.Context(), otherID, subscription)
	require.Error(t, err)
	// Installation revocation must stop already-established subscriptions before the next provider payload is released.
	_, err = f.brokerDB.Exec(t.Context(), `UPDATE fused_managed_auth_installations SET revoked_at=NOW() WHERE id=$1`, f.installation)
	require.NoError(t, err)
	f.publish(t, "team-verified", "event-after-revoke")
	require.Error(t, restarted.Step(t.Context()))
	assertMessageCount(t, f.consumerJS, 2)
}

// TestRemoteWebhookDisconnectWithdrawsSubscription covers durable remote cleanup after local connection deletion.
func TestRemoteWebhookDisconnectWithdrawsSubscription(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.receiver.Step(t.Context()))
	var id uuid.UUID
	require.NoError(t, f.consumerDB.QueryRow(t.Context(), `SELECT subscription_id FROM fused_webhook_receivers`).Scan(&id))
	_, err := f.consumerDB.Exec(t.Context(), `DELETE FROM fused_auth_connections WHERE id=$1`, f.connection)
	require.NoError(t, err)
	require.NoError(t, f.receiver.Step(t.Context()))
	_, err = f.client.Pull(t.Context(), id)
	require.Error(t, err)
	var count int
	require.NoError(t, f.consumerDB.QueryRow(t.Context(), `SELECT count(*) FROM fused_webhook_receivers`).Scan(&count))
	require.Zero(t, count)
}

// TestRemoteWebhookPolicyChangeRevokesProof rejects reuse of verified resource proof after routing-policy changes.
func TestRemoteWebhookPolicyChangeRevokesProof(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.receiver.Step(t.Context()))
	_, err := f.brokerDB.Exec(t.Context(), `UPDATE fused_workspace_webhooks SET relay_config=jsonb_set(relay_config,'{publish,event_resource_path}','"other_team"') WHERE id=$1`, f.registration)
	require.NoError(t, err)
	f.publish(t, "team-verified", "event-after-policy-change")
	require.Error(t, f.receiver.Step(t.Context()))
	assertMessageCount(t, f.consumerJS, 0)
}

// newFixture connects real broker handlers and the consumer worker, with only the external provider represented by a TLS fixture.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{brokerDB: isolatedPool(t), consumerDB: isolatedPool(t), service: uuid.New(), version: uuid.New(), registration: uuid.New(), localRegistration: uuid.New(), account: uuid.New(), key: bytes.Repeat([]byte{7}, 32)}
	var nc *nats.Conn
	f.brokerJS, nc = newStream(t)
	f.consumerJS, _ = newStream(t)
	seedWorkspace(t, f.brokerDB, uuid.New())
	seedWorkspace(t, f.consumerDB, f.account)
	f.installation = seedInstallation(t, f.brokerDB, "installation-access")
	seedRegistrations(t, f)
	proof := webhookrelay.ProofStore{DB: f.brokerDB}
	f.broker = &webhookrelay.Broker{Proof: proof, JS: f.brokerJS, Conn: nc, Key: f.key}
	provider := httptest.NewTLSServer(http.HandlerFunc(providerResponse))
	t.Cleanup(provider.Close)
	connect, err := managedauthbroker.NewConnectService(providerCatalog{provider.URL}, f.key, provider.Client())
	require.NoError(t, err)
	connect.Proof = &proof
	router := chi.NewRouter()
	installs := managedauthbroker.NewStore(f.brokerDB)
	managedauthbroker.MountConnectRoutes(router, connect, installs)
	managedauthbroker.MountWebhookRoutes(router, f.broker, installs)
	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)
	f.serverURL = httpServer.URL
	exchangeFixtureToken(t, f)
	proofID, proofErr := proof.Subscribe(t.Context(), f.installation, f.registration, uuid.New(), "provider-access")
	require.NoError(t, proofErr)
	_, proofErr = f.broker.Pull(t.Context(), f.installation, proofID)
	require.NoError(t, proofErr)
	f.client, err = webhookrelay.NewClient(httpServer.URL, staticToken("installation-access"), nil)
	require.NoError(t, err)
	f.receiver = &webhookrelay.Receiver{DB: f.consumerDB, JS: f.consumerJS, Client: f.client, Key: f.key}
	return f
}

// providerResponse issues resource claims only after validating the application's own secret and authorization code.
func providerResponse(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	// Wrong client credentials or code cannot establish provider ownership proof.
	if r.Form.Get("client_secret") != "test-secret" || r.Form.Get("code") != "valid-code" {
		w.WriteHeader(400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"provider-access","refresh_token":"provider-refresh","team":{"id":"team-verified"},"app_id":"app-verified"}`))
}

// exchangeFixtureToken checks that the broker retains proof while returning only the existing standard token fields.
func exchangeFixtureToken(t *testing.T, f *fixture) {
	t.Helper()
	request, err := http.NewRequest("POST", f.serverURL+"/managed-auth/broker/connect/"+f.service.String()+"/oauth/exchange", strings.NewReader(`{"code":"valid-code","redirect_uri":"https://consumer.example/callback"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer installation-access")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	var token map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&token))
	require.Equal(t, "provider-access", token["access_token"])
	require.NotContains(t, token, "team")
	require.NotContains(t, token, "app_id")
	require.NotContains(t, token, "client_secret")
}

// seedRegistrations stores canonical registration rows and a consumer-owned encrypted provider token.
func seedRegistrations(t *testing.T, f *fixture) {
	t.Helper()
	bucket := uuid.New()
	_, err := f.brokerDB.Exec(t.Context(), `INSERT INTO fused_buckets(id,name) VALUES($1,'broker')`, bucket)
	require.NoError(t, err)
	verification, _ := json.Marshal(testcontract.AuthenticatedSignature())
	policy := `{"publish":{"auth_name":"oauth","token_resource_path":"team.id","token_app_path":"app_id","event_resource_path":"team_id","event_app_path":"api_app_id","event_id_path":"event_id"}}`
	_, err = f.brokerDB.Exec(t.Context(), `INSERT INTO fused_workspace_webhooks(id,service_id,service_version_id,label,slug,signature_policy,secret_ref,secret_bucket_id,owning_config_key,relay_config) VALUES($1,$2,$3,'broker','broker-test',$6,'${bucket.broker.secret.signing}',$4,'webhook:broker',$5)`, f.registration, f.service, f.version, bucket, policy, verification)
	require.NoError(t, err)
	_, err = f.brokerDB.Exec(t.Context(), `INSERT INTO fused_oauth_publications(service_id,auth_name,bucket_id,service_version_id,flow_name,registration_hash) VALUES($1,'oauth',$2,$3,'authorizationCode','fixture-pinned')`, f.service, bucket, f.version)
	require.NoError(t, err)
	_, err = f.consumerDB.Exec(t.Context(), `INSERT INTO fused_buckets(id,name) VALUES($1,'consumer')`, bucket)
	require.NoError(t, err)
	wrapped, dek, err := store.WrapDEK(f.key)
	require.NoError(t, err)
	encrypted, err := store.EncryptWithDEK(dek, "provider-access")
	require.NoError(t, err)
	conn, err := store.NewPostgresStore(f.consumerDB).UpsertAuthConnection(t.Context(), store.AuthConnection{BucketID: bucket, ServiceID: f.service, ServiceVersionID: f.version, EndUserRef: "user", AuthType: "oauth", AuthName: "oauth", CredentialSourceServiceID: f.service, CredentialSourceAuthType: "oauth", CredentialSourceAuthName: "oauth", ManagedAuth: true, EncryptedDEK: wrapped, EncryptedAccessToken: encrypted})
	require.NoError(t, err)
	f.connection = conn.ID
	raw, err := json.Marshal(webhookrelay.Config{Source: &webhookrelay.Source{Bucket: "consumer", ConnectionID: f.connection, RegistrationID: f.registration}})
	require.NoError(t, err)
	_, err = f.consumerDB.Exec(t.Context(), `INSERT INTO fused_workspace_webhooks(id,service_id,service_version_id,label,slug,auth_type,owning_config_key,relay_config) VALUES($1,$2,$3,'consumer','consumer-test','fused_remote','webhook:consumer',$4)`, f.localRegistration, f.service, f.version, raw)
	require.NoError(t, err)
}

// seedInstallation uses the actual installation store and hashed bearer lookup for all HTTP authorization tests.
func seedInstallation(t *testing.T, pool *pgxpool.Pool, access string) uuid.UUID {
	t.Helper()
	identity := managedauthbroker.EnrollmentIdentity{AccountID: uuid.New(), InstallationID: uuid.New()}
	err := managedauthbroker.NewStore(pool).IssueInstallation(t.Context(), identity, []string{"managed_auth:connect"}, webhookrelay.Digest(access), webhookrelay.Digest(uuid.NewString()), uuid.New(), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT id FROM fused_managed_auth_installations WHERE engine_installation_id=$1`, identity.InstallationID).Scan(&id))
	return id
}

// seedWorkspace gives each test Engine a distinct downstream account audience.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool, account uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `INSERT INTO fused_workspaces(account_id,name,slug) VALUES($1,'relay-test','relay-test')`, account)
	require.NoError(t, err)
}

// publish exercises the existing post-verification stream boundary; signature verification has independent ingress coverage.
func (f *fixture) publish(t *testing.T, resource, eventID string) {
	t.Helper()
	var account uuid.UUID
	require.NoError(t, f.brokerDB.QueryRow(t.Context(), `SELECT account_id FROM fused_workspaces`).Scan(&account))
	msg := nats.NewMsg(fmt.Sprintf("webhooks.%s.%s.broker.app_mention", account, f.service))
	msg.Header.Set("X-Fused-Webhook-ID", f.registration.String())
	msg.Data = []byte(fmt.Sprintf(`{"body":{"team_id":%q,"api_app_id":"app-verified","event_id":%q,"event":{"type":"app_mention"}},"headers":{},"query":{},"path":{}}`, resource, eventID))
	_, err := f.brokerJS.PublishMsg(msg)
	require.NoError(t, err)
}

// assertMessageCount verifies durable consumer storage independently of worker status.
func assertMessageCount(t *testing.T, js nats.JetStreamContext, want uint64) {
	t.Helper()
	info, err := js.StreamInfo("WEBHOOKS")
	require.NoError(t, err)
	require.Equal(t, want, info.State.Msgs)
}

// newStream gives each Engine its own file-backed durable broker and acknowledgement state.
func newStream(t *testing.T) (nats.JetStreamContext, *nats.Conn) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second))
	t.Cleanup(s.Shutdown)
	nc, err := nats.Connect(s.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&nats.StreamConfig{Name: "WEBHOOKS", Subjects: []string{"webhooks.>"}, Storage: nats.FileStorage, MaxAge: 30 * 24 * time.Hour})
	require.NoError(t, err)
	return js, nc
}

// isolatedPool applies production DDL in a UUID schema while preserving every live test database row.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	// Integration tests require an explicitly selected local or CI PostgreSQL server.
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := "fused_relay_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(t.Context(), "CREATE SCHEMA "+identifier)
	require.NoError(t, err)
	// Cleanup is restricted to this test's own generated schema.
	t.Cleanup(func() {
		_, cleanupErr := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	u, err := url.Parse(databaseURL)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.InitEnginePostgres(t.Context(), u.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestRemoteWebhookRefreshPreservesProof rotates possession hashes without changing the provider resource or permitting a sibling installation to claim it.
func TestRemoteWebhookRefreshPreservesProof(t *testing.T) {
	f := newFixture(t)
	other := seedInstallation(t, f.brokerDB, "refresh-other")
	require.NoError(t, f.broker.Proof.RecordRefresh(t.Context(), other, f.service, "oauth", "provider-refresh", "foreign-access", "foreign-refresh"))
	_, err := f.broker.Proof.Subscribe(t.Context(), other, f.registration, uuid.New(), "foreign-access")
	require.Error(t, err)
	require.NoError(t, f.broker.Proof.RecordRefresh(t.Context(), f.installation, f.service, "oauth", "provider-refresh", "rotated-access", "rotated-refresh"))
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "provider-access")
	require.Error(t, err)
	id, err := f.client.Subscribe(t.Context(), f.registration, uuid.New(), "rotated-access")
	require.NoError(t, err)
	authorization, err := f.broker.Proof.Authorize(t.Context(), f.installation, id)
	require.NoError(t, err)
	require.Equal(t, "team-verified", authorization.ResourceID)
	require.Equal(t, "app-verified", authorization.AppID)
}

// TestRemoteWebhookSubscriptionQuotaIsBounded prevents one authenticated installation from creating unbounded JetStream consumers.
func TestRemoteWebhookSubscriptionQuotaIsBounded(t *testing.T) {
	f := newFixture(t)
	_, err := f.brokerDB.Exec(t.Context(), `INSERT INTO fused_webhook_subscriptions(grant_id,receiver_id)
 SELECT g.id,gen_random_uuid() FROM fused_webhook_grants g CROSS JOIN generate_series(1,127) WHERE g.installation_id=$1`, f.installation)
	require.NoError(t, err)
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "provider-access")
	require.Error(t, err)
}

// TestMultipleApplicationWebhookProofs pins proof and revocation to one application even when provider routing claims coincide.
func TestMultipleApplicationWebhookProofs(t *testing.T) {
	f := newFixture(t)
	application, registration := uuid.NewString(), uuid.New()
	_, err := f.brokerDB.Exec(t.Context(), `INSERT INTO fused_oauth_publications(service_id,auth_name,bucket_id,service_version_id,flow_name,registration_hash,application_id)
 SELECT service_id,auth_name,bucket_id,service_version_id,flow_name,'second-app-contract',$2 FROM fused_oauth_publications WHERE service_id=$1 AND application_id=''`, f.service, application)
	require.NoError(t, err)
	_, err = f.brokerDB.Exec(t.Context(), `INSERT INTO fused_workspace_webhooks(id,service_id,service_version_id,label,slug,signature_policy,secret_ref,secret_bucket_id,owning_config_key,relay_config)
 SELECT $2,service_id,service_version_id,'second-app','second-app',signature_policy,secret_ref,secret_bucket_id,'webhook:second-app',jsonb_set(relay_config,'{publish,managed_application_id}',to_jsonb($3::text)) FROM fused_workspace_webhooks WHERE id=$1`, f.registration, registration, application)
	require.NoError(t, err)
	raw := []byte(`{"team":{"id":"team-verified"},"app_id":"app-verified"}`)
	require.NoError(t, f.broker.Proof.RecordExchange(t.Context(), f.installation, f.service, "oauth", "default-access", "shared-refresh", raw))
	_, err = f.client.Subscribe(t.Context(), registration, uuid.New(), "default-access", application)
	require.Error(t, err)
	require.NoError(t, f.broker.Proof.RecordExchange(t.Context(), f.installation, f.service, "oauth", "named-access", "shared-refresh", raw, application))
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "named-access")
	require.Error(t, err)
	// A legacy/default receiver cannot consume a named application's proof.
	_, err = f.client.Subscribe(t.Context(), registration, uuid.New(), "named-access")
	require.Error(t, err)
	assertNamedReceiverSubscription(t, f, registration, application, "named-access")
	subscription, err := f.client.Subscribe(t.Context(), registration, uuid.New(), "named-access", application)
	require.NoError(t, err)
	// Even coincident refresh strings cannot rotate a proof for another application.
	require.NoError(t, f.broker.Proof.RecordRefresh(t.Context(), f.installation, f.service, "oauth", "shared-refresh", "named-rotated", "", application))
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "default-access")
	require.NoError(t, err)
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "named-rotated")
	require.Error(t, err)
	_, err = f.client.Subscribe(t.Context(), registration, uuid.New(), "named-rotated", application)
	require.NoError(t, err)
	_, err = f.brokerDB.Exec(t.Context(), `UPDATE fused_oauth_publications SET allow_all_enrolled=false WHERE service_id=$1 AND application_id=$2`, f.service, application)
	require.NoError(t, err)
	_, err = f.client.Pull(t.Context(), subscription)
	require.Error(t, err)
	_, err = f.client.Subscribe(t.Context(), registration, uuid.New(), "named-rotated", application)
	require.Error(t, err)
	// Revoking one publication leaves the separately authorized default proof usable.
	_, err = f.client.Subscribe(t.Context(), f.registration, uuid.New(), "default-access")
	require.NoError(t, err)
}

// assertNamedReceiverSubscription drives the real consumer worker using the application identity saved on its connection.
func assertNamedReceiverSubscription(t *testing.T, f *fixture, registration uuid.UUID, application, token string) {
	t.Helper()
	wrapped, dek, err := store.WrapDEK(f.key)
	require.NoError(t, err)
	encrypted, err := store.EncryptWithDEK(dek, token)
	require.NoError(t, err)
	_, err = f.consumerDB.Exec(t.Context(), `UPDATE fused_auth_connections SET managed_application_id=$2,encrypted_dek=$3,access_token=$4 WHERE id=$1`, f.connection, application, wrapped, encrypted)
	require.NoError(t, err)
	_, err = f.consumerDB.Exec(t.Context(), `UPDATE fused_workspace_webhooks SET relay_config=jsonb_set(relay_config,'{source,registration_id}',to_jsonb($2::text)) WHERE id=$1`, f.localRegistration, registration.String())
	require.NoError(t, err)
	require.NoError(t, f.receiver.Step(t.Context()))
	var subscription uuid.UUID
	require.NoError(t, f.consumerDB.QueryRow(t.Context(), `SELECT subscription_id FROM fused_webhook_receivers WHERE registration_id=$1`, f.localRegistration).Scan(&subscription))
	authorization, err := f.broker.Proof.Authorize(t.Context(), f.installation, subscription)
	require.NoError(t, err)
	require.Equal(t, application, authorization.Policy.ManagedApplicationID)
}
