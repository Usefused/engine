package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
)

var sensitiveGraphQLAuditFields = map[string]struct{}{
	"execution_token": {}, "value": {}, "literal_value": {}, "profile": {},
	"appTokens": {}, "bucketValues": {}, "bucketValuePage": {},
	"secretMetas": {}, "secretMetaPage": {}, "authConnections": {},
	"authConnectionPage": {}, "connectionResources": {}, "mcpAnalytics": {},
	"webhookEvents": {}, "webhookAnalytics": {}, "account": {}, "credits": {},
	"globalServiceAnalytics": {},
	// Access-control reads expose identities, credential metadata, memberships,
	// and effective grants. Persist their outcomes even though they are queries.
	"users": {}, "user": {}, "userEffectiveAccess": {}, "teamMembers": {},
	"teams": {}, "team": {}, "appBuildSelectors": {}, "appOwningTeams": {},
	"accessExplanation": {}, "auditEvents": {}, "workspaceConnectionProfile": {},
	"grantTeamAppAccess": {}, "revokeTeamAppAccess": {},
}

func classifyGraphQLAudit(request *http.Request) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxAuthorizationBodyBytes+1))
	request.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > maxAuthorizationBodyBytes {
		return "invalid", true
	}
	var payload struct {
		Query         string `json:"query"`
		OperationName string `json:"operationName"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Query) == "" {
		return "invalid", true
	}
	document, err := parser.Parse(parser.ParseParams{Source: payload.Query})
	if err != nil {
		return "invalid", true
	}
	operation, fragments := selectAuditOperation(document, payload.OperationName)
	if operation == nil {
		return "invalid", true
	}
	kind := operation.Operation
	return kind, kind == ast.OperationTypeMutation || auditSelectionsSensitive(operation.SelectionSet, fragments, map[string]bool{})
}

func selectAuditOperation(document *ast.Document, operationName string) (*ast.OperationDefinition, map[string]*ast.FragmentDefinition) {
	var selected *ast.OperationDefinition
	fragments := make(map[string]*ast.FragmentDefinition)
	operationCount := 0
	for _, definition := range document.Definitions {
		switch typed := definition.(type) {
		case *ast.OperationDefinition:
			operationCount++
			if operationName != "" && typed.Name != nil && typed.Name.Value == operationName {
				selected = typed
			}
			if operationName == "" {
				selected = typed
			}
		case *ast.FragmentDefinition:
			fragments[typed.Name.Value] = typed
		}
	}
	if operationName == "" && operationCount != 1 {
		return nil, fragments
	}
	return selected, fragments
}

func auditSelectionsSensitive(selectionSet *ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visiting map[string]bool) bool {
	if selectionSet == nil {
		return false
	}
	for _, selection := range selectionSet.Selections {
		if auditSelectionSensitive(selection, fragments, visiting) {
			return true
		}
	}
	return false
}

func auditSelectionSensitive(selection ast.Selection, fragments map[string]*ast.FragmentDefinition, visiting map[string]bool) bool {
	switch typed := selection.(type) {
	case *ast.Field:
		_, sensitive := sensitiveGraphQLAuditFields[typed.Name.Value]
		return sensitive || auditSelectionsSensitive(typed.SelectionSet, fragments, visiting)
	case *ast.InlineFragment:
		return auditSelectionsSensitive(typed.SelectionSet, fragments, visiting)
	case *ast.FragmentSpread:
		fragment := fragments[typed.Name.Value]
		if fragment == nil || visiting[typed.Name.Value] {
			return false
		}
		visiting[typed.Name.Value] = true
		defer delete(visiting, typed.Name.Value)
		return auditSelectionsSensitive(fragment.SelectionSet, fragments, visiting)
	default:
		return false
	}
}
