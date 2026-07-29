package sandbox

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// idempotencyStore is the minimal slice of store.Store that the cache-and-
// replay path needs -- mirrors executionAuditStore's pattern (worker package)
// of depending on a narrow interface rather than the full Store, wired via a
// package-level setter so the deeply-nested engineExecuteCore doesn't need
// the store threaded through every call site.
type idempotencyStore interface {
	GetIdempotentExecution(ctx context.Context, artifactID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error)
	SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error
}

var globalIdempotencyStore idempotencyStore

// idempotencyReplayedAttr mirrors models.EngineExecutionEvent.IdempotencyReplayed
// onto the OTEL span for this execution, so a cache-hit is visible in traces
// the same way it's visible in the persisted audit record -- debugging a
// "why didn't this call reach the vendor" question shouldn't require a DB query.
var idempotencyReplayedAttr = attribute.Bool("idempotency_replayed", true)

// SetIdempotencyStore wires the store used for idempotent-execution
// cache-and-replay. Called once at boot (see cmd/engine/cmd/start.go),
// alongside SetExecutionAuditRecorder. Until this is called, idempotency
// caching is a no-op and every call dispatches to the vendor normally.
func SetIdempotencyStore(s idempotencyStore) {
	globalIdempotencyStore = s
}

// idempotencyEligible reports whether a call can be cached and replayed at
// all. Only the plain request/response path qualifies -- SOAP, SSE, and
// paginated calls are stateful/multi-chunk and don't fit "cache one
// response", so they're excluded regardless of whether an idempotency key is
// present. A missing idempotency key also disqualifies a call: there's
// nothing to key the cache on.
func idempotencyEligible(ctx context.Context, obj *models.IntegrationObject) bool {
	if idempotencyKeyFromContext(ctx) == "" {
		return false
	}
	if obj == nil {
		return false
	}
	return obj.Method != "SOAP" && !obj.IsSSE && obj.Pagination == nil
}

// lookupCachedExecution returns a cached response for the current context's
// idempotency key, if one exists and is unexpired. ok=false with a nil error
// means "no cache entry, dispatch normally". A non-nil error (e.g. the
// idempotency key was reused with a different request body) means the
// caller should fail the request without dispatching.
func lookupCachedExecution(ctx context.Context, artifactID uuid.UUID) (exec *models.IdempotentExecution, ok bool, err error) {
	if globalIdempotencyStore == nil {
		return nil, false, nil
	}
	keyHash := hashExecutionValue(idempotencyKeyFromContext(ctx))
	if keyHash == "" {
		return nil, false, nil
	}
	bodyHash := requestBodyHashFromContext(ctx)
	found, err := globalIdempotencyStore.GetIdempotentExecution(ctx, artifactID, keyHash, bodyHash)
	if err != nil {
		if errors.Is(err, store.ErrIdempotentExecutionNotFound) {
			return nil, false, nil
		}
		if errors.Is(err, store.ErrIdempotencyKeyConflict) {
			return nil, false, errors.New("idempotency key already used with a different request body")
		}
		// Store errors (e.g. a transient DB blip) shouldn't block execution --
		// fall through to a normal dispatch rather than fail the call.
		return nil, false, nil
	}
	return found, true, nil
}

// saveCachedExecution persists a successful execution's response for later
// replay. Only called after a successful (execErr == nil) dispatch -- a
// transient failure is never cached, so it can't get "stuck" replaying an
// error for the TTL window. Best-effort: a save failure is not surfaced to
// the caller, who already has their real response.
func saveCachedExecution(ctx context.Context, artifactID uuid.UUID, responseBody []byte, environment string) {
	if globalIdempotencyStore == nil {
		return
	}
	keyHash := hashExecutionValue(idempotencyKeyFromContext(ctx))
	if keyHash == "" {
		return
	}
	now := time.Now()
	_ = globalIdempotencyStore.SaveIdempotentExecution(ctx, &models.IdempotentExecution{
		ID:                 uuid.New(),
		ArtifactID:         artifactID,
		IdempotencyKeyHash: keyHash,
		RequestBodyHash:    requestBodyHashFromContext(ctx),
		Environment:        environment,
		ResponseBody:       responseBody,
		CreatedAt:          now,
		ExpiresAt:          now.Add(models.IdempotencyTTL),
	})
}

// tryReplayFromIdempotencyCache is engineExecuteCore's first move once the
// endpoint is resolved: if this call is eligible and a cached response
// exists, serve it and skip the vendor entirely. Split out from
// engineExecuteCore (separation of concerns, and keeps cyclomatic complexity
// of both functions manageable) so "can we skip the vendor" is one isolated
// decision with its own error handling, distinct from "how do we dispatch".
//
// replayed=true means the caller is fully handled (streamed + audited) and
// engineExecuteCore should return nil immediately. A non-nil err means the
// caller should fail without ever dispatching (e.g. the idempotency key was
// reused with a different request body) -- span is annotated here since this
// is a user/agent-triggered execution path and every outcome must be
// traceable for debugging/audit, matching the span already opened for the
// vendor-dispatch path.
func tryReplayFromIdempotencyCache(
	ctx context.Context,
	span trace.Span,
	artifactID uuid.UUID,
	obj *models.IntegrationObject,
	stream engine.ResponseStream,
	auditState *executionAuditState,
) (replayed bool, err error) {
	if !idempotencyEligible(ctx, obj) {
		return false, nil
	}
	cached, hit, lookupErr := lookupCachedExecution(ctx, artifactID)
	if lookupErr != nil {
		span.SetStatus(codes.Error, lookupErr.Error())
		return false, lookupErr
	}
	if !hit {
		return false, nil
	}
	auditState.idempotencyReplayed = true
	auditState.selectedEnvironment = cached.Environment
	if sendErr := stream.Send(cached.ResponseBody); sendErr != nil {
		span.SetStatus(codes.Error, sendErr.Error())
		return false, sendErr
	}
	span.SetAttributes(idempotencyReplayedAttr)
	span.SetStatus(codes.Ok, "tool call replayed from idempotency cache")
	return true, nil
}

// dispatchAndCache runs the normal vendor dispatch and, when this call is
// idempotency-eligible, tees the response into the cache on success so a
// future retry or duplicate with the same key can replay it instead of
// hitting the vendor again. Splitting this out of engineExecuteCore keeps
// "how do we dispatch and remember the result" as one cohesive unit distinct
// from scope resolution and cache lookup.
func dispatchAndCache(
	ctx context.Context,
	dispatcher *engine.Dispatcher,
	match *scopedEndpoint,
	obj *models.IntegrationObject,
	params, credentials map[string]any,
	bucketValues []store.BucketValue,
	environment string,
	stream engine.ResponseStream,
	span trace.Span,
	artifactID uuid.UUID,
) (selectedEnvironment string, err error) {
	eligible := idempotencyEligible(ctx, obj)
	dispatchStream := stream
	var tee *teeResponseStream
	if eligible {
		tee = &teeResponseStream{inner: stream}
		dispatchStream = tee
	}
	selectedEnvironment, err = dispatchRuntimeEnvironment(ctx, dispatcher, match, obj, params, credentials, bucketValues, environment, dispatchStream, span)
	if err != nil {
		return selectedEnvironment, err
	}
	if tee != nil {
		saveCachedExecution(ctx, artifactID, tee.Bytes(), selectedEnvironment)
	}
	return selectedEnvironment, nil
}

// teeResponseStream forwards every chunk to the real stream while also
// accumulating it, so a successful execution's full response body can be
// captured for the idempotency cache without changing what the caller (gRPC
// edge or MCP buffer) actually receives.
type teeResponseStream struct {
	inner engine.ResponseStream
	buf   bytes.Buffer
}

func (t *teeResponseStream) Send(chunk []byte) error {
	t.buf.Write(chunk)
	return t.inner.Send(chunk)
}

func (t *teeResponseStream) Bytes() []byte { return t.buf.Bytes() }
