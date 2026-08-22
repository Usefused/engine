package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

const (
	acceptanceServiceName    = "acceptance-service"
	acceptanceServiceVersion = "1.0.0"
	acceptanceOperation      = "listThings"
)

type accessWorkflowFixture struct {
	ctx                context.Context
	pool               *pgxpool.Pool
	server             *httptest.Server
	authenticator      *accesscontrol.Authenticator
	registry           *accessWorkflowRegistry
	ownerKey           string
	accountID          uuid.UUID
	workspaceID        uuid.UUID
	serviceID          uuid.UUID
	serviceVersion     uuid.UUID
	bucketID           uuid.UUID
	ungrantedServiceID uuid.UUID
	ungrantedBucketID  uuid.UUID
}

type accessWorkflowTeams struct {
	allowed uuid.UUID
	forged  uuid.UUID
}

type accessWorkflowUser struct {
	id  uuid.UUID
	key string
}

type accessWorkflowPlan struct {
	ID         uuid.UUID
	SourceHash string
}

// TestAccessWorkflowPostgresAcceptance follows the public control-plane
// workflow as one chain. Direct store use is limited to the same bootstrap and
// Registry-catalogue activation performed before Engine starts serving users.
func TestAccessWorkflowPostgresAcceptance(t *testing.T) {
	fixture := newAccessWorkflowFixture(t)
	teams := fixture.createTeamsAndAccess(t)
	user := fixture.inviteAndActivateUser(t, teams.allowed)
	fixture.assertPersonalCredential(t, user)
	fixture.assertTeamSelectors(t, user.key, teams)

	fixture.planAndApplyArtifact(t, user.key, teams.allowed, "sdk", "acceptance-sdk")
	fixture.planAndApplyArtifact(t, user.key, teams.allowed, "mcp", "acceptance-mcp")
	fixture.assertForgedBuildsHaveNoSideEffects(t, user.key, teams)
	fixture.registry.assertHealthy(t)
}

func newAccessWorkflowFixture(t *testing.T) *accessWorkflowFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool := isolatedAcceptancePool(t, ctx)
	engineStore := store.NewPostgresStore(pool)
	configStore := store.NewPostgresConfigRepository(pool)
	accountID := uuid.New()
	workspaceID, err := engineStore.BootstrapWorkspace(ctx, accountID, "Access workflow acceptance")
	if err != nil {
		t.Fatalf("bootstrap workspace: %v", err)
	}
	ownerKey := "fsk_acceptance_" + uuid.NewString()
	bootstrap, err := accesscontrol.BootstrapOwner(ctx, engineStore.(accesscontrol.BootstrapRepository), accountID, ownerKey)
	if err != nil {
		t.Fatalf("bootstrap Owner: %v", err)
	}
	authenticator, err := accesscontrol.NewAuthenticator(engineStore.(accesscontrol.PrincipalLoader), bootstrap.Revision, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		t.Fatalf("create authenticator: %v", err)
	}
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	if err := engineStore.AddWorkspaceServiceVersion(ctx, serviceID, acceptanceServiceName, acceptanceServiceVersion, serviceVersionID, acceptanceServiceName, accountID); err != nil {
		t.Fatalf("activate catalogue service: %v", err)
	}
	ungrantedServiceID := uuid.New()
	if err := engineStore.AddWorkspaceServiceVersion(ctx, ungrantedServiceID, "ungranted-service", acceptanceServiceVersion, uuid.New(), "ungranted-service", accountID); err != nil {
		t.Fatalf("activate ungranted catalogue service: %v", err)
	}
	bucket, err := engineStore.GetBucketByName(ctx, "default")
	if err != nil {
		t.Fatalf("load default bucket: %v", err)
	}
	ungrantedBucket, err := engineStore.CreateBucket(ctx, "ungranted", false)
	if err != nil {
		t.Fatalf("create ungranted bucket: %v", err)
	}
	registry := newAccessWorkflowRegistry(t, accountID, serviceID, serviceVersionID)
	router := accessWorkflowRouter(t, engineStore, configStore, registry, authenticator)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &accessWorkflowFixture{
		ctx: ctx, pool: pool, server: server, authenticator: authenticator, registry: registry,
		ownerKey: ownerKey, accountID: accountID, workspaceID: workspaceID,
		serviceID: serviceID, serviceVersion: serviceVersionID, bucketID: bucket.ID,
		ungrantedServiceID: ungrantedServiceID, ungrantedBucketID: ungrantedBucket.ID,
	}
}

func isolatedAcceptancePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for the PostgreSQL acceptance workflow")
	}
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect acceptance database: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := "engine_access_acceptance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() { dropAcceptanceSchema(t, admin, schema, identifier) })
	dsn := acceptanceSchemaDSN(t, databaseURL, schema)
	pool, err := db.InitEnginePostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("initialize isolated Engine schema: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func acceptanceSchemaDSN(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("DATABASE_URL must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func dropAcceptanceSchema(t *testing.T, admin *pgxpool.Pool, schema, identifier string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
		t.Errorf("drop isolated acceptance schema: %v", err)
		return
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil || exists {
		t.Errorf("isolated acceptance schema cleanup failed: exists=%v error=%v", exists, err)
	}
}

func accessWorkflowRouter(t *testing.T, engineStore store.Store, configStore store.ConfigRepository, registry *accessWorkflowRegistry, authenticator *accesscontrol.Authenticator) http.Handler {
	t.Helper()
	t.Setenv("FUSED_ENV", "development")
	registryClient := sandbox.NewHTTPRegistryClient(registry.server.URL+"/graphql", registry.license)
	registryProxy := api.NewRegistryProxy(registry.server.URL+"/graphql", registry.license)
	auditRecorder, _ := engineStore.(accesscontrol.AuditRecorder)
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(controlActorMiddlewareWithAudit(authenticator, auditRecorder))
	router.Use(controlGraphQLAuditMiddleware(auditRecorder))
	router.Use(controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, newControlRequirementResolver(engineStore, configStore), auditRecorder))
	api.MountConfigRoutes(router, configStore, engineStore, registryClient, registryProxy, registryClient, nil, "")
	if err := api.MountMCPGraphQLRoute(router, configStore, engineStore, registryClient, registryClient, nil, authenticator); err != nil {
		t.Fatalf("mount Engine GraphQL: %v", err)
	}
	return router
}

func (f *accessWorkflowFixture) createTeamsAndAccess(t *testing.T) accessWorkflowTeams {
	t.Helper()
	allowed := f.createTeam(t, "Allowed builders", "allowed-builders")
	forged := f.createTeam(t, "Forged builders", "forged-builders")
	// Both teams are capable Builders with the same scoped resources. Only the
	// first receives membership, making the second team's denial a pure
	// membership/ownership decision rather than a missing-permission shortcut.
	f.configureTeam(t, allowed, "BUILDER")
	f.configureTeam(t, forged, "BUILDER")
	return accessWorkflowTeams{allowed: allowed, forged: forged}
}

func (f *accessWorkflowFixture) createTeam(t *testing.T, name, slug string) uuid.UUID {
	t.Helper()
	query := `mutation Create($input:CreateTeamInput!){createTeam(input:$input){team{id} changed}}`
	variables := map[string]any{"input": map[string]any{"name": name, "slug": slug}}
	var result struct {
		CreateTeam struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
			Changed bool `json:"changed"`
		} `json:"createTeam"`
	}
	f.graphQL(t, f.ownerKey, query, variables, &result)
	return requiredAcceptanceUUID(t, result.CreateTeam.Team.ID, result.CreateTeam.Changed, "created team")
}

func (f *accessWorkflowFixture) configureTeam(t *testing.T, teamID uuid.UUID, role string) {
	t.Helper()
	query := `mutation Configure($team:ID!,$service:ID!,$bucket:ID!,$role:TeamWorkspaceRole!){
workspace:setTeamWorkspaceRole(team_id:$team,role:$role){changed}
service:grantTeamServiceAccess(team_id:$team,service_id:$service,level:USER){changed}
bucket:grantTeamBucketAccess(team_id:$team,bucket_id:$bucket,level:USER){changed}}`
	variables := map[string]any{"team": teamID.String(), "service": f.serviceID.String(), "bucket": f.bucketID.String(), "role": role}
	var result struct {
		Workspace struct {
			Changed bool `json:"changed"`
		} `json:"workspace"`
		Service struct {
			Changed bool `json:"changed"`
		} `json:"service"`
		Bucket struct {
			Changed bool `json:"changed"`
		} `json:"bucket"`
	}
	f.graphQL(t, f.ownerKey, query, variables, &result)
	if !result.Workspace.Changed || !result.Service.Changed || !result.Bucket.Changed {
		t.Fatal("team role and resource grants were not all persisted")
	}
}

func (f *accessWorkflowFixture) inviteAndActivateUser(t *testing.T, teamID uuid.UUID) accessWorkflowUser {
	t.Helper()
	email := "builder-" + uuid.NewString() + "@example.test"
	createQuery := `mutation Invite($input:CreateUserInput!){createUser(input:$input){user{id status} changed}}`
	var created struct {
		CreateUser struct {
			User struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"user"`
			Changed bool `json:"changed"`
		} `json:"createUser"`
	}
	f.graphQL(t, f.ownerKey, createQuery, map[string]any{"input": map[string]any{"email": email, "display_name": "Acceptance Builder"}}, &created)
	userID := requiredAcceptanceUUID(t, created.CreateUser.User.ID, created.CreateUser.Changed && created.CreateUser.User.Status == "INVITED", "invited user")

	memberQuery := `mutation Add($team:ID!,$email:String!){addTeamMember(team_id:$team,email:$email,membership_role:MEMBER){membership{user_id} changed}}`
	var member struct {
		AddTeamMember struct {
			Membership struct {
				UserID string `json:"user_id"`
			} `json:"membership"`
			Changed bool `json:"changed"`
		} `json:"addTeamMember"`
	}
	f.graphQL(t, f.ownerKey, memberQuery, map[string]any{"team": teamID.String(), "email": email}, &member)
	if !member.AddTeamMember.Changed || member.AddTeamMember.Membership.UserID != userID.String() {
		t.Fatal("invited user membership was not persisted")
	}

	issueQuery := `mutation Issue($user:ID!){issueUserCredential(user_id:$user,name:"Acceptance personal credential"){credential{id} secret changed}}`
	var issued struct {
		IssueUserCredential struct {
			Secret  string `json:"secret"`
			Changed bool   `json:"changed"`
		} `json:"issueUserCredential"`
	}
	f.graphQL(t, f.ownerKey, issueQuery, map[string]any{"user": userID.String()}, &issued)
	if !issued.IssueUserCredential.Changed || !strings.HasPrefix(issued.IssueUserCredential.Secret, "fsk_") {
		t.Fatal("personal credential was not issued")
	}
	return accessWorkflowUser{id: userID, key: issued.IssueUserCredential.Secret}
}

func (f *accessWorkflowFixture) assertPersonalCredential(t *testing.T, user accessWorkflowUser) {
	t.Helper()
	actor, err := f.authenticator.AuthenticateControlCredential(f.ctx, user.key)
	if err != nil || actor.SubjectID != user.id || actor.WorkspaceID != f.workspaceID || actor.AccountID != f.accountID {
		t.Fatalf("personal credential authentication identity mismatch: %v", err)
	}
	var leaked bool
	if err := f.pool.QueryRow(f.ctx, `SELECT EXISTS (
		SELECT 1 FROM fused_audit_events
		WHERE metadata::text LIKE '%' || $1 || '%' OR action LIKE '%' || $1 || '%' OR trace_id LIKE '%' || $1 || '%'
	)`, user.key).Scan(&leaked); err != nil {
		t.Fatalf("inspect credential audit safety: %v", err)
	}
	if leaked {
		t.Fatal("personal credential leaked into Engine audit data")
	}
}

func (f *accessWorkflowFixture) assertTeamSelectors(t *testing.T, key string, teams accessWorkflowTeams) {
	t.Helper()
	query := `query Selectors($team:ID!){
appOwningTeams(limit:20){total items{id}}
services:appBuildSelectors(owner_team_id:$team,resource_type:SERVICE,limit:20){total items{resource_id}}
	buckets:appBuildSelectors(owner_team_id:$team,resource_type:BUCKET,limit:20){total items{resource_id}}}`
	allowed := f.selectorQuery(t, key, query, teams.allowed)
	f.assertAllowedSelectors(t, allowed, teams.allowed)
	forged := f.selectorQuery(t, key, query, teams.forged)
	f.assertForgedSelectors(t, forged, teams.forged)
}

func (f *accessWorkflowFixture) assertAllowedSelectors(t *testing.T, allowed accessWorkflowSelectorResult, allowedTeamID uuid.UUID) {
	t.Helper()
	if allowed.Owning.Total != 1 || allowed.Services.Total != 1 || allowed.Buckets.Total != 1 ||
		!selectorHasID(allowed.Owning.Items, allowedTeamID) || !selectorHasID(allowed.Services.Items, f.serviceID) || !selectorHasID(allowed.Buckets.Items, f.bucketID) ||
		selectorHasID(allowed.Services.Items, f.ungrantedServiceID) || selectorHasID(allowed.Buckets.Items, f.ungrantedBucketID) {
		t.Fatalf("allowed selectors = owning:%d/%v services:%d/%v buckets:%d/%v", allowed.Owning.Total, allowed.Owning.Items, allowed.Services.Total, allowed.Services.Items, allowed.Buckets.Total, allowed.Buckets.Items)
	}
}

func (f *accessWorkflowFixture) assertForgedSelectors(t *testing.T, forged accessWorkflowSelectorResult, forgedTeamID uuid.UUID) {
	t.Helper()
	if forged.Services.Total != 0 || forged.Buckets.Total != 0 || selectorHasID(forged.Owning.Items, forgedTeamID) {
		t.Fatal("forged owner-team selector exposed resources or ownership")
	}
}

type accessWorkflowSelectorResult struct {
	Owning struct {
		Total int                          `json:"total"`
		Items []accessWorkflowSelectorItem `json:"items"`
	} `json:"appOwningTeams"`
	Services struct {
		Total int                          `json:"total"`
		Items []accessWorkflowSelectorItem `json:"items"`
	} `json:"services"`
	Buckets struct {
		Total int                          `json:"total"`
		Items []accessWorkflowSelectorItem `json:"items"`
	} `json:"buckets"`
}

type accessWorkflowSelectorItem struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
}

func (f *accessWorkflowFixture) selectorQuery(t *testing.T, key, query string, teamID uuid.UUID) accessWorkflowSelectorResult {
	t.Helper()
	var result accessWorkflowSelectorResult
	f.graphQL(t, key, query, map[string]any{"team": teamID.String()}, &result)
	return result
}

func selectorHasID(items []accessWorkflowSelectorItem, expected uuid.UUID) bool {
	for _, item := range items {
		if item.ID == expected.String() || item.ResourceID == expected.String() {
			return true
		}
	}
	return false
}

func (f *accessWorkflowFixture) planAndApplyArtifact(t *testing.T, key string, ownerTeamID uuid.UUID, kind, name string) {
	t.Helper()
	plan := f.planArtifact(t, key, ownerTeamID, kind, name, http.StatusOK)
	path := "/" + kind + "-config/apply"
	payload := map[string]any{"plan_id": plan.ID.String(), "source_hash": plan.SourceHash}
	var applied struct {
		Status     string `json:"status"`
		ArtifactID string `json:"artifact_id"`
	}
	f.restJSON(t, key, http.MethodPost, path, payload, http.StatusOK, &applied)
	if applied.Status != "applied" || requiredAcceptanceUUID(t, applied.ArtifactID, true, kind+" artifact") == uuid.Nil {
		t.Fatalf("%s apply response was incomplete", kind)
	}
}

func (f *accessWorkflowFixture) planArtifact(t *testing.T, key string, ownerTeamID uuid.UUID, kind, name string, wantStatus int) accessWorkflowPlan {
	return f.planArtifactWithResources(t, key, ownerTeamID, kind, name, acceptanceServiceName, "default", wantStatus)
}

func (f *accessWorkflowFixture) planArtifactWithResources(t *testing.T, key string, ownerTeamID uuid.UUID, kind, name, serviceName, bucketName string, wantStatus int) accessWorkflowPlan {
	t.Helper()
	sourceHash := "sha256:" + kind + ":" + uuid.NewString()
	config := map[string]any{
		"apiVersion": "fused/v1", "kind": kind, "name": name, "version": "1.0.0", "bucket": bucketName,
		"services": map[string]any{serviceName: map[string]any{"version": acceptanceServiceVersion, "operations": []string{acceptanceOperation}}},
	}
	if kind == "sdk" {
		config["language"] = "go"
	}
	payload := map[string]any{
		"owner_team_id": ownerTeamID.String(), "config_key": kind + ":" + name + ":1.0.0",
		"source_hash": sourceHash, "config": config,
	}
	var response struct {
		PlanID string `json:"plan_id"`
	}
	f.restJSON(t, key, http.MethodPost, "/"+kind+"-config/plan", payload, wantStatus, &response)
	if wantStatus != http.StatusOK {
		return accessWorkflowPlan{}
	}
	return accessWorkflowPlan{ID: requiredAcceptanceUUID(t, response.PlanID, true, kind+" plan"), SourceHash: sourceHash}
}

func (f *accessWorkflowFixture) assertForgedBuildsHaveNoSideEffects(t *testing.T, key string, teams accessWorkflowTeams) {
	t.Helper()
	before := f.appMutationSnapshot(t)
	generationCalls := f.registry.generationCount()
	f.planArtifact(t, key, teams.forged, "sdk", "forged-sdk", http.StatusForbidden)
	f.planArtifact(t, key, teams.forged, "mcp", "forged-mcp", http.StatusForbidden)
	after := f.appMutationSnapshot(t)
	if before != after {
		t.Fatal("denied forged-team builds mutated plan, config, runtime, or token state")
	}
	if f.registry.generationCount() != generationCalls {
		t.Fatal("denied forged-team builds reached Registry generation")
	}
	f.assertUngrantedResourceBuildsDenied(t, key, teams.allowed, before)
}

func (f *accessWorkflowFixture) assertUngrantedResourceBuildsDenied(t *testing.T, key string, allowedTeamID uuid.UUID, before string) {
	t.Helper()
	registryCalls := f.registry.callCount()
	for _, kind := range []string{"sdk", "mcp"} {
		f.planArtifactWithResources(t, key, allowedTeamID, kind, "ungranted-service-"+kind, "ungranted-service", "default", http.StatusForbidden)
		f.planArtifactWithResources(t, key, allowedTeamID, kind, "ungranted-bucket-"+kind, acceptanceServiceName, "ungranted", http.StatusForbidden)
	}
	if f.appMutationSnapshot(t) != before {
		t.Fatal("denied ungranted-resource builds mutated plan, config, runtime, or token state")
	}
	if f.registry.callCount() != registryCalls {
		t.Fatal("denied ungranted-resource builds crossed the Registry boundary")
	}
}

func (f *accessWorkflowFixture) appMutationSnapshot(t *testing.T) string {
	t.Helper()
	// Denied plans must leave every Engine-owned app relationship unchanged;
	// checking the canonical tables prevents a partial family or token write
	// from being hidden behind the config-plan response.
	const query = `SELECT md5(
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.id)::text FROM fused_config_plans row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.config_key)::text FROM fused_config_states row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.app_family_id)::text FROM fused_app_families row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.app_id)::text FROM fused_apps row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.app_id, row_data.capability_key)::text FROM fused_app_capabilities row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.id)::text FROM fused_app_token_history row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.id)::text FROM fused_app_tokens row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.token_id, row_data.service_id, row_data.auth_name)::text FROM fused_app_token_bindings row_data), '[]') ||
		COALESCE((SELECT jsonb_agg(to_jsonb(row_data) ORDER BY row_data.app_family_id)::text FROM fused_app_family_buckets row_data), '[]'))`
	var snapshot string
	if err := f.pool.QueryRow(f.ctx, query).Scan(&snapshot); err != nil {
		t.Fatalf("capture app mutation snapshot: %v", err)
	}
	return snapshot
}

func (f *accessWorkflowFixture) graphQL(t *testing.T, key, query string, variables map[string]any, target any) {
	t.Helper()
	payload := map[string]any{"query": query, "variables": variables}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	f.restJSON(t, key, http.MethodPost, "/engine/graphql", payload, http.StatusOK, &envelope)
	if len(envelope.Errors) != 0 {
		t.Fatalf("Engine GraphQL returned %d error(s): %s", len(envelope.Errors), envelope.Errors[0].Message)
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		t.Fatalf("decode Engine GraphQL data: %v", err)
	}
}

func (f *accessWorkflowFixture) restJSON(t *testing.T, key, method, path string, payload any, wantStatus int, target any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode request payload: %v", err)
	}
	request, err := http.NewRequestWithContext(f.ctx, method, f.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create Engine request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", key)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("execute Engine request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		t.Fatalf("Engine %s %s status = %d, want %d: %s", method, path, response.StatusCode, wantStatus, responseBody)
	}
	if target != nil && response.StatusCode < http.StatusBadRequest {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode Engine %s %s response: %v", method, path, err)
		}
	}
}

func requiredAcceptanceUUID(t *testing.T, raw string, condition bool, label string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || !condition {
		t.Fatalf("%s was not returned", label)
	}
	return id
}

type accessWorkflowRegistry struct {
	mu               sync.Mutex
	server           *httptest.Server
	license          string
	accountID        uuid.UUID
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	endpointID       uuid.UUID
	calls            int
	generationCalls  int
	invalidIdentity  bool
	unexpected       []string
}

func newAccessWorkflowRegistry(t *testing.T, accountID, serviceID, serviceVersionID uuid.UUID) *accessWorkflowRegistry {
	t.Helper()
	registry := &accessWorkflowRegistry{
		license: "fsk_registry_acceptance_" + uuid.NewString(), accountID: accountID,
		serviceID: serviceID, serviceVersionID: serviceVersionID, endpointID: uuid.New(),
	}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serveHTTP))
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *accessWorkflowRegistry) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.beginRequest(request) {
		http.Error(w, "registry request rejected", http.StatusBadGateway)
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/integrations/versions/revisions":
		r.serveRevisions(w, request)
	case request.Method == http.MethodPost && request.URL.Path == "/graphql":
		r.serveGraphQL(w, request)
	case request.Method == http.MethodPost && request.URL.Path == "/sdks/generate":
		r.serveGeneration(w, request)
	default:
		r.recordUnexpected(request.Method + " " + request.URL.Path)
		http.Error(w, "unexpected registry request", http.StatusNotFound)
	}
}

func (r *accessWorkflowRegistry) beginRequest(request *http.Request) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	valid := request.Header.Get("X-API-Key") == r.license && request.Header.Get("Authorization") == "Bearer "+r.license
	if !valid {
		r.invalidIdentity = true
	}
	return valid && r.calls <= 24
}

func (r *accessWorkflowRegistry) serveRevisions(w http.ResponseWriter, request *http.Request) {
	var payload struct {
		Versions []sandbox.ServiceVersionRef `json:"versions"`
	}
	if json.NewDecoder(request.Body).Decode(&payload) != nil || len(payload.Versions) != 1 || payload.Versions[0].ServiceID != r.serviceID {
		r.recordUnexpected("invalid revision request")
		http.Error(w, "invalid revision request", http.StatusBadRequest)
		return
	}
	writeAcceptanceRegistryJSON(w, map[string]any{"versions": []sandbox.ServiceVersionRevision{{
		ServiceID: r.serviceID, Version: acceptanceServiceVersion, ServiceVersionID: r.serviceVersionID, Revision: 1, SourceHash: "contract-hash",
	}}})
}

func (r *accessWorkflowRegistry) serveGraphQL(w http.ResponseWriter, request *http.Request) {
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if json.NewDecoder(request.Body).Decode(&payload) != nil {
		http.Error(w, "invalid graphql request", http.StatusBadRequest)
		return
	}
	switch {
	case strings.Contains(payload.Query, "serviceVersionAuthConfigs"):
		writeAcceptanceRegistryJSON(w, map[string]any{"data": map[string]any{"serviceVersionAuthConfigs": []any{map[string]any{
			"service_id": r.serviceID, "version": acceptanceServiceVersion, "service_version_id": r.serviceVersionID, "auth_configs": []any{},
		}}}})
	case strings.Contains(payload.Query, "validateSDKSelections"):
		writeAcceptanceRegistryJSON(w, map[string]any{"data": map[string]any{"validateSDKSelections": true}})
	case strings.Contains(payload.Query, "endpointsByNames"):
		endpoint := fusedobject.Endpoint{ID: r.endpointID, Name: acceptanceOperation, Method: http.MethodGet, Path: "/things"}
		writeAcceptanceRegistryJSON(w, map[string]any{"data": map[string]any{"endpointsByNames": []fusedobject.Endpoint{endpoint}}})
	case strings.Contains(payload.Query, "driftSnapshotsForServices"):
		writeAcceptanceRegistryJSON(w, map[string]any{"data": map[string]any{"driftSnapshotsForServices": []any{}}})
	default:
		r.recordUnexpected("unhandled Registry GraphQL operation")
		http.Error(w, "unhandled graphql operation", http.StatusBadRequest)
	}
}

func (r *accessWorkflowRegistry) serveGeneration(w http.ResponseWriter, request *http.Request) {
	var payload models.SDKGenerationRequest
	if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.AppFamilyID == uuid.Nil || payload.AppID == uuid.Nil || len(payload.Selections) != 1 {
		r.recordUnexpected("invalid generation request")
		http.Error(w, "invalid generation request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.generationCalls++
	r.mu.Unlock()
	// Registry generation resolves requested operation names into the concrete
	// endpoint IDs persisted in the runtime scope.
	generatedSelections := append(models.SDKSelections(nil), payload.Selections...)
	generatedSelections[0].OperationNames = nil
	generatedSelections[0].EndpointIDs = []uuid.UUID{r.endpointID}
	writeAcceptanceRegistryJSON(w, models.SDKGenerationResult{
		AppFamilyID: payload.AppFamilyID, AppID: payload.AppID,
		AccountID: r.accountID, JobID: "acceptance-job", Status: models.SDKGenerationStatusComplete,
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: generatedSelections,
	})
}

func writeAcceptanceRegistryJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (r *accessWorkflowRegistry) recordUnexpected(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unexpected = append(r.unexpected, value)
}

func (r *accessWorkflowRegistry) generationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generationCalls
}

func (r *accessWorkflowRegistry) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *accessWorkflowRegistry) assertHealthy(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invalidIdentity || len(r.unexpected) != 0 || r.calls > 24 || r.generationCalls != 1 {
		t.Fatalf("bounded Registry contract failed: invalid_identity=%v unexpected=%d calls=%d generation_calls=%d", r.invalidIdentity, len(r.unexpected), r.calls, r.generationCalls)
	}
}
