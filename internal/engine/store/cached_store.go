package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

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
	cache *cache.InMemoryCache
	nc    *messaging.NATSClient
}

func NewCachedStore(delegate Store, nc *messaging.NATSClient) Store {
	cs := &cachedStore{
		Store: delegate,
		cache: cache.NewInMemoryCache(),
		nc:    nc,
	}

	if nc != nil && nc.Conn != nil {
		nc.Conn.Subscribe("engine.cache.invalidate.secrets.>", func(m *nats.Msg) {
			parts := strings.Split(m.Subject, ".")
			if len(parts) == 5 {
				// Secret writes are bucket-scoped; clear both old broad-list keys
				// and exact point keys so rotated credentials do not linger.
				cs.cache.DeletePrefix("list_secrets:" + parts[4] + ":")
				cs.cache.DeletePrefix("secret:" + parts[4] + ":")
			}
		})

		nc.Conn.Subscribe("engine.cache.invalidate.token.>", func(m *nats.Msg) {
			parts := strings.Split(m.Subject, ".")
			if len(parts) == 5 {
				cs.cache.DeletePrefix("validate_token:" + parts[4] + ":")
			}
		})
	}

	return cs
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

func (s *cachedStore) ListEngineExecutionEventsByArtifact(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	reader, ok := s.Store.(ArtifactExecutionEventReader)
	if !ok {
		return nil, 0, errors.New("store does not support artifact execution activity")
	}
	return reader.ListEngineExecutionEventsByArtifact(ctx, filter)
}

func (s *cachedStore) GetEngineExecutionAnalyticsByArtifact(ctx context.Context, filter EngineExecutionFilter) (models.ArtifactExecutionAnalytics, error) {
	reader, ok := s.Store.(ArtifactExecutionAnalyticsReader)
	if !ok {
		return models.ArtifactExecutionAnalytics{}, errors.New("store does not support artifact execution analytics")
	}
	return reader.GetEngineExecutionAnalyticsByArtifact(ctx, filter)
}

func (s *cachedStore) UpsertArtifactSnapshots(ctx context.Context, snapshots []ArtifactSnapshot) error {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return errors.New("store does not support artifact snapshots")
	}
	return repository.UpsertArtifactSnapshots(ctx, snapshots)
}

func (s *cachedStore) DeleteArtifactSnapshot(ctx context.Context, accountID, artifactID uuid.UUID) error {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return errors.New("store does not support artifact snapshots")
	}
	return repository.DeleteArtifactSnapshot(ctx, accountID, artifactID)
}

func (s *cachedStore) GetArtifactSnapshot(ctx context.Context, accountID, artifactID uuid.UUID) (*ArtifactSnapshot, error) {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return nil, errors.New("store does not support artifact snapshots")
	}
	return repository.GetArtifactSnapshot(ctx, accountID, artifactID)
}

func (s *cachedStore) GetArtifactSnapshotByName(ctx context.Context, accountID uuid.UUID, kind, name string) (*ArtifactSnapshot, error) {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return nil, errors.New("store does not support artifact snapshots")
	}
	return repository.GetArtifactSnapshotByName(ctx, accountID, kind, name)
}

func (s *cachedStore) GetArtifactSnapshotByIdentity(ctx context.Context, accountID uuid.UUID, kind, name, version string) (*ArtifactSnapshot, error) {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return nil, errors.New("store does not support artifact snapshots")
	}
	return repository.GetArtifactSnapshotByIdentity(ctx, accountID, kind, name, version)
}

func (s *cachedStore) ListArtifactSnapshots(ctx context.Context, accountID uuid.UUID, kind string, limit, offset int) ([]ArtifactSnapshot, int, error) {
	repository, ok := s.Store.(ArtifactSnapshotStore)
	if !ok {
		return nil, 0, errors.New("store does not support artifact snapshots")
	}
	return repository.ListArtifactSnapshots(ctx, accountID, kind, limit, offset)
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

func (s *cachedStore) SaveArtifactScope(ctx context.Context, scope ArtifactScope) error {
	err := s.Store.SaveArtifactScope(ctx, scope)
	if err == nil && s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.sdk_scope."+scope.ArtifactID.String(), nil)
	}
	return err
}

func (s *cachedStore) DeleteArtifactScope(ctx context.Context, accountID, artifactID uuid.UUID) error {
	err := s.Store.DeleteArtifactScope(ctx, accountID, artifactID)
	if err == nil && s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.sdk_scope."+artifactID.String(), nil)
	}
	return err
}

func (s *cachedStore) NotifyArtifactScopeChanged(_ context.Context, artifactID uuid.UUID) {
	if s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.sdk_scope."+artifactID.String(), nil)
	}
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

func (s *cachedStore) ListWorkspaceServiceVersionsMissingContractSnapshots(ctx context.Context, limit int) ([]WorkspaceServiceVersion, error) {
	delegate, ok := s.Store.(WorkspaceServiceVersionContractBackfillStore)
	if !ok {
		return nil, errors.New("workspace service version contract backfill store is unavailable")
	}
	return delegate.ListWorkspaceServiceVersionsMissingContractSnapshots(ctx, limit)
}

// DeactivateSDK/ReactivateSDK invalidate LocalObjectCache's cached scope the
// same way SaveArtifactScope does above -- deliberately not relying on a live
// MCP session's own disconnect to evict the cache. Without this, a new
// session connecting for an artifactID that already has another live session
// (cache hit, ConnectSDK's reuseCachedSDK path) would never call
// loadArtifactScope at all, so it would never see a deactivation that landed
// between "session A connected" and "session B tries to connect" -- this
// closes that window instead of accepting it as an eventual-consistency gap.
func (s *cachedStore) DeactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error {
	err := s.Store.DeactivateSDK(ctx, accountID, artifactID)
	if err == nil && s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.sdk_scope."+artifactID.String(), nil)
	}
	return err
}

func (s *cachedStore) ReactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error {
	err := s.Store.ReactivateSDK(ctx, accountID, artifactID)
	if err == nil && s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.sdk_scope."+artifactID.String(), nil)
	}
	return err
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

func (s *cachedStore) RevokeSDKToken(ctx context.Context, artifactID uuid.UUID, name string) error {
	err := s.Store.RevokeSDKToken(ctx, artifactID, name)
	if err == nil && s.nc != nil && s.nc.Conn != nil {
		s.nc.Conn.Publish("engine.cache.invalidate.token."+artifactID.String(), nil)
	}
	return err
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

func (s *cachedStore) ValidateToken(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error) {
	step := observability.ThreadFromContext(ctx).Step("Cache: ValidateToken")

	key := "validate_token:" + artifactID.String() + ":" + tokenHash
	if val, ok := s.cache.Get(key); ok {
		step.Success(ctx)
		return val.(uuid.UUID), nil
	}

	step.SubStep("Cache miss, querying DB", nil)
	id, err := s.Store.ValidateToken(ctx, artifactID, tokenHash)
	if err == nil {
		s.cache.Set(key, id, 5*time.Minute)
		step.Success(ctx)
	} else {
		step.Error(ctx, err)
	}
	return id, err
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

// UpsertWorkspaceProfileOverride delegates atomic profile writes because
// bindings are queried directly and are not cached by this wrapper.
func (s *cachedStore) UpsertWorkspaceProfileOverride(ctx context.Context, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) (*WorkspaceConnectionProfile, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	return delegate.UpsertWorkspaceProfileOverride(ctx, profile, bindings)
}

// ResetWorkspaceProfile forwards exact override removal without introducing a
// stale cache entry for subsequent execution.
func (s *cachedStore) ResetWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return err
	}
	return delegate.ResetWorkspaceProfile(ctx, serviceID, serviceVersionID, authType)
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

// ListWorkspaceBindingsForExecution preserves SQL-side operation filtering on
// the hot path; adding a cache here would require profile-aware invalidation.
func (s *cachedStore) ListWorkspaceBindingsForExecution(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) ([]WorkspaceConnectionBinding, error) {
	delegate, err := s.workspaceProfileStore()
	if err != nil {
		return nil, err
	}
	return delegate.ListWorkspaceBindingsForExecution(ctx, bucketID, serviceID, serviceVersionID, authType, operationID)
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
	return delegate.ReconcileWorkspaceProfiles(ctx, replacements, deletes)
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

func (s *cachedStore) UpsertWorkspaceExecutionPolicyOverride(ctx context.Context, override WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error) {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return nil, err
	}
	return delegate.UpsertWorkspaceExecutionPolicyOverride(ctx, override)
}

func (s *cachedStore) GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceExecutionPolicyOverride, error) {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return nil, err
	}
	return delegate.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, serviceVersionID)
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

func (s *cachedStore) ResetWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID uuid.UUID, serviceVersionID *uuid.UUID) error {
	delegate, err := s.workspaceExecutionPolicyStore()
	if err != nil {
		return err
	}
	return delegate.ResetWorkspaceExecutionPolicyOverride(ctx, serviceID, serviceVersionID)
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
