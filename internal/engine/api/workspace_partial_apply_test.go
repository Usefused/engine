package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/workspaceplan"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// partialApplyTestPool isolates real Engine migrations and fault injection from every other test or workspace.
func partialApplyTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	// Integration tests require an explicitly supplied disposable database.
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "workspace_partial_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	// Cleanup runs after the test pool closes and removes only its generated schema.
	t.Cleanup(func() {
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	parsed, err := url.Parse(dsn)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestWorkspacePartialApplyHundredServices proves a mid-service SQL failure leaves 99 independent successes and resumes only the failed group.
func TestWorkspacePartialApplyHundredServices(t *testing.T) {
	for _, stage := range []string{"policy", "projection", "receipt"} {
		// Dependent data, the state projection, and its receipt share one rollback boundary.
		t.Run(stage, func(t *testing.T) { runWorkspacePartialApplyHundredServices(t, stage) })
	}
}

// runWorkspacePartialApplyHundredServices checks the same acceptance contract at distinct transaction failure boundaries.
func runWorkspacePartialApplyHundredServices(t *testing.T, stage string) {
	t.Helper()
	pool := partialApplyTestPool(t)
	ctx := context.Background()
	s := store.NewPostgresStore(pool)
	repo := store.NewPostgresConfigRepository(pool)
	actor := uuid.New()
	failedID := uuid.New()
	baseURL := "https://api.example.test"
	local := false
	doc := workspaceConfigDocument{Kind: "workspace", Version: 1, Services: map[string]workspaceConfigService{}}
	actions := []workspacePlanAction{}
	for i := 0; i < 100; i++ {
		id := uuid.New()
		// Give the injected failure one exact target among the independent services.
		if i == 0 {
			id = failedID
		}
		versionID := uuid.New()
		doc.Services[fmt.Sprintf("service-%03d", i)] = workspaceConfigService{ServiceID: id.String(), ServiceName: fmt.Sprintf("Service %d", i), Versions: []workspaceConfigServiceVersion{{Version: "v1", ServiceVersionID: versionID.String(), ExecutionPolicy: &workspaceExecutionPolicy{Public: &local, BaseURL: &baseURL}}}}
		actions = append(actions, workspacePlanAction{ID: "policy:" + id.String(), Type: workspaceplan.ActionSetLocalServiceVersionExecutionPolicy, ServiceID: id.String(), ServiceVersionID: versionID.String(), Version: "v1"})
	}
	payload, err := json.Marshal(doc)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	actionJSON, err := json.Marshal(actions)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	required := json.RawMessage(`[{"permission":"workspace.update","resource_type":"workspace","resource_id":"` + actor.String() + `"}]`)
	plan, err := repo.CreateConfigPlan(ctx, store.CreateConfigPlanParams{ConfigKey: "workspace:hundred", ConfigType: store.ConfigTypeWorkspace, SourceHash: "sha256:reviewed", ResolvedPayload: payload, DesiredState: payload, Actions: actionJSON, RequiredPermissions: required, CreatedBy: actor})
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	// Count committed membership writes; a rolled-back service leaves no counter row.
	_, err = pool.Exec(ctx, `CREATE TABLE apply_write_counts(service_id uuid PRIMARY KEY,writes integer NOT NULL);
 CREATE FUNCTION count_service_write() RETURNS trigger AS $$ BEGIN
 INSERT INTO apply_write_counts VALUES(NEW.service_id,1) ON CONFLICT(service_id) DO UPDATE SET writes=apply_write_counts.writes+1;
 RETURN NEW; END $$ LANGUAGE plpgsql;
 CREATE TRIGGER count_service_write AFTER INSERT OR UPDATE ON fused_workspace_services FOR EACH ROW EXECUTE FUNCTION count_service_write();
 CREATE TABLE fail_apply(service_id uuid PRIMARY KEY, stage text);
 -- Reject one dependent policy write after activation has already reached its nested savepoint.
 CREATE FUNCTION fail_service_policy() RETURNS trigger AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM fail_apply WHERE service_id=NEW.service_id AND stage='policy') THEN RAISE EXCEPTION 'injected policy failure'; END IF;
 RETURN NEW; END $$ LANGUAGE plpgsql;
 CREATE TRIGGER fail_service_policy BEFORE INSERT ON fused_workspace_execution_policies FOR EACH ROW EXECUTE FUNCTION fail_service_policy();
 -- Reject the projection only when it would claim that the selected failed service committed.
 CREATE FUNCTION fail_service_projection() RETURNS trigger AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM fail_apply WHERE stage='projection' AND service_id::text=NEW.desired_state->'services'->'service-000'->>'service_id') THEN RAISE EXCEPTION 'injected projection failure'; END IF;
 RETURN NEW; END $$ LANGUAGE plpgsql;
 CREATE TRIGGER fail_service_projection BEFORE INSERT OR UPDATE ON fused_config_states FOR EACH ROW EXECUTE FUNCTION fail_service_projection();
 -- A failed success receipt must roll back even after all service data and state writes succeeded.
 CREATE FUNCTION fail_service_receipt() RETURNS trigger AS $$ BEGIN
 IF NEW.status='succeeded' AND EXISTS(SELECT 1 FROM fail_apply WHERE stage='receipt' AND NEW.step_key='service:'||service_id::text) THEN RAISE EXCEPTION 'injected receipt failure'; END IF;
 RETURN NEW; END $$ LANGUAGE plpgsql;
 CREATE TRIGGER fail_service_receipt BEFORE INSERT OR UPDATE ON fused_workspace_apply_steps FOR EACH ROW EXECUTE FUNCTION fail_service_receipt();`)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if _, err := pool.Exec(ctx, `INSERT INTO fail_apply VALUES($1,$2)`, failedID, stage); err != nil {
		t.Fatal(err)
	}
	call := workspaceApplyCall{planID: plan.ID, planRevision: plan.Revision, sourceHash: plan.SourceHash, accountID: actor}
	_, err = executeWorkspaceConfigApply(ctx, repo, s, &mockVerifier{}, call)
	var partial *workspacePartialApplyError
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if !errors.As(err, &partial) {
		t.Fatalf("expected partial result, got %v", err)
	}
	successes, failures := 0, 0
	for _, result := range partial.Results {
		// Every independent target must have a confirmed final local outcome.
		switch result.Status {
		case "succeeded":
			successes++
		case "failed":
			failures++
		default:
			t.Fatalf("unexpected result: %#v", result)
		}
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if successes != 99 || failures != 1 {
		t.Fatalf("wanted 99 successes and 1 failure, got %d/%d", successes, failures)
	}
	var active int
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_services`).Scan(&active); err != nil || active != 99 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	// Neither enabled versions nor policies may escape a failed enclosing transaction.
	for _, table := range []string{"fused_workspace_service_versions", "fused_workspace_execution_policies"} {
		var count int
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 99 {
			t.Fatalf("partial transaction escaped into %s: count=%d err=%v", table, count, err)
		}
	}
	state, err := repo.GetConfigState(ctx, plan.ConfigKey)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	managed, err := parseManagedWorkspaceResources(state)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || len(managed) != 99 {
		t.Fatalf("partial managed state=%d err=%v", len(managed), err)
	}
	// A failed service cannot appear in the applied projection just because it was requested.
	if _, exists := managed[failedID]; exists {
		t.Fatal("failed service recorded as managed")
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if _, err := pool.Exec(ctx, `DELETE FROM fail_apply`); err != nil {
		t.Fatal(err)
	}
	// Reconstruct repositories to model recovery without any process-local progress memory.
	repo = store.NewPostgresConfigRepository(pool)
	s = store.NewPostgresStore(pool)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if _, err := executeWorkspaceConfigApply(ctx, repo, s, &mockVerifier{}, call); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var writes, maxWrites int
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := pool.QueryRow(ctx, `SELECT SUM(writes),MAX(writes) FROM apply_write_counts`).Scan(&writes, &maxWrites); err != nil || writes != 100 || maxWrites != 1 {
		t.Fatalf("successful service replayed: writes=%d max=%d err=%v", writes, maxWrites, err)
	}
	saved, err := repo.GetConfigPlan(ctx, plan.ID)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || saved.Status != store.ConfigPlanStatusApplied {
		t.Fatalf("plan not completed: %#v %v", saved, err)
	}
	// A lost success response is recoverable by reusing the same plan identity.
	if _, err := executeWorkspaceConfigApply(ctx, repo, s, &mockVerifier{}, call); err != nil {
		t.Fatalf("completed replay: %v", err)
	}
}

// uncertainWorkspaceRegistry simulates a Registry mutation accepted remotely before its response was lost.
type uncertainWorkspaceRegistry struct {
	*mockVerifier
	ServiceVisibilityUpdater
	calls int
}

// DeprecateServiceVersion supplies the Registry capability while detecting an unexpected action in this publication-only fixture.
func (v *uncertainWorkspaceRegistry) DeprecateServiceVersion(context.Context, uuid.UUID, string, string) error {
	return errors.New("unexpected deprecation in publication fixture")
}

// PublishServiceExecutionPolicy returns an ambiguous transport failure after recording dispatch, rather than claiming a safe rejection.
func (v *uncertainWorkspaceRegistry) PublishServiceExecutionPolicy(context.Context, uuid.UUID, any, string) error {
	v.calls++
	return errors.New("injected lost Registry response")
}

// singleServicePartialApplyFixture supplies a real saved plan for handler-level admission and external-failure tests.
func singleServicePartialApplyFixture(t *testing.T, pool *pgxpool.Pool) (store.Store, store.ConfigRepository, workspaceApplyCall) {
	t.Helper()
	actor, id, versionID := uuid.New(), uuid.New(), uuid.New()
	public := true
	baseURL := "https://api.example.test"
	doc := workspaceConfigDocument{Kind: "workspace", Version: 1, Services: map[string]workspaceConfigService{"service": {ServiceID: id.String(), Versions: []workspaceConfigServiceVersion{{Version: "v1", ServiceVersionID: versionID.String()}}, ExecutionPolicy: &workspaceExecutionPolicy{Public: &public, BaseURL: &baseURL}}}}
	payload, err := json.Marshal(doc)
	// Fixture serialization must succeed before any mutation is exercised.
	if err != nil {
		t.Fatal(err)
	}
	actions, err := json.Marshal([]workspacePlanAction{{ID: "publish", Type: workspaceplan.ActionPublishServiceExecutionPolicy, ServiceID: id.String()}})
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	repo := store.NewPostgresConfigRepository(pool)
	required := json.RawMessage(`[{"permission":"workspace.update","resource_type":"workspace","resource_id":"` + actor.String() + `"}]`)
	plan, err := repo.CreateConfigPlan(context.Background(), store.CreateConfigPlanParams{ConfigKey: "workspace:single", ConfigType: store.ConfigTypeWorkspace, SourceHash: "reviewed", ResolvedPayload: payload, DesiredState: payload, Actions: actions, RequiredPermissions: required, CreatedBy: actor})
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	return store.NewPostgresStore(pool), repo, workspaceApplyCall{planID: plan.ID, planRevision: plan.Revision, sourceHash: plan.SourceHash, accountID: actor}
}

// TestWorkspacePartialApplyDoesNotReplayUncertainRegistry proves the full executor checks durable external receipts before retrying anything.
func TestWorkspacePartialApplyDoesNotReplayUncertainRegistry(t *testing.T) {
	pool := partialApplyTestPool(t)
	s, repo, call := singleServicePartialApplyFixture(t, pool)
	verifier := &uncertainWorkspaceRegistry{mockVerifier: &mockVerifier{}}
	// Two explicit apply calls model a client reconnecting after the first partial outcome.
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", strings.NewReader(fmt.Sprintf(`{"plan_id":%q,"source_hash":%q}`, call.planID, call.sourceHash)))
		response := httptest.NewRecorder()
		WorkspaceConfigApplyHandler(repo, s, verifier, nil)(response, controlTestRequest(request, call.accountID))
		var partial struct {
			Status   string                         `json:"status"`
			PlanID   string                         `json:"plan_id"`
			Services []store.WorkspaceApplyProgress `json:"services"`
		}
		// Legacy clients must receive a failure status while newer clients retain the exact plan's results.
		if err := json.Unmarshal(response.Body.Bytes(), &partial); err != nil || response.Code != http.StatusConflict || partial.Status != "partially_applied" || partial.PlanID != call.planID.String() {
			t.Fatalf("expected structured HTTP partial outcome: code=%d body=%s err=%v", response.Code, response.Body, err)
		}
		states := map[string]string{}
		for _, result := range partial.Services {
			states[result.Key] = result.Status
		}
		// Retrying the HTTP request must keep the unresolved dispatch visible.
		if states["registry"] != "running" {
			t.Fatalf("lost external uncertainty: %#v", states)
		}
	}
	// An expired/released request lease never authorizes a repeated external mutation.
	if verifier.calls != 1 {
		t.Fatalf("Registry mutation replayed %d times", verifier.calls)
	}
	saved, err := repo.GetConfigPlan(context.Background(), call.planID)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || len(saved.ApplyResults) != 2 || saved.Status != store.ConfigPlanStatusPending {
		t.Fatalf("durable partial proof missing: %#v %v", saved, err)
	}
}

// TestWorkspacePartialApplyRejectsHistoricalPlan proves upgrades never infer execution history for plans created without service receipts.
func TestWorkspacePartialApplyRejectsHistoricalPlan(t *testing.T) {
	pool := partialApplyTestPool(t)
	s, repo, call := singleServicePartialApplyFixture(t, pool)
	// Version zero is the additive migration default for historical pending plans.
	if _, err := pool.Exec(context.Background(), `UPDATE fused_config_plans SET workspace_apply_version=0 WHERE id=$1`, call.planID); err != nil {
		t.Fatal(err)
	}
	_, err := executeWorkspaceConfigApply(context.Background(), repo, s, &mockVerifier{}, call)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err == nil || !strings.Contains(err.Error(), "predates resumable apply") {
		t.Fatalf("historical plan was admitted: %v", err)
	}
	var count int
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM fused_workspace_services`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical plan mutated services: %d %v", count, err)
	}
}
