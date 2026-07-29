package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

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
		ConfigKey:         "sdk:test",
		ConfigType:        ConfigTypeSDK,
		SourceHash:        "sha256:test",
		Actions:           json.RawMessage(`  []`),
		ResolvedPayload:   json.RawMessage(`  {"ok":true}`),
		Blockers:          json.RawMessage(`  []`),
		Warnings:          json.RawMessage(`  []`),
		SupersedeExisting: false,
	}
	if err := validatePlanParams(&whitespacePlan); err != nil {
		t.Fatalf("expected whitespace-padded JSON to validate, got %v", err)
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

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	defer pool.Close()

	accountID := uuid.New()
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
	_, err = store.BootstrapWorkspace(ctx, accountID, "Config Repo Test")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}

	repo := NewPostgresConfigRepository(pool)
	t.Run("state upsert increments generation", func(t *testing.T) {
		params := UpsertConfigStateParams{
			ConfigKey:        "sdk:security",
			ConfigType:       ConfigTypeSDK,
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

	t.Run("new plan supersedes previous pending", func(t *testing.T) {
		first, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey:         "sdk:identity",
			ConfigType:        ConfigTypeSDK,
			SourceHash:        "sha256:first",
			Actions:           json.RawMessage(`[{"id":"remove-okta"}]`),
			ResolvedPayload:   json.RawMessage(`{"language":"typescript"}`),
			CreatedBy:         accountID,
			SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("first CreateConfigPlan: %v", err)
		}
		second, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey:         "sdk:identity",
			ConfigType:        ConfigTypeSDK,
			SourceHash:        "sha256:second",
			CreatedBy:         accountID,
			SupersedeExisting: true,
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
			CreatedBy: accountID, SupersedeExisting: true,
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
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration,
		})
		if err != nil {
			t.Fatalf("ApplyConfigPlan: %v", err)
		}
		applied, err := repo.GetConfigPlan(ctx, plan.ID)
		if err != nil || applied.Status != ConfigPlanStatusApplied {
			t.Fatalf("expected applied plan, got %#v, %v", applied, err)
		}
		if state.ConfigType != ConfigTypeMCP || state.LatestResourceID == nil || *state.LatestResourceID != runtimeID {
			t.Fatalf("unexpected applied state: %#v", state)
		}
	})

	t.Run("apply rejects stale generation without closing plan", func(t *testing.T) {
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{
			ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP, SourceHash: "sha256:stale", DesiredState: json.RawMessage(`{"kind":"mcp"}`),
			CreatedBy: accountID, SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		_, err = repo.UpsertConfigState(ctx, UpsertConfigStateParams{
			ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP,
			SourceHash: "sha256:newer", UpdatedBy: accountID,
		})
		if err != nil {
			t.Fatalf("UpsertConfigState: %v", err)
		}
		_, err = repo.ApplyConfigPlan(ctx, ApplyConfigPlanParams{
			State: UpsertConfigStateParams{
				ConfigKey: "test-mcp", ConfigType: ConfigTypeMCP,
				SourceHash: plan.SourceHash, DesiredState: plan.DesiredState, UpdatedBy: accountID,
			},
			PlanID: plan.ID, BaseGeneration: plan.BaseGeneration,
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
			ConfigKey:         "workspace:main",
			ConfigType:        ConfigTypeWorkspace,
			SourceHash:        "sha256:workspace",
			CreatedBy:         accountID,
			SupersedeExisting: true,
		})
		if err != nil {
			t.Fatalf("CreateConfigPlan: %v", err)
		}
		updated, err := repo.ReplaceConfigPlanActions(ctx, plan.ID, json.RawMessage(`[{"id":"remove-version","decision":"force_remove"}]`), accountID)
		if err != nil {
			t.Fatalf("ReplaceConfigPlanActions: %v", err)
		}
		if updated.Revision != plan.Revision+1 {
			t.Fatalf("expected revision %d, got %d", plan.Revision+1, updated.Revision)
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
