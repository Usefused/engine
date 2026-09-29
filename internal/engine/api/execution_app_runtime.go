package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

type unifiedAppManifest struct {
	SchemaVersion      int                           `json:"schemaVersion"`
	InputSchema        json.RawMessage               `json:"inputSchema"`
	OutputSchema       json.RawMessage               `json:"outputSchema"`
	Searchable         []string                      `json:"searchable"`
	SelectedOperations []unifiedAppManifestOperation `json:"selectedOperations"`
}

type unifiedAppManifestOperation struct {
	Service          string    `json:"service"`
	Operation        string    `json:"operation"`
	ServiceID        uuid.UUID `json:"serviceId"`
	ServiceVersionID uuid.UUID `json:"serviceVersionId"`
	EndpointID       uuid.UUID `json:"endpointId"`
}

type capabilityHostFetchRequest struct {
	Service    string                      `json:"service"`
	Operation  string                      `json:"operation"`
	Input      map[string]any              `json:"input"`
	Selector   capabilityHostFetchSelector `json:"selector"`
	Pagination *capabilityHostPagination   `json:"pagination,omitempty"`
}

type capabilityHostPagination struct {
	MaxPages int `json:"maxPages"`
}

type capabilityHostFetchSelector struct {
	Environment string `json:"environment"`
	EndUserRef  string `json:"endUserRef"`
	AuthType    string `json:"authType"`
	AuthName    string `json:"authName"`
	ResourceID  string `json:"resourceId"`
}

type executionCapabilityHost struct {
	runtime     *sandbox.EngineGRPCServer
	identity    auth.RuntimeIdentity
	executionID uuid.UUID
	bindings    map[string]sandbox.ExactOperationBinding
	mu          sync.Mutex
	callCount   int
	called      bool
}

// parseUnifiedAppManifest admits one bounded app-level execute descriptor shared by deployment and execution.
func parseUnifiedAppManifest(raw json.RawMessage) (*unifiedAppManifest, error) {
	// Oversized manifests cannot establish a bounded public execute contract.
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("unified app manifest is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest unifiedAppManifest
	if err := decoder.Decode(&manifest); err != nil || manifest.SchemaVersion != 1 {
		return nil, errors.New("unified app manifest is invalid")
	}
	// A trailing document cannot add undeclared authority after the admitted manifest.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unified app manifest has trailing data")
	}
	if err := validateUnifiedAppManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// validateUnifiedAppManifest requires one typed execute contract and exact selected operation set.
func validateUnifiedAppManifest(manifest *unifiedAppManifest) error {
	// Every Unified App needs at least one exact workspace operation, even if a particular run does not call it.
	if !json.Valid(manifest.InputSchema) || !json.Valid(manifest.OutputSchema) || len(manifest.SelectedOperations) == 0 || len(manifest.SelectedOperations) > maxUnifiedAppSelectedOperations {
		return errors.New("unified app manifest is invalid")
	}
	_, err := unifiedAppBindings(manifest)
	return err
}

// executionOperationKey keeps the author-facing service and operation pair exact and collision-free.
func executionOperationKey(service, operation string) string {
	return service + "\x00" + operation
}

// unifiedAppBindings converts only pinned manifest entries into Engine-owned physical identities.
func unifiedAppBindings(manifest *unifiedAppManifest) (map[string]sandbox.ExactOperationBinding, error) {
	bindings := make(map[string]sandbox.ExactOperationBinding, len(manifest.SelectedOperations))
	for _, selected := range manifest.SelectedOperations {
		// An incomplete identity cannot be repaired from worker-supplied names or URLs.
		if selected.Service == "" || selected.Operation == "" || selected.ServiceID == uuid.Nil || selected.ServiceVersionID == uuid.Nil || selected.EndpointID == uuid.Nil {
			return nil, errors.New("unified app selected operation is invalid")
		}
		key := executionOperationKey(selected.Service, selected.Operation)
		// Each author-facing pair must select exactly one physical endpoint.
		if _, exists := bindings[key]; exists {
			return nil, errors.New("unified app selected operation is duplicated")
		}
		bindings[key] = sandbox.ExactOperationBinding{
			ServiceID: selected.ServiceID, ServiceVersionID: selected.ServiceVersionID,
			EndpointID: selected.EndpointID, EndpointName: selected.Operation,
		}
	}
	return bindings, nil
}

// Fetch accepts only a selected workspace operation and delegates provider work to the shared physical boundary.
func (host *executionCapabilityHost) Fetch(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request capabilityHostFetchRequest
	// A worker cannot supply a URL, physical ID, or credential in place of one selected operation.
	if err := decoder.Decode(&request); err != nil || request.Service == "" || request.Operation == "" || request.Input == nil {
		return nil, errors.New("unified app workspace operation request is invalid")
	}
	var pagination *engine.PaginationIntent
	// Only a caller-owned page bound may cross the sandbox; operation policy remains Engine-owned.
	if request.Pagination != nil {
		pagination = &engine.PaginationIntent{MaxPages: request.Pagination.MaxPages}
		if err := engine.ValidatePaginationIntent(pagination); err != nil {
			return nil, err
		}
	}
	binding, allowed := host.bindings[executionOperationKey(request.Service, request.Operation)]
	// Manifest admission precedes any physical resolver or outbound traffic.
	if !allowed {
		return nil, errors.New("unified app workspace operation is not selected")
	}
	host.mu.Lock()
	host.callCount++
	callNumber := host.callCount
	host.mu.Unlock()
	// An invocation gets distinct physical idempotency keys for its selected calls.
	if callNumber > maxCapabilityFetchCalls {
		return nil, errors.New("unified app workspace operation limit exceeded")
	}
	canonical, err := json.Marshal(request.Input)
	if err != nil {
		return nil, errors.New("unified app workspace operation input is invalid")
	}
	digest := sha256.Sum256(canonical)
	selectors := sandbox.PhysicalExecutionSelectors{
		Environment: request.Selector.Environment, EndUserRef: request.Selector.EndUserRef,
		AuthType: request.Selector.AuthType, AuthName: request.Selector.AuthName, ResourceID: request.Selector.ResourceID,
	}
	host.mu.Lock()
	host.called = true
	host.mu.Unlock()
	// Unified App calls wait for shared provider capacity instead of failing
	// when another invocation currently occupies the account's physical slots.
	ctx = sandbox.WithUnifiedAppPhysicalQueue(ctx)
	// Link the existing provider receipt to the authored run without creating another provider event.
	ordinal, hasOrdinal := sandbox.CapabilityCallOrdinal(ctx)
	// Worker ordinals also include database calls and stay stable when concurrent calls race.
	if !hasOrdinal {
		ordinal = callNumber
	}
	ctx = executionevent.WithUnifiedChild(ctx, host.executionID, fmt.Sprintf("call_%d", ordinal), "forward")
	return host.runtime.ExecuteCapabilityWorkspaceOperation(ctx, host.identity, sandbox.CapabilityWorkspaceOperationRequest{
		Binding: binding, Input: request.Input, Selectors: selectors, Pagination: pagination,
		IdempotencyKey: fmt.Sprintf("%s:%d", host.executionID, callNumber), RequestBodyHash: hex.EncodeToString(digest[:]),
	})
}

const maxCapabilityFetchCalls = 32

// DBGet rejects direct access because only the buffered wrapper owns an execution's mutable document.
func (host *executionCapabilityHost) DBGet(context.Context) (json.RawMessage, error) {
	return nil, errors.New("unified app data requires the buffered execution host")
}

// DBSet rejects direct writes so no intermediate document can escape the atomic terminal commit.
func (host *executionCapabilityHost) DBSet(context.Context, json.RawMessage) error {
	return errors.New("unified app data requires the buffered execution host")
}

// providerCallsStarted indicates that a failure may follow a provider side effect.
func (host *executionCapabilityHost) providerCallsStarted() bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.called
}
