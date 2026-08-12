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
	GetIdempotentExecution(ctx context.Context, appID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error)
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
// all. SOAP and paginated calls cannot be represented by one cached response.
// SSE eligibility is decided only after the provider's actual status/media is
// matched; the tee then discards streamed bytes while finite alternatives on
// the same operation remain cacheable.
func idempotencyEligible(ctx context.Context, obj *models.IntegrationObject) bool {
	if idempotencyKeyFromContext(ctx) == "" {
		return false
	}
	if obj == nil {
		return false
	}
	return obj.Method != "SOAP" && obj.Pagination == nil
}

// lookupCachedExecution returns a cached response for the current context's
// idempotency key, if one exists and is unexpired. ok=false with a nil error
// means "no cache entry, dispatch normally". A non-nil error (e.g. the
// idempotency key was reused with a different request body) means the
// caller should fail the request without dispatching.
func lookupCachedExecution(ctx context.Context, appID uuid.UUID) (exec *models.IdempotentExecution, ok bool, err error) {
	if globalIdempotencyStore == nil {
		return nil, false, nil
	}
	keyHash := hashExecutionValue(idempotencyKeyFromContext(ctx))
	if keyHash == "" {
		return nil, false, nil
	}
	bodyHash := requestBodyHashFromContext(ctx)
	found, err := globalIdempotencyStore.GetIdempotentExecution(ctx, appID, keyHash, bodyHash)
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
func saveCachedExecution(ctx context.Context, appID uuid.UUID, responseBody []byte, environment string, responseStatus int, responseMediaFamily string) {
	if globalIdempotencyStore == nil {
		return
	}
	keyHash := hashExecutionValue(idempotencyKeyFromContext(ctx))
	if keyHash == "" {
		return
	}
	now := time.Now()
	_ = globalIdempotencyStore.SaveIdempotentExecution(ctx, &models.IdempotentExecution{
		ID:                  uuid.New(),
		AppID:               appID,
		IdempotencyKeyHash:  keyHash,
		RequestBodyHash:     requestBodyHashFromContext(ctx),
		Environment:         environment,
		ResponseBody:        responseBody,
		ResponseStatus:      responseStatus,
		ResponseMediaFamily: responseMediaFamily,
		CreatedAt:           now,
		ExpiresAt:           now.Add(models.IdempotencyTTL),
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
	appID uuid.UUID,
	obj *models.IntegrationObject,
	stream engine.ResponseStream,
	auditState *executionAuditState,
) (replayed bool, err error) {
	if !idempotencyEligible(ctx, obj) {
		return false, nil
	}
	cached, hit, lookupErr := lookupCachedExecution(ctx, appID)
	if lookupErr != nil {
		span.SetStatus(codes.Error, lookupErr.Error())
		return false, lookupErr
	}
	if !hit {
		return false, nil
	}
	auditState.idempotencyReplayed = true
	auditState.selectedEnvironment = cached.Environment
	auditState.providerHTTPStatus = cached.ResponseStatus
	if contractErr := engine.SendResponseContract(stream, cached.ResponseStatus, cached.ResponseMediaFamily); contractErr != nil {
		span.SetStatus(codes.Error, contractErr.Error())
		return false, contractErr
	}
	if sendErr := stream.Send(cached.ResponseBody); sendErr != nil {
		span.SetStatus(codes.Error, sendErr.Error())
		return false, sendErr
	}
	if statusErr := engine.SendResponseStatus(stream, cached.ResponseStatus); statusErr != nil {
		span.SetStatus(codes.Error, statusErr.Error())
		return false, statusErr
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
	appID uuid.UUID,
) (resolution RuntimeEnvironmentResolution, providerHTTPStatus int, err error) {
	eligible := idempotencyEligible(ctx, obj)
	dispatchStream := stream
	var tee *teeResponseStream
	if eligible {
		tee = &teeResponseStream{inner: stream, cacheable: true}
		dispatchStream = tee
	}
	resolution, providerHTTPStatus, err = dispatchRuntimeEnvironment(ctx, dispatcher, match, obj, params, credentials, bucketValues, environment, dispatchStream, span)
	if err != nil {
		return resolution, providerHTTPStatus, err
	}
	if tee != nil && tee.Cacheable() && providerHTTPStatus >= 200 && providerHTTPStatus < 400 {
		saveCachedExecution(ctx, appID, tee.Bytes(), resolution.Environment, providerHTTPStatus, tee.MediaFamily())
	}
	return resolution, providerHTTPStatus, nil
}

// teeResponseStream forwards every chunk to the real stream while also
// accumulating it, so a successful execution's full response body can be
// captured for the idempotency cache without changing what the caller (gRPC
// edge or MCP buffer) actually receives.
type teeResponseStream struct {
	inner       engine.ResponseStream
	buf         bytes.Buffer
	mediaFamily string
	cacheable   bool
}

func (t *teeResponseStream) Send(chunk []byte) error {
	if t.cacheable {
		t.buf.Write(chunk)
	}
	return t.inner.Send(chunk)
}

func (t *teeResponseStream) SendStatus(status int) error {
	return engine.SendResponseStatus(t.inner, status)
}

func (t *teeResponseStream) SendResponseContract(status int, mediaFamily string) error {
	t.mediaFamily = mediaFamily
	if mediaFamily == "sse" {
		// Runtime media selection is authoritative for mixed operations. SSE is
		// live and potentially unbounded, so it must never enter the one-body
		// idempotency cache.
		t.cacheable = false
		t.buf.Reset()
	}
	return engine.SendResponseContract(t.inner, status, mediaFamily)
}

func (t *teeResponseStream) Bytes() []byte   { return t.buf.Bytes() }
func (t *teeResponseStream) Cacheable() bool { return t.cacheable }
func (t *teeResponseStream) MediaFamily() string {
	if t.mediaFamily == "" {
		return "unknown"
	}
	return t.mediaFamily
}
