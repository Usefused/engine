package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

const (
	mcpEventStreamName       = "WEBHOOKS"
	maxMCPEventResourceCount = 128
	maxMCPEventPayloadBytes  = 256 * 1024
)

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

// mcpEventSnapshot is the bounded webhook occurrence returned by resources/read.
type mcpEventSnapshot struct {
	ID              string          `json:"id"`
	Event           string          `json:"event"`
	ReceivedAt      time.Time       `json:"received_at"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	PayloadBase64   string          `json:"payload_base64,omitempty"`
	PayloadEncoding string          `json:"payload_encoding,omitempty"`
}

// loadMCPEventResources derives exact resource and NATS identities from immutable app selections.
func loadMCPEventResources(ctx context.Context, appID string, identity auth.RuntimeIdentity) (map[string]mcpEventResource, error) {
	resources := map[string]mcpEventResource{}
	// Deployments without the event persistence dependencies cannot advertise a notification capability.
	if globalMCPEventRuntimeStore == nil || globalMCPEventConfigStore == nil {
		return resources, nil
	}
	runtime, selections, err := loadMCPEventScope(ctx, appID, identity)
	if err != nil {
		return nil, err
	}
	hasEvents, err := selectedMCPEvents(selections)
	if err != nil {
		return nil, err
	}
	// An operation-only version needs neither attachment lookup nor broker resources.
	if !hasEvents {
		return resources, nil
	}
	attachment, err := mcpEventAttachmentLabel(ctx, runtime)
	if err != nil {
		return nil, err
	}
	return projectMCPEventResources(selections, identity, attachment)
}

// loadMCPEventScope authenticates one immutable runtime selection snapshot for event discovery.
func loadMCPEventScope(ctx context.Context, appID string, identity auth.RuntimeIdentity) (*store.AppRuntime, []models.SDKSelection, error) {
	parsedAppID, err := uuid.Parse(appID)
	// Event authority repeats exact app identity validation before reading immutable scope.
	if err != nil {
		return nil, nil, errors.New("invalid MCP event app identity")
	}
	runtime, err := globalMCPEventRuntimeStore.GetAppRuntime(ctx, parsedAppID)
	// Missing runtime scope cannot be replaced by mutable workspace visibility.
	if err != nil {
		return nil, nil, fmt.Errorf("load MCP event scope: %w", err)
	}
	// Token, tenant, family, version, and app kind must all agree before subjects are derived.
	if !validMCPEventRuntimeIdentity(runtime, identity) {
		return nil, nil, errors.New("MCP event scope does not match the authorized runtime")
	}
	selections, err := models.DecodeAppSelections(runtime.ScopeSchemaVersion, runtime.Selections)
	// Invalid persisted selection data fails closed before attachment state is consulted.
	if err != nil {
		return nil, nil, fmt.Errorf("decode MCP event scope: %w", err)
	}
	return runtime, selections, nil
}

// validMCPEventRuntimeIdentity requires exact scope identity and hosted MCP consent for alternate app kinds.
func validMCPEventRuntimeIdentity(runtime *store.AppRuntime, identity auth.RuntimeIdentity) bool {
	return runtime.AppID == identity.AppID && runtime.AccountID == identity.AccountID &&
		runtime.AppFamilyID == identity.AppFamilyID && runtime.Kind == identity.Kind &&
		(runtime.Kind == store.AppKindMCP || isHostedMCPVersion(runtime.Kind, runtime.HostedMCP) && identity.HostedMCP)
}

// selectedMCPEvents detects exact webhook names while rejecting broad select-all authority.
func selectedMCPEvents(selections []models.SDKSelection) (bool, error) {
	hasEvents := false
	for _, selection := range selections {
		// A modern acknowledgement must enumerate a finite set of authorized resource URIs.
		if selection.WebhookSelectAll {
			return false, errors.New("MCP event scope cannot use webhook_select_all")
		}
		// Operation-only versions require no attachment lookup or broker capability.
		if len(selection.WebhookNames) > 0 {
			hasEvents = true
		}
	}
	return hasEvents, nil
}

// mcpEventAttachmentLabel admits one exact subject-safe label from applied desired state.
func mcpEventAttachmentLabel(ctx context.Context, runtime *store.AppRuntime) (string, error) {
	// The desired config key is the only server-owned link to the attached ingress label.
	if strings.TrimSpace(runtime.ConfigKey) == "" {
		return "", errors.New("MCP event scope has no applied config identity")
	}
	state, err := globalMCPEventConfigStore.GetConfigState(ctx, runtime.ConfigKey)
	// Store failures are not equivalent to an intentionally absent attachment.
	if err != nil {
		return "", fmt.Errorf("load MCP event attachment: %w", err)
	}
	// Selected events without their applied desired state cannot subscribe under guessed authority.
	if state == nil {
		return "", errors.New("MCP event attachment state is unavailable")
	}
	var document struct {
		WebhookAttachment string `json:"webhook_attachment"`
	}
	// Malformed desired state must not broaden or redirect event delivery.
	if err := json.Unmarshal(state.DesiredState, &document); err != nil {
		return "", errors.New("MCP event attachment state is invalid")
	}
	attachment := strings.TrimSpace(document.WebhookAttachment)
	// Planning requires the attachment, and runtime repeats the invariant before touching NATS.
	if attachment == "" {
		return "", errors.New("MCP event scope has no webhook_attachment")
	}
	safeAttachment := subjectSafeLabel(attachment)
	// Attachment labels occupy one fixed subject segment and cannot contain NATS wildcard syntax.
	if strings.ContainsAny(safeAttachment, "*> \t\r\n") {
		return "", errors.New("MCP webhook_attachment cannot be represented as an exact subscription")
	}
	return safeAttachment, nil
}

// projectMCPEventResources builds finite exact resource and subject identities from the selected events.
func projectMCPEventResources(selections []models.SDKSelection, identity auth.RuntimeIdentity, safeAttachment string) (map[string]mcpEventResource, error) {
	resources := make(map[string]mcpEventResource)
	for _, selection := range selections {
		for _, authoredEventName := range selection.WebhookNames {
			eventName := strings.TrimSpace(authoredEventName)
			// Wildcards or whitespace would turn an exact immutable selection into broader broker authority.
			if eventName == "" || strings.ContainsAny(eventName, "*> \t\r\n") {
				return nil, fmt.Errorf("MCP event name %q cannot be represented as an exact subscription", eventName)
			}
			// The global cap bounds acknowledgement size, broker handles, and per-request authorization work.
			if len(resources) >= maxMCPEventResourceCount {
				return nil, fmt.Errorf("MCP event scope exceeds %d resources", maxMCPEventResourceCount)
			}
			uri := fmt.Sprintf("fused://events/%s/%s", selection.ServiceID, url.PathEscape(eventName))
			qualifiedEvent := selection.ServiceID.String() + "." + eventName
			resources[uri] = mcpEventResource{
				URI: uri, Name: qualifiedEvent, Title: eventName,
				Description: "Latest retained webhook occurrence for " + eventName,
				MIMEType:    "application/json", Event: qualifiedEvent,
				Subject: "webhooks." + identity.AccountID.String() + "." + selection.ServiceID.String() + "." + safeAttachment + "." + eventName,
			}
		}
	}
	return resources, nil
}

// sortedMCPEventResources stabilizes resource discovery independently of map iteration order.
func sortedMCPEventResources(resources map[string]mcpEventResource) []mcpEventResource {
	result := make([]mcpEventResource, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource)
	}
	// Stable URI order makes cache comparisons and protocol tests deterministic.
	sort.Slice(result, func(left, right int) bool { return result[left].URI < result[right].URI })
	return result
}

// readMCPEventSnapshot loads the latest retained occurrence without creating consumer state or acknowledging SDK delivery.
func readMCPEventSnapshot(resource mcpEventResource) (*mcpEventSnapshot, error) {
	// Reading requires JetStream because core NATS deliberately retains no history for disconnected MCP listeners.
	if globalNATSClient == nil || globalNATSClient.JS == nil {
		return nil, errors.New("event storage is unavailable")
	}
	message, err := globalNATSClient.JS.GetLastMsg(mcpEventStreamName, resource.Subject)
	// No retained occurrence is a valid empty resource state before the first webhook arrives.
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil, nil
	}
	// Storage failures remain distinguishable from a resource with no current value.
	if err != nil {
		return nil, fmt.Errorf("read retained webhook occurrence: %w", err)
	}
	// Payload size is bounded before JSON parsing or base64 expansion can allocate a larger response.
	if len(message.Data) > maxMCPEventPayloadBytes {
		return nil, fmt.Errorf("retained webhook payload exceeds %d bytes", maxMCPEventPayloadBytes)
	}
	messageID := strings.TrimSpace(message.Header.Get("X-Webhook-Msg-ID"))
	// Older retained messages without publisher identity receive a stable sequence-derived opaque ID.
	if messageID == "" {
		messageID = fmt.Sprintf("nats:%d", message.Sequence)
	}
	snapshot := &mcpEventSnapshot{ID: messageID, Event: resource.Event, ReceivedAt: message.Time.UTC()}
	// Valid JSON remains structured inside the resource document instead of being double encoded.
	if json.Valid(message.Data) {
		snapshot.Payload = append(json.RawMessage(nil), message.Data...)
		return snapshot, nil
	}
	// Non-JSON provider bodies remain lossless and explicitly labelled rather than being coerced into text.
	snapshot.PayloadBase64 = base64.StdEncoding.EncodeToString(message.Data)
	snapshot.PayloadEncoding = "base64"
	return snapshot, nil
}
