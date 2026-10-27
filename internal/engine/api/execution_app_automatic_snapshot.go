package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// capabilityRunDeterminism persists replay controls before the first provider effect and restores them across attempts.
func (s *EngineGRPCServer) capabilityRunDeterminism(ctx context.Context, spec capabilityRunSpec, executionID uuid.UUID) (sandbox.CapabilityDeterminism, uuid.UUID, error) {
	// Historical and explicitly disabled snapshots keep ordinary per-attempt execution.
	if !spec.manifest.Snapshot {
		controls, err := sandbox.NewCapabilityDeterminism()
		return controls, uuid.Nil, err
	}
	repository, ok := s.store.(store.ExecutionRecoveryStore)
	// An automatic snapshot cannot degrade into an uncheckpointed execution.
	if !ok {
		return sandbox.CapabilityDeterminism{}, uuid.Nil, errors.New("snapshot storage unavailable")
	}
	state, err := repository.GetExecutionRecovery(ctx, spec.identity.AccountID, spec.identity.AppID, executionID)
	// The admitted run identity is authoritative; an attempt ID cannot replace it.
	if err != nil || state == nil {
		return sandbox.CapabilityDeterminism{}, uuid.Nil, errors.New("snapshot run unavailable")
	}
	controls, err := s.restoreAutomaticControls(ctx, repository, spec, executionID)
	return controls, state.RunID, err
}

// restoreAutomaticControls reuses the encrypted, revision-checked snapshot instead of adding a second checkpoint store.
func (s *EngineGRPCServer) restoreAutomaticControls(ctx context.Context, repository store.ExecutionRecoveryStore, spec capabilityRunSpec, id uuid.UUID) (sandbox.CapabilityDeterminism, error) {
	var controls sandbox.CapabilityDeterminism
	snapshot, err := repository.ReadExecutionSnapshot(ctx, spec.identity.AccountID, spec.identity.AppID, id, s.masterKey)
	// A failed read cannot authorize a new clock or random sequence.
	if err != nil {
		return controls, err
	}
	// Existing controls must validate before any replayed call identity is trusted.
	if snapshot.Revision > 0 {
		if json.Unmarshal(snapshot.State, &controls) != nil || !controls.Valid() {
			return controls, errors.New("snapshot controls invalid")
		}
		return controls, nil
	}
	controls, err = sandbox.NewCapabilityDeterminism()
	// Entropy failures cannot leave a usable but nonreplayable checkpoint.
	if err != nil {
		return controls, err
	}
	raw, err := json.Marshal(controls)
	// Encode before the CAS so a malformed control value is never acknowledged.
	if err != nil {
		return controls, err
	}
	_, err = repository.SaveExecutionSnapshot(ctx, spec.identity.AccountID, spec.identity.AppID, id, snapshot.Revision, raw, s.masterKey)
	return controls, err
}

// providerIdempotencyKey preserves authored keys and supplies automatic keys only for snapshot-enabled apps.
func (host *executionCapabilityHost) providerIdempotencyKey(request capabilityHostFetchRequest, ordinal int) (string, error) {
	key := request.IdempotencyKey
	// Headers and storage keys must agree exactly, without whitespace normalization or control characters.
	if len(key) > 256 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("invalid provider idempotency key")
	}
	// Authored identity has precedence, including when automatic snapshots are disabled.
	if key != "" {
		return key, nil
	}
	// No opt-in means no synthetic deduplication of ordinary provider calls.
	if !host.automaticSnapshot {
		return "", nil
	}
	// A lost logical identity cannot silently fall back to per-attempt deduplication.
	if host.snapshotRunID == uuid.Nil || ordinal < 1 {
		return "", errors.New("snapshot call identity unavailable")
	}
	return fmt.Sprintf("fused:run:%s:call:%d", host.snapshotRunID, ordinal), nil
}
