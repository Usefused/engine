package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// mcpModernTokenValidator returns one exact MCP identity for modern transport tests.
type mcpModernTokenValidator struct {
	token    string
	identity auth.RuntimeIdentity
}

// Validate requires the expected bearer and immutable app ID before returning the fixture identity.
func (validator *mcpModernTokenValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	// A mismatch must fail before any event resource state is loaded.
	if token != validator.token || appID != validator.identity.AppID {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	return validator.identity, nil
}

// mcpModernRuntimeStore exposes one immutable app runtime to event-resource admission.
type mcpModernRuntimeStore struct {
	runtime *store.AppRuntime
}

// GetAppRuntime returns the fixture only for its exact version identity.
func (fixture *mcpModernRuntimeStore) GetAppRuntime(_ context.Context, appID uuid.UUID) (*store.AppRuntime, error) {
	// Unknown versions must not inherit the configured event selection.
	if fixture.runtime == nil || fixture.runtime.AppID != appID {
		return nil, store.ErrAppRuntimeNotFound
	}
	return fixture.runtime, nil
}

// mcpModernConfigStore exposes the applied app document that names its webhook attachment.
type mcpModernConfigStore struct {
	key   string
	state *store.ConfigState
}

// GetConfigState returns desired state only for the exact immutable app config key.
func (fixture *mcpModernConfigStore) GetConfigState(_ context.Context, key string) (*store.ConfigState, error) {
	// Another config key cannot redirect this app to the fixture's webhook ingress label.
	if key != fixture.key {
		return nil, nil
	}
	return fixture.state, nil
}

// mcpModernFixture contains the authenticated HTTP and broker state shared by protocol tests.
type mcpModernFixture struct {
	router     http.Handler
	natsClient *messaging.NATSClient
	accountID  uuid.UUID
	serviceID  uuid.UUID
	familyID   uuid.UUID
	appID      uuid.UUID
	token      string
	uri        string
	subject    string
}

// installMCPModernFixture creates an isolated exact event scope and restores every process-global dependency.
func installMCPModernFixture(t *testing.T) *mcpModernFixture {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	// A real JetStream server is required to prove live fan-out and retained resource reads share one publication.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	// Tests cannot continue with a broker that has not accepted its local listener.
	if !natsServer.ReadyForConnections(5 * time.Second) {
		natsServer.Shutdown()
		t.Fatal("NATS server did not become ready")
	}
	connection, err := nats.Connect(natsServer.ClientURL())
	// Connection failure prevents both subscription and retention assertions.
	if err != nil {
		natsServer.Shutdown()
		t.Fatalf("connect NATS: %v", err)
	}
	jetStream, err := connection.JetStream()
	// JetStream is the authoritative retained resource store for this test.
	if err != nil {
		connection.Close()
		natsServer.Shutdown()
		t.Fatalf("create JetStream context: %v", err)
	}
	_, err = jetStream.AddStream(&nats.StreamConfig{Name: mcpEventStreamName, Subjects: []string{"webhooks.>"}})
	// The event stream must exist before the modern resource surface is admitted.
	if err != nil {
		connection.Close()
		natsServer.Shutdown()
		t.Fatalf("create webhook stream: %v", err)
	}
	accountID, serviceID, familyID, appID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	token, attachment, eventName := "modern-execution-token", "payments-events", "payment.succeeded"
	selections, err := json.Marshal([]models.SDKSelection{{
		SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: serviceID,
		ServiceVersionID: uuid.New(), WebhookNames: []string{eventName},
	}})
	// Invalid fixture encoding would make runtime-selection failures ambiguous.
	if err != nil {
		t.Fatalf("encode app selections: %v", err)
	}
	configKey := "mcp:payments-agent:1.0.0"
	runtime := &store.AppRuntime{
		AccountID: accountID, AppFamilyID: familyID, AppID: appID,
		Kind: store.AppKindMCP, Name: "payments-agent", Version: "1.0.0", ConfigKey: configKey,
		Description: "Receive payment events.", ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
	}
	identity := auth.RuntimeIdentity{
		AccountID: accountID, AppFamilyID: familyID, AppID: appID, TokenID: uuid.New(), Kind: store.AppKindMCP,
		TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}
	previousNATS, previousRuntimeStore, previousConfigStore := globalNATSClient, globalMCPEventRuntimeStore, globalMCPEventConfigStore
	previousResolver, previousValidator, previousCache := globalMCPRouteResolver, globalTokenValidator, globalObjectCache
	globalNATSClient = &messaging.NATSClient{Conn: connection, JS: jetStream}
	globalMCPEventRuntimeStore = &mcpModernRuntimeStore{runtime: runtime}
	globalMCPEventConfigStore = &mcpModernConfigStore{key: configKey, state: &store.ConfigState{DesiredState: []byte(`{"webhook_attachment":"payments-events"}`)}}
	globalMCPRouteResolver = &mcpRouteResolverStub{target: &store.MCPRouteTarget{AppFamilyID: familyID, AppID: appID, Stable: true}}
	globalTokenValidator = &mcpModernTokenValidator{token: token, identity: identity}
	globalObjectCache = &streamableSessionCache{richMockCache: &richMockCache{scopeJSON: selections}}
	router := chi.NewRouter()
	router.HandleFunc("/mcp/{id}", mcpStreamableHandler)
	uri := "fused://events/" + serviceID.String() + "/payment.succeeded"
	subject := "webhooks." + accountID.String() + "." + serviceID.String() + "." + attachment + "." + eventName
	t.Cleanup(func() {
		// Global restoration must happen before the isolated broker is stopped.
		globalNATSClient, globalMCPEventRuntimeStore, globalMCPEventConfigStore = previousNATS, previousRuntimeStore, previousConfigStore
		globalMCPRouteResolver, globalTokenValidator, globalObjectCache = previousResolver, previousValidator, previousCache
		connection.Close()
		natsServer.Shutdown()
	})
	return &mcpModernFixture{
		router: router, natsClient: globalNATSClient, accountID: accountID, serviceID: serviceID,
		familyID: familyID, appID: appID, token: token, uri: uri, subject: subject,
	}
}

// newMCPModernRequest builds the exact standard headers and request metadata used by every successful test call.
func newMCPModernRequest(t *testing.T, fixture *mcpModernFixture, id any, method string, fields map[string]any) *http.Request {
	t.Helper()
	params := map[string]any{
		"_meta": map[string]any{
			mcpProtocolVersionMetaKey:    mcpModernProtocolVersion,
			mcpClientCapabilitiesMetaKey: map[string]any{},
		},
	}
	for key, value := range fields {
		params[key] = value
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	// A malformed test request must fail at construction rather than masquerade as transport behavior.
	if err != nil {
		t.Fatalf("encode modern request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp/"+fixture.familyID.String(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(mcpProtocolVersionHeader, mcpModernProtocolVersion)
	request.Header.Set(mcpMethodHeader, method)
	// Name-bearing resource reads and tool calls mirror their exact routed parameter in a required header.
	if method == "resources/read" {
		request.Header.Set(mcpNameHeader, fields["uri"].(string))
	} else if method == "tools/call" {
		request.Header.Set(mcpNameHeader, fields["name"].(string))
	}
	return request
}

// decodeMCPModernResult extracts one successful result object from a direct JSON response.
func decodeMCPModernResult(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	// A successful status without a standard result envelope is a protocol failure.
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &envelope) != nil || envelope.Result == nil {
		t.Fatalf("modern response = status:%d body:%s", response.Code, response.Body.String())
	}
	return envelope.Result
}

// TestMCPModernDiscoveryAndResources proves the stateless surface advertises, lists, and reads only selected event resources.
func TestMCPModernDiscoveryAndResources(t *testing.T) {
	fixture := installMCPModernFixture(t)
	discovery := httptest.NewRecorder()
	fixture.router.ServeHTTP(discovery, newMCPModernRequest(t, fixture, 1, "server/discover", nil))
	discovered := decodeMCPModernResult(t, discovery)
	capabilities, _ := discovered["capabilities"].(map[string]any)
	resources, _ := capabilities["resources"].(map[string]any)
	tools, _ := capabilities["tools"].(map[string]any)
	// Discovery must advertise both the modern tool catalogue and the selected resource subscription capability without a session header.
	if discovered["resultType"] != "complete" || resources["subscribe"] != true || tools == nil || discovery.Header().Get(mcpSessionIDHeader) != "" {
		t.Fatalf("discovery result = %#v headers:%v", discovered, discovery.Header())
	}
	listedResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(listedResponse, newMCPModernRequest(t, fixture, 2, "resources/list", nil))
	listed := decodeMCPModernResult(t, listedResponse)
	items, _ := listed["resources"].([]any)
	// The immutable exact selection must project to one stable URI rather than a client-authored NATS subject.
	if len(items) != 1 || items[0].(map[string]any)["uri"] != fixture.uri {
		t.Fatalf("listed resources = %#v, want %s", items, fixture.uri)
	}
	message := nats.NewMsg(fixture.subject)
	message.Header.Set("X-Webhook-Msg-ID", "message-1")
	message.Data = []byte(`{"type":"payment.succeeded","amount":4200}`)
	_, err := fixture.natsClient.PublishMsgJS(message)
	// One retained publication must be visible to both SDK consumers and MCP resource reads.
	if err != nil {
		t.Fatalf("publish retained webhook: %v", err)
	}
	readResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(readResponse, newMCPModernRequest(t, fixture, 3, "resources/read", map[string]any{"uri": fixture.uri}))
	read := decodeMCPModernResult(t, readResponse)
	contents, _ := read["contents"].([]any)
	// The resource returns the latest retained payload with publisher identity, without acknowledging any durable SDK consumer.
	if len(contents) != 1 || !strings.Contains(contents[0].(map[string]any)["text"].(string), `"id":"message-1"`) || !strings.Contains(contents[0].(map[string]any)["text"].(string), `"amount":4200`) {
		t.Fatalf("resource contents = %#v", contents)
	}
}

// TestMCPModernToolsAndExplicitStateEndToEnd proves 2026 discovery, tool calls, and explicit cross-call state through a real child runtime.
func TestMCPModernToolsAndExplicitStateEndToEnd(t *testing.T) {
	// The shipped tool sandbox is a Node bundle, so this boundary cannot be exercised without Node.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for modern MCP tool coverage")
	}
	fixture := installMCPModernFixture(t)
	previousConfig, previousEnginePort := cfg, globalEnginePort
	localConfig := *cfg
	// Process startup under race instrumentation needs a wider bound than ordinary in-memory protocol tests.
	localConfig.Sandbox.ToolCallTimeoutSeconds = 10
	cfg, globalEnginePort = &localConfig, "1"
	t.Cleanup(func() {
		cfg, globalEnginePort = previousConfig, previousEnginePort
	})

	listedResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(listedResponse, newMCPModernRequest(t, fixture, 21, "tools/list", nil))
	listed := decodeMCPModernResult(t, listedResponse)
	tools, _ := listed["tools"].([]any)
	// The modern catalogue must expose both established tools and describe execute's explicit state input.
	if len(tools) != 2 || !strings.Contains(listedResponse.Body.String(), `"stateHandle"`) || strings.Contains(listedResponse.Body.String(), `"com.usefused/session"`) || listedResponse.Header().Get(mcpSessionIDHeader) != "" {
		t.Fatalf("modern tools/list = headers:%v body:%s", listedResponse.Header(), listedResponse.Body.String())
	}
	activeBeforeSearch := activeMCPSessionCount()
	searchResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(searchResponse, newMCPModernRequest(t, fixture, 20, "tools/call", map[string]any{
		"name": "search_docs", "arguments": map[string]any{"query": "payment"},
	}))
	// Stateless documentation search must complete without returning state or leaving a child process registered.
	if searchResponse.Code != http.StatusOK || strings.Contains(searchResponse.Body.String(), `"stateHandle"`) || activeMCPSessionCount() != activeBeforeSearch {
		t.Fatalf("modern search_docs = status:%d active:%d body:%s", searchResponse.Code, activeMCPSessionCount(), searchResponse.Body.String())
	}
	activeBeforeRejectedCall := activeMCPSessionCount()
	rejectedResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(rejectedResponse, newMCPModernRequest(t, fixture, 24, "tools/call", map[string]any{
		"name": "execute", "arguments": map[string]any{"script": 42},
	}))
	// A schema-rejected first execution returns its standard tool error without leaking a state handle or internal child.
	if rejectedResponse.Code != http.StatusOK || !strings.Contains(rejectedResponse.Body.String(), `"isError":true`) || strings.Contains(rejectedResponse.Body.String(), `"stateHandle"`) || activeMCPSessionCount() != activeBeforeRejectedCall {
		t.Fatalf("rejected modern execute = status:%d active:%d body:%s", rejectedResponse.Code, activeMCPSessionCount(), rejectedResponse.Body.String())
	}

	firstResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(firstResponse, newMCPModernRequest(t, fixture, 22, "tools/call", map[string]any{
		"name": "execute", "arguments": map[string]any{"script": `session.set("proof","modern"); return {proof:session.get("proof")};`},
	}))
	first := decodeMCPModernResult(t, firstResponse)
	structured, _ := first["structuredContent"].(map[string]any)
	handle, _ := structured[mcpModernStateHandleArgument].(string)
	// The first execution returns its ordinary value plus an explicit, non-header continuation handle.
	if !strings.HasPrefix(handle, mcpModernStateHandlePrefix) || !strings.Contains(firstResponse.Body.String(), `\"proof\":\"modern\"`) || firstResponse.Header().Get(mcpSessionIDHeader) != "" {
		t.Fatalf("first modern execute = headers:%v body:%s", firstResponse.Header(), firstResponse.Body.String())
	}
	sess := resolveMCPModernToolHandle(handle, fixture.familyID.String(), &mcpModernAdmission{target: &store.MCPRouteTarget{AppFamilyID: fixture.familyID, AppID: fixture.appID, Stable: true}, identity: globalTokenValidator.(*mcpModernTokenValidator).identity}, map[string]any{})
	// Test cleanup must release the exact child even if a later continuation assertion fails.
	if sess == nil {
		t.Fatalf("returned state handle did not resolve: %s", handle)
	}
	t.Cleanup(func() { terminateMCPSession(sess.sessionID, "test_cleanup") })

	rejectedContinuation := httptest.NewRecorder()
	fixture.router.ServeHTTP(rejectedContinuation, newMCPModernRequest(t, fixture, 25, "tools/call", map[string]any{
		"name": "execute", "arguments": map[string]any{"stateHandle": handle, "script": 42},
	}))
	// A rejected continuation returns the already-owned handle because validation failure cannot invalidate earlier state.
	if rejectedContinuation.Code != http.StatusOK || !strings.Contains(rejectedContinuation.Body.String(), `"isError":true`) || !strings.Contains(rejectedContinuation.Body.String(), handle) {
		t.Fatalf("rejected modern continuation = status:%d body:%s", rejectedContinuation.Code, rejectedContinuation.Body.String())
	}

	secondResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(secondResponse, newMCPModernRequest(t, fixture, 23, "tools/call", map[string]any{
		"name": "execute", "arguments": map[string]any{
			"stateHandle": handle, "script": `return {proof:session.get("proof")};`,
		},
	}))
	second := decodeMCPModernResult(t, secondResponse)
	secondStructured, _ := second["structuredContent"].(map[string]any)
	// Reusing the returned handle must reach the same sandbox state without allocating an HTTP protocol session.
	if secondStructured[mcpModernStateHandleArgument] != handle || !strings.Contains(secondResponse.Body.String(), `\"proof\":\"modern\"`) || !strings.Contains(secondResponse.Body.String(), mcpModernStateMetadataKey) || secondResponse.Header().Get(mcpSessionIDHeader) != "" {
		t.Fatalf("continued modern execute = headers:%v body:%s", secondResponse.Header(), secondResponse.Body.String())
	}
	mismatchedAdmission := &mcpModernAdmission{
		target:   &store.MCPRouteTarget{AppFamilyID: fixture.familyID, AppID: fixture.appID, Stable: true},
		identity: globalTokenValidator.(*mcpModernTokenValidator).identity,
	}
	mismatchedAdmission.identity.TokenID = uuid.New()
	// A newly issued token cannot inherit sandbox state minted for another execution-token identity.
	if resolved := resolveMCPModernToolHandle(handle, fixture.familyID.String(), mismatchedAdmission, map[string]any{}); resolved != nil {
		t.Fatalf("state handle crossed token identity: %#v", resolved)
	}
	// Shared session termination must atomically remove the public handle before process cleanup finishes.
	if !terminateMCPSession(sess.sessionID, "test_expired") {
		t.Fatal("terminate explicit state session returned false")
	}
	if resolved := resolveMCPModernToolHandle(handle, fixture.familyID.String(), &mcpModernAdmission{target: &store.MCPRouteTarget{AppFamilyID: fixture.familyID, AppID: fixture.appID, Stable: true}, identity: globalTokenValidator.(*mcpModernTokenValidator).identity}, map[string]any{}); resolved != nil {
		t.Fatalf("terminated state handle still resolved: %#v", resolved)
	}
}

// TestMCPModernExplicitStateResultRewritesContinuation verifies metadata preservation and ready-to-send retained-result recovery.
func TestMCPModernExplicitStateResultRewritesContinuation(t *testing.T) {
	handle := mcpModernStateHandlePrefix + uuid.NewString()
	result := map[string]any{
		"_meta": map[string]any{"runtime": "preserved"},
		"content": []any{map[string]any{
			"type": "text",
			"text": `{"next_request":{"tool":"execute","arguments":{"script":"return session.page(\"ref\", 1);"}}}`,
		}},
	}
	attachMCPModernStateHandle(result, handle)
	metadata, _ := result["_meta"].(map[string]any)
	stateMetadata, _ := metadata[mcpModernStateMetadataKey].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	// Existing runtime metadata and the explicit structured handle must coexist in the public tool result.
	if metadata["runtime"] != "preserved" || stateMetadata["handle"] != handle || structured[mcpModernStateHandleArgument] != handle {
		t.Fatalf("explicit state result = %#v", result)
	}
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	var payload map[string]any
	// The rewritten text must remain valid JSON before its continuation can be inspected.
	if err := json.Unmarshal([]byte(block["text"].(string)), &payload); err != nil {
		t.Fatalf("decode rewritten continuation: %v", err)
	}
	next := payload["next_request"].(map[string]any)
	arguments := next["arguments"].(map[string]any)
	// A client can send next_request directly because the owning state handle is now an ordinary argument.
	if arguments[mcpModernStateHandleArgument] != handle || payload["session"].(map[string]any)["scope"] != "explicit_state_handle" {
		t.Fatalf("rewritten continuation = %#v", payload)
	}
}

// TestMCPModernDisposableRuntimeSuppressesLifecycle proves implementation-only children do not appear as client sessions.
func TestMCPModernDisposableRuntimeSuppressesLifecycle(t *testing.T) {
	fixture := installMCPModernFixture(t)
	subscription, err := fixture.natsClient.Conn.SubscribeSync(messaging.FusedEngineSessionSubject(fixture.appID.String()))
	// The observer must be active before the suppression boundary is exercised.
	if err != nil {
		t.Fatalf("subscribe to lifecycle subject: %v", err)
	}
	defer subscription.Unsubscribe()
	// Flush establishes subscription ordering so a timeout proves suppression rather than setup delay.
	if err := fixture.natsClient.Conn.Flush(); err != nil {
		t.Fatalf("flush lifecycle subscription: %v", err)
	}
	sess := &mcpSession{
		appID: fixture.appID.String(), sessionID: uuid.NewString(), tokenID: uuid.New(),
		protocolVersion: mcpModernProtocolVersion, suppressLifecycle: true,
	}
	publishMCPSessionEvent(sess, "started", "")
	_, err = subscription.NextMsg(100 * time.Millisecond)
	// A disposable tools/list or search_docs child is not a protocol session and must publish nothing.
	if !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("suppressed lifecycle publication error = %v", err)
	}
	sess.suppressLifecycle = false
	publishMCPSessionEvent(sess, "started", "")
	message, err := subscription.NextMsg(time.Second)
	// Stateful execute handles still need normal lifecycle attribution for audit and cleanup visibility.
	if err != nil || !bytes.Contains(message.Data, []byte(`"protocol_version":"2026-07-28"`)) {
		t.Fatalf("stateful lifecycle message = %#v, error %v", message, err)
	}
}

// TestMCPModernSubscriptionsListen verifies acknowledgement ordering, URI filtering, and tagged live delivery over one POST SSE response.
func TestMCPModernSubscriptionsListen(t *testing.T) {
	fixture := installMCPModernFixture(t)
	server := httptest.NewServer(fixture.router)
	defer server.Close()
	request := newMCPModernRequest(t, fixture, "listen-1", "subscriptions/listen", map[string]any{
		"notifications": map[string]any{
			"toolsListChanged":      true,
			"resourceSubscriptions": []string{"fused://events/unknown", fixture.uri, fixture.uri},
		},
	})
	request.URL.Scheme, request.URL.Host = "http", strings.TrimPrefix(server.URL, "http://")
	request.RequestURI = ""
	response, err := server.Client().Do(request)
	// The HTTP client must receive the flushed acknowledgement without waiting for stream completion.
	if err != nil {
		t.Fatalf("open subscriptions/listen: %v", err)
	}
	defer response.Body.Close()
	// Modern listen responses are SSE and never allocate a protocol session header.
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") || response.Header.Get(mcpSessionIDHeader) != "" {
		t.Fatalf("listen response = status:%d headers:%v", response.StatusCode, response.Header)
	}
	reader := bufio.NewReader(response.Body)
	ack := readMCPModernSSEData(t, reader)
	// The first frame must acknowledge only the exact supported URI and omit unsupported notification classes.
	if !strings.Contains(ack, `"method":"notifications/subscriptions/acknowledged"`) || !strings.Contains(ack, `"resourceSubscriptions":["`+fixture.uri+`"]`) || strings.Contains(ack, "toolsListChanged") || !strings.Contains(ack, `"io.modelcontextprotocol/subscriptionId":"listen-1"`) {
		t.Fatalf("acknowledgement = %s", ack)
	}
	message := nats.NewMsg(fixture.subject)
	message.Header.Set("X-Webhook-Msg-ID", "message-live")
	message.Data = []byte(`{"type":"payment.succeeded"}`)
	_, err = fixture.natsClient.PublishMsgJS(message)
	// Publication failure would invalidate the live delivery assertion.
	if err != nil {
		t.Fatalf("publish live webhook: %v", err)
	}
	updated := readMCPModernSSEData(t, reader)
	// Every live update is correlated to the listen request and carries only the authorized resource URI.
	if !strings.Contains(updated, `"method":"notifications/resources/updated"`) || !strings.Contains(updated, `"uri":"`+fixture.uri+`"`) || !strings.Contains(updated, `"io.modelcontextprotocol/subscriptionId":"listen-1"`) {
		t.Fatalf("resource update = %s", updated)
	}
}

// TestMCPModernDisconnectedListenerDoesNotReplayButRetains proves transport loss and durable webhook storage remain independent.
func TestMCPModernDisconnectedListenerDoesNotReplayButRetains(t *testing.T) {
	fixture := installMCPModernFixture(t)
	resources, err := loadMCPEventResources(context.Background(), fixture.appID.String(), globalTokenValidator.(*mcpModernTokenValidator).identity)
	// The fixture must resolve the exact URI before listener lifecycle assertions begin.
	if err != nil {
		t.Fatalf("load event resources: %v", err)
	}
	firstEvents, firstOverflow := make(chan mcpModernEvent, 1), make(chan struct{})
	var firstOnce sync.Once
	firstSubscriptions, err := subscribeMCPModernResources([]string{fixture.uri}, resources, firstEvents, firstOverflow, &firstOnce)
	// The first listener must be live before simulating its disconnect.
	if err != nil {
		t.Fatalf("start first event listener: %v", err)
	}
	unsubscribeMCPModernResources(firstSubscriptions)
	message := nats.NewMsg(fixture.subject)
	message.Header.Set("X-Webhook-Msg-ID", "message-after-disconnect")
	message.Data = []byte(`{"type":"payment.succeeded","after_disconnect":true}`)
	_, err = fixture.natsClient.PublishMsgJS(message)
	// JetStream must retain the event even though no MCP listener currently owns a core subscription.
	if err != nil {
		t.Fatalf("publish disconnected webhook: %v", err)
	}
	secondEvents, secondOverflow := make(chan mcpModernEvent, 1), make(chan struct{})
	var secondOnce sync.Once
	secondSubscriptions, err := subscribeMCPModernResources([]string{fixture.uri}, resources, secondEvents, secondOverflow, &secondOnce)
	// Re-listening establishes future delivery only and must not create a retained-message replay consumer.
	if err != nil {
		t.Fatalf("start second event listener: %v", err)
	}
	defer unsubscribeMCPModernResources(secondSubscriptions)
	select {
	case event := <-secondEvents:
		t.Fatalf("new listener replayed disconnected event for %s", event.resource.URI)
	case <-time.After(100 * time.Millisecond):
		// Absence is expected because modern MCP reconnect recovery is re-listen plus resources/read, not transport replay.
	}
	snapshot, err := readMCPEventSnapshot(resources[fixture.uri])
	// The same event must remain recoverable as current resource state and available to an independent durable SDK consumer.
	if err != nil || snapshot == nil || snapshot.ID != "message-after-disconnect" || !bytes.Contains(snapshot.Payload, []byte(`"after_disconnect":true`)) {
		t.Fatalf("retained snapshot = %#v, error %v", snapshot, err)
	}
}

// readMCPModernSSEData reads one complete message event and returns its JSON data payload.
func readMCPModernSSEData(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		// A closed stream before its next data frame is an unexpected protocol loss in these tests.
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		// Only data lines carry JSON-RPC messages; event and blank framing lines are skipped.
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
}

// TestMCPModernRejectsInvalidRoutingAndMethods locks strict negotiation and routing agreement into the modern path.
func TestMCPModernRejectsInvalidRoutingAndMethods(t *testing.T) {
	fixture := installMCPModernFixture(t)
	request := newMCPModernRequest(t, fixture, 8, "resources/list", nil)
	request.Header.Set(mcpMethodHeader, "tools/list")
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// Gateway method disagreement must fail before application resource discovery.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":-32020`) {
		t.Fatalf("method mismatch = status:%d body:%s", response.Code, response.Body.String())
	}
	request = newMCPModernRequest(t, fixture, 9, "resources/list", nil)
	request.Header.Set(mcpProtocolVersionHeader, "2027-01-01")
	var body map[string]any
	// Re-encoding keeps the test request structurally valid while selecting an unsupported revision on both surfaces.
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatalf("decode modern request: %v", err)
	}
	params := body["params"].(map[string]any)
	params["_meta"].(map[string]any)[mcpProtocolVersionMetaKey] = "2027-01-01"
	encoded, err := json.Marshal(body)
	// Negotiation behavior cannot be asserted with a malformed replacement body.
	if err != nil {
		t.Fatalf("encode unsupported-version request: %v", err)
	}
	request.Body = io.NopCloser(bytes.NewReader(encoded))
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// Unsupported modern revisions return the advertised revision set rather than entering another protocol path.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":-32022`) || !strings.Contains(response.Body.String(), mcpModernProtocolVersion) {
		t.Fatalf("unsupported version = status:%d body:%s", response.Code, response.Body.String())
	}
	request = newMCPModernRequest(t, fixture, 10, "resources/subscribe", map[string]any{"uri": fixture.uri})
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// The removed resource subscription RPC must not silently enter another protocol path on a modern request.
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":-32601`) {
		t.Fatalf("removed method rejection = status:%d body:%s", response.Code, response.Body.String())
	}
	request = newMCPModernRequest(t, fixture, 11, "subscriptions/listen", nil)
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// An opt-in stream without a notifications object cannot be acknowledged as an implicit wildcard or empty listener.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":-32602`) {
		t.Fatalf("missing filter rejection = status:%d body:%s", response.Code, response.Body.String())
	}
	request = newMCPModernRequest(t, fixture, 12, "tools/call", map[string]any{"name": "execute", "arguments": map[string]any{"script": "return {};"}})
	request.Header.Set(mcpNameHeader, "search_docs")
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// Tool routing disagreement must fail before a model-selected name can allocate an execution child.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":-32020`) {
		t.Fatalf("tool name mismatch = status:%d body:%s", response.Code, response.Body.String())
	}
	activeBefore := activeMCPSessionCount()
	request = newMCPModernRequest(t, fixture, 13, "tools/call", map[string]any{
		"name": "execute", "arguments": map[string]any{"stateHandle": mcpModernStateHandlePrefix + uuid.NewString(), "script": "return {};"},
	})
	response = httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	// Unknown explicit state fails closed with actionable recovery and cannot allocate replacement state implicitly.
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"MCP_STATE_HANDLE_UNAVAILABLE"`) || activeMCPSessionCount() != activeBefore {
		t.Fatalf("unknown state handle = status:%d active:%d body:%s", response.Code, activeMCPSessionCount(), response.Body.String())
	}
}
