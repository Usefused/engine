package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	mcpModernProtocolVersion       = "2026-07-28"
	mcpMethodHeader                = "Mcp-Method"
	mcpNameHeader                  = "Mcp-Name"
	mcpSubscriptionIDMetaKey       = "io.modelcontextprotocol/subscriptionId"
	mcpProtocolVersionMetaKey      = "io.modelcontextprotocol/protocolVersion"
	mcpClientCapabilitiesMetaKey   = "io.modelcontextprotocol/clientCapabilities"
	mcpServerInfoMetaKey           = "io.modelcontextprotocol/serverInfo"
	mcpModernHeaderMismatchCode    = -32020
	mcpUnsupportedProtocolCode     = -32022
	mcpModernResourceCacheTTL      = 60 * 1000
	mcpModernKeepaliveInterval     = 15 * time.Second
	maxMCPModernResourceURIBytes   = 2048
	maxMCPModernSubscriptionEvents = 32
)

// mcpModernRequestMeta captures only the required standard metadata while leaving capability contents open.
type mcpModernRequestMeta struct {
	ProtocolVersion    string          `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

// mcpModernBaseParams decodes the metadata required on every modern request.
type mcpModernBaseParams struct {
	Meta mcpModernRequestMeta `json:"_meta"`
}

// mcpModernListenParams decodes one opt-in notification filter without accepting implicit notification classes.
type mcpModernListenParams struct {
	Notifications *struct {
		ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
		PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
		ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
		ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
	} `json:"notifications"`
	Meta mcpModernRequestMeta `json:"_meta"`
}

// mcpModernAdmission retains the exact authenticated app version and its public server identity for one stateless request.
type mcpModernAdmission struct {
	target    *store.MCPRouteTarget
	identity  auth.RuntimeIdentity
	server    FixtureServerMetadata
	resources map[string]mcpEventResource
}

// mcpModernEvent carries only the authorized resource identity needed to construct a change notification.
type mcpModernEvent struct {
	resource mcpEventResource
}

// isMCPModernRequest detects the stateless protocol before session-oriented admission can require initialize.
func isMCPModernRequest(r *http.Request, request mcpJSONRPCRequest) bool {
	// Standard modern methods are unambiguous even when a malformed client omitted its version header.
	if request.Method == "server/discover" || request.Method == "subscriptions/listen" {
		return true
	}
	// A modern protocol header routes malformed body metadata to the modern HeaderMismatch response.
	if strings.TrimSpace(r.Header.Get(mcpProtocolVersionHeader)) == mcpModernProtocolVersion {
		return true
	}
	var params mcpModernBaseParams
	// Any per-request protocol version selects modern validation so unsupported revisions receive the standard negotiation error.
	return json.Unmarshal(request.Params, &params) == nil && strings.TrimSpace(params.Meta.ProtocolVersion) != ""
}

// handleMCPModernPost serves one stateless 2026 request without allocating or accepting a protocol session ID.
func handleMCPModernPost(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, routeID, token string, request mcpJSONRPCRequest) {
	// Header and body agreement is validated before route lookup so malformed traffic cannot enumerate app state.
	if !admitMCPModernRequest(w, r, request) {
		recordMCPTransportOutcome(span, "invalid", true)
		return
	}
	admission, status, err := admitMCPModernApp(ctx, routeID, token)
	// Authentication and immutable metadata failures use bounded protocol errors without exposing lifecycle state.
	if err != nil {
		writeMCPModernError(w, request.ID, -32600, err.Error(), status, nil)
		recordMCPTransportOutcome(span, "denied", true)
		return
	}
	span.SetAttributes(
		attribute.String("app.id", admission.target.AppID.String()),
		attribute.String("app.family_id", admission.target.AppFamilyID.String()),
		attribute.Bool("mcp.route.stable", admission.target.Stable),
		attribute.String("mcp.protocol.version", mcpModernProtocolVersion),
	)
	// Every accepted modern request consumes the existing per-app message admission budget.
	if !allowMCPModernMessage(w, request.ID, admission.target.AppID.String()) {
		recordMCPTransportOutcome(span, "rate_limited", true)
		return
	}
	// The modern surface advertises only the resource methods implemented by this migration slice.
	switch request.Method {
	case "server/discover":
		handleMCPModernDiscover(w, request, admission)
	case "ping":
		writeMCPModernResult(w, request.ID, map[string]any{}, admission.server)
	case "resources/list":
		handleMCPModernResourcesList(w, request, admission)
	case "resources/templates/list":
		handleMCPModernResourceTemplatesList(w, request, admission)
	case "resources/read":
		handleMCPModernResourceRead(w, request, admission)
	case "subscriptions/listen":
		handleMCPModernSubscriptionsListen(ctx, w, token, request, admission)
	default:
		// Removed and unadvertised methods fail explicitly instead of falling through to the sessionful child runtime.
		writeMCPModernError(w, request.ID, -32601, "method is not available for protocol version "+mcpModernProtocolVersion, http.StatusNotFound, nil)
	}
	recordMCPTransportOutcome(span, "success", false)
}

// admitMCPModernRequest validates the 2026 metadata envelope and its required HTTP routing headers.
func admitMCPModernRequest(w http.ResponseWriter, r *http.Request, request mcpJSONRPCRequest) bool {
	// Modern requests are correlated; this slice implements no client-to-server notification method.
	if len(request.ID) == 0 || string(compactJSON(request.ID)) == "null" {
		writeMCPModernError(w, request.ID, -32600, "modern MCP requests require a non-null id", http.StatusBadRequest, nil)
		return false
	}
	// The transport requires JSON request content rather than form or text coercion.
	if !mcpModernMediaTypeMatches(r.Header.Get("Content-Type"), "application/json") {
		writeMCPModernError(w, request.ID, mcpModernHeaderMismatchCode, "Content-Type must be application/json", http.StatusBadRequest, nil)
		return false
	}
	// Every modern caller must be able to consume either a direct result or an SSE request response.
	if !mcpModernAccepts(r.Header.Get("Accept"), "application/json") || !mcpModernAccepts(r.Header.Get("Accept"), "text/event-stream") {
		writeMCPModernError(w, request.ID, mcpModernHeaderMismatchCode, "Accept must include application/json and text/event-stream", http.StatusBadRequest, nil)
		return false
	}
	var params mcpModernBaseParams
	// Missing or malformed params cannot carry the mandatory per-request capability metadata.
	if json.Unmarshal(request.Params, &params) != nil {
		writeMCPModernError(w, request.ID, -32602, "request params must contain modern MCP metadata", http.StatusBadRequest, nil)
		return false
	}
	headerVersion := strings.TrimSpace(r.Header.Get(mcpProtocolVersionHeader))
	// Unknown versions receive the standard supported/requested payload instead of a generic header error.
	if params.Meta.ProtocolVersion != mcpModernProtocolVersion || headerVersion != mcpModernProtocolVersion {
		// A disagreement is HeaderMismatch only when one side selected the supported modern revision.
		if params.Meta.ProtocolVersion == mcpModernProtocolVersion || headerVersion == mcpModernProtocolVersion {
			writeMCPModernError(w, request.ID, mcpModernHeaderMismatchCode, "MCP-Protocol-Version must match request _meta", http.StatusBadRequest, nil)
			return false
		}
		writeMCPModernError(w, request.ID, mcpUnsupportedProtocolCode, "unsupported MCP protocol version", http.StatusBadRequest, map[string]any{
			"supported": []string{mcpModernProtocolVersion}, "requested": params.Meta.ProtocolVersion,
		})
		return false
	}
	var capabilities map[string]json.RawMessage
	// The standard requires an object on every request, including when the client supports no optional capability.
	if len(params.Meta.ClientCapabilities) == 0 || json.Unmarshal(params.Meta.ClientCapabilities, &capabilities) != nil || capabilities == nil {
		writeMCPModernError(w, request.ID, -32602, "request _meta must include io.modelcontextprotocol/clientCapabilities as an object", http.StatusBadRequest, nil)
		return false
	}
	// Gateways depend on the method header, so absence and disagreement share the standard HeaderMismatch code.
	if strings.TrimSpace(r.Header.Get(mcpMethodHeader)) != request.Method {
		writeMCPModernError(w, request.ID, mcpModernHeaderMismatchCode, "Mcp-Method must match the JSON-RPC method", http.StatusBadRequest, nil)
		return false
	}
	expectedName, nameRequired, err := mcpModernRequestName(request)
	// A method-specific name cannot be mirrored until its params are valid.
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return false
	}
	// Only methods with a standard name or URI parameter require Mcp-Name.
	if nameRequired && r.Header.Get(mcpNameHeader) != expectedName {
		writeMCPModernError(w, request.ID, mcpModernHeaderMismatchCode, "Mcp-Name must match the JSON-RPC params", http.StatusBadRequest, nil)
		return false
	}
	return true
}

// mcpModernMediaTypeMatches compares one Content-Type after removing parameters such as charset.
func mcpModernMediaTypeMatches(value, expected string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	// Invalid or absent media types cannot be treated as their expected default.
	return err == nil && strings.EqualFold(mediaType, expected)
}

// mcpModernAccepts checks one comma-separated Accept header without interpreting unrelated parameters as media types.
func mcpModernAccepts(value, expected string) bool {
	for _, candidate := range strings.Split(value, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(candidate))
		// Malformed entries are ignored while another explicit supported entry may still satisfy the contract.
		if err == nil && strings.EqualFold(mediaType, expected) {
			return true
		}
	}
	return false
}

// allowMCPModernMessage applies the shared per-app budget while preserving a modern JSON-RPC failure envelope.
func allowMCPModernMessage(w http.ResponseWriter, requestID json.RawMessage, appID string) bool {
	// A nil limiter keeps transport tests and deliberately disabled rate limiting operational.
	if messageRateLimiter != nil && !messageRateLimiter.allow(appID) {
		w.Header().Set("Retry-After", "60")
		writeMCPModernError(w, requestID, -32000, "message rate limit exceeded for this MCP server", http.StatusTooManyRequests, nil)
		return false
	}
	return true
}

// allowMCPModernSubscriptionStart applies the existing connection-start budget to long-lived listen requests.
func allowMCPModernSubscriptionStart(w http.ResponseWriter, requestID json.RawMessage, appID string) bool {
	// Subscription POSTs replace GET listeners and therefore consume the same start budget as earlier long-lived connections.
	if sessionStartRateLimiter != nil && !sessionStartRateLimiter.allow(appID) {
		w.Header().Set("Retry-After", "60")
		writeMCPModernError(w, requestID, -32000, "too many subscriptions for this MCP server, please slow down", http.StatusTooManyRequests, nil)
		return false
	}
	return true
}

// mcpModernRequestName returns the exact parameter mirrored by Mcp-Name for standard routed methods.
func mcpModernRequestName(request mcpJSONRPCRequest) (string, bool, error) {
	// Only the currently implemented resource read method has a name-bearing parameter.
	if request.Method != "resources/read" {
		return "", false, nil
	}
	var params struct {
		URI string `json:"uri"`
	}
	// An empty or oversized URI cannot become either an authorization lookup or a routing header value.
	if json.Unmarshal(request.Params, &params) != nil || strings.TrimSpace(params.URI) == "" || len(params.URI) > maxMCPModernResourceURIBytes {
		return "", true, errors.New("resources/read requires a valid uri")
	}
	parsed, err := url.ParseRequestURI(params.URI)
	// Resource identities must be absolute URIs so routing cannot depend on an implicit base.
	if err != nil || parsed.Scheme == "" {
		return "", true, errors.New("resources/read requires an absolute uri")
	}
	return params.URI, true, nil
}

// admitMCPModernApp authenticates one request against an immutable route before loading public resource metadata.
func admitMCPModernApp(ctx context.Context, routeID, token string) (*mcpModernAdmission, int, error) {
	target, err := resolveMCPRoute(ctx, routeID)
	// Route lifecycle remains opaque until a valid execution token proves authority.
	if err != nil {
		return nil, http.StatusUnauthorized, errors.New("invalid token")
	}
	identity, err := validateMCPToken(ctx, target.AppID.String(), token)
	// Token failure never reveals whether the requested family or version exists.
	if err != nil {
		return nil, http.StatusUnauthorized, errors.New("invalid token")
	}
	loader, ok := globalObjectCache.(mcpServerMetadataLoader)
	// A server cannot produce standard serverInfo from guessed or process-wide identity.
	if !ok {
		return nil, http.StatusServiceUnavailable, errors.New("MCP server metadata is unavailable")
	}
	server, err := loader.GetMCPServerMetadata(ctx, target.AppID.String())
	// Incomplete immutable server identity makes this app version unrunnable.
	if err != nil {
		return nil, http.StatusServiceUnavailable, errors.New("MCP server metadata is unavailable")
	}
	server, err = validateMCPServerMetadata(server)
	// The same runtime validation applies to discovery, resources, and subscription results.
	if err != nil {
		return nil, http.StatusServiceUnavailable, errors.New("MCP server metadata is unavailable")
	}
	resources, err := loadMCPEventResources(ctx, target.AppID.String(), identity)
	// Invalid event authority cannot be downgraded into an empty capability after authentication.
	if err != nil {
		return nil, http.StatusServiceUnavailable, errors.New("MCP event resources are unavailable")
	}
	return &mcpModernAdmission{target: target, identity: identity, server: server, resources: resources}, http.StatusOK, nil
}

// handleMCPModernDiscover advertises the stateless revision and only the resource capabilities implemented by this slice.
func handleMCPModernDiscover(w http.ResponseWriter, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	capabilities := map[string]any{}
	// Resource capability is absent for operation-only versions instead of advertising an unusable empty namespace.
	if len(admission.resources) > 0 {
		capabilities["resources"] = map[string]any{"subscribe": true}
	}
	writeMCPModernResult(w, request.ID, map[string]any{
		"supportedVersions": []string{mcpModernProtocolVersion},
		"capabilities":      capabilities,
		"instructions":      admission.server.Description,
		"ttlMs":             mcpModernResourceCacheTTL,
		"cacheScope":        "private",
	}, admission.server)
}

// handleMCPModernResourcesList returns the finite immutable event resource catalogue.
func handleMCPModernResourcesList(w http.ResponseWriter, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	// This finite catalogue has one page and rejects invented continuation cursors.
	if err := admitMCPModernEmptyCursor(request.Params); err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	writeMCPModernResult(w, request.ID, map[string]any{
		"resources":  sortedMCPEventResources(admission.resources),
		"ttlMs":      mcpModernResourceCacheTTL,
		"cacheScope": "private",
	}, admission.server)
}

// handleMCPModernResourceTemplatesList reports the intentionally finite event namespace without templates.
func handleMCPModernResourceTemplatesList(w http.ResponseWriter, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	// This finite namespace has no template continuation state.
	if err := admitMCPModernEmptyCursor(request.Params); err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	writeMCPModernResult(w, request.ID, map[string]any{
		"resourceTemplates": []any{},
		"ttlMs":             mcpModernResourceCacheTTL,
		"cacheScope":        "private",
	}, admission.server)
}

// handleMCPModernResourceRead returns the latest retained webhook payload for one authorized event URI.
func handleMCPModernResourceRead(w http.ResponseWriter, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	uri, _, err := mcpModernRequestName(request)
	// Header validation already admitted the URI, so this branch protects direct focused calls and future routing changes.
	if err != nil {
		writeMCPModernError(w, request.ID, -32602, err.Error(), http.StatusBadRequest, nil)
		return
	}
	resource, allowed := admission.resources[uri]
	// Resource membership is the immutable authorization boundary; clients never submit broker subjects.
	if !allowed {
		writeMCPModernError(w, request.ID, -32602, "event resource is not selected by this MCP app", http.StatusBadRequest, nil)
		return
	}
	snapshot, err := readMCPEventSnapshot(resource)
	// Retention failures cannot be represented as an empty latest value.
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "event resource could not be read", http.StatusServiceUnavailable, nil)
		return
	}
	document := map[string]any{"event": resource.Event, "latest": snapshot}
	encoded, err := json.Marshal(document)
	// Fixed-shape output still fails closed if future metadata becomes unserializable.
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "event resource could not be encoded", http.StatusInternalServerError, nil)
		return
	}
	writeMCPModernResult(w, request.ID, map[string]any{
		"contents": []map[string]any{{"uri": uri, "mimeType": resource.MIMEType, "text": string(encoded)}},
		"ttlMs":    0, "cacheScope": "private",
	}, admission.server)
}

// handleMCPModernSubscriptionsListen opens one POST response stream for the exact resource URIs the server honors.
func handleMCPModernSubscriptionsListen(ctx context.Context, w http.ResponseWriter, token string, request mcpJSONRPCRequest, admission *mcpModernAdmission) {
	var params mcpModernListenParams
	// A missing notifications object is invalid because the stream is opt-in rather than unsolicited.
	if json.Unmarshal(request.Params, &params) != nil || params.Notifications == nil {
		writeMCPModernError(w, request.ID, -32602, "subscriptions/listen requires a notification filter", http.StatusBadRequest, nil)
		return
	}
	// The requested list is bounded independently of the app's already-bounded selected resource set.
	if len(params.Notifications.ResourceSubscriptions) > maxMCPEventResourceCount {
		writeMCPModernError(w, request.ID, -32602, fmt.Sprintf("resourceSubscriptions may contain at most %d URIs", maxMCPEventResourceCount), http.StatusBadRequest, nil)
		return
	}
	// A long-lived stream consumes the shared connection-start budget only after its filter has passed validation.
	if !allowMCPModernSubscriptionStart(w, request.ID, admission.target.AppID.String()) {
		return
	}
	honored := honoredMCPModernResourceSubscriptions(params.Notifications.ResourceSubscriptions, admission.resources)
	flusher, ok := w.(http.Flusher)
	// A server without streaming support cannot acknowledge a subscription it cannot keep open.
	if !ok {
		writeMCPModernError(w, request.ID, -32603, "streaming is unavailable", http.StatusInternalServerError, nil)
		return
	}
	events := make(chan mcpModernEvent, maxMCPModernSubscriptionEvents)
	overflow := make(chan struct{})
	var overflowOnce sync.Once
	subscriptions, err := subscribeMCPModernResources(honored, admission.resources, events, overflow, &overflowOnce)
	// Broker setup completes before acknowledgement so every honored URI is actually live.
	if err != nil {
		writeMCPModernError(w, request.ID, -32603, "event subscriptions could not be started", http.StatusServiceUnavailable, nil)
		return
	}
	defer unsubscribeMCPModernResources(subscriptions)
	streamCtx, cancel := mcpSessionContext(ctx, admission.identity.TokenPolicy.ExpiresAt)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(mcpProtocolVersionHeader, mcpModernProtocolVersion)
	acknowledged := map[string]any{}
	// Only exact authorized URIs appear in the honored subset; unsupported list-change filters remain omitted.
	if len(honored) > 0 {
		acknowledged["resourceSubscriptions"] = honored
	}
	ack := mcpModernNotification("notifications/subscriptions/acknowledged", request.ID, map[string]any{"notifications": acknowledged})
	// The acknowledgement is the first SSE message carrying this subscription ID.
	if !writeMCPModernSSEMessage(w, flusher, ack) {
		return
	}
	ticker := time.NewTicker(mcpModernKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-streamCtx.Done():
			// Client disconnect and token expiry terminate the request without pretending a graceful server result was delivered.
			return
		case <-overflow:
			// Dropping a resource update would violate the honored stream, so overload ends it and requires the client to re-listen and re-read.
			return
		case event := <-events:
			notification := mcpModernNotification("notifications/resources/updated", request.ID, map[string]any{"uri": event.resource.URI})
			// A transport write failure is an abrupt subscription loss and must stop all later notifications.
			if !writeMCPModernSSEMessage(w, flusher, notification) {
				return
			}
		case <-ticker.C:
			// Periodic authorization refresh closes revoked or deactivated subscriptions without leaking their cause on the stream.
			identity, err := validateMCPToken(streamCtx, admission.target.AppID.String(), token)
			if err != nil || identity.TokenID != admission.identity.TokenID {
				return
			}
			// Standard SSE comments preserve intermediary liveness without creating protocol notifications.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// admitMCPModernEmptyCursor accepts the first and only resource page while rejecting unsupported continuation state.
func admitMCPModernEmptyCursor(params json.RawMessage) error {
	var page struct {
		Cursor string `json:"cursor"`
	}
	// Invalid params cannot be interpreted as the default first page.
	if json.Unmarshal(params, &page) != nil {
		return errors.New("resource list params are invalid")
	}
	// No continuation token is minted for the finite immutable catalogue.
	if strings.TrimSpace(page.Cursor) != "" {
		return errors.New("resource list cursor is invalid")
	}
	return nil
}

// honoredMCPModernResourceSubscriptions intersects requested URIs with immutable app authority and removes duplicates.
func honoredMCPModernResourceSubscriptions(requested []string, resources map[string]mcpEventResource) []string {
	seen := make(map[string]struct{}, len(requested))
	honored := make([]string, 0, len(requested))
	for _, uri := range requested {
		// Oversized, unknown, and duplicate URIs are unsupported and therefore omitted from the acknowledged subset.
		if len(uri) == 0 || len(uri) > maxMCPModernResourceURIBytes {
			continue
		}
		if _, allowed := resources[uri]; !allowed {
			continue
		}
		if _, duplicate := seen[uri]; duplicate {
			continue
		}
		seen[uri] = struct{}{}
		honored = append(honored, uri)
	}
	// Stable acknowledgement order prevents request ordering from changing an otherwise identical subscription identity.
	sort.Strings(honored)
	return honored
}

// subscribeMCPModernResources creates exact live NATS subscriptions and flushes them before protocol acknowledgement.
func subscribeMCPModernResources(honored []string, resources map[string]mcpEventResource, events chan<- mcpModernEvent, overflow chan struct{}, overflowOnce *sync.Once) ([]*nats.Subscription, error) {
	// A non-empty honored filter cannot be fulfilled without a connected broker.
	if len(honored) > 0 && (globalNATSClient == nil || globalNATSClient.Conn == nil) {
		return nil, errors.New("event transport is unavailable")
	}
	subscriptions := make([]*nats.Subscription, 0, len(honored))
	for _, uri := range honored {
		resource := resources[uri]
		subscription, err := globalNATSClient.Subscribe(resource.Subject, func(_ *nats.Msg) {
			// The bounded queue preserves order while overload closes the stream instead of silently dropping an honored update.
			select {
			case events <- mcpModernEvent{resource: resource}:
			default:
				overflowOnce.Do(func() { close(overflow) })
			}
		})
		// Partial setup is rolled back so the acknowledgement never overstates live broker coverage.
		if err != nil {
			unsubscribeMCPModernResources(subscriptions)
			return nil, err
		}
		subscriptions = append(subscriptions, subscription)
	}
	// Flush proves every exact subscription reached the server before the first SSE frame is emitted.
	if len(subscriptions) > 0 {
		if err := globalNATSClient.Conn.Flush(); err != nil {
			unsubscribeMCPModernResources(subscriptions)
			return nil, err
		}
	}
	return subscriptions, nil
}

// unsubscribeMCPModernResources releases every core NATS handle owned by one completed listen request.
func unsubscribeMCPModernResources(subscriptions []*nats.Subscription) {
	for _, subscription := range subscriptions {
		// Cleanup is best-effort because the parent NATS connection may already be closing.
		if subscription != nil {
			_ = subscription.Unsubscribe()
		}
	}
}

// mcpModernNotification builds one subscription-tagged JSON-RPC notification with method-specific fields.
func mcpModernNotification(method string, subscriptionID json.RawMessage, fields map[string]any) map[string]any {
	params := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		params[key] = value
	}
	params["_meta"] = map[string]any{mcpSubscriptionIDMetaKey: subscriptionID}
	return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
}

// writeMCPModernSSEMessage writes and flushes one standard message event on a request-scoped response stream.
func writeMCPModernSSEMessage(w io.Writer, flusher http.Flusher, message map[string]any) bool {
	payload, err := json.Marshal(message)
	// Serialization failure cannot be replaced with an unrelated notification on the established stream.
	if err != nil {
		return false
	}
	// A failed or partial write ends this request so no later notification can overtake it.
	if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// writeMCPModernResult adds the mandatory result type and immutable server identity to one successful response.
func writeMCPModernResult(w http.ResponseWriter, id json.RawMessage, result map[string]any, server FixtureServerMetadata) {
	result["resultType"] = "complete"
	result["_meta"] = map[string]any{mcpServerInfoMetaKey: mcpModernServerInfo(server)}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(mcpProtocolVersionHeader, mcpModernProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// mcpModernServerInfo maps authored app metadata onto the standard implementation identity shape.
func mcpModernServerInfo(server FixtureServerMetadata) map[string]any {
	return map[string]any{
		"name": server.Name, "title": server.Title, "version": server.Version, "description": server.Description,
	}
}

// writeMCPModernError emits one bounded modern JSON-RPC failure and repeats the selected protocol response header.
func writeMCPModernError(w http.ResponseWriter, id json.RawMessage, code int, message string, status int, data any) {
	w.Header().Set(mcpProtocolVersionHeader, mcpModernProtocolVersion)
	writeMCPJSONRPCErrorData(w, id, code, message, status, data)
}
