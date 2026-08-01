package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var errRequiredPermissionContext = errors.New("authenticated workspace scope is required to compute plan permissions")

func workspacePlanRequiredPermissions(ctx context.Context, actions json.RawMessage) (json.RawMessage, int, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok || actor.WorkspaceID == uuid.Nil {
		return nil, 0, errRequiredPermissionContext
	}
	requirements, err := accesscontrol.WorkspacePlanApplyRequirements(actor.WorkspaceID, actions)
	displayNames := map[accesscontrol.ResourceRef]string{
		{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}: "workspace",
	}
	var namedActions []struct {
		ServiceID   uuid.UUID `json:"service_id"`
		ServiceName string    `json:"service_name"`
	}
	if json.Unmarshal(actions, &namedActions) == nil {
		for _, action := range namedActions {
			if action.ServiceID != uuid.Nil && action.ServiceName != "" {
				displayNames[accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: action.ServiceID}] = action.ServiceName
			}
		}
	}
	return marshalPlanRequiredPermissions(requirements, displayNames, err)
}

func artifactPlanRequiredPermissions(
	ctx context.Context,
	s store.Store,
	current *store.ConfigState,
	serviceNames map[uuid.UUID]string,
	bucketName string,
	artifactName string,
) (json.RawMessage, int, error) {
	buckets, err := artifactNamedBuckets(ctx, s, bucketName)
	if err != nil {
		return nil, 0, err
	}
	return artifactPlanRequiredPermissionsWithBuckets(ctx, current, serviceNames, buckets, artifactName)
}

func artifactPlanRequiredPermissionsWithBuckets(
	ctx context.Context,
	current *store.ConfigState,
	serviceNames map[uuid.UUID]string,
	buckets []store.Bucket,
	artifactName string,
) (json.RawMessage, int, error) {
	workspaceID, err := requiredPermissionWorkspaceID(ctx)
	if err != nil {
		return nil, 0, errRequiredPermissionContext
	}
	requirements := artifactMutationRequirements(workspaceID, current)
	displayNames := map[accesscontrol.ResourceRef]string{
		{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}: "workspace",
	}
	serviceRequirements, serviceDisplayNames, err := artifactServiceRequirements(serviceNames)
	if err != nil {
		return nil, 0, err
	}
	requirements = append(requirements, serviceRequirements...)
	mergeResourceDisplayNames(displayNames, serviceDisplayNames)
	if current != nil && current.LatestResourceID != nil && *current.LatestResourceID != uuid.Nil {
		displayNames[accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *current.LatestResourceID}] = artifactName
	}
	bucketRequirement, bucketDisplayNames, err := artifactBucketRequirements(buckets)
	if err != nil {
		return nil, 0, err
	}
	requirements = append(requirements, bucketRequirement...)
	mergeResourceDisplayNames(displayNames, bucketDisplayNames)
	return marshalPlanRequiredPermissions(requirements, displayNames, nil)
}

func artifactNamedBuckets(ctx context.Context, s store.Store, bucketName string) ([]store.Bucket, error) {
	if bucketName == "" {
		return nil, nil
	}
	bucket, err := s.GetBucketByName(ctx, bucketName)
	if err != nil || bucket == nil || bucket.ID == uuid.Nil {
		return nil, accesscontrol.ErrInvalidRequirement
	}
	return []store.Bucket{*bucket}, nil
}

func requiredPermissionWorkspaceID(ctx context.Context) (uuid.UUID, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok || actor.WorkspaceID == uuid.Nil {
		return uuid.Nil, errRequiredPermissionContext
	}
	return actor.WorkspaceID, nil
}

func artifactServiceRequirements(serviceNames map[uuid.UUID]string) ([]accesscontrol.Requirement, map[accesscontrol.ResourceRef]string, error) {
	requirements := make([]accesscontrol.Requirement, 0, len(serviceNames))
	displayNames := make(map[accesscontrol.ResourceRef]string, len(serviceNames))
	for serviceID, serviceName := range serviceNames {
		if serviceID == uuid.Nil {
			return nil, nil, accesscontrol.ErrInvalidRequirement
		}
		resource := accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}
		requirements = append(requirements, accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceConsume, Resource: resource})
		displayNames[resource] = serviceName
	}
	return requirements, displayNames, nil
}

func artifactBucketRequirements(buckets []store.Bucket) ([]accesscontrol.Requirement, map[accesscontrol.ResourceRef]string, error) {
	requirements := make([]accesscontrol.Requirement, 0, len(buckets))
	displayNames := make(map[accesscontrol.ResourceRef]string, len(buckets))
	for _, bucket := range buckets {
		if bucket.ID == uuid.Nil || bucket.Name == "" {
			return nil, nil, accesscontrol.ErrInvalidRequirement
		}
		resource := accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucket.ID}
		requirements = append(requirements, accesscontrol.Requirement{Permission: accesscontrol.PermissionBucketUse, Resource: resource})
		displayNames[resource] = bucket.Name
	}
	return requirements, displayNames, nil
}

func mergeResourceDisplayNames(target, additions map[accesscontrol.ResourceRef]string) {
	for resource, name := range additions {
		target[resource] = name
	}
}

func artifactMutationRequirements(workspaceID uuid.UUID, current *store.ConfigState) []accesscontrol.Requirement {
	// New artifacts require create at workspace scope. Once an artifact exists,
	// its stable identity becomes the authorization boundary and subsequent
	// plans require manage on that exact artifact instead of another create.
	if current == nil {
		return []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionArtifactCreate,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		}}
	}
	if current.LatestResourceID != nil && *current.LatestResourceID != uuid.Nil {
		return []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionArtifactManage,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *current.LatestResourceID},
		}}
	}
	return []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionArtifactManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	}}
}

func marshalPlanRequiredPermissions(requirements []accesscontrol.Requirement, displayNames map[accesscontrol.ResourceRef]string, requirementErr error) (json.RawMessage, int, error) {
	if requirementErr != nil {
		return nil, 0, requirementErr
	}
	raw, err := accesscontrol.MarshalRequiredPermissionsWithDisplayNames(requirements, displayNames)
	if err != nil {
		return nil, 0, err
	}
	return raw, len(requirements), nil
}

func serviceNamesFromResolved(services []sdkResolvedService) map[uuid.UUID]string {
	names := make(map[uuid.UUID]string, len(services))
	for _, service := range services {
		names[service.ServiceID] = service.ServiceName
	}
	return names
}

func requiredPermissionsFromContext(ctx context.Context, actions json.RawMessage) (json.RawMessage, int, error) {
	// Middleware captures the requirements it actually authorized. Persist that
	// server-owned snapshot with the plan so apply cannot accept a weaker set
	// reconstructed later from client-provided action text.
	requirements, ok := accesscontrol.RequiredPermissionsFromContext(ctx)
	if !ok || len(requirements) == 0 {
		return nil, 0, errRequiredPermissionContext
	}
	displayNames := map[accesscontrol.ResourceRef]string{}
	var namedActions []struct {
		ServiceID   uuid.UUID `json:"service_id"`
		ServiceName string    `json:"service_name"`
	}
	if json.Unmarshal(actions, &namedActions) == nil {
		for _, action := range namedActions {
			if action.ServiceID != uuid.Nil && action.ServiceName != "" {
				displayNames[accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: action.ServiceID}] = action.ServiceName
			}
		}
	}
	if actor, present := accesscontrol.ActorFromContext(ctx); present {
		displayNames[accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}] = "workspace"
	}
	return marshalPlanRequiredPermissions(requirements, displayNames, nil)
}
