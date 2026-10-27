package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"time"
)

type bridgeSnapshotStore interface {
	GetIdempotentExecution(context.Context, uuid.UUID, string, string) (*models.IdempotentExecution, error)
	SaveBridgeSnapshotResponse(context.Context, *models.IdempotentExecution) error
}
type bridgeSnapshotAuthorizer interface {
	AuthorizeSnapshot(context.Context, string, bridgehost.Pin, []byte) error
}

func bridgeSnapshotHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// executeBridgeSnapshot returns an Engine-retained response before contacting the local installation.
func (s *EngineGRPCServer) executeBridgeSnapshot(ctx context.Context, id auth.RuntimeIdentity, alias string, pin bridgehost.Pin, input json.RawMessage) (json.RawMessage, error) {
	repository, ok := s.store.(bridgeSnapshotStore)
	if !ok {
		return nil, errors.New("Bridge snapshot storage unavailable")
	}
	authorizer, ok := s.bridgeHost.(bridgeSnapshotAuthorizer)
	if !ok {
		return nil, errors.New("Bridge snapshot authorization unavailable")
	}
	if err := authorizer.AuthorizeSnapshot(ctx, id.AccountID.String(), pin, input); err != nil {
		return nil, err
	}
	key, run := bridgehost.SnapshotCall(ctx)
	if key == "" || len(key) > 256 || run == uuid.Nil {
		return nil, errors.New("invalid Bridge snapshot identity")
	}
	// Bind input to the exact immutable destination and operation, independent of attempts and session generations.
	raw, err := json.Marshal([]any{pin, input})
	if err != nil {
		return nil, err
	}
	record := &models.IdempotentExecution{ID: uuid.New(), AppID: id.AppID, RunID: &run, IdempotencyKeyHash: bridgeSnapshotHash([]byte("bridge:" + key)), RequestBodyHash: bridgeSnapshotHash(raw), ExpiresAt: time.Now().Add(models.IdempotencyTTL)}
	cached, err := repository.GetIdempotentExecution(ctx, id.AppID, record.IdempotencyKeyHash, record.RequestBodyHash)
	if err == nil {
		return sandbox.RunBridgeInvocation(sandbox.WithBridgeReplay(ctx), id, alias, func(context.Context) (json.RawMessage, error) { return cached.ResponseBody, nil })
	}
	if !errors.Is(err, store.ErrIdempotentExecutionNotFound) {
		return nil, err
	}
	// Scope the wire key to the immutable app, so two apps cannot claim each other's Bridge records.
	ctx = bridgehost.WithSnapshotCall(ctx, "fused:"+id.AppID.String()+":"+key, run)
	return sandbox.RunBridgeInvocation(ctx, id, alias, func(ctx context.Context) (json.RawMessage, error) {
		result, err := s.bridgeHost.Invoke(ctx, id.AccountID.String(), pin, input)
		if err != nil {
			return nil, err
		}
		if len(result) > 1<<20 || !json.Valid(result) {
			return nil, errors.New("invalid Bridge snapshot response")
		}
		record.ResponseBody = result
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err = repository.SaveBridgeSnapshotResponse(finish, record); err != nil {
			return nil, err
		}
		return result, nil
	})
}
