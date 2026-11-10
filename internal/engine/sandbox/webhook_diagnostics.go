package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxWebhookDiagnosticBodyBytes = 64 << 10

var webhookDiagnosticsRepository store.WebhookDiagnosticsStore
var webhookDiagnosticsKey []byte

type webhookDiagnosticCaptureKey struct{}

// webhookDiagnosticCapture contains only body bytes, never signature, authorization, cookie or query values.
type webhookDiagnosticCapture struct {
	Body          string `json:"body"`
	Encoding      string `json:"encoding"`
	ContentType   string `json:"content_type"`
	Method        string `json:"method"`
	OriginalBytes int    `json:"original_bytes"`
	CapturedBytes int    `json:"captured_bytes"`
	Truncated     bool   `json:"truncated"`
}

// SetWebhookDiagnosticsStore wires the optional encrypted recorder once at HTTP server startup.
func SetWebhookDiagnosticsStore(s store.Store, key []byte) {
	webhookDiagnosticsRepository, _ = s.(store.WebhookDiagnosticsStore)
	webhookDiagnosticsKey = append([]byte(nil), key...)
}

// withWebhookDiagnosticCapture snapshots exact original body bytes before verification or normalization.
func withWebhookDiagnosticCapture(ctx context.Context, r *http.Request, raw []byte) context.Context {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	// A provider-controlled header cannot bypass the private evidence size limit.
	if len(contentType) > 256 {
		contentType = ""
	}
	capture := webhookDiagnosticCapture{Method: r.Method, ContentType: contentType, OriginalBytes: len(raw), Encoding: "utf-8"}
	// A hostile sender cannot turn diagnostic storage into an unbounded body archive.
	if len(raw) > maxWebhookDiagnosticBodyBytes {
		raw = raw[:maxWebhookDiagnosticBodyBytes]
		capture.Truncated = true
	}
	capture.CapturedBytes = len(raw)
	capture.Body = string(raw)
	// Binary or truncated UTF-8 remains byte-exact through an explicitly labelled base64 representation.
	if !utf8.Valid(raw) {
		capture.Body = base64.StdEncoding.EncodeToString(raw)
		capture.Encoding = "base64"
	}
	return context.WithValue(ctx, webhookDiagnosticCaptureKey{}, capture)
}

// saveWebhookDiagnosticCapture never places captured bytes on the canonical bus or changes provider acknowledgement.
func saveWebhookDiagnosticCapture(ctx context.Context, event models.EngineExecutionEvent) {
	capture, present := ctx.Value(webhookDiagnosticCaptureKey{}).(webhookDiagnosticCapture)
	// Synthetic delivery outcomes and legacy adapters cannot manufacture an original ingress body.
	if !present || webhookDiagnosticsRepository == nil {
		return
	}
	payload, _ := json.Marshal(capture)
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	scope := store.WebhookDiagnosticScope{AccountID: event.AccountID, ServiceID: event.ServiceID, RegistrationID: event.WebhookID, EventID: event.ID}
	// A capture outage must not replay provider work or disclose bytes through an error/log fallback.
	if err := webhookDiagnosticsRepository.SaveWebhookDiagnostics(writeCtx, scope, payload, webhookDiagnosticsKey); err != nil {
		trace.SpanFromContext(ctx).AddEvent("webhook.diagnostics.capture", trace.WithAttributes(attribute.String("outcome", "unavailable")))
	}
}
