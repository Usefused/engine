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
	ReserveIdempotentExecution(context.Context, *models.IdempotentExecution) error
	ReleaseIdempotentExecution(context.Context, uuid.UUID) error
	RetainIdempotentExecution(context.Context, uuid.UUID, uuid.UUID) error
}

var globalIdempotencyStore idempotencyStore

// IdempotencyRejectionError proves this request was blocked before provider dispatch.
// A persistence error after dispatch must not claim the provider was never called.
type IdempotencyRejectionError struct{ Cause error }

// Error retains the underlying diagnostic while transports project safe public guidance.
func (e *IdempotencyRejectionError) Error() string { return e.Cause.Error() }

// Unwrap preserves the store decision for errors.Is across execution boundaries.
func (e *IdempotencyRejectionError) Unwrap() error { return e.Cause }

// idempotencyRejection marks only known replay gates, leaving storage outages unclassified.
func idempotencyRejection(err error) error {
	// Only these decisions prove an existing key prevented this provider dispatch.
	if errors.Is(err, store.ErrIdempotencyKeyConflict) || errors.Is(err, store.ErrIdempotencyInProgress) {
		return &IdempotencyRejectionError{Cause: err}
	}
	return err
}

// idempotencyReplayedAttr mirrors models.EngineExecutionEvent.IdempotencyReplayed
// onto the OTEL span for this execution, so a cache-hit is visible in traces
// the same way it's visible in the persisted audit record -- debugging a
// "why didn't this call reach the vendor" question shouldn't require a DB query.
var idempotencyReplayedAttr = attribute.Bool("idempotency_replayed", true)

// SetIdempotencyStore wires the store used for idempotent-execution
// cache-and-replay. Called once at boot (see cmd/engine/cmd/start.go),
// alongside SetExecutionAuditRecorder. Keyed calls fail closed until storage is available.
func SetIdempotencyStore(s idempotencyStore) {
	globalIdempotencyStore = s
}

// idempotencyEligible reports whether a call can be cached and replayed at
// all. SOAP and paginated calls cannot be represented by one cached response.
// SSE eligibility is decided only after the provider's actual status/media is
// matched; the tee then discards streamed bytes while finite alternatives on
// the same operation remain cacheable.
func idempotencyEligible(ctx context.Context, obj *models.IntegrationObject) bool {
	// Only explicit or snapshot-supplied keys activate deduplication.
	if idempotencyKeyFromContext(ctx) == "" {
		return false
	}
	// No resolved operation means no safe cached-response shape.
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
	// Keyed calls cannot safely bypass unavailable persistence.
	if globalIdempotencyStore == nil {
		return nil, false, errors.New("idempotency storage unavailable")
	}
	keyHash := hashExecutionValue(idempotencyKeyFromContext(ctx))
	// Empty keys preserve ordinary non-deduplicated execution.
	if keyHash == "" {
		return nil, false, nil
	}
	bodyHash := requestBodyHashFromContext(ctx)
	found, err := globalIdempotencyStore.GetIdempotentExecution(ctx, appID, keyHash, bodyHash)
	// Only an authoritative miss permits dispatch.
	if err != nil {
		// Not-found is distinct from unavailable or uncertain state.
		if errors.Is(err, store.ErrIdempotentExecutionNotFound) {
			return nil, false, nil
		}
		return nil, false, idempotencyRejection(err)
	}
	// A shared business key must remain available for every run that consumes its response.
	if err := retainCachedExecution(ctx, found); err != nil {
		return nil, false, err
	}
	return found, true, nil
}

// reserveCachedExecution establishes durable ownership before the provider receives a request.
func reserveCachedExecution(ctx context.Context, appID uuid.UUID) (*models.IdempotentExecution, error) {
	now := time.Now()
	record := &models.IdempotentExecution{ID: uuid.New(), AppID: appID, IdempotencyKeyHash: hashExecutionValue(idempotencyKeyFromContext(ctx)), RequestBodyHash: requestBodyHashFromContext(ctx), CreatedAt: now, ExpiresAt: now.Add(models.IdempotencyTTL)}
	runID, ok := ctx.Value(idempotencyRunKey{}).(uuid.UUID)
	// Logical-run retention overrides the standalone cache lifetime without copying provider responses.
	if ok {
		record.RunID = &runID
	}
	return record, idempotencyRejection(globalIdempotencyStore.ReserveIdempotentExecution(ctx, record))
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
	// Unsupported call shapes cannot silently bypass an automatic checkpoint.
	if err := validateSnapshotProvider(ctx, obj); err != nil {
		return false, err
	}
	// Calls without a key keep the existing dispatcher behavior.
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

// dispatchAndCache buffers keyed finite responses until their durable result is acknowledged.
func dispatchAndCache(ctx context.Context, dispatcher *engine.Dispatcher, match *scopedEndpoint, obj *models.IntegrationObject,
	params, credentials map[string]any, bucketValues []store.BucketValue, environment string, stream engine.ResponseStream, span trace.Span, appID uuid.UUID,
) (resolution RuntimeEnvironmentResolution, providerHTTPStatus int, err error) {
	// Unkeyed or unsupported ordinary calls retain their normal streaming path.
	if !idempotencyEligible(ctx, obj) {
		return dispatchRuntimeEnvironment(ctx, dispatcher, match, obj, params, credentials, bucketValues, environment, stream, span)
	}
	record, err := reserveCachedExecution(ctx, appID)
	// A concurrent owner, unavailable store, or unknown previous outcome must not reach the provider.
	if err != nil {
		return resolution, 0, err
	}
	_, automatic := ctx.Value(idempotencyRunKey{}).(uuid.UUID)
	tee := &teeResponseStream{inner: stream, cacheable: true, requireFinite: automatic}
	resolution, providerHTTPStatus, err = dispatchRuntimeEnvironment(ctx, dispatcher, match, obj, params, credentials, bucketValues, environment, tee, span)
	return resolution, providerHTTPStatus, completeCachedDispatch(ctx, record, obj, tee, resolution.Environment, providerHTTPStatus, err)
}

// completeCachedDispatch keeps uncertain writes reserved and publishes finite success only after persistence.
func completeCachedDispatch(ctx context.Context, record *models.IdempotentExecution, obj *models.IntegrationObject, tee *teeResponseStream, environment string, status int, dispatchErr error) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Failed read-only requests and live streams can safely release their reservation.
	if dispatchErr != nil || !tee.Cacheable() || status < 200 || status >= 400 {
		// Mutating requests with unknown outcomes must remain fenced even after an Engine restart.
		if !tee.Cacheable() || safeIdempotentRetry(obj) {
			if err := globalIdempotencyStore.ReleaseIdempotentExecution(finishCtx, record.ID); err != nil {
				return err
			}
		}
		// A failed dispatch has no acknowledged response to publish.
		if dispatchErr != nil {
			return dispatchErr
		}
		return tee.flush(status)
	}
	record.Environment, record.ResponseStatus, record.ResponseMediaFamily = environment, status, tee.MediaFamily()
	record.ResponseBody = tee.Bytes()
	// This write is the acknowledgement boundary; losing it leaves the original reservation intact.
	if err := globalIdempotencyStore.SaveIdempotentExecution(finishCtx, record); err != nil {
		return err
	}
	return tee.flush(status)
}

// teeResponseStream holds finite bytes until commit while allowing ordinary SSE streams to remain live.
type teeResponseStream struct {
	inner         engine.ResponseStream
	buf           bytes.Buffer
	mediaFamily   string
	cacheable     bool
	requireFinite bool
}

// Send bounds pending response memory and withholds bytes until persistence succeeds.
func (t *teeResponseStream) Send(chunk []byte) error {
	// Live SSE is deliberately outside the finite-response cache contract.
	if !t.cacheable {
		return t.inner.Send(chunk)
	}
	// A bounded buffer cannot exhaust Engine memory on an unexpected provider response.
	if t.buf.Len()+len(chunk) > 16<<20 {
		return errors.New("idempotent response exceeds 16 MiB")
	}
	_, err := t.buf.Write(chunk)
	return err
}

// SendStatus defers finite status delivery until the response has committed.
func (t *teeResponseStream) SendStatus(status int) error {
	// Streaming calls retain their immediate transport status.
	if !t.cacheable {
		return engine.SendResponseStatus(t.inner, status)
	}
	return nil
}

// SendResponseContract switches to live transport only for an actual SSE response.
func (t *teeResponseStream) SendResponseContract(status int, mediaFamily string) error {
	t.mediaFamily = mediaFamily
	// SSE is unbounded and cannot be represented by a stored finite result.
	if mediaFamily == "sse" {
		// An undeclared streaming response cannot bypass automatic checkpoint acknowledgement.
		if t.requireFinite {
			return errors.New("snapshot provider returned an unsupported stream")
		}
		t.cacheable = false
		t.buf.Reset()
		return engine.SendResponseContract(t.inner, status, mediaFamily)
	}
	return nil
}

// flush preserves the original response contract, body, and status ordering after commit.
func (t *teeResponseStream) flush(status int) error {
	// Live streams have already delivered their bytes.
	if !t.cacheable {
		return nil
	}
	// Contract delivery must precede bytes for typed consumers.
	if err := engine.SendResponseContract(t.inner, status, t.MediaFamily()); err != nil {
		return err
	}
	// A disconnected caller may retry the already committed result.
	if err := t.inner.Send(t.Bytes()); err != nil {
		return err
	}
	return engine.SendResponseStatus(t.inner, status)
}

// Bytes exposes the finite response only to the synchronous persistence boundary.
func (t *teeResponseStream) Bytes() []byte { return t.buf.Bytes() }

// Cacheable distinguishes finite responses from live streams.
func (t *teeResponseStream) Cacheable() bool { return t.cacheable }

// MediaFamily preserves the contract for providers without explicit response metadata.
func (t *teeResponseStream) MediaFamily() string {
	// Historical providers use the existing unknown-media contract.
	if t.mediaFamily == "" {
		return "unknown"
	}
	return t.mediaFamily
}

// safeIdempotentRetry recognizes only contract-declared reads whose repetition cannot write provider state.
func safeIdempotentRetry(obj *models.IntegrationObject) bool {
	return obj.Method == "GET" || obj.Method == "HEAD" || obj.Method == "OPTIONS" || (obj.ProviderProtocol == models.ProviderProtocolGraphQL && obj.OperationKind == models.OperationKindQuery)
}

// retainCachedExecution adds retention metadata only when an authored key is shared across logical runs.
func retainCachedExecution(ctx context.Context, record *models.IdempotentExecution) error {
	run, automatic := ctx.Value(idempotencyRunKey{}).(uuid.UUID)
	// The original run already owns retention; ordinary callers keep the existing standalone TTL.
	if !automatic || (record.RunID != nil && *record.RunID == run) {
		return nil
	}
	return globalIdempotencyStore.RetainIdempotentExecution(ctx, record.ID, run)
}
