package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var (
	errInvalidGraphQLRequest = errors.New("invalid GraphQL request")
	errGraphQLPolicyMissing  = errors.New("GraphQL authorization policy missing")
)

type graphQLRequestPayload struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

type graphQLFieldPolicy struct {
	permissions         []accesscontrol.Permission
	authenticated       bool
	scope               graphQLScopeMode
	resource            accesscontrol.ResourceType
	argument            string
	relatedPermission   accesscontrol.Permission
	relatedResource     accesscontrol.ResourceType
	protectedArgument   string
	protectedValue      string
	protectedPermission accesscontrol.Permission
}

type graphQLScopeMode string

const (
	graphQLScopeWorkspace   graphQLScopeMode = "workspace"
	graphQLScopeArgument    graphQLScopeMode = "argument"
	graphQLScopeAppArgument graphQLScopeMode = "app_argument"
	// Collection roots pass the actor's complete authorized scope into SQL so
	// filtering happens before pagination and totals are calculated.
	graphQLScopeCollection graphQLScopeMode = "collection"
	graphQLScopeDeployment graphQLScopeMode = "deployment"
	graphQLScopeRelated    graphQLScopeMode = "related"
	graphQLScopeConnection graphQLScopeMode = "connection"
)

type graphQLAuthorizationPolicy struct {
	queryRoots    map[string]graphQLFieldPolicy
	mutationRoots map[string]graphQLFieldPolicy
	protected     map[string]graphQLFieldPolicy
}

// engineGraphQLPolicy is deliberately declared beside the Engine schema rather
// than inferred from operation names. Adding a root resolver without adding a
// policy is therefore a fail-closed server configuration error.
var engineGraphQLPolicy = graphQLAuthorizationPolicy{
	queryRoots: map[string]graphQLFieldPolicy{
		"appScaffoldRequirements":     collectionPermissions(accesscontrol.ResourceService, accesscontrol.PermissionServiceRead),
		"currentActorAccess":          authenticatedOnly(),
		"app":                         collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"apps":                        collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"appVersions":                 collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"appServices":                 collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"accessExplanation":           permissions(accesscontrol.PermissionAccessRead),
		"auditEvents":                 permissions(accesscontrol.PermissionAuditRead),
		"appBuildSelectors":           permissions(accesscontrol.PermissionAppCreate),
		"appOwningTeams":              permissions(accesscontrol.PermissionAppCreate),
		"users":                       permissions(accesscontrol.PermissionAccessRead),
		"user":                        permissions(accesscontrol.PermissionAccessRead),
		"userEffectiveAccess":         permissions(accesscontrol.PermissionAccessRead),
		"teamMembers":                 permissions(accesscontrol.PermissionAccessRead),
		"teams":                       permissions(accesscontrol.PermissionAccessRead),
		"team":                        permissions(accesscontrol.PermissionAccessRead),
		"workspaceShares":             permissions(accesscontrol.PermissionAccessRead),
		"bucketReference":             collectionPermissions(accesscontrol.ResourceBucket, accesscontrol.PermissionBucketRead),
		"serviceReference":            collectionPermissions(accesscontrol.ResourceService, accesscontrol.PermissionServiceRead),
		"appReference":                collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"appFamilyReference":          collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"workspaceConnectionProfile":  argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead),
		"workspaceConnectConfigs":     permissions(accesscontrol.PermissionConnectionRead),
		"mcpServers":                  collectionPermissions(accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"mcpServerByName":             permissions(accesscontrol.PermissionAppRead),
		"mcpAnalytics":                appArgumentPermissions("app_id", accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead),
		"mcpSessions":                 appArgumentPermissions("app_id", accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead),
		"bucketSummaries":             collectionPermissions(accesscontrol.ResourceBucket, accesscontrol.PermissionBucketRead),
		"bucketSummary":               argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketRead),
		"bucketSummaryPage":           collectionPermissions(accesscontrol.ResourceBucket, accesscontrol.PermissionBucketRead),
		"bucketConnectSummary":        argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketRead, accesscontrol.PermissionConnectionRead),
		"workspaceServices":           collectionPermissions(accesscontrol.ResourceService, accesscontrol.PermissionServiceRead),
		"workspaceServicePage":        collectionPermissions(accesscontrol.ResourceService, accesscontrol.PermissionServiceRead),
		"workspaceWebhooks":           argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead),
		"webhookEvents":               argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead, accesscontrol.PermissionAuditRead),
		"webhookAnalytics":            argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead, accesscontrol.PermissionAuditRead),
		"engineExecutionEvents":       argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead, accesscontrol.PermissionAuditRead),
		"appExecutionEvents":          appArgumentPermissions("app_id", accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead),
		"appExecutionAnalytics":       appArgumentPermissions("app_id", accesscontrol.PermissionAppRead, accesscontrol.PermissionAuditRead),
		"engineExecutionAnalytics":    argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead, accesscontrol.PermissionAuditRead),
		"workspaceExecutionAnalytics": permissions(accesscontrol.PermissionAuditRead),
		"publicServiceInsights":       argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead, accesscontrol.PermissionAuditRead),
		"serviceConsumers":            argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceRead),
		"workspaceNotifications":      permissions(accesscontrol.PermissionWorkspaceRead),
		"appTokens":                   argumentPermissions(accesscontrol.ResourceApp, "app_family_id", accesscontrol.PermissionAppTokensManage),
		"sdkBuckets":                  relatedCollectionPermissions(accesscontrol.ResourceApp, "app_family_id", accesscontrol.PermissionAppRead, accesscontrol.ResourceBucket, accesscontrol.PermissionBucketRead),
		"bucketSDKPage":               relatedCollectionPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketRead, accesscontrol.ResourceApp, accesscontrol.PermissionAppRead),
		"bucketServicePage":           relatedCollectionPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketRead, accesscontrol.ResourceService, accesscontrol.PermissionServiceRead),
		"bucketValues":                argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketValuesRead),
		"bucketValuePage":             argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionBucketValuesRead),
		"secretMetas":                 argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionCredentialsMetadataRead),
		"secretMetaPage":              argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionCredentialsMetadataRead),
		"authConnections":             argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionConnectionRead),
		"authConnectionPage":          argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionConnectionRead),
		"connectionResources":         connectionPermissions("connection_id", accesscontrol.PermissionConnectionRead),
	},
	mutationRoots: map[string]graphQLFieldPolicy{
		"createUser":                        permissions(accesscontrol.PermissionAccessManage),
		"updateUser":                        permissions(accesscontrol.PermissionAccessManage),
		"suspendUser":                       permissions(accesscontrol.PermissionAccessManage),
		"reactivateUser":                    permissions(accesscontrol.PermissionAccessManage),
		"addTeamMember":                     permissions(accesscontrol.PermissionAccessManage),
		"removeTeamMember":                  permissions(accesscontrol.PermissionAccessManage),
		"issueUserCredential":               permissions(accesscontrol.PermissionAccessManage),
		"revokeUserCredential":              permissions(accesscontrol.PermissionAccessManage),
		"createTeam":                        permissions(accesscontrol.PermissionAccessManage),
		"updateTeam":                        permissions(accesscontrol.PermissionAccessManage),
		"archiveTeam":                       permissions(accesscontrol.PermissionAccessManage),
		"setTeamWorkspaceRole":              protectedValuePermissions("role", "OWNER", accesscontrol.PermissionAccountManage, accesscontrol.PermissionAccessManage),
		"grantTeamServiceAccess":            permissions(accesscontrol.PermissionAccessManage),
		"revokeTeamServiceAccess":           permissions(accesscontrol.PermissionAccessManage),
		"grantTeamBucketAccess":             permissions(accesscontrol.PermissionAccessManage),
		"revokeTeamBucketAccess":            permissions(accesscontrol.PermissionAccessManage),
		"grantTeamAppAccess":                permissions(accesscontrol.PermissionAccessManage),
		"revokeTeamAppAccess":               permissions(accesscontrol.PermissionAccessManage),
		"grantWorkspaceBucketAccess":        permissions(accesscontrol.PermissionAccessManage),
		"revokeWorkspaceBucketAccess":       permissions(accesscontrol.PermissionAccessManage),
		"grantWorkspaceAppAccess":           permissions(accesscontrol.PermissionAccessManage),
		"revokeWorkspaceAppAccess":          permissions(accesscontrol.PermissionAccessManage),
		"setWorkspaceConnectionProfile":     argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceManage),
		"resetWorkspaceConnectionProfile":   argumentPermissions(accesscontrol.ResourceService, "service_id", accesscontrol.PermissionServiceManage),
		"updateWorkspaceNotificationStatus": permissions(accesscontrol.PermissionNotificationUpdate),
		"deployMcpServer":                   deploymentPermissions(accesscontrol.PermissionWorkspaceRead),
		"deprecateApp":                      appArgumentPermissions("app_id", accesscontrol.PermissionAppManage),
		"undeprecateApp":                    appArgumentPermissions("app_id", accesscontrol.PermissionAppManage),
		"deactivateApp":                     appArgumentPermissions("app_id", accesscontrol.PermissionAppManage),
		"upsertSecrets":                     argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionCredentialsManage),
		"deleteSecrets":                     argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionCredentialsManage),
		"startConnectSession":               argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionConnectionManage, accesscontrol.PermissionBucketUse),
		"deleteAuthConnection":              argumentPermissions(accesscontrol.ResourceBucket, "bucket_id", accesscontrol.PermissionConnectionManage),
		"setDefaultConnectionResource":      connectionPermissions("connection_id", accesscontrol.PermissionConnectionManage),
		"rediscoverConnectionResources":     connectionPermissions("connection_id", accesscontrol.PermissionConnectionManage),
		"refreshMissingServiceContracts":    permissions(accesscontrol.PermissionServiceManage),
	},
	protected: map[string]graphQLFieldPolicy{
		// An MCP execution token is a credential, even when reached through a
		// read-only root or a fragment, so selecting it requires token management.
		"MCPServer.execution_token": permissions(accesscontrol.PermissionAppTokensManage),
		"BucketValue.value":         permissions(accesscontrol.PermissionBucketValuesRead),
		// Literal connection bindings can contain configuration values that are
		// more sensitive than the surrounding connection-profile metadata.
		"WorkspaceConnectionBinding.literal_value": permissions(accesscontrol.PermissionCredentialsMetadataRead),
		"WorkspaceConnectionProfile.profile":       permissions(accesscontrol.PermissionCredentialsMetadataRead),
	},
}

func permissions(values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{permissions: values, scope: graphQLScopeWorkspace}
}

func protectedValuePermissions(argument, value string, protected accesscontrol.Permission, values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{
		permissions: values, scope: graphQLScopeWorkspace,
		protectedArgument: argument, protectedValue: value, protectedPermission: protected,
	}
}

func authenticatedOnly() graphQLFieldPolicy {
	return graphQLFieldPolicy{authenticated: true, scope: graphQLScopeWorkspace}
}

func argumentPermissions(resource accesscontrol.ResourceType, argument string, values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{permissions: values, scope: graphQLScopeArgument, resource: resource, argument: argument}
}

func appArgumentPermissions(argument string, values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{permissions: values, scope: graphQLScopeAppArgument, resource: accesscontrol.ResourceApp, argument: argument}
}

func collectionPermissions(resource accesscontrol.ResourceType, values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{permissions: values, scope: graphQLScopeCollection, resource: resource}
}

func deploymentPermissions(values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{permissions: values, scope: graphQLScopeDeployment}
}

func connectionPermissions(argument string, values ...accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{
		permissions: values, scope: graphQLScopeConnection, resource: accesscontrol.ResourceBucket, argument: argument,
	}
}

func relatedCollectionPermissions(resource accesscontrol.ResourceType, argument string, permission accesscontrol.Permission, relatedResource accesscontrol.ResourceType, relatedPermission accesscontrol.Permission) graphQLFieldPolicy {
	return graphQLFieldPolicy{
		permissions: []accesscontrol.Permission{permission}, scope: graphQLScopeRelated, resource: resource, argument: argument,
		relatedPermission: relatedPermission, relatedResource: relatedResource,
	}
}

func validateGraphQLAuthorizationPolicy(schema *graphql.Schema, policy graphQLAuthorizationPolicy) error {
	if err := validateRootPolicies(schema.QueryType(), policy.queryRoots); err != nil {
		return err
	}
	if err := validateRootPolicies(schema.MutationType(), policy.mutationRoots); err != nil {
		return err
	}
	for path, fieldPolicy := range policy.protected {
		parts := strings.Split(path, ".")
		if len(parts) != 2 || !schemaHasObjectField(schema, parts[0], parts[1]) {
			return fmt.Errorf("%w: protected field %q does not exist", errGraphQLPolicyMissing, path)
		}
		if err := validateFieldPolicy(path, fieldPolicy); err != nil {
			return err
		}
	}
	return nil
}

func validateRootPolicies(root *graphql.Object, policies map[string]graphQLFieldPolicy) error {
	if root == nil {
		return fmt.Errorf("%w: root type is absent", errGraphQLPolicyMissing)
	}
	for fieldName := range root.Fields() {
		// Schema-to-policy coverage prevents a newly added resolver from becoming
		// reachable before its permissions are deliberately classified.
		fieldPolicy, ok := policies[fieldName]
		if !ok {
			return fmt.Errorf("%w: %s.%s", errGraphQLPolicyMissing, root.Name(), fieldName)
		}
		if err := validateFieldPolicy(root.Name()+"."+fieldName, fieldPolicy); err != nil {
			return err
		}
	}
	for fieldName := range policies {
		// Policy-to-schema coverage rejects stale declarations so renames cannot
		// silently leave reviewers believing a removed rule is still enforced.
		if _, ok := root.Fields()[fieldName]; !ok {
			return fmt.Errorf("%w: stale %s.%s", errGraphQLPolicyMissing, root.Name(), fieldName)
		}
	}
	return nil
}

func validateFieldPolicy(path string, policy graphQLFieldPolicy) error {
	if policy.authenticated {
		if len(policy.permissions) != 0 || policy.scope != graphQLScopeWorkspace {
			return fmt.Errorf("%w: %s has an invalid authenticated-only policy", errGraphQLPolicyMissing, path)
		}
		return nil
	}
	if len(policy.permissions) == 0 {
		return fmt.Errorf("%w: %s has no permission", errGraphQLPolicyMissing, path)
	}
	for _, permission := range policy.permissions {
		if err := accesscontrol.ValidatePermission(permission); err != nil {
			return fmt.Errorf("%w: %s: %v", errGraphQLPolicyMissing, path, err)
		}
	}
	if err := validateProtectedValuePolicy(path, policy); err != nil {
		return err
	}
	return validatePolicyScope(path, policy)
}

func validateProtectedValuePolicy(path string, policy graphQLFieldPolicy) error {
	configured := policy.protectedArgument != "" || policy.protectedValue != "" || policy.protectedPermission != ""
	if !configured {
		return nil
	}
	if policy.protectedArgument == "" || policy.protectedValue == "" {
		return fmt.Errorf("%w: %s has an incomplete protected value policy", errGraphQLPolicyMissing, path)
	}
	if err := accesscontrol.ValidatePermission(policy.protectedPermission); err != nil {
		return fmt.Errorf("%w: %s has an invalid protected permission: %v", errGraphQLPolicyMissing, path, err)
	}
	return nil
}

func validatePolicyScope(path string, policy graphQLFieldPolicy) error {
	switch policy.scope {
	case graphQLScopeWorkspace, graphQLScopeDeployment:
		return nil
	case graphQLScopeCollection:
		return validateCollectionPolicyScope(path, policy)
	case graphQLScopeArgument, graphQLScopeAppArgument, graphQLScopeConnection:
		return validateArgumentPolicyScope(path, policy)
	case graphQLScopeRelated:
		return validateRelatedPolicyScope(path, policy)
	default:
		return fmt.Errorf("%w: %s has unknown scope mode %q", errGraphQLPolicyMissing, path, policy.scope)
	}
}

func validateCollectionPolicyScope(path string, policy graphQLFieldPolicy) error {
	if accesscontrol.ValidateResourceType(policy.resource) != nil || policy.resource == accesscontrol.ResourceWorkspace {
		return fmt.Errorf("%w: %s has an invalid collection scope", errGraphQLPolicyMissing, path)
	}
	return nil
}

func validateArgumentPolicyScope(path string, policy graphQLFieldPolicy) error {
	if policy.argument == "" || accesscontrol.ValidateResourceType(policy.resource) != nil || policy.resource == accesscontrol.ResourceWorkspace {
		return fmt.Errorf("%w: %s has an invalid argument scope", errGraphQLPolicyMissing, path)
	}
	return nil
}

func validateRelatedPolicyScope(path string, policy graphQLFieldPolicy) error {
	if policy.argument == "" || accesscontrol.ValidateResourceType(policy.resource) != nil || accesscontrol.ValidateResourceType(policy.relatedResource) != nil {
		return fmt.Errorf("%w: %s has an invalid related scope", errGraphQLPolicyMissing, path)
	}
	if err := accesscontrol.ValidatePermission(policy.relatedPermission); err != nil {
		return fmt.Errorf("%w: %s has invalid related permission: %v", errGraphQLPolicyMissing, path, err)
	}
	return nil
}

func schemaHasObjectField(schema *graphql.Schema, typeName, fieldName string) bool {
	named, ok := schema.TypeMap()[typeName]
	if !ok {
		return false
	}
	object, ok := named.(*graphql.Object)
	if !ok {
		return false
	}
	_, ok = object.Fields()[fieldName]
	return ok
}

type graphQLAuthorizationPlan struct {
	requirements        []accesscontrol.Requirement
	scopes              []graphQLScopeRequest
	rootFields          int
	deployments         []sdkConfigDocument
	connections         []graphQLConnectionRequirement
	apps                []graphQLAppRequirement
	resolvedConnections map[uuid.UUID]store.AuthConnection
}

type graphQLAppRequirement struct {
	appID      uuid.UUID
	permission accesscontrol.Permission
}

type graphQLScopeRequest struct {
	permission accesscontrol.Permission
	resource   accesscontrol.ResourceType
}

type graphQLConnectionRequirement struct {
	connectionID uuid.UUID
	permission   accesscontrol.Permission
}

type graphQLPlanBuilder struct {
	schema        *graphql.Schema
	policy        graphQLAuthorizationPolicy
	fragments     map[string]*ast.FragmentDefinition
	requirements  map[accesscontrol.Requirement]struct{}
	scopes        map[graphQLScopeRequest]struct{}
	visiting      map[string]bool
	variables     map[string]any
	workspaceID   uuid.UUID
	introspection bool
	rootFields    int
	deployments   []sdkConfigDocument
	connections   []graphQLConnectionRequirement
	apps          []graphQLAppRequirement
}

func buildGraphQLAuthorizationPlan(schema *graphql.Schema, body []byte, workspaceID uuid.UUID) (graphQLAuthorizationPlan, error) {
	return buildGraphQLAuthorizationPlanWithOptions(schema, body, workspaceID, false)
}

func buildGraphQLAuthorizationPlanWithOptions(schema *graphql.Schema, body []byte, workspaceID uuid.UUID, allowIntrospection bool) (graphQLAuthorizationPlan, error) {
	var payload graphQLRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Query) == "" {
		return graphQLAuthorizationPlan{}, fmt.Errorf("%w: malformed payload", errInvalidGraphQLRequest)
	}
	document, err := parser.Parse(parser.ParseParams{Source: payload.Query})
	if err != nil {
		return graphQLAuthorizationPlan{}, fmt.Errorf("%w: parse query: %v", errInvalidGraphQLRequest, err)
	}
	operation, fragments, err := selectGraphQLOperation(document, payload.OperationName)
	if err != nil {
		return graphQLAuthorizationPlan{}, err
	}
	builder := graphQLPlanBuilder{
		schema: schema, policy: engineGraphQLPolicy, fragments: fragments, variables: payload.Variables, workspaceID: workspaceID,
		requirements: make(map[accesscontrol.Requirement]struct{}), scopes: make(map[graphQLScopeRequest]struct{}), visiting: make(map[string]bool), introspection: allowIntrospection,
	}
	if err := builder.collectOperation(operation); err != nil {
		return graphQLAuthorizationPlan{}, err
	}
	return builder.plan()
}

func selectGraphQLOperation(document *ast.Document, operationName string) (*ast.OperationDefinition, map[string]*ast.FragmentDefinition, error) {
	operations := make([]*ast.OperationDefinition, 0, 1)
	fragments := make(map[string]*ast.FragmentDefinition)
	for _, definition := range document.Definitions {
		switch typed := definition.(type) {
		case *ast.OperationDefinition:
			operations = append(operations, typed)
		case *ast.FragmentDefinition:
			fragments[typed.Name.Value] = typed
		}
	}
	operation := namedOperation(operations, operationName)
	if operation == nil {
		return nil, nil, fmt.Errorf("%w: operationName is required or does not match", errInvalidGraphQLRequest)
	}
	return operation, fragments, nil
}

func namedOperation(operations []*ast.OperationDefinition, operationName string) *ast.OperationDefinition {
	if operationName == "" && len(operations) == 1 {
		return operations[0]
	}
	for _, operation := range operations {
		if operation.Name != nil && operation.Name.Value == operationName {
			return operation
		}
	}
	return nil
}

func (b *graphQLPlanBuilder) collectOperation(operation *ast.OperationDefinition) error {
	rootType, policies := b.rootPolicy(operation.Operation)
	if rootType == nil {
		return fmt.Errorf("%w: unsupported operation %q", errInvalidGraphQLRequest, operation.Operation)
	}
	return b.collectRootSelections(operation.SelectionSet, rootType, policies)
}

func (b *graphQLPlanBuilder) rootPolicy(operation string) (*graphql.Object, map[string]graphQLFieldPolicy) {
	switch operation {
	case ast.OperationTypeQuery:
		return b.schema.QueryType(), b.policy.queryRoots
	case ast.OperationTypeMutation:
		return b.schema.MutationType(), b.policy.mutationRoots
	default:
		return nil, nil
	}
}

func (b *graphQLPlanBuilder) collectRootSelections(selectionSet *ast.SelectionSet, rootType *graphql.Object, policies map[string]graphQLFieldPolicy) error {
	return b.walkSelections(selectionSet, rootType, func(field *ast.Field, _ graphql.Type) (accesscontrol.ResourceRef, error) {
		policy, ok := policies[field.Name.Value]
		if !ok {
			return accesscontrol.ResourceRef{}, fmt.Errorf("%w: %s.%s", errGraphQLPolicyMissing, rootType.Name(), field.Name.Value)
		}
		if policy.scope == graphQLScopeCollection {
			b.addScopeRequests(policy)
			b.rootFields++
			return accesscontrol.ResourceRef{Type: policy.resource}, nil
		}
		if policy.scope == graphQLScopeDeployment {
			if err := b.collectDeployment(field); err != nil {
				return accesscontrol.ResourceRef{}, err
			}
		}
		if policy.scope == graphQLScopeConnection {
			return b.collectConnection(field, policy)
		}
		if policy.scope == graphQLScopeAppArgument {
			return b.collectApp(field, policy)
		}
		if policy.scope == graphQLScopeRelated {
			b.scopes[graphQLScopeRequest{permission: policy.relatedPermission, resource: policy.relatedResource}] = struct{}{}
		}
		resource, err := b.policyResource(field, policy)
		if err != nil {
			return accesscontrol.ResourceRef{}, err
		}
		b.addRequirements(policy.permissions, resource)
		if err := b.addProtectedValueRequirement(field, policy, resource); err != nil {
			return accesscontrol.ResourceRef{}, err
		}
		b.rootFields++
		return resource, nil
	})
}

func (b *graphQLPlanBuilder) collectApp(field *ast.Field, policy graphQLFieldPolicy) (accesscontrol.ResourceRef, error) {
	appID, err := b.uuidArgument(field, policy.argument)
	if err != nil {
		return accesscontrol.ResourceRef{}, err
	}
	for _, permission := range policy.permissions {
		b.apps = append(b.apps, graphQLAppRequirement{appID: appID, permission: permission})
	}
	b.rootFields++
	return accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: appID}, nil
}

func (b *graphQLPlanBuilder) addProtectedValueRequirement(field *ast.Field, policy graphQLFieldPolicy, resource accesscontrol.ResourceRef) error {
	if policy.protectedArgument == "" {
		return nil
	}
	value, present, err := b.optionalStringArgument(field, policy.protectedArgument)
	if err != nil || !present || value != policy.protectedValue {
		return err
	}
	b.addRequirements([]accesscontrol.Permission{policy.protectedPermission}, resource)
	return nil
}

func (b *graphQLPlanBuilder) optionalStringArgument(field *ast.Field, name string) (string, bool, error) {
	for _, argument := range field.Arguments {
		if argument.Name.Value != name {
			continue
		}
		switch typed := argument.Value.(type) {
		case *ast.EnumValue:
			return typed.Value, true, nil
		case *ast.StringValue:
			return typed.Value, true, nil
		case *ast.Variable:
			value, present := b.variables[typed.Name.Value]
			if value == nil {
				return "", false, nil
			}
			text, ok := value.(string)
			if !present || !ok {
				return "", false, fmt.Errorf("%w: %s must be a string", errInvalidGraphQLRequest, name)
			}
			return text, true, nil
		default:
			return "", false, fmt.Errorf("%w: %s must be a string", errInvalidGraphQLRequest, name)
		}
	}
	return "", false, nil
}

func (b *graphQLPlanBuilder) collectConnection(field *ast.Field, policy graphQLFieldPolicy) (accesscontrol.ResourceRef, error) {
	connectionID, err := b.uuidArgument(field, policy.argument)
	if err != nil {
		return accesscontrol.ResourceRef{}, err
	}
	for _, permission := range policy.permissions {
		b.connections = append(b.connections, graphQLConnectionRequirement{connectionID: connectionID, permission: permission})
		b.scopes[graphQLScopeRequest{permission: permission, resource: accesscontrol.ResourceBucket}] = struct{}{}
	}
	b.rootFields++
	// The connection's bucket is resolved in one batch before execution. An
	// empty ID here prevents the opaque connection ID being mistaken for a bucket.
	return accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket}, nil
}

func (b *graphQLPlanBuilder) collectDeployment(field *ast.Field) error {
	for _, argument := range field.Arguments {
		if argument.Name.Value != "config" {
			continue
		}
		variable, ok := argument.Value.(*ast.Variable)
		if !ok {
			return fmt.Errorf("%w: deployMcpServer config must use a variable", errInvalidGraphQLRequest)
		}
		raw, err := json.Marshal(b.variables[variable.Name.Value])
		if err != nil {
			return fmt.Errorf("%w: invalid deployMcpServer config", errInvalidGraphQLRequest)
		}
		var document sdkConfigDocument
		if err := json.Unmarshal(raw, &document); err != nil || strings.TrimSpace(document.Bucket) == "" {
			return fmt.Errorf("%w: invalid deployMcpServer config", errInvalidGraphQLRequest)
		}
		b.deployments = append(b.deployments, document)
		return nil
	}
	return fmt.Errorf("%w: deployMcpServer config is required", errInvalidGraphQLRequest)
}

func (b *graphQLPlanBuilder) addScopeRequests(policy graphQLFieldPolicy) {
	for _, permission := range policy.permissions {
		b.scopes[graphQLScopeRequest{permission: permission, resource: policy.resource}] = struct{}{}
	}
}

type graphQLFieldVisitor func(field *ast.Field, fieldType graphql.Type) (accesscontrol.ResourceRef, error)

func (b *graphQLPlanBuilder) walkSelections(selectionSet *ast.SelectionSet, parent *graphql.Object, visit graphQLFieldVisitor) error {
	if selectionSet == nil {
		return nil
	}
	for _, selection := range selectionSet.Selections {
		if err := b.walkSelection(selection, parent, visit); err != nil {
			return err
		}
	}
	return nil
}

func (b *graphQLPlanBuilder) walkSelection(selection ast.Selection, parent *graphql.Object, visit graphQLFieldVisitor) error {
	switch typed := selection.(type) {
	case *ast.Field:
		return b.walkField(typed, parent, visit)
	case *ast.InlineFragment:
		return b.walkInlineFragment(typed, parent, visit)
	case *ast.FragmentSpread:
		return b.walkFragment(typed.Name.Value, parent, visit)
	default:
		return fmt.Errorf("%w: unsupported selection", errInvalidGraphQLRequest)
	}
}

func (b *graphQLPlanBuilder) walkField(field *ast.Field, parent *graphql.Object, visit graphQLFieldVisitor) error {
	if field.Name.Value == "__typename" {
		return nil
	}
	if strings.HasPrefix(field.Name.Value, "__") {
		return b.collectIntrospection(field, parent)
	}
	definition, ok := parent.Fields()[field.Name.Value]
	if !ok {
		return fmt.Errorf("%w: unknown field %s.%s", errInvalidGraphQLRequest, parent.Name(), field.Name.Value)
	}
	resource, err := visit(field, definition.Type)
	if err != nil {
		return err
	}
	if protected, ok := b.policy.protected[parent.Name()+"."+field.Name.Value]; ok {
		if resource.ID == uuid.Nil && resource.Type != accesscontrol.ResourceWorkspace {
			// A protected child selected from a collection must not combine a
			// root grant for resource A with a protected-field grant for B. Until
			// per-root scope intersections are carried into repository queries,
			// require the protected permission workspace-wide and fail closed.
			workspace, err := b.workspaceResource()
			if err != nil {
				return err
			}
			b.addRequirements(protected.permissions, workspace)
		} else {
			b.addRequirements(protected.permissions, resource)
		}
	}
	child, ok := graphql.GetNamed(definition.Type).(*graphql.Object)
	if !ok {
		return nil
	}
	return b.walkSelections(field.SelectionSet, child, inheritedFieldPolicy(resource))
}

func (b *graphQLPlanBuilder) collectIntrospection(field *ast.Field, parent *graphql.Object) error {
	validRoot := parent == b.schema.QueryType() && (field.Name.Value == "__schema" || field.Name.Value == "__type")
	if !b.introspection || !validRoot {
		return fmt.Errorf("%w: introspection is disabled", errInvalidGraphQLRequest)
	}
	// Development introspection needs only the authenticated Actor already
	// required by the handler; it exposes schema metadata, not workspace data.
	b.rootFields++
	return nil
}

func inheritedFieldPolicy(resource accesscontrol.ResourceRef) graphQLFieldVisitor {
	return func(*ast.Field, graphql.Type) (accesscontrol.ResourceRef, error) {
		// Child fields without their own resolvers inherit the root resource.
		// Sensitive projections are additive and declared in policy.protected.
		return resource, nil
	}
}

func (b *graphQLPlanBuilder) walkInlineFragment(fragment *ast.InlineFragment, parent *graphql.Object, visit graphQLFieldVisitor) error {
	fragmentType, err := b.fragmentType(parent, fragment.TypeCondition)
	if err != nil {
		return err
	}
	return b.walkSelections(fragment.SelectionSet, fragmentType, visit)
}

func (b *graphQLPlanBuilder) walkFragment(name string, parent *graphql.Object, visit graphQLFieldVisitor) error {
	fragment := b.fragments[name]
	if fragment == nil || b.visiting[name] {
		return fmt.Errorf("%w: invalid fragment %q", errInvalidGraphQLRequest, name)
	}
	b.visiting[name] = true
	defer delete(b.visiting, name)
	fragmentType, err := b.fragmentType(parent, fragment.TypeCondition)
	if err != nil {
		return err
	}
	return b.walkSelections(fragment.SelectionSet, fragmentType, visit)
}

func (b *graphQLPlanBuilder) fragmentType(parent *graphql.Object, condition *ast.Named) (*graphql.Object, error) {
	if condition == nil {
		return parent, nil
	}
	typeName := condition.Name.Value
	named, ok := b.schema.TypeMap()[typeName]
	if !ok {
		return nil, fmt.Errorf("%w: unknown fragment type %q", errInvalidGraphQLRequest, typeName)
	}
	object, ok := named.(*graphql.Object)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported fragment type %q", errInvalidGraphQLRequest, typeName)
	}
	if object != parent {
		return nil, fmt.Errorf("%w: fragment type %q cannot apply to %q", errInvalidGraphQLRequest, typeName, parent.Name())
	}
	return object, nil
}

func (b *graphQLPlanBuilder) addRequirements(permissions []accesscontrol.Permission, resource accesscontrol.ResourceRef) {
	for _, permission := range permissions {
		b.requirements[accesscontrol.Requirement{Permission: permission, Resource: resource}] = struct{}{}
	}
}

func (b *graphQLPlanBuilder) policyResource(field *ast.Field, policy graphQLFieldPolicy) (accesscontrol.ResourceRef, error) {
	if policy.scope != graphQLScopeArgument && policy.scope != graphQLScopeRelated {
		return b.workspaceResource()
	}
	value, err := b.uuidArgument(field, policy.argument)
	if err != nil {
		return accesscontrol.ResourceRef{}, err
	}
	return accesscontrol.ResourceRef{Type: policy.resource, ID: value}, nil
}

func (b *graphQLPlanBuilder) workspaceResource() (accesscontrol.ResourceRef, error) {
	if b.workspaceID == uuid.Nil {
		return accesscontrol.ResourceRef{}, fmt.Errorf("%w: actor workspace is invalid", errGraphQLPolicyMissing)
	}
	return accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: b.workspaceID}, nil
}

func (b *graphQLPlanBuilder) uuidArgument(field *ast.Field, name string) (uuid.UUID, error) {
	for _, argument := range field.Arguments {
		if argument.Name.Value == name {
			return b.parseUUIDValue(argument.Value, name)
		}
	}
	return uuid.Nil, fmt.Errorf("%w: %s requires %s", errInvalidGraphQLRequest, field.Name.Value, name)
}

func (b *graphQLPlanBuilder) parseUUIDValue(value ast.Value, argumentName string) (uuid.UUID, error) {
	var raw string
	switch typed := value.(type) {
	case *ast.StringValue:
		raw = typed.Value
	case *ast.Variable:
		raw, _ = b.variables[typed.Name.Value].(string)
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s must be a UUID", errInvalidGraphQLRequest, argumentName)
	}
	return parsed, nil
}

func (b *graphQLPlanBuilder) plan() (graphQLAuthorizationPlan, error) {
	if b.workspaceID == uuid.Nil {
		return graphQLAuthorizationPlan{}, fmt.Errorf("%w: actor workspace is invalid", errGraphQLPolicyMissing)
	}
	requirements := make([]accesscontrol.Requirement, 0, len(b.requirements))
	for requirement := range b.requirements {
		requirements = append(requirements, requirement)
	}
	sort.Slice(requirements, func(i, j int) bool { return requirementSortKey(requirements[i]) < requirementSortKey(requirements[j]) })
	scopes := make([]graphQLScopeRequest, 0, len(b.scopes))
	for request := range b.scopes {
		scopes = append(scopes, request)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return string(scopes[i].permission)+string(scopes[i].resource) < string(scopes[j].permission)+string(scopes[j].resource)
	})
	return graphQLAuthorizationPlan{
		requirements: requirements, scopes: scopes, rootFields: b.rootFields,
		deployments: b.deployments, connections: b.connections, apps: b.apps,
	}, nil
}

func requirementSortKey(requirement accesscontrol.Requirement) string {
	return string(requirement.Permission) + "\x00" + string(requirement.Resource.Type) + "\x00" + requirement.Resource.ID.String()
}

func (p *graphQLAuthorizationPlan) mergeRequirements(additional []accesscontrol.Requirement) {
	if len(additional) == 0 {
		return
	}
	unique := make(map[accesscontrol.Requirement]struct{}, len(p.requirements)+len(additional))
	for _, requirement := range p.requirements {
		unique[requirement] = struct{}{}
	}
	for _, requirement := range additional {
		unique[requirement] = struct{}{}
	}
	p.requirements = sortedRequirements(unique)
}
