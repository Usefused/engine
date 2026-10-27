package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrExecutionRecoveryOrigin = errors.New("execution recovery origin no longer has valid authority")

var ErrExecutionRecoveryConflict = errors.New("execution is active, completed, superseded, or has exhausted its attempts")

// ExecutionRecoveryPolicy is immutable app policy; attempts include the original invocation.
type ExecutionRecoveryPolicy struct {
	Mode           string `json:"mode"`
	MaxAttempts    int    `json:"maxAttempts"`
	BackoffSeconds int    `json:"backoffSeconds"`
}

// Validate prevents unbounded retry loops and keeps automatic recovery explicitly opt-in.
func (policy ExecutionRecoveryPolicy) Validate() error {
	// Both trigger modes share the same finite attempt and delay bounds.
	if (policy.Mode != "manual" && policy.Mode != "automatic") || policy.MaxAttempts < 1 || policy.MaxAttempts > 10 || policy.BackoffSeconds < 1 || policy.BackoffSeconds > 300 {
		return ErrExecutionResultInvalid
	}
	return nil
}

// ExecutionRecovery exposes attempt metadata without exposing encrypted continuation state.
type ExecutionRecovery struct {
	Ready       bool                    `json:"ready"`
	ExecutionID uuid.UUID               `json:"executionId"`
	RunID       uuid.UUID               `json:"runId"`
	Attempt     int                     `json:"attempt"`
	Current     bool                    `json:"current"`
	Revision    int64                   `json:"snapshotRevision"`
	LeaseUntil  time.Time               `json:"leaseUntil"`
	Policy      ExecutionRecoveryPolicy `json:"policy"`
}

// ExecutionSnapshot is visible only to the confined execution that owns the current attempt.
type ExecutionSnapshot struct {
	Revision int64           `json:"revision"`
	State    json.RawMessage `json:"state"`
}

// ExecutionRerunRequest distinguishes a caller-authorized attempt from policy-authorized recovery.
type ExecutionRerunRequest struct {
	Enqueue                             bool
	AccountID, AppID, SourceID, TokenID uuid.UUID
	KeyHash                             string
	Automatic                           bool
}

// ExecutionRecoveryStore extends existing execution records rather than introducing a second job runtime.
type ExecutionRecoveryStore interface {
	ClaimQueuedExecutionAttempt(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
	AuthorizeExecutionRecovery(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*AuthProjection, error)
	ListExecutionAttempts(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]ExecutionAttemptSummary, error)
	GetExecutionRecovery(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*ExecutionRecovery, error)
	ReadExecutionSnapshot(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (ExecutionSnapshot, error)
	SaveExecutionSnapshot(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, json.RawMessage, []byte) (int64, error)
	CheckExecutionAttempt(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	ReserveExecutionAttempt(context.Context, ExecutionRerunRequest) (*ExecutionResult, bool, error)
	DueExecutionRecoveries(context.Context) ([]ExecutionResult, error)
}

// ExecutionAttemptSummary provides bounded history without disclosing private inputs or snapshots.
type ExecutionAttemptSummary struct {
	ExecutionID      uuid.UUID `json:"executionId"`
	RunID            uuid.UUID `json:"runId"`
	Attempt          int       `json:"attempt"`
	Current          bool      `json:"current"`
	Status           string    `json:"status"`
	SnapshotRevision int64     `json:"snapshotRevision"`
}
