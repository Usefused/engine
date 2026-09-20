// Command managedauthprocess prepares and verifies disposable databases for two real Engine processes.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
)

// main exposes fixture preparation separately from the unmodified Engine executable.
func main() {
	// NATS instances use distinct loopback ports and storage, matching independent deployments.
	if os.Getenv("FIXTURE_MODE") == "nats" {
		serveNATS()
		return
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	// Guard the destructive fixture helper against any non-disposable database.
	var database string
	must(pool.QueryRow(ctx, "SELECT current_database()").Scan(&database))
	if database != "fused_process_broker_test" && database != "fused_process_consumer_test" {
		log.Fatal("refusing non-fixture database")
	}
	// Verification reads actual durable grants without substituting a runtime implementation.
	if os.Getenv("FIXTURE_MODE") == "verify" {
		verify(ctx, pool)
		return
	}
	// Slack acceptance uses its provider success flag instead of the Google userinfo contract.
	if os.Getenv("FIXTURE_MODE") == "verify-slack" {
		verifySlack(ctx, pool)
		return
	}
	// Runtime acceptance adds reviewed operations without reseeding credentials or connections.
	if os.Getenv("FIXTURE_MODE") == "catalog" {
		installRuntimeCatalog(ctx, pool)
		return
	}
	seed(ctx, pool)
}

// must stops setup at the first failed invariant without printing credential values.
func must(err error) {
	// Failed setup cannot count as a successful integration check.
	if err != nil {
		log.Fatal(err)
	}
}

// serveNATS isolates event buses for the broker and consumer processes.
func serveNATS() {
	for index, port := range []int{14222, 14223} {
		n, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: fmt.Sprintf("%s/nats-%d", os.Getenv("FIXTURE_DIR"), index)})
		must(err)
		n.Start()
		// Engines must not race the fixture message bus startup.
		if !n.ReadyForConnections(10 * time.Second) {
			log.Fatal("NATS startup failed")
		}
	}
	log.Print("two isolated NATS listeners ready")
	select {}
}

// seed installs reviewed provider metadata through the production snapshot and bucket stores.
func seed(ctx context.Context, pool *pgxpool.Pool) {
	var metadata fusedobject.ServiceMetadata
	raw, err := os.ReadFile(os.Getenv("METADATA_FILE"))
	must(err)
	must(json.Unmarshal(raw, &metadata))
	// Runtime envelopes are intentionally excluded from nested metadata JSON.
	metadata.ExecutionContractEnvelope = fusedobject.EngineExecutionContractSupport()
	runtime := store.NewPostgresStore(pool)
	bucket, err := runtime.GetBucketByName(ctx, fixtureSlug())
	// Re-running fixture preparation reuses its own bucket after an interrupted setup.
	if err != nil {
		bucket, err = runtime.CreateBucket(ctx, fixtureSlug(), false)
	}
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_services(service_id,service_slug,service_name) VALUES($1,$2,$3) ON CONFLICT(service_id) DO NOTHING`, metadata.ID, fixtureSlug(), metadata.Name)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO fused_workspace_service_versions(service_id,service_version_id,version) VALUES($1,$2,'1.0.0') ON CONFLICT(service_id,service_version_id) DO NOTHING`, metadata.ID, metadata.ServiceVersionID)
	must(err)
	_, err = runtime.(store.ServiceContractSnapshotStore).UpsertServiceContractSnapshot(ctx, store.ServiceContractSnapshot{ExecutionContractEnvelope: metadata.ExecutionContractEnvelope, ServiceID: metadata.ID, ServiceVersionID: metadata.ServiceVersionID, Version: "1.0.0", ServiceMetadata: metadata})
	must(err)
	// Only the broker fixture receives the provider application credentials.
	if os.Getenv("FIXTURE_MODE") == "broker" {
		savePair(ctx, runtime, bucket.ID, metadata.ID)
	}
	must(os.WriteFile(os.Getenv("BUCKET_OUTPUT_FILE"), []byte(bucket.ID.String()), 0600))
	log.Print("canonical fixture metadata and bucket ready")
}

// savePair uses ordinary encrypted bucket records, never a parallel credential catalogue.
func savePair(ctx context.Context, runtime store.Store, bucket, service uuid.UUID) {
	idKey, secretKey, ok := credentialkeys.OAuthApplication("oauth2")
	// Fixture typos cannot silently write unrelated API-key records.
	if !ok {
		log.Fatal("invalid OAuth scheme")
	}
	var rows []store.WorkspaceSecret
	for _, pair := range []struct{ key, value string }{{idKey, fixtureCredential("CLIENT_ID", "FUSED_GOOGLE_OAUTH_ID")}, {secretKey, fixtureCredential("CLIENT_SECRET", "FUSED_GOOGLE_OAUTH_SECRET")}} {
		// Missing interactive input must fail before publication.
		if pair.value == "" {
			log.Fatal("missing provider application credential")
		}
		wrapped, dek, err := store.WrapDEK(masterKey())
		must(err)
		encrypted, err := store.EncryptWithDEK(dek, pair.value)
		must(err)
		rows = append(rows, store.WorkspaceSecret{WorkspaceSecretMeta: store.WorkspaceSecretMeta{BucketID: bucket, ServiceID: service, KeyName: pair.key, CredentialType: "oauth"}, EncryptedDEK: wrapped, EncryptedValue: encrypted})
	}
	must(runtime.UpsertSecrets(ctx, rows))
}

// verify proves the consumer's encrypted grant works against Google's real userinfo endpoint.
func verify(ctx context.Context, pool *pgxpool.Pool) {
	var id uuid.UUID
	must(pool.QueryRow(ctx, `SELECT id FROM fused_auth_connections WHERE end_user_ref='google-process-test'`).Scan(&id))
	runtime := store.NewPostgresStore(pool).(store.AuthConnectionRefreshStore)
	conn, err := runtime.GetAuthConnectionByID(ctx, id)
	must(err)
	// Both encrypted provider tokens and delegated source identity must survive persistence.
	if !conn.ManagedAuth || conn.EncryptedRefreshToken == "" || conn.EncryptedAccessToken == "" {
		log.Fatal("missing managed connection material")
	}
	dek, err := store.UnwrapDEK(masterKey(), conn.EncryptedDEK)
	must(err)
	access, err := store.DecryptWithDEK(dek, conn.EncryptedAccessToken)
	must(err)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	must(err)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	must(err)
	response.Body.Close()
	// The provider response body contains identity data and is deliberately never printed.
	if response.StatusCode != 200 {
		log.Fatalf("Google userinfo HTTP %d", response.StatusCode)
	}
	log.Print("encrypted consumer grant: Google userinfo HTTP 200")
}

// masterKey uses the same base64 encryption-key contract as Engine startup.
func masterKey() []byte {
	key, err := base64.StdEncoding.DecodeString(os.Getenv("FUSED_ENCRYPTION_KEY"))
	must(err)
	return key
}

// fixtureSlug lets provider fixtures share the same setup without changing runtime routing.
func fixtureSlug() string {
	// Existing Google acceptance commands retain their original default identity.
	if slug := os.Getenv("FIXTURE_SERVICE_SLUG"); slug != "" {
		return slug
	}
	return "google-process"
}

// fixtureCredential preserves the original Google test inputs while accepting a provider-neutral pair.
func fixtureCredential(field, fallback string) string {
	// The explicit pair takes precedence so separate provider fixtures cannot mix credentials.
	if value := os.Getenv("FUSED_TEST_OAUTH_" + field); value != "" {
		return value
	}
	return os.Getenv(fallback)
}
