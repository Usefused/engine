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

func configPlanRequiredPermissions(
	ctx context.Context,
	s store.Store,
	current *store.ConfigState,
	serviceNames map[uuid.UUID]string,
	bucketName string,
	configName string,
) (json.RawMessage, int, error) {
	buckets, err := configNamedBuckets(ctx, s, bucketName)
	if err != nil {
		return nil, 0, err
	}
	return configPlanRequiredPermissionsWithBuckets(ctx, s, current, serviceNames, buckets, configName)
}

func configPlanRequiredPermissionsWithBuckets(
	ctx context.Context,
	s store.Store,
	current *store.ConfigState,
	serviceNames map[uuid.UUID]string,
	buckets []store.Bucket,
	configName string,
) (json.RawMessage, int, error) {
	workspaceID, err := requiredPermissionWorkspaceID(ctx)
	if err != nil {
		return nil, 0, errRequiredPermissionContext
	}
	familyID, err := currentAppFamilyID(ctx, s, current)
	if err != nil {
		return nil, 0, err
	}
	requirements := configMutationRequirements(workspaceID, current, familyID)
	displayNames := map[accesscontrol.ResourceRef]string{
		{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}: "workspace",
	}
	serviceRequirements, serviceDisplayNames, err := configServiceRequirements(serviceNames)
	if err != nil {
		return nil, 0, err
	}
	requirements = append(requirements, serviceRequirements...)
	mergeResourceDisplayNames(displayNames, serviceDisplayNames)
	if familyID != uuid.Nil {
		displayNames[accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}] = configName
	}
	bucketRequirement, bucketDisplayNames, err := configBucketRequirements(buckets)
	if err != nil {
		return nil, 0, err
	}
	requirements = append(requirements, bucketRequirement...)
	mergeResourceDisplayNames(displayNames, bucketDisplayNames)
	return marshalPlanRequiredPermissions(requirements, displayNames, nil)
}

func configNamedBuckets(ctx context.Context, s store.Store, bucketName string) ([]store.Bucket, error) {
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

func configServiceRequirements(serviceNames map[uuid.UUID]string) ([]accesscontrol.Requirement, map[accesscontrol.ResourceRef]string, error) {
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

func configBucketRequirements(buckets []store.Bucket) ([]accesscontrol.Requirement, map[accesscontrol.ResourceRef]string, error) {
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

func currentAppFamilyID(ctx context.Context, s store.Store, current *store.ConfigState) (uuid.UUID, error) {
	if current == nil || current.LatestResourceID == nil || *current.LatestResourceID == uuid.Nil {
		return uuid.Nil, nil
	}
	if s == nil {
		return uuid.Nil, accesscontrol.ErrInvalidRequirement
	}
	app, err := s.GetApp(ctx, *current.LatestResourceID)
	if err != nil || app == nil || app.AppFamilyID == uuid.Nil {
		return uuid.Nil, accesscontrol.ErrInvalidRequirement
	}
	return app.AppFamilyID, nil
}

func configMutationRequirements(workspaceID uuid.UUID, current *store.ConfigState, familyID uuid.UUID) []accesscontrol.Requirement {
	// New apps require create at workspace scope. Once a version exists, the
	// family is the stable boundary so one grant consistently covers every version.
	if current == nil {
		return []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionAppCreate,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		}}
	}
	if familyID != uuid.Nil {
		return []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionAppManage,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID},
		}}
	}
	return []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionAppManage,
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
