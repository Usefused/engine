package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// lostSnapshotReplyFixture models a committed save whose acknowledgement never reaches the runtime.
type lostSnapshotReplyFixture struct {
	store.ExecutionRecoveryStore
	saved bool
}

// CheckExecutionAttempt keeps this test focused on the uncertain persistence boundary.
func (*lostSnapshotReplyFixture) CheckExecutionAttempt(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error {
	return nil
}

// ReadExecutionSnapshot supplies the starting revision of an admitted attempt.
func (*lostSnapshotReplyFixture) ReadExecutionSnapshot(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (store.ExecutionSnapshot, error) {
	return store.ExecutionSnapshot{State: json.RawMessage("null")}, nil
}

// SaveExecutionSnapshot commits locally but models an unavailable database acknowledgement.
func (fixture *lostSnapshotReplyFixture) SaveExecutionSnapshot(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, json.RawMessage, []byte) (int64, error) {
	fixture.saved = true
	return 0, context.DeadlineExceeded
}

// TestSnapshotLostReplyCancelsAttempt prevents authored catch blocks from continuing after an uncertain durable save.
func TestSnapshotLostReplyCancelsAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repository := &lostSnapshotReplyFixture{}
	host := &executionCapabilityHost{apps: &EngineGRPCServer{}, recovery: repository, cancelAttempt: cancel}
	_, err := host.Fetch(ctx, json.RawMessage(`{"snapshot":"save","state":{"cursor":7}}`))
	// The committed-but-unacknowledged save must invalidate the whole execution, not only the host call.
	if err == nil || !repository.saved || ctx.Err() == nil {
		t.Fatalf("err=%v saved=%v ctx=%v", err, repository.saved, ctx.Err())
	}
	// A script that catches the error and returns output must still fail terminal validation.
	if recordedCapabilityError(nil, ctx.Err(), nil) == nil {
		t.Fatal("caught snapshot error became success")
	}
}

// TestSnapshotRequiresRecoveryDeclaration prevents raw bundles from silently writing ephemeral checkpoints.
func TestSnapshotRequiresRecoveryDeclaration(t *testing.T) {
	host := &executionCapabilityHost{}
	_, err := host.Fetch(context.Background(), json.RawMessage(`{"snapshot":"save","state":{}}`))
	// A bundle must explicitly carry the compiled durable recovery contract.
	if err == nil {
		t.Fatal("snapshot accepted without recovery")
	}
}
