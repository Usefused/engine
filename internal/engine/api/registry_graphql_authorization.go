package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var errInvalidRegistryGraphQLRequest = errors.New("invalid Registry GraphQL request")

type registryGraphQLRequestPayload struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
}

var registryGraphQLQueryPolicies = registryGraphQLPolicies(
	[]string{
		"eligibleConnectionProfiles", "connectionProfileContracts", "connectionProfileRevision",
		"services", "serviceIdBySlug", "serviceIdsBySlugs", "servicesByIds", "serviceVersions",
		"latestServiceVersions", "serviceVersionAuthConfigs", "serviceVersionExecutionAuthContracts", "serviceRuntimeContracts", "serviceWebhookMetadata",
		"serviceVersionImportIdentities", "service", "resourceIntegrations", "getServiceComponent",
		"integration", "endpointByName", "endpointsByNames", "serviceOperations", "validateSDKSelections",
		"searchEndpoints", "searchServices", "parseSDKIntent", "driftSnapshots", "driftSnapshotsForServices",
		"serviceChangelogSince",
	},
	[]accesscontrol.Permission{accesscontrol.PermissionCatalogueRead},
	map[string][]accesscontrol.Permission{
		"globalServiceAnalytics": {accesscontrol.PermissionCatalogueRead},
		"__typename":             {},
	},
)

var registryGraphQLMutationPolicies = map[string][]accesscontrol.Permission{
	"updateServicePublic":        {accesscontrol.PermissionCatalogueManage},
	"updateServiceVersionPublic": {accesscontrol.PermissionCatalogueManage},
	"setConnectionProfile":       {accesscontrol.PermissionServiceManage, accesscontrol.PermissionCredentialsManage},
	"__typename":                 {},
}

func authorizeRegistryGraphQLOperation(ctx context.Context, body []byte) (string, error) {
	document, operation, err := parseRegistryGraphQLOperation(body)
	if err != nil {
		return "", err
	}
	rootFields, err := collectRegistryRootFields(document, operation)
	if err != nil {
		return "", err
	}
	requirements, err := registryGraphQLRequirements(ctx, operation.Operation, rootFields)
	if err != nil {
		return "", err
	}
	if err := accesscontrol.ValidateAuditableRequirementCount(requirements); err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidRegistryGraphQLRequest, err)
	}
	accesscontrol.CaptureRequiredPermissions(ctx, requirements)
	if err := accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, requirements...); err != nil {
		accesscontrol.CaptureMissingPermissions(ctx, accesscontrol.MissingRequirements(err))
		return "", err
	}
	accesscontrol.CaptureMissingPermissions(ctx, nil)
	return operation.Operation, nil
}

func parseRegistryGraphQLOperation(body []byte) (*ast.Document, *ast.OperationDefinition, error) {
	var payload registryGraphQLRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Query) == "" {
		return nil, nil, fmt.Errorf("%w: decode payload", errInvalidRegistryGraphQLRequest)
	}
	document, err := parser.Parse(parser.ParseParams{Source: payload.Query})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parse document: %v", errInvalidRegistryGraphQLRequest, err)
	}
	operation, err := selectRegistryGraphQLOperation(document, payload.OperationName)
	if err != nil {
		return nil, nil, err
	}
	return document, operation, nil
}

func selectRegistryGraphQLOperation(document *ast.Document, operationName string) (*ast.OperationDefinition, error) {
	operations := registryGraphQLOperations(document)
	if operationName == "" && len(operations) == 1 {
		return supportedRegistryGraphQLOperation(operations[0])
	}
	for _, operation := range operations {
		if operation.Name != nil && operation.Name.Value == operationName {
			return supportedRegistryGraphQLOperation(operation)
		}
	}
	return nil, fmt.Errorf("%w: selected operation not found", errInvalidRegistryGraphQLRequest)
}

func registryGraphQLOperations(document *ast.Document) []*ast.OperationDefinition {
	operations := make([]*ast.OperationDefinition, 0, len(document.Definitions))
	for _, definition := range document.Definitions {
		if operation, ok := definition.(*ast.OperationDefinition); ok {
			operations = append(operations, operation)
		}
	}
	return operations
}

func supportedRegistryGraphQLOperation(operation *ast.OperationDefinition) (*ast.OperationDefinition, error) {
	if operation.Operation != "query" && operation.Operation != "mutation" {
		return nil, fmt.Errorf("%w: unsupported operation %q", errInvalidRegistryGraphQLRequest, operation.Operation)
	}
	return operation, nil
}

func collectRegistryRootFields(document *ast.Document, operation *ast.OperationDefinition) ([]string, error) {
	fragments := make(map[string]*ast.FragmentDefinition)
	for _, definition := range document.Definitions {
		if fragment, ok := definition.(*ast.FragmentDefinition); ok && fragment.Name != nil {
			fragments[fragment.Name.Value] = fragment
		}
	}
	fields := make(map[string]struct{})
	if err := collectRegistrySelections(operation.SelectionSet, fragments, make(map[string]bool), fields); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(fields))
	for field := range fields {
		result = append(result, field)
	}
	sort.Strings(result)
	return result, nil
}

func collectRegistrySelections(selectionSet *ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visiting map[string]bool, fields map[string]struct{}) error {
	if selectionSet == nil {
		return fmt.Errorf("%w: missing root selection", errInvalidRegistryGraphQLRequest)
	}
	for _, selection := range selectionSet.Selections {
		if err := collectRegistrySelection(selection, fragments, visiting, fields); err != nil {
			return err
		}
	}
	return nil
}

func collectRegistrySelection(selection ast.Selection, fragments map[string]*ast.FragmentDefinition, visiting map[string]bool, fields map[string]struct{}) error {
	switch selected := selection.(type) {
	case *ast.Field:
		if selected.Name != nil {
			fields[selected.Name.Value] = struct{}{}
		}
		return nil
	case *ast.InlineFragment:
		return collectRegistrySelections(selected.SelectionSet, fragments, visiting, fields)
	case *ast.FragmentSpread:
		return collectRegistryFragment(selected, fragments, visiting, fields)
	default:
		return fmt.Errorf("%w: unsupported selection", errInvalidRegistryGraphQLRequest)
	}
}

func collectRegistryFragment(spread *ast.FragmentSpread, fragments map[string]*ast.FragmentDefinition, visiting map[string]bool, fields map[string]struct{}) error {
	if spread.Name == nil || fragments[spread.Name.Value] == nil || visiting[spread.Name.Value] {
		return fmt.Errorf("%w: invalid fragment spread", errInvalidRegistryGraphQLRequest)
	}
	visiting[spread.Name.Value] = true
	err := collectRegistrySelections(fragments[spread.Name.Value].SelectionSet, fragments, visiting, fields)
	delete(visiting, spread.Name.Value)
	return err
}

func registryGraphQLRequirements(ctx context.Context, operation string, fields []string) ([]accesscontrol.Requirement, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return nil, accesscontrol.ErrAuthenticationRequired
	}
	policies := registryGraphQLQueryPolicies
	if operation == "mutation" {
		policies = registryGraphQLMutationPolicies
	}
	requirements := make([]accesscontrol.Requirement, 0, len(fields))
	for _, field := range fields {
		permissions, classified := registryGraphQLFieldPolicy(field, policies)
		if !classified {
			return nil, fmt.Errorf("unclassified Registry GraphQL root field %q", field)
		}
		for _, permission := range permissions {
			requirements = append(requirements, accesscontrol.Requirement{
				Permission: permission,
				Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID},
			})
		}
	}
	return requirements, nil
}

func registryGraphQLFieldPolicy(field string, policies map[string][]accesscontrol.Permission) ([]accesscontrol.Permission, bool) {
	permissions, ok := policies[field]
	if ok {
		return permissions, true
	}
	if (field == "__schema" || field == "__type") && os.Getenv("FUSED_ENV") == "development" {
		return nil, true
	}
	return nil, false
}

func registryGraphQLPolicies(fields []string, permissions []accesscontrol.Permission, exceptions map[string][]accesscontrol.Permission) map[string][]accesscontrol.Permission {
	policies := make(map[string][]accesscontrol.Permission, len(fields)+len(exceptions))
	for _, field := range fields {
		policies[field] = permissions
	}
	for field, fieldPermissions := range exceptions {
		policies[field] = fieldPermissions
	}
	return policies
}
