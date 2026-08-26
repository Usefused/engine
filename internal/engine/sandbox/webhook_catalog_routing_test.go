package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tidwall/gjson"
)

// TestWebhookCatalogueRoutesExactWireIdentities prevents SDK catalogue names from drifting from parsed provider identities.
func TestWebhookCatalogueRoutesExactWireIdentities(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		headers    map[string]string
		expression string
		urlEvent   string
		want       string
	}{
		{
			name: "Stripe event type", body: `{"id":"evt_example","object":"event","type":"charge.succeeded","data":{"object":{"id":"ch_example"}}}`,
			expression: "body.type", want: "charge.succeeded",
		},
		{
			name: "Notion event type", body: `{"id":"event-example","type":"page.created","entity":{"id":"page-example","type":"page"}}`,
			expression: "body.type", want: "page.created",
		},
		{
			name: "Supabase uppercase identity", body: `{"type":"INSERT","table":"tasks","schema":"public","record":{"id":1},"old_record":null}`,
			expression: "body.type", want: "INSERT",
		},
		{
			name: "Jira webhookEvent identity", body: `{"timestamp":1787702400000,"webhookEvent":"jira:issue_created","issue":{"id":"10001","key":"DEMO-1"}}`,
			expression: "body.webhookEvent", want: "jira:issue_created",
		},
		{
			name: "Bitbucket header identity", body: `{"repository":{"name":"example"},"push":{"changes":[]}}`,
			headers: map[string]string{"X-Event-Key": "repo:push"}, expression: "header.X-Event-Key", want: "repo:push",
		},
		{
			name: "Trello action identity", body: `{"action":{"id":"action-example","type":"createCard","data":{"card":{"id":"card-example"}}},"model":{},"webhook":{}}`,
			expression: "body.action.type", want: "createCard",
		},
		{
			name: "Slack nested event rather than envelope", body: `{"type":"event_callback","event":{"type":"app_mention","text":"hello"},"event_id":"EvExample"}`,
			expression: "body.event.type", want: "app_mention",
		},
		{
			name: "Slack subtype remains payload detail", body: `{"type":"event_callback","event":{"type":"message","subtype":"message_deleted","deleted_ts":"1787702400.000001"}}`,
			expression: "body.event.type", want: "message",
		},
		{
			name: "Slack current organization example double wrapper", body: `{"type":"event_callback","event":{"team_id":null,"enterprise_id":"EExample","event":{"type":"team_access_granted","team_ids":["TExample"]}}}`,
			expression: "body.event.event.type|body.event.type", want: "team_access_granted",
		},
		{
			name: "Slack reviewed nested fallback preserves ordinary events", body: `{"type":"event_callback","event":{"type":"message","subtype":"message_deleted","deleted_ts":"1787702400.000001"}}`,
			expression: "body.event.event.type|body.event.type", want: "message",
		},
		{
			name: "Google Drive bodyless state header", body: "",
			headers: map[string]string{"X-Goog-Resource-State": "update", "X-Goog-Channel-ID": "channel-example"}, expression: "header.X-Goog-Resource-State", want: "update",
		},
		{
			name: "Linear exact composite identity", body: `{"type":"Issue","action":"create","data":{"id":"issue-example"}}`,
			expression: "body.type+body.action", want: "Issue.create",
		},
		{
			name: "Twilio normalized message status", body: "MessageSid=SMexample&MessageStatus=delivered",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"}, expression: "body.MessageStatus|body.CallStatus", want: "delivered",
		},
		{
			name: "Twilio normalized call status fallback", body: "CallSid=CAexample&CallStatus=in-progress",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, expression: "body.MessageStatus|body.CallStatus", want: "in-progress",
		},
		{
			name: "Twilio ordered status precedence", body: "MessageStatus=sent&CallStatus=completed",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, expression: "body.MessageStatus|body.CallStatus", want: "sent",
		},
		{
			name: "Twilio empty first status falls back", body: "MessageStatus=&CallStatus=completed",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, expression: "body.MessageStatus|body.CallStatus", want: "completed",
		},
		{
			name: "Gmail raw PubSub envelope", body: `{"message":{"data":"eyJlbWFpbEFkZHJlc3MiOiJ1c2VyQGV4YW1wbGUudGVzdCIsImhpc3RvcnlJZCI6IjEyMyJ9","messageId":"123"},"subscription":"projects/example/subscriptions/gmail"}`,
			want: "RAW",
		},
		{
			name: "Confluence exact descriptor URL identity", body: `{"page":{"id":"123","title":"Example"}}`,
			urlEvent: "page_created", want: "page_created",
		},
		{
			name: "Jira Exclude body exact URL fallback", body: "",
			expression: "body.webhookEvent", urlEvent: "jira:issue_deleted", want: "jira:issue_deleted",
		},
	}
	for _, test := range tests {
		// Independent fixtures exercise the real parser before the same extraction function used at ingress.
		t.Run(test.name, func(t *testing.T) {
			request := webhookCatalogueRequest(test.body, test.headers, test.urlEvent)
			body, err := parseWebhookPayload(request, []byte(test.body))
			// Parsing failure means a valid callback cannot reach its catalogue-selected event filter.
			if err != nil {
				t.Fatalf("parseWebhookPayload(): %v", err)
			}
			got := extractEventName(request, body, &webhookConfig{EventExtractionPath: test.expression})
			// Exact case, punctuation, and fallback order form the contract between provider payloads and SDK subscriptions.
			if got != test.want {
				t.Fatalf("extractEventName(%q) = %q, want exact catalogue name %q", test.expression, got, test.want)
			}
		})
	}
}

// TestWebhookCatalogueOktaBatchCannotFanOut documents that selecting an array is not equivalent to publishing each contained event.
func TestWebhookCatalogueOktaBatchCannotFanOut(t *testing.T) {
	const payload = `{"eventType":"com.okta.event_hook","data":{"events":[{"uuid":"event-one","eventType":"user.lifecycle.create"},{"uuid":"event-two","eventType":"user.lifecycle.activate"}]}}`
	request := webhookCatalogueRequest(payload, nil, "")
	body, err := parseWebhookPayload(request, []byte(payload))
	// The batch is valid transport data; unsupported fan-out must not be confused with a parsing failure.
	if err != nil {
		t.Fatalf("parseWebhookPayload(): %v", err)
	}
	// A scalar path cannot identify two events inside an array and therefore must not appear to select either one.
	if got := extractEventName(request, body, &webhookConfig{EventExtractionPath: "body.data.events.eventType"}); got != "RAW" {
		t.Fatalf("scalar extraction of an Okta batch = %q, want unresolved RAW fallback", got)
	}
	projected := extractEventName(request, body, &webhookConfig{EventExtractionPath: "body.data.events.#.eventType"})
	values := gjson.Parse(projected)
	// The current API returns one array-shaped string, not separate catalogue identities or separately published deliveries.
	if !values.IsArray() || len(values.Array()) != 2 {
		t.Fatalf("projected batch = %q, want an unsupported two-event array value", projected)
	}
	for _, event := range values.Array() {
		// Neither contained event can be treated as the resolved scalar identity for the complete delivery.
		if projected == event.String() {
			t.Fatalf("batch projection unexpectedly became a selectable scalar event %q", projected)
		}
	}
}

// webhookCatalogueRequest supplies realistic request metadata without invoking storage, authentication, or network services.
func webhookCatalogueRequest(body string, headers map[string]string, eventName string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "https://engine.example.test/webhook/catalogue-example", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	route := chi.NewRouteContext()
	route.URLParams.Add("urlSlug", "catalogue-example")
	// Only an explicitly configured per-event receiver URL may provide the missing wire discriminator.
	if eventName != "" {
		route.URLParams.Add("eventName", eventName)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
}
