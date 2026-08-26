package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestUnifiedReceiptMetadataExcludesPayloads retains diagnostic steps without mapped or provider content.
func TestUnifiedReceiptMetadataExcludesPayloads(t *testing.T) {
	call := preparedUnifiedCall{receiptID: uuid.New(), appID: uuid.New(), operation: "items.read", transport: "mcp", input: map[string]any{"secret": "input-private"}, identity: auth.RuntimeIdentity{AccountID: uuid.New(), AppFamilyID: uuid.New(), AppVersion: "1.0.0"}}
	response := &enginev1.ExecuteUnifiedResponse{
		Results:         []*enginev1.UnifiedTargetResult{{Target: "list", Status: "success", DataJson: []byte(`"provider-private"`)}, {Target: "details", Status: "error", ErrorCode: "provider_failed"}, {Target: "dependent", Status: "skipped", ErrorCode: "dependency_failed"}},
		RollbackResults: []*enginev1.UnifiedRollbackResult{{Target: "list", Status: "success"}}, OutputJson: []byte(`"output-private"`),
	}
	started := time.Now().UTC()
	event := unifiedReceipt(context.Background(), call, response, started, started.Add(90*time.Millisecond))
	// Compensation is separate evidence and cannot turn the failed operation into success.
	if event.Status != "failed" || len(event.UnifiedSteps) != 4 || event.LatencyMs != 90 {
		t.Fatal("incorrect logical outcome")
	}
	// The canonical contract must accept the metadata we actually produce.
	if err := executionevent.ValidateUnifiedMetadata(event); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	// Serialization failures must not conceal an absent privacy assertion.
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"input-private", "provider-private", "output-private"} {
		// Never print the serialized payload when the privacy check fails.
		if strings.Contains(string(encoded), sentinel) {
			t.Fatal("logical receipt leaked payload content")
		}
	}
	response.OutputErrorCode = "output_validation_failed"
	event = unifiedReceipt(context.Background(), call, response, started, started)
	// Root output failure stays distinguishable from provider failures.
	if event.FailureCode != response.OutputErrorCode {
		t.Fatal("output error was lost")
	}
}

// TestUnifiedReceiptPublishedForSDKAndMCP confirms both authenticated adapters use one canonical producer.
func TestUnifiedReceiptPublishedForSDKAndMCP(t *testing.T) {
	for _, transport := range []string{"sdk", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
			capture, _ := installUnifiedAccountingCaptures(t)
			ctx := grpcTestContext(appID)
			// MCP uses the same Unified runtime with a server-owned transport label.
			if transport == "mcp" {
				server.store.(*grpcRuntimeStore).scope.Kind = store.AppKindMCP
				validator := server.tokenValidator.(unifiedTestValidator)
				validator.identity.Kind = store.AppKindMCP
				server.tokenValidator = validator
				ctx = sandbox.ContextWithMCPExecutionTransport(ctx)
			}
			_, err := server.ExecuteUnified(ctx, unifiedRuntimeRequest())
			// The test runtime has no physical audit producer, leaving exactly the logical receipt.
			if err != nil || len(capture.messages) != 1 {
				t.Fatalf("logical receipt count=%d error=%v", len(capture.messages), err)
			}
			var envelope models.EngineExecutionEventEnvelope
			if err := json.Unmarshal(capture.messages[0], &envelope); err != nil {
				t.Fatal(err)
			}
			// Producer identity is durable even when OpenTelemetry is disabled.
			if envelope.Event.ExecutionKind != "unified" || envelope.Event.Transport != transport || envelope.Event.AppID != appID {
				t.Fatal("incorrect logical transport/identity")
			}
		})
	}
}

// TestUnifiedReceiptRESTUsesSharedProducer keeps the HTTP adapter on the same logical audit path.
func TestUnifiedReceiptRESTUsesSharedProducer(t *testing.T) {
	server, _, _ := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	capture, _ := installUnifiedAccountingCaptures(t)
	scope := server.store.(*grpcRuntimeStore).scope
	identity := server.tokenValidator.(unifiedTestValidator).identity
	input := unifiedRuntimeRequest()
	_, err := server.executeRESTUnified(context.Background(), scope, identity, restExecutionRequest{Input: input.InputJson, Targets: input.Targets}, restExecutionPlan{operation: input.Operation, kind: "unified"}, input.IdempotencyKey)
	// The test physical runtime intentionally emits no receipts of its own.
	if err != nil || len(capture.messages) != 1 {
		t.Fatal("REST did not publish one logical receipt")
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(capture.messages[0], &envelope); err != nil {
		t.Fatal(err)
	}
	// REST remains an SDK app transport rather than acquiring an adapter-specific history model.
	if envelope.Event.ExecutionKind != "unified" || envelope.Event.Transport != "rest" {
		t.Fatal("REST logical receipt identity mismatch")
	}
}

// TestUnifiedControlTargetRejectedBeforeDispatch prevents post-execution audit loss for unsafe authored identities.
func TestUnifiedControlTargetRejectedBeforeDispatch(t *testing.T) {
	server, runtime, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	request := unifiedRuntimeRequest()
	request.Targets = []string{"read\nitems"}
	_, err := server.ExecuteUnified(grpcTestContext(appID), request)
	// No physical work can occur for a target that cannot enter durable history safely.
	if err == nil || runtime.executeCalls != 0 {
		t.Fatal("unsafe target reached provider execution")
	}
}
