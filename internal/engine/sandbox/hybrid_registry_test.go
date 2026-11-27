package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestHybridRegistryClientStrictFallbackAndPermissionGate verifies Local-first hits, permission-gated Cloud fallback, and Enterprise revocation.
func TestHybridRegistryClientStrictFallbackAndPermissionGate(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	localID := uuid.New()
	cloudID := uuid.New()
	versionID := uuid.New()
	var localHits, cloudHits atomic.Int32

	localServer := newMockServiceRegistryServer(t, localID, "Local Payments", "local-payments", versionID, &localHits)
	defer localServer.Close()
	cloudServer := newMockServiceRegistryServer(t, cloudID, "Linear", "linear", versionID, &cloudHits)
	defer cloudServer.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
		Plan:                 "enterprise",
		LocalRegistryEnabled: true,
	})
	defer entitlement.LiveEntitlement.Reset()

	type cloudAuthKey struct{}
	client := NewHTTPRegistryClient(cloudServer.URL, "fk_com_ent")
	localClient := NewLocalHTTPRegistryClient(localServer.URL, "fk_com_ent")
	client.ConfigureHybridLocalRegistry(localClient, nil, func(ctx context.Context) bool {
		allowed, _ := ctx.Value(cloudAuthKey{}).(bool)
		return allowed
	})

	// 1. Local hit returns immediately without contacting Fused Cloud Registry.
	ctxLocal, cellLocal := WithRegistrySourceOut(context.WithValue(context.Background(), cloudAuthKey{}, true))
	name, _, _, _, err := client.VerifyServiceExists(ctxLocal, localID, "key")
	if err != nil || name != "Local Payments" || cellLocal.Value() != RegistrySourceLocal || cloudHits.Load() != 0 {
		t.Fatalf("local hit failed: name=%q err=%v source=%q cloudHits=%d", name, err, cellLocal.Value(), cloudHits.Load())
	}

	// 2. Local miss WITHOUT catalogue.cloud.read never contacts Fused Cloud Registry.
	ctxDenied := context.WithValue(context.Background(), cloudAuthKey{}, false)
	_, _, _, _, err = client.VerifyServiceExists(ctxDenied, cloudID, "key")
	if !errors.Is(err, ErrServiceNotFound) || cloudHits.Load() != 0 {
		t.Fatalf("expected ErrServiceNotFound with 0 cloud hits, got err=%v cloudHits=%d", err, cloudHits.Load())
	}

	// 3. Local miss WITH catalogue.cloud.read falls back to Fused Cloud Registry.
	ctxAllowed, cellCloud := WithRegistrySourceOut(context.WithValue(context.Background(), cloudAuthKey{}, true))
	name, _, _, _, err = client.VerifyServiceExists(ctxAllowed, cloudID, "key")
	if err != nil || name != "Linear" || cellCloud.Value() != RegistrySourceCloud || cloudHits.Load() != 1 {
		t.Fatalf("cloud fallback failed: name=%q err=%v source=%q cloudHits=%d", name, err, cellCloud.Value(), cloudHits.Load())
	}

	// 4. Revoking Enterprise entitlement blocks Local Registry access immediately.
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{
		Plan:                 "scale-up",
		LocalRegistryEnabled: false,
	})
	_, _, _, _, err = client.VerifyServiceExists(ctxAllowed, localID, "key")
	if !errors.Is(err, ErrLocalRegistryUnauthorized) {
		t.Fatalf("expected ErrLocalRegistryUnauthorized after downgrade, got %v", err)
	}
}

// newMockServiceRegistryServer serves slim GraphQL VerifyService responses for one owned service UUID.
func newMockServiceRegistryServer(t *testing.T, targetID uuid.UUID, name, slug string, versionID uuid.UUID, counter *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		var query struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&query)
		// Return null service when the queried UUID does not reside in this Registry.
		if query.Variables["id"] != targetID.String() {
			_, _ = w.Write([]byte(`{"data":{"service":null}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"service": map[string]any{
					"id":                      targetID.String(),
					"name":                    name,
					"slug":                    slug,
					"canonical_ref":           "@acme/" + slug,
					"current_service_version": "1.0.0",
					"service_versions": []map[string]any{
						{"id": versionID.String(), "name": "1.0.0"},
					},
				},
			},
		})
	}))
}
