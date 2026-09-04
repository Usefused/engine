package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestWorkspaceApplyReceiptsCommitWithService covers rollback, retry skipping, state generations and direct-write fencing in real PostgreSQL.
func TestWorkspaceApplyReceiptsCommitWithService(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// A disposable database is mandatory; no implicit production connection is permitted.
	if dsn == "" {
		t.Skip("DATABASE_URL required for PostgreSQL transaction test")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	s := NewPostgresStore(pool).(*postgresStore)
	repo := NewPostgresConfigRepository(pool)
	actor := uuid.New()
	plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{ConfigKey: "workspace:partial", ConfigType: ConfigTypeWorkspace, SourceHash: "sha256:reviewed", Actions: json.RawMessage(`[]`), DesiredState: json.RawMessage(`{}`), ResolvedPayload: json.RawMessage(`{}`), RequiredPermissions: testRequiredPermissions(actor), CreatedBy: actor})
	// The test uses the real saved-plan and lease admission path.
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	serviceID, versionID := uuid.New(), uuid.New()
	step := WorkspaceApplyStep{PlanID: plan.ID, Revision: plan.Revision, LeaseID: lease.ID, Key: "service:" + serviceID.String()}
	injected := errors.New("injected failure after activation")
	calls := 0
	fail := true
	// The callback deliberately fails after a nested service transaction has released its savepoint.
	mutation := func(ctx context.Context, txStore Store, _ ConfigRepository, current *ConfigState) (*UpsertConfigStateParams, error) {
		calls++
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if err := txStore.AddWorkspaceServiceVersion(ctx, serviceID, "partial", "v1", versionID, "Partial", actor); err != nil {
			return nil, err
		}
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if fail {
			return nil, injected
		}
		policy := txStore.(interface {
			UpsertWorkspaceExecutionPolicyOverride(context.Context, WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error)
		})
		baseURL := "https://api.example.test"
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if _, err := policy.UpsertWorkspaceExecutionPolicyOverride(ctx, WorkspaceExecutionPolicyOverride{ServiceID: serviceID, ServiceVersionID: &versionID, BaseURL: &baseURL}); err != nil {
			return nil, err
		}
		return &UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace, SourceHash: "sha256:partial", DesiredState: json.RawMessage(`{"services":{}}`), ManagedResources: json.RawMessage(`{"services":[]}`), UpdatedBy: actor}, nil
	}
	// Failed service writes and their nested commits must roll back together.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); !errors.Is(err, injected) {
		t.Fatalf("wanted injected failure, got %v", err)
	}
	var count int
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_services WHERE service_id=$1`, serviceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("activation escaped rollback: count=%d err=%v", count, err)
	}
	rows, err := s.WorkspaceApplyProgress(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed transaction wrote receipt: %#v %v", rows, err)
	}
	// A durable failure record remains retryable under the original exact plan.
	if err := s.RecordWorkspaceApplyStep(ctx, step, "failed", "service_apply_failed"); err != nil {
		t.Fatal(err)
	}
	fail = false
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if calls != 2 {
		t.Fatalf("successful mutation replayed: %d calls", calls)
	}
	rows, err = s.WorkspaceApplyProgress(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || len(rows) != 1 || rows[0].Status != "succeeded" {
		t.Fatalf("missing success receipt: %#v %v", rows, err)
	}
	saved, err := repo.GetConfigPlan(ctx, plan.ID)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil || saved.ApplyGeneration == nil || *saved.ApplyGeneration != 1 || saved.BaseGeneration != 0 {
		t.Fatalf("immutable base/progress mismatch: %#v %v", saved, err)
	}
	// Editing a started plan cannot rebind success receipts to different approved actions.
	if _, err := repo.ReplaceConfigPlanActions(ctx, plan.ID, json.RawMessage(`[]`), testRequiredPermissions(actor), actor); !errors.Is(err, ErrConfigPlanApplyInProgress) {
		t.Fatalf("changed active plan: %v", err)
	}
	// Direct service writes do not use config state, so the database revision must fence them too.
	if err := s.DisableWorkspaceServiceVersion(ctx, serviceID, "v1"); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); !errors.Is(err, ErrWorkspaceApplyStale) {
		t.Fatalf("direct mutation not detected: %v", err)
	}
}

// TestWorkspaceApplyExternalMarkerSurvivesLeaseExpiry proves an unknown Registry outcome cannot be superseded into a blind retry.
func TestWorkspaceApplyExternalMarkerSurvivesLeaseExpiry(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// Only an explicitly supplied test database may host this fixture.
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	s := NewPostgresStore(pool).(*postgresStore)
	repo := NewPostgresConfigRepository(pool)
	actor := uuid.New()
	params := CreateConfigPlanParams{ConfigKey: "workspace:external", ConfigType: ConfigTypeWorkspace, SourceHash: "review", Actions: json.RawMessage(`[]`), DesiredState: json.RawMessage(`{}`), ResolvedPayload: json.RawMessage(`{}`), RequiredPermissions: testRequiredPermissions(actor), CreatedBy: actor}
	plan, err := repo.CreateConfigPlan(ctx, params)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	step := WorkspaceApplyStep{PlanID: plan.ID, Revision: plan.Revision, LeaseID: lease.ID, Key: "registry"}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RecordWorkspaceApplyStep(ctx, step, "running", "needs_reconciliation"); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := repo.ReleaseConfigPlanApply(ctx, plan.ID, plan.Revision, lease.ID); err != nil {
		t.Fatal(err)
	}
	params.SupersedeExisting = true
	// External uncertainty survives worker release and must remain inspectable.
	if _, err := repo.CreateConfigPlan(ctx, params); !errors.Is(err, ErrConfigPlanApplyInProgress) {
		t.Fatalf("unknown external action superseded: %v", err)
	}
}

// lostWorkspaceCommitDatabase injects a lost acknowledgement after the real outer COMMIT succeeds.
type lostWorkspaceCommitDatabase struct{ storeDatabase }

// Begin wraps only the outer transaction, leaving nested store savepoints unchanged.
func (d lostWorkspaceCommitDatabase) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.storeDatabase.Begin(ctx)
	// An actual begin failure must retain its original database classification.
	if err != nil {
		return nil, err
	}
	return lostWorkspaceCommitTransaction{tx}, nil
}

type lostWorkspaceCommitTransaction struct{ pgx.Tx }

// Commit models a network failure after PostgreSQL durably accepted every local write and its receipt.
func (t lostWorkspaceCommitTransaction) Commit(ctx context.Context) error {
	// Genuine database failures take precedence over the injected lost response.
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	return errors.New("injected lost commit acknowledgement")
}

// TestWorkspaceApplyLostCommitResponseUsesReceipt proves an ambiguous acknowledgement cannot cause duplicate local writes.
func TestWorkspaceApplyLostCommitResponseUsesReceipt(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// This proof requires a real transaction in an isolated test database.
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	repo := NewPostgresConfigRepository(pool)
	actor := uuid.New()
	serviceID := uuid.New()
	plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{ConfigKey: "workspace:lost-response", ConfigType: ConfigTypeWorkspace, SourceHash: "reviewed", Actions: json.RawMessage(`[]`), DesiredState: json.RawMessage(`{}`), ResolvedPayload: json.RawMessage(`{}`), RequiredPermissions: testRequiredPermissions(actor), CreatedBy: actor})
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	step := WorkspaceApplyStep{PlanID: plan.ID, Revision: plan.Revision, LeaseID: lease.ID, Key: "service:" + serviceID.String()}
	writes := 0
	// The enclosing receipt transaction must also own the service's nested transaction.
	mutation := func(ctx context.Context, s Store, _ ConfigRepository, _ *ConfigState) (*UpsertConfigStateParams, error) {
		writes++
		// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
		if err := s.AddWorkspaceServiceVersion(ctx, serviceID, "lost-response", "v1", uuid.New(), "Lost response", actor); err != nil {
			return nil, err
		}
		return &UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace, SourceHash: "committed", DesiredState: json.RawMessage(`{}`), ManagedResources: json.RawMessage(`{}`), UpdatedBy: actor}, nil
	}
	uncertain := &postgresStore{db: lostWorkspaceCommitDatabase{pool}}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := uncertain.RunWorkspaceApplyStep(ctx, step, mutation); !errors.Is(err, ErrWorkspaceApplyOutcomeUnknown) {
		t.Fatalf("lost response misclassified: %v", err)
	}
	recovered := NewPostgresStore(pool).(*postgresStore)
	// A fresh store instance consults durable state instead of process-local success memory.
	if err := recovered.RunWorkspaceApplyStep(ctx, step, mutation); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if writes != 1 {
		t.Fatalf("committed write replayed %d times", writes)
	}
}

// TestWorkspaceApplyRejectsReplacedLease checks the fencing token at the transaction boundary rather than trusting earlier HTTP admission.
func TestWorkspaceApplyRejectsReplacedLease(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// This race boundary requires PostgreSQL-owned leases and locks.
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	s := NewPostgresStore(pool).(*postgresStore)
	repo := NewPostgresConfigRepository(pool)
	actor := uuid.New()
	plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{ConfigKey: "workspace:lease-fence", ConfigType: ConfigTypeWorkspace, SourceHash: "reviewed", Actions: json.RawMessage(`[]`), DesiredState: json.RawMessage(`{}`), ResolvedPayload: json.RawMessage(`{}`), RequiredPermissions: testRequiredPermissions(actor), CreatedBy: actor})
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	oldLease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	// Expiration permits a replacement worker, but never revives the original token.
	if _, err := pool.Exec(ctx, `UPDATE fused_config_plans SET apply_lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	newLease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	// The callback must remain uncalled for an obsolete lease even when its plan revision still matches.
	mutation := func(context.Context, Store, ConfigRepository, *ConfigState) (*UpsertConfigStateParams, error) {
		writes++
		return nil, nil
	}
	step := WorkspaceApplyStep{PlanID: plan.ID, Revision: plan.Revision, LeaseID: oldLease.ID, Key: "service:test"}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); !errors.Is(err, ErrConfigPlanRevisionMismatch) {
		t.Fatalf("obsolete lease admitted: %v", err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if writes != 0 {
		t.Fatal("obsolete worker reached service writes")
	}
	step.LeaseID = newLease.ID
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if err := s.RunWorkspaceApplyStep(ctx, step, mutation); err != nil {
		t.Fatal(err)
	}
	// Reject an invalid fixture or outcome rather than accepting unproven recovery behavior.
	if writes != 1 {
		t.Fatalf("replacement worker calls=%d", writes)
	}
}
