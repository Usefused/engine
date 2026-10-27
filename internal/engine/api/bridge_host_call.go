package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
	"io"
)

// callBridge records one selected local effect in the ordinary worker fetch transcript.
func (host *executionCapabilityHost) callBridge(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	// Calls require the same Engine admission boundary in ordinary and recovery modes.
	if host.apps == nil {
		return nil, errors.New("Bridge calls unavailable in this execution mode")
	}
	// Snapshot effects require a stable logical run identity before any dispatch.
	if host.automaticSnapshot && host.snapshotRunID == uuid.Nil {
		return nil, errors.New("snapshot call identity unavailable")
	}
	request, err := decodeBridgeHostRequest(raw)
	// Reject malformed envelopes before counting or dispatching a provider operation.
	if err != nil {
		return nil, err
	}

	host.mu.Lock()
	host.callCount++
	count := host.callCount
	host.mu.Unlock()
	// Local and remote effects share one execution budget.
	if count > maxCapabilityFetchCalls {
		return nil, errors.New("Unified App operation limit exceeded")
	}
	host.mu.Lock()
	host.called = true
	host.mu.Unlock()
	ctx = sandbox.WithUnifiedAppPhysicalQueue(ctx)
	ordinal, found := sandbox.CapabilityCallOrdinal(ctx)
	// Stable worker ordinals correlate the local effect with its authored execution receipt.
	if !found {
		ordinal = count
	}
	ctx = executionevent.WithUnifiedChild(ctx, host.executionID, fmt.Sprintf("call_%d", ordinal), "forward")
	// Stable provider keys let recovery reuse a completed Bridge effect.
	if host.automaticSnapshot {
		key, err := host.providerIdempotencyKey(capabilityHostFetchRequest{}, ordinal)
		// Invalid snapshot identity cannot fall back to an untracked operation.
		if err != nil {
			return nil, err
		}
		ctx = bridgehost.WithSnapshotCall(ctx, key, host.snapshotRunID)
	}
	result, err := host.apps.ExecuteAttachedBridge(ctx, host.identity, request.Alias, request.Input)
	// Unknown effects and unconfirmed commits still halt the attempt before later authored effects.
	if host.cancelBridgeSnapshot(err) {
		host.cancelAttempt()
	}
	return result, err
}

type bridgeHostRequest struct {
	Alias string          `json:"bridge"`
	Input json.RawMessage `json:"input"`
}

// decodeBridgeHostRequest admits exactly one alias/input envelope without alternate routing fields.
func decodeBridgeHostRequest(raw json.RawMessage) (bridgeHostRequest, error) {
	var request bridgeHostRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	// Input is mandatory even when its JSON value is null or an empty object.
	if decoder.Decode(&request) != nil || len(request.Input) == 0 {
		return request, errors.New("invalid Bridge call")
	}
	var trailing any
	// A second JSON document must never be interpreted as another host operation.
	if decoder.Decode(&trailing) != io.EOF {
		return request, errors.New("invalid Bridge call")
	}
	return request, nil
}

// cancelBridgeSnapshot preserves conservative interruption only when the effect was not definitely rejected before dispatch.
func (host *executionCapabilityHost) cancelBridgeSnapshot(err error) bool {
	var invocation *bridgehost.InvocationError
	// Wrapped invocation errors retain the transport's authoritative no-effect guarantee.
	notStarted := errors.As(err, &invocation) && invocation.Outcome == "not_started"
	return host.automaticSnapshot && err != nil && !notStarted && host.cancelAttempt != nil
}
