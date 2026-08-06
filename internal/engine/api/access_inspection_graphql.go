package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var accessResourceTypeGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "AccessResourceType",
	Values: graphql.EnumValueConfigMap{
		"WORKSPACE": &graphql.EnumValueConfig{Value: accesscontrol.ResourceWorkspace},
		"SERVICE":   &graphql.EnumValueConfig{Value: accesscontrol.ResourceService},
		"BUCKET":    &graphql.EnumValueConfig{Value: accesscontrol.ResourceBucket},
		"APP":       &graphql.EnumValueConfig{Value: accesscontrol.ResourceApp},
	},
})

var auditOutcomeGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "AuditOutcome",
	Values: graphql.EnumValueConfigMap{
		"ATTEMPTED": &graphql.EnumValueConfig{Value: accesscontrol.AuditAttempted},
		"ALLOWED":   &graphql.EnumValueConfig{Value: accesscontrol.AuditAllowed},
		"DENIED":    &graphql.EnumValueConfig{Value: accesscontrol.AuditDenied},
		"SUCCEEDED": &graphql.EnumValueConfig{Value: accesscontrol.AuditSucceeded},
		"FAILED":    &graphql.EnumValueConfig{Value: accesscontrol.AuditFailed},
	},
})

var accessRequirementGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AccessRequirement",
	Fields: graphql.Fields{
		"permission":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_type": &graphql.Field{Type: graphql.NewNonNull(accessResourceTypeGraphQLEnum)},
		"resource_id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
	},
})

var currentActorAccessGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "CurrentActorAccess",
	Fields: graphql.Fields{
		"subject_id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"workspace_id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"kind":                   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"grants":                 &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(accessRequirementGraphQLType)))},
	},
})

var accessGrantSourceGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AccessGrantSource",
	Fields: graphql.Fields{
		"principal_type": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"principal_id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"team_name":      &graphql.Field{Type: graphql.String},
		"role_slug":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_type":  &graphql.Field{Type: graphql.NewNonNull(accessResourceTypeGraphQLEnum)},
		"resource_id":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
	},
})

var accessExplanationGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AccessExplanation",
	Fields: graphql.Fields{
		"requirement": &graphql.Field{Type: graphql.NewNonNull(accessRequirementGraphQLType)},
		"allowed":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"sources":     &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(accessGrantSourceGraphQLType)))},
		"missing":     &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(accessRequirementGraphQLType)))},
	},
})

var auditRecordGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AuditRecord",
	Fields: graphql.Fields{
		"id":                   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"occurred_at":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"actor_subject_id":     &graphql.Field{Type: graphql.ID},
		"actor_credential_id":  &graphql.Field{Type: graphql.ID},
		"action":               &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"permission":           &graphql.Field{Type: graphql.String},
		"resource_type":        &graphql.Field{Type: accessResourceTypeGraphQLEnum},
		"resource_id":          &graphql.Field{Type: graphql.ID},
		"request_id":           &graphql.Field{Type: graphql.String},
		"trace_id":             &graphql.Field{Type: graphql.String},
		"method":               &graphql.Field{Type: graphql.String},
		"path":                 &graphql.Field{Type: graphql.String},
		"outcome":              &graphql.Field{Type: graphql.NewNonNull(auditOutcomeGraphQLEnum)},
		"status_code":          &graphql.Field{Type: graphql.Int},
		"reason_code":          &graphql.Field{Type: graphql.String},
		"missing_requirements": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(accessRequirementGraphQLType)))},
		"metadata":             &graphql.Field{Type: engineJSONType},
	},
})

var auditPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AuditPage",
	Fields: graphql.Fields{
		"items":       &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(auditRecordGraphQLType)))},
		"total":       &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"next_cursor": &graphql.Field{Type: graphql.String},
	},
})

func currentActorAccessGraphQLField() *graphql.Field {
	return &graphql.Field{
		Type: currentActorAccessGraphQLType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, ok := accesscontrol.ActorFromContext(p.Context)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			grants := actor.Authorization.EffectiveGrants(actor.WorkspaceID)
			return map[string]interface{}{
				"subject_id": actor.SubjectID.String(), "workspace_id": actor.WorkspaceID.String(),
				"kind": string(actor.Kind), "authorization_revision": actor.Authorization.Revision,
				"grants": projectCurrentActorGrants(grants),
			}, nil
		},
	}
}

func projectCurrentActorGrants(grants []accesscontrol.Grant) []map[string]interface{} {
	projected := make([]map[string]interface{}, len(grants))
	for index, grant := range grants {
		projected[index] = map[string]interface{}{
			"permission": string(grant.Permission), "resource_type": grant.Resource.Type,
			"resource_id": grant.Resource.ID.String(),
		}
	}
	return projected
}

func accessExplanationGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: accessExplanationGraphQLType,
		Args: graphql.FieldConfigArgument{
			"target_subject_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
			"permission":        &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"resource_type":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(accessResourceTypeGraphQLEnum)},
			"resource_id":       &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, ok := accesscontrol.ActorFromContext(p.Context)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			query, err := accessExplanationQuery(p, actor.SubjectID)
			if err != nil {
				return nil, err
			}
			repository, ok := s.(store.AccessInspectionRepository)
			if !ok {
				return nil, errors.New("access explanation is unavailable")
			}
			explanation, err := repository.ExplainAccess(p.Context, query)
			if err != nil {
				// Hidden and missing resources are deliberately indistinguishable.
				return nil, errors.New("access explanation is unavailable")
			}
			return projectAccessExplanation(explanation), nil
		},
	}
}

func auditEventsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: auditPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"actor_subject_id": &graphql.ArgumentConfig{Type: graphql.ID},
			"actions":          &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"outcomes":         &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(auditOutcomeGraphQLEnum))},
			"from":             &graphql.ArgumentConfig{Type: graphql.String},
			"to":               &graphql.ArgumentConfig{Type: graphql.String},
			"after":            &graphql.ArgumentConfig{Type: graphql.String},
			"limit":            &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 50},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.audit_events")
			defer span.End()
			actor, ok := accesscontrol.ActorFromContext(ctx)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			query, err := auditQueryFromGraphQL(p, actor.SubjectID)
			if err != nil {
				return nil, err
			}
			repository, ok := s.(store.AuditRepository)
			if !ok {
				return nil, errors.New("audit events are unavailable")
			}
			page, err := repository.QueryAuditEvents(ctx, query)
			if err != nil {
				return nil, errors.New("audit events are unavailable")
			}
			span.SetAttributes(attribute.Int("audit.result_count", len(page.Items)))
			return projectAuditPage(page), nil
		},
	}
}

func accessExplanationQuery(p graphql.ResolveParams, requester uuid.UUID) (store.AccessExplanationQuery, error) {
	target, err := requiredGraphQLUUIDArg(p, "target_subject_id")
	if err != nil {
		return store.AccessExplanationQuery{}, err
	}
	resourceID, err := requiredGraphQLUUIDArg(p, "resource_id")
	if err != nil {
		return store.AccessExplanationQuery{}, err
	}
	resourceType, ok := p.Args["resource_type"].(accesscontrol.ResourceType)
	permission := accesscontrol.Permission(strings.TrimSpace(graphQLArgString(p, "permission")))
	if !ok || accesscontrol.ValidatePermission(permission) != nil {
		return store.AccessExplanationQuery{}, errors.New("invalid access requirement")
	}
	return store.AccessExplanationQuery{
		RequesterSubjectID: requester, TargetSubjectID: target,
		Requirement: accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: resourceType, ID: resourceID}},
	}, nil
}

func auditQueryFromGraphQL(p graphql.ResolveParams, requester uuid.UUID) (store.AuditQuery, error) {
	actorSubjectID, err := optionalGraphQLUUIDArg(p, "actor_subject_id")
	if err != nil {
		return store.AuditQuery{}, err
	}
	from, err := optionalGraphQLTimeArg(p, "from")
	if err != nil {
		return store.AuditQuery{}, err
	}
	to, err := optionalGraphQLTimeArg(p, "to")
	if err != nil {
		return store.AuditQuery{}, err
	}
	after, err := decodeAuditCursor(graphQLArgString(p, "after"))
	if err != nil {
		return store.AuditQuery{}, err
	}
	limit, _ := p.Args["limit"].(int)
	if limit < 1 || limit > 200 {
		return store.AuditQuery{}, errors.New("invalid audit limit")
	}
	return store.AuditQuery{
		RequesterSubjectID: requester, ActorSubjectID: actorSubjectID, Actions: graphQLStringListArg(p, "actions"),
		Outcomes: graphQLAuditOutcomes(p), From: from, To: to, After: after, Limit: limit,
	}, nil
}

func graphQLAuditOutcomes(p graphql.ResolveParams) []accesscontrol.AuditOutcome {
	values, _ := p.Args["outcomes"].([]interface{})
	outcomes := make([]accesscontrol.AuditOutcome, 0, len(values))
	for _, value := range values {
		if outcome, ok := value.(accesscontrol.AuditOutcome); ok {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes
}

func optionalGraphQLTimeArg(p graphql.ResolveParams, name string) (*time.Time, error) {
	value := strings.TrimSpace(graphQLArgString(p, name))
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", name)
	}
	return &parsed, nil
}

func encodeAuditCursor(cursor *store.AuditCursor) string {
	if cursor == nil {
		return ""
	}
	raw, _ := json.Marshal(struct {
		OccurredAt time.Time `json:"occurred_at"`
		ID         uuid.UUID `json:"id"`
	}{cursor.OccurredAt, cursor.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAuditCursor(value string) (*store.AuditCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 512 {
		return nil, errors.New("invalid audit cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid audit cursor")
	}
	var encoded struct {
		OccurredAt time.Time `json:"occurred_at"`
		ID         uuid.UUID `json:"id"`
	}
	if json.Unmarshal(raw, &encoded) != nil || encoded.ID == uuid.Nil || encoded.OccurredAt.IsZero() {
		return nil, errors.New("invalid audit cursor")
	}
	return &store.AuditCursor{OccurredAt: encoded.OccurredAt, ID: encoded.ID}, nil
}

func projectAccessExplanation(explanation store.AccessExplanation) map[string]interface{} {
	sources := make([]map[string]interface{}, 0, len(explanation.Sources))
	for _, source := range explanation.Sources {
		sources = append(sources, map[string]interface{}{
			"principal_type": source.PrincipalType, "principal_id": source.PrincipalID.String(), "team_name": source.TeamName,
			"role_slug": source.RoleSlug, "resource_type": source.Resource.Type, "resource_id": source.Resource.ID.String(),
		})
	}
	return map[string]interface{}{
		"requirement": projectAccessRequirement(explanation.Requirement), "allowed": explanation.Allowed,
		"sources": sources, "missing": projectAccessRequirements(explanation.Missing),
	}
}

func projectAccessRequirements(requirements []accesscontrol.Requirement) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(requirements))
	for _, requirement := range requirements {
		items = append(items, projectAccessRequirement(requirement))
	}
	return items
}

func projectAccessRequirement(requirement accesscontrol.Requirement) map[string]interface{} {
	return map[string]interface{}{
		"permission": string(requirement.Permission), "resource_type": requirement.Resource.Type, "resource_id": requirement.Resource.ID.String(),
	}
}

func projectAuditPage(page store.AuditPage) map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(page.Items))
	for _, record := range page.Items {
		items = append(items, projectAuditRecord(record))
	}
	nextCursor := encodeAuditCursor(page.NextCursor)
	var cursor interface{}
	if nextCursor != "" {
		cursor = nextCursor
	}
	return map[string]interface{}{"items": items, "total": page.Total, "next_cursor": cursor}
}

func projectAuditRecord(record store.AuditRecord) map[string]interface{} {
	missing := make([]map[string]interface{}, 0, len(record.MissingRequirements))
	for _, requirement := range record.MissingRequirements {
		missing = append(missing, projectAccessRequirement(requirement))
	}
	item := map[string]interface{}{
		"id": record.ID.String(), "occurred_at": record.OccurredAt.UTC().Format(time.RFC3339Nano), "action": record.Action,
		"outcome": record.Outcome, "status_code": record.StatusCode, "reason_code": record.ReasonCode, "request_id": record.RequestID,
		"trace_id": record.TraceID, "method": record.Method, "path": record.Path, "missing_requirements": missing, "metadata": record.Metadata,
	}
	if record.ActorSubjectID != nil {
		item["actor_subject_id"] = record.ActorSubjectID.String()
	}
	if record.ActorCredentialID != nil {
		item["actor_credential_id"] = record.ActorCredentialID.String()
	}
	if record.Permission != "" {
		item["permission"] = string(record.Permission)
	}
	if record.Resource.ID != uuid.Nil {
		item["resource_type"], item["resource_id"] = record.Resource.Type, record.Resource.ID.String()
	}
	return item
}
