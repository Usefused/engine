package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type catalogAPIStore struct {
	store.Store
	active   bool
	snapshot *store.MCPCatalogSnapshot
	draft    *store.MCPCatalogDraft
	scope    store.MCPCatalogScope
	saves    int
}

// IsWorkspaceServiceVersionActive isolates endpoint admission from unrelated Registry behavior.
func (s *catalogAPIStore) IsWorkspaceServiceVersionActive(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return s.active, nil
}

// GetMCPCatalog supplies only the current test snapshot without querying provider state.
func (s *catalogAPIStore) GetMCPCatalog(context.Context, store.MCPCatalogScope) (*store.MCPCatalogSnapshot, error) {
	return s.snapshot, nil
}

// SaveMCPCatalogDraft records scope so the handler's actor binding can be asserted.
func (s *catalogAPIStore) SaveMCPCatalogDraft(_ context.Context, scope store.MCPCatalogScope, draft store.MCPCatalogDraft) error {
	s.scope = scope
	s.draft = &draft
	s.saves++
	return nil
}

// GetMCPCatalogDraft returns only the exact server-owned preview, never client-supplied definitions.
func (s *catalogAPIStore) GetMCPCatalogDraft(_ context.Context, _ store.MCPCatalogScope, id uuid.UUID) (*store.MCPCatalogDraft, error) {
	// Only a fixture-created exact draft can exercise the delayed-apply boundary.
	if s.draft != nil && s.draft.ID == id {
		return s.draft, nil
	}
	return nil, store.ErrMCPCatalogConflict
}

// ApplyMCPCatalogDraft fails if a handler tries to promote an unadmitted test draft.
func (s *catalogAPIStore) ApplyMCPCatalogDraft(context.Context, store.MCPCatalogScope, uuid.UUID) (*store.MCPCatalogSnapshot, error) {
	return nil, errors.New("unexpected apply")
}

// catalogTestActor constructs explicit service grants instead of granting an implicit owner bypass.
func catalogTestActor(t *testing.T, serviceID uuid.UUID, manage bool) accesscontrol.Actor {
	t.Helper()
	actor := accesscontrol.Actor{SubjectID: uuid.New(), WorkspaceID: uuid.New(), AccountID: uuid.New(), Kind: accesscontrol.SubjectUser}
	grants := []accesscontrol.Grant{{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}}}
	// Read-only test actors must be denied before discovery touches the network.
	if manage {
		grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.PermissionServiceManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}}, accesscontrol.Grant{Permission: accesscontrol.PermissionCatalogueImport, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	}
	var err error
	actor.Authorization, err = accesscontrol.NewAuthorizationSnapshot(1, grants...)
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

// catalogAPIRequest exercises chi path parsing with the same actor context used by authenticated Engine routes.
func catalogAPIRequest(handler http.HandlerFunc, actor *accesscontrol.Actor, serviceID, versionID uuid.UUID, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/services/{id}/versions/{version_id}/mcp-catalog", handler)
	request := httptest.NewRequest("POST", "/services/"+serviceID.String()+"/versions/"+versionID.String()+"/mcp-catalog", strings.NewReader(body))
	// Absence of an actor deliberately tests direct-handler authentication failure.
	if actor != nil {
		request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), *actor))
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// TestMCPCatalogHTTPAdmission proves authorization, active-version, strict-body, and failure boundaries precede persistence.
func TestMCPCatalogHTTPAdmission(t *testing.T) {
	// Exercise existing behavior only after the operator explicitly opts into imports.
	t.Setenv("FUSED_ENABLE_IMPORTED_MCP", "true")
	serviceID, versionID := uuid.New(), uuid.New()
	actor := catalogTestActor(t, serviceID, true)
	readOnly := catalogTestActor(t, serviceID, false)
	cases := []struct {
		name   string
		actor  *accesscontrol.Actor
		active bool
		body   string
		status int
	}{
		{"unauthenticated", nil, true, `{"url":"https://example.com/mcp"}`, 401},
		{"read only", &readOnly, true, `{"url":"https://example.com/mcp"}`, 403},
		{"inactive", &actor, false, `{"url":"https://example.com/mcp"}`, 404},
		{"unsafe URL", &actor, true, `{"url":"http://127.0.0.1/mcp"}`, 400},
		{"unknown property", &actor, true, `{"url":"https://example.com/mcp","token":"secret"}`, 400},
		{"trailing JSON", &actor, true, `{"url":"https://example.com/mcp"} {}`, 400},
		{"partial credential", &actor, true, `{"url":"https://example.com/mcp","bucket_name":"default"}`, 422},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := &catalogAPIStore{active: test.active}
			called := false
			// Any call in these denied cases is a security boundary regression.
			discover := func(context.Context, string, string) (mcpcatalog.Catalog, error) {
				called = true
				return mcpcatalog.Catalog{}, nil
			}
			response := catalogAPIRequest(discoverMCPCatalogHandler(s, nil, discover, catalogOwnerVerifier{owner: true}), test.actor, serviceID, versionID, test.body)
			// Denied requests must return the expected status before any network call or draft write.
			if response.Code != test.status || called || s.saves != 0 {
				t.Fatalf("status=%d body=%s called=%v saves=%d", response.Code, response.Body.String(), called, s.saves)
			}
		})
	}
}

// TestMCPCatalogHTTPDiscovery verifies discovery saves a private preview and scrubs upstream error prose.
func TestMCPCatalogHTTPDiscovery(t *testing.T) {
	// Exercise existing behavior only after the operator explicitly opts into imports.
	t.Setenv("FUSED_ENABLE_IMPORTED_MCP", "true")
	serviceID, versionID := uuid.New(), uuid.New()
	actor := catalogTestActor(t, serviceID, true)
	s := &catalogAPIStore{active: true}
	discover := func(context.Context, string, string) (mcpcatalog.Catalog, error) { return catalogBrowserFixture(), nil }
	response := catalogAPIRequest(discoverMCPCatalogHandler(s, nil, discover, catalogOwnerVerifier{owner: true}), &actor, serviceID, versionID, `{"url":"https://example.com/mcp"}`)
	// Discovery must preserve the complete catalog while leaving approval to the user.
	if response.Code != 200 || s.draft == nil || s.snapshot != nil || s.scope.SubjectID != actor.SubjectID || s.draft.Changes.Added != 4 {
		t.Fatalf("discovery=%d %s scope=%+v", response.Code, response.Body.String(), s.scope)
	}
	// Error prose is untrusted even when it comes from the network adapter.
	failed := func(context.Context, string, string) (mcpcatalog.Catalog, error) {
		return mcpcatalog.Catalog{}, errors.New("private-token https://private.test")
	}
	response = catalogAPIRequest(discoverMCPCatalogHandler(s, nil, failed, catalogOwnerVerifier{owner: true}), &actor, serviceID, versionID, `{"url":"https://example.com/mcp"}`)
	// Provider failure prose must stay out of responses and must not create a second preview.
	if response.Code != 500 || strings.Contains(response.Body.String(), "private-token") || s.saves != 1 {
		t.Fatalf("failure=%s saves=%d", response.Body.String(), s.saves)
	}
}

// catalogBrowserFixture provides all four categories for deterministic handler and browser coverage.
func catalogBrowserFixture() mcpcatalog.Catalog {
	return mcpcatalog.Catalog{ProtocolVersion: "2026-07-28", Server: mcpcatalog.ServerInfo{Name: "Project knowledge", Version: "1.0.0"}, Supported: map[string]bool{"tools": true, "prompts": true, "resources": true, "resource_templates": true},
		Tools:             []json.RawMessage{json.RawMessage(`{"name":"search_docs","title":"Search documentation","description":"Search the project knowledge base.","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]},"outputSchema":{"type":"object","properties":{"matches":{"type":"array"}}}}`)},
		Prompts:           []json.RawMessage{json.RawMessage(`{"name":"summarize_project","title":"Summarize a project","description":"Create a concise project summary.","arguments":[{"name":"project","required":true}]}`)},
		Resources:         []json.RawMessage{json.RawMessage(`{"name":"Project guide","description":"Getting started with the project.","uri":"docs://guide","mimeType":"text/markdown"}`)},
		ResourceTemplates: []json.RawMessage{json.RawMessage(`{"name":"Issue details","description":"Read the metadata for a project issue.","uriTemplate":"issues://{id}","mimeType":"application/json"}`)}}
}

// TestMCPCatalogBrowserPreview serves the actual UI component and Engine handlers against an explicitly disposable PostgreSQL fixture.
func TestMCPCatalogBrowserPreview(t *testing.T) {
	directory := os.Getenv("FUSED_MCP_UI_DIR")
	// The manual browser fixture is opt-in and never starts during the ordinary test suite.
	if directory == "" {
		t.Skip("FUSED_MCP_UI_DIR not set")
	}
	dsn := os.Getenv("DATABASE_URL")
	// Guard the dedicated fixture port so this helper cannot seed an ordinary development database.
	if !strings.Contains(dsn, "127.0.0.1:55489/") {
		t.Fatal("browser fixture requires the dedicated local test database")
	}
	pool, err := db.InitEnginePostgres(context.Background(), dsn)
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := store.NewPostgresStore(pool)
	serviceID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	versionID := uuid.MustParse("20000000-0000-4000-8000-000000000001")
	actor := catalogTestActor(t, serviceID, true)
	actor.SubjectID = uuid.MustParse("30000000-0000-4000-8000-000000000001")
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err = s.AddWorkspaceServiceVersion(context.Background(), serviceID, "fixture", "1", versionID, "Fixture service", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	// Authentication is a synthetic test actor; this listener never exposes real credentials or workspace data.
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(accesscontrol.ContextWithActor(r.Context(), actor)))
		})
	})
	base := "/workspace/services/{id}/versions/{version_id}/mcp-catalog"
	router.Get(base, GetMCPCatalogHandler(s))
	router.Post(base+"/apply", ApplyMCPCatalogHandler(s, catalogOwnerVerifier{owner: true}))
	// Endpoint names choose deterministic provider conditions while all persistence and API behavior remain real.
	discover := func(ctx context.Context, endpoint, token string) (mcpcatalog.Catalog, error) {
		if strings.Contains(endpoint, "unavailable") {
			return mcpcatalog.Catalog{}, mcpcatalog.ErrDiscovery
		}
		result := catalogBrowserFixture()
		// The requested fixture endpoint selects an intentional provider failure or capability subset.
		if strings.Contains(endpoint, "tools-only") {
			result.Supported["prompts"] = false
			result.Prompts = []json.RawMessage{}
			result.Supported["resources"] = false
			result.Supported["resource_templates"] = false
			result.Resources = []json.RawMessage{}
			result.ResourceTemplates = []json.RawMessage{}
		}
		return result, nil
	}
	router.Post(base+"/discover", discoverMCPCatalogHandler(s, nil, discover, catalogOwnerVerifier{owner: true}))
	router.Handle("/*", http.FileServer(http.Dir(directory)))
	server := &http.Server{Addr: "127.0.0.1:4179", Handler: router, ReadHeaderTimeout: 5 * time.Second}
	t.Log("Browser fixture available at http://127.0.0.1:4179")
	// The test process owns this listener and is stopped by the browser-test runner.
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

// catalogOwnerVerifier makes service ownership independent of the test actor's workspace grants.
type catalogOwnerVerifier struct {
	owner   bool
	missing bool
	err     error
}

// FetchServiceVisibility supplies only authoritative ownership metadata without accepting caller credentials.
func (v catalogOwnerVerifier) FetchServiceVisibility(_ context.Context, ids []uuid.UUID, _ string) (map[uuid.UUID]sandbox.ServiceVisibility, error) {
	out := map[uuid.UUID]sandbox.ServiceVisibility{}
	// Missing Registry results deliberately test fail-closed admission.
	if !v.missing {
		for _, id := range ids {
			out[id] = sandbox.ServiceVisibility{ServiceID: id, IsOwner: v.owner}
		}
	}
	return out, v.err
}

// TestMCPCatalogOwnerAdmission proves management grants alone cannot discover or apply catalogs.
func TestMCPCatalogOwnerAdmission(t *testing.T) {
	// Exercise existing behavior only after the operator explicitly opts into imports.
	t.Setenv("FUSED_ENABLE_IMPORTED_MCP", "true")
	serviceID, versionID := uuid.New(), uuid.New()
	actor := catalogTestActor(t, serviceID, true)
	cases := []struct {
		name     string
		verifier ServiceVisibilityResolver
		status   int
	}{
		{"non-owner", catalogOwnerVerifier{}, http.StatusForbidden},
		{"missing service", catalogOwnerVerifier{missing: true}, http.StatusForbidden},
		{"unavailable", catalogOwnerVerifier{err: errors.New("private registry detail")}, http.StatusServiceUnavailable},
		{"missing verifier", nil, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		// Both mutation endpoints must reject before reading a draft, contacting a provider, or writing a snapshot.
		t.Run(tc.name, func(t *testing.T) {
			s := &catalogAPIStore{active: true}
			called := false
			// Any provider call here would violate the owner-only mutation boundary.
			discover := func(context.Context, string, string) (mcpcatalog.Catalog, error) {
				called = true
				return mcpcatalog.Catalog{}, nil
			}
			handlers := []http.HandlerFunc{discoverMCPCatalogHandler(s, nil, discover, tc.verifier), ApplyMCPCatalogHandler(s, tc.verifier)}
			for _, handler := range handlers {
				response := catalogAPIRequest(handler, &actor, serviceID, versionID, `{"url":"https://example.com/mcp"}`)
				// Fixed error text protects Registry diagnostics as well as the ownership boundary.
				if response.Code != tc.status || called || s.saves != 0 || strings.Contains(response.Body.String(), "private registry detail") {
					t.Fatalf("status=%d response=%s called=%v saves=%d", response.Code, response.Body.String(), called, s.saves)
				}
			}
		})
	}
}

// TestMCPCatalogAnonymousOnly blocks credential references and preserves saved state after an auth challenge.
func TestMCPCatalogAnonymousOnly(t *testing.T) {
	t.Setenv("FUSED_ENABLE_IMPORTED_MCP", "true")
	serviceID, versionID := uuid.New(), uuid.New()
	actor := catalogTestActor(t, serviceID, true)
	saved := &store.MCPCatalogSnapshot{ID: uuid.New(), URL: "https://example.com/mcp"}
	for _, body := range []string{`{"url":"https://example.com/mcp"}`, `{"url":"https://example.com/mcp","bucket_name":"default","secret_name":"token"}`} {
		s := &catalogAPIStore{active: true, snapshot: saved}
		calls := 0
		// A provider challenge supplies fixed guidance without saving a preview or reading a secret.
		discover := func(_ context.Context, _ string, token string) (mcpcatalog.Catalog, error) {
			calls++
			// The HTTP boundary must pass no credentials even to injected discovery implementations.
			if token != "" {
				t.Error("credential forwarded")
			}
			return mcpcatalog.Catalog{}, mcpcatalog.ErrAuthenticatedDiscoveryUnsupported
		}
		response := catalogAPIRequest(discoverMCPCatalogHandler(s, nil, discover, catalogOwnerVerifier{owner: true}), &actor, serviceID, versionID, body)
		// Neither an unsupported input nor a challenge may alter the existing catalog.
		if response.Code != 422 || !strings.Contains(response.Body.String(), "mcp_catalog_auth_unsupported") || !strings.Contains(response.Body.String(), "Authenticated discovery isn't supported yet.") || s.saves != 0 || s.snapshot != saved {
			t.Fatalf("status=%d body=%s saves=%d", response.Code, response.Body.String(), s.saves)
		}
		// Explicit credential references are rejected before contacting any provider.
		if strings.Contains(body, "bucket_name") && calls != 0 {
			t.Fatal("credential request reached provider")
		}
	}
	s := &catalogAPIStore{active: true, draft: &store.MCPCatalogDraft{MCPCatalogSnapshot: store.MCPCatalogSnapshot{ID: uuid.New(), BucketID: uuid.New()}}}
	response := catalogAPIRequest(ApplyMCPCatalogHandler(s, catalogOwnerVerifier{owner: true}), &actor, serviceID, versionID, `{"draft_id":"`+s.draft.ID.String()+`"}`)
	// An older authenticated draft must not bypass the new import restriction via delayed apply.
	if response.Code != 422 || !strings.Contains(response.Body.String(), "mcp_catalog_auth_unsupported") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
