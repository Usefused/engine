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
	Invalidate(serviceID string)
	InvalidateAppRuntime(appID string)
	GetAppRuntime(ctx context.Context, appID string) (string, []byte, error)
}

type LocalObjectCache struct {
	db                    store.Store
	mu                    sync.RWMutex
	serviceMetadataCache  map[string]*fusedobject.ServiceMetadata
	endpointMetadataCache map[string]*fusedobject.Endpoint // cache key: serviceID:endpointName
	scopes                map[string][]byte
	sdkVersions           map[string]map[string]string
	scopeRefCounts        map[string]int
	objectRefCounts       map[string]int
}

// NewLocalObjectCache accepts only the Engine store because execution must
// never regain a live Registry fallback through constructor wiring.
func NewLocalObjectCache(db store.Store) *LocalObjectCache {
	return &LocalObjectCache{
		db:                    db,
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
	selections, err := models.DecodeAppSelections(scope.ScopeSchemaVersion, scope.Selections)
	if err != nil {
		return nil, nil, fmt.Errorf("ScopeError: unsupported app selection schema")
	}
	return scope.Selections, selections, nil
}

// sdkSelectionCacheEntry carries the exact immutable identity used by both the
// store batches and the in-memory cache commit.
type sdkSelectionCacheEntry struct {
	selection models.SDKSelection
	ref       store.ServiceContractMetadataRef
	cacheKey  string
}

// sdkSelectionCachePlan contains query inputs and the final version map so
// validation and I/O can finish before shared cache state changes.
type sdkSelectionCachePlan struct {
	entries         []sdkSelectionCacheEntry
	missingMetadata []store.ServiceContractMetadataRef
	namedSelections []store.ServiceContractEndpointSelection
	versions        map[string]string
}

// cacheSDKSelections preloads one app scope with a constant number of store
// reads and commits only after every required local snapshot row is present.
func (c *LocalObjectCache) cacheSDKSelections(ctx context.Context, appID string, selections []models.SDKSelection) error {
	plan, err := c.planSDKSelectionCache(selections)
	// Invalid unpinned selections are rejected before any database work.
	if err != nil {
		return err
	}
	metadata, err := c.loadSDKSelectionMetadata(ctx, plan.missingMetadata)
	// Snapshot batch failures leave the staged plan uncommitted.
	if err != nil {
		return err
	}
	endpoints, err := c.loadSDKSelectionEndpoints(ctx, plan.namedSelections)
	// Endpoint batch failures likewise leave shared entries and refcounts intact.
	if err != nil {
		return err
	}
	// Endpoint completeness is checked before commit so a missing named
	// operation cannot leave metadata or refcounts retained by a failed app.
	if err := validateSDKSelectionEndpoints(plan.entries, endpoints); err != nil {
		return err
	}
	c.commitSDKSelectionCache(appID, plan, metadata, endpoints)
	return nil
}

// planSDKSelectionCache validates all pinned versions and deduplicates only
// request identities; persisted rows remain filtered by the set-based SQL.
func (c *LocalObjectCache) planSDKSelectionCache(selections []models.SDKSelection) (sdkSelectionCachePlan, error) {
	plan := sdkSelectionCachePlan{entries: make([]sdkSelectionCacheEntry, 0, len(selections)), versions: make(map[string]string)}
	missing := make(map[store.ServiceContractMetadataRef]struct{}, len(selections))
	for index, selection := range selections {
		version, err := selectionVersionIdentity(selection)
		// All selections are validated before a plan can reach a store batch.
		if err != nil {
			return sdkSelectionCachePlan{}, err
		}
		serviceID := selection.ServiceID.String()
		cacheKey := serviceID + ":" + version
		ref := store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
		plan.entries = append(plan.entries, sdkSelectionCacheEntry{selection: selection, ref: ref, cacheKey: cacheKey})
		plan.versions[serviceID] = version
		// Existing shared metadata already carries the workspace overlay and does
		// not need another database read for this app connection.
		if _, cached := c.serviceMetadataCache[cacheKey]; !cached {
			// Duplicate selections retain distinct refcounts but share one metadata
			// and policy batch input.
			if _, planned := missing[ref]; !planned {
				missing[ref] = struct{}{}
				plan.missingMetadata = append(plan.missingMetadata, ref)
			}
		}
		// Empty operation names represent select-all scopes and intentionally
		// retain lazy endpoint loading rather than materializing full contracts.
		if len(selection.OperationNames) > 0 {
			plan.namedSelections = append(plan.namedSelections, store.ServiceContractEndpointSelection{
				SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
				SelectAll: true, EndpointNames: selection.OperationNames,
			})
		}
	}
	return plan, nil
}

// loadSDKSelectionMetadata fetches all missing immutable metadata and applies
// workspace policy rows obtained by a second fixed-count batch.
func (c *LocalObjectCache) loadSDKSelectionMetadata(ctx context.Context, refs []store.ServiceContractMetadataRef) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	result := make(map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, len(refs))
	// A fully warm metadata set avoids both metadata and policy store reads.
	if len(refs) == 0 {
		return result, nil
	}
	batchStore, ok := c.db.(store.ServiceContractMetadataBatchStore)
	// A scalar compatibility fallback would restore N+1 behavior, so stores
	// used for execution must expose the batch capability.
	if !ok {
		return nil, errors.New("service contract metadata batch store is unavailable")
	}
	metadata, err := batchStore.ListServiceContractMetadata(ctx, refs)
	// Metadata is mandatory, unlike optional workspace policy overlays.
	if err != nil {
		return nil, err
	}
	overrides := c.loadSDKSelectionPolicyOverrides(ctx, refs)
	for _, ref := range refs {
		value := metadata[ref]
		// Every requested reference must be represented even when a test or
		// alternate adapter does not enforce PostgreSQL's strict left join.
		if err := validateExecutionContractMetadata(value); err != nil {
			return nil, err
		}
		result[ref] = mergeExecutionPolicyOverride(value, overrides[ref])
	}
	return result, nil
}

// loadSDKSelectionPolicyOverrides preserves the existing soft-failure policy:
// immutable snapshot values remain executable when a local override read fails.
func (c *LocalObjectCache) loadSDKSelectionPolicyOverrides(ctx context.Context, refs []store.ServiceContractMetadataRef) map[store.ServiceContractMetadataRef]*store.WorkspaceExecutionPolicyOverride {
	result := make(map[store.ServiceContractMetadataRef]*store.WorkspaceExecutionPolicyOverride, len(refs))
	batchStore, ok := c.db.(store.WorkspaceExecutionPolicyBatchStore)
	// Narrow test adapters and staged deployments may not expose workspace
	// overlays, in which case immutable snapshot policy remains authoritative.
	if !ok {
		return result
	}
	policyRefs := make([]store.WorkspaceExecutionPolicyRef, len(refs))
	for index, ref := range refs {
		policyRefs[index] = store.WorkspaceExecutionPolicyRef{ServiceID: ref.ServiceID, ServiceVersionID: ref.ServiceVersionID}
	}
	overrides, err := batchStore.GetEffectiveWorkspaceExecutionPolicyOverrides(ctx, policyRefs)
	// A local policy read failure is observable but does not make an immutable
	// service contract unusable, matching single-metadata runtime fetches.
	if err != nil {
		slog.WarnContext(ctx, "execution policy override batch lookup failed, serving snapshot values", slog.Any("error", err))
		return result
	}
	for ref, override := range overrides {
		result[store.ServiceContractMetadataRef{ServiceID: ref.ServiceID, ServiceVersionID: ref.ServiceVersionID}] = override
	}
	return result
}

// loadSDKSelectionEndpoints resolves every named selection in one local
// snapshot query while leaving select-all selections absent and lazy.
func (c *LocalObjectCache) loadSDKSelectionEndpoints(ctx context.Context, selections []store.ServiceContractEndpointSelection) (map[int][]fusedobject.Endpoint, error) {
	result := make(map[int][]fusedobject.Endpoint, len(selections))
	// No named selections means endpoint prewarming requires no query.
	if len(selections) == 0 {
		return result, nil
	}
	contractStore := c.serviceContractStore()
	// Runtime execution cannot consult Registry when its local contract store
	// capability is absent.
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	matches, err := contractStore.ListServiceContractEndpointsForSelections(ctx, selections, nil)
	// A single failed endpoint batch aborts the staged app connection.
	if err != nil {
		return nil, err
	}
	for _, match := range matches {
		result[match.SelectionIndex] = append(result[match.SelectionIndex], match.Endpoint)
	}
	return result, nil
}

// validateSDKSelectionEndpoints ensures every immutable named operation was
// applied locally before the cache transaction is committed.
func validateSDKSelectionEndpoints(entries []sdkSelectionCacheEntry, endpoints map[int][]fusedobject.Endpoint) error {
	for index, entry := range entries {
		// Select-all selections intentionally have no prewarm completeness check.
		if len(entry.selection.OperationNames) == 0 {
			continue
		}
		// Named operations are immutable app capabilities and must all exist in
		// the Engine-local snapshot before execution begins.
		if !containsAllEndpointNames(endpoints[index], entry.selection.OperationNames) {
			return fmt.Errorf("prefetch endpoints for %s: %w", entry.ref.ServiceID, store.ErrServiceContractEndpointNotFound)
		}
	}
	return nil
}

// commitSDKSelectionCache applies a fully validated plan while ConnectSDK
// holds the cache write lock, preserving refcounts shared by other apps.
func (c *LocalObjectCache) commitSDKSelectionCache(appID string, plan sdkSelectionCachePlan, metadata map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, endpoints map[int][]fusedobject.Endpoint) {
	c.sdkVersions[appID] = plan.versions
	for _, entry := range plan.entries {
		// Only missing rows were fetched, so assigning from the staged map cannot
		// replace metadata already retained by another connected app.
		if value, missing := metadata[entry.ref]; missing {
			c.serviceMetadataCache[entry.cacheKey] = value
		}
		c.objectRefCounts[entry.cacheKey]++
	}
	for index, values := range endpoints {
		entry := plan.entries[index]
		for endpointIndex := range values {
			endpoint := values[endpointIndex]
			c.endpointMetadataCache[entry.cacheKey+":"+endpoint.Name] = &endpoint
		}
	}
}

func containsAllEndpointNames(endpoints []fusedobject.Endpoint, names []string) bool {
	available := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		available[endpoint.Name] = struct{}{}
	}
	for _, name := range names {
		if _, exists := available[name]; !exists {
			return false
		}
	}
	return true
}

func selectionVersionIdentity(sel models.SDKSelection) (string, error) {
	if sel.ServiceVersionID != uuid.Nil {
		return sel.ServiceVersionID.String(), nil
	}
	return "", fmt.Errorf("ScopeError: service_version_id required for service %s", sel.ServiceID)
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
		if err := validateExecutionContractMetadata(entry); err != nil {
			return nil, err
		}
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
	if !hasVersionID {
		return nil, fmt.Errorf("%w: service_version_id is required", store.ErrServiceContractSnapshotNotFound)
	}
	ep, source, err := c.fetchEndpointByName(ctx, svcUUID, versionID, endpointName)
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

// ListEndpointsForSelections builds every MCP catalog through one snapshot
// query. PostgreSQL always applies app scope and conditionally intersects the
// token operation names, avoiding both N+1 lookups and Go-side filtering.
func (c *LocalObjectCache) ListEndpointsForSelections(ctx context.Context, selections []models.SDKSelection, names []string) (map[int][]fusedobject.Endpoint, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	requested := make([]store.ServiceContractEndpointSelection, 0, len(selections))
	for index, selection := range selections {
		requested = append(requested, store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
			SelectAll: selection.SelectAll, EndpointIDs: selection.EndpointIDs, OperationNames: selection.OperationNames,
		})
	}
	matches, err := contractStore.ListServiceContractEndpointsForSelections(ctx, requested, names)
	if err != nil {
		return nil, err
	}
	grouped := make(map[int][]fusedobject.Endpoint, len(selections))
	for _, match := range matches {
		grouped[match.SelectionIndex] = append(grouped[match.SelectionIndex], match.Endpoint)
	}
	return grouped, nil
}

// FetchServiceMetadata resolves only the immutable local runtime snapshot.
// Registry is a control-plane source; consulting it during execution would let
// an app run a contract that was never applied to this Engine.
func (c *LocalObjectCache) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	metadata, _, err := c.fetchServiceMetadata(ctx, serviceID, version)
	return metadata, err
}

func (c *LocalObjectCache) fetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, string, error) {
	versionID, hasVersionID := parseServiceVersionID(version)
	if !hasVersionID {
		return nil, "", fmt.Errorf("%w: service_version_id is required", store.ErrServiceContractSnapshotNotFound)
	}
	metadata, err := c.fetchSnapshotMetadata(ctx, serviceID, versionID)
	if err != nil {
		return nil, "", err
	}
	if err := validateExecutionContractMetadata(metadata); err != nil {
		return nil, "", err
	}
	return c.applyExecutionPolicyOverride(ctx, serviceID, versionID, metadata), "snapshot", nil
}

// applyExecutionPolicyOverride is the single point where a workspace's local
// execution_policy declaration takes effect over the immutable service
// snapshot. Resolving it at cache read time makes an applied local change
// visible without evicting and rebuilding the canonical snapshot.
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

// mergeExecutionPolicyOverride copies immutable metadata only when a workspace
// row has fields to overlay, keeping the canonical snapshot unmodified.
func mergeExecutionPolicyOverride(metadata *fusedobject.ServiceMetadata, override *store.WorkspaceExecutionPolicyOverride) *fusedobject.ServiceMetadata {
	// An absent workspace row leaves the immutable contract pointer unchanged.
	if override == nil {
		return metadata
	}
	overridden := *metadata
	// Each non-nil field represents an intentional workspace override; nil
	// continues to inherit the provider's immutable contract value.
	if override.RateLimit != nil {
		overridden.RateLimit = override.RateLimit
	}
	// Retry configuration follows the same per-field inheritance rule.
	if override.RetryConfig != nil {
		overridden.RetryConfig = override.RetryConfig
	}
	// Timeout remains provider-owned unless explicitly set for this workspace.
	if override.TimeoutMs != nil {
		overridden.TimeoutMs = override.TimeoutMs
	}
	// Pagination can be replaced independently of other execution policy fields.
	if override.Pagination != nil {
		overridden.Pagination = override.Pagination
	}
	// Event extraction overrides affect webhook parsing without replacing the
	// surrounding metadata document.
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
	// Server variables remain inherited unless the workspace supplied a map.
	if override.ServerVariables != nil {
		overridden.ServerVariables = override.ServerVariables
	}
	// Incoming webhook settings can be overridden without changing dispatch.
	if override.IncomingWebhookConfig != nil {
		overridden.IncomingWebhookConfig = override.IncomingWebhookConfig
	}
	return &overridden
}

func (c *LocalObjectCache) fetchEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, string, error) {
	if serviceVersionID == uuid.Nil {
		return nil, "", fmt.Errorf("%w: service_version_id is required", store.ErrServiceContractSnapshotNotFound)
	}
	endpoint, err := c.fetchSnapshotEndpointByName(ctx, serviceID, serviceVersionID, endpointName)
	return endpoint, "snapshot", err
}

func (c *LocalObjectCache) fetchEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, names []string) ([]fusedobject.Endpoint, string, error) {
	if len(names) == 0 {
		return nil, "none", nil
	}
	endpoints, err := c.fetchSnapshotEndpointsByNames(ctx, serviceID, serviceVersionID, names)
	return endpoints, "snapshot", err
}

func (c *LocalObjectCache) fetchSnapshotMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	contractStore := c.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return contractStore.GetServiceContractMetadata(ctx, serviceID, serviceVersionID)
}

func validateExecutionContractMetadata(metadata *fusedobject.ServiceMetadata) error {
	if metadata == nil {
		return store.ErrServiceContractSnapshotNotFound
	}
	if err := fusedobject.ValidateExecutionContractEnvelope(metadata.ExecutionContractEnvelope); err != nil {
		return fmt.Errorf("runtime service contract: %w", err)
	}
	return nil
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
