package messaging

import (
	"testing"

	"github.com/google/uuid"
)

// TestFusedAuthWebhookSubjectRoundTrip proves system routing remains family-scoped while the public parser exposes only service semantics.
func TestFusedAuthWebhookSubjectRoundTrip(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()
	serviceID := uuid.New()
	subject := FusedAuthWebhookSubject(accountID, familyID, serviceID, "fused.auth.connection.completed")

	parsed, ok := ParseWebhookSubject(subject)
	// A valid Engine-owned subject must be distinguishable from provider ingress before analytics are considered.
	if !ok || !parsed.FusedAuth {
		t.Fatalf("ParseWebhookSubject(%q) = %#v, %v", subject, parsed, ok)
	}
	if parsed.ServiceID != serviceID || parsed.EventName != "fused.auth.connection.completed" {
		t.Fatalf("parsed subject = %#v", parsed)
	}
}

// TestParseWebhookSubjectPreservesProviderLayout protects the established service-label-event contract.
func TestParseWebhookSubjectPreservesProviderLayout(t *testing.T) {
	serviceID := uuid.New()
	parsed, ok := ParseWebhookSubject("webhooks." + uuid.NewString() + "." + serviceID.String() + ".orders.created.v2")
	// Provider events must not be mistaken for internal auth transitions merely because their names contain dots.
	if !ok || parsed.FusedAuth {
		t.Fatalf("provider subject parsed as %#v, %v", parsed, ok)
	}
	if parsed.ServiceID != serviceID || parsed.EventName != "created.v2" {
		t.Fatalf("parsed provider subject = %#v", parsed)
	}
}

// TestParseWebhookSubjectRejectsMalformedSystemIdentity keeps reserved subjects fail-closed.
func TestParseWebhookSubjectRejectsMalformedSystemIdentity(t *testing.T) {
	_, ok := ParseWebhookSubject("webhooks.not-an-account.fused-auth.not-a-family.not-a-service.fused.auth.token.refreshed")
	// Malformed relational routing must never be delivered as either a provider or Fused event.
	if ok {
		t.Fatal("malformed Fused auth subject was accepted")
	}
}

// TestParseWebhookSubjectRejectsMalformedProviderAccount prevents invalid workspace routing from reaching provider analytics.
func TestParseWebhookSubjectRejectsMalformedProviderAccount(t *testing.T) {
	_, ok := ParseWebhookSubject("webhooks.not-an-account." + uuid.NewString() + ".orders.created")
	// A valid service UUID cannot compensate for a malformed workspace segment.
	if ok {
		t.Fatal("provider subject with malformed account was accepted")
	}
}
