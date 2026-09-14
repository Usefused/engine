package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/trace"
)

const maxMCPEventNotifications = 32

// mcpEventResource binds one standard MCP resource URI to an Engine-authorized broker subject.
type mcpEventResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
	Event       string `json:"-"`
	Subject     string `json:"-"`
}

// mcpEventSnapshot is the credential-free occurrence metadata exposed by resources/read.
type mcpEventSnapshot struct {
	ID         string    `json:"id"`
	Event      string    `json:"event"`
	ReceivedAt time.Time `json:"received_at"`
}

// loadMCPEventResources derives exact resource and NATS identities from immutable app selections.
func loadMCPEventResources(ctx context.Context, appID string, identity auth.RuntimeIdentity) (map[string]mcpEventResource, error) {
	resources := map[string]mcpEventResource{}
	// Tests and deployments without event persistence retain the existing operation-only MCP behavior.
	if globalMCPEventRuntimeStore == nil || globalMCPEventConfigStore == nil {
		return resources, nil
	}
	parsedAppID, err := uuid.Parse(appID)
	// Session admission has already parsed the route, but event authority still rejects an invalid exact identity.
	if err != nil {
		return nil, errors.New("invalid MCP event app identity")
	}
	runtime, err := globalMCPEventRuntimeStore.GetAppRuntime(ctx, parsedAppID)
	// Missing or unreadable immutable scope cannot become event subscription authority.
	if err != nil {
		return nil, fmt.Errorf("load MCP event scope: %w", err)
	}
	// The authorized token and loaded runtime must describe the same MCP version and tenant.
	if runtime.AppID != identity.AppID || runtime.AccountID != identity.AccountID || runtime.AppFamilyID != identity.AppFamilyID || runtime.Kind != store.AppKindMCP || identity.Kind != store.AppKindMCP {
		return nil, errors.New("MCP event scope does not match the authorized runtime")
	}
	selections, err := models.DecodeAppSelections(runtime.ScopeSchemaVersion, runtime.Selections)
	// Invalid persisted selection data must fail closed before attachment state is consulted.
	if err != nil {
		return nil, fmt.Errorf("decode MCP event scope: %w", err)
	}
	hasEvents := false
	for _, selection := range selections {
		// The initial MCP resource surface deliberately requires a finite set of event URIs.
		if selection.WebhookSelectAll {
			return nil, errors.New("MCP event scope cannot use webhook_select_all")
		}
		// Attachment state is unnecessary for operation-only MCP versions.
		if len(selection.WebhookNames) > 0 {
			hasEvents = true
		}
	}
	// Existing MCP versions with no event selection keep their current initialization path.
	if !hasEvents {
		return resources, nil
	}
	// An applied app config key is the only server-owned route to its attachment label.
	if strings.TrimSpace(runtime.ConfigKey) == "" {
		return nil, errors.New("MCP event scope has no applied config identity")
	}
	state, err := globalMCPEventConfigStore.GetConfigState(ctx, runtime.ConfigKey)
	// Store failures cannot be interpreted as an absent event attachment.
	if err != nil {
		return nil, fmt.Errorf("load MCP event attachment: %w", err)
	}
	// Selected events without their exact applied desired state would otherwise subscribe under guessed authority.
	if state == nil {
		return nil, errors.New("MCP event attachment state is unavailable")
	}
	var document struct {
		WebhookAttachment string `json:"webhook_attachment"`
	}
	// Malformed desired state must not broaden or redirect a subscription.
	if err := json.Unmarshal(state.DesiredState, &document); err != nil {
		return nil, errors.New("MCP event attachment state is invalid")
	}
	attachment := strings.TrimSpace(document.WebhookAttachment)
	// Plan and apply require this field, while runtime repeats the invariant before touching NATS.
	if attachment == "" {
		return nil, errors.New("MCP event scope has no webhook_attachment")
	}
	safeAttachment := subjectSafeLabel(attachment)
	// Attachment labels occupy one exact subject segment and therefore cannot contain NATS wildcard or whitespace syntax.
	if strings.ContainsAny(safeAttachment, "*> \t\r\n") {
		return nil, errors.New("MCP webhook_attachment cannot be represented as an exact subscription")
	}
	for _, selection := range selections {
		for _, eventName := range selection.WebhookNames {
			eventName = strings.TrimSpace(eventName)
			// NATS wildcard characters in authored names could turn one selection into a broader subscription.
			if eventName == "" || strings.ContainsAny(eventName, "*> \t\r\n") {
				return nil, fmt.Errorf("MCP event name %q cannot be represented as an exact subscription", eventName)
			}
			uri := fmt.Sprintf("fused://events/%s/%s", selection.ServiceID, url.PathEscape(eventName))
			qualifiedEvent := selection.ServiceID.String() + "." + eventName
			resources[uri] = mcpEventResource{
				URI: uri, Name: qualifiedEvent, Title: eventName,
				Description: "Latest occurrence metadata for " + eventName,
				MIMEType:    "application/json", Event: qualifiedEvent,
				Subject: "webhooks." + identity.AccountID.String() + "." + selection.ServiceID.String() + "." + safeAttachment + "." + eventName,
			}
		}
	}
	return resources, nil
}

// handleMCPEventResourceRequest serves Engine-owned event resources and reports whether their protocol result succeeded.
func handleMCPEventResourceRequest(span trace.Span, w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest) (bool, bool) {
	// Operation-only sessions leave resource methods to the child runtime's normal method-not-found behavior.
	if sess == nil || len(sess.eventResources) == 0 {
		return false, false
	}
	// Unrelated protocol methods remain child-owned even while this session exposes event resources.
	if !isMCPEventResourceMethod(request.Method) {
		return false, false
	}
	// MCP clients must complete their initialized notification before using any advertised server capability.
	if !mcpClientInitializationComplete(sess) {
		writeMCPJSONRPCError(w, request.ID, -32600, "notifications/initialized is required before event resource methods", http.StatusBadRequest)
		recordMCPTransportOutcome(span, "invalid", true)
		return true, false
	}
	success := true
	// Only the finite MCP resource method set is intercepted; tools and other protocol methods remain child-owned.
	switch request.Method {
	case "resources/list":
		// Discovery is still a correlated request and cannot be sent as a notification.
		if !validMCPEventRequestID(request.ID) {
			writeMCPJSONRPCError(w, request.ID, -32600, "event resource method requires a request id", http.StatusBadRequest)
			success = false
		} else {
			writeMCPJSONRPCResult(w, request.ID, map[string]any{"resources": sortedMCPEventResources(sess.eventResources)})
		}
	case "resources/templates/list":
		// The initial finite namespace has no templates, but its empty result remains request-correlated.
		if !validMCPEventRequestID(request.ID) {
			writeMCPJSONRPCError(w, request.ID, -32600, "event resource method requires a request id", http.StatusBadRequest)
			success = false
		} else {
			writeMCPJSONRPCResult(w, request.ID, map[string]any{"resourceTemplates": []any{}})
		}
	case "resources/read":
		success = readMCPEventResource(w, sess, request)
	case "resources/subscribe":
		success = subscribeMCPEventResource(w, sess, request)
	case "resources/unsubscribe":
		success = unsubscribeMCPEventResource(w, sess, request)
	default:
		return false, false
	}
	outcome := "invalid"
	// Successful Engine-owned results share the ordinary MCP transport success outcome.
	if success {
		outcome = "success"
	}
	recordMCPTransportOutcome(span, outcome, !success)
	return true, success
}

// isMCPEventResourceMethod identifies the finite Engine-owned MCP resource surface.
func isMCPEventResourceMethod(method string) bool {
	switch method {
	case "resources/list", "resources/templates/list", "resources/read", "resources/subscribe", "resources/unsubscribe":
		return true
	default:
		return false
	}
}

// validMCPEventRequestID rejects notifications for methods that require a standard result envelope.
func validMCPEventRequestID(id json.RawMessage) bool {
	return len(id) > 0 && string(compactJSON(id)) != "null"
}

// mcpEventResourceURI extracts one bounded URI parameter shared by read and subscription requests.
func mcpEventResourceURI(request mcpJSONRPCRequest) (string, error) {
	// Resource methods are requests because their result must remain correlated to one caller action.
	if !validMCPEventRequestID(request.ID) {
		return "", errors.New("event resource method requires a request id")
	}
	var params struct {
		URI string `json:"uri"`
	}
	// Exact URI admission prevents extra shapes from being treated as broker filters.
	if json.Unmarshal(request.Params, &params) != nil || strings.TrimSpace(params.URI) == "" || len(params.URI) > 2048 {
		return "", errors.New("event resource method requires a valid uri")
	}
	return params.URI, nil
}

// readMCPEventResource returns only occurrence metadata; webhook payload delivery remains outside the basic notification slice.
func readMCPEventResource(w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest) bool {
	uri, err := mcpEventResourceURI(request)
	// Invalid parameters retain the standard JSON-RPC invalid-params code.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		return false
	}
	resource, allowed := sess.eventResources[uri]
	// A client cannot read a resource outside the immutable app selection.
	if !allowed {
		writeMCPJSONRPCError(w, request.ID, -32602, "event resource is not selected by this MCP app", http.StatusBadRequest)
		return false
	}
	sess.eventMu.Lock()
	latest, found := sess.eventLatest[uri]
	sess.eventMu.Unlock()
	body := map[string]any{"event": resource.Event, "latest": nil}
	// The first delivery replaces the explicit null marker with bounded occurrence metadata.
	if found {
		body["latest"] = latest
	}
	encoded, err := json.Marshal(body)
	// Fixed-shape metadata should serialize, but transport must still fail closed if it cannot.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32603, "event resource could not be encoded", http.StatusInternalServerError)
		return false
	}
	writeMCPJSONRPCResult(w, request.ID, map[string]any{"contents": []map[string]any{{
		"uri": uri, "mimeType": resource.MIMEType, "text": string(encoded),
	}}})
	return true
}

// subscribeMCPEventResource creates one exact live NATS subscription for this MCP transport session.
func subscribeMCPEventResource(w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest) bool {
	uri, err := mcpEventResourceURI(request)
	// Invalid parameters never reach the broker.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		return false
	}
	resource, allowed := sess.eventResources[uri]
	// URI membership, not client-authored subject text, is the subscription authorization boundary.
	if !allowed {
		writeMCPJSONRPCError(w, request.ID, -32602, "event resource is not selected by this MCP app", http.StatusBadRequest)
		return false
	}
	// A missing broker means this Engine cannot offer the advertised live delivery path.
	if globalNATSClient == nil || globalNATSClient.Conn == nil {
		writeMCPJSONRPCError(w, request.ID, -32603, "event transport is unavailable", http.StatusServiceUnavailable)
		return false
	}
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Subscription creation cannot race a session that has already been claimed for termination.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		writeMCPJSONRPCError(w, request.ID, -32600, errMCPSessionUnavailable.Error(), http.StatusNotFound)
		return false
	}
	sess.eventMu.Lock()
	// Repeated subscribe calls are idempotent for one session and URI.
	if _, exists := sess.eventSubscriptions[uri]; exists {
		sess.eventMu.Unlock()
		writeMCPJSONRPCResult(w, request.ID, map[string]any{})
		return true
	}
	// The callback translates broker delivery into occurrence metadata and a protocol notification for this exact session.
	subscription, err := globalNATSClient.Subscribe(resource.Subject, func(message *nats.Msg) {
		recordMCPEventOccurrence(sess, resource, message)
	})
	// Broker admission must complete before the client receives subscription success.
	if err == nil {
		err = globalNATSClient.Conn.Flush()
	}
	// A failed subscription owns no map entry and returns a bounded runtime error.
	if err != nil {
		sess.eventMu.Unlock()
		writeMCPJSONRPCError(w, request.ID, -32603, "event subscription could not be started", http.StatusServiceUnavailable)
		return false
	}
	sess.eventSubscriptions[uri] = subscription
	sess.eventMu.Unlock()
	writeMCPJSONRPCResult(w, request.ID, map[string]any{})
	return true
}

// unsubscribeMCPEventResource removes only this session's exact live subscription.
func unsubscribeMCPEventResource(w http.ResponseWriter, sess *mcpSession, request mcpJSONRPCRequest) bool {
	uri, err := mcpEventResourceURI(request)
	// Invalid parameters cannot alter session subscription state.
	if err != nil {
		writeMCPJSONRPCError(w, request.ID, -32602, err.Error(), http.StatusBadRequest)
		return false
	}
	// Unknown URIs are outside this app even when no subscription currently exists.
	if _, allowed := sess.eventResources[uri]; !allowed {
		writeMCPJSONRPCError(w, request.ID, -32602, "event resource is not selected by this MCP app", http.StatusBadRequest)
		return false
	}
	sess.eventMu.Lock()
	subscription := sess.eventSubscriptions[uri]
	delete(sess.eventSubscriptions, uri)
	sess.eventMu.Unlock()
	// Repeated unsubscribe calls are idempotent while a live handle is closed exactly once.
	if subscription != nil {
		_ = subscription.Unsubscribe()
	}
	writeMCPJSONRPCResult(w, request.ID, map[string]any{})
	return true
}

// recordMCPEventOccurrence updates the readable resource and queues one standard MCP change notification.
func recordMCPEventOccurrence(sess *mcpSession, resource mcpEventResource, message *nats.Msg) {
	// A callback that races cleanup cannot revive or notify a retired session.
	if _, active := lookupMCPSession(sess.sessionID); !active {
		return
	}
	messageID := strings.TrimSpace(message.Header.Get("X-Webhook-Msg-ID"))
	// Older or synthetic publishers without an ID still receive one opaque occurrence identity.
	if messageID == "" {
		messageID = uuid.NewString()
	}
	snapshot := mcpEventSnapshot{ID: messageID, Event: resource.Event, ReceivedAt: time.Now().UTC()}
	sess.eventMu.Lock()
	sess.eventLatest[resource.URI] = snapshot
	sess.eventMu.Unlock()
	enqueueMCPEventNotification(sess, resource.URI)
}

// enqueueMCPEventNotification sends a bounded resources/updated signal without blocking webhook ingestion.
func enqueueMCPEventNotification(sess *mcpSession, uri string) bool {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": map[string]string{"uri": uri},
	})
	// Fixed-shape serialization failure cannot mutate queue state.
	if err != nil {
		return false
	}
	sess.lifecycleMu.Lock()
	defer sess.lifecycleMu.Unlock()
	// Only the exact active session can receive an event notification.
	if !mcpSessionRegisteredAndActiveLocked(sess) {
		return false
	}
	// A bounded non-blocking queue makes the basic transport explicitly live and best-effort.
	select {
	case sess.eventNotifications <- string(payload):
		return true
	default:
		return false
	}
}

// unregisterMCPEventSubscriptions closes all broker consumers owned by one terminating MCP session.
func unregisterMCPEventSubscriptions(sess *mcpSession) {
	// Sessions without event resources have no broker ownership to release.
	if sess == nil {
		return
	}
	sess.eventMu.Lock()
	subscriptions := make([]*nats.Subscription, 0, len(sess.eventSubscriptions))
	for _, subscription := range sess.eventSubscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	sess.eventSubscriptions = map[string]*nats.Subscription{}
	sess.eventMu.Unlock()
	for _, subscription := range subscriptions {
		// Cleanup is best-effort because the parent NATS connection may already be closing.
		if subscription != nil {
			_ = subscription.Unsubscribe()
		}
	}
}

// writeMCPJSONRPCResult writes one correlated standard result envelope.
func writeMCPJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// sortedMCPEventResources stabilizes resource discovery independently of map iteration order.
func sortedMCPEventResources(resources map[string]mcpEventResource) []mcpEventResource {
	result := make([]mcpEventResource, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource)
	}
	// Stable URI order makes client caches and tests deterministic.
	sort.Slice(result, func(left, right int) bool { return result[left].URI < result[right].URI })
	return result
}

// addMCPEventResourceCapability augments a successful child initialize result with Engine-owned resource support.
func addMCPEventResourceCapability(response string, enabled bool) (string, error) {
	// Operation-only MCP apps retain the child runtime response byte-for-byte.
	if !enabled {
		return response, nil
	}
	var envelope map[string]any
	// Only a successful object result can advertise Engine-owned capabilities.
	if json.Unmarshal([]byte(response), &envelope) != nil {
		return "", errors.New("invalid MCP initialize response")
	}
	result, ok := envelope["result"].(map[string]any)
	// Error responses and scalar results cannot be converted into successful initialization.
	if !ok {
		return "", errors.New("invalid MCP initialize result")
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	// A missing capability object is initialized without disturbing sibling result fields.
	if !ok {
		capabilities = map[string]any{}
		result["capabilities"] = capabilities
	}
	resources, ok := capabilities["resources"].(map[string]any)
	// The Engine-owned resource capability replaces no valid child resource object.
	if !ok {
		resources = map[string]any{}
		capabilities["resources"] = resources
	}
	resources["subscribe"] = true
	encoded, err := json.Marshal(envelope)
	// Serialization failure prevents publication of a partially mutated initialize response.
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
