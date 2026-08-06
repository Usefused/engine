package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

type ObjectCache interface {
	ConnectSDK(ctx context.Context, appID string) error
	DisconnectSDK(appID string)
	GetOrFetchServiceMetadata(ctx context.Context, appID string, serviceID string) (*fusedobject.ServiceMetadata, error)
	GetEndpoint(ctx context.Context, appID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error)
	// ListEndpointsForSelection returns the endpoints one SDKSelection grants
	// access to (SelectAll, or the ones named by EndpointIDs), resolved
	// against the service+version's full operation list. Distinct from
	// GetEndpoint's by-name lookup used on the dispatch hot path: this exists
	// to build a per-session MCP fixture (mcp_session_fixture.go) from a
	// selection that only carries opaque endpoint IDs, not names.
	ListEndpointsForSelection(ctx context.Context, appID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error)
	Invalidate(serviceID string)
	InvalidateAppRuntime(appID string)
	GetAppRuntime(ctx context.Context, appID string) (string, []byte, error)
}

type LocalObjectCache struct {
	db                    store.Store
	registryClient        RegistryClient
	mu                    sync.RWMutex
	serviceMetadataCache  map[string]*fusedobject.ServiceMetadata
	endpointMetadataCache map[string]*fusedobject.Endpoint // cache key: serviceID:endpointName
	scopes                map[string][]byte
	sdkVersions           map[string]map[string]string
	scopeRefCounts        map[string]int
	objectRefCounts       map[string]int
}

func NewLocalObjectCache(db store.Store, rc RegistryClient) *LocalObjectCache {
	return &LocalObjectCache{
		db:                    db,
		registryClient:        rc,
		serviceMetadataCache:  make(map[string]*fusedobject.ServiceMetadata),
		endpointMetadataCache: make(map[string]*fusedobject.Endpoint),
		scopes:                make(map[string][]byte),
		sdkVersions:           make(map[string]map[string]string),
		scopeRefCounts:        make(map[string]int),
		objectRefCounts:       make(map[string]int),
	}
}

func (c *LocalObjectCache) ConnectSDK(ctx context.Context, appID string) error {
	started := time.Now()
	sdkUUID, err := uuid.Parse(appID)
	if err != nil {
		return fmt.Errorf("invalid sdk id format: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.scopes[appID]; exists {
		err := c.reuseCachedSDK(ctx, appID)
		slog.InfoContext(ctx, "SDK connect cache timing",
			slog.String("app.id", appID),
			slog.String("cache_status", "reused"),
			slog.Float64("duration_ms", float64(time.Since(started).Microseconds())/1000),
		)
		return err
	}

	scopeStarted := time.Now()
	scopeJSON, selections, err := c.loadAppRuntime(ctx, sdkUUID)
	scopeDuration := time.Since(scopeStarted)
	if err != nil {
		return err
	}
	metadataStarted := time.Now()
	if err := c.cacheSDKSelections(ctx, appID, selections); err != nil {
		return err
	}
	metadataDuration := time.Since(metadataStarted)

	c.scopes[appID] = scopeJSON
	c.scopeRefCounts[appID] = 1

	slog.InfoContext(ctx, "SDK connected (loaded to cache)",
		slog.String("appID", appID),
		slog.Int("selection_count", len(selections)),
		slog.Float64("scope_load_ms", float64(scopeDuration.Microseconds())/1000),
		slog.Float64("service_metadata_load_ms", float64(metadataDuration.Microseconds())/1000),
		slog.Float64("total_ms", float64(time.Since(started).Microseconds())/1000),
	)
	return nil
}

func (c *LocalObjectCache) reuseCachedSDK(ctx context.Context, appID string) error {
	c.scopeRefCounts[appID]++

	var selections []models.SDKSelection
	_ = json.Unmarshal(c.scopes[appID], &selections)
	versions := c.sdkVersions[appID]
	for _, sel := range selections {
		svcID := sel.ServiceID.String()
		version := versions[svcID]
		cacheKey := svcID + ":" + version
		c.objectRefCounts[cacheKey]++
	}
	slog.InfoContext(ctx, "SDK connected (re-used cache)", slog.String("appID", appID))
	return nil
}

func (c *LocalObjectCache) loadAppRuntime(ctx context.Context, sdkUUID uuid.UUID) ([]byte, []models.SDKSelection, error) {
	scope, err := c.db.GetAppRuntime(ctx, sdkUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch sdk scope: %w", err)
	}
	if scope.ScopeSchemaVersion != models.AppScopeSchemaVersion {
		return nil, nil, fmt.Errorf("ScopeError: unsupported scope schema version")
	}

	var selections []models.SDKSelection
	if err := json.Unmarshal(scope.Selections, &selections); err != nil {
		return nil, nil, fmt.Errorf("ScopeError: invalid scope format")
	}
	return scope.Selections, selections, nil
}

func (c *LocalObjectCache) cacheSDKSelections(ctx context.Context, appID string, selections []models.SDKSelection) error {
	for _, sel := range selections {
		if c.sdkVersions[appID] == nil {
			c.sdkVersions[appID] = make(map[string]string)
		}

		version, err := selectionVersionIdentity(sel)
		if err != nil {
			return err
		}

		svcID := sel.ServiceID.String()
		c.sdkVersions[appID][svcID] = version
		cacheKey := svcID + ":" + version

		if err := c.fetchServiceMetadataIfMissing(ctx, sel.ServiceID, version, cacheKey); err != nil {
			return err
		}
		c.objectRefCounts[cacheKey]++

		// Pre-warm the endpoint cache for every operation in this selection so
		// that the first dispatch to each endpoint is a cache hit rather than a
		// synchronous Registry round trip on the hot execution path.
		//
		// SelectAll scopes omit OperationNames (the full list isn't known at
		// scope-generation time), so those fall back to lazy fetch on first use.
		if len(sel.OperationNames) > 0 {
			if err := c.prefetchEndpoints(ctx, sel, svcID, version); err != nil {
				// Endpoint pre-warm is best-effort. A failure means the first
				// dispatch to each endpoint falls back to lazy fetch — the
				// pre-existing behavior — so the SDK can still function.
				slog.WarnContext(ctx, "Endpoint pre-warm failed; falling back to lazy fetch",
					slog.String("service_id", svcID),
					slog.Any("error", err),
				)
			}
		}
	}
	return nil
}

// prefetchEndpoints batch-fetches all named endpoints for a selection from the
// local snapshot when present, otherwise from Registry for migration-era
// activations. Called with c.mu held (write lock from ConnectSDK →
// cacheSDKSelections).
func (c *LocalObjectCache) prefetchEndpoints(ctx context.Context, sel models.SDKSelection, svcID, version string) error {
	prefetchStarted := time.Now()
	eps, source, err := c.fetchEndpointsByNames(ctx, sel.ServiceID, sel.ServiceVersionID, sel.OperationNames)
	if err != nil {
		return fmt.Errorf("prefetch endpoints for %s: %w", svcID, err)
	}
	for i := range eps {
		ep := &eps[i]
		cacheKey := svcID + ":" + version + ":" + ep.Name
		c.endpointMetadataCache[cacheKey] = ep
	}
	slog.InfoContext(ctx, "Endpoint cache pre-warmed",
		slog.String("service_id", svcID),
		slog.Int("endpoint_count", len(eps)),
		slog.String("source", source),
		slog.Float64("contract_fetch_ms", float64(time.Since(prefetchStarted).Microseconds())/1000),
	)
	return nil
}

func selectionVersionIdentity(sel models.SDKSelection) (string, error) {
	if sel.ServiceVersionID != uuid.Nil {
		return sel.ServiceVersionID.String(), nil
	}
	return "", fmt.Errorf("ScopeError: service_version_id required for service %s", sel.ServiceID)
}

func (c *LocalObjectCache) fetchServiceMetadataIfMissing(ctx context.Context, serviceID uuid.UUID, version, cacheKey string) error {
	if _, exists := c.serviceMetadataCache[cacheKey]; exists {
		slog.DebugContext(ctx, "Service metadata cache hit",
			slog.String("service_id", serviceID.String()),
			slog.String("service_version_id", version),
		)
		return nil
	}
	fetchStarted := time.Now()
	fo, source, err := c.fetchServiceMetadata(ctx, serviceID, version)
	fetchDuration := time.Since(fetchStarted)
	if err != nil {
		return fmt.Errorf("failed to fetch service metadata %s: %w", serviceID, err)
	}

	c.serviceMetadataCache[cacheKey] = fo
	slog.InfoContext(ctx, "Service metadata fetched for SDK cache",
		slog.String("service_id", serviceID.String()),
		slog.String("service_version_id", version),
		slog.String("source", source),
		slog.Float64("contract_fetch_ms", float64(fetchDuration.Microseconds())/1000),
	)
	return nil
}

func (c *LocalObjectCache) DisconnectSDK(appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.scopes[appID]; !exists {
		return
	}

	c.scopeRefCounts[appID]--
	if c.scopeRefCounts[appID] <= 0 {
		var selections []models.SDKSelection
		_ = json.Unmarshal(c.scopes[appID], &selections)

		versions := c.sdkVersions[appID]

		for _, sel := range selections {
			svcID := sel.ServiceID.String()
			version := versions[svcID]
			cacheKey := svcID + ":" + version

			c.objectRefCounts[cacheKey]--
			if c.objectRefCounts[cacheKey] <= 0 {
				delete(c.serviceMetadataCache, cacheKey)
				delete(c.objectRefCounts, cacheKey)

				// Evict associated endpoints
				prefix := cacheKey + ":"
				for k := range c.endpointMetadataCache {
					// naive prefix check
					if len(k) > len(prefix) && k[:len(prefix)] == prefix {
						delete(c.endpointMetadataCache, k)
					}
				}

				slog.Info("Evicted ServiceMetadata from cache", slog.String("serviceID", svcID), slog.String("version", version))
			}
		}
		delete(c.scopes, appID)
		delete(c.scopeRefCounts, appID)
		delete(c.sdkVersions, appID)
		slog.Info("SDK disconnected (evicted from cache)", slog.String("appID", appID))
	} else {
		var selections []models.SDKSelection
		_ = json.Unmarshal(c.scopes[appID], &selections)
		versions := c.sdkVersions[appID]
		for _, sel := range selections {
			svcID := sel.ServiceID.String()
			version := versions[svcID]
			cacheKey := svcID + ":" + version
			c.objectRefCounts[cacheKey]--
		}
		slog.Info("SDK disconnected (decremented refcount)", slog.String("appID", appID))
	}
}

func (c *LocalObjectCache) GetOrFetchServiceMetadata(ctx context.Context, appID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	c.mu.RLock()
	versions, ok := c.sdkVersions[appID]
	if !ok {
		c.mu.RUnlock()
		return nil, fmt.Errorf("sdk session %s not initialized", appID)
	}
	version, ok := versions[serviceID]
	if !ok {
		c.mu.RUnlock()
		return nil, fmt.Errorf("service %s not found in sdk session", serviceID)
	}

	cacheKey := serviceID + ":" + version
	entry, exists := c.serviceMetadataCache[cacheKey]
	c.mu.RUnlock()

	if exists && entry != nil {
		engine.RecordExecutionTiming(ctx, "service_metadata_cache_hit", 0)
		// Apply execution_policy overrides (base_url, rate_limit, etc.) at read
		// time so a workspace apply that changes these fields takes effect
		// immediately without requiring a cache eviction or Engine restart.
		// The cached entry itself is intentionally left unmodified so repeated
		// reads don't accumulate stacked overrides.
		svcUUID, err := uuid.Parse(serviceID)
		if err != nil {
			return entry, nil
		}
		versionID, _ := parseServiceVersionID(version)
		return c.applyExecutionPolicyOverride(ctx, svcUUID, versionID, entry), nil
	}

	return nil, fmt.Errorf("service metadata not found in connection cache")
}

func (c *LocalObjectCache) GetEndpoint(ctx context.Context, appID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	c.mu.RLock()
	versions, ok := c.sdkVersions[appID]
	if !ok {
		c.mu.RUnlock()
		return nil, fmt.Errorf("sdk session %s not initialized", appID)
	}
	version, ok := versions[serviceID]
	if !ok {
		c.mu.RUnlock()
		return nil, fmt.Errorf("service %s not found in sdk session", serviceID)
	}

	cacheKey := serviceID + ":" + version + ":" + endpointName

	if ep, exists := c.endpointMetadataCache[cacheKey]; exists {
		c.mu.RUnlock()
		engine.RecordExecutionTiming(ctx, "endpoint_cache_hit", 0)
		return ep, nil
	}

	svcCacheKey := serviceID + ":" + version
	entry, exists := c.serviceMetadataCache[svcCacheKey]
	c.mu.RUnlock()

	if !exists || entry == nil {
		return nil, fmt.Errorf("service metadata not in connection cache for endpoint fetch")
	}

	svcUUID, err := uuid.Parse(serviceID)
	if err != nil {
		return nil, err
	}

	fetchStarted := time.Now()
	versionID, hasVersionID := parseServiceVersionID(version)
	ep, source, err := c.fetchEndpointByName(ctx, svcUUID, versionID, hasVersionID, version, endpointName)
	engine.MeasureExecutionTiming(ctx, "endpoint_contract_fetch", fetchStarted)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.endpointMetadataCache[cacheKey] = ep
	c.mu.Unlock()
	slog.InfoContext(ctx, "Endpoint metadata fetched for SDK cache",
		slog.String("service_id", serviceID),
		slog.String("service_version_id", version),
		slog.String("endpoint_name", endpointName),
		slog.String("source", source),
		slog.Float64("contract_fetch_ms", float64(time.Since(fetchStarted).Microseconds())/1000),
	)

	return ep, nil
}

// ListEndpointsForSelection resolves the endpoints one SDK selection grants.
// Snapshot-backed reads filter endpoint IDs in SQL; Registry fallback keeps
// old pre-snapshot activations usable until a refresh materializes them.
// Results are also written into endpointMetadataCache so a later per-call
// GetEndpoint for the same name is a cache hit.
func (c *LocalObjectCache) ListEndpointsForSelection(ctx context.Context, appID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	all, err := c.listEndpointsForSelection(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("fetch service operations for %s: %w", sel.ServiceID, err)
	}

	svcID := sel.ServiceID.String()
	version := sel.ServiceVersionID.String()
	c.mu.Lock()
	for i := range all {
		cacheKey := svcID + ":" + version + ":" + all[i].Name
		c.endpointMetadataCache[cacheKey] = &all[i]
	}
	c.mu.Unlock()

	return all, nil
}

// FetchServiceMetadata implements middleware.MetadataFetcher, letting
// RuntimeEnforcer (the HTTP proxy path) resolve rate_limit/retry_config/
// pagination/incoming_webhook_config from the cached runtime contract
// snapshot -- falling back to a live Registry call only when no snapshot
// exists yet -- instead of always hitting the Registry directly on every
// proxied request. This is the same resolution fetchServiceMetadata already
// gives SDK dispatch; exporting it here is what makes it one choke point
// instead of two.
func (c *LocalObjectCache) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	metadata, _, err := c.fetchServiceMetadata(ctx, serviceID, version)
	return metadata, err
}

func (c *LocalObjectCache) fetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, string, error) {
	versionID, hasVersionID := parseServiceVersionID(version)
	if hasVersionID {
		metadata, err := c.fetchSnapshotMetadata(ctx, serviceID, versionID)
		if err == nil {
			return c.applyExecutionPolicyOverride(ctx, serviceID, versionID, metadata), "snapshot", nil
		}
		if !isSnapshotAbsent(err) {
			return nil, "", err
		}
	}
	// Why fallback exists: old activations will not have snapshots until they
	// are refreshed, so phase 1 can roll forward without stranding live SDKs.
	metadata, err := c.registryClient.FetchServiceMetadata(ctx, serviceID, version)
	if err != nil {
		return nil, "", err
	}
	return c.applyExecutionPolicyOverride(ctx, serviceID, versionID, metadata), "registry", nil
}

// applyExecutionPolicyOverride is the single point where a workspace's local
// execution_policy declaration (fused_workspace_execution_policies -- set for
// a service the workspace does not own, or has not published) takes effect
// over the Registry-sourced snapshot/live value, per field, when present.
// This is resolved here rather than at snapshot-fetch time (see
// plans/plan-service-config-restructure.md's local-enforcement gap and the
// design decision to mirror GetEffectiveWorkspaceProfile's read-time
// resolution) so every caller of fetchServiceMetadata -- SDK dispatch via
// fusedToService, and the HTTP proxy via RuntimeEnforcer.fetchMetadata --
// gets the override without each needing its own lookup. serviceVersionID may
// be uuid.Nil when the caller only had a version name to resolve with; that
// still correctly falls back to the service-default override tier (see
// GetEffectiveWorkspaceExecutionPolicyOverride).
//
// A type assertion, not an added Store interface method, is how this reaches
// the override store -- the same rollout idiom secret_resolver.go's
// loadBucketBindings uses for WorkspaceProfileStore. Any db that doesn't
// implement it (e.g. a narrow test double) silently skips the overlay rather
// than failing metadata resolution.
func (c *LocalObjectCache) applyExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID, metadata *fusedobject.ServiceMetadata) *fusedobject.ServiceMetadata {
	if metadata == nil {
		return metadata
	}
	overrideStore, ok := c.db.(interface {
		GetEffectiveWorkspaceExecutionPolicyOverride(context.Context, uuid.UUID, uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error)
	})
	if !ok {
		return metadata
	}
	override, err := overrideStore.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, serviceID, serviceVersionID)
	if err != nil {
		slog.WarnContext(ctx, "execution policy override lookup failed, serving snapshot value",
			slog.String("service_id", serviceID.String()), slog.Any("error", err))
		return metadata
	}
	if override == nil {
		return metadata
	}
	return mergeExecutionPolicyOverride(metadata, override)
}

func mergeExecutionPolicyOverride(metadata *fusedobject.ServiceMetadata, override *store.WorkspaceExecutionPolicyOverride) *fusedobject.ServiceMetadata {
	overridden := *metadata
	if override.RateLimit != nil {
		overridden.RateLimit = override.RateLimit
	}
	if override.RetryConfig != nil {
		overridden.RetryConfig = override.RetryConfig
	}
	if override.TimeoutMs != nil {
		overridden.TimeoutMs = override.TimeoutMs
	}
	if override.Pagination != nil {
		overridden.Pagination = override.Pagination
	}
	if override.EventExtractionPath != nil {
		overridden.EventExtractionPath = *override.EventExtractionPath
	}
	if override.BaseURL != nil {
		// This is the one field here that overrides a value fusedToService
		// (dispatch_map.go) reads directly off ServiceMetadata rather than
		// through a separate mapXxx conversion -- setting it here is
		// sufficient because every downstream consumer (dispatcher.go's
		// bindingBaseURL, SDK request building) already just reads
		// metadata.BaseURL with no override-awareness of its own needed.
		overridden.BaseURL = *override.BaseURL
		// We must also clear the servers list so resolveRuntimeEnvironment
		// doesn't prioritize an original IsDefault server over the override.
		overridden.Servers = nil
	}
	if override.IncomingWebhookConfig != nil {
		overridden.IncomingWebhookConfig = override.IncomingWebhookConfig
	}
	return &overridden
}

func (c *LocalObjectCache) fetchEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, hasVersionID bool, version, endpointName string) (*fusedobject.Endpoint, string, error) {
	if hasVersionID {
		endpoint, err := c.fetchSnapshotEndpointByName(ctx, serviceID, serviceVersionID, endpointName)
		if err == nil {
			return endpoint, "snapshot", nil
		}
		if !isSnapshotAbsent(err) {
			return nil, "", err
		}
	}
	endpoint, err := c.registryClient.FetchEndpointByName(ctx, serviceID, version, endpointName)
	return endpoint, "registry", err
}

func (c *LocalObjectCache) fetchEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, names []string) ([]fusedobject.Endpoint, string, error) {
	if len(names) == 0 {
		return nil, "none", nil
	}
	if endpoints, err := c.fetchSnapshotEndpointsByNames(ctx, serviceID, serviceVersionID, names); err == nil {
		return endpoints, "snapshot", nil
	} else if !isSnapshotAbsent(err) {
		return nil, "", err
	}
	endpoints, err := c.registryClient.FetchEndpointsByNames(ctx, serviceID, serviceVersionID, names)
	return endpoints, "registry", err
}

func (c *LocalObjectCache) listEndpointsForSelection(ctx context.Context, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	if sel.SelectAll {
		return c.listAllServiceOperations(ctx, sel.ServiceID, sel.ServiceVersionID)
	}
	if len(sel.EndpointIDs) == 0 {
		return nil, nil
	}
	if endpoints, err := c.listSnapshotEndpointsByIDs(ctx, sel.ServiceID, sel.ServiceVersionID, sel.EndpointIDs); err == nil {
		return endpoints, nil
	} else if !isSnapshotAbsent(err) {
		return nil, err
	}
	// Why fallback uses Registry's unfiltered operation list here: this path is
	// only for pre-snapshot activations, and the subsequent ID filter preserves
	// the SDK's existing scope semantics until refresh creates a local copy.
	all, err := c.registryClient.FetchServiceOperations(ctx, sel.ServiceID, sel.ServiceVersionID)
	if err != nil {
		return nil, err
	}
	return filterEndpointsByIDs(all, sel.EndpointIDs), nil
}

func (c *LocalObjectCache) listAllServiceOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	if endpoints, err := c.listSnapshotServiceOperations(ctx, serviceID, serviceVersionID); err == nil {
		return endpoints, nil
	} else if !isSnapshotAbsent(err) {
		return nil, err
	}
	return c.registryClient.FetchServiceOperations(ctx, serviceID, serviceVersionID)
}

func (c *LocalObjectCache) fetchSnapshotMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.GetServiceContractMetadata(ctx, serviceID, serviceVersionID)
}

func (c *LocalObjectCache) fetchSnapshotEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.GetServiceContractEndpointByName(ctx, serviceID, serviceVersionID, endpointName)
}

func (c *LocalObjectCache) fetchSnapshotEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.ListServiceContractEndpointsByNames(ctx, serviceID, serviceVersionID, endpointNames)
}

func (c *LocalObjectCache) listSnapshotEndpointsByIDs(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointIDs []uuid.UUID) ([]fusedobject.Endpoint, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.ListServiceContractEndpointsByIDs(ctx, serviceID, serviceVersionID, endpointIDs)
}

func (c *LocalObjectCache) listSnapshotServiceOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.ListServiceContractOperations(ctx, serviceID, serviceVersionID)
}

func (c *LocalObjectCache) serviceContractStore() store.ServiceContractSnapshotStore {
	contractStore, ok := c.db.(store.ServiceContractSnapshotStore)
	if !ok {
		return nil
	}
	return contractStore
}

func parseServiceVersionID(version string) (uuid.UUID, bool) {
	id, err := uuid.Parse(version)
	return id, err == nil && id != uuid.Nil
}

func isSnapshotAbsent(err error) bool {
	return errors.Is(err, store.ErrServiceContractSnapshotNotFound)
}

// filterEndpointsByIDs keeps only the endpoints whose ID appears in ids,
// preserving all's order. It is only used for Registry fallback during the
// migration window; snapshot-backed scoped reads push this filter into SQL.
func filterEndpointsByIDs(all []fusedobject.Endpoint, ids []uuid.UUID) []fusedobject.Endpoint {
	wanted := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	matched := make([]fusedobject.Endpoint, 0, len(ids))
	for _, ep := range all {
		if _, ok := wanted[ep.ID]; ok {
			matched = append(matched, ep)
		}
	}
	return matched
}

func (c *LocalObjectCache) InvalidateAppRuntime(appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fast path: if the SDK isn't in our scopes cache, there's nothing to invalidate
	if _, exists := c.scopes[appID]; !exists {
		return
	}

	// We must decrement objectRefCounts for all service versions referenced by this SDK's selections.
	var selections []models.SDKSelection
	_ = json.Unmarshal(c.scopes[appID], &selections)
	versions := c.sdkVersions[appID]
	for _, sel := range selections {
		svcID := sel.ServiceID.String()
		version := versions[svcID]
		cacheKey := svcID + ":" + version

		c.objectRefCounts[cacheKey]--
		if c.objectRefCounts[cacheKey] <= 0 {
			delete(c.serviceMetadataCache, cacheKey)
			delete(c.objectRefCounts, cacheKey)

			// Evict associated endpoints
			prefix := cacheKey + ":"
			for k := range c.endpointMetadataCache {
				if len(k) > len(prefix) && k[:len(prefix)] == prefix {
					delete(c.endpointMetadataCache, k)
				}
			}
			slog.Info("Evicted ServiceMetadata from cache (SDK scope invalidated)", slog.String("serviceID", svcID), slog.String("version", version))
		}
	}

	// Remove the scope completely
	delete(c.scopes, appID)
	delete(c.scopeRefCounts, appID)
	delete(c.sdkVersions, appID)
	slog.Info("SDK Scope invalidated and evicted from cache", slog.String("appID", appID))
}

func (c *LocalObjectCache) Invalidate(serviceID string) {
	c.mu.Lock()
	prefix := serviceID + ":"

	for k := range c.serviceMetadataCache {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(c.serviceMetadataCache, k)
		}
	}

	// Invalidate endpoints too
	for k := range c.endpointMetadataCache {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(c.endpointMetadataCache, k)
		}
	}
	c.mu.Unlock()
	slog.Info("Invalidated local ServiceMetadata cache", slog.String("serviceID", serviceID))
}

func (c *LocalObjectCache) GetAppRuntime(ctx context.Context, appID string) (string, []byte, error) {
	c.mu.RLock()
	scopeJSON, exists := c.scopes[appID]
	c.mu.RUnlock()

	if exists {
		return "", scopeJSON, nil
	}

	return "", nil, fmt.Errorf("scope not found in connection cache")
}
