package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresStore(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	accountID := uuid.New()
	serviceID := uuid.New()
	version := "1.0"

	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("reset singleton workspace: %v", err)
	}

	t.Run("StableEngineInstallationIdentity", func(t *testing.T) {
		identityStore, ok := s.(EngineInstallationStore)
		if !ok {
			t.Fatal("store does not expose Engine installation identity")
		}
		first, err := identityStore.LoadEngineInstallationID(ctx)
		if err != nil {
			t.Fatalf("LoadEngineInstallationID: %v", err)
		}
		second, err := identityStore.LoadEngineInstallationID(ctx)
		if err != nil {
			t.Fatalf("LoadEngineInstallationID second read: %v", err)
		}
		if first == uuid.Nil || first != second {
			t.Fatalf("expected one stable installation ID, got %s and %s", first, second)
		}
	})

	t.Run("BootstrapWorkspace", func(t *testing.T) {
		wsID1, err := s.BootstrapWorkspace(ctx, accountID, "Test Workspace")
		if err != nil {
			t.Fatalf("failed to bootstrap workspace: %v", err)
		}

		// Should be idempotent
		wsID2, err := s.BootstrapWorkspace(ctx, accountID, "Test Workspace")
		if err != nil {
			t.Fatalf("failed to fetch existing workspace: %v", err)
		}

		if wsID1 != wsID2 {
			t.Errorf("expected idempotent workspace bootstrap, got different IDs: %s != %s", wsID1, wsID2)
		}

		t.Run("AddWorkspaceServiceVersion", func(t *testing.T) {
			if err := s.AddWorkspaceServiceVersion(ctx, serviceID, "", version, uuid.Nil, "Test Service", accountID); err != nil {
				t.Fatalf("failed to activate service: %v", err)
			}
			// Idempotent
			if err := s.AddWorkspaceServiceVersion(ctx, serviceID, "", version, uuid.Nil, "Test Service", accountID); err != nil {
				t.Fatalf("failed to activate service idempotently: %v", err)
			}
		})
	})

	t.Run("LoadDefaultBucketID", func(t *testing.T) {
		loader, ok := s.(interface {
			LoadDefaultBucketID(context.Context) (uuid.UUID, error)
		})
		if !ok {
			t.Fatal("store does not expose default bucket point lookup")
		}
		bucketID, err := loader.LoadDefaultBucketID(ctx)
		if err != nil || bucketID == uuid.Nil {
			t.Fatalf("LoadDefaultBucketID = %s, %v", bucketID, err)
		}
	})

	t.Run("GetSDKAccountID", func(t *testing.T) {
		appFamilyID, appID := uuid.New(), uuid.New()
		ownerTeamID := seedAppOwnerTeam(t, ctx, pool)
		_, err := pool.Exec(ctx, `
			INSERT INTO fused_app_families
				(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
			VALUES ($1, $2, 'sdk', $3, 'Account lookup SDK', 'typescript', $4)
		`, appFamilyID, accountID, "account-lookup-"+appFamilyID.String(), ownerTeamID)
		if err != nil {
			t.Fatalf("failed to insert test sdk family: %v", err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, status)
			VALUES ($3, $1, $2, '1.0.0', $4, 'account-lookup', 'active')
		`, appFamilyID, accountID, appID, "sdk:account-lookup:"+appFamilyID.String())
		if err != nil {
			t.Fatalf("failed to insert test sdk: %v", err)
		}

		fetchedAccountID, err := s.GetSDKAccountID(ctx, appID)
		if err != nil {
			t.Fatalf("failed to get sdk account id: %v", err)
		}
		if fetchedAccountID != accountID {
			t.Errorf("expected %s, got %s", accountID, fetchedAccountID)
		}

		_, err = s.GetSDKAccountID(ctx, uuid.New())
		if err == nil {
			t.Errorf("expected error for invalid sdk id, got nil")
		}
	})
}

func TestValidateWorkspaceOwner(t *testing.T) {
	accountID := uuid.New()
	if err := validateWorkspaceOwner(accountID, accountID); err != nil {
		t.Fatalf("expected matching owner to pass, got %v", err)
	}
	if err := validateWorkspaceOwner(accountID, uuid.New()); !errors.Is(err, ErrWorkspaceOwnerMismatch) {
		t.Fatalf("expected ErrWorkspaceOwnerMismatch, got %v", err)
	}
}

func TestBootstrapWorkspace_RegistryOwnsAccountAndEngineIsSingleton(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("reset singleton workspace: %v", err)
	}
	freshAccountID := uuid.New()

	wsID, err := s.BootstrapWorkspace(ctx, freshAccountID, "Fresh Account Workspace")
	if err != nil {
		t.Fatalf("BootstrapWorkspace: %v", err)
	}
	if wsID == uuid.Nil {
		t.Error("expected a non-nil workspace ID")
	}
	if _, err := accesscontrol.BootstrapOwner(ctx, s.(accesscontrol.BootstrapRepository), freshAccountID, "registry-issued-key"); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}

	var accountTableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.fused_accounts') IS NOT NULL`).Scan(&accountTableExists); err != nil {
		t.Fatalf("check local account table: %v", err)
	}
	if accountTableExists {
		t.Fatal("Engine must not contain a Registry account projection table")
	}
	var legacyAPIKeyTableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.fused_api_keys') IS NOT NULL`).Scan(&legacyAPIKeyTableExists); err != nil {
		t.Fatalf("check legacy API-key table: %v", err)
	}
	if legacyAPIKeyTableExists {
		t.Fatal("Engine must not contain the removed fused_api_keys table")
	}

	_, err = s.BootstrapWorkspace(ctx, uuid.New(), "Second Workspace")
	if !errors.Is(err, ErrWorkspaceOwnerMismatch) {
		t.Fatalf("expected ErrWorkspaceOwnerMismatch, got %v", err)
	}

	var workspaceRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspaces`).Scan(&workspaceRows); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if workspaceRows != 1 {
		t.Fatalf("expected exactly one Engine workspace, got %d", workspaceRows)
	}
}

// TestRuntimeEntitlementRoundTrip verifies PostgreSQL preserves the complete Registry contract including API capacity.
func TestRuntimeEntitlementRoundTrip(t *testing.T) {
	ctx, cancel, pool, s := runtimeReportingTestStore(t)
	defer cancel()
	defer pool.Close()

	entitlementStore := s.(interface {
		SaveRuntimeEntitlement(context.Context, models.RuntimeEntitlement) error
		GetRuntimeEntitlement(context.Context) (models.RuntimeEntitlement, error)
	})
	entitlement := models.DefaultRuntimeEntitlement()
	entitlement.EntitlementRevision = "enterprise-revision"
	entitlement.Plan = "enterprise"
	entitlement.HeartbeatIntervalSeconds = 15
	entitlement.MaxBuckets = models.IntPtr(5)
	entitlement.MaxAPIFamilies = models.IntPtr(4)
	entitlement.MaxSDKFamilies = models.IntPtr(3)
	entitlement.MaxMCPFamilies = models.IntPtr(3)
	entitlement.MaxServices = models.IntPtr(10)
	entitlement.MaxSandboxConcurrency = models.IntPtr(20)
	entitlement.DriftMonitoringEnabled = true
	entitlement.WebhookIngestionEnabled = true
	entitlement.PublicServiceInsightsEnabled = true
	entitlement.SSOEnabled = true
	entitlement.ExecutionRetentionDays = models.IntPtr(90)
	if err := entitlementStore.SaveRuntimeEntitlement(ctx, entitlement); err != nil {
		t.Fatalf("SaveRuntimeEntitlement: %v", err)
	}
	got, err := entitlementStore.GetRuntimeEntitlement(ctx)
	if err != nil {
		t.Fatalf("GetRuntimeEntitlement: %v", err)
	}
	if got.Plan != "enterprise" || got.EntitlementRevision != "enterprise-revision" || got.HeartbeatIntervalSeconds != 15 {
		t.Fatalf("unexpected entitlement: %#v", got)
	}
	if *got.MaxBuckets != 5 || *got.MaxAPIFamilies != 4 || *got.MaxSDKFamilies != 3 || *got.MaxMCPFamilies != 3 || *got.MaxServices != 10 || *got.MaxSandboxConcurrency != 20 {
		t.Fatalf("unexpected capability limits: %#v", got)
	}
	assertRuntimeEntitlementFeatureGates(t, got)
	if *got.ExecutionRetentionDays != 90 {
		t.Fatalf("unexpected retention days: %#v", got)
	}
}

func assertRuntimeEntitlementFeatureGates(t *testing.T, entitlement models.RuntimeEntitlement) {
	t.Helper()
	if !entitlement.DriftMonitoringEnabled || !entitlement.WebhookIngestionEnabled || !entitlement.PublicServiceInsightsEnabled || !entitlement.SSOEnabled {
		t.Fatalf("unexpected feature gates: %#v", entitlement)
	}
}

// TestExecutionEventHasCommercialUsage excludes logical summaries and inbound deliveries from provider accounting.
func TestExecutionEventHasCommercialUsage(t *testing.T) {
	base := models.EngineExecutionEvent{Direction: models.EngineExecutionDirectionOutbound, Status: models.EngineExecutionStatusSuccess}
	for _, test := range []struct {
		name  string
		event models.EngineExecutionEvent
		want  bool
	}{
		{name: "outbound physical", event: base, want: true},
		{name: "failed outbound physical", event: models.EngineExecutionEvent{Direction: models.EngineExecutionDirectionOutbound, Status: models.EngineExecutionStatusFailed}, want: true},
		{name: "logical parent", event: models.EngineExecutionEvent{Direction: models.EngineExecutionDirectionOutbound, Status: models.EngineExecutionStatusSuccess, ExecutionKind: "unified"}},
		{name: "inbound webhook", event: models.EngineExecutionEvent{Direction: models.EngineExecutionDirectionInbound, Status: models.EngineExecutionStatusSuccess}},
		{name: "unknown outcome", event: models.EngineExecutionEvent{Direction: models.EngineExecutionDirectionOutbound, Status: "pending"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Each event shape must remain on its intended commercial boundary.
			if got := executionEventHasCommercialUsage(test.event); got != test.want {
				t.Fatalf("executionEventHasCommercialUsage() = %t, want %t", got, test.want)
			}
		})
	}
}

// TestRuntimeUsageReportsAggregateAndLateIncrements proves receipt redelivery is idempotent while later executions form fresh reports.
func TestRuntimeUsageReportsAggregateAndLateIncrements(t *testing.T) {
	ctx, cancel, pool, s := runtimeReportingTestStore(t)
	defer cancel()
	defer pool.Close()

	usageStore := s.(interface {
		BatchCreateEngineExecutionEventsAndUsage(context.Context, []models.EngineExecutionEvent, bool) error
		ListPendingRuntimeUsageReports(context.Context, int) ([]models.EngineUsageReport, error)
		MarkRuntimeUsageReportsFlushed(context.Context, []uuid.UUID, time.Time) error
	})
	bucketStart := time.Now().UTC().Truncate(time.Minute)
	events := []models.EngineExecutionEvent{
		runtimeUsageExecutionEvent(uuid.New(), models.EngineExecutionStatusSuccess, bucketStart),
		runtimeUsageExecutionEvent(uuid.New(), models.EngineExecutionStatusSuccess, bucketStart),
		runtimeUsageExecutionEvent(uuid.New(), models.EngineExecutionStatusFailed, bucketStart),
	}
	if err := usageStore.BatchCreateEngineExecutionEventsAndUsage(ctx, events, true); err != nil {
		t.Fatalf("BatchCreateEngineExecutionEventsAndUsage: %v", err)
	}
	pending := pendingRuntimeUsageReports(t, ctx, usageStore)
	assertRuntimeUsageCounts(t, pending, map[string]int64{
		models.EngineUsageMetricExecutionTotal: 3, models.EngineUsageMetricExecutionSuccess: 2, models.EngineUsageMetricExecutionFailed: 1,
	})
	// A JetStream redelivery must update the receipt without charging the event IDs again.
	if err := usageStore.BatchCreateEngineExecutionEventsAndUsage(ctx, events, true); err != nil {
		t.Fatalf("replay execution events: %v", err)
	}
	assertRuntimeUsageCounts(t, pendingRuntimeUsageReports(t, ctx, usageStore), map[string]int64{
		models.EngineUsageMetricExecutionTotal: 3, models.EngineUsageMetricExecutionSuccess: 2, models.EngineUsageMetricExecutionFailed: 1,
	})
	// A disabled entitlement still preserves Activity without creating commercial counters.
	disabled := runtimeUsageExecutionEvent(uuid.New(), models.EngineExecutionStatusSuccess, bucketStart)
	if err := usageStore.BatchCreateEngineExecutionEventsAndUsage(ctx, []models.EngineExecutionEvent{disabled}, false); err != nil {
		t.Fatalf("disabled usage execution event: %v", err)
	}
	assertRuntimeUsageCounts(t, pendingRuntimeUsageReports(t, ctx, usageStore), map[string]int64{
		models.EngineUsageMetricExecutionTotal: 3, models.EngineUsageMetricExecutionSuccess: 2, models.EngineUsageMetricExecutionFailed: 1,
	})
	// Replaying that same ID after aggregate reporting is enabled must preserve the first-seen non-billable decision.
	if err := usageStore.BatchCreateEngineExecutionEventsAndUsage(ctx, []models.EngineExecutionEvent{disabled}, true); err != nil {
		t.Fatalf("replay disabled usage execution event: %v", err)
	}
	assertRuntimeUsageCounts(t, pendingRuntimeUsageReports(t, ctx, usageStore), map[string]int64{
		models.EngineUsageMetricExecutionTotal: 3, models.EngineUsageMetricExecutionSuccess: 2, models.EngineUsageMetricExecutionFailed: 1,
	})
	if err := usageStore.MarkRuntimeUsageReportsFlushed(ctx, runtimeUsageReportIDs(pending), time.Now().UTC()); err != nil {
		t.Fatalf("MarkRuntimeUsageReportsFlushed: %v", err)
	}
	late := runtimeUsageExecutionEvent(uuid.New(), models.EngineExecutionStatusSuccess, bucketStart)
	if err := usageStore.BatchCreateEngineExecutionEventsAndUsage(ctx, []models.EngineExecutionEvent{late}, true); err != nil {
		t.Fatalf("late execution event: %v", err)
	}
	pending = pendingRuntimeUsageReports(t, ctx, usageStore)
	assertRuntimeUsageCounts(t, pending, map[string]int64{
		models.EngineUsageMetricExecutionTotal: 1, models.EngineUsageMetricExecutionSuccess: 1,
	})
}

// runtimeUsageExecutionEvent builds one final outbound physical receipt for transactional accounting tests.
func runtimeUsageExecutionEvent(id uuid.UUID, status string, startedAt time.Time) models.EngineExecutionEvent {
	return models.EngineExecutionEvent{
		ID: id, AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0",
		Transport: models.EngineExecutionTransportSDK, Direction: models.EngineExecutionDirectionOutbound,
		EndpointName: "execute", Status: status, StartedAt: startedAt, EndedAt: startedAt.Add(time.Millisecond), CreatedAt: startedAt,
	}
}

// assertRuntimeUsageCounts compares the pending closed-vocabulary aggregate without depending on row order.
func assertRuntimeUsageCounts(t *testing.T, reports []models.EngineUsageReport, want map[string]int64) {
	t.Helper()
	got := make(map[string]int64, len(reports))
	for _, report := range reports {
		got[report.Metric] += report.Count
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage counts = %#v, want %#v", got, want)
	}
}

// runtimeUsageReportIDs returns the exact pending report identities acknowledged by Registry.
func runtimeUsageReportIDs(reports []models.EngineUsageReport) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.ReportID)
	}
	return ids
}

// runtimeReportingTestStore opens a bounded disposable store and removes only reporting fixtures.
func runtimeReportingTestStore(t *testing.T) (context.Context, context.CancelFunc, *pgxpool.Pool, Store) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		cancel()
		t.Fatalf("failed to connect to DB: %v", err)
	}

	s := NewPostgresStore(pool)
	if _, err := pool.Exec(ctx, "DELETE FROM fused_runtime_entitlements; DELETE FROM fused_engine_usage_accounted_events; DELETE FROM fused_engine_usage_counter_reports"); err != nil {
		t.Fatalf("reset runtime reporting tables: %v", err)
	}
	return ctx, cancel, pool, s
}

// pendingRuntimeUsageReports loads the current durable outbox through its public store boundary.
func pendingRuntimeUsageReports(t *testing.T, ctx context.Context, usageStore interface {
	ListPendingRuntimeUsageReports(context.Context, int) ([]models.EngineUsageReport, error)
}) []models.EngineUsageReport {
	t.Helper()
	pending, err := usageStore.ListPendingRuntimeUsageReports(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingRuntimeUsageReports: %v", err)
	}
	return pending
}
