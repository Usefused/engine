package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

const maxAuditResponseBytes = 1 << 20

const (
	auditFinalizationTimeout  = 2 * time.Second
	auditFinalizationAttempts = 2
)

var errControlAuditUnavailable = errors.New("control audit recorder is unavailable")

// auditStatusWriter can delay an ordinary mutation response until its final
// audit event is durable. Streaming handlers retain normal Flusher behaviour;
// flushing commits the response because HTTP cannot replace it afterwards.
type auditStatusWriter struct {
	http.ResponseWriter
	status           int
	committed        bool
	deferCommit      bool
	captureEnvelope  bool
	header           http.Header
	originalHeader   http.Header
	body             bytes.Buffer
	bodyOverflow     bool
	envelope         bytes.Buffer
	envelopeOverflow bool
	flushRequested   bool
	preserveOverflow bool
}

func newAuditStatusWriter(w http.ResponseWriter, deferCommit, captureEnvelope, preserveOverflow bool) *auditStatusWriter {
	original := w.Header().Clone()
	return &auditStatusWriter{
		ResponseWriter: w, deferCommit: deferCommit, captureEnvelope: captureEnvelope, preserveOverflow: preserveOverflow,
		header: original.Clone(), originalHeader: original,
	}
}

func (writer *auditStatusWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *auditStatusWriter) Header() http.Header {
	if writer.deferCommit && !writer.committed {
		if writer.header == nil {
			writer.header = writer.ResponseWriter.Header().Clone()
		}
		return writer.header
	}
	return writer.ResponseWriter.Header()
}

func (writer *auditStatusWriter) Flush() {
	if writer.deferCommit && !writer.committed {
		if writer.status == 0 {
			writer.status = http.StatusOK
		}
		writer.flushRequested = true
		return
	}
	writer.commit()
	writer.flushUnderlying()
}

func (writer *auditStatusWriter) flushUnderlying() {
	if err := http.NewResponseController(writer.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("failed to flush audited response", slog.Any("error", err))
	}
}

func (writer *auditStatusWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	if writer.deferCommit && !writer.committed {
		return
	}
	writer.committed = true
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *auditStatusWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	writer.captureGraphQLEnvelope(body)
	if !writer.deferCommit || writer.committed {
		return writer.ResponseWriter.Write(body)
	}
	if writer.bodyOverflow {
		return len(body), nil
	}
	if writer.body.Len()+len(body) > maxAuditResponseBytes {
		if writer.preserveOverflow {
			writer.commit()
			return writer.ResponseWriter.Write(body)
		}
		writer.bodyOverflow = true
		writer.body.Reset()
		return len(body), nil
	}
	return writer.body.Write(body)
}

func (writer *auditStatusWriter) captureGraphQLEnvelope(chunk []byte) {
	if !writer.captureEnvelope || writer.envelopeOverflow {
		return
	}
	if writer.envelope.Len()+len(chunk) > maxAuditResponseBytes {
		writer.envelopeOverflow = true
		writer.envelope.Reset()
		return
	}
	_, _ = writer.envelope.Write(chunk)
}

func (writer *auditStatusWriter) graphQLEnvelopeFailed() bool {
	if writer.envelopeOverflow {
		return true
	}
	if writer.envelope.Len() == 0 {
		return false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(writer.envelope.Bytes(), &envelope) != nil {
		return true
	}
	return nonEmptyJSONValue(envelope["errors"]) || nonEmptyJSONValue(envelope["error"])
}

func nonEmptyJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]"))
}

func (writer *auditStatusWriter) statusCode() int {
	if writer.status == 0 {
		return http.StatusOK
	}
	return writer.status
}

func (writer *auditStatusWriter) commit() {
	if writer.committed {
		return
	}
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.deferCommit {
		replaceHTTPHeader(writer.ResponseWriter.Header(), writer.Header())
	}
	writer.committed = true
	writer.ResponseWriter.WriteHeader(writer.status)
	if writer.body.Len() > 0 {
		_, _ = writer.ResponseWriter.Write(writer.body.Bytes())
		writer.body.Reset()
	}
	if writer.flushRequested {
		writer.flushUnderlying()
	}
}

func (writer *auditStatusWriter) serviceUnavailable() bool {
	if writer.committed {
		return false
	}
	replaceHTTPHeader(writer.ResponseWriter.Header(), writer.originalHeader)
	writer.body.Reset()
	writer.bodyOverflow = false
	writer.status = http.StatusServiceUnavailable
	writer.committed = true
	http.Error(writer.ResponseWriter, `{"error":"audit service unavailable"}`, http.StatusServiceUnavailable)
	return true
}

func replaceHTTPHeader(destination, source http.Header) {
	clear(destination)
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

func recordControlAudit(ctx context.Context, recorder accesscontrol.AuditRecorder, event accesscontrol.AuditEvent) error {
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditFinalizationTimeout)
	defer cancel()
	var err error
	for attempt := 1; attempt <= auditFinalizationAttempts; attempt++ {
		err = persistControlAudit(durableCtx, recorder, event, "audit_persist")
		if err == nil || errors.Is(err, errControlAuditUnavailable) {
			break
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to persist control audit", slog.Any("error", err))
	}
	return err
}

// requireControlAudit fails closed before an externally visible mutation. The
// event is an attempt receipt; it is not the final authorization or execution
// outcome, which is recorded after all field checks and handler execution.
func requireControlAudit(ctx context.Context, recorder accesscontrol.AuditRecorder, event accesscontrol.AuditEvent) error {
	return persistControlAudit(ctx, recorder, event, "audit_preflight")
}

func persistControlAudit(ctx context.Context, recorder accesscontrol.AuditRecorder, event accesscontrol.AuditEvent, errorType string) error {
	if recorder == nil {
		recordAuditPersistenceFailure(ctx, errorType, errControlAuditUnavailable)
		return errControlAuditUnavailable
	}
	if err := recorder.RecordAuthorizationAudit(ctx, event); err != nil {
		recordAuditPersistenceFailure(ctx, errorType, err)
		return err
	}
	return nil
}

func recordAuditPersistenceFailure(ctx context.Context, errorType string, err error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("engine.audit.persist_failed", trace.WithAttributes(attribute.String("error.type", errorType)))
	span.RecordError(err)
	span.SetStatus(codes.Error, "audit persistence failed")
}

func newControlAuditEvent(r *http.Request, actor accesscontrol.Actor, action, path string, requirements []accesscontrol.Requirement, outcome accesscontrol.AuditOutcome, status int, reason string) accesscontrol.AuditEvent {
	event := accesscontrol.AuditEvent{
		ID:             uuid.New(),
		ActorSubjectID: actor.SubjectID, ActorCredentialID: actor.CredentialID,
		Action: action, RequestID: middleware.GetReqID(r.Context()), Method: r.Method,
		Path: path, Outcome: outcome, StatusCode: status, ReasonCode: reason,
		Metadata: map[string]any{"requirements": len(requirements), "authorization_revision": actor.Authorization.Revision},
	}
	if spanContext := trace.SpanContextFromContext(r.Context()); spanContext.IsValid() {
		event.TraceID = spanContext.TraceID().String()
	}
	if len(requirements) > 0 {
		event.Permission = requirements[0].Permission
		event.Resource = requirements[0].Resource
	}
	if outcome == accesscontrol.AuditDenied {
		event.MissingRequirements, _ = accesscontrol.MissingPermissionsFromContext(r.Context())
		if len(event.MissingRequirements) > 0 {
			event.Permission = event.MissingRequirements[0].Permission
			event.Resource = event.MissingRequirements[0].Resource
		}
	}
	return event
}

func setAuditMissingRequirements(event *accesscontrol.AuditEvent, missing []accesscontrol.Requirement) {
	event.MissingRequirements = append([]accesscontrol.Requirement(nil), missing...)
	if len(missing) > 0 {
		event.Permission = missing[0].Permission
		event.Resource = missing[0].Resource
	}
}

func controlAuditAction(method string) string {
	return "control.http." + strings.ToLower(method)
}

func controlAuditOutcome(status int) accesscontrol.AuditOutcome {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return accesscontrol.AuditDenied
	}
	if status >= http.StatusBadRequest {
		return accesscontrol.AuditFailed
	}
	return accesscontrol.AuditSucceeded
}

func controlAuditReason(outcome accesscontrol.AuditOutcome, status int) string {
	if outcome != accesscontrol.AuditDenied {
		return ""
	}
	if status == http.StatusUnauthorized {
		return "unauthenticated"
	}
	return "permission_denied"
}

func controlGraphQLAuditMiddleware(recorder accesscontrol.AuditRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveGraphQLAuditRequest(w, r, next, recorder)
		})
	}
}

func serveGraphQLAuditRequest(w http.ResponseWriter, r *http.Request, next http.Handler, recorder accesscontrol.AuditRecorder) {
	if !isGraphQLControlPath(r.URL.Path) {
		next.ServeHTTP(w, r)
		return
	}
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	if !ok {
		next.ServeHTTP(w, r)
		return
	}
	captureContext, _ := accesscontrol.ContextWithRequiredPermissionsCapture(r.Context())
	operation, auditable := classifyGraphQLAudit(r)
	if operation == "mutation" {
		captureContext = accesscontrol.ContextWithMutationAuditEvidence(captureContext)
	}
	r = r.WithContext(captureContext)
	requirements := auditRequirements(r.Context(), nil)
	sensitiveRead, proceed := requireGraphQLAuditReceipt(w, r, recorder, actor, operation, auditable, requirements)
	if !proceed {
		return
	}
	if recorder == nil {
		next.ServeHTTP(w, r)
		return
	}
	writer := newAuditStatusWriter(w, operation == "mutation" || sensitiveRead, true, operation == "mutation")
	if recovered, panicked := serveAuditedHandler(next, writer, r); panicked {
		recordGraphQLPanic(r, recorder, actor, operation, requirements)
		panic(recovered)
	}
	finalizeGraphQLAudit(r, recorder, actor, operation, auditable, requirements, writer)
}

func requireGraphQLAuditReceipt(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, operation string, auditable bool, requirements []accesscontrol.Requirement) (bool, bool) {
	if operation == "mutation" {
		return false, recordGraphQLMutationAttempt(w, r, recorder, actor, requirements)
	}
	sensitiveRead := operation == "query" && auditable
	if sensitiveRead {
		return true, recordGraphQLReadAttempt(w, r, recorder, actor, requirements)
	}
	return false, true
}

func recordGraphQLMutationAttempt(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement) bool {
	preflight := newControlAuditEvent(r, actor, "control.graphql.mutation", r.URL.Path, requirements, accesscontrol.AuditAttempted, 0, "attempted")
	if err := requireControlAudit(r.Context(), recorder, preflight); err != nil {
		slog.ErrorContext(r.Context(), "GraphQL mutation blocked because audit is unavailable", slog.Any("error", err))
		http.Error(w, `{"error":"audit service unavailable"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func recordGraphQLReadAttempt(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement) bool {
	preflight := newControlAuditEvent(r, actor, "control.graphql.query", r.URL.Path, requirements, accesscontrol.AuditAttempted, 0, "attempted")
	if err := requireControlAudit(r.Context(), recorder, preflight); err != nil {
		http.Error(w, `{"error":"audit service unavailable"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func serveAuditedHandler(next http.Handler, writer *auditStatusWriter, r *http.Request) (recovered any, panicked bool) {
	defer func() {
		if recovered = recover(); recovered != nil {
			panicked = true
		}
	}()
	next.ServeHTTP(writer, r)
	return nil, false
}

func recordGraphQLPanic(r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, operation string, fallback []accesscontrol.Requirement) {
	requirements := auditRequirements(r.Context(), fallback)
	event := newControlAuditEvent(r, actor, "control.graphql."+operation, r.URL.Path, requirements, accesscontrol.AuditFailed, http.StatusInternalServerError, "handler_panic")
	_ = recordControlAudit(r.Context(), recorder, event)
}

func finalizeGraphQLAudit(r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, operation string, auditable bool, requirements []accesscontrol.Requirement, writer *auditStatusWriter) {
	status, outcome, reason := graphQLAuditResult(writer)
	missing, _ := accesscontrol.MissingPermissionsFromContext(r.Context())
	if len(missing) > 0 {
		outcome, reason = accesscontrol.AuditDenied, "permission_denied"
	}
	if !auditable && outcome != accesscontrol.AuditDenied {
		writer.commit()
		return
	}
	requirements = auditRequirements(r.Context(), requirements)
	event := newControlAuditEvent(r, actor, "control.graphql."+operation, r.URL.Path, requirements, outcome, status, reason)
	if operation == "mutation" {
		applyMutationAuditEvidence(r.Context(), &event)
	}
	err := recordControlAudit(r.Context(), recorder, event)
	finishAuditedResponse(writer, operation != "mutation" && writer.deferCommit, err)
}

func graphQLAuditResult(writer *auditStatusWriter) (int, accesscontrol.AuditOutcome, string) {
	status := writer.statusCode()
	outcome := controlAuditOutcome(status)
	reason := controlAuditReason(outcome, status)
	if writer.bodyOverflow {
		return http.StatusServiceUnavailable, accesscontrol.AuditFailed, "response_too_large"
	}
	if writer.graphQLEnvelopeFailed() && outcome == accesscontrol.AuditSucceeded {
		outcome = accesscontrol.AuditFailed
	}
	return status, outcome, reason
}

func finishAuditedResponse(writer *auditStatusWriter, failClosed bool, auditErr error) {
	if writer.bodyOverflow {
		writer.serviceUnavailable()
		return
	}
	if auditErr != nil && failClosed && writer.serviceUnavailable() {
		return
	}
	writer.commit()
}

func auditRequirements(ctx context.Context, fallback []accesscontrol.Requirement) []accesscontrol.Requirement {
	if requirements, ok := accesscontrol.RequiredPermissionsFromContext(ctx); ok {
		return requirements
	}
	return fallback
}
