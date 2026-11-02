package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/Usefused/engine/internal/engine/auth"
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

// bindGRPCRuntimeBucket accepts one canonical selection without changing a caller's shared context.
func bindGRPCRuntimeBucket(ctx context.Context, appID, rawBucket string) (context.Context, *uuid.UUID, error) {
	// Proto omission preserves the existing app's configured credential routing.
	if rawBucket == "" {
		return ctx, nil, nil
	}
	bucketID, bucketErr := uuid.Parse(rawBucket)
	exactAppID, appErr := uuid.Parse(appID)
	// Explicit routing requires real UUIDs and a stable textual identity for replay hashing.
	if bucketErr != nil || appErr != nil || bucketID == uuid.Nil || exactAppID == uuid.Nil || bucketID.String() != rawBucket {
		return ctx, nil, errors.New("runtime bucket selection is invalid")
	}
	return WithUnifiedAppBucket(ctx, exactAppID, &bucketID), &bucketID, nil
}

// bindRuntimeBucketRequestHash prevents identical idempotency keys from replaying across credential buckets.
func bindRuntimeBucketRequestHash(requestHash string, bucketID *uuid.UUID) string {
	// Legacy callers retain their existing replay identity when no override is supplied.
	if bucketID == nil {
		return requestHash
	}
	digest := sha256.Sum256([]byte("fused.runtime.bucket.v1\x00" + requestHash + "\x00" + bucketID.String()))
	return hex.EncodeToString(digest[:])
}

type runtimeBucketAuthorizer interface {
	authorizeSelectedRuntimeBucket(context.Context, auth.RuntimeIdentity, uuid.UUID) error
}

// authorizeRuntimeBucketBeforeReplay rejects stale bucket authority even when execution would return a cached result.
func authorizeRuntimeBucketBeforeReplay(ctx context.Context, identity auth.RuntimeIdentity) error {
	selected, ok := ctx.Value(unifiedAppBucketKey{}).(unifiedAppBucket)
	// Child apps and calls without an override retain their own static credentials.
	if !ok || selected.appID != identity.AppID || selected.bucketID == nil {
		return nil
	}
	resolver, ok := globalSecretResolver.(runtimeBucketAuthorizer)
	// A missing resolver cannot treat explicit routing as an unscoped passthrough request.
	if !ok {
		return errors.New("runtime bucket authorization unavailable")
	}
	return resolver.authorizeSelectedRuntimeBucket(ctx, identity, *selected.bucketID)
}

// authorizeSelectedRuntimeBucket checks the same authoritative store before replay and again during credential resolution.
func (r *secretResolver) authorizeSelectedRuntimeBucket(ctx context.Context, identity auth.RuntimeIdentity, bucketID uuid.UUID) error {
	_, err := resolveUnifiedAppExecutionBucket(WithUnifiedAppBucket(ctx, identity.AppID, &bucketID), r.db, identity.AppFamilyID, CredentialRequest{AccountID: identity.AccountID, AppID: identity.AppID, TokenID: identity.TokenID})
	return err
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
