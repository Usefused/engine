package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type leaseQueryCall struct {
	SQL  string
	Args []any
}

type leaseQueryTracer struct {
	calls chan leaseQueryCall
}

func (tracer *leaseQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "apply_lease_expires_at = NOW()") {
		tracer.calls <- leaseQueryCall{SQL: data.SQL, Args: append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*leaseQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestConfigRepositoryValidation(t *testing.T) {
	repo := &postgresConfigRepository{}

	_, err := repo.UpsertConfigState(context.Background(), UpsertConfigStateParams{
		ConfigType: ConfigTypeSDK,
		SourceHash: "sha256:test",
	})
	if !errors.Is(err, ErrConfigKeyRequired) {
		t.Fatalf("expected ErrConfigKeyRequired, got %v", err)
	}

	_, err = repo.CreateConfigPlan(context.Background(), CreateConfigPlanParams{
		ConfigKey:  "sdk:test",
		ConfigType: ConfigType("other"),
		SourceHash: "sha256:test",
	})
	if !errors.Is(err, ErrConfigTypeInvalid) {
		t.Fatalf("expected ErrConfigTypeInvalid, got %v", err)
	}

	_, err = repo.CreateConfigPlan(context.Background(), CreateConfigPlanParams{
		ConfigKey:  "sdk:test",
		ConfigType: ConfigTypeSDK,
		SourceHash: "sha256:test",
		Actions:    json.RawMessage(`{`),
	})
	if !errors.Is(err, ErrConfigJSONInvalid) {
		t.Fatalf("expected ErrConfigJSONInvalid, got %v", err)
	}

	_, err = repo.CreateConfigPlan(context.Background(), CreateConfigPlanParams{
		ConfigKey:  "sdk:test",
		ConfigType: ConfigTypeSDK,
		SourceHash: "sha256:test",
		Actions:    json.RawMessage(`{"id":"force"}`),
	})
	if !errors.Is(err, ErrConfigJSONArrayRequired) {
		t.Fatalf("expected ErrConfigJSONArrayRequired, got %v", err)
	}

	_, err = repo.UpsertConfigState(context.Background(), UpsertConfigStateParams{
		ConfigKey:    "sdk:test",
		ConfigType:   ConfigTypeSDK,
		SourceHash:   "sha256:test",
		DesiredState: json.RawMessage(`["not-an-object"]`),
	})
	if !errors.Is(err, ErrConfigJSONObjectRequired) {
		t.Fatalf("expected ErrConfigJSONObjectRequired, got %v", err)
	}

	whitespacePlan := CreateConfigPlanParams{
		ConfigKey:           "sdk:test",
		ConfigType:          ConfigTypeSDK,
		SourceHash:          "sha256:test",
		Actions:             json.RawMessage(`  []`),
		ResolvedPayload:     json.RawMessage(`  {"ok":true}`),
		Blockers:            json.RawMessage(`  []`),
		Warnings:            json.RawMessage(`  []`),
		RequiredPermissions: testRequiredPermissions(uuid.New()),
		SupersedeExisting:   false,
	}
	ownerTeamID := uuid.New()
	whitespacePlan.OwnerTeamID = &ownerTeamID
	if err := validatePlanParams(&whitespacePlan); err != nil {
		t.Fatalf("expected whitespace-padded JSON to validate, got %v", err)
	}

	missingPermissions := whitespacePlan
	missingPermissions.RequiredPermissions = nil
	if err := validatePlanParams(&missingPermissions); !errors.Is(err, ErrRequiredPermissions) {
		t.Fatalf("expected ErrRequiredPermissions, got %v", err)
	}
}

func testRequiredPermissions(resourceID uuid.UUID) json.RawMessage {
	return json.RawMessage(`[{"permission":"workspace.update","resource_type":"workspace","resource_id":"` + resourceID.String() + `","display_name":"workspace"}]`)
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal(want, &wantValue) != nil {
		t.Fatalf("decode JSON: got=%s want=%s", got, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch: got=%s want=%s", got, want)
	}
}

func assertDatabaseLeaseExpiry(t *testing.T, expiresAt, databaseStartedAt, databaseFinishedAt time.Time) {
	t.Helper()
	minimum := databaseStartedAt.Add(configPlanApplyLeaseTTL)
	maximum := databaseFinishedAt.Add(configPlanApplyLeaseTTL)
	if expiresAt.Before(minimum) || expiresAt.After(maximum) {
		t.Fatalf("lease expiry %s is outside database clock window [%s, %s]", expiresAt, minimum, maximum)
	}
}

func assertStoredLeaseExpiry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, planID uuid.UUID, want time.Time) {
	t.Helper()
	var stored time.Time
	if err := pool.QueryRow(ctx, `SELECT apply_lease_expires_at FROM fused_config_plans WHERE id = $1`, planID).Scan(&stored); err != nil {
		t.Fatalf("read stored lease expiry: %v", err)
	}
	if !stored.Equal(want) {
		t.Fatalf("stored lease expiry = %s, returned %s", stored, want)
	}
}

func assertLeaseQueryUsesDatabaseClock(t *testing.T, call leaseQueryCall) {
	t.Helper()
	if !strings.Contains(call.SQL, "NOW() + ($4 * INTERVAL '1 second')") {
		t.Fatalf("lease query does not compute expiry from PostgreSQL NOW(): %s", call.SQL)
	}
	if len(call.Args) != 4 {
		t.Fatalf("lease query argument count = %d, want 4", len(call.Args))
	}
	seconds, ok := call.Args[3].(int64)
	if !ok || seconds != configPlanApplyLeaseTTLSeconds {
		t.Fatalf("lease expiry argument = %#v, want database interval seconds %d", call.Args[3], configPlanApplyLeaseTTLSeconds)
	}
	for _, argument := range call.Args {
		if _, isApplicationTimestamp := argument.(time.Time); isApplicationTimestamp {
			t.Fatalf("lease query received application-computed timestamp %#v", argument)
		}
	}
}

func nextLeaseQuery(t *testing.T, calls <-chan leaseQueryCall) leaseQueryCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("lease update query was not traced")
		return leaseQueryCall{}
	}
}

// TestUpdateWorkspaceNotificationStatusRejectsPendingWithoutDB proves the
// target-status guard runs before any query -- a nil-db repo would panic on
// an actual query, so this only passes if the invalid-status check truly
// short-circuits first (see UpdateWorkspaceNotificationStatus's own doc
// comment on why 'pending' is never a valid transition target).
func TestUpdateWorkspaceNotificationStatusRejectsPendingWithoutDB(t *testing.T) {
	repo := &postgresConfigRepository{}
	if _, err := repo.UpdateWorkspaceNotificationStatus(context.Background(), uuid.New(), WorkspaceNotificationStatusPending, uuid.New()); !errors.Is(err, ErrWorkspaceNotificationStatusInvalid) {
		t.Fatalf("expected ErrWorkspaceNotificationStatusInvalid, got %v", err)
	}
	if _, err := repo.UpdateWorkspaceNotificationStatus(context.Background(), uuid.New(), WorkspaceNotificationStatus("bogus"), uuid.New()); !errors.Is(err, ErrWorkspaceNotificationStatusInvalid) {
		t.Fatalf("expected ErrWorkspaceNotificationStatusInvalid for an unknown status, got %v", err)
	}
}

func TestConfigRepositoryPostgres(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping config repository integration tests: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := isolatedBootstrapPool(t, ctx, dbURL)
	defer pool.Close()

	accountID := uuid.New()
	requiredPermissions := testRequiredPermissions(accountID)
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("reset singleton workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_config_states"); err != nil {
		t.Fatalf("reset config states: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_config_plans"); err != nil {
		t.Fatalf("reset config plans: %v", err)
	}
	store := NewPostgresStore(pool)
	_, err := store.BootstrapWorkspace(ctx, accountID, "Config Repo Test")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	ownerTeamID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, 'Config owners', $2)`, ownerTeamID, "config-owners-"+ownerTeamID.String()); err != nil {
		t.Fatalf("create owner team: %v", err)
	}

	repo := NewPostgresConfigRepository(pool)
	t.Run("database rejects an empty webhook owner", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO fused_workspace_webhooks
				(service_id, service_version_id, label, slug, owning_config_key)
			VALUES ($1, $2, 'unowned', $3, '')
		`, uuid.New(), uuid.New(), "unowned-"+uuid.NewString())
		if err == nil {
			t.Fatal("empty webhook owner bypassed the clean-schema constraint")
		}
	})
	t.Run("state upsert increments generation", func(t *testing.T) {
		params := UpsertConfigStateParams{
			ConfigKey:        "sdk:security",
			ConfigType:       ConfigTypeSDK,
			OwnerTeamID:      &ownerTeamID,
			SourceHash:       "sha256:first",
			DesiredState:     json.RawMessage(`{"name":"security"}`),
			ManagedResources: json.RawMessage(`{"services":["okta"]}`),
			UpdatedBy:        accountID,
		}
		first, err := repo.UpsertConfigState(ctx, params)
		if err != nil {
			t.Fatalf("first UpsertConfigState: %v", err)
		}
		params.SourceHash = "sha256:second"
		second, err := repo.UpsertConfigState(ctx, params)
		if err != nil {
			t.Fatalf("second UpsertConfigState: %v", err)
		}
		if second.ID != first.ID {
			t.Fatalf("expected same state row, got %s then %s", first.ID, second.ID)
		}
		if second.Generation != first.Generation+1 {
			t.Fatalf("expected generation to increment from %d to %d, got %d", first.Generation, first.Generation+1, second.Generation)
		}
	})

	t.Run("state identity is immutable in repository and database", func(t *testing.T) {
		otherTeamID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, 'Immutable owners', $2)`, otherTeamID, "immutable-owners-"+otherTeamID.String()); err != nil {
			t.Fatalf("create other team: %v", err)
		}
		params := UpsertConfigStateParams{
			ConfigKey: "sdk:immutable", ConfigType: ConfigTypeSDK, OwnerTeamID: &ownerTeamID,
			SourceHash: "sha256:immutable", UpdatedBy: accountID,
		}
		if _, err := repo.UpsertConfigState(ctx, params); err != nil {
			t.Fatalf("seed immutable state: %v", err)
		}
		params.OwnerTeamID = &otherTeamID
		if _, err := repo.UpsertConfigState(ctx, params); !errors.Is(err, ErrConfigStateIdentityMismatch) {
			t.Fatalf("owner replacement error = %v, want ErrConfigStateIdentityMismatch", err)
		}
		params.OwnerTeamID = &ownerTeamID
		params.ConfigType = ConfigTypeMCP
		if _, err := repo.UpsertConfigState(ctx, params); !errors.Is(err, ErrConfigStateIdentityMismatch) {
			t.Fatalf("type replacement error = %v, want ErrConfigStateIdentityMismatch", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE fused_config_states SET owner_team_id = $2 WHERE config_key = $1`, params.ConfigKey, otherTeamID); err == nil {
			t.Fatal("direct database owner replacement unexpectedly succeeded")
		}
		state, err := repo.GetConfigState(ctx, params.ConfigKey)
		if err != nil || state.OwnerTeamID == nil || *state.OwnerTeamID != ownerTeamID || state.ConfigType != ConfigTypeSDK {
			t.Fatalf("immutable state changed: %#v, %v", state, err)
		}
	})

	t.Run("new plan supersedes previous pending", func(t *testing.T) {
		first, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey:           "sdk:identity",
			ConfigType:          ConfigTypeSDK,
			OwnerTeamID:         &ownerTeamID,
			SourceHash:          "sha256:first",
			Actions:             json.RawMessage(`[{"id":"remove-okta"}]`),
			ResolvedPayload:     json.RawMessage(`{"language":"typescript"}`),
			RequiredPermissions: requiredPermissions,
			CreatedBy:           accountID,
			SupersedeExisting:   true,
		})
		if err != nil {
			t.Fatalf("first CreateConfigPlan: %v", err)
		}
		second, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey:           "sdk:identity",
			ConfigType:          ConfigTypeSDK,
			OwnerTeamID:         &ownerTeamID,
			SourceHash:          "sha256:second",
			RequiredPermissions: requiredPermissions,
			CreatedBy:           accountID,
			SupersedeExisting:   true,
		})
		if err != nil {
			t.Fatalf("second CreateConfigPlan: %v", err)
		}
		refetched, err := repo.GetConfigPlan(ctx, first.ID)
		if err != nil {
			t.Fatalf("GetConfigPlan first: %v", err)
		}
		if refetched.Status != ConfigPlanStatusSuperseded {
			t.Fatalf("expected first plan superseded, got %q", refetched.Status)
		}
		if second.Status != ConfigPlanStatusPending {
			t.Fatalf("expected second plan pending, got %q", second.Status)
		}
		assertJSONEqual(t, second.RequiredPermissions, requiredPermissions)
	})

	t.Run("list config states by type", func(t *testing.T) {
		_, err := repo.UpsertConfigState(ctx, UpsertConfigStateParams{
			ConfigKey:        "workspace",
			ConfigType:       ConfigTypeWorkspace,
			SourceHash:       "sha256:workspace-list",
			DesiredState:     json.RawMessage(`{"kind":"workspace"}`),
			ManagedResources: json.RawMessage(`{"services":[]}`),
			UpdatedBy:        accountID,
		})
		if err != nil {
			t.Fatalf("UpsertConfigState workspace: %v", err)
		}
		states, err := repo.ListConfigStates(ctx, ConfigTypeSDK)
		if err != nil {
			t.Fatalf("ListConfigStates: %v", err)
		}
		for _, state := range states {
			if state.ConfigType != ConfigTypeSDK {
				t.Fatalf("expected only SDK states, got %#v", state)
			}
		}
	})

	t.Run("apply writes mcp state and closes plan atomically", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP, SourceHash: "sha256:mcp", DesiredState: json.RawMessage(`{"kind":"mcp"}`),
			OwnerTeamID: &ownerTeamID,
			CreatedBy:   accountID, SupersedeExisting: true,
			RequiredPermissions: requiredPermissions,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		runtimeID := uuid.New()
		state, err := repo.ApplyConfigPlan(ctx, ApplyConfigPlanParams{
			State: UpsertConfigStateParams{
				ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP,
				SourceHash: plan.SourceHash, DesiredState: plan.DesiredState,
				LatestResourceID: &runtimeID, UpdatedBy: accountID,
			},
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
		})
		if err != nil {
			t.Fatalf("ApplyConfigPlan: %v", err)
		}
		applied, err := repo.GetConfigPlan(ctx, plan.ID)
		if err != nil || applied.Status != ConfigPlanStatusApplied {
			t.Fatalf("expected applied plan, got %#v, %v", applied, err)
		}
		if state.ConfigType != ConfigTypeMCP || state.LatestResourceID == nil || *state.LatestResourceID != runtimeID || state.OwnerTeamID == nil || *state.OwnerTeamID != ownerTeamID {
			t.Fatalf("unexpected applied state: %#v", state)
		}
		resolvedOwner, err := repo.ResolveArtifactOwnerTeam(ctx, plan.ConfigKey)
		if err != nil || resolvedOwner != ownerTeamID {
			t.Fatalf("ResolveArtifactOwnerTeam = %s, %v", resolvedOwner, err)
		}
		otherTeamID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, 'Other owners', $2)`, otherTeamID, "other-owners-"+otherTeamID.String()); err != nil {
			t.Fatalf("create other owner team: %v", err)
		}
		_, err = repo.CreateConfigPlan(ctx, CreateConfigPlanParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeMCP,
			OwnerTeamID: &otherTeamID, SourceHash: "sha256:forged-owner", RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true})
		if !errors.Is(err, ErrArtifactOwnerTeamMismatch) {
			t.Fatalf("owner replacement error = %v, want ErrArtifactOwnerTeamMismatch", err)
		}
	})

	t.Run("apply rejects stale generation without closing plan", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP, SourceHash: "sha256:stale", DesiredState: json.RawMessage(`{"kind":"mcp"}`),
			OwnerTeamID: &ownerTeamID,
			CreatedBy:   accountID, SupersedeExisting: true,
			RequiredPermissions: requiredPermissions,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		_, err = repo.UpsertConfigState(ctx, UpsertConfigStateParams{
			ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP,
			OwnerTeamID: &ownerTeamID, SourceHash: "sha256:newer", UpdatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("UpsertConfigState: %v", err)
		}
		_, err = repo.ApplyConfigPlan(ctx, ApplyConfigPlanParams{
			State: UpsertConfigStateParams{
				ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP,
				SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
			},
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
		})
		if err == nil {
			t.Fatal("expected stale generation to fail")
		}
		pending, fetchErr := repo.GetConfigPlan(ctx, plan.ID)
		if fetchErr != nil || pending.Status != ConfigPlanStatusPending {
			t.Fatalf("expected plan to remain pending, got %#v, %v", pending, fetchErr)
		}
	})

	t.Run("action replacement increments revision", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey:           "workspace:main",
			ConfigType:          ConfigTypeWorkspace,
			SourceHash:          "sha256:workspace",
			CreatedBy:           accountID,
			RequiredPermissions: requiredPermissions,
			SupersedeExisting:   true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		updatedPermissions := testRequiredPermissions(uuid.New())
		updated, err := repo.ReplaceConfigPlanActions(ctx, plan.ID, json.RawMessage(`[{"id":"remove-version","decision":"force_remove"}]`), updatedPermissions, accountID)
		if err != nil {
			t.Fatalf("ReplaceConfigPlanActions: %v", err)
		}
		if updated.Revision != plan.Revision+1 {
			t.Fatalf("expected revision %d, got %d", plan.Revision+1, updated.Revision)
		}
		assertJSONEqual(t, updated.RequiredPermissions, updatedPermissions)
		_, err = repo.ApplyConfigPlan(ctx, ApplyConfigPlanParams{
			State: UpsertConfigStateParams{
				ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace,
				SourceHash: plan.SourceHash, UpdatedBy: accountID,
			},
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision, ApplyLeaseID: uuid.New(),
		})
		if !errors.Is(err, ErrConfigPlanRevisionMismatch) {
			t.Fatalf("apply authorized revision error = %v, want ErrConfigPlanRevisionMismatch", err)
		}
		if state, err := repo.GetConfigState(ctx, plan.ConfigKey); err != nil || state != nil {
			t.Fatalf("revision-mismatched apply wrote state: %#v, %v", state, err)
		}
	})

	t.Run("apply lease blocks action replacement and supports safe retry", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "workspace:leased", ConfigType: ConfigTypeWorkspace,
			SourceHash: "sha256:leased", CreatedBy: accountID,
			RequiredPermissions: requiredPermissions, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		if err != nil {
			t.Fatalf("ReserveConfigPlanApply: %v", err)
		}
		if _, err := repo.ReplaceConfigPlanActions(ctx, plan.ID, json.RawMessage(`[]`), requiredPermissions, accountID); !errors.Is(err, ErrConfigPlanApplyInProgress) {
			t.Fatalf("replacement during apply = %v, want ErrConfigPlanApplyInProgress", err)
		}
		if _, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace, SourceHash: "sha256:replacement",
			CreatedBy: accountID, RequiredPermissions: requiredPermissions, SupersedeExisting: true,
		}); !errors.Is(err, ErrConfigPlanApplyInProgress) {
			t.Fatalf("superseding plan during apply = %v, want ErrConfigPlanApplyInProgress", err)
		}
		if _, err := repo.RenewConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); err != nil {
			t.Fatalf("RenewConfigPlanApply: %v", err)
		}
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); err != nil {
			t.Fatalf("ReleaseConfigPlanApply: %v", err)
		}
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); err != nil {
			t.Fatalf("idempotent ReleaseConfigPlanApply: %v", err)
		}
		retryLease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		if err != nil {
			t.Fatalf("retry ReserveConfigPlanApply: %v", err)
		}
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, retryLease.ID); err != nil {
			t.Fatalf("release retry lease: %v", err)
		}
		updated, err := repo.ReplaceConfigPlanActions(ctx, plan.ID, json.RawMessage(`[]`), requiredPermissions, accountID)
		if err != nil || updated.Revision != plan.Revision+1 {
			t.Fatalf("replacement after release = %#v, %v", updated, err)
		}
	})

	t.Run("lease expiry is calculated by the database clock", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "workspace:database-clock-" + uuid.NewString(), ConfigType: ConfigTypeWorkspace,
			SourceHash: "sha256:database-clock", CreatedBy: accountID,
			RequiredPermissions: requiredPermissions, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		tracer := &leaseQueryTracer{calls: make(chan leaseQueryCall, 2)}
		// Clone the already-scoped pool config so the tracer cannot escape the
		// test's isolated schema by reconnecting with the caller's base URL.
		traceConfig := pool.Config()
		traceConfig.ConnConfig.Tracer = tracer
		tracedPool, err := pgxpool.NewWithConfig(ctx, traceConfig)
		if err != nil {
			t.Fatalf("open traced database pool: %v", err)
		}
		defer tracedPool.Close()
		tracedRepo := NewPostgresConfigRepository(tracedPool)

		var reserveStartedAt, reserveFinishedAt time.Time
		if err := pool.QueryRow(ctx, `SELECT NOW()`).Scan(&reserveStartedAt); err != nil {
			t.Fatalf("read database clock before reserve: %v", err)
		}
		lease, err := tracedRepo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		if err != nil {
			t.Fatalf("ReserveConfigPlanApply: %v", err)
		}
		assertLeaseQueryUsesDatabaseClock(t, nextLeaseQuery(t, tracer.calls))
		if err := pool.QueryRow(ctx, `SELECT NOW()`).Scan(&reserveFinishedAt); err != nil {
			t.Fatalf("read database clock after reserve: %v", err)
		}
		assertDatabaseLeaseExpiry(t, lease.ExpiresAt, reserveStartedAt, reserveFinishedAt)
		assertStoredLeaseExpiry(t, ctx, pool, plan.ID, lease.ExpiresAt)

		if _, err := pool.Exec(ctx, `
			UPDATE fused_config_plans
			SET apply_lease_expires_at = NOW() + INTERVAL '1 minute'
			WHERE id = $1
		`, plan.ID); err != nil {
			t.Fatalf("shorten lease before renewal: %v", err)
		}
		var renewStartedAt, renewFinishedAt time.Time
		if err := pool.QueryRow(ctx, `SELECT NOW()`).Scan(&renewStartedAt); err != nil {
			t.Fatalf("read database clock before renew: %v", err)
		}
		renewed, err := tracedRepo.RenewConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID)
		if err != nil {
			t.Fatalf("RenewConfigPlanApply: %v", err)
		}
		assertLeaseQueryUsesDatabaseClock(t, nextLeaseQuery(t, tracer.calls))
		if err := pool.QueryRow(ctx, `SELECT NOW()`).Scan(&renewFinishedAt); err != nil {
			t.Fatalf("read database clock after renew: %v", err)
		}
		assertDatabaseLeaseExpiry(t, renewed.ExpiresAt, renewStartedAt, renewFinishedAt)
		assertStoredLeaseExpiry(t, ctx, pool, plan.ID, renewed.ExpiresAt)
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); err != nil {
			t.Fatalf("ReleaseConfigPlanApply: %v", err)
		}
	})

	t.Run("concurrent lease reservation has one fenced winner", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "workspace:lease-race-" + uuid.NewString(), ConfigType: ConfigTypeWorkspace,
			SourceHash: "sha256:lease-race", CreatedBy: accountID,
			RequiredPermissions: requiredPermissions, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		start := make(chan struct{})
		leases := make([]*ConfigPlanApplyLease, 2)
		errs := make([]error, len(leases))
		var wait sync.WaitGroup
		for i := range leases {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				<-start
				leases[index], errs[index] = repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
			}(i)
		}
		close(start)
		wait.Wait()

		var winner *ConfigPlanApplyLease
		for i, reserveErr := range errs {
			if reserveErr == nil {
				if winner != nil {
					t.Fatalf("multiple reservations succeeded: %#v", leases)
				}
				winner = leases[i]
				continue
			}
			if !errors.Is(reserveErr, ErrConfigPlanApplyInProgress) {
				t.Fatalf("reservation loser error = %v, want ErrConfigPlanApplyInProgress", reserveErr)
			}
		}
		if winner == nil {
			t.Fatalf("no reservation succeeded: %v", errs)
		}
		var storedLeaseID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT apply_lease_id FROM fused_config_plans WHERE id = $1`, plan.ID).Scan(&storedLeaseID); err != nil {
			t.Fatalf("read winning lease: %v", err)
		}
		if storedLeaseID != winner.ID {
			t.Fatalf("stored lease = %s, want winner %s", storedLeaseID, winner.ID)
		}
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, winner.ID); err != nil {
			t.Fatalf("release winning lease: %v", err)
		}
	})

	t.Run("expired apply lease cannot be renewed and is recoverable", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "workspace:expired-lease", ConfigType: ConfigTypeWorkspace,
			SourceHash: "sha256:expired", CreatedBy: accountID,
			RequiredPermissions: requiredPermissions, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		if err != nil {
			t.Fatalf("ReserveConfigPlanApply: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE fused_config_plans SET apply_lease_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, plan.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		if _, err := repo.RenewConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); !errors.Is(err, ErrConfigPlanRevisionMismatch) {
			t.Fatalf("renew expired lease = %v, want revision mismatch", err)
		}
		replacementLease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		if err != nil {
			t.Fatalf("reserve after crashed lease expiry: %v", err)
		}
		_, err = repo.ApplyConfigPlan(ctx, ApplyConfigPlanParams{
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision, ApplyLeaseID: lease.ID,
			State: UpsertConfigStateParams{
				ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace, SourceHash: plan.SourceHash,
				DesiredState: json.RawMessage(`{}`), ManagedResources: json.RawMessage(`{}`), UpdatedBy: accountID,
			},
		})
		if !errors.Is(err, ErrConfigPlanRevisionMismatch) {
			t.Fatalf("expired holder finalization = %v, want fencing rejection", err)
		}
		if state, stateErr := repo.GetConfigState(ctx, plan.ConfigKey); stateErr != nil || state != nil {
			t.Fatalf("expired holder wrote config state: %#v, %v", state, stateErr)
		}
		if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, replacementLease.ID); err != nil {
			t.Fatalf("release replacement lease: %v", err)
		}
	})

	t.Run("webhook apply persists a large reconciliation batch", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "webhook:large-batch", ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
			SourceHash: "sha256:webhook-large-batch", DesiredState: json.RawMessage(`{"name":"large-batch"}`),
			RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		const batchSize = 512
		ownerKey := plan.ConfigKey
		registrations := make([]WorkspaceWebhook, batchSize)
		serviceIDs := make([]uuid.UUID, batchSize)
		for i := range registrations {
			serviceIDs[i] = uuid.New()
			registrations[i] = WorkspaceWebhook{
				ServiceID: serviceIDs[i], ServiceVersionID: uuid.New(), Label: "large-batch",
				Slug: "large-" + uuid.NewString(), OwningConfigKey: ownerKey,
			}
		}
		result, err := repo.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State: UpsertConfigStateParams{
					ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
					SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
				},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
			},
			Registrations: registrations, KeepServiceIDs: serviceIDs,
		})
		if err != nil {
			t.Fatalf("ApplyWebhookConfigPlan: %v", err)
		}
		if len(result.Registrations) != batchSize || result.Registrations[0].ServiceID != serviceIDs[0] || result.Registrations[batchSize-1].ServiceID != serviceIDs[batchSize-1] {
			t.Fatalf("large batch result did not preserve size/order: len=%d", len(result.Registrations))
		}
		var persisted int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_webhooks WHERE owning_config_key = $1`, plan.ConfigKey).Scan(&persisted); err != nil || persisted != batchSize {
			t.Fatalf("persisted large batch = %d, %v; want %d", persisted, err, batchSize)
		}
	})

	t.Run("concurrent webhook apply has one coherent winner", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "webhook:concurrent-" + uuid.NewString(), ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
			SourceHash: "sha256:webhook-concurrent", DesiredState: json.RawMessage(`{"name":"concurrent"}`),
			RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		serviceID := uuid.New()
		params := ApplyWebhookConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State: UpsertConfigStateParams{
					ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
					SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
				},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
			},
			Registrations: []WorkspaceWebhook{{
				ServiceID: serviceID, ServiceVersionID: uuid.New(), Label: "concurrent",
				Slug: "concurrent-" + uuid.NewString(), OwningConfigKey: plan.ConfigKey,
			}},
			KeepServiceIDs: []uuid.UUID{serviceID},
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wait sync.WaitGroup
		for i := range errs {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				<-start
				local := params
				local.Registrations = append([]WorkspaceWebhook(nil), params.Registrations...)
				local.KeepServiceIDs = append([]uuid.UUID(nil), params.KeepServiceIDs...)
				_, errs[index] = repo.ApplyWebhookConfigPlan(ctx, local)
			}(i)
		}
		close(start)
		wait.Wait()
		succeeded := 0
		for _, applyErr := range errs {
			if applyErr == nil {
				succeeded++
				continue
			}
			if !errors.Is(applyErr, ErrConfigPlanNotFound) && !errors.Is(applyErr, ErrConfigPlanRevisionMismatch) && !strings.Contains(applyErr.Error(), "config generation changed") {
				t.Fatalf("concurrent loser error = %v", applyErr)
			}
		}
		if succeeded != 1 {
			t.Fatalf("successful concurrent applies = %d, errors=%v", succeeded, errs)
		}
		var registrations, states, applied, generation int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_webhooks WHERE owning_config_key = $1`, plan.ConfigKey).Scan(&registrations); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MAX(generation), 0) FROM fused_config_states WHERE config_key = $1`, plan.ConfigKey).Scan(&states, &generation); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_config_plans WHERE id = $1 AND status = 'applied'`, plan.ID).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		if registrations != 1 || states != 1 || generation != 1 || applied != 1 {
			t.Fatalf("incoherent winner registrations=%d states=%d generation=%d applied=%d", registrations, states, generation, applied)
		}
	})

	t.Run("competing webhook owners have one atomic winner", func(t *testing.T) {
		serviceID := uuid.New()
		plans := make([]*ConfigPlan, 2)
		for i := range plans {
			var err error
			plans[i], err = repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
				ConfigKey: "webhook:competing-" + uuid.NewString(), ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
				SourceHash: "sha256:webhook-competing", DesiredState: json.RawMessage(`{"name":"competing"}`),
				RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
			})
			if err != nil {
				t.Fatalf("CreateConfigPlan[%d]: %v", i, err)
			}
		}

		start := make(chan struct{})
		errs := make([]error, len(plans))
		var wait sync.WaitGroup
		for i, plan := range plans {
			wait.Add(1)
			go func(index int, candidate *ConfigPlan) {
				defer wait.Done()
				<-start
				_, errs[index] = repo.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{
					Plan: ApplyConfigPlanParams{
						State: UpsertConfigStateParams{
							ConfigKey: candidate.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
							SourceHash: candidate.SourceHash, DesiredState: candidate.DesiredState, UpdatedBy: accountID,
						},
						PlanID: candidate.ID, BaseGeneration: candidate.BaseGeneration, ExpectedRevision: candidate.Revision,
					},
					Registrations: []WorkspaceWebhook{{
						ServiceID: serviceID, ServiceVersionID: uuid.New(), Label: "competing",
						Slug: "competing-" + uuid.NewString(), OwningConfigKey: candidate.ConfigKey,
					}},
					KeepServiceIDs: []uuid.UUID{serviceID},
				})
			}(i, plan)
		}
		close(start)
		wait.Wait()

		winner := -1
		for i, applyErr := range errs {
			if applyErr == nil {
				if winner >= 0 {
					t.Fatalf("both competing owners succeeded: %v", errs)
				}
				winner = i
				continue
			}
			if !errors.Is(applyErr, ErrWorkspaceWebhookOwnerConflict) {
				t.Fatalf("competing loser error = %v, want owner conflict", applyErr)
			}
		}
		if winner < 0 {
			t.Fatalf("no competing owner succeeded: %v", errs)
		}

		var persistedOwner string
		if err := pool.QueryRow(ctx, `SELECT owning_config_key FROM fused_workspace_webhooks WHERE service_id = $1 AND label = 'competing'`, serviceID).Scan(&persistedOwner); err != nil {
			t.Fatalf("load winning webhook: %v", err)
		}
		if persistedOwner != plans[winner].ConfigKey {
			t.Fatalf("persisted owner = %q, want %q", persistedOwner, plans[winner].ConfigKey)
		}
		loser := 1 - winner
		var winnerStates, loserStates int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_config_states WHERE config_key = $1`, plans[winner].ConfigKey).Scan(&winnerStates); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_config_states WHERE config_key = $1`, plans[loser].ConfigKey).Scan(&loserStates); err != nil {
			t.Fatal(err)
		}
		loserPlan, err := repo.GetConfigPlan(ctx, plans[loser].ID)
		if winnerStates != 1 || loserStates != 0 || err != nil || loserPlan.Status != ConfigPlanStatusPending {
			t.Fatalf("competing rollback mismatch: winner_states=%d loser_states=%d loser_plan=%#v err=%v", winnerStates, loserStates, loserPlan, err)
		}
	})

	t.Run("webhook batch rejects duplicate persisted identities", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "webhook:duplicate-batch", ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
			SourceHash: "sha256:webhook-duplicate-batch", DesiredState: json.RawMessage(`{"name":"duplicate-batch"}`),
			RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		ownerKey, serviceID := plan.ConfigKey, uuid.New()
		firstSlug, discardedSlug := "first-"+uuid.NewString(), "second-"+uuid.NewString()
		_, err = repo.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State: UpsertConfigStateParams{
					ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
					SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
				},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
			},
			Registrations: []WorkspaceWebhook{
				{ServiceID: serviceID, ServiceVersionID: uuid.New(), Label: "duplicate", Slug: firstSlug, AuthType: "first", OwningConfigKey: ownerKey},
				{ServiceID: serviceID, ServiceVersionID: uuid.New(), Label: "duplicate", Slug: discardedSlug, AuthType: "second", OwningConfigKey: ownerKey},
			},
			KeepServiceIDs: []uuid.UUID{serviceID},
		})
		if !errors.Is(err, ErrWorkspaceWebhookDuplicate) {
			t.Fatalf("ApplyWebhookConfigPlan = %v, want ErrWorkspaceWebhookDuplicate", err)
		}
		var persisted int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_webhooks WHERE service_id = $1 AND label = 'duplicate'`, serviceID).Scan(&persisted); err != nil || persisted != 0 {
			t.Fatalf("duplicate batch persisted %d rows: %v", persisted, err)
		}
	})

	t.Run("webhook apply rolls back every registration when one conflicts", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "webhook:atomic", ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
			SourceHash: "sha256:webhook-atomic", DesiredState: json.RawMessage(`{"name":"atomic"}`),
			RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		firstServiceID, conflictServiceID := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO fused_workspace_webhooks (service_id, service_version_id, label, slug, owning_config_key)
			VALUES ($1, $2, 'atomic', $3, 'webhook:other')
		`, conflictServiceID, uuid.New(), "existing-"+uuid.NewString()); err != nil {
			t.Fatalf("seed conflicting webhook: %v", err)
		}
		ownerKey := plan.ConfigKey
		_, err = repo.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State: UpsertConfigStateParams{
					ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &ownerTeamID,
					SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
				},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
			},
			Registrations: []WorkspaceWebhook{
				{ServiceID: firstServiceID, ServiceVersionID: uuid.New(), Label: "atomic", Slug: "first-" + uuid.NewString(), OwningConfigKey: ownerKey},
				{ServiceID: conflictServiceID, ServiceVersionID: uuid.New(), Label: "atomic", Slug: "second-" + uuid.NewString(), OwningConfigKey: ownerKey},
			},
			KeepServiceIDs: []uuid.UUID{firstServiceID, conflictServiceID},
		})
		if !errors.Is(err, ErrWorkspaceWebhookOwnerConflict) {
			t.Fatalf("ApplyWebhookConfigPlan error = %v, want owner conflict", err)
		}
		var firstCount, stateCount int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_webhooks WHERE service_id = $1`, firstServiceID).Scan(&firstCount); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_config_states WHERE config_key = $1`, plan.ConfigKey).Scan(&stateCount); err != nil {
			t.Fatal(err)
		}
		pending, fetchErr := repo.GetConfigPlan(ctx, plan.ID)
		if firstCount != 0 || stateCount != 0 || fetchErr != nil || pending.Status != ConfigPlanStatusPending {
			t.Fatalf("partial webhook apply persisted: webhook=%d state=%d plan=%#v err=%v", firstCount, stateCount, pending, fetchErr)
		}
	})

	t.Run("webhook apply rejects archived owner before writes", func(t *testing.T) {
		archivedTeamID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO fused_teams (id, name, slug, status) VALUES ($1, 'Archived owners', $2, 'archived')`, archivedTeamID, "archived-owners-"+archivedTeamID.String()); err != nil {
			t.Fatalf("create archived owner: %v", err)
		}
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "webhook:archived", ConfigType: ConfigTypeWebhook, OwnerTeamID: &archivedTeamID,
			SourceHash: "sha256:webhook-archived", DesiredState: json.RawMessage(`{"name":"archived"}`),
			RequiredPermissions: requiredPermissions, CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		ownerKey, serviceID := plan.ConfigKey, uuid.New()
		_, err = repo.ApplyWebhookConfigPlan(ctx, ApplyWebhookConfigPlanParams{
			Plan: ApplyConfigPlanParams{
				State:  UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWebhook, OwnerTeamID: &archivedTeamID, SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID},
				PlanID: plan.ID, BaseGeneration: plan.BaseGeneration, ExpectedRevision: plan.Revision,
			},
			Registrations:  []WorkspaceWebhook{{ServiceID: serviceID, ServiceVersionID: uuid.New(), Label: "archived", Slug: "archived-" + uuid.NewString(), OwningConfigKey: ownerKey}},
			KeepServiceIDs: []uuid.UUID{serviceID},
		})
		if !errors.Is(err, ErrConfigOwnerTeamInactive) {
			t.Fatalf("ApplyWebhookConfigPlan error = %v, want inactive owner", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_webhooks WHERE owning_config_key = $1`, plan.ConfigKey).Scan(&count); err != nil || count != 0 {
			t.Fatalf("inactive owner wrote %d registrations: %v", count, err)
		}
	})

	t.Run("workspace notification dedupes by plan action and version", func(t *testing.T) {
		serviceID := uuid.New()
		planID := uuid.New()
		first, err := repo.CreateWorkspaceNotification(ctx, CreateWorkspaceNotificationParams{
			Type:      WorkspaceNotificationTypeVersionRemoved,
			Severity:  WorkspaceNotificationSeverityBreaking,
			ServiceID: &serviceID,
			Version:   "2026-07-01",
			ConfigKey: "sdk:security",
			Message:   "first removal",
			Metadata:  json.RawMessage(`{"plan_id":"` + planID.String() + `","action_id":"remove:2026-07-01"}`),
			CreatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("CreateWorkspaceNotification first: %v", err)
		}
		duplicate, err := repo.CreateWorkspaceNotification(ctx, CreateWorkspaceNotificationParams{
			Type:      WorkspaceNotificationTypeVersionRemoved,
			Severity:  WorkspaceNotificationSeverityBreaking,
			ServiceID: &serviceID,
			Version:   "2026-07-01",
			ConfigKey: "sdk:security",
			Message:   "duplicate removal",
			Metadata:  json.RawMessage(`{"plan_id":"` + planID.String() + `","action_id":"remove:2026-07-01"}`),
			CreatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("CreateWorkspaceNotification duplicate: %v", err)
		}
		if duplicate.ID != first.ID {
			t.Fatalf("expected duplicate to return existing notification %s, got %s", first.ID, duplicate.ID)
		}
		secondVersion, err := repo.CreateWorkspaceNotification(ctx, CreateWorkspaceNotificationParams{
			Type:      WorkspaceNotificationTypeVersionRemoved,
			Severity:  WorkspaceNotificationSeverityBreaking,
			ServiceID: &serviceID,
			Version:   "2026-08-01",
			ConfigKey: "sdk:platform",
			Message:   "second version removal",
			Metadata:  json.RawMessage(`{"plan_id":"` + planID.String() + `","action_id":"remove:2026-08-01"}`),
			CreatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("CreateWorkspaceNotification second version: %v", err)
		}
		if secondVersion.ID == first.ID {
			t.Fatalf("expected distinct notification for different version action, got %s", secondVersion.ID)
		}
	})

	t.Run("update workspace notification status enforces the state machine", func(t *testing.T) {
		serviceID := uuid.New()
		resolvedBy := uuid.New()
		note, err := repo.CreateWorkspaceNotification(ctx, CreateWorkspaceNotificationParams{
			Type:      WorkspaceNotificationTypeRegistryVersionAdded,
			Severity:  WorkspaceNotificationSeverityNonBreaking,
			ServiceID: &serviceID,
			Version:   "2026-09-01",
			ConfigKey: "sdk:status-machine",
			Message:   "a new version was published",
			Metadata:  json.RawMessage(`{"registry_changelog_id":"` + uuid.New().String() + `"}`),
			CreatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("CreateWorkspaceNotification: %v", err)
		}

		if _, err := repo.UpdateWorkspaceNotificationStatus(ctx, note.ID, WorkspaceNotificationStatusPending, resolvedBy); !errors.Is(err, ErrWorkspaceNotificationStatusInvalid) {
			t.Fatalf("expected ErrWorkspaceNotificationStatusInvalid for target=pending, got %v", err)
		}

		acked, err := repo.UpdateWorkspaceNotificationStatus(ctx, note.ID, WorkspaceNotificationStatusAcknowledged, resolvedBy)
		if err != nil {
			t.Fatalf("pending -> acknowledged: %v", err)
		}
		if acked.Status != WorkspaceNotificationStatusAcknowledged {
			t.Fatalf("expected status acknowledged, got %s", acked.Status)
		}
		if acked.ResolvedBy == nil || *acked.ResolvedBy != resolvedBy {
			t.Fatalf("expected resolved_by %s, got %v", resolvedBy, acked.ResolvedBy)
		}

		dismissed, err := repo.UpdateWorkspaceNotificationStatus(ctx, note.ID, WorkspaceNotificationStatusDismissed, resolvedBy)
		if err != nil {
			t.Fatalf("acknowledged -> dismissed: %v", err)
		}
		if dismissed.Status != WorkspaceNotificationStatusDismissed {
			t.Fatalf("expected status dismissed, got %s", dismissed.Status)
		}

		if _, err := repo.UpdateWorkspaceNotificationStatus(ctx, note.ID, WorkspaceNotificationStatusAcknowledged, resolvedBy); !errors.Is(err, ErrWorkspaceNotificationImmutable) {
			t.Fatalf("expected ErrWorkspaceNotificationImmutable for dismissed -> acknowledged, got %v", err)
		}

		if _, err := repo.UpdateWorkspaceNotificationStatus(ctx, uuid.New(), WorkspaceNotificationStatusAcknowledged, resolvedBy); !errors.Is(err, ErrWorkspaceNotificationNotFound) {
			t.Fatalf("expected ErrWorkspaceNotificationNotFound for an unknown id, got %v", err)
		}
	})
}
