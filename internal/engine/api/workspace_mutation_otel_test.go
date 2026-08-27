package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestWorkspaceServiceAdmissionEmitsMutationSpans proves add/remove attempts
// use live OTEL spans even when authentication rejects them before storage.
func TestWorkspaceServiceAdmissionEmitsMutationSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// Cleanup restores process-global tracing so unrelated package tests remain isolated.
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(t.Context())
	})

	tests := []struct {
		name, method, path, spanName string
		handler                      http.Handler
	}{
		{name: "add", method: http.MethodPost, path: "/workspace/services", spanName: "engine.workspace.add_service", handler: addServiceHandler(nil, nil)},
		{name: "remove", method: http.MethodDelete, path: "/workspace/services/not-a-uuid", spanName: "engine.workspace.remove_service", handler: removeServiceHandler(nil)},
	}
	// Both workspace-service mutation entry points must cover pre-admission failure.
	for _, test := range tests {
		// Each case receives isolated span-count assertions against the shared recorder.
		t.Run(test.name, func(t *testing.T) {
			before := len(recorder.Ended())
			response := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)

			test.handler.ServeHTTP(response, request)

			ended := recorder.Ended()
			// Each rejected mutation owns exactly one newly ended request span.
			if len(ended) != before+1 {
				t.Fatalf("ended spans = %d, want %d", len(ended), before+1)
			}
			span := ended[len(ended)-1]
			attributes := map[string]string{}
			// Attribute projection keeps the assertion independent of exporter ordering.
			for _, item := range span.Attributes() {
				attributes[string(item.Key)] = item.Value.AsString()
			}
			// Authentication denial is stable and proves no mutation committed.
			if span.Name() != test.spanName || attributes["control.mutation.error_code"] != "authentication_required" || attributes["control.mutation.commit_state"] != "not_committed" {
				t.Fatalf("workspace mutation span = %q %#v", span.Name(), attributes)
			}
		})
	}
}

// TestWorkspaceBucketSecretMutationSuccessSpanIncludesOutcome verifies generic workspace secrets retain their stable audit result.
func TestWorkspaceBucketSecretMutationSuccessSpanIncludesOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// Cleanup restores the process tracer after the mutation span ends.
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(t.Context())
	})

	repository := &workspaceTestStore{}
	if err := upsertWorkspaceBucketSecrets(t.Context(), repository, []store.WorkspaceSecret{{}}); err != nil {
		t.Fatalf("upsert bucket secrets: %v", err)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("credential mutation spans = %d, want 1", len(spans))
	}
	for _, span := range spans {
		attributes := map[string]string{}
		// Exporter order is irrelevant; only the fixed outcome is required here.
		for _, item := range span.Attributes() {
			attributes[string(item.Key)] = item.Value.AsString()
		}
		if attributes["outcome"] != "success" || span.Status().Code != codes.Ok {
			t.Fatalf("credential mutation span %q = %#v status=%#v", span.Name(), attributes, span.Status())
		}
	}
}
