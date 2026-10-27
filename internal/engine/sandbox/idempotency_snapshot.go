package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type idempotencyRunKey struct{}

// WithIdempotencyRun links response retention to an Engine-admitted logical run, never a caller header.
func WithIdempotencyRun(ctx context.Context, runID uuid.UUID) context.Context {
	return context.WithValue(ctx, idempotencyRunKey{}, runID)
}

// validateSnapshotProvider rejects call shapes the finite provider result store cannot replay faithfully.
func validateSnapshotProvider(ctx context.Context, obj *models.IntegrationObject) error {
	_, automatic := ctx.Value(idempotencyRunKey{}).(uuid.UUID)
	// Standalone execution keeps its historical streaming and pagination contract.
	if !automatic {
		return nil
	}
	// Snapshot mode must never pretend unsupported provider effects were checkpointed.
	if !idempotencyEligible(ctx, obj) {
		return errors.New("snapshot requires a finite non-SOAP, non-paginated provider operation")
	}
	for _, response := range obj.Responses {
		for _, representation := range response.Representations {
			// Reject streaming alternatives before dispatch, including mixed JSON/SSE contracts.
			if representation.MediaType == "text/event-stream" {
				return errors.New("snapshot does not support streaming provider operations")
			}
		}
	}
	return nil
}

// bindIdempotentRequest fingerprints admitted operation, input, credentials selectors, and bucket routing server-side.
func bindIdempotentRequest(ctx context.Context, identity auth.RuntimeIdentity, match *scopedEndpoint, request PhysicalExecutionRequest) (context.Context, error) {
	// No key means no hashing or persistence overhead on the ordinary path.
	if idempotencyKeyFromContext(ctx) == "" {
		return ctx, nil
	}
	bucket, _ := ctx.Value(unifiedAppBucketKey{}).(unifiedAppBucket)
	raw, err := json.Marshal(struct {
		CallerHash string
		Service    uuid.UUID
		Version    string
		Endpoint   uuid.UUID
		Token      uuid.UUID
		Bucket     *uuid.UUID
		Request    PhysicalExecutionRequest
	}{requestBodyHashFromContext(ctx), match.service.ID, match.serviceVersionID, match.endpoint.ID, identity.TokenID, bucket.bucketID, request})
	// Unrepresentable input cannot authorize reuse of a cached response.
	if err != nil {
		return ctx, err
	}
	return contextWithExecutionIdentity(ctx, idempotencyKeyFromContext(ctx), hashExecutionValue(string(raw))), nil
}
