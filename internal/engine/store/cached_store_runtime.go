package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/Usefused/engine/internal/shared/cache"
)

const (
	runtimeCacheTTL                  = 30 * time.Second
	runtimeCacheLoadTimeout          = 5 * time.Second
	runtimeInvalidationSubject       = "engine.cache.invalidate.runtime_config"
	sdkScopeInvalidationSubject      = "engine.cache.invalidate.sdk_scope."
	secretInvalidationSubjectPattern = "engine.cache.invalidate.secrets.>"
)

type runtimeCacheState struct {
	cache      *cache.InMemoryCache
	loads      singleflight.Group
	generation atomic.Uint64
	ttl        time.Duration
	loadLimit  time.Duration
}

type runtimeLoadValue[T any] struct {
	value     T
	fromCache bool
}

type runtimePolicyValue struct {
	encoded []byte
}

// newRuntimeCacheState centralizes cache policy so each cached query receives
// the same bounded-staleness and load-timeout guarantees.
func newRuntimeCacheState() *runtimeCacheState {
	return &runtimeCacheState{
		cache:     cache.NewInMemoryCache(),
		ttl:       runtimeCacheTTL,
		loadLimit: runtimeCacheLoadTimeout,
	}
}

// subscribeCacheInvalidations keeps peer delivery best-effort because the
// absolute TTL, rather than messaging availability, is the correctness bound.
func (s *cachedStore) subscribeCacheInvalidations() {
	if s.nc == nil || s.nc.Conn == nil {
		return
	}
	_, _ = s.nc.Conn.Subscribe(secretInvalidationSubjectPattern, s.handleSecretInvalidation)
	_, _ = s.nc.Conn.Subscribe(runtimeInvalidationSubject, func(*nats.Msg) {
		s.invalidateRuntimeCacheLocal()
	})
}

// handleSecretInvalidation clears both supported secret lookup shapes because
// callers may switch auth alternatives after a rotation.
func (s *cachedStore) handleSecretInvalidation(message *nats.Msg) {
	parts := strings.Split(message.Subject, ".")
	if len(parts) != 5 {
		return
	}
	// Secret writes are bucket-scoped; clear both broad-list and exact keys so
	// a credential rotation cannot leave either resolution shape stale.
	s.cache.DeletePrefix("list_secrets:" + parts[4] + ":")
	s.cache.DeletePrefix("secret:" + parts[4] + ":")
}

// GetAppRuntime caches the full immutable runtime row; partial projections
// would add more cache keys and make hard-deactivation invalidation incomplete.
func (s *cachedStore) GetAppRuntime(ctx context.Context, appID uuid.UUID) (*AppRuntime, error) {
	value, err := loadRuntimeValue(ctx, s.runtime, "app:"+appID.String(), "app_runtime", cloneAppRuntime, func(loadCtx context.Context) (AppRuntime, error) {
		runtime, loadErr := s.Store.GetAppRuntime(loadCtx, appID)
		if loadErr != nil {
			return AppRuntime{}, loadErr
		}
		return *runtime, nil
	})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// DeprecateApp refreshes the cached status only after the lifecycle write is
// durable; deprecated versions remain executable but must expose fresh state.
func (s *cachedStore) DeprecateApp(ctx context.Context, appID uuid.UUID, message string, plannedDeactivationAt *time.Time) error {
	err := s.Store.DeprecateApp(ctx, appID, message, plannedDeactivationAt)
	if err == nil {
		s.NotifyAppRuntimeChanged(ctx, appID)
	}
	return err
}

// UndeprecateApp mirrors deprecation invalidation because both transitions
// alter the status projected by GetAppRuntime.
func (s *cachedStore) UndeprecateApp(ctx context.Context, appID uuid.UUID) error {
	err := s.Store.UndeprecateApp(ctx, appID)
	if err == nil {
		s.NotifyAppRuntimeChanged(ctx, appID)
	}
	return err
}

// DeactivateAppVersion invalidates both runtime configuration and the exact
// connected SDK scope after the tombstone transaction removes executability.
func (s *cachedStore) DeactivateAppVersion(ctx context.Context, appID, deactivatedBy uuid.UUID) error {
	err := s.Store.DeactivateAppVersion(ctx, appID, deactivatedBy)
	// Cache and peer invalidation happen only after the tombstone transaction commits successfully.
	if err == nil {
		s.NotifyAppRuntimeChanged(ctx, appID)
	}
	// Local cache eviction precedes stream cancellation so a racing registration's source recheck sees the tombstone.
	if err == nil && s.appRuntimeInvalidator != nil {
		s.appRuntimeInvalidator.InvalidateAppRuntime(appID)
	}
	return err
}

// SetAppFamilyBucket broadly invalidates runtime rows because one family-level
// assignment can affect multiple immutable app versions without an N+1 lookup.
func (s *cachedStore) SetAppFamilyBucket(ctx context.Context, appFamilyID, bucketID uuid.UUID) error {
	err := s.Store.SetAppFamilyBucket(ctx, appFamilyID, bucketID)
	if err == nil {
		s.invalidateRuntimeConfiguration(ctx)
	}
	return err
}

// runtimeBindingsKey includes every SQL predicate; length-prefixing authType
// prevents a delimiter in an operation name from creating an aliasing key.
func runtimeBindingsKey(bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) string {
	return "bindings:" + bucketID.String() + ":" + serviceID.String() + ":" + serviceVersionID.String() + ":" + strconv.Itoa(len(authType)) + ":" + authType + ":" + operationID
}

// runtimePolicyKey mirrors the two-level effective-policy lookup identity.
func runtimePolicyKey(serviceID, serviceVersionID uuid.UUID) string {
	return "policy:" + serviceID.String() + ":" + serviceVersionID.String()
}

// loadRuntimeValue applies one generation and one coalescing policy to every
// runtime DTO instead of duplicating subtly different cache behavior per read.
func loadRuntimeValue[T any](ctx context.Context, state *runtimeCacheState, logicalKey, kind string, clone func(T) T, load func(context.Context) (T, error)) (T, error) {
	generation := state.generation.Load()
	key := strconv.FormatUint(generation, 10) + ":" + logicalKey
	if value, ok := readRuntimeCache(state.cache, key, clone); ok {
		recordRuntimeCacheLookup(ctx, kind, "hit")
		return value, nil
	}
	result := state.loads.DoChan(key, func() (any, error) {
		return fetchRuntimeValue(ctx, state, generation, key, clone, load)
	})
	return waitForRuntimeValue(ctx, result, kind, clone)
}

// fetchRuntimeValue performs the leader's synchronous source-of-truth load and
// refuses to repopulate a generation invalidated while that load was running.
func fetchRuntimeValue[T any](ctx context.Context, state *runtimeCacheState, generation uint64, key string, clone func(T) T, load func(context.Context) (T, error)) (runtimeLoadValue[T], error) {
	if value, ok := readRuntimeCache(state.cache, key, clone); ok {
		return runtimeLoadValue[T]{value: value, fromCache: true}, nil
	}
	// A leader outlives an individual canceled waiter so one short client
	// deadline does not poison every request coalesced behind the same DB read.
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), state.loadLimit)
	defer cancel()
	value, err := load(loadCtx)
	if err != nil {
		return runtimeLoadValue[T]{}, err
	}
	if state.generation.Load() == generation {
		state.cache.Set(key, clone(value), state.ttl)
	}
	return runtimeLoadValue[T]{value: value}, nil
}

// waitForRuntimeValue lets each waiter retain its own cancellation deadline
// even though the shared leader continues for other coalesced callers.
func waitForRuntimeValue[T any](ctx context.Context, result <-chan singleflight.Result, kind string, clone func(T) T) (T, error) {
	select {
	case <-ctx.Done():
		var zero T
		recordRuntimeCacheLookup(ctx, kind, "canceled")
		return zero, ctx.Err()
	case loaded := <-result:
		return finishRuntimeLoad(ctx, loaded, kind, clone)
	}
}

// finishRuntimeLoad keeps result classification and type recovery outside the
// select so the wait path remains small and easy to audit.
func finishRuntimeLoad[T any](ctx context.Context, result singleflight.Result, kind string, clone func(T) T) (T, error) {
	if result.Err != nil {
		var zero T
		recordRuntimeCacheLookup(ctx, kind, "error")
		return zero, result.Err
	}
	loaded := result.Val.(runtimeLoadValue[T])
	cacheResult := runtimeLoadResult(loaded.fromCache, result.Shared)
	recordRuntimeCacheLookup(ctx, kind, cacheResult)
	return clone(loaded.value), nil
}

// runtimeLoadResult exposes only a bounded cache outcome in telemetry.
func runtimeLoadResult(fromCache, shared bool) string {
	if fromCache {
		return "hit"
	}
	if shared {
		return "coalesced"
	}
	return "miss"
}

// readRuntimeCache rejects an unexpected value type rather than allowing a
// bad cache entry to panic the execution path.
func readRuntimeCache[T any](runtimeCache *cache.InMemoryCache, key string, clone func(T) T) (T, bool) {
	value, ok := runtimeCache.Get(key)
	if ok {
		typed, valid := value.(T)
		if valid {
			return clone(typed), true
		}
		runtimeCache.Delete(key)
	}
	var zero T
	return zero, false
}

// cloneAppRuntime protects the cached JSON selection bytes from caller edits.
func cloneAppRuntime(value AppRuntime) AppRuntime {
	value.Selections = append([]byte(nil), value.Selections...)
	value.UnifiedDefinitions = append([]byte(nil), value.UnifiedDefinitions...)
	return value
}

// cloneWorkspaceBindings copies every result row so callers cannot mutate the
// complete SQL-filtered result retained for later executions.
func cloneWorkspaceBindings(bindings []WorkspaceConnectionBinding) []WorkspaceConnectionBinding {
	cloned := make([]WorkspaceConnectionBinding, len(bindings))
	for index := range bindings {
		cloned[index] = cloneWorkspaceBinding(bindings[index])
	}
	return cloned
}

// cloneWorkspaceBinding copies the slice and pointer fields that otherwise
// alias one cached binding across requests.
func cloneWorkspaceBinding(binding WorkspaceConnectionBinding) WorkspaceConnectionBinding {
	binding.LiteralValue = cloneStringPointer(binding.LiteralValue)
	binding.SourcePath = cloneStringPointer(binding.SourcePath)
	binding.SourceProfileRevision = cloneIntPointer(binding.SourceProfileRevision)
	binding.OperationIDs = append([]string(nil), binding.OperationIDs...)
	return binding
}

// cloneRuntimePolicyValue copies the encoded immutable representation; JSON
// avoids maintaining fragile hand-written clones for nested policy contracts.
func cloneRuntimePolicyValue(value runtimePolicyValue) runtimePolicyValue {
	value.encoded = append([]byte(nil), value.encoded...)
	return value
}

// encodeRuntimePolicyValue snapshots a validated store DTO once per cache
// fill, including nil fallback results that would otherwise repeat DB reads.
func encodeRuntimePolicyValue(override *WorkspaceExecutionPolicyOverride, loadErr error) (runtimePolicyValue, error) {
	if loadErr != nil || override == nil {
		return runtimePolicyValue{}, loadErr
	}
	encoded, err := json.Marshal(override)
	return runtimePolicyValue{encoded: encoded}, err
}

// decodeRuntimePolicyValue returns an independent nested policy graph to each
// caller while keeping serialization work off the database load path on hits.
func decodeRuntimePolicyValue(value runtimePolicyValue) (*WorkspaceExecutionPolicyOverride, error) {
	if len(value.encoded) == 0 {
		return nil, nil
	}
	var override WorkspaceExecutionPolicyOverride
	if err := json.Unmarshal(value.encoded, &override); err != nil {
		return nil, err
	}
	return &override, nil
}

// cloneStringPointer prevents cache callers from sharing mutable literals and
// source paths retained inside binding rows.
func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// cloneIntPointer prevents source-revision and timeout pointer aliasing.
func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// invalidateRuntimeConfiguration makes the local commit visible before its
// best-effort peer notification and records the bounded propagation outcome.
func (s *cachedStore) invalidateRuntimeConfiguration(ctx context.Context) {
	s.invalidateRuntimeCacheLocal()
	propagation := s.publishCacheInvalidation(runtimeInvalidationSubject)
	recordRuntimeCacheInvalidation(ctx, "runtime_configuration", propagation)
}

// invalidateRuntimeCacheLocal advances the generation before clearing entries
// so post-commit readers never join a pre-commit singleflight.
func (s *cachedStore) invalidateRuntimeCacheLocal() {
	// The generation changes before the clear so a post-commit reader cannot
	// join an in-flight load that began against the previous configuration.
	s.runtime.generation.Add(1)
	s.runtime.cache.Clear()
}

// publishCacheInvalidation deliberately returns a stable outcome rather than
// exposing transport errors or identifiers in mutation telemetry.
func (s *cachedStore) publishCacheInvalidation(subject string) string {
	if s.nc == nil || s.nc.Conn == nil {
		return "unavailable"
	}
	if err := s.nc.Conn.Publish(subject, nil); err != nil {
		return "publish_failed"
	}
	return "published"
}

// recordRuntimeCacheLookup adds bounded diagnostics to the caller's existing
// span without introducing a parallel telemetry pipeline.
func recordRuntimeCacheLookup(ctx context.Context, kind, result string) {
	trace.SpanFromContext(ctx).AddEvent("engine.runtime_cache.lookup", trace.WithAttributes(
		attribute.String("cache.kind", kind),
		attribute.String("cache.result", result),
	))
}

// recordRuntimeCacheInvalidation makes user/agent-triggered configuration
// changes auditable while excluding workspace, app, and secret identifiers.
func recordRuntimeCacheInvalidation(ctx context.Context, kind, propagation string) {
	trace.SpanFromContext(ctx).AddEvent("engine.runtime_cache.invalidated", trace.WithAttributes(
		attribute.String("cache.kind", kind),
		attribute.String("cache.propagation", propagation),
	))
}
