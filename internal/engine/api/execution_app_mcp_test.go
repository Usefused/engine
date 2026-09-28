package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestExecutionAppInputSchemaUsesExactBundle checks hosted MCP reads only its exact Execution App version.
func TestExecutionAppInputSchemaUsesExactBundle(t *testing.T) {
	appID := uuid.New()
	manifest := json.RawMessage(`{"schemaVersion":1,"inputSchema":{"type":"object","properties":{"name":{"type":"string"}}},"outputSchema":{"type":"object"},"searchable":[],"selectedOperations":[{"service":"issues","operation":"createIssue","serviceId":"11111111-1111-4111-8111-111111111111","serviceVersionId":"22222222-2222-4222-8222-222222222222","endpointId":"33333333-3333-4333-8333-333333333333"}]}`)
	fixture := &capabilityRouteStore{activeAppID: appID, bundle: store.ExecutionAppBundle{AppID: appID, SourceHash: "sha256:fixture", Manifest: manifest}}
	server := &EngineGRPCServer{store: fixture}
	identity := auth.RuntimeIdentity{AppID: appID, Kind: store.AppKindExecution, HostedMCP: true, Status: store.AppStatusActive}
	schema, found, err := server.ExecutionAppInputSchema(context.Background(), identity)
	if err != nil || !found || len(schema) == 0 {
		t.Fatalf("schema=%s found=%v err=%v", schema, found, err)
	}
	// A different version without its own app record cannot borrow this bundle's authored schema.
	identity.AppID = uuid.New()
	schema, found, err = server.ExecutionAppInputSchema(context.Background(), identity)
	if err == nil || found || len(schema) != 0 {
		t.Fatalf("sibling schema=%s found=%v err=%v", schema, found, err)
	}
}

// TestExecuteExecutionAppRejectsOrdinarySDK keeps non-HostedMCP tokens out of authored execution.
func TestExecuteExecutionAppRejectsOrdinarySDK(t *testing.T) {
	server := &EngineGRPCServer{}
	_, err := server.ExecuteExecutionApp(context.Background(), auth.RuntimeIdentity{Kind: store.AppKindSDK}, json.RawMessage(`{}`))
	// The adapter rejects the caller before a bundle or durable result can be loaded.
	if err != sandbox.ErrMCPCapabilityUnavailable {
		t.Fatalf("unopted execution error = %v", err)
	}
}
