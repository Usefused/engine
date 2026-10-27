package api

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bridgeInvoker keeps protocol routing independent from authenticated app admission.
type bridgeInvoker interface {
	Invoke(context.Context, string, bridgehost.Pin, []byte) ([]byte, error)
}

// ConfigureBridgeHost shares the process session owner across all app transports.
func (s *EngineGRPCServer) ConfigureBridgeHost(host *bridgehost.Server) { s.bridgeHost = host }

// AttachedBridges exposes only exact published bindings admitted by the caller's family token.
func (s *EngineGRPCServer) AttachedBridges(ctx context.Context, identity auth.RuntimeIdentity) ([]models.BridgeBinding, error) {
	repository, ok := s.store.(store.BridgeBindingStore)
	// Unsupported storage and identities cannot turn discovery into runtime authority.
	if !ok || (identity.Kind != store.AppKindSDK && identity.Kind != store.AppKindMCP && identity.Kind != store.AppKindUnifiedApp) {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	bindings, err := repository.ReadBridgeBindings(ctx, identity.AccountID, identity.AppID)
	// Immutable publication must remain readable before any contract is exposed.
	if err != nil {
		return nil, err
	}
	allowed := make([]models.BridgeBinding, 0, len(bindings))
	for _, b := range bindings {
		// Event subscriptions are not executable methods, and token restrictions remain authoritative.
		if b.Kind != "event" && identity.AllowsOperation("bridge:"+b.Alias) {
			allowed = append(allowed, b)
		}
	}
	return allowed, nil
}

// ExecuteAttachedBridge dispatches one immutable alias through the shared capacity gate and receipt path.
func (s *EngineGRPCServer) ExecuteAttachedBridge(ctx context.Context, identity auth.RuntimeIdentity, alias string, input json.RawMessage) (json.RawMessage, error) {
	// Caller input never supplies a pairing, destination, connection, or contract override.
	if s.bridgeHost == nil || !unifiedAppAlias.MatchString(alias) || !identity.AllowsOperation("bridge:"+alias) {
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	bindings, err := s.AttachedBridges(ctx, identity)
	// Storage errors cannot fall through to a broader catalogue lookup.
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		// The alias must belong to this exact immutable app version.
		if b.Alias == alias {
			pin := bridgehost.Pin{PairingID: b.PairingID, ProfileID: b.ProfileID, ManifestHash: b.ManifestHash, ConnectionID: b.ConnectionID, OperationID: b.OperationID, ContractHash: b.ContractHash}
			if _, run := bridgehost.SnapshotCall(ctx); run != uuid.Nil {
				return s.executeBridgeSnapshot(ctx, identity, alias, pin, input)
			}
			// Dispatch performs live pairing, schema and generation checks immediately before the call.
			return sandbox.RunBridgeInvocation(ctx, identity, alias, func(ctx context.Context) (json.RawMessage, error) {
				return s.bridgeHost.Invoke(ctx, identity.AccountID.String(), pin, input)
			})
		}
	}
	return nil, sandbox.ErrMCPCapabilityUnavailable
}

// tryBridgeRun keeps local-provider calls on the authenticated app endpoint without retrying commands.
func (s *EngineGRPCServer) tryBridgeRun(w http.ResponseWriter, r *http.Request, identity auth.RuntimeIdentity, decoded restExecutionRequest) bool {
	// Other operation namespaces retain their own dispatchers.
	if !strings.HasPrefix(decoded.Operation, "bridge:") {
		return false
	}
	// Local operations have no provider routing or pagination overrides.
	if decoded.Selector != nil || decoded.Pagination != nil {
		writeRESTExecutionError(w, newRESTExecutionError(400, "invalid_bridge_call", "Bridge calls do not accept selectors or pagination"))
		return true
	}
	result, err := s.ExecuteAttachedBridge(sandbox.ContextWithRESTExecutionTransport(r.Context()), identity, strings.TrimPrefix(decoded.Operation, "bridge:"), decoded.Input)
	// Uncertainty is never translated into an automatic retry suggestion.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(409, "bridge_unavailable", "Bridge execution failed or its outcome is unknown; do not automatically retry commands"))
		return true
	}
	// Local results use the shared REST envelope so clients can verify app and operation identity.
	writeRESTExecutionJSON(w, http.StatusOK, restExecutionSuccess{AppID: identity.AppID.String(), Operation: decoded.Operation, Kind: "bridge", StatusCode: http.StatusOK, Results: []json.RawMessage{result}})
	return true
}

// executeBridgeGRPC authenticates the exact app before resolving an authored alias.
func (s *EngineGRPCServer) executeBridgeGRPC(req *enginev1.ExecuteRequest, stream enginev1.EngineService_ExecuteServer) error {
	_, identity, err := s.authenticatedAppRuntimeFromGRPC(stream.Context())
	// The same family token and active-version rules apply to every SDK transport.
	if err != nil {
		return err
	}
	// Bridge configuration is owned by the local installation, never caller credentials.
	if len(req.Credentials) > 0 || req.Environment != "" || req.Pagination != nil {
		return status.Error(codes.InvalidArgument, "unsupported Bridge request controls")
	}
	result, err := s.ExecuteAttachedBridge(stream.Context(), identity, strings.TrimPrefix(req.EndpointName, "bridge:"), req.Params)
	// A transport error cannot prove that a local command did not run.
	if err != nil {
		return status.Error(codes.FailedPrecondition, "Bridge execution failed or its outcome is unknown; do not automatically retry commands")
	}
	return stream.Send(&enginev1.ExecuteResponse{Result: result, StatusCode: 200})
}
