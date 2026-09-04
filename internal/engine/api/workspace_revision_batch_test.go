package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

// TestResolveWorkspaceVersionsAcrossRegistryBatches exercises the real HTTP client through workspace resolution, including missing-version rejection.
func TestResolveWorkspaceVersionsAcrossRegistryBatches(t *testing.T) {
	for _, missing := range []bool{false, true} {
		// Both cases exceed the real Registry limit and differ only in one invisible version.
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			doc := workspaceConfigDocument{Services: map[string]workspaceConfigService{}}
			ids := map[uuid.UUID]uuid.UUID{}
			hidden := uuid.New()
			local := false
			baseURL := "https://api.example.test"
			for i := 0; i < 115; i++ {
				serviceID := uuid.New()
				// Give the omission a stable identity despite map iteration order.
				if i == 0 {
					serviceID = hidden
				}
				ids[serviceID] = uuid.New()
				doc.Services[serviceID.String()] = workspaceConfigService{ServiceID: serviceID.String(), Versions: []workspaceConfigServiceVersion{{Version: "v1", ExecutionPolicy: &workspaceExecutionPolicy{Public: &local, BaseURL: &baseURL}}}}
			}
			calls := 0
			// The fixture enforces the production request ceiling and visibility-filtered response contract.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				// Workspace resolution must use the authorized revision endpoint for every chunk.
				if r.URL.Path != "/integrations/versions/revisions" || r.Header.Get("X-API-Key") != "engine-license" {
					t.Error("wrong revision request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				var request struct {
					Versions []sandbox.ServiceVersionRef `json:"versions"`
				}
				// Bad input must not accidentally look like a valid empty Registry result.
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				// This rejection reproduces the observed deployed Engine failure before the fix.
				if len(request.Versions) > 50 {
					http.Error(w, "maximum of 50 service versions per request", http.StatusBadRequest)
					return
				}
				rows := []sandbox.ServiceVersionRevision{}
				for _, ref := range request.Versions {
					// An inaccessible version remains absent; Engine must not guess or substitute it.
					if missing && ref.ServiceID == hidden {
						continue
					}
					rows = append(rows, sandbox.ServiceVersionRevision{ServiceID: ref.ServiceID, Version: ref.Version, ServiceVersionID: ids[ref.ServiceID], IsPublic: true})
				}
				// Encoding failures are fixture failures, not evidence of correct client behavior.
				if err := json.NewEncoder(w).Encode(map[string]any{"versions": rows}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv("FUSED_ENV", "development")
			client := sandbox.NewHTTPRegistryClient(server.URL+"/graphql", "engine-license")
			err := resolveWorkspaceServiceVersions(context.Background(), client, "caller-secret", &doc)
			// Both complete and visibility-filtered responses must traverse all three bounded batches.
			if calls != 3 {
				t.Fatalf("expected 3 requests, got %d", calls)
			}
			// The existing planning guard must still reject the unresolved exact version.
			if missing {
				// A successful plan here would silently admit an incomplete catalogue.
				if err == nil || !strings.Contains(err.Error(), "has no exact service_version_id") {
					t.Fatalf("missing version was not rejected: %v", err)
				}
				return
			}
			// A complete catalogue should resolve without changing per-version local policy.
			if err != nil {
				t.Fatal(err)
			}
			for _, service := range doc.Services {
				version := service.Versions[0]
				serviceID := uuid.MustParse(service.ServiceID)
				// Identity, Registry visibility, and local routing must survive the batch merge together.
				if version.ServiceVersionID != ids[serviceID].String() || version.RegistryPublic == nil || !*version.RegistryPublic || version.ExecutionPolicy == nil || version.ExecutionPolicy.BaseURL == nil || *version.ExecutionPolicy.BaseURL != baseURL || version.ExecutionPolicy.Public == nil || *version.ExecutionPolicy.Public {
					t.Fatalf("version identity or policy changed: %#v", version)
				}
			}
		})
	}
}
