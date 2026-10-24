package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/store"
)

// snapshotRequest uses the existing recorded host channel so replay never makes durable writes.
type snapshotRequest struct {
	Snapshot string          `json:"snapshot"`
	State    json.RawMessage `json:"state,omitempty"`
}

// executeSnapshot serializes snapshot operations and poisons execution on uncertain persistence.
func (host *executionCapabilityHost) executeSnapshot(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	host.snapshotMu.Lock()
	defer host.snapshotMu.Unlock()
	var request snapshotRequest
	// Snapshot effects require a recovery-enabled bundle and an admitted owner.
	if host.recovery == nil || json.Unmarshal(raw, &request) != nil {
		return nil, errors.New("snapshot is unavailable")
	}
	snapshot, err := host.recovery.ReadExecutionSnapshot(ctx, host.identity.AccountID, host.identity.AppID, host.executionID, host.apps.masterKey)
	// Continuing after an uncertain read could skip or repeat already committed work.
	if err != nil {
		host.cancelAttempt()
		return nil, errors.New("snapshot is unavailable")
	}
	switch request.Snapshot {
	// Loading returns explicit saved progress, never an interpreter stack or mutable global state.
	case "load":
		return snapshot.State, nil
	// Saving is acknowledged only after a revision-checked encrypted database write.
	case "save":
		_, err = host.recovery.SaveExecutionSnapshot(ctx, host.identity.AccountID, host.identity.AppID, host.executionID, snapshot.Revision, request.State, host.apps.masterKey)
		// Even authored catch blocks cannot continue after losing the save acknowledgement.
		if err != nil {
			host.cancelAttempt()
			return nil, errors.New("snapshot save was not acknowledged")
		}
		return json.RawMessage("null"), nil
	// Unknown operations cannot broaden the confined worker's infrastructure access.
	default:
		return nil, errors.New("invalid snapshot operation")
	}
}

// prepareAttemptHost enables snapshot authority only when the admitted bundle declares recovery.
func (s *EngineGRPCServer) prepareAttemptHost(spec capabilityRunSpec, host *executionCapabilityHost, cancel context.CancelFunc) {
	// Historical bundles retain their existing execution contract without synthetic snapshot state.
	if spec.manifest.Recovery == nil {
		return
	}
	host.recovery, _ = s.store.(store.ExecutionRecoveryStore)
	host.cancelAttempt = cancel
}

// requireRecoveryStore prevents partial admission of a recovery-enabled bundle.
func (s *EngineGRPCServer) requireRecoveryStore(spec capabilityRunSpec) *restExecutionError {
	// Historical bundles do not require the new durable continuation contract.
	if spec.manifest == nil || spec.manifest.Recovery == nil {
		return nil
	}
	// Feature discovery cannot substitute for authoritative Engine storage.
	if _, ok := s.store.(store.ExecutionRecoveryStore); !ok {
		return newRESTExecutionError(503, "runtime_unavailable", "execution recovery is unavailable")
	}
	return nil
}
