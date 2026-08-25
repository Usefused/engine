package sandbox

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/nats-io/nats.go"
)

// TestExtractEventNamePreservesCompositeAndFallbackSemantics proves alternatives never weaken strict composite routing.
func TestExtractEventNamePreservesCompositeAndFallbackSemantics(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		headers    map[string]string
		expression string
		want       string
	}{
		{name: "nested event wins", body: `{"type":"event_callback","event":{"type":"message"}}`, expression: "body.event.type|body.type", want: "message"},
		{name: "envelope fallback", body: `{"type":"url_verification"}`, expression: "body.event.type|body.type", want: "url_verification"},
		{name: "incomplete composite falls back", body: `{"type":"event_callback","event":{"type":"message"}}`, expression: "body.event.type+header.X-Event-Version|body.type", want: "event_callback"},
		{name: "complete composite remains atomic", body: `{"type":"event_callback","event":{"type":"message"}}`, headers: map[string]string{"X-Event-Version": "v2"}, expression: "body.event.type+header.X-Event-Version|body.type", want: "message.v2"},
	}
	for _, test := range tests {
		// Each transport combination independently exercises the provider-neutral extraction grammar.
		t.Run(test.name, func(t *testing.T) {
			request := makeWebhookRequest(http.MethodPost, "event-extraction", test.body, test.headers)
			// The resolved subject fragment must preserve each case's atomicity and fallback expectation.
			if got := extractEventName(request, []byte(test.body), &webhookConfig{EventExtractionPath: test.expression}); got != test.want {
				t.Fatalf("extractEventName() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestWebhookHandlerRoutesEnvelopeFallback proves the production ingress path publishes a fallback event instead of RAW.
func TestWebhookHandlerRoutesEnvelopeFallback(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	const slug = "event-fallback"
	seedConfig(slug, &webhookConfig{AuthType: "none", EventExtractionPath: "body.event.type|body.type"}, "")
	var published *nats.Msg
	previous := webhookPublishFunc
	webhookPublishFunc = func(message *nats.Msg) error {
		published = message
		return nil
	}
	t.Cleanup(func() { webhookPublishFunc = previous })

	response := httptest.NewRecorder()
	webhookIngressHandler(response, makeWebhookRequest(http.MethodPost, slug, `{"type":"url_verification"}`, nil))
	// A successful request must reach the publish boundary with the resolved envelope event name.
	if response.Code >= http.StatusBadRequest || published == nil || !strings.HasSuffix(published.Subject, ".url_verification") {
		t.Fatalf("status=%d subject=%q", response.Code, publishedSubject(published))
	}
}

// publishedSubject keeps the failure path nil-safe so a missing publish reports the ingress defect instead of panicking.
func publishedSubject(message *nats.Msg) string {
	// No message means ingress stopped before the publish boundary.
	if message == nil {
		return ""
	}
	return message.Subject
}
