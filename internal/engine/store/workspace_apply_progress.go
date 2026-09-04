package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// storeDatabase allows the existing SQL writers to run against a pool or one enclosing transaction.
type storeDatabase interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	Acquire(context.Context) (*pgxpool.Conn, error)
}

// workspaceTransaction keeps nested store transactions as savepoints inside the service commit boundary.
type workspaceTransaction struct{ pgx.Tx }

// BeginTx permits ordinary nested writes without silently changing transaction isolation.
func (t workspaceTransaction) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	// Isolation belongs to the outer transaction and cannot be replaced by a nested writer.
	if opts != (pgx.TxOptions{}) {
		return nil, errors.New("workspace apply cannot change transaction options")
	}
	return t.Begin(ctx)
}

// Acquire prevents a transactional writer from escaping onto another database connection.
func (t workspaceTransaction) Acquire(context.Context) (*pgxpool.Conn, error) {
	return nil, errors.New("workspace apply cannot acquire an independent connection")
}

var ErrWorkspaceApplyOutcomeUnknown = errors.New("workspace apply commit outcome unknown")
var ErrWorkspaceApplyStale = errors.New("workspace apply state changed; create a new plan")

// WorkspaceApplyStep identifies one immutable portion of a saved workspace plan.
type WorkspaceApplyStep struct {
	PlanID   uuid.UUID
	Revision int
	LeaseID  uuid.UUID
	Key      string
}

// WorkspaceApplyProgress contains bounded recovery metadata, never source documents or credentials.
type WorkspaceApplyProgress struct {
	Key       string `json:"key"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

// WorkspaceApplyMutation receives transaction-bound repositories and the current committed configuration projection.
type WorkspaceApplyMutation func(context.Context, Store, ConfigRepository, *ConfigState) (*UpsertConfigStateParams, error)

// WorkspaceApplyProgressStore owns durable service receipts and rejects stale workers at the write boundary.
type WorkspaceApplyProgressStore interface {
	WorkspaceApplyProgress(context.Context, uuid.UUID, int) ([]WorkspaceApplyProgress, error)
	RunWorkspaceApplyStep(context.Context, WorkspaceApplyStep, WorkspaceApplyMutation) error
	RecordWorkspaceApplyStep(context.Context, WorkspaceApplyStep, string, string) error
}

// WorkspaceApplyProgress loads receipts for the exact approved revision; missing rows remain unattempted.
func (s *postgresStore) WorkspaceApplyProgress(ctx context.Context, planID uuid.UUID, revision int) ([]WorkspaceApplyProgress, error) {
	rows, err := s.db.Query(ctx, `SELECT step_key, status, error_code FROM fused_workspace_apply_steps WHERE plan_id=$1 AND plan_revision=$2 ORDER BY step_key`, planID, revision)
	// Recovery must not guess when receipt storage is unavailable.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceApplyProgress
	for rows.Next() {
		var row WorkspaceApplyProgress
		// A malformed receipt invalidates the snapshot instead of losing a success marker.
		if err := rows.Scan(&row.Key, &row.Status, &row.ErrorCode); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// lockWorkspaceApplyStep fences each commit by plan revision, current lease and the operation's own last committed generation.
func lockWorkspaceApplyStep(ctx context.Context, tx pgx.Tx, step WorkspaceApplyStep) (string, error) {
	var configKey string
	var expectedGeneration int
	var expectedWorkspaceRevision int64
	err := tx.QueryRow(ctx, `SELECT config_key, COALESCE(apply_generation,base_generation), COALESCE(apply_workspace_revision,base_workspace_revision) FROM fused_config_plans
 WHERE id=$1 AND revision=$2 AND config_type='workspace' AND status='pending'
 AND apply_lease_id=$3 AND apply_lease_expires_at>NOW() FOR UPDATE`, step.PlanID, step.Revision, step.LeaseID).Scan(&configKey, &expectedGeneration, &expectedWorkspaceRevision)
	// An expired or replaced worker may never write another service.
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrConfigPlanRevisionMismatch
	}
	// Database failures cannot be classified as a service rejection.
	if err != nil {
		return "", err
	}
	// The workspace lock also serializes separate plans before a missing config-state row exists.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "workspace-apply:"+configKey); err != nil {
		return "", err
	}
	var workspaceRevision int64
	// Locking the revision serializes all service groups with direct configuration writes.
	if err := tx.QueryRow(ctx, `SELECT revision FROM fused_workspace_apply_revision WHERE singleton=true FOR UPDATE`).Scan(&workspaceRevision); err != nil {
		return "", err
	}
	// A changed workspace needs a fresh review rather than blindly replaying an old desired snapshot.
	if workspaceRevision != expectedWorkspaceRevision {
		return "", ErrWorkspaceApplyStale
	}
	var generation int
	err = tx.QueryRow(ctx, `SELECT generation FROM fused_config_states WHERE config_key=$1 FOR UPDATE`, configKey).Scan(&generation)
	// A workspace's first service legitimately starts from generation zero.
	if errors.Is(err, pgx.ErrNoRows) {
		generation = 0
	} else if err != nil {
		return "", err
	}
	// Prior successes from this plan advance apply_generation; unrelated applies do not.
	if generation != expectedGeneration {
		return "", ErrWorkspaceApplyStale
	}
	return configKey, nil
}

// writeWorkspaceApplyStep records outcomes under the same plan lock used for local mutations.
func writeWorkspaceApplyStep(ctx context.Context, tx pgx.Tx, step WorkspaceApplyStep, status, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO fused_workspace_apply_steps(plan_id,plan_revision,step_key,status,error_code)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(plan_id,plan_revision,step_key)
 DO UPDATE SET status=EXCLUDED.status,error_code=EXCLUDED.error_code,updated_at=NOW()`, step.PlanID, step.Revision, step.Key, status, code)
	return err
}

// RecordWorkspaceApplyStep stores proven failures or external dispatch markers without changing the applied configuration.
func (s *postgresStore) RecordWorkspaceApplyStep(ctx context.Context, step WorkspaceApplyStep, status, code string) error {
	tx, err := s.db.Begin(ctx)
	// Receipt admission must finish before recording an external request as dispatched.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The lease and generation checks apply equally to bookkeeping and mutations.
	if _, err := lockWorkspaceApplyStep(ctx, tx, step); err != nil {
		return err
	}
	// A durable success must never regress because a delayed failure arrives.
	var previous string
	err = tx.QueryRow(ctx, `SELECT status FROM fused_workspace_apply_steps WHERE plan_id=$1 AND plan_revision=$2 AND step_key=$3`, step.PlanID, step.Revision, step.Key).Scan(&previous)
	// Only a missing receipt permits fresh work; storage failures invalidate recovery.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// External running markers are deliberately not replayable after a lost response.
	if previous == "succeeded" || (previous == "running" && status == "running") {
		return ErrConfigPlanApplyInProgress
	}
	// SQL constraints enforce the closed state vocabulary.
	if err := writeWorkspaceApplyStep(ctx, tx, step, status, code); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunWorkspaceApplyStep commits local changes, actual managed state and their success receipt as one transaction.
func (s *postgresStore) RunWorkspaceApplyStep(ctx context.Context, step WorkspaceApplyStep, mutate WorkspaceApplyMutation) error {
	tx, err := s.db.Begin(ctx)
	// No work is accepted unless an enclosing transaction exists.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	key, err := lockWorkspaceApplyStep(ctx, tx, step)
	// Stale plans and workers must fail before invoking any service writer.
	if err != nil {
		return err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM fused_workspace_apply_steps WHERE plan_id=$1 AND plan_revision=$2 AND step_key=$3`, step.PlanID, step.Revision, step.Key).Scan(&status)
	// Missing receipts identify fresh work; other lookup failures are not safe to ignore.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// The receipt and all service writes committed together, so repeating them is unnecessary.
	if status == "succeeded" {
		return nil
	}
	repository := &postgresConfigRepository{db: workspaceTransaction{tx}}
	current, err := repository.GetConfigState(ctx, key)
	// The first committed service creates the applied-state projection.
	if err != nil {
		return err
	}
	state, err := mutate(ctx, &postgresStore{db: workspaceTransaction{tx}}, repository, current)
	// Returning an error rolls back every nested store savepoint as well as this service's writes.
	if err != nil {
		return err
	}
	// Metadata-only steps need a receipt but must not rewrite desired state.
	if state != nil {
		// Callbacks cannot write another configuration under this plan's lease.
		if state.ConfigKey != key || state.ConfigType != ConfigTypeWorkspace {
			return ErrConfigStateIdentityMismatch
		}
		written, err := repository.UpsertConfigState(ctx, *state)
		// A failed projection write must roll back the service, too.
		if err != nil {
			return err
		}
		// Track this plan's own progress separately from its immutable base generation.
		if _, err := tx.Exec(ctx, `UPDATE fused_config_plans SET apply_generation=$2 WHERE id=$1`, step.PlanID, written.Generation); err != nil {
			return err
		}
	}
	// Include every row-level revision bump produced by this group's nested writers.
	if _, err := tx.Exec(ctx, `UPDATE fused_config_plans SET apply_workspace_revision=(SELECT revision FROM fused_workspace_apply_revision WHERE singleton=true) WHERE id=$1`, step.PlanID); err != nil {
		return err
	}
	// A success marker is trustworthy only when committed with all preceding local writes.
	if err := writeWorkspaceApplyStep(ctx, tx, step, "succeeded", ""); err != nil {
		return err
	}
	// A lost COMMIT acknowledgement may conceal success; callers must inspect receipts before retrying.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceApplyOutcomeUnknown, err)
	}
	return nil
}

// WorkspaceApplyProgress delegates durable reads without caching changing execution state.
func (s *cachedStore) WorkspaceApplyProgress(ctx context.Context, planID uuid.UUID, revision int) ([]WorkspaceApplyProgress, error) {
	repository, ok := s.Store.(WorkspaceApplyProgressStore)
	// Missing transactional support cannot fall back to independently committed writes.
	if !ok {
		return nil, errors.New("workspace apply receipts unavailable")
	}
	return repository.WorkspaceApplyProgress(ctx, planID, revision)
}

// RecordWorkspaceApplyStep keeps receipt writes in the authoritative database.
func (s *cachedStore) RecordWorkspaceApplyStep(ctx context.Context, step WorkspaceApplyStep, status, code string) error {
	repository, ok := s.Store.(WorkspaceApplyProgressStore)
	// A deployment without receipt storage cannot safely resume an apply.
	if !ok {
		return errors.New("workspace apply receipts unavailable")
	}
	return repository.RecordWorkspaceApplyStep(ctx, step, status, code)
}

// RunWorkspaceApplyStep publishes cache invalidation only after the transaction completes or its outcome requires reconciliation.
func (s *cachedStore) RunWorkspaceApplyStep(ctx context.Context, step WorkspaceApplyStep, mutate WorkspaceApplyMutation) error {
	repository, ok := s.Store.(WorkspaceApplyProgressStore)
	// Atomic service writes are a required capability, not an optional optimization.
	if !ok {
		return errors.New("workspace apply transactions unavailable")
	}
	err := repository.RunWorkspaceApplyStep(ctx, step, mutate)
	// An uncertain commit can also have changed state, so discard cached configuration conservatively.
	if err == nil || errors.Is(err, ErrWorkspaceApplyOutcomeUnknown) {
		s.invalidateRuntimeConfiguration(context.WithoutCancel(ctx))
	}
	return err
}
