package api

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var (
	teamSlugSeparator = regexp.MustCompile(`[^a-z0-9]+`)
	errEmptyTeamPatch = errors.New("team update requires at least one field")
)

var teamBindingGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamBinding",
	Fields: graphql.Fields{
		"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"team_id":               &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"role_slug":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"role_display_name":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_type":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_id":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_display_name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"created_at":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var teamGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "Team",
	Fields: graphql.Fields{
		"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"slug":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"description": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"status":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"bindings":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(teamBindingGraphQLType)))},
		"created_at":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"updated_at":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var teamPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(teamGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var teamMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamMutationPayload",
	Fields: graphql.Fields{
		"team":                   &graphql.Field{Type: graphql.NewNonNull(teamGraphQLType)},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var teamBindingMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamBindingMutationPayload",
	Fields: graphql.Fields{
		"binding":                &graphql.Field{Type: teamBindingGraphQLType},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var createTeamGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "CreateTeamInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"name":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"slug":        &graphql.InputObjectFieldConfig{Type: graphql.String},
		"description": &graphql.InputObjectFieldConfig{Type: graphql.String},
	},
})

var updateTeamGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "UpdateTeamInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"name":        &graphql.InputObjectFieldConfig{Type: graphql.String},
		"slug":        &graphql.InputObjectFieldConfig{Type: graphql.String},
		"description": &graphql.InputObjectFieldConfig{Type: graphql.String},
	},
})

var teamWorkspaceRoleGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "TeamWorkspaceRole",
	Values: graphql.EnumValueConfigMap{
		"OWNER":   &graphql.EnumValueConfig{Value: accesscontrol.RoleOwner},
		"ADMIN":   &graphql.EnumValueConfig{Value: accesscontrol.RoleAdmin},
		"BUILDER": &graphql.EnumValueConfig{Value: accesscontrol.RoleBuilder},
		"VIEWER":  &graphql.EnumValueConfig{Value: accesscontrol.RoleViewer},
	},
})

var teamAccessLevelGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "TeamAccessLevel",
	Values: graphql.EnumValueConfigMap{
		"USER":    &graphql.EnumValueConfig{Value: "user"},
		"MANAGER": &graphql.EnumValueConfig{Value: "manager"},
	},
})

var teamArtifactAccessLevelGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "TeamArtifactAccessLevel",
	Values: graphql.EnumValueConfigMap{
		"READER":  &graphql.EnumValueConfig{Value: "reader"},
		"MANAGER": &graphql.EnumValueConfig{Value: "manager"},
	},
})

func teamsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: teamPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"search":           &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":            &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":           &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
			"include_archived": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repository, err := teamRepository(s)
			if err != nil {
				return nil, err
			}
			options := teamListOptions(p)
			teams, total, err := repository.ListTeams(p.Context, options)
			if err != nil {
				return nil, teamGraphQLError(err)
			}
			return map[string]interface{}{"items": projectGraphQLTeams(teams), "total": total}, nil
		},
	}
}

func teamGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: teamGraphQLType,
		Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, err := requiredGraphQLUUIDArg(p, "id")
			if err != nil {
				return nil, err
			}
			repository, err := teamRepository(s)
			if err != nil {
				return nil, err
			}
			team, err := repository.GetTeam(p.Context, id)
			if err != nil {
				return nil, teamGraphQLError(err)
			}
			return projectGraphQLTeam(team), nil
		},
	}
}

func createTeamGraphQLField(s store.Store) *graphql.Field {
	return teamMutationField(teamMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createTeamGraphQLInput)},
	}, "team.create", s, func(p graphql.ResolveParams, repository store.TeamRepository, actor store.MutationActor) (interface{}, error) {
		input, _ := p.Args["input"].(map[string]interface{})
		name, _ := input["name"].(string)
		slug, _ := input["slug"].(string)
		if slug == "" {
			// The UI only needs a team name for the common path; deriving the
			// stable slug server-side keeps CLI and UI behavior identical.
			slug = deriveTeamSlug(name)
		}
		description, _ := input["description"].(string)
		result, err := repository.CreateTeam(p.Context, store.TeamMutation{Name: name, Slug: slug, Description: description, Actor: actor})
		return projectTeamMutationResult(result), err
	})
}

func updateTeamGraphQLField(s store.Store) *graphql.Field {
	return teamMutationField(teamMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"id":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateTeamGraphQLInput)},
	}, "team.update", s, func(p graphql.ResolveParams, repository store.TeamRepository, actor store.MutationActor) (interface{}, error) {
		id, err := requiredGraphQLUUIDArg(p, "id")
		if err != nil {
			return nil, err
		}
		patch := teamPatchFromGraphQL(p.Args["input"], actor)
		if patch.Name == nil && patch.Slug == nil && patch.Description == nil {
			return nil, errEmptyTeamPatch
		}
		result, err := repository.UpdateTeam(p.Context, id, patch)
		return projectTeamMutationResult(result), err
	})
}

func archiveTeamGraphQLField(s store.Store) *graphql.Field {
	return teamMutationField(teamMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}, "team.archive", s, func(p graphql.ResolveParams, repository store.TeamRepository, actor store.MutationActor) (interface{}, error) {
		id, err := requiredGraphQLUUIDArg(p, "id")
		if err != nil {
			return nil, err
		}
		result, err := repository.ArchiveTeam(p.Context, id, actor)
		invalidateTeamAuthorization(p.Context, result.AuthorizationRevision, result.Changed, err)
		return projectTeamMutationResult(result), err
	})
}

func setTeamWorkspaceRoleGraphQLField(s store.Store) *graphql.Field {
	args := graphql.FieldConfigArgument{
		"team_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		// Null means remove the team's workspace role. Keeping this on the set
		// operation avoids another mutation/policy concept for a single nullable state.
		"role": &graphql.ArgumentConfig{Type: teamWorkspaceRoleGraphQLEnum},
	}
	return teamMutationField(teamBindingMutationPayloadGraphQLType, args, "team.workspace_role.set", s, resolveTeamWorkspaceRole)
}

func resolveTeamWorkspaceRole(p graphql.ResolveParams, repository store.TeamRepository, actor store.MutationActor) (interface{}, error) {
	teamID, err := requiredGraphQLUUIDArg(p, "team_id")
	if err != nil {
		return nil, err
	}
	workspaceID, err := teamBindingResourceID(p, accesscontrol.ResourceWorkspace, "")
	if err != nil {
		return nil, err
	}
	role, _ := p.Args["role"].(string)
	result, err := mutateTeamWorkspaceRole(p.Context, repository, teamID, workspaceID, role, actor)
	invalidateTeamAuthorization(p.Context, result.AuthorizationRevision, result.Changed, err)
	return projectTeamBindingMutationResult(result), err
}

func mutateTeamWorkspaceRole(ctx context.Context, repository store.TeamRepository, teamID, workspaceID uuid.UUID, role string, actor store.MutationActor) (store.TeamBindingMutationResult, error) {
	if role == "" {
		return repository.ClearTeamWorkspaceRole(ctx, teamID, workspaceID, actor)
	}
	return repository.AddTeamBinding(ctx, store.TeamBindingMutation{
		TeamID: teamID, RoleSlug: role,
		Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}, Actor: actor,
	})
}

func grantTeamServiceAccessGraphQLField(s store.Store) *graphql.Field {
	return teamResourceAccessGraphQLField(s, "team.service_access.grant", accesscontrol.ResourceService, "service_id", true)
}

func revokeTeamServiceAccessGraphQLField(s store.Store) *graphql.Field {
	return teamResourceAccessGraphQLField(s, "team.service_access.revoke", accesscontrol.ResourceService, "service_id", false)
}

func grantTeamBucketAccessGraphQLField(s store.Store) *graphql.Field {
	return teamResourceAccessGraphQLField(s, "team.bucket_access.grant", accesscontrol.ResourceBucket, "bucket_id", true)
}

func revokeTeamBucketAccessGraphQLField(s store.Store) *graphql.Field {
	return teamResourceAccessGraphQLField(s, "team.bucket_access.revoke", accesscontrol.ResourceBucket, "bucket_id", false)
}

func grantTeamArtifactAccessGraphQLField(s store.Store) *graphql.Field {
	return teamArtifactAccessGraphQLField(s, "team.artifact_access.grant", true)
}

func revokeTeamArtifactAccessGraphQLField(s store.Store) *graphql.Field {
	return teamArtifactAccessGraphQLField(s, "team.artifact_access.revoke", false)
}

func teamArtifactAccessGraphQLField(s store.Store, action string, add bool) *graphql.Field {
	role := func(p graphql.ResolveParams) string {
		if level, _ := p.Args["level"].(string); level == "manager" {
			return accesscontrol.RoleArtifactManager
		}
		return accesscontrol.RoleArtifactReader
	}
	return teamBindingMutationField(s, action, accesscontrol.ResourceArtifact, "artifact_id", teamArtifactAccessLevelGraphQLEnum, role, add)
}

func teamResourceAccessGraphQLField(s store.Store, action string, resourceType accesscontrol.ResourceType, resourceArgument string, add bool) *graphql.Field {
	role := func(p graphql.ResolveParams) string {
		level, _ := p.Args["level"].(string)
		return scopedTeamRole(resourceType, level)
	}
	return teamBindingMutationField(s, action, resourceType, resourceArgument, teamAccessLevelGraphQLEnum, role, add)
}

func teamBindingMutationField(s store.Store, action string, resourceType accesscontrol.ResourceType, resourceArgument string, levelType graphql.Input, role func(graphql.ResolveParams) string, add bool) *graphql.Field {
	args := graphql.FieldConfigArgument{
		"team_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}
	if resourceType == accesscontrol.ResourceWorkspace {
		args["role"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(teamWorkspaceRoleGraphQLEnum)}
	} else {
		args[resourceArgument] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}
		args["level"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(levelType)}
	}
	return teamMutationField(teamBindingMutationPayloadGraphQLType, args, action, s, func(p graphql.ResolveParams, repository store.TeamRepository, actor store.MutationActor) (interface{}, error) {
		teamID, err := requiredGraphQLUUIDArg(p, "team_id")
		if err != nil {
			return nil, err
		}
		resourceID, err := teamBindingResourceID(p, resourceType, resourceArgument)
		if err != nil {
			return nil, err
		}
		mutation := store.TeamBindingMutation{TeamID: teamID, RoleSlug: role(p), Resource: accesscontrol.ResourceRef{Type: resourceType, ID: resourceID}, Actor: actor}
		result, err := mutateTeamBinding(p.Context, repository, mutation, add)
		invalidateTeamAuthorization(p.Context, result.AuthorizationRevision, result.Changed, err)
		return projectTeamBindingMutationResult(result), err
	})
}

type teamMutationResolver func(graphql.ResolveParams, store.TeamRepository, store.MutationActor) (interface{}, error)

func teamMutationField(resultType graphql.Output, args graphql.FieldConfigArgument, action string, s store.Store, resolve teamMutationResolver) *graphql.Field {
	return &graphql.Field{Type: resultType, Args: args, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql."+action)
		defer span.End()
		p.Context = ctx
		repository, err := teamRepository(s)
		if err != nil {
			return nil, recordTeamMutationError(span, err)
		}
		actor, err := teamMutationActor(ctx)
		if err != nil {
			return nil, recordTeamMutationError(span, err)
		}
		span.SetAttributes(attribute.String("user_action", action), attribute.String("actor_subject_id", actor.SubjectID.String()))
		result, err := resolve(p, repository, actor)
		if err != nil {
			recordTeamMutationError(span, err)
			return nil, teamGraphQLError(err)
		}
		span.SetAttributes(attribute.String("outcome", "success"))
		return result, nil
	}}
}

func teamRepository(s store.Store) (store.TeamRepository, error) {
	repository, ok := s.(store.TeamRepository)
	if !ok {
		return nil, errors.New("team management is unavailable")
	}
	return repository, nil
}

func teamMutationActor(ctx context.Context) (store.MutationActor, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return store.MutationActor{}, accesscontrol.ErrAuthenticationRequired
	}
	return store.MutationActor{
		SubjectID: actor.SubjectID, CredentialID: actor.CredentialID,
		RequestID: middleware.GetReqID(ctx), TraceID: teamMutationTraceID(ctx),
	}, nil
}

func teamMutationTraceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func recordTeamMutationError(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetAttributes(attribute.String("outcome", "failure"))
	return err
}

func teamListOptions(p graphql.ResolveParams) store.TeamListOptions {
	includeArchived, _ := p.Args["include_archived"].(bool)
	statuses := []store.TeamStatus{store.TeamStatusActive}
	if includeArchived {
		statuses = []store.TeamStatus{store.TeamStatusActive, store.TeamStatusArchived}
	}
	search, _ := p.Args["search"].(string)
	limit, _ := p.Args["limit"].(int)
	offset, _ := p.Args["offset"].(int)
	return store.TeamListOptions{Statuses: statuses, Search: search, Limit: limit, Offset: offset}
}

func teamPatchFromGraphQL(value interface{}, actor store.MutationActor) store.TeamPatch {
	input, _ := value.(map[string]interface{})
	return store.TeamPatch{
		Name:        optionalGraphQLString(input, "name"),
		Slug:        optionalGraphQLString(input, "slug"),
		Description: optionalGraphQLString(input, "description"),
		Actor:       actor,
	}
}

func optionalGraphQLString(input map[string]interface{}, key string) *string {
	value, ok := input[key]
	if !ok {
		return nil
	}
	text, _ := value.(string)
	return &text
}

func teamBindingResourceID(p graphql.ResolveParams, resourceType accesscontrol.ResourceType, argument string) (uuid.UUID, error) {
	if resourceType == accesscontrol.ResourceWorkspace {
		actor, ok := accesscontrol.ActorFromContext(p.Context)
		if !ok {
			return uuid.Nil, accesscontrol.ErrAuthenticationRequired
		}
		return actor.WorkspaceID, nil
	}
	return requiredGraphQLUUIDArg(p, argument)
}

func mutateTeamBinding(ctx context.Context, repository store.TeamRepository, mutation store.TeamBindingMutation, add bool) (store.TeamBindingMutationResult, error) {
	if add {
		return repository.AddTeamBinding(ctx, mutation)
	}
	return repository.RemoveTeamBinding(ctx, mutation)
}

func invalidateTeamAuthorization(ctx context.Context, revision int64, changed bool, err error) {
	// The transaction has already committed when this runs. Advance the
	// process-local revision immediately so the next request cannot reuse an
	// authorization snapshot that predates the binding change.
	if err != nil || !changed {
		return
	}
	sink, _ := ctx.Value(mcpGraphQLRevisionSinkKey).(authorizationRevisionSink)
	if sink != nil {
		sink.SetRevision(revision)
	}
}

func scopedTeamRole(resourceType accesscontrol.ResourceType, level string) string {
	// API levels are intentionally resource-agnostic; this is the single map
	// from that compact UX to the built-in, resource-specific role catalogue.
	if resourceType == accesscontrol.ResourceService && level == "manager" {
		return accesscontrol.RoleServiceManager
	}
	if resourceType == accesscontrol.ResourceService {
		return accesscontrol.RoleServiceUser
	}
	if level == "manager" {
		return accesscontrol.RoleBucketManager
	}
	return accesscontrol.RoleBucketUser
}

func deriveTeamSlug(name string) string {
	slug := strings.Trim(teamSlugSeparator.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
	if len(slug) > 63 {
		slug = strings.TrimRight(slug[:63], "-")
	}
	return slug
}

func projectGraphQLTeams(teams []store.Team) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(teams))
	for _, team := range teams {
		items = append(items, projectGraphQLTeam(team))
	}
	return items
}

func projectGraphQLTeam(team store.Team) map[string]interface{} {
	bindings := make([]map[string]interface{}, 0, len(team.Bindings))
	for _, binding := range team.Bindings {
		bindings = append(bindings, projectGraphQLTeamBinding(binding))
	}
	return map[string]interface{}{
		"id": team.ID.String(), "name": team.Name, "slug": team.Slug, "description": team.Description,
		"status": string(team.Status), "bindings": bindings,
		"created_at": graphQLTeamTime(team.CreatedAt), "updated_at": graphQLTeamTime(team.UpdatedAt),
	}
}

func projectGraphQLTeamBinding(binding store.TeamBinding) map[string]interface{} {
	return map[string]interface{}{
		"id": binding.ID.String(), "team_id": binding.TeamID.String(), "role_slug": binding.RoleSlug,
		"role_display_name": binding.RoleDisplayName, "resource_type": string(binding.Resource.Type),
		"resource_id": binding.Resource.ID.String(), "resource_display_name": binding.ResourceDisplayName,
		"created_at": graphQLTeamTime(binding.CreatedAt),
	}
}

func projectTeamMutationResult(result store.TeamMutationResult) map[string]interface{} {
	return map[string]interface{}{"team": projectGraphQLTeam(result.Team), "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed}
}

func projectTeamBindingMutationResult(result store.TeamBindingMutationResult) map[string]interface{} {
	var binding interface{}
	// An idempotent revoke has no persisted binding row. Return null rather
	// than projecting the repository's requested identity with a zero UUID.
	if result.Binding.ID != uuid.Nil {
		binding = projectGraphQLTeamBinding(result.Binding)
	}
	return map[string]interface{}{"binding": binding, "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed}
}

func graphQLTeamTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func teamGraphQLError(err error) error {
	switch {
	case errors.Is(err, errEmptyTeamPatch):
		return errEmptyTeamPatch
	case errors.Is(err, store.ErrTeamNotFound):
		return errors.New("team not found")
	case errors.Is(err, store.ErrTeamSlugConflict):
		return errors.New("team slug already exists")
	case errors.Is(err, store.ErrTeamArchiveConflict):
		return errors.New("team cannot be archived while it has active access")
	case errors.Is(err, store.ErrTeamArchived):
		return errors.New("archived team cannot be changed")
	case errors.Is(err, store.ErrOwnerManagementForbidden):
		return errors.New("only a workspace Owner can change an Owner team")
	case errors.Is(err, store.ErrInvalidTeam), errors.Is(err, store.ErrInvalidTeamBinding):
		return errors.New("invalid team request")
	default:
		return errors.New("team operation failed")
	}
}
