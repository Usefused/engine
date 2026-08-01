package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/secretref"
)

const maxAuthorizationBodyBytes = 16 << 20

type defaultBucketLoader interface {
	LoadDefaultBucketID(context.Context) (uuid.UUID, error)
}

type storeBackedControlRequirementResolver struct {
	store       store.Store
	configStore store.ConfigRepository
}

type authorizationDisplayNameStore interface {
	ResolveAuthorizationResourceDisplayNames(context.Context, []accesscontrol.ResourceRef) (map[accesscontrol.ResourceRef]string, error)
}

func enrichControlPermissionDenial(ctx context.Context, actor accesscontrol.Actor, authorizer accesscontrol.Authorizer, resolver controlRequirementResolver, authorizationErr error) error {
	var denied *accesscontrol.PermissionDeniedError
	if !errors.As(authorizationErr, &denied) || authorizer == nil {
		return authorizationErr
	}
	storeResolver, ok := resolver.(*storeBackedControlRequirementResolver)
	if !ok {
		return authorizationErr
	}
	displayStore, ok := storeResolver.store.(authorizationDisplayNameStore)
	if !ok {
		return authorizationErr
	}
	resources := readableDeniedResources(ctx, actor, authorizer, denied.Missing)
	if len(resources) == 0 {
		return authorizationErr
	}
	names, err := displayStore.ResolveAuthorizationResourceDisplayNames(ctx, resources)
	if err == nil {
		denied.DisplayNames = names
	}
	return authorizationErr
}

func readableDeniedResources(ctx context.Context, actor accesscontrol.Actor, authorizer accesscontrol.Authorizer, missing []accesscontrol.Requirement) []accesscontrol.ResourceRef {
	unique := make(map[accesscontrol.ResourceRef]struct{}, len(missing))
	for _, requirement := range missing {
		readPermission := resourceReadPermission(requirement.Resource.Type)
		if readPermission == "" {
			continue
		}
		read := accesscontrol.Requirement{Permission: readPermission, Resource: requirement.Resource}
		if authorizer.CheckAll(ctx, actor, read) == nil {
			unique[requirement.Resource] = struct{}{}
		}
	}
	resources := make([]accesscontrol.ResourceRef, 0, len(unique))
	for resource := range unique {
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool {
		return string(resources[i].Type)+resources[i].ID.String() < string(resources[j].Type)+resources[j].ID.String()
	})
	return resources
}

func resourceReadPermission(resourceType accesscontrol.ResourceType) accesscontrol.Permission {
	switch resourceType {
	case accesscontrol.ResourceWorkspace:
		return accesscontrol.PermissionWorkspaceRead
	case accesscontrol.ResourceService:
		return accesscontrol.PermissionServiceRead
	case accesscontrol.ResourceBucket:
		return accesscontrol.PermissionBucketRead
	case accesscontrol.ResourceArtifact:
		return accesscontrol.PermissionArtifactRead
	default:
		return ""
	}
}

func newControlRequirementResolver(s store.Store, configStore store.ConfigRepository) controlRequirementResolver {
	return &storeBackedControlRequirementResolver{store: s, configStore: configStore}
}

func (r *storeBackedControlRequirementResolver) ResolveControlRequirements(
	ctx context.Context,
	actor accesscontrol.Actor,
	kind dynamicRequirementKind,
	params map[string]string,
	request *http.Request,
) ([]accesscontrol.Requirement, error) {
	if kind == dynamicArtifactDownload {
		return r.artifactDownloadRequirements(ctx, params)
	}
	switch kind {
	case dynamicServiceCreate:
		return serviceCreateRequirements(request)
	case dynamicBucketByName:
		return r.bucketByNameRequirements(ctx, params)
	case dynamicSecretWrite:
		return r.secretWriteRequirements(ctx, request)
	case dynamicWorkspaceApply:
		return r.workspaceApplyRequirements(ctx, actor, request)
	case dynamicWorkspacePlan:
		return r.workspacePlanRequirements(ctx, actor, request)
	default:
		return r.resolveArtifactControlRequirements(ctx, actor, kind, params, request)
	}
}

func (r *storeBackedControlRequirementResolver) resolveArtifactControlRequirements(ctx context.Context, actor accesscontrol.Actor, kind dynamicRequirementKind, params map[string]string, request *http.Request) ([]accesscontrol.Requirement, error) {
	switch kind {
	case dynamicConfigPlanAction:
		return r.configPlanActionRequirements(ctx, actor, params, request)
	case dynamicArtifactPlan:
		return r.artifactPlanRequestRequirements(ctx, actor, request)
	case dynamicArtifactApply:
		return r.artifactApplyRequirements(ctx, actor, request)
	case dynamicSDKGenerate:
		return r.sdkGenerateRequirements(ctx, actor, request)
	default:
		return nil, accesscontrol.ErrPolicyDenied
	}
}

func (r *storeBackedControlRequirementResolver) artifactDownloadRequirements(ctx context.Context, params map[string]string) ([]accesscontrol.Requirement, error) {
	if r.configStore == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	name, err := url.PathUnescape(params["artifact_name"])
	if err != nil || strings.TrimSpace(name) == "" {
		return nil, accesscontrol.ErrPolicyDenied
	}
	configKey := "sdk:" + name
	if strings.HasPrefix(name, "sdk:") {
		configKey = name
	}
	state, err := r.configStore.GetConfigState(ctx, configKey)
	if err != nil || state == nil || state.LatestResourceID == nil || *state.LatestResourceID == uuid.Nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionArtifactRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *state.LatestResourceID},
	}}, nil
}

func (r *storeBackedControlRequirementResolver) sdkGenerateRequirements(ctx context.Context, actor accesscontrol.Actor, request *http.Request) ([]accesscontrol.Requirement, error) {
	var payload struct {
		Selections []struct {
			ServiceID uuid.UUID `json:"service_id"`
		} `json:"selections"`
		BucketID string `json:"bucket_id"`
		Bucket   string `json:"bucket"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
		return nil, err
	}
	requirements := make([]accesscontrol.Requirement, 0, len(payload.Selections)+1)
	for _, selection := range payload.Selections {
		if selection.ServiceID == uuid.Nil {
			return nil, accesscontrol.ErrPolicyDenied
		}
		requirements = append(requirements, accesscontrol.Requirement{
			Permission: accesscontrol.PermissionServiceConsume,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: selection.ServiceID},
		})
	}
	if request.URL.Path == "/sdks/generate" && len(payload.Selections) == 0 {
		return nil, accesscontrol.ErrPolicyDenied
	}
	if payload.BucketID != "" {
		bucket, err := oneResourceRequirement(accesscontrol.PermissionBucketUse, accesscontrol.ResourceBucket, payload.BucketID)
		if err != nil {
			return nil, err
		}
		return append(requirements, bucket...), nil
	}
	if payload.Bucket != "" {
		bucket, err := r.bucketNameRequirements(ctx, actor.WorkspaceID, []string{payload.Bucket}, accesscontrol.PermissionBucketUse)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, bucket...)
	}
	return requirements, nil
}

func serviceCreateRequirements(request *http.Request) ([]accesscontrol.Requirement, error) {
	var payload struct {
		ServiceID string `json:"service_id"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
		return nil, err
	}
	return oneResourceRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, payload.ServiceID)
}

func (r *storeBackedControlRequirementResolver) bucketByNameRequirements(ctx context.Context, params map[string]string) ([]accesscontrol.Requirement, error) {
	name, err := url.PathUnescape(params["bucket_name"])
	if err != nil || name == "" || r.store == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	bucket, err := r.store.GetBucketByName(ctx, name)
	if err != nil || bucket == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return oneResourceRequirement(accesscontrol.PermissionBucketManage, accesscontrol.ResourceBucket, bucket.ID.String())
}

func (r *storeBackedControlRequirementResolver) secretWriteRequirements(ctx context.Context, request *http.Request) ([]accesscontrol.Requirement, error) {
	rawBucketID := request.URL.Query().Get("bucket_id")
	if request.Method != http.MethodDelete {
		var payload struct {
			BucketID string `json:"bucket_id"`
		}
		if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
			return nil, err
		}
		rawBucketID = payload.BucketID
	}
	bucketID, err := r.resolveBucketID(ctx, rawBucketID)
	if err != nil {
		return nil, err
	}
	return oneResourceRequirement(accesscontrol.PermissionCredentialsManage, accesscontrol.ResourceBucket, bucketID.String())
}

func (r *storeBackedControlRequirementResolver) resolveBucketID(ctx context.Context, raw string) (uuid.UUID, error) {
	if raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return uuid.Nil, accesscontrol.ErrPolicyDenied
		}
		return id, nil
	}
	loader, ok := r.store.(defaultBucketLoader)
	if !ok {
		return uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	id, err := loader.LoadDefaultBucketID(ctx)
	if err != nil {
		return uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	return id, nil
}

func (r *storeBackedControlRequirementResolver) workspaceApplyRequirements(ctx context.Context, actor accesscontrol.Actor, request *http.Request) ([]accesscontrol.Requirement, error) {
	plan, err := r.planFromRequest(ctx, request)
	if err != nil || plan.ConfigType != store.ConfigTypeWorkspace {
		return nil, accesscontrol.ErrPolicyDenied
	}
	// The handler must execute the exact action revision authorized here. The
	// request context is the immutable hand-off across the middleware boundary.
	*request = *request.WithContext(api.ContextWithAuthorizedPlanRevision(request.Context(), plan.Revision))
	requirements, err := workspaceActionRequirements(actor.WorkspaceID, plan.Actions)
	if err != nil {
		return nil, err
	}
	credentialRequirements, err := r.workspaceCredentialMaterialRequirements(ctx, actor.WorkspaceID, request)
	if err != nil {
		return nil, err
	}
	return append(requirements, credentialRequirements...), nil
}

func (r *storeBackedControlRequirementResolver) configPlanActionRequirements(
	ctx context.Context,
	actor accesscontrol.Actor,
	params map[string]string,
	request *http.Request,
) ([]accesscontrol.Requirement, error) {
	planID, err := uuid.Parse(params["plan_id"])
	if err != nil || r.configStore == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	plan, err := r.configStore.GetConfigPlan(ctx, planID)
	if err != nil || plan == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	if plan.ConfigType != store.ConfigTypeWorkspace {
		requirements, err := r.artifactPlanRequirements(ctx, actor.WorkspaceID, plan)
		if err != nil {
			return nil, err
		}
		selections, err := storedArtifactSelectionRequirements(plan, accesscontrol.PermissionServiceConsume, accesscontrol.PermissionBucketUse)
		if err != nil {
			return nil, err
		}
		return append(requirements, selections...), nil
	}
	var payload struct {
		Actions json.RawMessage `json:"actions"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
		return nil, err
	}
	return workspaceActionRequirements(actor.WorkspaceID, payload.Actions)
}

func (r *storeBackedControlRequirementResolver) artifactApplyRequirements(ctx context.Context, actor accesscontrol.Actor, request *http.Request) ([]accesscontrol.Requirement, error) {
	plan, err := r.planFromRequest(ctx, request)
	if err != nil || plan.ConfigType == store.ConfigTypeWorkspace {
		return nil, accesscontrol.ErrPolicyDenied
	}
	if !requestMatchesConfigType(request.URL.Path, plan.ConfigType) {
		return nil, accesscontrol.ErrPolicyDenied
	}
	// Bind the exact plan revision inspected here to the downstream handler.
	// A concurrent action replacement must not make apply execute a different
	// permission snapshot from the one authorized at this boundary.
	*request = *request.WithContext(api.ContextWithAuthorizedPlanRevision(request.Context(), plan.Revision))
	requirements, err := r.artifactPlanRequirements(ctx, actor.WorkspaceID, plan)
	if err != nil {
		return nil, err
	}
	selections, err := storedArtifactSelectionRequirements(plan, accesscontrol.PermissionServiceConsume, accesscontrol.PermissionBucketUse)
	if err != nil {
		return nil, err
	}
	return append(requirements, selections...), nil
}

func (r *storeBackedControlRequirementResolver) artifactPlanRequestRequirements(ctx context.Context, actor accesscontrol.Actor, request *http.Request) ([]accesscontrol.Requirement, error) {
	var envelope struct {
		ConfigKey string          `json:"config_key"`
		Config    json.RawMessage `json:"config"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &envelope); err != nil {
		return nil, err
	}
	mutation, err := r.artifactPlanMutationRequirement(ctx, actor.WorkspaceID, envelope.ConfigKey)
	if err != nil {
		return nil, err
	}
	selections, err := r.artifactDocumentRequirements(ctx, actor.WorkspaceID, envelope.Config, accesscontrol.PermissionServiceRead, accesscontrol.PermissionBucketRead)
	if err != nil {
		return nil, err
	}
	return append(mutation, selections...), nil
}

func (r *storeBackedControlRequirementResolver) artifactPlanMutationRequirement(ctx context.Context, workspaceID uuid.UUID, configKey string) ([]accesscontrol.Requirement, error) {
	if r.configStore == nil || strings.TrimSpace(configKey) == "" {
		return nil, accesscontrol.ErrPolicyDenied
	}
	state, err := r.configStore.GetConfigState(ctx, configKey)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	// Engine state, not a client-supplied artifact ID, decides whether this is
	// a create or update. The handler repeats this distinction during ownership
	// preflight before persisting a plan.
	if state == nil {
		return []accesscontrol.Requirement{workspaceAccessRequirement(workspaceID, accesscontrol.PermissionArtifactCreate)}, nil
	}
	if state.LatestResourceID != nil && *state.LatestResourceID != uuid.Nil {
		return []accesscontrol.Requirement{{
			Permission: accesscontrol.PermissionArtifactManage,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *state.LatestResourceID},
		}}, nil
	}
	return []accesscontrol.Requirement{workspaceAccessRequirement(workspaceID, accesscontrol.PermissionArtifactManage)}, nil
}

func (r *storeBackedControlRequirementResolver) workspacePlanRequirements(ctx context.Context, actor accesscontrol.Actor, request *http.Request) ([]accesscontrol.Requirement, error) {
	var envelope struct {
		ConfigKey string          `json:"config_key"`
		Config    json.RawMessage `json:"config"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &envelope); err != nil {
		return nil, err
	}
	var doc struct {
		Services     map[string]json.RawMessage `json:"services"`
		Buckets      map[string]json.RawMessage `json:"buckets"`
		Deprecations json.RawMessage            `json:"deprecations"`
	}
	if json.Unmarshal(envelope.Config, &doc) != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	configKey := envelope.ConfigKey
	if configKey == "" {
		configKey = "workspace"
	}
	if r.configStore == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	current, err := r.configStore.GetConfigState(ctx, configKey)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	serviceIDs, bucketNames, err := workspacePlanChangedResources(doc.Services, doc.Buckets, doc.Deprecations, current)
	if err != nil {
		return nil, err
	}
	requirements := []accesscontrol.Requirement{workspaceAccessRequirement(actor.WorkspaceID, accesscontrol.PermissionWorkspaceRead)}
	for _, serviceID := range serviceIDs {
		resolved, err := oneResourceRequirement(accesscontrol.PermissionServiceManage, accesscontrol.ResourceService, serviceID)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, resolved...)
	}
	bucketRequirements, err := r.bucketNameRequirements(ctx, actor.WorkspaceID, bucketNames, accesscontrol.PermissionBucketManage)
	if err != nil {
		return nil, err
	}
	return append(requirements, bucketRequirements...), nil
}

func workspacePlanChangedResources(desiredServices, desiredBuckets map[string]json.RawMessage, desiredDeprecations json.RawMessage, current *store.ConfigState) ([]string, []string, error) {
	var prior struct {
		Services     map[string]json.RawMessage `json:"services"`
		Buckets      map[string]json.RawMessage `json:"buckets"`
		Deprecations json.RawMessage            `json:"deprecations"`
	}
	if current != nil && json.Unmarshal(current.DesiredState, &prior) != nil {
		return nil, nil, accesscontrol.ErrPolicyDenied
	}
	serviceIDs := make(map[string]struct{})
	if err := addChangedServiceIDs(serviceIDs, desiredServices, prior.Services); err != nil {
		return nil, nil, err
	}
	if !jsonValuesEqual(desiredDeprecations, prior.Deprecations) {
		if err := addDeprecationServiceIDs(serviceIDs, desiredDeprecations, prior.Deprecations); err != nil {
			return nil, nil, err
		}
	}
	return mapKeys(serviceIDs), changedJSONKeys(desiredBuckets, prior.Buckets), nil
}

func addChangedServiceIDs(serviceIDs map[string]struct{}, desired, prior map[string]json.RawMessage) error {
	for _, key := range changedJSONKeys(desired, prior) {
		for _, raw := range []json.RawMessage{desired[key], prior[key]} {
			if len(raw) == 0 {
				continue
			}
			var service struct {
				ServiceID string `json:"service_id"`
			}
			if json.Unmarshal(raw, &service) != nil || service.ServiceID == "" {
				return accesscontrol.ErrPolicyDenied
			}
			serviceIDs[service.ServiceID] = struct{}{}
		}
	}
	return nil
}

func addDeprecationServiceIDs(serviceIDs map[string]struct{}, payloads ...json.RawMessage) error {
	for _, raw := range payloads {
		var deprecations []struct {
			ServiceID string `json:"service_id"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &deprecations) != nil {
			return accesscontrol.ErrPolicyDenied
		}
		for _, deprecation := range deprecations {
			if deprecation.ServiceID == "" {
				return accesscontrol.ErrPolicyDenied
			}
			serviceIDs[deprecation.ServiceID] = struct{}{}
		}
	}
	return nil
}

func changedJSONKeys(desired, prior map[string]json.RawMessage) []string {
	keys := make(map[string]struct{}, len(desired)+len(prior))
	for key := range desired {
		keys[key] = struct{}{}
	}
	for key := range prior {
		keys[key] = struct{}{}
	}
	changed := make(map[string]struct{})
	for key := range keys {
		if !jsonValuesEqual(desired[key], prior[key]) {
			changed[key] = struct{}{}
		}
	}
	return mapKeys(changed)
}

func jsonValuesEqual(first, second json.RawMessage) bool {
	if len(first) == 0 || len(second) == 0 {
		return len(first) == len(second)
	}
	var left any
	var right any
	if json.Unmarshal(first, &left) != nil || json.Unmarshal(second, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func (r *storeBackedControlRequirementResolver) artifactDocumentRequirements(
	ctx context.Context,
	workspaceID uuid.UUID,
	raw json.RawMessage,
	servicePermission accesscontrol.Permission,
	bucketPermission accesscontrol.Permission,
) ([]accesscontrol.Requirement, error) {
	var doc struct {
		Bucket   string `json:"bucket"`
		Services map[string]struct {
			Secret string `json:"secret"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Services) == 0 || r.store == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	serviceNames := mapKeys(doc.Services)
	services, err := r.store.ListWorkspaceServices(ctx, serviceNames)
	if err != nil || len(services) != len(doc.Services) {
		return nil, accesscontrol.ErrPolicyDenied
	}
	requirements := make([]accesscontrol.Requirement, 0, len(services)+1)
	for _, service := range services {
		requirements = append(requirements, accesscontrol.Requirement{
			Permission: servicePermission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service.ServiceID},
		})
	}
	return r.appendArtifactDocumentBucketRequirements(ctx, workspaceID, requirements, doc.Bucket, doc.Services, bucketPermission)
}

func (r *storeBackedControlRequirementResolver) appendArtifactDocumentBucketRequirements(
	ctx context.Context,
	workspaceID uuid.UUID,
	requirements []accesscontrol.Requirement,
	defaultBucket string,
	services map[string]struct {
		Secret string `json:"secret"`
	},
	bucketPermission accesscontrol.Permission,
) ([]accesscontrol.Requirement, error) {
	bucketNames, err := artifactDocumentBucketNames(defaultBucket, services)
	if err != nil || len(bucketNames) == 0 {
		if err != nil {
			return nil, err
		}
		return requirements, nil
	}
	buckets, err := r.bucketNameRequirements(ctx, workspaceID, bucketNames, bucketPermission)
	if err != nil {
		return nil, err
	}
	return append(requirements, buckets...), nil
}

func artifactDocumentBucketNames(topLevel string, services map[string]struct {
	Secret string `json:"secret"`
}) ([]string, error) {
	names := make(map[string]struct{})
	if strings.TrimSpace(topLevel) != "" {
		names[topLevel] = struct{}{}
	}
	for _, service := range services {
		if strings.TrimSpace(service.Secret) == "" {
			continue
		}
		ref, err := secretref.Parse(service.Secret)
		if err != nil || ref.Kind != secretref.KindSecret {
			return nil, accesscontrol.ErrPolicyDenied
		}
		names[ref.Bucket] = struct{}{}
	}
	return mapKeys(names), nil
}

func storedArtifactSelectionRequirements(
	plan *store.ConfigPlan,
	servicePermission accesscontrol.Permission,
	bucketPermission accesscontrol.Permission,
) ([]accesscontrol.Requirement, error) {
	if plan.ConfigType == store.ConfigTypeWebhook {
		return storedWebhookSelectionRequirements(plan, servicePermission, bucketPermission)
	}
	var payload struct {
		BucketID   uuid.UUID `json:"bucket_id"`
		Selections []struct {
			ServiceID uuid.UUID `json:"service_id"`
		} `json:"selections"`
	}
	if json.Unmarshal(plan.ResolvedPayload, &payload) != nil || len(payload.Selections) == 0 || payload.BucketID == uuid.Nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	requirements := make([]accesscontrol.Requirement, 0, len(payload.Selections)+1)
	for _, selection := range payload.Selections {
		if selection.ServiceID == uuid.Nil {
			return nil, accesscontrol.ErrPolicyDenied
		}
		requirements = append(requirements, accesscontrol.Requirement{
			Permission: servicePermission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: selection.ServiceID},
		})
	}
	return append(requirements, accesscontrol.Requirement{
		Permission: bucketPermission,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: payload.BucketID},
	}), nil
}

func storedWebhookSelectionRequirements(plan *store.ConfigPlan, servicePermission, bucketPermission accesscontrol.Permission) ([]accesscontrol.Requirement, error) {
	// Webhook references are name-shaped in YAML but identity-shaped in the
	// stored server snapshot. Apply must authorize those immutable IDs rather
	// than re-resolving a same-name bucket that could have been recreated.
	stored, err := accesscontrol.UnmarshalRequiredPermissions(plan.RequiredPermissions)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	selected := make([]accesscontrol.Requirement, 0, len(stored))
	serviceCount := 0
	for _, requirement := range stored {
		switch {
		case requirement.Permission == servicePermission && requirement.Resource.Type == accesscontrol.ResourceService:
			selected = append(selected, requirement)
			serviceCount++
		case requirement.Permission == bucketPermission && requirement.Resource.Type == accesscontrol.ResourceBucket:
			selected = append(selected, requirement)
		}
	}
	if serviceCount == 0 {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return selected, nil
}

func (r *storeBackedControlRequirementResolver) bucketNameRequirements(ctx context.Context, workspaceID uuid.UUID, names []string, permission accesscontrol.Permission) ([]accesscontrol.Requirement, error) {
	if len(names) == 0 {
		return nil, nil
	}
	buckets, err := r.store.GetBucketsByNames(ctx, names)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	requirements := make([]accesscontrol.Requirement, 0, len(names))
	for _, bucket := range buckets {
		requirements = append(requirements, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucket.ID}})
	}
	// A missing named bucket is a create decision, which cannot carry a
	// resource UUID yet and therefore requires workspace authority.
	if len(buckets) != len(names) {
		requirements = append(requirements, workspaceAccessRequirement(workspaceID, permission))
	}
	return requirements, nil
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func requestMatchesConfigType(path string, configType store.ConfigType) bool {
	switch {
	case path == "/sdk-config/apply":
		return configType == store.ConfigTypeSDK
	case path == "/mcp-config/apply":
		return configType == store.ConfigTypeMCP
	case path == "/webhook-config/apply":
		return configType == store.ConfigTypeWebhook
	default:
		return false
	}
}

func (r *storeBackedControlRequirementResolver) artifactPlanRequirements(ctx context.Context, workspaceID uuid.UUID, plan *store.ConfigPlan) ([]accesscontrol.Requirement, error) {
	if plan.BaseGeneration == 0 {
		return []accesscontrol.Requirement{workspaceAccessRequirement(workspaceID, accesscontrol.PermissionArtifactCreate)}, nil
	}
	if r.configStore == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	state, err := r.configStore.GetConfigState(ctx, plan.ConfigKey)
	if err != nil || state == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	if state.LatestResourceID == nil || *state.LatestResourceID == uuid.Nil {
		return []accesscontrol.Requirement{workspaceAccessRequirement(workspaceID, accesscontrol.PermissionArtifactManage)}, nil
	}
	return []accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionArtifactManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceArtifact, ID: *state.LatestResourceID},
	}}, nil
}

func (r *storeBackedControlRequirementResolver) planFromRequest(ctx context.Context, request *http.Request) (*store.ConfigPlan, error) {
	if r.configStore == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	var payload struct {
		PlanID string `json:"plan_id"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
		return nil, err
	}
	planID, err := uuid.Parse(payload.PlanID)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	plan, err := r.configStore.GetConfigPlan(ctx, planID)
	if err != nil || plan == nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return plan, nil
}

func workspaceActionRequirements(workspaceID uuid.UUID, raw json.RawMessage) ([]accesscontrol.Requirement, error) {
	return accesscontrol.WorkspacePlanApplyRequirements(workspaceID, raw)
}

func (r *storeBackedControlRequirementResolver) workspaceCredentialMaterialRequirements(ctx context.Context, workspaceID uuid.UUID, request *http.Request) ([]accesscontrol.Requirement, error) {
	var payload struct {
		AuthMaterials         map[string]json.RawMessage `json:"auth_materials"`
		ProfileMaterials      map[string]json.RawMessage `json:"profile_materials"`
		BucketSecretMaterials map[string]json.RawMessage `json:"bucket_secret_materials"`
	}
	if err := decodeAndRestoreAuthorizationBody(request, &payload); err != nil {
		return nil, err
	}
	bucketNames := make(map[string]struct{})
	for _, materials := range []map[string]json.RawMessage{payload.AuthMaterials, payload.ProfileMaterials, payload.BucketSecretMaterials} {
		for key := range materials {
			parts := strings.SplitN(key, "\x00", 2)
			if len(parts) != 2 || parts[0] == "" {
				return nil, accesscontrol.ErrPolicyDenied
			}
			bucketNames[parts[0]] = struct{}{}
		}
	}
	bucketRequirements, err := r.bucketNameRequirements(ctx, workspaceID, mapKeys(bucketNames), accesscontrol.PermissionCredentialsManage)
	if err != nil {
		return nil, err
	}
	return bucketRequirements, nil
}

func workspaceAccessRequirement(workspaceID uuid.UUID, permission accesscontrol.Permission) accesscontrol.Requirement {
	return accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}
}

func oneResourceRequirement(permission accesscontrol.Permission, resourceType accesscontrol.ResourceType, rawID string) ([]accesscontrol.Requirement, error) {
	id, err := uuid.Parse(rawID)
	if err != nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return []accesscontrol.Requirement{{Permission: permission, Resource: accesscontrol.ResourceRef{Type: resourceType, ID: id}}}, nil
}

func decodeAndRestoreAuthorizationBody(request *http.Request, destination any) error {
	if request.Body == nil {
		return accesscontrol.ErrPolicyDenied
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxAuthorizationBodyBytes+1))
	if err != nil {
		return accesscontrol.ErrPolicyDenied
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxAuthorizationBodyBytes {
		return accesscontrol.ErrPolicyDenied
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return accesscontrol.ErrPolicyDenied
	}
	return nil
}
