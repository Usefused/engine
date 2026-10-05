package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type privateExecutionCall struct {
	Ordinal   int    `json:"ordinal"`
	Request   string `json:"request"`
	Response  string `json:"response"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}
type privateExecutionDiagnostics struct {
	Error      *executionappvm.DiagnosticError `json:"error,omitempty"`
	Calls      []privateExecutionCall          `json:"calls"`
	Incomplete bool                            `json:"incomplete"`
	Truncated  bool                            `json:"truncated"`
}

// diagnostics takes an immutable snapshot even when cancellation left a provider call incomplete.
func (host *recordingCapabilityHost) diagnostics(runErr error) json.RawMessage {
	host.mu.Lock()
	defer host.mu.Unlock()
	detail := privateExecutionDiagnostics{Error: executionappvm.PrivateDiagnostic(runErr), Calls: []privateExecutionCall{}, Incomplete: host.completed != len(host.calls)}
	for index, call := range host.calls {
		item := privateExecutionCall{Ordinal: call.Ordinal, Request: executionappvm.BoundDiagnostic(string(call.Request)), Response: executionappvm.BoundDiagnostic(string(call.Response)), Truncated: len(call.Request) > 65536 || len(call.Response) > 65536}
		// A provider HTTP error can have a useful body even though no public result is valid.
		if failure := host.diagnosticErrors[index]; failure != nil {
			item.Error = failure.Message
			item.Response = failure.Response
			item.Truncated = item.Truncated || failure.Truncated
		}
		detail.Calls = append(detail.Calls, item)
		raw, _ := json.Marshal(detail)
		// Preserve earlier captured calls and report the storage boundary instead of failing execution.
		if len(raw) > 900000 {
			detail.Calls = detail.Calls[:len(detail.Calls)-1]
			detail.Truncated = true
			break
		}
	}
	raw, _ := json.Marshal(detail)
	return raw
}

// saveExecutionDiagnostics retains stacks and payloads separately from caller-facing messages and telemetry.
func (s *EngineGRPCServer) saveExecutionDiagnostics(ctx context.Context, spec capabilityRunSpec, id uuid.UUID, recorder *recordingCapabilityHost, runErr error) {
	repository, ok := s.store.(store.ExecutionDiagnosticsStore)
	// Lightweight test stores and older installations report no diagnostics rather than leaking a fallback.
	if !ok {
		return
	}
	// Diagnostic persistence failure cannot disclose details or turn a committed execution into a retry.
	if err := repository.SaveExecutionDiagnostics(ctx, spec.identity.AccountID, spec.identity.AppID, id, recorder.diagnostics(runErr), s.masterKey); err != nil {
		trace.SpanFromContext(ctx).SetStatus(codes.Error, "execution_diagnostics_unavailable")
	}
}

// ExecutionDiagnosticsHandler requires a live management actor and a dedicated family-scoped grant on every read.
func ExecutionDiagnosticsHandler(s store.Store, key []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.execution.diagnostics.read")
		defer span.End()
		w.Header().Set("Cache-Control", "no-store")
		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		// Runtime execution tokens and unrelated accounts cannot use the internal diagnostic route.
		// Permission and ownership failures stop before retrieving retained payloads.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		ref := accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionUnifiedAppDiagnosticsRead, Resource: ref})
		// Permission and ownership failures stop before retrieving retained payloads.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "execution_id"))
		// Malformed identifiers never become storage selectors.
		if err != nil {
			http.Error(w, "invalid execution ID", 400)
			return
		}
		repository, ok := s.(store.ExecutionDiagnosticsStore)
		results, resultsOK := s.(store.ExecutionResultStore)
		// No fallback may expose data from an installation without private storage.
		if !ok || !resultsOK {
			http.Error(w, "diagnostics unavailable", 503)
			return
		}
		record, err := results.GetExecutionResult(ctx, actor.AccountID, app.AppID, id)
		// Result ownership and expiry remain authoritative even for privileged actors.
		if err != nil {
			http.Error(w, "execution unavailable", 404)
			return
		}
		detail, err := repository.GetExecutionDiagnostics(ctx, actor.AccountID, app.AppID, id, key)
		// Historical results without captured details are explicit, never invented or reconstructed by rerunning code.
		if err != nil {
			http.Error(w, "diagnostics were not captured or have expired", 404)
			return
		}
		// Record access identity without copying request, response, or error values into telemetry.
		span.SetAttributes(attribute.String("app.id", app.AppID.String()), attribute.String("app.family_id", app.AppFamilyID.String()), attribute.String("actor.subject_id", actor.SubjectID.String()))
		span.SetStatus(codes.Ok, "read")
		writeJSON(w, struct {
			ExecutionID uuid.UUID       `json:"execution_id"`
			Version     string          `json:"version"`
			Request     json.RawMessage `json:"request"`
			Response    json.RawMessage `json:"response,omitempty"`
			Diagnostics json.RawMessage `json:"diagnostics"`
		}{id, record.AppVersion, record.Input, record.Output, detail})
	}
}
