package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/cache"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
)

// cachedStore wraps a store.Store with an in-memory cache to ensure
// sub-millisecond local resolution, guaranteeing ultra-low latency and
// offline resilience across the Engine API surface.
type cachedStore struct {
	Store
	cache   *cache.InMemoryCache
	runtime *runtimeCacheState
	nc      *messaging.NATSClient
}

var _ AuthConnectionRefreshStore = (*cachedStore)(nil)

func NewCachedStore(delegate Store, nc *messaging.NATSClient) Store {
	cs := &cachedStore{
		Store:   delegate,
		cache:   cache.NewInMemoryCache(),
		runtime: newRuntimeCacheState(),
		nc:      nc,
	}
	cs.subscribeCacheInvalidations()

	return cs
}

func (s *cachedStore) LoadEngineInstallationID(ctx context.Context) (uuid.UUID, error) {
	repository, ok := s.Store.(EngineInstallationStore)
	if !ok {
		return uuid.Nil, errors.New("store does not support Engine installation identity")
	}
	return repository.LoadEngineInstallationID(ctx)
}

func (s *cachedStore) LoadDefaultBucketID(ctx context.Context) (uuid.UUID, error) {
	loader, ok := s.Store.(interface {
		LoadDefaultBucketID(context.Context) (uuid.UUID, error)
	})
	if !ok {
		return uuid.Nil, errors.New("store does not support default bucket lookup")
	}
	return loader.LoadDefaultBucketID(ctx)
}

func (s *cachedStore) CountProjectedActiveServices(ctx context.Context, desiredIDs, removableIDs []uuid.UUID) (int, int, error) {
	counter, ok := s.Store.(WorkspaceServiceCapacityStore)
	if !ok {
		return 0, 0, errors.New("store does not support workspace service capacity")
	}
	return counter.CountProjectedActiveServices(ctx, desiredIDs, removableIDs)
}

func (s *cachedStore) ListSDKPackageLeaseRenewals(ctx context.Context, after uuid.UUID, limit int) ([]models.SDKPackageLeaseRenewal, error) {
	repository, ok := s.Store.(SDKPackageLeaseStore)
	if !ok {
		return nil, errors.New("store does not support SDK package lease renewal")
	}
	return repository.ListSDKPackageLeaseRenewals(ctx, after, limit)
}

func (s *cachedStore) GetSDKPackageBuildRequest(ctx context.Context, accountID, appID uuid.UUID) (*models.SDKGenerationRequest, error) {
	repository, ok := s.Store.(SDKPackageBuildStore)
	if !ok {
		return nil, errors.New("store does not support SDK package build recovery")
	}
	return repository.GetSDKPackageBuildRequest(ctx, accountID, appID)
}

func (s *cachedStore) ListEngineExecutionEventsByApp(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	reader, ok := s.Store.(AppExecutionEventReader)
	if !ok {
		return nil, 0, errors.New("store does not support app execution activity")
	}
	return reader.ListEngineExecutionEventsByApp(ctx, filter)
}

func (s *cachedStore) GetEngineExecutionAnalyticsByApp(ctx context.Context, filter EngineExecutionFilter) (models.AppExecutionAnalytics, error) {
	reader, ok := s.Store.(AppExecutionAnalyticsReader)
	if !ok {
		return models.AppExecutionAnalytics{}, errors.New("store does not support app execution analytics")
	}
	return reader.GetEngineExecutionAnalyticsByApp(ctx, filter)
}

func (s *cachedStore) UpsertSecret(ctx context.Context, secret WorkspaceSecret) error {
	err := s.Store.UpsertSecret(ctx, secret)
	if err == nil {
		s.invalidateSecretCaches(secret.BucketID, []WorkspaceSecret{secret})
	}
	return err
}

// UpsertSecrets mirrors Store's transaction boundary before invalidating
// caches, preserving hot-path credential consistency after paired rotations.
func (s *cachedStore) UpsertSecrets(ctx context.Context, secrets []WorkspaceSecret) error {
	err := s.Store.UpsertSecrets(ctx, secrets)
	if err != nil || len(secrets) == 0 {
		return err
	}
	// Batch writes are used for paired auth material. Cache invalidation waits
	// until the transaction commits so dispatch never observes a half-rotated
	// credential family from this Engine node.
	secretsByBucket := map[uuid.UUID][]WorkspaceSecret{}
	for _, secret := range secrets {
		secretsByBucket[secret.BucketID] = append(secretsByBucket[secret.BucketID], secret)
	}
	for bucketID, bucketSecrets := range secretsByBucket {
		s.invalidateSecretCaches(bucketID, bucketSecrets)
	}
	return nil
}

// invalidateSecretCaches clears exact keys plus bucket list caches because
// dispatch may resolve either shape depending on auth type and SDK scope.
func (s *cachedStore) invalidateSecretCaches(bucketID uuid.UUID, secrets []WorkspaceSecret) {
	for _, secret := range secrets {
		s.cache.Delete(secretCacheKey(secret.BucketID, secret.ServiceID, secret.KeyName))
	}
	// Clear secret caches locally and synchronously because dispatch reads
	// them on the hot path. Publishing to NATS below still propagates the
	// invalidation to other nodes, but this node must not wait for its own
	// async message before it stops serving a credential it just rotated.
	s.cache.DeletePrefix("list_secrets:" + bucketID.String() + ":")
	if s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.secrets."+bucketID.String(), nil)
	}
}

// NotifyAppRuntimeChanged invalidates both the store DTOs and the connected
// SDK object cache because an apply can change selections and derived metadata.
func (s *cachedStore) NotifyAppRuntimeChanged(ctx context.Context, appID uuid.UUID) {
	s.invalidateRuntimeConfiguration(ctx)
	propagation := s.publishCacheInvalidation(sdkScopeInvalidationSubject + appID.String())
	recordRuntimeCacheInvalidation(ctx, "sdk_scope", propagation)
}

// IsWorkspaceServiceVersionActive forwards the exact SQL lookup rather than
// hiding the optional capability behind the cache wrapper used by the Engine.
func (s *cachedStore) IsWorkspaceServiceVersionActive(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (bool, error) {
	delegate, ok := s.Store.(WorkspaceServiceVersionStatusStore)
	if !ok {
		return false, errors.New("workspace service version status store is unavailable")
	}
	return delegate.IsWorkspaceServiceVersionActive(ctx, serviceID, serviceVersionID)
}

func (s *cachedStore) GetWorkspaceServiceVersion(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceServiceVersion, error) {
	delegate, ok := s.Store.(WorkspaceServiceVersionLookupStore)
	if !ok {
		return nil, errors.New("workspace service version lookup store is unavailable")
	}
	return delegate.GetWorkspaceServiceVersion(ctx, serviceID, serviceVersionID)
}

// authConnectionRefreshDelegate resolves the optional refresh capability from
// the wrapped store without bypassing the Engine's production cache wrapper.
func (s *cachedStore) authConnectionRefreshDelegate() (AuthConnectionRefreshStore, error) {
	delegate, ok := s.Store.(AuthConnectionRefreshStore)
	if !ok {
		return nil, errors.New("auth connection refresh store is unavailable")
	}
	return delegate, nil
}

// ClaimAuthConnectionsForRefresh forwards the worker's atomic SKIP LOCKED page
// claim unchanged; token rows are intentionally never cached.
func (s *cachedStore) ClaimAuthConnectionsForRefresh(ctx context.Context, cutoff, passStartedAt, now, leaseExpiresAt time.Time, limit int) ([]AuthConnectionRefreshClaim, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return nil, err
	}
	return delegate.ClaimAuthConnectionsForRefresh(ctx, cutoff, passStartedAt, now, leaseExpiresAt, limit)
}

// TryClaimAuthConnectionRefresh forwards the request-time exact-version lease
// without populating any credential cache entry.
func (s *cachedStore) TryClaimAuthConnectionRefresh(ctx context.Context, id, serviceVersionID uuid.UUID, now, leaseExpiresAt time.Time) (*AuthConnectionRefreshClaim, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return nil, err
	}
	return delegate.TryClaimAuthConnectionRefresh(ctx, id, serviceVersionID, now, leaseExpiresAt)
}

// CompleteAuthConnectionRefresh forwards the lease-token CAS so rotated token
// material is committed only by the current owner.
func (s *cachedStore) CompleteAuthConnectionRefresh(ctx context.Context, id, leaseToken uuid.UUID, refreshed AuthConnection, refreshedAt time.Time) (*AuthConnection, bool, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return nil, false, err
	}
	return delegate.CompleteAuthConnectionRefresh(ctx, id, leaseToken, refreshed, refreshedAt)
}

// ReleaseAuthConnectionRefresh forwards transient retry state while preserving
// the raw PostgreSQL adapter as the only lease transition implementation.
func (s *cachedStore) ReleaseAuthConnectionRefresh(ctx context.Context, id, leaseToken uuid.UUID, retryNotBefore time.Time, failureCode, traceID string, failedAt time.Time) (bool, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return false, err
	}
	return delegate.ReleaseAuthConnectionRefresh(ctx, id, leaseToken, retryNotBefore, failureCode, traceID, failedAt)
}

// MarkAuthConnectionReconnectRequired forwards the permanent provider-grant
// transition without caching security-sensitive connection state.
func (s *cachedStore) MarkAuthConnectionReconnectRequired(ctx context.Context, id, leaseToken uuid.UUID, failureCode, traceID string, failedAt time.Time) (bool, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return false, err
	}
	return delegate.MarkAuthConnectionReconnectRequired(ctx, id, leaseToken, failureCode, traceID, failedAt)
}

// GetAuthConnectionByID forwards an internal contention reload; tenant-facing
// paths still use bucket-authorized connection lookups from Store.
func (s *cachedStore) GetAuthConnectionByID(ctx context.Context, id uuid.UUID) (*AuthConnection, error) {
	delegate, err := s.authConnectionRefreshDelegate()
	if err != nil {
		return nil, err
	}
	return delegate.GetAuthConnectionByID(ctx, id)
}

func (s *cachedStore) ListWorkspaceServiceVersionsMissingContractSnapshots(ctx context.Context, limit int) ([]WorkspaceServiceVersion, error) {
	delegate, ok := s.Store.(WorkspaceServiceVersionContractBackfillStore)
	if !ok {
		return nil, errors.New("workspace service version contract backfill store is unavailable")
	}
	return delegate.ListWorkspaceServiceVersionsMissingContractSnapshots(ctx, limit)
}

func (s *cachedStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	err := s.Store.DeleteSecret(ctx, bucketID, serviceID, keyName)
	if err == nil {
		s.cache.Delete(secretCacheKey(bucketID, serviceID, keyName))
		s.cache.DeletePrefix("list_secrets:" + bucketID.String() + ":")
		if s.nc != nil && s.nc.Conn != nil {
			s.nc.Conn.Publish("engine.cache.invalidate.secrets."+bucketID.String(), nil)
		}
	}
	return err
}

func (s *cachedStore) DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error {
	err := s.Store.DeleteSecrets(ctx, bucketID, serviceID, keyNames)
	if err != nil {
		return err
	}
	secrets := make([]WorkspaceSecret, 0, len(keyNames))
	for _, keyName := range keyNames {
		secrets = append(secrets, WorkspaceSecret{WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: bucketID, ServiceID: serviceID, KeyName: keyName}})
	}
	s.invalidateSecretCaches(bucketID, secrets)
	return nil
}

func (s *cachedStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	return s.Store.VerifyWorkspaceOwner(ctx, accountID)
}

func (s *cachedStore) GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error) {
	step := observability.ThreadFromContext(ctx).Step("Cache: GetLatestWorkspaceServiceVersion")

	key := "workspace_service_version:" + accountID.String() + ":" + serviceID.String()
	if val, ok := s.cache.Get(key); ok {
		step.Success(ctx)
		return val.(string), nil
	}

	step.SubStep("Cache miss, querying DB", nil)
	version, err := s.Store.GetLatestWorkspaceServiceVersion(ctx, accountID, serviceID)
	if err == nil {
		s.cache.Set(key, version, 5*time.Minute)
		step.Success(ctx)
	} else {
		step.Error(ctx, err)
	}
	return version, err
}

func (s *cachedStore) GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*WorkspaceSecret, error) {
	key := secretCacheKey(bucketID, serviceID, keyName)
	if val, ok := s.cache.Get(key); ok {
		sec, ok := cachedSecretValue(val)
		if !ok {
			return nil, nil
		}
		return sec, nil
	}

	sec, err := s.Store.GetSecret(ctx, bucketID, serviceID, keyName)
	if err == nil {
		// Cache the result including not-found (nil) to avoid DB hammering
		// for non-existent secrets on every webhook request.
		s.cache.Set(key, sec, 5*time.Minute)
	}
	return sec, err
}

func (s *cachedStore) GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, error) {
	keyNames = uniqueSecretKeyNames(keyNames)
	if len(keyNames) == 0 {
		return nil, nil
	}
	cached, missing := s.cachedSecrets(bucketID, serviceID, keyNames)
	if len(missing) == 0 {
		return cached, nil
	}
	fetched, err := s.Store.GetSecrets(ctx, bucketID, serviceID, missing)
	if err != nil {
		return nil, err
	}
	s.cacheFetchedSecrets(bucketID, serviceID, missing, fetched)
	return append(cached, fetched...), nil
}

func (s *cachedStore) GetFirstCompleteSecretSet(ctx context.Context, bucketID, serviceID uuid.UUID, alternatives []SecretKeyAlternative) ([]WorkspaceSecret, error) {
	// Selection must happen atomically in the database; composing per-key cache
	// hits here could mix OR branches or turn the hot path into N+1 lookups.
	return s.Store.GetFirstCompleteSecretSet(ctx, bucketID, serviceID, alternatives)
}

func (s *cachedStore) GetBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]BucketValue, error) {
	// Not caching bucket values yet, fall through to underlying store.
	return s.Store.GetBucketValues(ctx, bucketID, serviceID, keyNames)
}

func (s *cachedStore) cachedSecrets(bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, []string) {
	var cached []WorkspaceSecret
	var missing []string
	for _, keyName := range keyNames {
		val, ok := s.cache.Get(secretCacheKey(bucketID, serviceID, keyName))
		if !ok {
			missing = append(missing, keyName)
			continue
		}
		if sec, ok := cachedSecretValue(val); ok {
			cached = append(cached, *sec)
		}
	}
	return cached, missing
}

func (s *cachedStore) cacheFetchedSecrets(bucketID, serviceID uuid.UUID, requested []string, fetched []WorkspaceSecret) {
	fetchedByKey := workspaceSecretsByKey(fetched)
	for _, keyName := range requested {
		sec := fetchedByKey[keyName]
		// Cache nil misses too; selected auth should not turn a missing optional
		// secret into repeated DB work on every request.
		if sec == nil {
			s.cache.Set(secretCacheKey(bucketID, serviceID, keyName), nil, 5*time.Minute)
			continue
		}
		s.cache.Set(secretCacheKey(bucketID, serviceID, keyName), sec, 5*time.Minute)
	}
}

func cachedSecretValue(val any) (*WorkspaceSecret, bool) {
	if val == nil {
		return nil, false
	}
	sec, _ := val.(*WorkspaceSecret)
	return sec, sec != nil
}

func workspaceSecretsByKey(secrets []WorkspaceSecret) map[string]*WorkspaceSecret {
	out := make(map[string]*WorkspaceSecret, len(secrets))
	for i := range secrets {
		out[secrets[i].KeyName] = &secrets[i]
	}
	return out
}

func secretCacheKey(bucketID, serviceID uuid.UUID, keyName string) string {
	return "secret:" + bucketID.String() + ":" + serviceID.String() + ":" + keyName
}

// workspaceProfileStore unwraps the optional targeted capability explicitly so
// the cache layer cannot make runtime fall back to broad bucket reads.
func (s *cachedStore) workspaceProfileStore() (WorkspaceProfileStore, error) {
	delegate, ok := s.Store.(WorkspaceProfileStore)
	if !ok {
		return nil, errors.New("connection profile store is unavailable")
	}
	return delegate, nil
}

// UpsertWorkspaceProfileOverride invalidates only after the atomic profile and
// binding replacement commits, so execution never caches a half-written view.
func (s *cachedStore) UpsertWorkspaceProfileOverride(ctx context.Context, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) (*WorkspaceConnectionProfile, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	stored, err := delegate.UpsertWorkspaceProfileOverride(ctx, profile, bindings)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return stored, err
}

// ResetWorkspaceProfile clears the effective binding cache after the baseline
// fallback becomes visible in the database.
func (s *cachedStore) ResetWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return err
	}
	err = delegate.ResetWorkspaceProfile(ctx, serviceID, serviceVersionID, authType)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return err
}

// GetEffectiveWorkspaceProfile preserves the delegate's exact composite lookup semantics.
func (s *cachedStore) GetEffectiveWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) (*WorkspaceConnectionProfile, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	return delegate.GetEffectiveWorkspaceProfile(ctx, serviceID, serviceVersionID, authType)
}

// GetEffectiveWorkspaceProfiles retains the delegate's one-query batch read
// across pinned versions instead of looping through the single-profile method.
func (s *cachedStore) GetEffectiveWorkspaceProfiles(ctx context.Context, refs []WorkspaceProfileRef) ([]WorkspaceConnectionProfile, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	return delegate.GetEffectiveWorkspaceProfiles(ctx, refs)
}

// ListWorkspaceProfileBindings forwards the already scoped admin query unchanged.
func (s *cachedStore) ListWorkspaceProfileBindings(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) ([]WorkspaceConnectionBinding, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListWorkspaceProfileBindings(ctx, serviceID, serviceVersionID, authType)
}

// ListWorkspaceBindingsForExecution caches the complete SQL-filtered result;
// it never broad-loads bindings for filtering in Go.
func (s *cachedStore) ListWorkspaceBindingsForExecution(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) ([]WorkspaceConnectionBinding, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	key := runtimeBindingsKey(bucketID, serviceID, serviceVersionID, authType, operationID)
	return loadRuntimeValue(ctx, s.runtime, key, "workspace_bindings", cloneWorkspaceBindings, func(loadCtx context.Context) ([]WorkspaceConnectionBinding, error) {
		return delegate.ListWorkspaceBindingsForExecution(loadCtx, bucketID, serviceID, serviceVersionID, authType, operationID)
	})
}

// MarkWorkspaceProfilePublished is bookkeeping only (no cached read depends on
// it directly), so it simply delegates without any cache invalidation.
func (s *cachedStore) MarkWorkspaceProfilePublished(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return err
	}
	return delegate.MarkWorkspaceProfilePublished(ctx, serviceID, serviceVersionID, authType)
}

// ReconcileWorkspaceProfiles keeps multi-version workspace apply on the
// delegate's fixed-query transaction instead of expanding it into per-version writes.
func (s *cachedStore) ReconcileWorkspaceProfiles(ctx context.Context, replacements []WorkspaceProfileReplacement, deletes []WorkspaceProfileRef) error {
	delegate, ok := s.Store.(WorkspaceProfileBatchStore)
	if !ok {
		return errors.New("connection profile batch store is unavailable")
	}
	err := delegate.ReconcileWorkspaceProfiles(ctx, replacements, deletes)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return err
}

func (s *cachedStore) SaveRuntimeEntitlement(ctx context.Context, entitlement models.RuntimeEntitlement) error {
	delegate, ok := s.Store.(interface {
		SaveRuntimeEntitlement(context.Context, models.RuntimeEntitlement) error
	})
	if !ok {
		return errors.New("runtime entitlement store is unavailable")
	}
	return delegate.SaveRuntimeEntitlement(ctx, entitlement)
}

func (s *cachedStore) GetRuntimeEntitlement(ctx context.Context) (models.RuntimeEntitlement, error) {
	delegate, ok := s.Store.(interface {
		GetRuntimeEntitlement(context.Context) (models.RuntimeEntitlement, error)
	})
	if !ok {
		return models.DefaultRuntimeEntitlement(), errors.New("runtime entitlement store is unavailable")
	}
	return delegate.GetRuntimeEntitlement(ctx)
}

func (s *cachedStore) IncrementRuntimeUsageCounters(ctx context.Context, increments []models.EngineUsageIncrement) error {
	delegate, ok := s.Store.(interface {
		IncrementRuntimeUsageCounters(context.Context, []models.EngineUsageIncrement) error
	})
	if !ok {
		return errors.New("runtime usage counter store is unavailable")
	}
	return delegate.IncrementRuntimeUsageCounters(ctx, increments)
}

func (s *cachedStore) ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error) {
	delegate, ok := s.Store.(interface {
		ListPendingRuntimeUsageReports(context.Context, int) ([]models.EngineUsageReport, error)
	})
	if !ok {
		return nil, errors.New("runtime usage report store is unavailable")
	}
	return delegate.ListPendingRuntimeUsageReports(ctx, limit)
}

func (s *cachedStore) MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error {
	delegate, ok := s.Store.(interface {
		MarkRuntimeUsageReportsFlushed(context.Context, []uuid.UUID, time.Time) error
	})
	if !ok {
		return errors.New("runtime usage report store is unavailable")
	}
	return delegate.MarkRuntimeUsageReportsFlushed(ctx, reportIDs, flushedAt)
}

func (s *cachedStore) UpsertServiceContractSnapshot(ctx context.Context, snapshot ServiceContractSnapshot) (*ServiceContractSnapshot, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.UpsertServiceContractSnapshot(ctx, snapshot)
}

func (s *cachedStore) GetServiceContractMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.GetServiceContractMetadata(ctx, serviceID, serviceVersionID)
}

// ListServiceContractMetadata forwards the fixed-query cold-cache read because
// the runtime cache, rather than this wrapper, owns app-scope refcounts.
func (s *cachedStore) ListServiceContractMetadata(ctx context.Context, refs []ServiceContractMetadataRef) (map[ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	delegate, ok := s.Store.(ServiceContractMetadataBatchStore)
	// Refusing a scalar fallback keeps cold initialization query count bounded.
	if !ok {
		return nil, errors.New("service contract metadata batch store is unavailable")
	}
	return delegate.ListServiceContractMetadata(ctx, refs)
}

func (s *cachedStore) GetServiceContractEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.GetServiceContractEndpointByName(ctx, serviceID, serviceVersionID, endpointName)
}

func (s *cachedStore) ListServiceContractEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListServiceContractEndpointsByNames(ctx, serviceID, serviceVersionID, endpointNames)
}

func (s *cachedStore) ListServiceContractEndpointsForSelections(ctx context.Context, selections []ServiceContractEndpointSelection, endpointNames []string) ([]ServiceContractEndpointMatch, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListServiceContractEndpointsForSelections(ctx, selections, endpointNames)
}

func (s *cachedStore) ListServiceContractEndpointsByIDs(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointIDs []uuid.UUID) ([]fusedobject.Endpoint, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListServiceContractEndpointsByIDs(ctx, serviceID, serviceVersionID, endpointIDs)
}

func (s *cachedStore) ListServiceContractOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	delegate, err := s.serviceContractSnapshotStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListServiceContractOperations(ctx, serviceID, serviceVersionID)
}

// workspaceConnectSyncStore unwraps the paired export capability explicitly;
// embedding Store alone cannot promote optional methods from its delegate.
func (s *cachedStore) workspaceConnectSyncStore() (WorkspaceConnectSyncStore, error) {
	delegate, ok := s.Store.(WorkspaceConnectSyncStore)
	if !ok {
		return nil, errors.New("workspace connect config sync is unavailable")
	}
	return delegate, nil
}

func (s *cachedStore) workspaceExecutionPolicyStore() (WorkspaceExecutionPolicyStore, error) {
	delegate, ok := s.Store.(WorkspaceExecutionPolicyStore)
	if !ok {
		return nil, errors.New("workspace execution policy store is unavailable")
	}
	return delegate, nil
}

// UpsertWorkspaceExecutionPolicyOverride invalidates after commit so runtime
// metadata cannot retain the policy that the user or agent just replaced.
func (s *cachedStore) UpsertWorkspaceExecutionPolicyOverride(ctx context.Context, override WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error) {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return nil, err
	}
	stored, err := delegate.UpsertWorkspaceExecutionPolicyOverride(ctx, override)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return stored, err
}

// GetEffectiveWorkspaceExecutionPolicyOverride caches the delegate's complete
// precedence-resolved row, including nil, rather than querying both layers.
func (s *cachedStore) GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceExecutionPolicyOverride, error) {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return nil, err
	}
	value, err := loadRuntimeValue(ctx, s.runtime, runtimePolicyKey(serviceID, serviceVersionID), "execution_policy", cloneRuntimePolicyValue, func(loadCtx context.Context) (runtimePolicyValue, error) {
		override, loadErr := delegate.GetEffectiveWorkspaceExecutionPolicyOverride(loadCtx, serviceID, serviceVersionID)
		return encodeRuntimePolicyValue(override, loadErr)
	})
	if err != nil {
		return nil, err
	}
	return decodeRuntimePolicyValue(value)
}

func (s *cachedStore) GetEffectiveWorkspaceExecutionPolicyOverrides(ctx context.Context, refs []WorkspaceExecutionPolicyRef) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error) {
	delegate, ok := s.Store.(WorkspaceExecutionPolicyBatchStore)
	if !ok {
		return nil, errors.New("workspace execution policy batch store is unavailable")
	}
	return delegate.GetEffectiveWorkspaceExecutionPolicyOverrides(ctx, refs)
}

func (s *cachedStore) GetWorkspaceExecutionPolicyOverrides(ctx context.Context, refs []WorkspaceExecutionPolicyRef) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error) {
	delegate, ok := s.Store.(WorkspaceExecutionPolicyExactBatchStore)
	if !ok {
		return nil, errors.New("store does not support exact workspace execution policy batches")
	}
	return delegate.GetWorkspaceExecutionPolicyOverrides(ctx, refs)
}

// ResetWorkspaceExecutionPolicyOverride invalidates after deletion so the
// immutable snapshot becomes the effective value on the next runtime read.
func (s *cachedStore) ResetWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID uuid.UUID, serviceVersionID *uuid.UUID) error {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return err
	}
	err = delegate.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, serviceVersionID)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return err
}

func (s *cachedStore) serviceContractSnapshotStore() (ServiceContractSnapshotStore, error) {
	delegate, ok := s.Store.(ServiceContractSnapshotStore)
	if !ok {
		return nil, errors.New("service contract snapshot store is unavailable")
	}
	return delegate, nil
}

// ListWorkspaceConnectConfigs preserves the delegate's single workspace query
// because exported OAuth configuration is administrative and not a hot path.
func (s *cachedStore) ListWorkspaceConnectConfigs(ctx context.Context) ([]WorkspaceConnectConfig, error) {
	delegate, err := s.workspaceConnectSyncStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListWorkspaceConnectConfigs(ctx)
}

// ListWorkspaceConnectProfiles forwards the matching fixed-query profile read
// instead of deriving profile state from cached binding rows.
func (s *cachedStore) ListWorkspaceConnectProfiles(ctx context.Context) ([]WorkspaceConnectionProfile, error) {
	delegate, err := s.workspaceConnectSyncStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListWorkspaceConnectProfiles(ctx)
}
