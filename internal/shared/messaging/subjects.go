package messaging

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	FusedEngineStream = "FUSED_ENGINE_EVENTS"
	// ProviderRateLimitBucket is a separate compacted JetStream stream. Its
	// latest value is the live quota authority; PostgreSQL consumes revisions
	// only as an eventual projection.
	ProviderRateLimitBucket = "FUSED_PROVIDER_RATE_LIMITS"

	FusedEngineSessionWildcard   = "fused_engine.session.>"
	EngineExecutionEventsSubject = "engine.execution.events.v1"
	EngineAuthEventsSubject      = "engine.auth.events.v1"
	AppTokenInvalidatedSubject   = "engine.app_token.invalidated.v1"
	// AuthWebhookSubjectMarker reserves a non-UUID service segment for
	// Engine-owned events so user-created provider webhook labels cannot collide.
	AuthWebhookSubjectMarker = "fused-auth"
)

// WebhookSubject describes the public routing fields shared by provider and Engine-owned deliveries.
type WebhookSubject struct {
	ServiceID uuid.UUID
	EventName string
	FusedAuth bool
}

func ProviderRateLimitKVStream() string {
	return "KV_" + ProviderRateLimitBucket
}

func ProviderRateLimitKVSubject() string {
	return "$KV." + ProviderRateLimitBucket + ".>"
}

// FusedEngineStreamSubjects returns every subject retained by the shared internal Engine stream.
func FusedEngineStreamSubjects() []string {
	return []string{
		FusedEngineSessionWildcard,
		EngineExecutionEventsSubject,
		EngineAuthEventsSubject,
		AppTokenInvalidatedSubject,
	}
}

func FusedEngineSessionSubject(appID string) string {
	return fmt.Sprintf("fused_engine.session.%s", appID)
}

// FusedAuthWebhookSubject scopes one projected auth transition to its workspace, app family, and service.
func FusedAuthWebhookSubject(accountID, appFamilyID, serviceID uuid.UUID, eventName string) string {
	return fmt.Sprintf("webhooks.%s.%s.%s.%s.%s", accountID, AuthWebhookSubjectMarker, appFamilyID, serviceID, eventName)
}

// ParseWebhookSubject recovers the service and public event name without exposing internal family routing to SDKs.
func ParseWebhookSubject(subject string) (WebhookSubject, bool) {
	parts := strings.Split(subject, ".")
	// Every retained webhook subject begins with its stream namespace and workspace identity.
	if len(parts) < 5 || parts[0] != "webhooks" {
		return WebhookSubject{}, false
	}
	accountID, accountErr := uuid.Parse(parts[1])
	// Provider and system subjects both require a real workspace UUID before any narrower routing is trusted.
	if accountErr != nil || accountID == uuid.Nil {
		return WebhookSubject{}, false
	}
	// Engine-owned auth subjects reserve the third segment and carry family identity before service identity.
	if parts[2] == AuthWebhookSubjectMarker {
		return parseFusedAuthWebhookSubject(parts)
	}
	serviceID, err := uuid.Parse(parts[2])
	// Provider webhook subjects require an exact service UUID before the registration label.
	if err != nil {
		return WebhookSubject{}, false
	}
	return WebhookSubject{ServiceID: serviceID, EventName: strings.Join(parts[4:], ".")}, true
}

// parseFusedAuthWebhookSubject validates every family-scoped system segment before exposing its event name.
func parseFusedAuthWebhookSubject(parts []string) (WebhookSubject, bool) {
	// The fixed prefix, account, marker, family, service, and event require at least six segments.
	if len(parts) < 6 {
		return WebhookSubject{}, false
	}
	appFamilyID, familyErr := uuid.Parse(parts[3])
	serviceID, serviceErr := uuid.Parse(parts[4])
	// Family and service remain mandatory after the shared parser has already admitted workspace identity.
	if familyErr != nil || serviceErr != nil || appFamilyID == uuid.Nil || serviceID == uuid.Nil {
		return WebhookSubject{}, false
	}
	eventName := strings.Join(parts[5:], ".")
	// An empty trailing event would create a receiver message that no generated handler can acknowledge safely.
	if strings.TrimSpace(eventName) == "" {
		return WebhookSubject{}, false
	}
	return WebhookSubject{ServiceID: serviceID, EventName: eventName, FusedAuth: true}, true
}
