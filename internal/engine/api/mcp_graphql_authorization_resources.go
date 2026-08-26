package api

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type graphQLAuthorizationResources struct {
	store        store.Store
	configStore  store.ConfigRepository
	slugResolver ServiceSlugResolver
	revisionSink authorizationRevisionSink
}

func (r graphQLAuthorizationResources) resolveApps(ctx context.Context, accountID uuid.UUID, requests []graphQLAppRequirement) ([]accesscontrol.Requirement, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	resolver, ok := r.store.(store.AppFamilyAccessResolver)
	if !ok {
		return nil, fmt.Errorf("%w: app access resolver unavailable", errGraphQLPolicyMissing)
	}
	appIDs := uniqueAppRequirementIDs(requests)
	families, err := resolver.ResolveAppFamilyAccess(ctx, accountID, appIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve app access: %v", errGraphQLPolicyMissing, err)
	}
	requirements := make(map[accesscontrol.Requirement]struct{}, len(requests))
	for _, request := range requests {
		familyID := families[request.appID]
		if familyID == uuid.Nil {
			return nil, accesscontrol.ErrPolicyDenied
		}
		requirements[accesscontrol.Requirement{
			Permission: request.permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID},
		}] = struct{}{}
	}
	return sortedRequirements(requirements), nil
}

func (r graphQLAuthorizationResources) resolveAppRequirements(ctx context.Context, accountID uuid.UUID, requirements []accesscontrol.Requirement) ([]accesscontrol.Requirement, error) {
	requests := make([]graphQLAppRequirement, 0)
	for _, requirement := range requirements {
		if requirement.Resource.Type == accesscontrol.ResourceApp {
			requests = append(requests, graphQLAppRequirement{appID: requirement.Resource.ID, permission: requirement.Permission})
		}
	}
	resolved, err := r.resolveApps(ctx, accountID, requests)
	if err != nil || len(requests) == 0 {
		return requirements, err
	}
	result := make([]accesscontrol.Requirement, 0, len(requirements))
	for _, requirement := range requirements {
		if requirement.Resource.Type != accesscontrol.ResourceApp {
			result = append(result, requirement)
		}
	}
	return append(result, resolved...), nil
}

func uniqueAppRequirementIDs(requests []graphQLAppRequirement) []uuid.UUID {
	unique := make(map[uuid.UUID]struct{}, len(requests))
	for _, request := range requests {
		unique[request.appID] = struct{}{}
	}
	ids := make([]uuid.UUID, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func (r graphQLAuthorizationResources) resolveConnections(ctx context.Context, requests []graphQLConnectionRequirement) ([]accesscontrol.Requirement, map[uuid.UUID]store.AuthConnection, error) {
	if len(requests) == 0 {
		return nil, nil, nil
	}
	if r.store == nil {
		return nil, nil, fmt.Errorf("%w: connection resource store unavailable", errGraphQLPolicyMissing)
	}
	connections, err := r.store.GetAuthConnectionsByIDs(ctx, connectionRequestIDs(requests))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: resolve connections: %v", errGraphQLPolicyMissing, err)
	}
	requirements, err := resolvedConnectionRequirements(requests, connections)
	return requirements, connections, err
}

func connectionRequestIDs(requests []graphQLConnectionRequirement) []uuid.UUID {
	unique := make(map[uuid.UUID]struct{}, len(requests))
	for _, request := range requests {
		unique[request.connectionID] = struct{}{}
	}
	ids := make([]uuid.UUID, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func resolvedConnectionRequirements(requests []graphQLConnectionRequirement, connections map[uuid.UUID]store.AuthConnection) ([]accesscontrol.Requirement, error) {
	requirements := make(map[accesscontrol.Requirement]struct{}, len(requests))
	for _, request := range requests {
		connection, ok := connections[request.connectionID]
		if !ok || connection.BucketID == uuid.Nil {
			// Opaque IDs fail as a generic denial so callers cannot use this lookup
			// to discover whether a connection exists outside their authorized scope.
			return nil, accesscontrol.ErrPolicyDenied
		}
		requirements[accesscontrol.Requirement{
			Permission: request.permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: connection.BucketID},
		}] = struct{}{}
	}
	return sortedRequirements(requirements), nil
}

func (r graphQLAuthorizationResources) resolveDeployments(ctx context.Context, workspaceID uuid.UUID, documents []sdkConfigDocument, apiKey string) ([]accesscontrol.Requirement, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if r.store == nil || r.configStore == nil {
		return nil, fmt.Errorf("%w: deployment resource store unavailable", errGraphQLPolicyMissing)
	}
	bucketNames, serviceSlugs := deploymentResourceNames(documents)
	buckets, err := r.store.GetBucketsByNames(ctx, bucketNames)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve deployment buckets: %v", errGraphQLPolicyMissing, err)
	}
	serviceIDs, err := r.resolveDeploymentServiceIDs(ctx, serviceSlugs, apiKey)
	if err != nil {
		return nil, err
	}
	states, err := r.configStore.GetConfigStatesByKeys(ctx, deploymentConfigKeys(documents))
	if err != nil {
		return nil, fmt.Errorf("%w: resolve deployment apps: %v", errGraphQLPolicyMissing, err)
	}
	return deploymentRequirements(workspaceID, documents, buckets, serviceIDs, states)
}

func (r graphQLAuthorizationResources) resolveDeploymentServiceIDs(ctx context.Context, serviceSlugs []string, apiKey string) (map[string]uuid.UUID, error) {
	serviceIDs, err := r.store.ResolveWorkspaceServiceIDsByKeys(ctx, serviceSlugs)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve local deployment services: %v", errGraphQLPolicyMissing, err)
	}
	missing := unresolvedServiceKeys(serviceSlugs, serviceIDs)
	if len(missing) == 0 {
		return serviceIDs, nil
	}
	if r.slugResolver == nil {
		return nil, fmt.Errorf("%w: service slug resolution unavailable", errGraphQLPolicyMissing)
	}
	registryIDs, err := r.slugResolver.ResolveServiceIDsBySlugs(ctx, missing, apiKey)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve deployment services: %v", errGraphQLPolicyMissing, err)
	}
	for key, serviceID := range registryIDs {
		serviceIDs[key] = serviceID
	}
	return serviceIDs, nil
}

func deploymentConfigKeys(documents []sdkConfigDocument) []string {
	keys := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		keys[fmt.Sprintf("mcp:%s:%s", document.Name, document.Version)] = struct{}{}
	}
	return sortedStringKeys(keys)
}

func unresolvedServiceKeys(keys []string, resolved map[string]uuid.UUID) []string {
	missing := make([]string, 0)
	for _, key := range keys {
		if resolved[key] == uuid.Nil {
			missing = append(missing, key)
		}
	}
	return missing
}

func deploymentResourceNames(documents []sdkConfigDocument) ([]string, []string) {
	buckets := make(map[string]struct{})
	services := make(map[string]struct{})
	for _, document := range documents {
		buckets[document.Bucket] = struct{}{}
		for slug := range document.Services {
			services[slug] = struct{}{}
		}
	}
	return sortedStringKeys(buckets), sortedStringKeys(services)
}

func sortedStringKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}

func deploymentRequirements(workspaceID uuid.UUID, documents []sdkConfigDocument, buckets []store.Bucket, serviceIDs map[string]uuid.UUID, states map[string]store.ConfigState) ([]accesscontrol.Requirement, error) {
	bucketIDs := make(map[string]uuid.UUID, len(buckets))
	for _, bucket := range buckets {
		bucketIDs[bucket.Name] = bucket.ID
	}
	requirements := make(map[accesscontrol.Requirement]struct{})
	for _, document := range documents {
		addDeploymentAppRequirement(requirements, workspaceID, document, states)
		bucketID := bucketIDs[document.Bucket]
		if bucketID == uuid.Nil {
			return nil, fmt.Errorf("%w: deployment bucket %q was not found", errGraphQLPolicyMissing, document.Bucket)
		}
		requirements[accesscontrol.Requirement{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}}] = struct{}{}
		for slug := range document.Services {
			serviceID := serviceIDs[slug]
			if serviceID == uuid.Nil {
				return nil, fmt.Errorf("%w: deployment service %q was not found", errGraphQLPolicyMissing, slug)
			}
			requirements[accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}}] = struct{}{}
		}
	}
	return sortedRequirements(requirements), nil
}

func addDeploymentAppRequirement(requirements map[accesscontrol.Requirement]struct{}, workspaceID uuid.UUID, document sdkConfigDocument, states map[string]store.ConfigState) {
	state, exists := states[fmt.Sprintf("mcp:%s:%s", document.Name, document.Version)]
	if exists && state.LatestResourceID != nil && *state.LatestResourceID != uuid.Nil {
		requirements[accesscontrol.Requirement{Permission: accesscontrol.PermissionAppManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: *state.LatestResourceID}}] = struct{}{}
		return
	}
	requirements[accesscontrol.Requirement{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}] = struct{}{}
}

func sortedRequirements(values map[accesscontrol.Requirement]struct{}) []accesscontrol.Requirement {
	requirements := make([]accesscontrol.Requirement, 0, len(values))
	for requirement := range values {
		requirements = append(requirements, requirement)
	}
	sort.Slice(requirements, func(i, j int) bool { return requirementSortKey(requirements[i]) < requirementSortKey(requirements[j]) })
	return requirements
}
