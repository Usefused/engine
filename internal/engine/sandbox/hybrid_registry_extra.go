package sandbox

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// clientForServiceMutation selects the authoritative Registry client for a service-scoped write operation.
func (c *HTTPRegistryClient) clientForServiceMutation(ctx context.Context, serviceID uuid.UUID) (*HTTPRegistryClient, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	// Route mutations for Cloud-pinned services to Fused Cloud Registry when permitted.
	if c.pinnedServiceSource(ctx, serviceID) == RegistrySourceCloud {
		if !c.allowCloudFallback(ctx) {
			return nil, ErrServiceNotFound
		}
		return c.cloudClone(), nil
	}
	return c.local, nil
}

// partitionServiceIDs splits service UUIDs into Local-default and Cloud-pinned slices.
func (c *HTTPRegistryClient) partitionServiceIDs(ctx context.Context, serviceIDs []uuid.UUID) (localIDs, cloudIDs []uuid.UUID) {
	if RegistryTargetFromContext(ctx) == RegistrySourceCloud {
		return nil, serviceIDs
	}
	sources := c.resolveBatchSources(ctx, serviceIDs)
	for _, id := range serviceIDs {
		// Only services explicitly pinned to Cloud bypass Local-first routing.
		if sources[id] == RegistrySourceCloud {
			cloudIDs = append(cloudIDs, id)
		} else {
			localIDs = append(localIDs, id)
		}
	}
	return localIDs, cloudIDs
}

// fetchHybridServiceVisibility resolves visibility maps across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridServiceVisibility(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) (map[uuid.UUID]ServiceVisibility, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]ServiceVisibility, len(serviceIDs))
	localIDs, cloudIDs := c.partitionServiceIDs(ctx, serviceIDs)
	if len(localIDs) > 0 {
		localVis, err := c.local.FetchServiceVisibility(ctx, localIDs, apiKey)
		if err != nil {
			return nil, err
		}
		for id, vis := range localVis {
			out[id] = vis
		}
		// Unpinned IDs absent from Local Registry may fall back to Cloud when permitted.
		for _, id := range localIDs {
			if _, ok := out[id]; !ok && c.allowCloudFallback(ctx) {
				cloudIDs = append(cloudIDs, id)
			}
		}
	}
	if len(cloudIDs) > 0 && c.allowCloudFallback(ctx) {
		cloudVis, err := c.cloudClone().FetchServiceVisibility(ctx, cloudIDs, apiKey)
		if err != nil {
			return nil, err
		}
		for id, vis := range cloudVis {
			out[id] = vis
		}
	}
	return out, nil
}

// fetchHybridServiceVersionAuthConfigs resolves version auth configurations across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridServiceVersionAuthConfigs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionAuthConfigs, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localRefs, cloudRefs := c.partitionVersionRefs(ctx, refs)
	var out []ServiceVersionAuthConfigs
	if len(localRefs) > 0 {
		localItems, err := c.local.FetchServiceVersionAuthConfigs(ctx, localRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, localItems...)
		found := make(map[ServiceVersionRef]bool, len(localItems))
		for _, item := range localItems {
			found[ServiceVersionRef{ServiceID: item.ServiceID, Version: item.Version}] = true
		}
		for _, ref := range localRefs {
			// Fall back to Cloud for unpinned Local misses when permitted.
			if !found[ref] && c.allowCloudFallback(ctx) {
				cloudRefs = append(cloudRefs, ref)
			}
		}
	}
	if len(cloudRefs) > 0 && c.allowCloudFallback(ctx) {
		cloudItems, err := c.cloudClone().FetchServiceVersionAuthConfigs(ctx, cloudRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, cloudItems...)
	}
	return out, nil
}

// fetchHybridServiceVersionIDs resolves service/version pairs across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridServiceVersionIDs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionResolvedRef, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localRefs, cloudRefs := c.partitionVersionRefs(ctx, refs)
	var out []ServiceVersionResolvedRef
	if len(localRefs) > 0 {
		localResolved, err := c.local.FetchServiceVersionIDs(ctx, localRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, localResolved...)
		found := make(map[ServiceVersionRef]bool, len(localResolved))
		for _, item := range localResolved {
			found[ServiceVersionRef{ServiceID: item.ServiceID, Version: item.Version}] = true
		}
		for _, ref := range localRefs {
			// Fall back to Cloud for unpinned Local misses when permitted.
			if !found[ref] && c.allowCloudFallback(ctx) {
				cloudRefs = append(cloudRefs, ref)
			}
		}
	}
	if len(cloudRefs) > 0 && c.allowCloudFallback(ctx) {
		cloudResolved, err := c.cloudClone().FetchServiceVersionIDs(ctx, cloudRefs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, cloudResolved...)
	}
	return out, nil
}

// fetchHybridLatestServiceVersions resolves omitted versions Local-first with permission-gated Cloud fallback.
func (c *HTTPRegistryClient) fetchHybridLatestServiceVersions(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]ServiceVersionResolvedRef, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localIDs, cloudIDs := c.partitionServiceIDs(ctx, serviceIDs)
	var out []ServiceVersionResolvedRef
	if len(localIDs) > 0 {
		localResolved, err := c.local.FetchLatestServiceVersions(ctx, localIDs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, localResolved...)
		found := make(map[uuid.UUID]bool, len(localResolved))
		for _, item := range localResolved {
			found[item.ServiceID] = true
		}
		for _, id := range localIDs {
			// Fall back to Cloud for unpinned Local misses when permitted.
			if !found[id] && c.allowCloudFallback(ctx) {
				cloudIDs = append(cloudIDs, id)
			}
		}
	}
	if len(cloudIDs) > 0 && c.allowCloudFallback(ctx) {
		cloudResolved, err := c.cloudClone().FetchLatestServiceVersions(ctx, cloudIDs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, cloudResolved...)
	}
	return out, nil
}

// fetchHybridServiceMetadata resolves runtime connect metadata from the pinned Registry or Local-first fallback.
func (c *HTTPRegistryClient) fetchHybridServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	pinned, _, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	// Use the pinned Registry directly when workspace provenance is known.
	if pinned != nil {
		return pinned.FetchServiceMetadata(ctx, serviceID, version)
	}
	meta, localErr := c.local.FetchServiceMetadata(ctx, serviceID, version)
	// Return immediately on Local hit or when Cloud fallback is disallowed.
	if localErr == nil || !c.allowCloudFallback(ctx) {
		return meta, localErr
	}
	return c.cloudClone().FetchServiceMetadata(ctx, serviceID, version)
}

// fetchHybridServiceMetadataBatch resolves webhook metadata batches across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridServiceMetadataBatch(ctx context.Context, refs []ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ServiceID)
	}
	sources := c.resolveBatchSources(ctx, ids)
	var localRefs, cloudRefs []ServiceMetadataRef
	for _, ref := range refs {
		// Partition metadata batch references by workspace provenance.
		if sources[ref.ServiceID] == RegistrySourceCloud {
			cloudRefs = append(cloudRefs, ref)
		} else {
			localRefs = append(localRefs, ref)
		}
	}
	out := make(map[string]*fusedobject.ServiceMetadata, len(refs))
	if len(localRefs) > 0 {
		localBatch, err := c.local.FetchServiceMetadataBatch(ctx, localRefs)
		if err != nil {
			return nil, err
		}
		for k, v := range localBatch {
			out[k] = v
		}
	}
	if len(cloudRefs) > 0 {
		if !c.allowCloudFallback(ctx) {
			return nil, ErrServiceNotFound
		}
		cloudBatch, err := c.cloudClone().FetchServiceMetadataBatch(ctx, cloudRefs)
		if err != nil {
			return nil, err
		}
		for k, v := range cloudBatch {
			out[k] = v
		}
	}
	return out, nil
}

// fetchHybridEndpointsByNames resolves live discovery endpoints from the pinned Registry or Local-first fallback.
func (c *HTTPRegistryClient) fetchHybridEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, names []string) ([]fusedobject.Endpoint, error) {
	pinned, _, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	// Use the pinned Registry directly when workspace provenance is known.
	if pinned != nil {
		return pinned.FetchEndpointsByNames(ctx, serviceID, serviceVersionID, names)
	}
	eps, localErr := c.local.FetchEndpointsByNames(ctx, serviceID, serviceVersionID, names)
	if (localErr == nil && len(eps) > 0) || !c.allowCloudFallback(ctx) {
		return eps, localErr
	}
	return c.cloudClone().FetchEndpointsByNames(ctx, serviceID, serviceVersionID, names)
}

// fetchHybridServiceOperations resolves operation contracts from the pinned Registry or Local-first fallback.
func (c *HTTPRegistryClient) fetchHybridServiceOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	pinned, _, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if pinned != nil {
		return pinned.FetchServiceOperations(ctx, serviceID, serviceVersionID)
	}
	ops, localErr := c.local.FetchServiceOperations(ctx, serviceID, serviceVersionID)
	if (localErr == nil && len(ops) > 0) || !c.allowCloudFallback(ctx) {
		return ops, localErr
	}
	return c.cloudClone().FetchServiceOperations(ctx, serviceID, serviceVersionID)
}

// fetchHybridConnectionProfileContracts resolves profile validation contracts across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridConnectionProfileContracts(ctx context.Context, versionIDs []uuid.UUID, apiKey string) ([]ConnectionProfileContract, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localItems, err := c.local.FetchConnectionProfileContracts(ctx, versionIDs, apiKey)
	if err != nil || !c.allowCloudFallback(ctx) {
		return localItems, err
	}
	found := make(map[uuid.UUID]bool, len(localItems))
	for _, item := range localItems {
		found[item.ServiceVersionID] = true
	}
	var missing []uuid.UUID
	for _, vid := range versionIDs {
		// Query Cloud only for version IDs absent from Local Registry.
		if !found[vid] {
			missing = append(missing, vid)
		}
	}
	if len(missing) == 0 {
		return localItems, nil
	}
	cloudItems, err := c.cloudClone().FetchConnectionProfileContracts(ctx, missing, apiKey)
	if err != nil {
		return nil, err
	}
	return append(localItems, cloudItems...), nil
}

// fetchHybridEligibleConnectionProfiles resolves published connection profiles Local-first with Cloud fallback on misses.
func (c *HTTPRegistryClient) fetchHybridEligibleConnectionProfiles(ctx context.Context, refs []ConnectionProfileRef, apiKey string) ([]ConnectionProfileRevision, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localItems, err := c.local.FetchEligibleConnectionProfiles(ctx, refs, apiKey)
	if err != nil || !c.allowCloudFallback(ctx) {
		return localItems, err
	}
	found := make(map[ConnectionProfileRef]bool, len(localItems))
	for _, item := range localItems {
		found[ConnectionProfileRef{ServiceVersionID: item.ServiceVersionID, AuthType: item.AuthType, AuthName: item.AuthName}] = true
	}
	var missing []ConnectionProfileRef
	for _, ref := range refs {
		// Query Cloud only for profile streams not published on Local Registry.
		if !found[ref] {
			missing = append(missing, ref)
		}
	}
	if len(missing) == 0 {
		return localItems, nil
	}
	cloudItems, err := c.cloudClone().FetchEligibleConnectionProfiles(ctx, missing, apiKey)
	if err != nil {
		return nil, err
	}
	return append(localItems, cloudItems...), nil
}

// fetchHybridDriftSnapshots loads pending drift for one service from its pinned Registry or Local-first.
func (c *HTTPRegistryClient) fetchHybridDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	pinned, _, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if pinned != nil {
		return pinned.FetchDriftSnapshots(ctx, serviceID, apiKey)
	}
	snaps, localErr := c.local.FetchDriftSnapshots(ctx, serviceID, apiKey)
	if (localErr == nil && len(snaps) > 0) || !c.allowCloudFallback(ctx) {
		return snaps, localErr
	}
	return c.cloudClone().FetchDriftSnapshots(ctx, serviceID, apiKey)
}

// fetchHybridDriftSnapshotsForServices partitions drift polling across Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridDriftSnapshotsForServices(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	localIDs, cloudIDs := c.partitionServiceIDs(ctx, serviceIDs)
	var out []models.DriftSnapshot
	if len(localIDs) > 0 {
		localSnaps, err := c.local.FetchDriftSnapshotsForServices(ctx, localIDs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, localSnaps...)
	}
	if len(cloudIDs) > 0 && c.allowCloudFallback(ctx) {
		cloudSnaps, err := c.cloudClone().FetchDriftSnapshotsForServices(ctx, cloudIDs, apiKey)
		if err != nil {
			return nil, err
		}
		out = append(out, cloudSnaps...)
	}
	return out, nil
}

// fetchHybridServiceChangelogSince polls service changelogs from the service's owning Registry.
func (c *HTTPRegistryClient) fetchHybridServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	pinned, _, err := c.clientForPinnedService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if pinned != nil {
		return pinned.FetchServiceChangelogSince(ctx, serviceID, since, apiKey)
	}
	return c.local.FetchServiceChangelogSince(ctx, serviceID, since, apiKey)
}

// fetchHybridSDKPackageDownloadCounts merges SDK package download counts from Local and Cloud Registries.
func (c *HTTPRegistryClient) fetchHybridSDKPackageDownloadCounts(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, err
	}
	counts, err := c.local.FetchSDKPackageDownloadCounts(ctx, appIDs)
	if err != nil {
		return nil, err
	}
	cloudCounts, cloudErr := c.cloudClone().FetchSDKPackageDownloadCounts(ctx, appIDs)
	// Best-effort Cloud merge preserves Local analytics even if Cloud is temporarily unreachable.
	if cloudErr == nil {
		for id, count := range cloudCounts {
			counts[id] += count
		}
	}
	return counts, nil
}

// publishHybridConnectionProfile routes public connection-profile publication to the service's owning Registry.
func (c *HTTPRegistryClient) publishHybridConnectionProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, name string, profile connectionprofile.Profile, apiKey string) (*ConnectionProfileRevision, error) {
	target, err := c.clientForServiceMutation(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	return target.PublishConnectionProfile(ctx, serviceID, serviceVersionID, name, profile, apiKey)
}
