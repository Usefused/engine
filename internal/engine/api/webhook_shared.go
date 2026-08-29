package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// These transport-agnostic helpers keep event validation, subject filtering,
// delivery analytics, and attachment resolution separate from the gRPC stream.

// partitionWebhookEventRequests separates reserved Fused auth names before provider workspace validation performs any lookup.
func partitionWebhookEventRequests(events []string) ([]string, []string) {
	providerEvents := make([]string, 0, len(events))
	authEvents := make([]string, 0, len(events))
	// A single pass preserves request order while separating the independently authorized namespaces.
	for _, event := range events {
		_, eventName, found := strings.Cut(event, ".")
		// Every reserved name is authorized from exact SDK selections, never from mutable workspace service or webhook rows.
		if found && strings.HasPrefix(eventName, "fused.auth.") {
			authEvents = append(authEvents, event)
			continue
		}
		providerEvents = append(providerEvents, event)
	}
	return providerEvents, authEvents
}

// validateRequestedEvents retains provider events selected by this exact immutable SDK version without per-event database reads.
func validateRequestedEvents(runtime *store.AppRuntime, events []string) ([]string, error) {
	// An empty provider half needs no selection decoding.
	if len(events) == 0 {
		return nil, nil
	}
	selections, err := models.DecodeAppSelections(runtime.ScopeSchemaVersion, runtime.Selections)
	// Invalid immutable scope cannot authorize a provider subject from mutable workspace state.
	if err != nil {
		return nil, err
	}
	selected := selectedWebhookEvents(selections)
	validEvents := make([]string, 0, len(events))
	// Membership checks use the already-decoded immutable selection index for the complete request batch.
	for _, event := range events {
		// ALL retains its established client-side catch-all registration behavior.
		if event == "ALL" {
			validEvents = append(validEvents, event)
			continue
		}
		serviceText, eventName, found := strings.Cut(event, ".")
		serviceID, parseErr := uuid.Parse(serviceText)
		// Malformed values and names absent from this exact version cannot create broker filters.
		if !found || parseErr != nil || !selectedWebhookEvent(selected, serviceID, eventName) {
			continue
		}
		validEvents = append(validEvents, event)
	}
	return validEvents, nil
}

// webhookEventSelection records either a bounded exact-name set or an explicit all-webhooks grant for one service.
type webhookEventSelection struct {
	all   bool
	names map[string]struct{}
}

// selectedWebhookEvents projects the exact immutable selection list into constant-time request membership checks.
func selectedWebhookEvents(selections []models.SDKSelection) map[uuid.UUID]webhookEventSelection {
	selected := make(map[uuid.UUID]webhookEventSelection, len(selections))
	// Duplicate service selections merge into one bounded authorization set without storage access.
	for _, selection := range selections {
		current := selected[selection.ServiceID]
		// SelectAll historically includes webhooks, while WebhookSelectAll is the explicit webhook-only equivalent.
		if selection.SelectAll || selection.WebhookSelectAll {
			current.all = true
		}
		// Exact selected names remain bounded by the immutable app selection contract.
		if current.names == nil {
			current.names = make(map[string]struct{}, len(selection.WebhookNames))
		}
		// Persisted exact names are copied into a constant-time membership set.
		for _, name := range selection.WebhookNames {
			current.names[name] = struct{}{}
		}
		selected[selection.ServiceID] = current
	}
	return selected
}

// selectedWebhookEvent checks one provider event against its exact service selection.
func selectedWebhookEvent(selected map[uuid.UUID]webhookEventSelection, serviceID uuid.UUID, eventName string) bool {
	selection, found := selected[serviceID]
	// Missing services never inherit webhook eligibility from a shared bucket or mutable workspace activation.
	if !found {
		return false
	}
	// An explicit all-webhooks selection authorizes every documented event generated for this exact service version.
	if selection.all {
		return true
	}
	_, found = selection.names[eventName]
	return found
}

// buildFilterSubjects creates only explicit provider-webhook filters; reserved Fused auth subjects are added separately.
func buildFilterSubjects(accountID uuid.UUID, webhookLabel string, validEvents []string) []string {
	var filterSubjects []string
	// Provider deliveries require an explicit kind:webhook attachment label.
	if webhookLabel != "" {
		// Each authorized provider event becomes one exact retained-stream subject.
		for _, ev := range validEvents {
			// ALL has no bounded provider subject expansion in the existing attachment contract.
			if ev == "ALL" {
				continue
			}
			serviceID, eventName, found := strings.Cut(ev, ".")
			// Malformed and reserved system events cannot become provider subjects under a user label.
			if !found || isFusedAuthEventName(eventName) {
				continue
			}
			filterSubjects = append(filterSubjects, "webhooks."+accountID.String()+"."+serviceID+"."+subjectSafeLabel(webhookLabel)+"."+eventName)
		}
	}
	return filterSubjects
}

// resolveAuthEventFilterSubjects authorizes reserved auth events against the exact immutable SDK selection.
func resolveAuthEventFilterSubjects(ctx context.Context, s store.Store, appID, accountID, appFamilyID uuid.UUID, validEvents []string) ([]string, error) {
	// Provider-only subscriptions keep their established path and do not require SDK connected-auth selection loading.
	if !containsFusedAuthRequest(validEvents) {
		return nil, nil
	}
	runtime, err := s.GetAppRuntime(ctx, appID)
	// Missing or malformed exact runtime state cannot authorize an implicit family subscription.
	if err != nil {
		return nil, err
	}
	return authEventFilterSubjectsForRuntime(runtime, accountID, appFamilyID, validEvents)
}

// authEventFilterSubjectsForRuntime authorizes reserved names from one already-loaded exact SDK version.
func authEventFilterSubjectsForRuntime(runtime *store.AppRuntime, accountID, appFamilyID uuid.UUID, validEvents []string) ([]string, error) {
	// Exact app-kind enforcement applies only when the client requested the reserved auth-event surface.
	if len(validEvents) == 0 {
		return nil, nil
	}
	// Authentication and persisted runtime identity must agree before family-scoped subjects are constructed.
	if runtime.AccountID != accountID || runtime.AppFamilyID != appFamilyID || runtime.Kind != store.AppKindSDK {
		return nil, errors.New("app runtime identity does not authorize SDK auth events")
	}
	selections, err := models.DecodeAppSelections(runtime.ScopeSchemaVersion, runtime.Selections)
	// Invalid immutable selections fail closed instead of treating an SDK as broadly OAuth-capable.
	if err != nil {
		return nil, err
	}
	connectedServices := connectedAuthSelectionServices(selections)
	return buildAuthEventFilterSubjects(accountID, appFamilyID, connectedServices, validEvents)
}

// containsFusedAuthRequest detects the reserved namespace before the exact vocabulary and service authorization checks run.
func containsFusedAuthRequest(events []string) bool {
	// Prefix detection intentionally precedes exact vocabulary validation so unknown reserved names fail closed later.
	for _, event := range events {
		_, eventName, found := strings.Cut(event, ".")
		// Any requested name under the reserved namespace must be explicitly admitted or rejected.
		if found && strings.HasPrefix(eventName, "fused.auth.") {
			return true
		}
	}
	return false
}

// connectedAuthSelectionServices builds the bounded exact-service set whose selected auth path supports OAuth or OIDC.
func connectedAuthSelectionServices(selections []models.SDKSelection) map[uuid.UUID]struct{} {
	services := make(map[uuid.UUID]struct{}, len(selections))
	// One bounded set supports every requested auth lifecycle name without repeated selection scans.
	for _, selection := range selections {
		// A service is implicit-event capable only when this exact SDK version selected a connected-auth path.
		if sdkSelectionUsesConnectedAuth(selection) {
			services[selection.ServiceID] = struct{}{}
		}
	}
	return services
}

// sdkSelectionUsesConnectedAuth recognizes canonical and admitted OAuth/OIDC aliases in the immutable selection.
func sdkSelectionUsesConnectedAuth(selection models.SDKSelection) bool {
	// The pinned selected auth path is authoritative when planning resolved one exact scheme.
	if isConnectedAuthType(selection.AuthType) || isConnectedAuthType(selection.CredentialSourceAuthType) {
		return true
	}
	// RequiredAuth is the persisted selected AND-member set, so unused OAuth alternatives never enter this loop.
	for _, required := range selection.RequiredAuth {
		// Required-auth entries preserve OAuth capability for selected mixed or alternative operation contracts.
		if isConnectedAuthType(required.AuthType) {
			return true
		}
	}
	return false
}

// isConnectedAuthType normalizes punctuation without weakening the OAuth/OIDC family allowlist.
func isConnectedAuthType(value string) bool {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	// Only browser-connected authorization families receive Fused lifecycle events.
	switch normalized {
	case "oauth", "oauth2", "oauth2authorizationcode", "oidc", "openidconnect":
		return true
	default:
		return false
	}
}

// buildAuthEventFilterSubjects creates the implicit half of the receiver's explicit-plus-system subject union.
func buildAuthEventFilterSubjects(accountID, appFamilyID uuid.UUID, connectedServices map[uuid.UUID]struct{}, validEvents []string) ([]string, error) {
	filterSubjects := make([]string, 0, len(validEvents))
	// Every request must independently satisfy reserved-name and exact-service authorization.
	for _, requested := range validEvents {
		// ALL is retained for provider compatibility but cannot bypass exact Fused event authorization.
		if requested == "ALL" {
			continue
		}
		serviceText, eventName, found := strings.Cut(requested, ".")
		// Non-system names remain the explicit provider webhook path.
		if !found || !strings.HasPrefix(eventName, "fused.auth.") {
			continue
		}
		serviceID, err := uuid.Parse(serviceText)
		// validateRequestedEvents already checked this shape, but construction independently fails closed.
		if err != nil || !isFusedAuthEventName(eventName) {
			return nil, errors.New("invalid Fused auth event request")
		}
		_, allowed := connectedServices[serviceID]
		// A service present for static auth or another SDK version does not authorize this exact subscription.
		if !allowed {
			return nil, errors.New("Fused auth event service is not selected for connected auth")
		}
		filterSubjects = append(filterSubjects, messaging.FusedAuthWebhookSubject(accountID, appFamilyID, serviceID, eventName))
	}
	return filterSubjects, nil
}

// isFusedAuthEventName reserves the exact public lifecycle vocabulary from provider webhook labels.
func isFusedAuthEventName(eventName string) bool {
	// One canonical mapping keeps projection and subscription admission on the same closed vocabulary.
	return authevent.IsWebhookEventName(eventName)
}

// shouldPublishWebhookAnalytics admits only structurally valid provider subjects to provider delivery accounting.
func shouldPublishWebhookAnalytics(subject string) bool {
	parsed, ok := messaging.ParseWebhookSubject(subject)
	// Reserved Fused auth subjects share transport mechanics but are not provider webhook executions.
	if !ok || parsed.FusedAuth {
		return false
	}
	return true
}

// publishFailedAnalytics records exhausted provider delivery without admitting Fused-owned system events.
func publishFailedAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, msgID string, m *nats.Msg) {
	publishWebhookOutcome(ctx, accountID, serviceID.String(), eventName, msgID, "failed", "delivery attempts exhausted", m)
}

// publishSuccessAnalytics records one acknowledged provider delivery on the canonical execution-event path.
func publishSuccessAnalytics(ctx context.Context, accountID uuid.UUID, msgID string, m *nats.Msg) {
	parsed, ok := messaging.ParseWebhookSubject(m.Subject)
	// Malformed or Engine-owned subjects never enter provider delivery analytics.
	if !ok || parsed.FusedAuth {
		return
	}
	publishWebhookOutcome(ctx, accountID, parsed.ServiceID.String(), parsed.EventName, msgID, "success", "", m)
}

// publishWebhookOutcome projects one provider delivery result into the canonical execution event stream.
func publishWebhookOutcome(ctx context.Context, accountID uuid.UUID, serviceIDStr, eventName, msgID, status, failureReason string, message *nats.Msg) {
	serviceID, _ := uuid.Parse(serviceIDStr)
	serviceVersionID, _ := uuid.Parse(message.Header.Get("X-Fused-Service-Version-ID"))
	registrationID, _ := uuid.Parse(message.Header.Get("X-Fused-Webhook-ID"))
	event := executionevent.NewWebhookEvent(executionevent.WebhookEventInput{
		MessageID: msgID, AccountID: accountID, ServiceID: serviceID, ServiceVersionID: serviceVersionID,
		RegistrationID: registrationID, EventName: eventName, DeliveryStatus: status, VerificationStatus: "verified",
		FailureReason: failureReason, Environment: "production", PayloadSize: int64(len(message.Data)),
		LatencyMs: int64(computeLatencyMs(message.Header.Get("X-Webhook-Start-Time"))), AttemptCount: webhookDeliveryCount(message), OccurredAt: time.Now(),
	})
	// Analytics publication is best-effort and must not change provider message acknowledgement.
	if err := executionevent.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Failed to publish webhook delivery event", slog.Any("error", err), slog.String("event_id", event.ID.String()))
	}
}

// webhookDeliveryCount normalizes unavailable JetStream metadata to the first delivery attempt.
func webhookDeliveryCount(message *nats.Msg) int {
	metadata, err := message.Metadata()
	// Direct unit messages and first deliveries both have one effective attempt.
	if err != nil || metadata.NumDelivered == 0 {
		return 1
	}
	return int(metadata.NumDelivered)
}

// resolveWebhookAttachmentLabel looks up which kind: webhook desired config (if
// any) appID's own config attached, entirely server-side -- the connecting
// client (generated SDK/MCP code) never has to know or report this itself.
// fused_apps.config_key (set by the atomic app apply transaction)
// links this runtime identity back to the exact kind: sdk/kind: mcp document
// that created it, and that document's own webhook_attachment field
// (sdkConfigDocument.WebhookAttachment) names the registration whose events
// this connection may receive -- see plans/plan-webhook-kind.md's subject-filter
// section. A missing scope, a scope with no config_key (never happens for a
// config-created scope, but defensive), or a config with no
// webhook_attachment all return ("", nil): "this connection gets no webhook
// subscription," not an error -- most SDKs/MCPs never attach a webhook at
// all, and that's a normal, valid connection.
func resolveWebhookAttachmentLabel(ctx context.Context, configStore store.ConfigRepository, s store.Store, appID uuid.UUID) (string, error) {
	scope, err := s.GetAppRuntime(ctx, appID)
	// A missing exact app is a normal no-attachment result for callers that race deactivation.
	if err != nil {
		if errors.Is(err, store.ErrAppRuntimeNotFound) {
			return "", nil
		}
		return "", err
	}
	return resolveWebhookAttachmentLabelForRuntime(ctx, configStore, scope)
}

// resolveWebhookAttachmentLabelForRuntime reads explicit attachment state for one already-authorized immutable app.
func resolveWebhookAttachmentLabelForRuntime(ctx context.Context, configStore store.ConfigRepository, scope *store.AppRuntime) (string, error) {
	// Apps without a desired-state key cannot have an explicit kind:webhook attachment.
	if strings.TrimSpace(scope.ConfigKey) == "" {
		return "", nil
	}
	state, err := configStore.GetConfigState(ctx, scope.ConfigKey)
	// Storage errors stay distinguishable from an omitted attachment.
	if err != nil {
		return "", err
	}
	// A missing desired-state row means there is no explicit attachment to authorize.
	if state == nil {
		return "", nil
	}
	var doc struct {
		WebhookAttachment string `json:"webhook_attachment"`
	}
	// Malformed desired state cannot silently broaden a provider webhook subscription.
	if err := json.Unmarshal(state.DesiredState, &doc); err != nil {
		return "", err
	}
	return strings.TrimSpace(doc.WebhookAttachment), nil
}

// subjectSafeLabel guards the NATS subject's fixed segment positions -- see
// sandbox/webhook.go's identical function (duplicated rather than shared
// across packages for one line) because a literal "." in a label must never
// reach the subject as-is.
func subjectSafeLabel(label string) string {
	return strings.ReplaceAll(label, ".", "-")
}

// computeLatencyMs converts trusted publisher timing metadata into bounded delivery analytics input.
func computeLatencyMs(startTimeStr string) int64 {
	// Older webhook publishers without a start header report a neutral latency.
	if startTimeStr == "" {
		return 0
	}
	start, err := time.Parse(time.RFC3339Nano, startTimeStr)
	// Invalid provider timing metadata is not promoted into negative or unbounded analytics.
	if err != nil {
		return 0
	}
	return time.Since(start).Milliseconds()
}
