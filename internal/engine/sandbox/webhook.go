package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/tidwall/gjson"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookid"
	"github.com/Usefused/engine/internal/engine/webhookverify"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// webhookConfigStore is the minimal slice of store.Store the ingress path
// needs -- mirrors idempotencyStore's pattern (idempotency_cache.go) of
// depending on a narrow interface wired via a package-level setter, rather
// than threading a full store.Store through the HTTP handler chain.
type webhookConfigStore interface {
	GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*store.WorkspaceWebhook, error)
}

var globalWebhookConfigStore webhookConfigStore

// SetWebhookConfigStore wires the store used to resolve inbound webhook
// slugs. Called once at boot (see cmd/engine/cmd/start.go), alongside
// SetIdempotencyStore. Until this is called, every inbound webhook 404s.
func SetWebhookConfigStore(s webhookConfigStore) {
	globalWebhookConfigStore = s
}

// webhookConfig holds the resolved configuration for a webhook integration,
// denormalized from fused_workspace_webhooks at apply time (see
// engine_owned_webhooks_plan.md) so ingress never needs to look anything up
// beyond the one indexed row this came from.
type webhookConfig struct {
	RegistrationID      uuid.UUID
	AccountID           string
	ServiceID           string
	ServiceVersionID    uuid.UUID
	EventExtractionPath string
	AuthType            string
	AuthLocation        string
	AuthKeyName         string
	SignatureHeader     string
	VerificationHeaders []string
	SignaturePolicy     *signaturepolicy.Config
	CallbackURL         string
	// SecretBucketID is the immutable apply-time binding used for runtime
	// lookup; SecretRef supplies only its validated secret key.
	SecretBucketID uuid.UUID
	SecretRef      string
	// Label is the registration's identity (store.WorkspaceWebhook.Label --
	// a kind: webhook desired config's own name; see plans/plan-webhook-kind.md).
	// Published into the NATS subject alongside service/event so two
	// registrations on the same service that happen to produce the same
	// event name stay on distinct subjects -- without this, any delivery
	// subscriber listening on that service+event received every matching
	// delivery regardless of which registration it came from (the isolation
	// bug plan-webhook-kind.md's subject-filter section describes).
	Label string
}

// webhookIngressHandler processes incoming webhook requests.
//
// An OTEL thread is opened for every inbound request because each webhook
// represents an externally-triggered execution (provider → Fused → customer).
// Internal infrastructure calls (the config store read) are not traced here
// — only the user/agent-visible execution boundary gets a thread.
func webhookIngressHandler(w http.ResponseWriter, r *http.Request) {
	if !entitlement.LiveEntitlement.Load().WebhookIngestionEnabled {
		writeError(w, http.StatusPaymentRequired, "webhook ingestion not enabled on current plan")
		return
	}

	urlSlug := chi.URLParam(r, "urlSlug")
	if urlSlug == "" {
		// URL slug must be provided to identify the integration.
		writeError(w, http.StatusBadRequest, "urlSlug required")
		return
	}

	// Providers may append routing segments to the slug, so normalize to the
	// fixed token width. This must be a fixed-width cut, never a delimiter search:
	// the nanoid alphabet itself includes '-', the same character separating
	// the token from its decorative "-serviceSlug" suffix, so "find the
	// first '-'" would truncate a legitimate token early.
	if len(urlSlug) > webhookid.SlugLength {
		urlSlug = urlSlug[:webhookid.SlugLength]
	}

	// Open an OTEL span for the full ingress lifecycle.
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook.ingest", trace.WithAttributes(
		attribute.Bool("webhook.registration.present", true),
	))
	defer span.End()

	rawBody, err := captureWebhookBody(r)
	if err != nil {
		span.SetStatus(codes.Error, "payload_capture_failed")
		writeError(w, http.StatusBadRequest, "invalid request payload")
		return
	}
	body, err := parseWebhookPayload(r, rawBody)
	if err != nil {
		span.SetStatus(codes.Error, "payload_parse_failed")
		writeError(w, http.StatusBadRequest, "invalid request payload")
		return
	}

	config, err := fetchWebhookConfig(ctx, urlSlug)
	if err != nil {
		if errors.Is(err, store.ErrWorkspaceWebhookNotFound) {
			span.SetStatus(codes.Error, "webhook not found")
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		// Database errors can include statement values, so keep ingress telemetry
		// useful without copying the underlying error into stdout or span data.
		span.SetStatus(codes.Error, "config fetch failed")
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Attach account/service identifiers once the config is resolved.
	span.SetAttributes(
		attribute.String("account_id", config.AccountID),
		attribute.String("service_id", config.ServiceID),
	)

	authOutcome := validateWebhookAuth(ctx, w, r, rawBody, config)
	if webhookAuthStopsIngress(span, authOutcome) {
		return
	}

	eventName := extractEventName(r, body, config)
	if eventName == "" {
		publishRejection(ctx, config, "UNKNOWN", "failed to extract event name", len(body))
		span.SetStatus(codes.Error, "failed to extract event name")
		writeError(w, http.StatusBadRequest, "failed to extract event name using configured extraction path")
		return
	}

	_, pubSpan := otel.Tracer("engine").Start(ctx, "engine.webhook.publish")
	publishWebhookEvent(w, r, body, urlSlug, eventName, config)
	pubSpan.SetStatus(codes.Ok, "published")
	pubSpan.End()

	span.SetStatus(codes.Ok, "webhook ingested")
}

func webhookAuthStopsIngress(span trace.Span, outcome webhookAuthOutcome) bool {
	if outcome == webhookAuthAccepted {
		return false
	}
	if outcome == webhookAuthRejected {
		span.SetStatus(codes.Error, "auth rejected")
	} else {
		span.SetStatus(codes.Ok, "challenge responded")
	}
	return true
}

// parseWebhookPayload reads the request payload, adapting for form data or JSON.
const maxWebhookBodyBytes = 2 << 20

func captureWebhookBody(r *http.Request) ([]byte, error) {
	if r.Method == http.MethodGet || r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil || len(body) > maxWebhookBodyBytes {
		return nil, errors.New("webhook body is invalid")
	}
	// Downstream parsing reads only this immutable capture, so verification and
	// event extraction cannot observe different bytes.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func parseWebhookPayload(r *http.Request, rawBody []byte) ([]byte, error) {
	contentType := r.Header.Get("Content-Type")

	// If it's a GET request, encode the URL query parameters as JSON.
	if r.Method == http.MethodGet {
		return queryOrFormToJSON(r.URL.Query())
	}

	// If it's URL encoded form data, parse the form and convert to JSON.
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(rawBody))
		if err != nil {
			return nil, fmt.Errorf("failed to parse form data")
		}
		return queryOrFormToJSON(form)
	}

	// Fallback to reading the raw body (typically for application/json).
	// If body is empty but query parameters exist on POST/PUT, use the query params.
	if len(rawBody) == 0 && len(r.URL.Query()) > 0 {
		return queryOrFormToJSON(r.URL.Query())
	}

	return rawBody, nil
}

// fetchWebhookConfig resolves an inbound webhook slug against the Engine's
// own fused_workspace_webhooks table using a single indexed Postgres read.
// No cache layer sits in front of this, so a slug rotation or a
// registration's removal takes effect immediately instead of within a TTL
// window.
func fetchWebhookConfig(ctx context.Context, urlSlug string) (*webhookConfig, error) {
	if globalWebhookConfigStore == nil {
		return nil, fmt.Errorf("webhook store not configured")
	}
	ww, err := globalWebhookConfigStore.GetWorkspaceWebhookBySlug(ctx, urlSlug)
	if err != nil {
		return nil, err
	}
	var secretBucketID uuid.UUID
	if ww.SecretBucketID != nil {
		secretBucketID = *ww.SecretBucketID
	}
	return &webhookConfig{
		RegistrationID:      ww.ID,
		AccountID:           ww.AccountID.String(),
		ServiceID:           ww.ServiceID.String(),
		ServiceVersionID:    ww.ServiceVersionID,
		EventExtractionPath: ww.EventExtractionPath,
		AuthType:            ww.AuthType,
		AuthLocation:        ww.AuthLocation,
		AuthKeyName:         ww.AuthKeyName,
		SignatureHeader:     ww.SignatureHeader,
		VerificationHeaders: ww.VerificationHeaders,
		SignaturePolicy:     ww.SignaturePolicy,
		CallbackURL:         ww.CallbackURL,
		SecretBucketID:      secretBucketID,
		SecretRef:           ww.SecretRef,
		Label:               ww.Label,
	}, nil
}

// validateWebhookAuth delegates signature verification to the webhookverify
// package — a standalone, zero-dependency trust component (S6.1). The handler
// retains HTTP response writing so the verifier stays pure and testable.
//
// Complexity: 1 (verify call) + 1 (result check) = 2
type webhookAuthOutcome string

const (
	webhookAuthAccepted webhookAuthOutcome = "accepted"
	webhookAuthHandled  webhookAuthOutcome = "handled"
	webhookAuthRejected webhookAuthOutcome = "rejected"
)

func validateWebhookAuth(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, config *webhookConfig) webhookAuthOutcome {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.webhook.verify")
	defer span.End()
	if config.SignaturePolicy != nil {
		return validateSignaturePolicy(ctx, span, w, r, body, config)
	}

	var signingSecret string
	if config.AuthType != "" && config.AuthType != "none" {
		secret, err := globalSecretResolver.GetWebhookSecret(ctx, uuid.MustParse(config.AccountID), config.SecretBucketID, config.SecretRef)
		if err != nil {
			span.SetStatus(codes.Error, "failed to resolve webhook secret")
			publishRejection(ctx, config, "UNKNOWN", "internal config error", len(body))
			writeError(w, http.StatusInternalServerError, "internal server error")
			return webhookAuthRejected
		}
		signingSecret = secret
	}

	result := webhookverify.Verify(r, body, webhookverify.Config{
		AuthType:            config.AuthType,
		AuthLocation:        config.AuthLocation,
		AuthKeyName:         config.AuthKeyName,
		SignatureHeader:     config.SignatureHeader,
		SigningSecret:       signingSecret,
		VerificationHeaders: config.VerificationHeaders,
	})

	observability.WebhookVerify.Add(ctx, 1)
	span.SetAttributes(attribute.String("webhook.verification.result", result.Code))

	if result.OK {
		span.SetStatus(codes.Ok, "verified")
		return webhookAuthAccepted
	}

	// Keep telemetry dimensions bounded; the human-readable reason can include
	// configured header names and belongs only in the scoped rejection response.
	span.SetStatus(codes.Error, "verification rejected")
	// Durable analytics accepts only the bounded verifier code. The response may
	// name a configured header for an operator, but that value is not safe as a
	// metric/event dimension and must not outlive this request.
	publishRejection(ctx, config, "", result.Code, len(body))
	writeError(w, http.StatusUnauthorized, result.Reason)
	return webhookAuthRejected
}

func validateSignaturePolicy(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, body []byte, config *webhookConfig) webhookAuthOutcome {
	result := webhookverify.VerifyPolicy(ctx, config.SignaturePolicy, webhookverify.PolicyInput{
		Request: r, RawBody: body, CallbackURL: config.CallbackURL,
		Resolve: func(resolveCtx context.Context, ref string) (string, error) {
			// The reviewed recipe may select the key but never a different bucket
			// binding than the immutable registration resolved at apply time.
			if ref != config.SecretRef {
				return "", errors.New("signature secret reference does not match registration")
			}
			return globalSecretResolver.GetWebhookSecret(resolveCtx, uuid.MustParse(config.AccountID), config.SecretBucketID, ref)
		},
	})
	observability.WebhookVerify.Add(ctx, 1)
	span.SetAttributes(attribute.String("webhook.verification.result", result.Code))
	if !result.OK {
		span.SetStatus(codes.Error, "verification rejected")
		publishRejection(ctx, config, "", result.Code, len(body))
		writeError(w, http.StatusUnauthorized, result.Reason)
		return webhookAuthRejected
	}
	if result.Code != webhookverify.CodeChallengeResponded {
		span.SetStatus(codes.Ok, "verified")
		return webhookAuthAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.ChallengeBody)
	span.SetStatus(codes.Ok, "challenge responded")
	return webhookAuthHandled
}

// extractEventName resolves the event name from the request using the configured JSON path or header.
func extractEventName(r *http.Request, body []byte, config *webhookConfig) string {
	if config.EventExtractionPath != "" {
		// Composite paths are joined with "+", e.g. "body.eventType+body.action".
		// Each segment is extracted independently and the values are joined with ".".
		segments := strings.Split(config.EventExtractionPath, "+")
		isComposite := len(segments) > 1
		var parts []string
		for _, seg := range segments {
			val := extractSegmentValue(r, body, seg)

			// For composite paths every segment must resolve — a partial match
			// would produce an ambiguous event name and route incorrectly.
			if val == "" && isComposite {
				return ""
			}
			if val != "" {
				parts = append(parts, val)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ".")
		}
	}

	// Fallback to the URL parameter 'eventName' if no path matches.
	eventName := chi.URLParam(r, "eventName")
	if eventName == "" {
		eventName = "RAW"
	}
	return eventName
}

func extractSegmentValue(r *http.Request, body []byte, seg string) string {
	seg = strings.TrimSpace(seg)
	switch {
	case strings.HasPrefix(seg, "header."):
		return r.Header.Get(seg[7:])
	case strings.HasPrefix(seg, "body."):
		return gjson.GetBytes(body, seg[5:]).String()
	case strings.HasPrefix(seg, "query."):
		return r.URL.Query().Get(seg[6:])
	default:
		slog.WarnContext(r.Context(), "unrecognized event extraction prefix", slog.String("segment", seg))
		return ""
	}
}

// webhookPublishFunc is the NATS publish hook for webhook events. It is a
// package-level var so tests can substitute a no-op without a real NATS server.
// Production wires in the real JetStream publish via Init.
var webhookPublishFunc = func(msg *nats.Msg) error {
	_, err := globalNATSClient.PublishMsgJS(msg)
	return err
}

// publishWebhookEvent builds the NATS payload and publishes it synchronously.
func publishWebhookEvent(w http.ResponseWriter, r *http.Request, body []byte, urlSlug, eventName string, config *webhookConfig) {
	downstreamPayload := buildDownstreamPayload(r, body, urlSlug, eventName)
	eventData, _ := json.Marshal(downstreamPayload)

	msgID := uuid.New().String()
	// Subject layout: webhooks.<account>.<service>.<label>.<event>. label is
	// inserted between service and event (not appended after) so every
	// publisher and consumer resolves the registration and event positions
	// consistently.
	subject := fmt.Sprintf("webhooks.%s.%s.%s.%s", config.AccountID, config.ServiceID, subjectSafeLabel(config.Label), eventName)

	natsMsg := nats.NewMsg(subject)
	natsMsg.Data = eventData
	natsMsg.Header.Set("X-Webhook-Msg-ID", msgID)
	natsMsg.Header.Set("X-Webhook-Start-Time", time.Now().Format(time.RFC3339Nano))
	natsMsg.Header.Set("X-Fused-Service-Version-ID", config.ServiceVersionID.String())
	natsMsg.Header.Set("X-Fused-Webhook-ID", config.RegistrationID.String())

	// Synchronous wait for NATS JetStream ACK to ensure delivery.
	if err := webhookPublishFunc(natsMsg); err != nil {
		slog.ErrorContext(r.Context(), "Failed to publish webhook to NATS JetStream", slog.Any("error", err), slog.String("subject", subject))
		writeError(w, http.StatusInternalServerError, "failed to publish event: "+err.Error())
		return
	}

	publishAnalyticsIngestion(r.Context(), msgID, eventName, len(eventData), config)

	if shouldObserveWebhookSchema() {
		publishSchemaObservation(body, eventName, config)
	}

	slog.InfoContext(r.Context(), "Webhook received, validated, and published to NATS", slog.String("service_id", config.ServiceID), slog.String("event", eventName))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// subjectSafeLabel guards the NATS subject's fixed segment positions: "." is
// the subject delimiter, and a kind: webhook desired config's name (the label) has
// no character restriction today, so a literal "." in a name would otherwise
// shift every downstream positional parse. FilterSubjects construction must
// apply this exact substitution or a dotted label would not match its own
// published subject.
func subjectSafeLabel(label string) string {
	return strings.ReplaceAll(label, ".", "-")
}

func shouldObserveWebhookSchema() bool {
	return os.Getenv("FUSED_ENV") != "development"
}

// buildDownstreamPayload constructs the payload struct representing the normalized webhook request.
func buildDownstreamPayload(r *http.Request, body []byte, urlSlug, eventName string) map[string]any {
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	queryParams := make(map[string]any)
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			queryParams[k] = v[0]
		} else if len(v) > 1 {
			queryParams[k] = v
		}
	}

	var jsonBody any
	// If we can parse the body as JSON, do so. Otherwise stringify it.
	if r.Method != http.MethodGet && len(body) > 0 {
		if err := json.Unmarshal(body, &jsonBody); err != nil {
			jsonBody = string(body)
		}
	}

	return map[string]any{
		"body":    jsonBody,
		"headers": headers,
		"query":   queryParams,
		"path": map[string]string{
			"urlSlug":   urlSlug,
			"eventName": eventName,
		},
	}
}

// publishAnalyticsIngestion records a successful webhook ingestion in analytics.
func publishAnalyticsIngestion(ctx context.Context, msgID, eventName string, payloadSize int, config *webhookConfig) {
	accountID, serviceID := webhookExecutionIDs(config)
	if accountID == uuid.Nil || serviceID == uuid.Nil {
		return
	}
	event := executionevent.NewWebhookEvent(executionevent.WebhookEventInput{
		MessageID: msgID, AccountID: accountID, ServiceID: serviceID,
		ServiceVersionID: config.ServiceVersionID, RegistrationID: config.RegistrationID, EventName: eventName,
		DeliveryStatus: "ingested", VerificationStatus: "verified", PayloadSize: int64(payloadSize), OccurredAt: time.Now(),
	})
	if err := executionevent.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Failed to publish webhook execution event", slog.Any("error", err), slog.String("event_id", event.ID.String()))
	}
}

// publishRejection records an aborted/rejected webhook in analytics.
func publishRejection(ctx context.Context, config *webhookConfig, eventName, errorReason string, payloadSize int) {
	if config == nil || config.AccountID == "" || config.ServiceID == "" {
		return
	}
	accountID, serviceID := webhookExecutionIDs(config)
	if accountID == uuid.Nil || serviceID == uuid.Nil {
		return
	}
	if eventName == "" {
		eventName = "UNKNOWN"
	}
	msgID := uuid.New().String()
	event := executionevent.NewWebhookEvent(executionevent.WebhookEventInput{
		MessageID: msgID, AccountID: accountID, ServiceID: serviceID,
		ServiceVersionID: config.ServiceVersionID, RegistrationID: config.RegistrationID, EventName: eventName,
		DeliveryStatus: "rejected", VerificationStatus: "rejected", FailureReason: errorReason,
		PayloadSize: int64(payloadSize), OccurredAt: time.Now(),
	})
	if err := executionevent.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Failed to publish rejected webhook execution event", slog.Any("error", err), slog.String("event_id", event.ID.String()))
	}
}

func webhookExecutionIDs(config *webhookConfig) (uuid.UUID, uuid.UUID) {
	if config == nil {
		return uuid.Nil, uuid.Nil
	}
	accountID, _ := uuid.Parse(config.AccountID)
	serviceID, _ := uuid.Parse(config.ServiceID)
	return accountID, serviceID
}

// queryOrFormToJSON converts URL query params or URL-encoded forms into JSON data.
func queryOrFormToJSON(values map[string][]string) ([]byte, error) {
	data := make(map[string]any)
	for k, v := range values {
		if len(v) == 1 {
			data[k] = v[0]
		} else if len(v) > 1 {
			data[k] = v
		}
	}
	return json.Marshal(data)
}
