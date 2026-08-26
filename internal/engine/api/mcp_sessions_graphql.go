package api

import (
	"errors"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

var mcpSessionPageType = graphql.NewObject(graphql.ObjectConfig{
	Name: "MCPSessionPage",
	Fields: graphql.Fields{
		"items":       &graphql.Field{Type: graphql.NewList(mcpSessionSummaryType)},
		"next_cursor": &graphql.Field{Type: graphql.String},
		"has_more":    &graphql.Field{Type: graphql.Boolean},
	},
})

// mcpSessionsField exposes exact-app history only through the shared app.read/audit.read GraphQL gate.
func mcpSessionsField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: mcpSessionPageType,
		Args: graphql.FieldConfigArgument{
			"app_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"first":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 25},
		},
		// The resolver repeats ownership checks before touching permission-gated network provenance.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return resolveMCPSessions(p, s) },
	}
}

// resolveMCPSessions never falls back to in-process sessions or exposes raw storage errors.
func resolveMCPSessions(p graphql.ResolveParams, s store.Store) (interface{}, error) {
	actor, err := actorFromContext(p.Context)
	// Unauthenticated requests cannot inspect even an empty history page.
	if err != nil {
		return nil, err
	}
	appID, err := uuid.Parse(graphQLArgString(p, "app_id"))
	// Exact version identity is required rather than a family or display name.
	if err != nil {
		return nil, errors.New("invalid app_id")
	}
	_, err = appOwnedBy(p.Context, s, actor.accountID, appID)
	// Cross-account app identity must fail before issuing the history query.
	if err != nil {
		return nil, err
	}
	reader, ok := s.(store.MCPSessionReader)
	// Unsupported stores cannot substitute an unscoped history source.
	if !ok {
		return nil, errors.New("MCP session history is unavailable")
	}
	first, _ := p.Args["first"].(int)
	page, err := reader.ListMCPSessions(p.Context, actor.accountID, appID, graphQLArgString(p, "after"), first)
	// Storage and cursor errors may contain internal details, so the browser gets one fixed boundary.
	if err != nil {
		return nil, errors.New("MCP session history could not be loaded; check pagination and retry")
	}
	return map[string]any{"items": mcpSessionHistoryFields(page.Items), "next_cursor": page.NextCursor, "has_more": page.HasMore}, nil
}

// mcpSessionHistoryFields returns display provenance only; credentials and provider data have no projection.
func mcpSessionHistoryFields(sessions []models.MCPSession) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(sessions))
	for _, session := range sessions {
		endedAt := ""
		// Absence is historical uncertainty, not a fabricated termination timestamp.
		if session.EndedAt != nil {
			endedAt = session.EndedAt.Format(mcpGraphQLTimeFormat)
		}
		items = append(items, map[string]interface{}{
			"id": session.ID.String(), "app_token_id": optionalGraphQLUUIDValue(session.AppTokenID),
			"session_id": session.SessionID, "protocol_version": session.ProtocolVersion,
			"started_at": session.StartedAt.Format(mcpGraphQLTimeFormat), "last_activity_at": session.LastActivityAt.Format(mcpGraphQLTimeFormat),
			"ended_at": endedAt, "end_reason": session.EndReason,
			"client_name": session.ClientName, "client_version": session.ClientVersion, "initial_client_ip": session.InitialClientIP,
		})
	}
	return items
}
