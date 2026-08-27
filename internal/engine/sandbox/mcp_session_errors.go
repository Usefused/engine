package sandbox

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	mcpJSONRPCTransportErrorCode     = -32001
	mcpSessionInitializeRequiredCode = "MCP_SESSION_INITIALIZE_REQUIRED"
	mcpSessionHeaderRequiredCode     = "MCP_SESSION_HEADER_REQUIRED"
	mcpSessionUnavailableCode        = "MCP_SESSION_UNAVAILABLE"
	mcpSessionProtocolMismatchCode   = "MCP_SESSION_PROTOCOL_MISMATCH"
	mcpSessionStartFailedCode        = "MCP_SESSION_START_FAILED"
	mcpRuntimeDispatchFailedCode     = "MCP_RUNTIME_DISPATCH_FAILED"
	mcpRuntimeResponseFailedCode     = "MCP_RUNTIME_RESPONSE_FAILED"
	mcpRuntimeOutcomeUnknownCode     = "MCP_EXECUTION_OUTCOME_UNKNOWN"
)

var (
	errMCPSessionHeaderRequired   = errors.New("Mcp-Session-Id header is required")
	errMCPSessionUnavailable      = errors.New("mcp session not found or expired")
	errMCPSessionProtocolMismatch = errors.New("MCP-Protocol-Version does not match the session")
)

// mcpSessionFailureData separates compact agent recovery from richer bounded transport diagnostics.
type mcpSessionFailureData struct {
	Code              string `json:"code"`
	RecoveryAction    string `json:"recovery_action"`
	ExecuteRequest    string `json:"execute_request"`
	ProviderExecution string `json:"provider_execution"`
	AutomaticReplay   bool   `json:"automatic_replay"`

	Phase           string `json:"-"`
	SessionState    string `json:"-"`
	RequestDelivery string `json:"-"`
	SideEffectState string `json:"-"`
}

// mcpInitializeRequiredFailure directs the transport owner to establish state before any tool request.
func mcpInitializeRequiredFailure() mcpSessionFailureData {
	return mcpSessionFailureData{
		Code: mcpSessionInitializeRequiredCode, RecoveryAction: "initialize_connection",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: "session_initialization", SessionState: "missing", RequestDelivery: "not_started", SideEffectState: "none",
	}
}

// mcpSessionHeaderRequiredFailure keeps missing transport state out of model-authored arguments.
func mcpSessionHeaderRequiredFailure() mcpSessionFailureData {
	return mcpSessionFailureData{
		Code: mcpSessionHeaderRequiredCode, RecoveryAction: "initialize_connection",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: "session_authentication", SessionState: "missing", RequestDelivery: "not_started", SideEffectState: "none",
	}
}

// mcpUnavailableSessionFailure makes a dead or inaccessible transport state recoverable without confirming its former owner.
func mcpUnavailableSessionFailure() mcpSessionFailureData {
	return mcpSessionFailureData{
		Code: mcpSessionUnavailableCode, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "reformat_if_session_state_used", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: "session_authentication", SessionState: "unavailable", RequestDelivery: "not_started", SideEffectState: "none",
	}
}

// mcpSessionProtocolMismatchFailure directs the client to its negotiated header without asking the model to guess it.
func mcpSessionProtocolMismatchFailure() mcpSessionFailureData {
	return mcpSessionFailureData{
		Code: mcpSessionProtocolMismatchCode, RecoveryAction: "use_negotiated_protocol",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: "protocol_validation", SessionState: "active", RequestDelivery: "not_started", SideEffectState: "none",
	}
}

// mcpSessionStartFailureData tells the agent that changing execute syntax cannot repair server initialization.
func mcpSessionStartFailureData(code string) mcpSessionFailureData {
	recoveryAction := "retry_initialize"
	// Reviewed catalogue failures need an owner correction, while transient runtime startup can be retried once.
	if code != mcpSessionStartFailedCode {
		recoveryAction = "contact_server_owner"
	}
	return mcpSessionFailureData{
		Code: code, RecoveryAction: recoveryAction,
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: "session_initialization", SessionState: "missing", RequestDelivery: "not_started", SideEffectState: "none",
	}
}

// mcpSessionAuthenticationFailure maps only reviewed session states and leaves credential failures opaque.
func mcpSessionAuthenticationFailure(err error) (mcpSessionFailureData, bool) {
	// Missing transport identity requires initialization, not a model-authored session parameter.
	if errors.Is(err, errMCPSessionHeaderRequired) {
		return mcpSessionHeaderRequiredFailure(), true
	}
	// Lookup deliberately merges expiry, wrong app, and cross-transport use to avoid confirming another session.
	if errors.Is(err, errMCPSessionUnavailable) {
		return mcpUnavailableSessionFailure(), true
	}
	// The already negotiated version is client transport state and must be reused unchanged.
	if errors.Is(err, errMCPSessionProtocolMismatch) {
		return mcpSessionProtocolMismatchFailure(), true
	}
	return mcpSessionFailureData{}, false
}

// mcpDispatchFailure distinguishes a proven rejection from request-aware partial delivery.
func mcpDispatchFailure(request mcpJSONRPCRequest, written int) mcpSessionFailureData {
	// Zero written bytes prove the child could not begin this request, including execute.
	if written == 0 {
		failure := mcpSessionFailureData{
			Code: mcpRuntimeDispatchFailedCode, RecoveryAction: "reinitialize_connection",
			ExecuteRequest: "reformat_if_session_state_used", ProviderExecution: "not_started", AutomaticReplay: false,
			Phase: "runtime_dispatch", SessionState: "terminated", RequestDelivery: "not_started", SideEffectState: "none",
		}
		// Read-only protocol and discovery requests have no session-authored execute script to rebuild.
		if !mcpRequestMayExecuteProvider(request) {
			failure.ExecuteRequest = "unchanged"
		}
		return failure
	}
	return mcpRuntimeFailureForRequest(request, mcpRuntimeDispatchFailedCode, "runtime_dispatch", "unknown")
}

// mcpRuntimeFailureForRequest preserves uncertainty only for requests capable of provider execution.
func mcpRuntimeFailureForRequest(request mcpJSONRPCRequest, code, phase, delivery string) mcpSessionFailureData {
	return mcpRuntimeFailureForExecutionCapability(mcpRequestMayExecuteProvider(request), code, phase, delivery)
}

// mcpRuntimeFailureForExecutionCapability classifies retained timeout state without retaining request content.
func mcpRuntimeFailureForExecutionCapability(mayExecuteProvider bool, code, phase, delivery string) mcpSessionFailureData {
	// Execute can contain provider mutations, so delivery without a result cannot be replayed automatically.
	if mayExecuteProvider {
		return mcpUnknownRuntimeOutcome(phase, delivery)
	}
	return mcpSessionFailureData{
		Code: code, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
		Phase: phase, SessionState: "terminated", RequestDelivery: delivery, SideEffectState: "none",
	}
}

// mcpRequestMayExecuteProvider recognizes the sole model tool that can cross the provider boundary.
func mcpRequestMayExecuteProvider(request mcpJSONRPCRequest) bool {
	// Protocol methods and notifications never invoke provider operations.
	if request.Method != "tools/call" {
		return false
	}
	var params struct {
		Name string `json:"name"`
	}
	// Malformed or future tool-call shapes default to uncertainty rather than authorizing an unsafe replay.
	if json.Unmarshal(request.Params, &params) != nil {
		return true
	}
	// Documentation search is Engine metadata only; execute may perform one or more provider calls.
	return params.Name != "search_docs"
}

// mcpUnknownRuntimeOutcome preserves mutation uncertainty after a request may have reached the child runtime.
func mcpUnknownRuntimeOutcome(phase, delivery string) mcpSessionFailureData {
	return mcpSessionFailureData{
		Code: mcpRuntimeOutcomeUnknownCode, RecoveryAction: "do_not_replay",
		ExecuteRequest: "do_not_replay", ProviderExecution: "unknown", AutomaticReplay: false,
		Phase: phase, SessionState: "terminated", RequestDelivery: delivery, SideEffectState: "unknown",
	}
}

// writeMCPJSONRPCSessionError retains request correlation while adding machine-readable recovery state.
func writeMCPJSONRPCSessionError(w http.ResponseWriter, id json.RawMessage, message string, status int, failure mcpSessionFailureData) {
	writeMCPJSONRPCErrorData(w, id, mcpJSONRPCTransportErrorCode, message, status, failure)
}

// writeMCPSessionHTTPError serves typed state when a transport request has no usable JSON-RPC correlation.
func writeMCPSessionHTTPError(w http.ResponseWriter, status int, message string, failure mcpSessionFailureData) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message, "data": failure})
}

// recordMCPTransportFailure adds only bounded enums to the existing transport span.
func recordMCPTransportFailure(span trace.Span, outcome string, failure mcpSessionFailureData) {
	span.SetAttributes(
		attribute.String("error.code", failure.Code),
		attribute.String("mcp.request_delivery", failure.RequestDelivery),
		attribute.String("mcp.session_state", failure.SessionState),
		attribute.String("mcp.side_effect_state", failure.SideEffectState),
	)
	recordMCPTransportOutcome(span, outcome, true)
}
