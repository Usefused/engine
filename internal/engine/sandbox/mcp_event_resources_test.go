package sandbox

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/trace"
)

// mcpEventRuntimeStoreStub returns one immutable runtime for event authority tests.
type mcpEventRuntimeStoreStub struct {
	runtime *store.AppRuntime
	err     error
}

// GetAppRuntime implements the narrow MCP event runtime boundary.
func (stub mcpEventRuntimeStoreStub) GetAppRuntime(context.Context, uuid.UUID) (*store.AppRuntime, error) {
	return stub.runtime, stub.err
}

// mcpEventConfigStoreStub returns one applied desired-state document for attachment tests.
type mcpEventConfigStoreStub struct {
	state *store.ConfigState
	err   error
}

// GetConfigState implements the narrow MCP event attachment boundary.
func (stub mcpEventConfigStoreStub) GetConfigState(context.Context, string) (*store.ConfigState, error) {
	return stub.state, stub.err
}

// TestLoadMCPEventResourcesBuildsExactAuthorizedSubjects proves client input never supplies broker routing authority.
func TestLoadMCPEventResourcesBuildsExactAuthorizedSubjects(t *testing.T) {
	accountID, appID, serviceID, serviceVersionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: serviceVersionID, SchemaVersion: models.AppSelectionSchemaVersion,
		WebhookNames: []string{"payment.succeeded", "payment.failed"},
	}})
	// Fixture serialization must succeed before globals are installed.
	if err != nil {
		t.Fatal(err)
	}
	previousRuntimeStore, previousConfigStore := globalMCPEventRuntimeStore, globalMCPEventConfigStore
	globalMCPEventRuntimeStore = mcpEventRuntimeStoreStub{runtime: &store.AppRuntime{
		AccountID: accountID, AppFamilyID: appID, AppID: appID, Kind: store.AppKindMCP, ConfigKey: "mcp:payments:1.0.0",
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
	}}
	globalMCPEventConfigStore = mcpEventConfigStoreStub{state: &store.ConfigState{DesiredState: json.RawMessage(`{"webhook_attachment":"payments.events"}`)}}
	// Shared package globals are restored so unrelated session tests keep their original dependencies.
	t.Cleanup(func() {
		globalMCPEventRuntimeStore, globalMCPEventConfigStore = previousRuntimeStore, previousConfigStore
	})
	resources, err := loadMCPEventResources(context.Background(), appID.String(), auth.RuntimeIdentity{
		AccountID: accountID, AppFamilyID: appID, AppID: appID, Kind: store.AppKindMCP,
	})
	// Two explicit selected names must become two finite resources.
	if err != nil || len(resources) != 2 {
		t.Fatalf("resources=%#v err=%v", resources, err)
	}
	uri := "fused://events/" + serviceID.String() + "/payment.succeeded"
	resource, found := resources[uri]
	// The attachment label is subject-safe and the event remains an exact final subject suffix.
	if !found || resource.Subject != "webhooks."+accountID.String()+"."+serviceID.String()+".payments-events.payment.succeeded" {
		t.Fatalf("resource=%#v found=%v", resource, found)
	}
}

// TestAddMCPEventResourceCapabilityPreservesChildCapabilities verifies Engine augmentation is additive.
func TestAddMCPEventResourceCapabilityPreservesChildCapabilities(t *testing.T) {
	response, err := addMCPEventResourceCapability(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}}}}`, true)
	// Capability augmentation must retain the child tool capability and advertise standard subscriptions.
	if err != nil || !strings.Contains(response, `"subscribe":true`) || !strings.Contains(response, `"listChanged":true`) {
		t.Fatalf("response=%s err=%v", response, err)
	}
}

// TestMCPEventResourcesRequireClientInitialization preserves the MCP lifecycle before Engine-owned methods run.
func TestMCPEventResourcesRequireClientInitialization(t *testing.T) {
	sess := &mcpSession{eventResources: map[string]mcpEventResource{"fused://events/test": {URI: "fused://events/test"}}}
	response := httptest.NewRecorder()
	handled, success := handleMCPEventResourceRequest(trace.SpanFromContext(context.Background()), response, sess, mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "resources/list", Params: json.RawMessage(`{}`),
	})
	// Resource discovery cannot bypass the required notifications/initialized transition merely because Engine intercepts it.
	if !handled || success || response.Code != 400 || !strings.Contains(response.Body.String(), "notifications/initialized") {
		t.Fatalf("handled=%v success=%v response=%s", handled, success, response.Body.String())
	}
}

// TestLegacyMCPSSEEventResourcesQueueCorrelatedResults proves legacy POSTs answer on the open SSE stream.
func TestLegacyMCPSSEEventResourcesQueueCorrelatedResults(t *testing.T) {
	uri := "fused://events/service/payment.succeeded"
	sess := &mcpSession{
		clientInitialized: true,
		eventResources:    map[string]mcpEventResource{uri: {URI: uri, Title: "payment.succeeded"}},
		sseResponses:      make(chan string, 1),
	}
	response := httptest.NewRecorder()
	handled := dispatchMCPSSEEventResource(trace.SpanFromContext(context.Background()), response, sess, mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "resources/list", Params: json.RawMessage(`{}`),
	})
	// The POST acknowledges transport acceptance while the correlated JSON-RPC result waits for SSE delivery.
	if !handled || response.Code != 202 {
		t.Fatalf("handled=%v status=%d body=%s", handled, response.Code, response.Body.String())
	}
	select {
	case payload := <-sess.sseResponses:
		// The queued response preserves caller correlation and the finite selected resource.
		if !strings.Contains(payload, `"id":7`) || !strings.Contains(payload, uri) {
			t.Fatalf("payload=%s", payload)
		}
	default:
		t.Fatal("legacy resource response was not queued")
	}
}

// TestLegacyMCPSSEInitializeAdvertisesEventResources verifies capability parity with Streamable HTTP.
func TestLegacyMCPSSEInitializeAdvertisesEventResources(t *testing.T) {
	sessionID := uuid.NewString()
	sess := &mcpSession{
		sessionID: sessionID, initializeRequestID: "11",
		eventResources: map[string]mcpEventResource{"fused://events/test": {URI: "fused://events/test"}},
	}
	mcpSessions.Lock()
	mcpSessions.m[sessionID] = sess
	mcpSessions.Unlock()
	// Cleanup removes the synthetic lookup state used by capability augmentation.
	t.Cleanup(func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sessionID)
		mcpSessions.Unlock()
	})
	response := augmentMCPSSEInitializeResponse(sessionID, `{"jsonrpc":"2.0","id":11,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}}}}`)
	// Legacy initialization must advertise standard resource subscription without dropping the child tool surface.
	if !strings.Contains(response, `"resources":{"subscribe":true}`) || !strings.Contains(response, `"tools":{}`) {
		t.Fatalf("response=%s", response)
	}
}

// TestMCPEventResourceLiveNotification exercises subscribe, broker delivery, notification, and read as one local transport flow.
func TestMCPEventResourceLiveNotification(t *testing.T) {
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1})
	// A local broker is required to prove the callback path rather than only queue serialization.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	// Test setup cannot continue until the local broker accepts connections.
	if !natsServer.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	// The test must use a real NATS connection to cover exact subject delivery.
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	previousNATS := globalNATSClient
	globalNATSClient = &messaging.NATSClient{Conn: connection}
	// The process-global broker is restored after this focused live flow.
	t.Cleanup(func() { globalNATSClient = previousNATS })
	uri := "fused://events/service/payment.succeeded"
	resource := mcpEventResource{
		URI: uri, Name: "service.payment.succeeded", Title: "payment.succeeded", MIMEType: "application/json",
		Event: "service.payment.succeeded", Subject: "webhooks.account.service.payments.payment.succeeded",
	}
	sess := &mcpSession{
		sessionID: uuid.NewString(), eventResources: map[string]mcpEventResource{uri: resource},
		eventSubscriptions: map[string]*nats.Subscription{}, eventLatest: map[string]mcpEventSnapshot{},
		eventNotifications: make(chan string, maxMCPEventNotifications), clientInitialized: true,
	}
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	// Cleanup removes the registry entry before closing any remaining broker consumers.
	t.Cleanup(func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sess.sessionID)
		mcpSessions.Unlock()
		unregisterMCPEventSubscriptions(sess)
	})
	subscribeRequest := mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "resources/subscribe", Params: json.RawMessage(`{"uri":"` + uri + `"}`),
	}
	subscribeResponse := httptest.NewRecorder()
	handled, success := handleMCPEventResourceRequest(trace.SpanFromContext(context.Background()), subscribeResponse, sess, subscribeRequest)
	// A standard empty result confirms the exact broker subscription is active.
	if !handled || !success || subscribeResponse.Code != 200 {
		t.Fatalf("subscribe handled=%v success=%v response=%s", handled, success, subscribeResponse.Body.String())
	}
	message := nats.NewMsg(resource.Subject)
	message.Header.Set("X-Webhook-Msg-ID", "evt_123")
	message.Data = []byte(`{"sensitive":"not exposed by notification"}`)
	// Core NATS publication exercises the same subject seen by the webhook JetStream publisher.
	if err := connection.PublishMsg(message); err != nil {
		t.Fatalf("publish webhook event: %v", err)
	}
	if err := connection.Flush(); err != nil {
		t.Fatalf("flush webhook event: %v", err)
	}
	streamContext, cancelStream := context.WithCancel(context.Background())
	streamWriter := &streamableEventWriter{writes: make(chan string, 1)}
	streamDone := make(chan struct{})
	go func() {
		streamMCPServerEvents(streamContext, streamWriter, streamWriter, sess)
		close(streamDone)
	}()
	select {
	case payload := <-streamWriter.writes:
		// The open SSE connection carries a standard MCP notification and never includes the webhook body.
		if !strings.HasPrefix(payload, "event: message\ndata: ") || !strings.Contains(payload, `"method":"notifications/resources/updated"`) || !strings.Contains(payload, uri) || strings.Contains(payload, "sensitive") {
			t.Fatalf("notification=%s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MCP event notification on SSE")
	}
	cancelStream()
	select {
	case <-streamDone:
		// Cancellation releases only the GET listener; the session remains available for resources/read.
	case <-time.After(time.Second):
		t.Fatal("MCP event SSE listener did not stop after cancellation")
	}
	readResponse := httptest.NewRecorder()
	readRequest := mcpJSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "resources/read", Params: json.RawMessage(`{"uri":"` + uri + `"}`)}
	_, success = handleMCPEventResourceRequest(trace.SpanFromContext(context.Background()), readResponse, sess, readRequest)
	// Read exposes bounded occurrence metadata while keeping the provider payload outside this first slice.
	if !success || !strings.Contains(readResponse.Body.String(), "evt_123") || strings.Contains(readResponse.Body.String(), "sensitive") {
		t.Fatalf("read response=%s", readResponse.Body.String())
	}
}
