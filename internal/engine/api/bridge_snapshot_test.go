package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

type bridgeSnapshotFixture struct {
	bridgeRuntimeFixture
	rows         map[string]*models.IdempotentExecution
	device       map[string][]byte
	online       bool
	denied       bool
	writeFailure bool
	readFailure  bool
	effects      int
}

func (f *bridgeSnapshotFixture) AuthorizeSnapshot(context.Context, string, bridgehost.Pin, []byte) error {
	if f.denied {
		return errors.New("revoked")
	}
	return nil
}
func (f *bridgeSnapshotFixture) GetIdempotentExecution(_ context.Context, _ uuid.UUID, key, body string) (*models.IdempotentExecution, error) {
	if f.readFailure {
		return nil, errors.New("storage failed")
	}
	row := f.rows[key]
	if row == nil {
		return nil, store.ErrIdempotentExecutionNotFound
	}
	if row.RequestBodyHash != body {
		return nil, store.ErrIdempotencyKeyConflict
	}
	return row, nil
}
func (f *bridgeSnapshotFixture) SaveBridgeSnapshotResponse(_ context.Context, r *models.IdempotentExecution) error {
	if f.writeFailure {
		return errors.New("commit failed")
	}
	copy := *r
	f.rows[r.IdempotencyKeyHash] = &copy
	return nil
}
func (f *bridgeSnapshotFixture) Invoke(ctx context.Context, _ string, _ bridgehost.Pin, input []byte) ([]byte, error) {
	f.calls++
	if !f.online {
		return nil, &bridgehost.InvocationError{Outcome: "not_started", Code: "unavailable"}
	}
	key := bridgehost.CallIdentity(ctx)
	if key == "" {
		return nil, errors.New("missing wire identity")
	}
	if out := f.device[key]; out != nil {
		return out, nil
	}
	f.effects++
	f.device[key] = append([]byte(nil), input...)
	return input, nil
}
func newBridgeSnapshotFixture() (*bridgeSnapshotFixture, *EngineGRPCServer, auth.RuntimeIdentity) {
	f := &bridgeSnapshotFixture{rows: map[string]*models.IdempotentExecution{}, device: map[string][]byte{}, online: true}
	f.bindings = []models.BridgeBinding{{Alias: "volume", Kind: "command", ContractHash: "contract", BridgeReference: models.BridgeReference{PairingID: uuid.NewString(), ProfileID: "home", ConnectionID: "speaker", OperationID: "set", ManifestHash: "manifest"}}}
	s := &EngineGRPCServer{store: f, bridgeHost: f}
	id := auth.RuntimeIdentity{Kind: store.AppKindUnifiedApp, AppID: uuid.New(), AccountID: uuid.New(), TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"bridge:volume"}}}
	return f, s, id
}
func TestBridgeSnapshotReplayOfflineAndAuthority(t *testing.T) {
	f, s, id := newBridgeSnapshotFixture()
	run := uuid.New()
	ctx := bridgehost.WithSnapshotCall(t.Context(), "run:call:1", run)
	first, e := s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.NoError(t, e)
	require.Equal(t, 1, f.effects)
	// A replacement Engine instance sees the same durable response while Bridge is offline.
	s = &EngineGRPCServer{store: f, bridgeHost: f}
	f.online = false
	again, e := s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.NoError(t, e)
	require.Equal(t, first, again)
	require.Equal(t, 1, f.calls)
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`30`))
	require.ErrorIs(t, e, store.ErrIdempotencyKeyConflict)
	require.Equal(t, 1, f.calls)
	f.denied = true
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.ErrorContains(t, e, "revoked")
	f.denied = false
	id.TokenPolicy = store.AppTokenPolicy{AllowedOperations: []string{"other"}}
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.Error(t, e)
}
func TestBridgeSnapshotLostEngineCommitRecoversSameWireKey(t *testing.T) {
	f, s, id := newBridgeSnapshotFixture()
	ctx := bridgehost.WithSnapshotCall(t.Context(), "run:call:1", uuid.New())
	f.writeFailure = true
	_, e := s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.Error(t, e)
	require.Equal(t, 1, f.effects)
	f.writeFailure = false
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.NoError(t, e)
	require.Equal(t, 2, f.calls)
	require.Equal(t, 1, f.effects)
	second := bridgehost.WithSnapshotCall(t.Context(), "run:call:2", uuid.New())
	_, e = s.ExecuteAttachedBridge(second, id, "volume", json.RawMessage(`20`))
	require.NoError(t, e)
	require.Equal(t, 2, f.effects)
	f.readFailure = true
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.Error(t, e)
	require.Equal(t, 3, f.calls)
}
func TestBridgeSnapshotOfflineFirstAttemptCanRecover(t *testing.T) {
	f, s, id := newBridgeSnapshotFixture()
	ctx := bridgehost.WithSnapshotCall(t.Context(), "run:call:1", uuid.New())
	f.online = false
	_, e := s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.Error(t, e)
	require.Empty(t, f.rows)
	require.Zero(t, f.effects)
	f.online = true
	_, e = s.ExecuteAttachedBridge(ctx, id, "volume", json.RawMessage(`20`))
	require.NoError(t, e)
	require.Equal(t, 1, f.effects)
}
