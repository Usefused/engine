package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/google/uuid"
)

// CapabilityWorkspaceOperationRequest contains only the exact operation and
// Engine-owned execution context selected from an immutable capability bundle.
// The TypeScript worker must never supply Binding or TrustedCredentials.
type CapabilityWorkspaceOperationRequest struct {
	Binding            ExactOperationBinding
	Input              map[string]any
	Selectors          PhysicalExecutionSelectors
	Pagination         *engine.PaginationIntent
	TrustedCredentials map[string]any
	IdempotencyKey     string
	RequestBodyHash    string
}

// ExecuteCapabilityWorkspaceOperation binds the capability bridge to the
// Engine's active immutable app cache and canonical provider dispatcher.
func (*EngineGRPCServer) ExecuteCapabilityWorkspaceOperation(
	ctx context.Context,
	identity auth.RuntimeIdentity,
	request CapabilityWorkspaceOperationRequest,
) (json.RawMessage, error) {
	// A host without its runtime cache cannot establish selected workspace scope.
	if globalObjectCache == nil || globalDispatcher == nil {
		return nil, errors.New("Engine capability operation runtime is unavailable")
	}
	return ExecuteCapabilityWorkspaceOperation(ctx, globalObjectCache, globalDispatcher, identity, request)
}

// ExecuteCapabilityWorkspaceOperation admits selected calls and retains input-validation explanations before dispatch.
func ExecuteCapabilityWorkspaceOperation(
	ctx context.Context,
	cache ObjectCache,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	request CapabilityWorkspaceOperationRequest,
) (json.RawMessage, error) {
	// A capability may only use the exact app identity established by its authenticated host execution.
	if identity.AppID == uuid.Nil {
		return nil, errors.New("capability execution has no app identity")
	}
	resolved, err := ResolveExactPhysicalOperations(ctx, cache, identity.AppID, []ExactOperationBinding{request.Binding})
	// Exact service, version, and endpoint admission must finish before input or provider work.
	if err != nil {
		return nil, err
	}
	// The one requested binding must resolve to one immutable physical operation.
	if len(resolved) != 1 {
		return nil, errors.New("capability operation resolution is incomplete")
	}
	operation := resolved[0]
	// A page bound must strictly narrow this exact operation's reviewed pagination policy.
	if err := ValidateResolvedPhysicalPaginationIntent(operation, request.Pagination); err != nil {
		return nil, err
	}
	// Runtime selectors cannot override the selected operation's auth or environment contract.
	if err := operation.ValidateSelectors(request.Selectors); err != nil {
		return nil, err
	}
	fixture, err := capabilityFixtureOperation(operation)
	// The MCP catalogue schema conversion is also the capability input validation source.
	if err != nil {
		return nil, err
	}
	// Invalid provider inputs stop before the physical dispatcher can make outbound traffic.
	if err := validateCallParams(fixture, request.Input); err != nil {
		return nil, &executionappvm.DiagnosticError{Phase: "operation_validation", Message: executionappvm.BoundDiagnostic(err.Error())}
	}
	credentials := copyCredentialEnvelope(request.TrustedCredentials)
	// Anonymous calls still need a writable map when a validated routing selector is present.
	if credentials == nil {
		credentials = make(map[string]any)
	}
	for key, value := range physicalSelectorCredentials(request.Selectors) {
		// Validated caller selectors take precedence over inherited Engine routing context.
		credentials[key] = value
	}
	physical := PhysicalExecutionRequest{
		Params: request.Input, Credentials: credentials, Environment: request.Selectors.Environment,
		IdempotencyKey: request.IdempotencyKey, RequestBodyHash: request.RequestBodyHash, Pagination: request.Pagination,
	}
	return executeCapabilityPhysicalValue(ctx, dispatcher, identity, operation, physical)
}

// capabilityFixtureOperation reuses MCP's schema view and exact-version index after app-scope resolution.
func capabilityFixtureOperation(operation ResolvedPhysicalOperation) (*FixtureOperation, error) {
	match := operation.match
	// An invalid opaque operation cannot become a schema-validation bypass.
	if match == nil || match.service == nil || !match.allowed {
		return nil, errors.New("resolved capability operation is invalid")
	}
	fixture, err := endpointToFixtureOperation(match.service.ID.String(), match.endpoint, nil)
	// Unsafe or malformed provider schemas are never admitted into a capability worker call.
	if err != nil {
		return nil, err
	}
	// JSON conversion omits runtime indexes; rebind every schema surface to the resolved version before validation.
	if err := bindMCPDefinitionIndex(&fixture, match.service.DefinitionIndex); err != nil {
		return nil, err
	}
	stripMCPAuthParameters(&fixture, match.selection.AuthName)
	return &fixture, nil
}

// executeCapabilityPhysicalValue preserves one physical receipt and separates provider explanations from raw bodies.
func executeCapabilityPhysicalValue(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	identity auth.RuntimeIdentity,
	operation ResolvedPhysicalOperation,
	request PhysicalExecutionRequest,
) (json.RawMessage, error) {
	stream := engine.NewBoundedBufferStream(maxMCPPhysicalResultBytes)
	var result json.RawMessage
	// The collector finalizes the exact bounded value before the physical receipt is marked successful.
	validate := func() error {
		var err error
		result, err = mcpPhysicalBufferedResult(stream)
		return err
	}
	// Validation remains inside the physical boundary so result failures produce one failed child receipt.
	if err := executeResolvedPhysicalBoundary(ctx, dispatcher, identity, operation, request, stream, validate); err != nil {
		var missing *CredentialMaterialMissingError
		// Credential failures happen before dispatch and must retain their typed, value-free recovery guidance.
		if errors.As(err, &missing) {
			return nil, missing
		}
		message := capabilityProviderFailureMessage(stream, err)
		// Only the selected explanation is public; the complete provider body remains privileged evidence.
		return nil, &executionappvm.DiagnosticError{Phase: "provider", Message: executionappvm.BoundDiagnostic(message), Response: executionappvm.BoundDiagnostic(stream.String()), Truncated: len(stream.Bytes()) > 65536 || len(message) > 65536}
	}
	return result, nil
}

// capabilityProviderFailureMessage extracts conventional JSON error text without exposing the whole response.
func capabilityProviderFailureMessage(stream *engine.BufferStream, failure error) string {
	// Transport and local failures have no authoritative provider error response to parse.
	if stream.Status() < http.StatusBadRequest {
		return failure.Error()
	}
	var body struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	// HTML, malformed JSON, and unknown response shapes stay in permission-gated diagnostics.
	if json.Unmarshal(stream.Bytes(), &body) != nil {
		return failure.Error()
	}
	var nested struct {
		Message string `json:"message"`
	}
	var plain string
	_ = json.Unmarshal(body.Error, &nested)
	_ = json.Unmarshal(body.Error, &plain)
	for _, message := range []string{nested.Message, plain, body.Message, body.Detail} {
		// Prefer the provider's specific error over a generic envelope description.
		if strings.TrimSpace(message) != "" {
			return fmt.Sprintf("provider returned HTTP %d: %s", stream.Status(), message)
		}
	}
	return failure.Error()
}
