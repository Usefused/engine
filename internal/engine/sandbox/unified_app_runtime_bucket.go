package sandbox

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type unifiedAppBucketKey struct{}
type unifiedAppBucket struct {
	appID    uuid.UUID
	bucketID *uuid.UUID
}

// WithUnifiedAppBucket binds routing to one invocation and exact app; child runs replace this context value.
func WithUnifiedAppBucket(ctx context.Context, appID uuid.UUID, bucketID *uuid.UUID) context.Context {
	return context.WithValue(ctx, unifiedAppBucketKey{}, unifiedAppBucket{appID: appID, bucketID: bucketID})
}

// resolveUnifiedAppExecutionBucket repeats live authorization at credential use without changing shared scope.
func resolveUnifiedAppExecutionBucket(ctx context.Context, repository store.Store, familyID uuid.UUID, request CredentialRequest) (*store.AppFamilyBucket, error) {
	selected, ok := ctx.Value(unifiedAppBucketKey{}).(unifiedAppBucket)
	// Ordinary SDK/MCP calls and child apps retain their own configured buckets.
	if !ok || selected.appID != request.AppID || selected.bucketID == nil {
		return repository.ResolveAppFamilyServiceBucket(ctx, familyID, request.ServiceID)
	}
	authority, ok := repository.(store.UnifiedAppRuntimeBucketStore)
	// Neither stale caches nor a missing database boundary can authorize alternate credentials.
	if !ok {
		return nil, errors.New("runtime bucket authorization unavailable")
	}
	allowed, err := authority.AuthorizeUnifiedAppRuntimeBucket(ctx, request.AccountID, request.AppID, request.TokenID, *selected.bucketID)
	if err != nil {
		return nil, errors.New("runtime bucket authorization unavailable")
	}
	if !allowed {
		return nil, errors.New("runtime bucket selection denied")
	}
	return &store.AppFamilyBucket{BucketID: *selected.bucketID}, nil
}
