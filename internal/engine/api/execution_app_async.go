package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type capabilityAsyncOutcome struct {
	result capabilityExecutionEnvelope
	err    *restExecutionError
}

// capabilityWaitBudget accepts a bounded Prefer wait or immediate asynchronous response.
func capabilityWaitBudget(request *http.Request) (time.Duration, bool, *restExecutionError) {
	values := request.Header.Values("Prefer")
	// Ordinary calls retain synchronous behavior unless the caller requests a bounded wait.
	if len(values) == 0 {
		return 0, false, nil
	}
	var wait time.Duration
	requested := false
	hasWait := false
	for _, value := range values {
		for _, term := range strings.Split(value, ",") {
			term = strings.TrimSpace(term)
			// RFC respond-async asks for the durable ID before provider work finishes.
			if term == "respond-async" {
				requested = true
				// An explicit wait preference keeps its budget regardless of header ordering.
				if !hasWait {
					wait = 0
				}
				continue
			}
			// A caller can wait for up to thirty seconds before receiving the pending handle.
			if strings.HasPrefix(term, "wait=") {
				seconds, err := strconv.Atoi(strings.TrimPrefix(term, "wait="))
				if err != nil || seconds < 0 || seconds > 30 {
					return 0, false, newRESTExecutionError(http.StatusBadRequest, "invalid_preference", "wait must be between 0 and 30 seconds")
				}
				requested = true
				hasWait = true
				wait = time.Duration(seconds) * time.Second
			}
		}
	}
	return wait, requested, nil
}

// executeCapabilityRunWithWait returns one retained handle while the detached worker finishes independently.
func (s *EngineGRPCServer) executeCapabilityRunWithWait(ctx context.Context, spec capabilityRunSpec, wait time.Duration) (capabilityExecutionEnvelope, *restExecutionError) {
	admitted, requestErr := s.admitCapabilityRun(ctx, spec)
	if requestErr != nil {
		return capabilityExecutionEnvelope{}, requestErr
	}
	// Duplicate reruns have already reserved their work and never start another worker.
	if !admitted.created {
		return loadCapabilityRunEnvelope(ctx, admitted.results, spec.identity, admitted.id, "")
	}
	workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	done := make(chan capabilityAsyncOutcome, 1)
	go func() {
		defer cancel()
		workerCtx, span := otel.Tracer("engine").Start(workerCtx, "engine.execution_app.worker")
		defer span.End()
		span.SetAttributes(attribute.String("execution.id", admitted.id.String()), attribute.String("execution.mode", spec.mode))
		result, err := s.runAdmittedCapability(workerCtx, spec, admitted)
		// A detached failure still receives a bounded OTEL outcome after the request returns.
		if err != nil {
			span.SetStatus(codes.Error, err.code)
		} else {
			span.SetAttributes(attribute.String("execution.status", result.Status))
		}
		done <- capabilityAsyncOutcome{result: result, err: err}
	}()
	// Zero wait returns the durable reservation without waiting for external effects.
	if wait == 0 {
		return loadCapabilityRunEnvelope(ctx, admitted.results, spec.identity, admitted.id, admitted.readHandle)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case outcome := <-done:
		return outcome.result, outcome.err
	case <-timer.C:
		return loadCapabilityRunEnvelope(ctx, admitted.results, spec.identity, admitted.id, admitted.readHandle)
	case <-ctx.Done():
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusRequestTimeout, "request_cancelled", "request ended before its wait budget")
	}
}
