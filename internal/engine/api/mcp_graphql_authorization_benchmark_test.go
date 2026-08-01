package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

const graphQLPreflightDatabaseQueryLimit = 3

type graphQLPreflightStore struct {
	store.Store
	bucket              store.Bucket
	bucketQueries       int
	localServiceQueries int
}

func (s *graphQLPreflightStore) GetBucketsByNames(context.Context, []string) ([]store.Bucket, error) {
	s.bucketQueries++
	return []store.Bucket{s.bucket}, nil
}

func (s *graphQLPreflightStore) ResolveWorkspaceServiceIDsByKeys(context.Context, []string) (map[string]uuid.UUID, error) {
	s.localServiceQueries++
	// The fixture deliberately exercises the single batched Registry fallback.
	return map[string]uuid.UUID{}, nil
}

func (s *graphQLPreflightStore) databaseQueries() int {
	return s.bucketQueries + s.localServiceQueries
}

type graphQLPreflightConfigStore struct {
	store.ConfigRepository
	queries int
}

func (s *graphQLPreflightConfigStore) GetConfigStatesByKeys(context.Context, []string) (map[string]store.ConfigState, error) {
	s.queries++
	return map[string]store.ConfigState{}, nil
}

type graphQLPreflightSlugResolver struct {
	serviceID uuid.UUID
	queries   int
}

func (r *graphQLPreflightSlugResolver) ResolveServiceIDsBySlugs(context.Context, []string, string) (map[string]uuid.UUID, error) {
	r.queries++
	return map[string]uuid.UUID{"github": r.serviceID}, nil
}

type graphQLPreflightFixture struct {
	schema      graphql.Schema
	requestBody []byte
	actor       accesscontrol.Actor
	resources   graphQLAuthorizationResources
	store       *graphQLPreflightStore
	configStore *graphQLPreflightConfigStore
	resolver    *graphQLPreflightSlugResolver
}

type graphQLTotalRequestPrincipalLoader struct {
	principal accesscontrol.ControlPrincipal
	loads     int
}

func (l *graphQLTotalRequestPrincipalLoader) LoadControlPrincipal(context.Context, string) (accesscontrol.ControlPrincipal, error) {
	l.loads++
	return l.principal, nil
}

func TestGraphQLAuthorizationPreflightQueryCountIsBounded(t *testing.T) {
	for _, fieldCount := range []int{1, 10, 25} {
		t.Run("fields_"+strconv.Itoa(fieldCount), func(t *testing.T) {
			fixture := newGraphQLPreflightFixture(t, fieldCount)
			request := fixture.request(t.Context())
			plan, err := authorizeEngineGraphQL(request, &fixture.schema, fixture.actor, fixture.resources, false)
			if err != nil {
				t.Fatalf("authorize GraphQL preflight: %v", err)
			}
			if plan.rootFields != fieldCount {
				t.Fatalf("selected root fields = %d, want %d", plan.rootFields, fieldCount)
			}
			if got := fixture.databaseQueries(); got != graphQLPreflightDatabaseQueryLimit {
				t.Fatalf("database queries = %d, want bounded count %d", got, graphQLPreflightDatabaseQueryLimit)
			}
			if fixture.resolver.queries != 1 {
				t.Fatalf("Registry resolution calls = %d, want one batch", fixture.resolver.queries)
			}
		})
	}
}

func BenchmarkAuthorizationAcceptance(b *testing.B) {
	for _, fieldCount := range []int{1, 10, 25} {
		name := "fields_" + strconv.Itoa(fieldCount)
		b.Run("graphql_preflight/"+name, func(b *testing.B) {
			benchmarkGraphQLPreflight(b, fieldCount)
		})
		b.Run("total_request/"+name, func(b *testing.B) {
			benchmarkGraphQLTotalRequest(b, fieldCount)
		})
	}
}

func TestGraphQLTotalRequestBenchmarkMiddlewareUsesCredentialCache(t *testing.T) {
	actor := benchmarkActor(t, uuid.New())
	loader, authenticator := benchmarkTotalRequestAuthenticator(t, actor, nil)
	handler := benchmarkControlActorMiddleware(authenticator, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if _, ok := accesscontrol.ActorFromContext(request.Context()); !ok {
			t.Fatal("authenticated actor was not added to the request")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/engine/graphql", nil)
		request.Header.Set("X-API-Key", "fsk_benchmark_total")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("response status = %d, want 204", response.Code)
		}
	}
	if loader.loads != 1 {
		t.Fatalf("principal loads = %d, want one warm load", loader.loads)
	}
}

func benchmarkGraphQLPreflight(b *testing.B, fieldCount int) {
	fixture := newGraphQLPreflightFixture(b, fieldCount)
	request := fixture.request(context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := authorizeEngineGraphQL(request, &fixture.schema, fixture.actor, fixture.resources, false); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(fixture.databaseQueries())/float64(b.N), "db_queries/op")
	b.ReportMetric(float64(fixture.resolver.queries)/float64(b.N), "external_queries/op")
}

func benchmarkGraphQLTotalRequest(b *testing.B, fieldCount int) {
	workspaceID, bucketID := uuid.New(), uuid.New()
	testStore := &workspaceTestStore{
		accountID:       uuid.New(),
		bucketSummaries: []store.BucketSummary{{Bucket: store.Bucket{ID: bucketID, Name: "production"}}},
	}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, testStore, &mockVerifier{}, &mockRegistryClient{}, testMasterKey)
	if err != nil {
		b.Fatal(err)
	}
	var handler http.Handler = mcpGraphQLHandler(schema)
	grants := []accesscontrol.Grant{{
		Permission: accesscontrol.PermissionBucketRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID},
	}}
	actor := benchmarkActor(b, workspaceID, grants...)
	loader, authenticator := benchmarkTotalRequestAuthenticator(b, actor, grants)
	handler = benchmarkControlActorMiddleware(authenticator, handler)
	body := graphQLBucketQueryBody(b, bucketID, fieldCount)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-API-Key", "fsk_benchmark_total")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("GraphQL response status = %d: %s", response.Code, response.Body.String())
		}
	}
	b.StopTimer()
	if loader.loads != 1 {
		b.Fatalf("principal loads = %d, want one warm load", loader.loads)
	}
}

func benchmarkTotalRequestAuthenticator(tb testing.TB, actor accesscontrol.Actor, grants []accesscontrol.Grant) (*graphQLTotalRequestPrincipalLoader, *accesscontrol.Authenticator) {
	tb.Helper()
	loader := &graphQLTotalRequestPrincipalLoader{principal: accesscontrol.ControlPrincipal{
		AccountID: actor.AccountID, WorkspaceID: actor.WorkspaceID, SubjectID: actor.SubjectID,
		CredentialID: actor.CredentialID, Kind: accesscontrol.SubjectUser, Revision: 1,
		EffectiveGrants: grants,
	}}
	authenticator, err := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		tb.Fatalf("create total-request authenticator: %v", err)
	}
	if _, err := authenticator.AuthenticateControlCredential(context.Background(), "fsk_benchmark_total"); err != nil {
		tb.Fatalf("warm total-request credential cache: %v", err)
	}
	return loader, authenticator
}

func benchmarkControlActorMiddleware(authenticator *accesscontrol.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		actor, err := authenticator.AuthenticateControlCredential(request.Context(), strings.TrimSpace(request.Header.Get("X-API-Key")))
		if err != nil {
			accesscontrol.WriteAuthorizationError(w, err)
			return
		}
		next.ServeHTTP(w, request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor)))
	})
}

func newGraphQLPreflightFixture(tb testing.TB, fieldCount int) graphQLPreflightFixture {
	tb.Helper()
	workspaceID, bucketID, serviceID := uuid.New(), uuid.New(), uuid.New()
	testStore := &workspaceTestStore{accountID: uuid.New()}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, testStore, &mockVerifier{}, &mockRegistryClient{}, testMasterKey)
	if err != nil {
		tb.Fatalf("create benchmark schema: %v", err)
	}
	preflightStore := &graphQLPreflightStore{bucket: store.Bucket{ID: bucketID, Name: "production"}}
	configStore := &graphQLPreflightConfigStore{}
	resolver := &graphQLPreflightSlugResolver{serviceID: serviceID}
	actor := benchmarkActor(tb, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionArtifactCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	)
	return graphQLPreflightFixture{
		schema: schema, requestBody: graphQLDeploymentQueryBody(tb, fieldCount), actor: actor,
		resources: graphQLAuthorizationResources{store: preflightStore, configStore: configStore, slugResolver: resolver},
		store:     preflightStore, configStore: configStore, resolver: resolver,
	}
}

func (f graphQLPreflightFixture) request(ctx context.Context) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(f.requestBody)))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(ctx)
}

func (f graphQLPreflightFixture) databaseQueries() int {
	return f.store.databaseQueries() + f.configStore.queries
}

func graphQLDeploymentQueryBody(tb testing.TB, fieldCount int) []byte {
	tb.Helper()
	var query strings.Builder
	query.WriteString("mutation Deploy($config: EngineJSON!) {")
	for index := range fieldCount {
		query.WriteString(" field")
		query.WriteString(strconv.Itoa(index))
		query.WriteString(": deployMcpServer(config: $config) { id }")
	}
	query.WriteString(" }")
	body, err := json.Marshal(map[string]any{
		"query": query.String(),
		"variables": map[string]any{"config": map[string]any{
			"apiVersion": "fused/v1", "kind": "mcp", "name": "team-agent", "version": "1.0.0", "bucket": "production",
			"services": map[string]any{"github": map[string]any{"version": "1.0.0"}},
		}},
	})
	if err != nil {
		tb.Fatalf("marshal deployment request: %v", err)
	}
	return body
}

func graphQLBucketQueryBody(tb testing.TB, bucketID uuid.UUID, fieldCount int) []byte {
	tb.Helper()
	var query strings.Builder
	query.WriteString("query {")
	for index := range fieldCount {
		query.WriteString(" field")
		query.WriteString(strconv.Itoa(index))
		query.WriteString(": bucketSummary(bucket_id: \"")
		query.WriteString(bucketID.String())
		query.WriteString("\") { id }")
	}
	query.WriteString(" }")
	body, err := json.Marshal(map[string]string{"query": query.String()})
	if err != nil {
		tb.Fatalf("marshal bucket request: %v", err)
	}
	return body
}

func benchmarkActor(tb testing.TB, workspaceID uuid.UUID, grants ...accesscontrol.Grant) accesscontrol.Actor {
	tb.Helper()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		tb.Fatalf("create authorization snapshot: %v", err)
	}
	return accesscontrol.Actor{
		AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(),
		CredentialID: uuid.New(), Authorization: snapshot,
	}
}
