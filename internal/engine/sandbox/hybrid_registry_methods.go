package sandbox

import (
	"context"
	"errors"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// verifyHybridServiceExists checks pinned provenance first, otherwise tries Local Registry then permission-gated Cloud.
func (c *HTTPRegistryClient) verifyHybridServiceExists(ctx context.Context, serviceID uuid.UUID, apiKey string) (string, string, string, uuid.UUID, error) {
	pinnedClient, pinnedSource, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return "", "", "", uuid.Nil, err
	}
	// Route directly to the authoritative Registry when the service is already active in this workspace.
	if pinnedClient != nil {
		name, slug, versionTag, versionID, verifyErr := pinnedClient.VerifyServiceExists(ctx, serviceID, apiKey)
		if verifyErr == nil {
			recordContextRegistrySource(ctx, pinnedSource)
		}
		return name, slug, versionTag, versionID, verifyErr
	}

	name, slug, versionTag, versionID, localErr := c.local.VerifyServiceExists(ctx, serviceID, apiKey)
	// Return immediately whenever Local Registry resolves the service.
	if localErr == nil {
		recordContextRegistrySource(ctx, RegistrySourceLocal)
		return name, slug, versionTag, versionID, nil
	}
	// Non-404 transport failures on Local Registry fail closed instead of masking an internal outage via Cloud.
	if !errors.Is(localErr, ErrServiceNotFound) {
		return "", "", "", uuid.Nil, localErr
	}
	// Fall back to Fused Cloud Registry only when the caller holds catalogue.cloud.read.
	if !c.allowCloudFallback(ctx) {
		return "", "", "", uuid.Nil, ErrServiceNotFound
	}
	name, slug, versionTag, versionID, cloudErr := c.cloudClone().VerifyServiceExists(ctx, serviceID, apiKey)
	if cloudErr == nil {
		recordContextRegistrySource(ctx, RegistrySourceCloud)
	}
	return name, slug, versionTag, versionID, cloudErr
}

// resolveHybridServiceIDsBySlugs queries Local Registry first and resolves remaining misses on Cloud when permitted.
func (c *HTTPRegistryClient) resolveHybridServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	normalized := uniqueNonEmptyStrings(slugs)
	// Empty inputs require no network round trips.
	if len(normalized) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		if !c.allowCloudFallback(ctx) {
			return map[string]uuid.UUID{}, nil
		}
		return c.cloudClone().ResolveServiceIDsBySlugs(ctx, normalized, apiKey)
	}

	resolved, err := c.local.ResolveServiceIDsBySlugs(ctx, normalized, apiKey)
	if err != nil {
		return nil, err
	}
	missing := unresolvedSlugs(normalized, resolved)
	// Skip Cloud Registry completely when Local Registry resolved every requested slug or Cloud access is denied.
	if len(missing) == 0 || !c.allowCloudFallback(ctx) {
		return resolved, nil
	}
	cloudResolved, err := c.cloudClone().ResolveServiceIDsBySlugs(ctx, missing, apiKey)
	if err != nil {
		return nil, err
	}
	for slug, id := range cloudResolved {
		// Preserve Local Registry precedence if a server returned canonical keys.
		if _, exists := resolved[slug]; !exists {
			resolved[slug] = id
		}
	}
	return resolved, nil
}

// unresolvedSlugs returns requested slug keys absent from resolved.
func unresolvedSlugs(requested []string, resolved map[string]uuid.UUID) []string {
	missing := make([]string, 0)
	for _, slug := range requested {
		if _, ok := resolved[slug]; !ok {
			missing = append(missing, slug)
		}
	}
	return missing
}

// searchHybridCatalogue enforces strict fallback: Cloud is queried only when Local returns zero hits and permission allows.
func (c *HTTPRegistryClient) searchHybridCatalogue(ctx context.Context, searchQuery string, page int, limit int) ([]CatalogueService, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		if !c.allowCloudFallback(ctx) {
			return []CatalogueService{}, nil
		}
		return c.cloudClone().SearchCatalogue(ctx, searchQuery, page, limit)
	}
	localResults, err := c.local.SearchCatalogue(ctx, searchQuery, page, limit)
	if err != nil {
		return nil, err
	}
	// Strict fallback returns Local hits directly without querying Fused Cloud Registry.
	if len(localResults) > 0 || !c.allowCloudFallback(ctx) {
		recordContextRegistrySource(ctx, RegistrySourceLocal)
		return localResults, nil
	}
	cloudResults, err := c.cloudClone().SearchCatalogue(ctx, searchQuery, page, limit)
	if err == nil && len(cloudResults) > 0 {
		recordContextRegistrySource(ctx, RegistrySourceCloud)
	}
	return cloudResults, err
}

// fetchHybridServiceVersionRevisions partitions version lookups between Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridServiceVersionRevisions(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	localRefs, cloudRefs := c.partitionVersionRefs(ctx, refs)
	out := make([]ServiceVersionRevision, 0, len(refs))
	unresolved := make([]ServiceVersionRef, 0)

	if len(localRefs) > 0 {
		localRevs, err := c.local.FetchServiceVersionRevisions(ctx, localRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, localRevs...)
		unresolved = missingVersionRefs(localRefs, localRevs)
	}
	// Allow unpinned misses from Local to fall back to Cloud when the actor holds catalogue.cloud.read.
	if len(unresolved) > 0 && c.allowCloudFallback(ctx) {
		cloudRefs = append(cloudRefs, unresolved...)
	}
	if len(cloudRefs) > 0 && c.allowCloudFallback(ctx) {
		cloudRevs, err := c.cloudClone().FetchServiceVersionRevisions(ctx, cloudRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, cloudRevs...)
	}
	return out, nil
}

// partitionVersionRefs splits version references into Local-first/Local-pinned vs Cloud-pinned slices.
func (c *HTTPRegistryClient) partitionVersionRefs(ctx context.Context, refs []ServiceVersionRef) (localRefs, cloudRefs []ServiceVersionRef) {
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		return nil, refs
	}
	sources := c.resolveBatchSources(ctx, versionRefServiceIDs(refs))
	for _, ref := range refs {
		if sources[ref.ServiceID] == RegistrySourceCloud {
			cloudRefs = append(cloudRefs, ref)
		} else {
			localRefs = append(localRefs, ref)
		}
	}
	return localRefs, cloudRefs
}

// missingVersionRefs identifies requested service+version pairs absent from revisions.
func missingVersionRefs(requested []ServiceVersionRef, revisions []ServiceVersionRevision) []ServiceVersionRef {
	found := make(map[ServiceVersionRef]bool, len(revisions))
	for _, rev := range revisions {
		found[ServiceVersionRef{ServiceID: rev.ServiceID, Version: rev.Version}] = true
	}
	var missing []ServiceVersionRef
	for _, ref := range requested {
		if !found[ref] {
			missing = append(missing, ref)
		}
	}
	return missing
}

// versionRefServiceIDs extracts service UUIDs from a slice of ServiceVersionRef entries.
func versionRefServiceIDs(refs []ServiceVersionRef) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ServiceID)
	}
	return ids
}

// fetchHybridRuntimeContracts loads contracts from each service's pinned Registry, trying Local then Cloud for unpinned items.
func (c *HTTPRegistryClient) fetchHybridRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string, retainGeneration bool) ([]store.ServiceContractSnapshot, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		if !c.allowCloudFallback(ctx) {
			return nil, ErrServiceNotFound
		}
		snap, err := c.cloudClone().fetchRuntimeContracts(ctx, versions, apiKey, retainGeneration)
		if err == nil && len(snap) > 0 {
			recordContextRegistrySource(ctx, RegistrySourceCloud)
		}
		return snap, err
	}

	localBatch, cloudBatch, unpinned := c.partitionWorkspaceVersions(ctx, versions)
	out := make([]store.ServiceContractSnapshot, 0, len(versions))

	if len(localBatch) > 0 {
		snaps, err := c.local.fetchRuntimeContracts(ctx, localBatch, apiKey, retainGeneration)
		if err != nil {
			return nil, err
		}
		out = append(out, snaps...)
	}
	for _, item := range unpinned {
		snap, source, err := c.fetchUnpinnedRuntimeContract(ctx, item, apiKey, retainGeneration)
		if err != nil {
			return nil, err
		}
		recordContextRegistrySource(ctx, source)
		out = append(out, snap)
	}
	if len(cloudBatch) > 0 {
		if !c.allowCloudFallback(ctx) {
			return nil, ErrServiceNotFound
		}
		snaps, err := c.cloudClone().fetchRuntimeContracts(ctx, cloudBatch, apiKey, retainGeneration)
		if err != nil {
			return nil, err
		}
		out = append(out, snaps...)
	}
	return out, nil
}

// fetchUnpinnedRuntimeContract tries Local Registry first and falls back to Cloud when Local returns 0 contracts and permission allows.
func (c *HTTPRegistryClient) fetchUnpinnedRuntimeContract(ctx context.Context, item store.WorkspaceServiceVersion, apiKey string, retainGeneration bool) (store.ServiceContractSnapshot, string, error) {
	localSnaps, err := c.local.fetchRuntimeContracts(ctx, []store.WorkspaceServiceVersion{item}, apiKey, retainGeneration)
	if err == nil && len(localSnaps) == 1 {
		return localSnaps[0], RegistrySourceLocal, nil
	}
	if err != nil && !c.allowCloudFallback(ctx) {
		return store.ServiceContractSnapshot{}, "", err
	}
	if !c.allowCloudFallback(ctx) {
		return store.ServiceContractSnapshot{}, "", ErrServiceNotFound
	}
	cloudSnaps, cloudErr := c.cloudClone().fetchRuntimeContracts(ctx, []store.WorkspaceServiceVersion{item}, apiKey, retainGeneration)
	if cloudErr != nil {
		return store.ServiceContractSnapshot{}, "", cloudErr
	}
	if len(cloudSnaps) != 1 {
		return store.ServiceContractSnapshot{}, "", ErrServiceNotFound
	}
	return cloudSnaps[0], RegistrySourceCloud, nil
}

// partitionWorkspaceVersions classifies version tuples into pinned-local, pinned-cloud, and unpinned slices.
func (c *HTTPRegistryClient) partitionWorkspaceVersions(ctx context.Context, versions []store.WorkspaceServiceVersion) (local, cloud, unpinned []store.WorkspaceServiceVersion) {
	if target := RegistryTargetFromContext(ctx); target == RegistrySourceLocal {
		return versions, nil, nil
	}
	ids := make([]uuid.UUID, 0, len(versions))
	for _, v := range versions {
		ids = append(ids, v.ServiceID)
	}
	sources := c.resolveBatchSources(ctx, ids)
	for _, v := range versions {
		switch sources[v.ServiceID] {
		case RegistrySourceLocal:
			local = append(local, v)
		case RegistrySourceCloud:
			cloud = append(cloud, v)
		default:
			unpinned = append(unpinned, v)
		}
	}
	return local, cloud, unpinned
}

// resolveBatchSources looks up workspace registry_source values, returning an empty map when unavailable.
func (c *HTTPRegistryClient) resolveBatchSources(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]string {
	if c.sourceResolver == nil || len(ids) == 0 {
		return map[uuid.UUID]string{}
	}
	sources, err := c.sourceResolver(ctx, ids)
	if err != nil {
		return map[uuid.UUID]string{}
	}
	return sources
}

// preflightHybridImport routes import preflight strictly to Local Registry unless Cloud publication was explicitly selected.
func (c *HTTPRegistryClient) preflightHybridImport(ctx context.Context, applyBody []byte) (*ImportContractPreflight, error) {
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		return c.cloudClone().PreflightImport(ctx, applyBody)
	}
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	return c.local.PreflightImport(ctx, applyBody)
}

// generateHybridSDK compiles SDKs on Local Registry whenever any selection is Local, keeping internal contracts private.
func (c *HTTPRegistryClient) generateHybridSDK(ctx context.Context, generation models.SDKGenerationRequest) (models.SDKGenerationResult, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return models.SDKGenerationResult{}, err
	}
	// Route exclusively Cloud-owned SDKs to Fused Cloud Registry.
	if c.allSelectionsCloudOwned(ctx, generation.Selections) {
		return c.cloudClone().GenerateSDK(ctx, generation)
	}
	enriched, err := c.attachCloudInlineContracts(ctx, generation)
	// Abort if any Cloud-pinned selection in a mixed SDK lacks an admitted local contract snapshot.
	if err != nil {
		return models.SDKGenerationResult{}, err
	}
	return c.local.GenerateSDK(ctx, enriched)
}

// attachCloudInlineContracts populates InlineContracts for any Cloud-pinned selection before sending a mixed build to Local Registry.
func (c *HTTPRegistryClient) attachCloudInlineContracts(ctx context.Context, generation models.SDKGenerationRequest) (models.SDKGenerationRequest, error) {
	// Skip lookup when the caller already attached inline contracts or no snapshot builder is wired.
	if len(generation.InlineContracts) > 0 || c.inlineBuilder == nil || len(generation.Selections) == 0 {
		return generation, nil
	}
	ids := make([]uuid.UUID, 0, len(generation.Selections))
	for _, sel := range generation.Selections {
		ids = append(ids, sel.ServiceID)
	}
	sources := c.resolveBatchSources(ctx, ids)
	var cloudRefs []store.ServiceContractMetadataRef
	for _, sel := range generation.Selections {
		// Export only Cloud-pinned selections; Local Registry loads its own native services directly.
		if sources[sel.ServiceID] == RegistrySourceCloud {
			cloudRefs = append(cloudRefs, store.ServiceContractMetadataRef{
				ServiceID:        sel.ServiceID,
				ServiceVersionID: sel.ServiceVersionID,
			})
		}
	}
	// Purely Local SDKs need no inline contract payloads.
	if len(cloudRefs) == 0 {
		return generation, nil
	}
	contracts, err := c.inlineBuilder(ctx, cloudRefs)
	// Fail closed if local snapshots for Cloud services are missing.
	if err != nil {
		return models.SDKGenerationRequest{}, err
	}
	generation.InlineContracts = contracts
	return generation, nil
}

// allSelectionsCloudOwned reports whether a non-empty selection slice references exclusively Cloud-pinned services.
func (c *HTTPRegistryClient) allSelectionsCloudOwned(ctx context.Context, selections []models.SDKSelection) bool {
	if len(selections) == 0 {
		return false
	}
	ids := make([]uuid.UUID, 0, len(selections))
	for _, sel := range selections {
		ids = append(ids, sel.ServiceID)
	}
	sources := c.resolveBatchSources(ctx, ids)
	for _, sel := range selections {
		if sources[sel.ServiceID] != RegistrySourceCloud {
			return false
		}
	}
	return true
}

// downloadHybridSDKPackage checks Local Registry first and falls back to Cloud Registry on 404 for Cloud-built SDKs.
func (c *HTTPRegistryClient) downloadHybridSDKPackage(ctx context.Context, appID uuid.UUID) (*http.Response, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	res, err := c.local.DownloadSDKPackage(ctx, appID)
	// Return any successful or non-404 response directly from Local Registry.
	if err != nil || res.StatusCode != http.StatusNotFound {
		return res, err
	}
	_ = res.Body.Close()
	return c.cloudClone().DownloadSDKPackage(ctx, appID)
}

// renewHybridSDKPackageLeases renews active SDK cache leases on both Local and Cloud Registries.
func (c *HTTPRegistryClient) renewHybridSDKPackageLeases(ctx context.Context, apps []models.SDKPackageLeaseRenewal) (int64, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return 0, err
	}
	localRenewed, localErr := c.local.RenewSDKPackageLeases(ctx, apps)
	cloudRenewed, _ := c.cloudClone().RenewSDKPackageLeases(ctx, apps)
	return localRenewed + cloudRenewed, localErr
}
