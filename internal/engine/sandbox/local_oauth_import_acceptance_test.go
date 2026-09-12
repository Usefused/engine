package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

type localOAuthAcceptanceVersion struct {
	ServiceID        uuid.UUID `json:"service_id"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	Version          string    `json:"version"`
	Slug             string    `json:"slug"`
}

// TestLocalOAuthImportedContractDispatch consumes the companion Registry's real GraphQL results, persists snapshots, and sends only synthetic tokens to loopback.
func TestLocalOAuthImportedContractDispatch(t *testing.T) {
	directory := os.Getenv("FUSED_LOCAL_IMPORT_DIR")
	// Provider acceptance is explicitly opt-in; normal tests remain independent of external source corpora.
	if directory == "" {
		t.Skip("FUSED_LOCAL_IMPORT_DIR not set")
	}
	parsedURL, err := url.Parse(os.Getenv("DATABASE_URL"))
	// The retained connection fixture belongs only in a disposable local PostgreSQL database.
	if err != nil || (parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "localhost") {
		t.Fatal("local DATABASE_URL required")
	}
	var ready struct {
		URL      string                        `json:"url"`
		Versions []localOAuthAcceptanceVersion `json:"versions"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "registry-ready.json"))
	// Registry must finish real imports before the Engine side can request immutable versions.
	if err != nil {
		t.Fatal(err)
	}
	// Invalid handoff data cannot select remote services or guessed identities.
	if err := json.Unmarshal(data, &ready); err != nil {
		t.Fatal(err)
	}
	registryURL, err := url.Parse(ready.URL)
	// No real license credential is used by this local GraphQL test bridge.
	if err != nil || registryURL.Hostname() != "127.0.0.1" {
		t.Fatal("Registry bridge must be loopback")
	}
	t.Setenv("FUSED_ENV", "development")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, os.Getenv("DATABASE_URL"))
	// Production schema initialization is part of the PostgreSQL acceptance boundary.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := store.NewPostgresStore(pool)
	snapshots := repository.(store.ServiceContractSnapshotStore)
	registry := NewHTTPRegistryClient(ready.URL, "synthetic-local-registry-license")
	bucketID := uuid.New()
	// A real bucket anchors encrypted connected credentials without borrowing any user's secrets.
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, bucketID, "local-oauth-acceptance-"+bucketID.String()); err != nil {
		t.Fatal(err)
	}
	resolver := NewSecretResolver(repository, dispatchProfileMasterKey).(*secretResolver)
	for _, version := range ready.Versions {
		snapshot, err := registry.FetchRuntimeContract(ctx, version.ServiceID, version.ServiceVersionID, version.Version, "")
		// This traverses Registry SQL, GraphQL projection, HTTP decoding, and Engine runtime admission.
		if err != nil {
			t.Fatalf("%s fetch: %v", version.Slug, err)
		}
		// Persist unchanged imported routing; loopback is selected later through the normal runtime override.
		if _, err := snapshots.UpsertServiceContractSnapshot(ctx, *snapshot); err != nil {
			t.Fatal(err)
		}
		metadata, err := snapshots.GetServiceContractMetadata(ctx, version.ServiceID, version.ServiceVersionID)
		// Dispatch must reload PostgreSQL JSONB rather than reuse the in-memory import result.
		if err != nil {
			t.Fatal(err)
		}
		// The exact overlay provenance and custom header must survive both databases.
		if len(metadata.AuthConfigs) != 1 || metadata.AuthConfigs[0].OAuthTokenPlacement == nil || metadata.AuthConfigs[0].PolicyProvenance["oauth_token_placement"] != "reviewed_overlay" {
			t.Fatal("imported OAuth placement lost")
		}
		operations, err := snapshots.ListServiceContractOperations(ctx, version.ServiceID, version.ServiceVersionID)
		// Every persisted GET can exercise wire delivery without any mutating provider operation.
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		for _, operation := range operations {
			// Body-bearing writes are outside this credential-delivery smoke test.
			if operation.Method != http.MethodGet {
				continue
			}
			for _, token := range []string{"synthetic-connected-token-one", "synthetic-connected-token-two"} {
				expires := time.Now().Add(time.Hour)
				encrypted := dispatchEncrypt(t, token)
				_, err := repository.UpsertAuthConnection(ctx, store.AuthConnection{BucketID: bucketID, ServiceID: version.ServiceID, ServiceVersionID: version.ServiceVersionID, EndUserRef: "local-acceptance-user", AuthType: "oauth", AuthName: metadata.AuthConfigs[0].Name, EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0], TokenType: "Bearer", RefreshState: "ok", ExpiresAt: &expires})
				// Replacing the connected token proves dispatch resolves current encrypted state on each call.
				if err != nil {
					t.Fatal(err)
				}
				credentials := map[string]any{"fused_end_user_ref": "local-acceptance-user", "fused_auth_type": "oauth", "fused_auth_name": metadata.AuthConfigs[0].Name}
				// Use the production connected-token resolver and its exact scheme selection.
				if err := resolver.resolveConnectedAuth(ctx, bucketID, version.ServiceID, metadata.AuthConfigs, operation.SecurityRequirements, credentials); err != nil {
					t.Fatal(err)
				}
				service, endpoint := fusedToService(metadata), fusedToIntegrationObject(metadata, operation)
				params := map[string]any{}
				for _, parameter := range endpoint.Parameters {
					// Synthetic values preserve imported parameter serialization while avoiding customer data.
					if !parameter.Required && parameter.In != "path" {
						continue
					}
					params[parameter.Name] = "test-value"
					// Arrays must reach the serializer in their declared JSON shape.
					if parameter.Type == "array" {
						params[parameter.Name] = []any{"test-value"}
					}
				}
				var received atomic.Bool
				// The fake provider checks the current decrypted credential and rejects default-header duplication.
				vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received.Store(true)
					// This is the wire assertion, independent of the Engine's internal header mapping.
					if r.Header.Get(metadata.AuthConfigs[0].OAuthTokenPlacement.Name) != token || r.Header.Get("Authorization") != "" {
						t.Error("incorrect provider token delivery")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				_, err = engine.NewDispatcher().ExecuteStream(ctx, service, endpoint, params, credentials, []store.BucketValue{{Location: "base_url", Value: vendor.URL, Mode: "force", SourceKind: "connection_resource"}}, engine.NewBufferStream())
				vendor.Close()
				// Both successful dispatch and actual loopback delivery are required; no call may reach Amazon.
				if err != nil || !received.Load() {
					t.Fatalf("%s %s dispatch: %v received=%v", version.Slug, operation.Name, err, received.Load())
				}
				calls++
			}
		}
		t.Logf("%s: %d persisted operations, %d authenticated HTTP calls with two successive tokens", version.Slug, len(operations), calls)
	}
	// Emit success only after every imported service completes its PostgreSQL and wire assertions.
	if !t.Failed() {
		if err := os.WriteFile(filepath.Join(directory, "engine-complete.json"), []byte(`{"passed":true}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
