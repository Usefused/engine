package bridgehost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Pin is copied into an immutable app contract; runtime callers cannot author these routing fields.
type Pin struct {
	PairingID    string `json:"pairing_id"`
	ProfileID    string `json:"profile_id"`
	ManifestHash string `json:"manifest_hash"`
	ConnectionID string `json:"connection_id"`
	OperationID  string `json:"operation_id"`
	ContractHash string `json:"contract_hash"`
}

// InvocationError preserves uncertainty without reflecting provider data in diagnostics.
type InvocationError struct {
	Outcome string
	Code    string
}

// Error exposes only bounded protocol vocabulary to execution adapters.
func (e *InvocationError) Error() string { return "bridge " + e.Code + " (" + e.Outcome + ")" }

type routedCall struct {
	Account string         `json:"account"`
	Pin     Pin            `json:"pin"`
	Call    *bridgev1.Call `json:"call"`
}

// StartRouting subscribes only this replica's private live-call subject, with bounded concurrency.
func (s *Server) StartRouting(ctx context.Context) error {
	// Unit fixtures without a broker still exercise local dispatch.
	if s.conn == nil {
		return nil
	}
	slots := make(chan struct{}, 32)
	sub, err := s.conn.Subscribe("fused.bridge.calls."+s.instance, func(message *nats.Msg) {
		// Saturated replicas reject before accepting any command responsibility.
		select {
		case slots <- struct{}{}:
		default:
			raw, _ := json.Marshal(&bridgev1.CallResult{Outcome: "not_started", ErrorCode: "busy"})
			_ = message.Respond(raw)
			return
		}
		go func() {
			defer func() { <-slots }()
			result := s.handleRoutedCall(ctx, message.Data)
			raw, _ := json.Marshal(result)
			_ = message.Respond(raw)
		}()
	})
	// Failed internal routing cannot be advertised as an available provider session.
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = sub.Unsubscribe() }()
	return s.conn.FlushTimeout(5 * time.Second)
}

// handleRoutedCall rechecks authoritative routing state on the owning replica.
func (s *Server) handleRoutedCall(parent context.Context, raw []byte) *bridgev1.CallResult {
	var request routedCall
	// Internal messages still have explicit size and shape boundaries.
	if len(raw) > MaxManifest || json.Unmarshal(raw, &request) != nil || request.Call == nil {
		return &bridgev1.CallResult{Outcome: "not_started", ErrorCode: "invalid_request"}
	}
	ctx, cancel := context.WithDeadline(parent, time.UnixMilli(request.Call.DeadlineUnixMs))
	defer cancel()
	p, owner, _, err := s.resolvePin(ctx, request.Account, request.Pin, request.Call.InputJson)
	// Routing is generation-bound; a stale owner never forwards to its successor automatically.
	if err != nil || owner != s.instance || p.Generation != request.Call.Generation {
		return &bridgev1.CallResult{RequestId: request.Call.RequestId, Outcome: "not_started", ErrorCode: "session_unavailable"}
	}
	return s.invokeLocal(ctx, p, request.Call)
}

// Invoke routes one authorized immutable selection without retrying uncertain provider execution.
func (s *Server) Invoke(ctx context.Context, account string, pin Pin, input []byte) ([]byte, error) {
	p, owner, contract, err := s.resolvePin(ctx, account, pin, input)
	// Contract and current permission checks precede every local or remote dispatch.
	if err != nil {
		return nil, &InvocationError{Outcome: "not_started", Code: "unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	call := &bridgev1.Call{RequestId: uuid.NewString(), ProfileId: pin.ProfileID, ConnectionId: pin.ConnectionID, OperationId: pin.OperationID, ContractHash: pin.ContractHash, InputJson: input, DeadlineUnixMs: deadline.UnixMilli(), Generation: p.Generation, ManifestHash: pin.ManifestHash, IdempotencyKey: CallIdentity(ctx)}
	var result *bridgev1.CallResult
	// Ownership determines the single forwarding destination, never a broadcast or durable command queue.
	if owner == s.instance {
		result = s.invokeLocal(ctx, p, call)
	} else {
		result, err = s.invokeRemote(ctx, account, pin, owner, call)
		if err != nil {
			return nil, err
		}
	}
	return validateResult(result, call, contract)
}

// invokeRemote treats a lost reply as unknown rather than resending a possible command.
func (s *Server) invokeRemote(ctx context.Context, account string, pin Pin, owner string, call *bridgev1.Call) (*bridgev1.CallResult, error) {
	// Missing broker transport fails before command dispatch.
	if s.conn == nil {
		return nil, &InvocationError{Outcome: "not_started", Code: "session_unavailable"}
	}
	raw, _ := json.Marshal(routedCall{Account: account, Pin: pin, Call: call})
	reply, err := s.conn.RequestWithContext(ctx, "fused.bridge.calls."+owner, raw)
	// No responder is known not to have accepted this request; other failures are uncertain.
	if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrMaxPayload) {
		return nil, &InvocationError{Outcome: "not_started", Code: "session_unavailable"}
	}
	if err != nil {
		return nil, &InvocationError{Outcome: "unknown", Code: "connection_lost"}
	}
	var result bridgev1.CallResult
	// Invalid peer responses cannot be interpreted as successful execution.
	if len(reply.Data) > MaxManifest || json.Unmarshal(reply.Data, &result) != nil {
		return nil, &InvocationError{Outcome: "unknown", Code: "invalid_result"}
	}
	return &result, nil
}

// resolvePin validates archived schemas and verifies that the currently shared profile still supports them.
func (s *Server) resolvePin(ctx context.Context, account string, pin Pin, input []byte) (Pairing, string, Contract, error) {
	return s.resolveAdmittedPin(ctx, account, pin, input, true)
}

func (s *Server) resolveAdmittedPin(ctx context.Context, account string, pin Pin, input []byte, online bool) (Pairing, string, Contract, error) {
	var p Pairing
	var owner string
	var archived, current []byte
	err := s.repo.Pool.QueryRow(ctx, `SELECT p.id,p.destination_id,p.installation_id,p.name,p.scope,p.profile_ids,p.account_id,p.workspace_id,p.generation,p.owner_instance,m.document,live.document
 FROM fused_bridge_pairings p
 JOIN fused_bridge_manifests m ON m.pairing_id=p.id AND m.profile_id=$3 AND m.hash=$4
 JOIN fused_bridge_profiles c ON c.pairing_id=p.id AND c.profile_id=$3
 JOIN fused_bridge_manifests live ON live.pairing_id=p.id AND live.profile_id=c.profile_id AND live.hash=c.manifest_hash
 WHERE p.id=$1 AND p.account_id=$2 AND p.revoked_at IS NULL AND p.scope='workspace' AND c.state<>'removed' AND (NOT $5 OR (p.lease_until>now() AND p.inventory_generation=p.generation AND c.state='online')) AND `+ownerAuthorized, pin.PairingID, account, pin.ProfileID, pin.ManifestHash, online).Scan(&p.ID, &p.DestinationID, &p.InstallationID, &p.Name, &p.Scope, &p.Profiles, &p.AccountID, &p.WorkspaceID, &p.Generation, &owner, &archived, &current)
	// Cross-account or revoked pins cannot borrow the catalogue's previous authority.
	if err != nil {
		return p, "", Contract{}, ErrDenied
	}
	var oldDoc, liveDoc ManifestDocument
	if json.Unmarshal(archived, &oldDoc) != nil || json.Unmarshal(current, &liveDoc) != nil {
		return p, "", Contract{}, ErrContract
	}
	contract, err := operation(oldDoc, pin.ConnectionID, pin.OperationID, pin.ContractHash)
	// Events are subscriptions, never executable unary methods.
	if err != nil || contract.Kind == "event" {
		return p, "", Contract{}, ErrContract
	}
	if _, err = operation(liveDoc, pin.ConnectionID, pin.OperationID, pin.ContractHash); err != nil {
		return p, "", Contract{}, err
	}
	if err = ValidatePayload(contract.InputSchema, input); err != nil {
		return p, "", Contract{}, err
	}
	return p, owner, contract, nil
}

// invokeLocal uses a bounded pending map and never retries after handing a frame to the session.
func (s *Server) invokeLocal(ctx context.Context, p Pairing, call *bridgev1.Call) *bridgev1.CallResult {
	result := &bridgev1.CallResult{RequestId: call.RequestId, Generation: call.Generation, Outcome: "not_started", ErrorCode: "session_unavailable"}
	s.mu.Lock()
	current := s.sessions[p.ID]
	s.mu.Unlock()
	// An old local map entry cannot receive work for the new persisted generation.
	if current == nil || current.pairing.Generation != p.Generation {
		return result
	}
	if call.IdempotencyKey != "" && !current.pairing.DurableCalls {
		result.ErrorCode = "idempotency_unsupported"
		return result
	}
	current.mu.Lock()
	// Pending calls are bounded independently of the stream's send buffer.
	if len(current.results) >= 32 {
		current.mu.Unlock()
		result.ErrorCode = "busy"
		return result
	}
	output := make(chan *bridgev1.CallResult, 1)
	current.results[call.RequestId] = output
	current.mu.Unlock()
	defer func() { current.mu.Lock(); delete(current.results, call.RequestId); current.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return result
	case <-current.done:
		return result
	case current.out <- &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_Call{Call: call}}:
	}
	return awaitCallResult(ctx, current, call, output, result)
}

// awaitCallResult preserves uncertainty after dispatch and sends only best-effort cancellation on timeout.
func awaitCallResult(ctx context.Context, current *session, call *bridgev1.Call, output <-chan *bridgev1.CallResult, result *bridgev1.CallResult) *bridgev1.CallResult {
	// Once enqueued, transport failure cannot prove that the device did not execute.
	result.Outcome = "unknown"
	result.ErrorCode = "connection_lost"
	select {
	case response := <-output:
		return response
	case <-current.done:
		return result
	case <-ctx.Done():
		// Cancellation is best effort and never claims that upstream effects were undone.
		select {
		case current.out <- &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_Cancel{Cancel: &bridgev1.Cancel{RequestId: call.RequestId, Generation: call.Generation}}}:
		default:
		}
		result.ErrorCode = "deadline_exceeded"
		return result
	}
}

// validateResult binds responses to their call and validates successful outputs against the pinned schema.
func validateResult(result *bridgev1.CallResult, call *bridgev1.Call, contract Contract) ([]byte, error) {
	// Some pre-dispatch internal denials have no generation because no Bridge session was contacted.
	if result == nil {
		return nil, &InvocationError{Outcome: "unknown", Code: "invalid_result"}
	}
	if result.Outcome == "not_started" {
		return nil, &InvocationError{Outcome: "not_started", Code: "rejected"}
	}
	if result.RequestId != call.RequestId || result.Generation != call.Generation || result.Outcome != "completed" {
		return nil, &InvocationError{Outcome: "unknown", Code: "incomplete"}
	}
	// A malformed output does not mean the provider command failed to run.
	if err := ValidatePayload(contract.OutputSchema, result.ResultJson); err != nil {
		return nil, &InvocationError{Outcome: "unknown", Code: "invalid_result"}
	}
	return result.ResultJson, nil
}
