package api

import (
	"context"
	"errors"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var errEmptyUserPatch = errors.New("user update requires at least one field")

func usersGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: userPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"search":            &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: ""},
			"limit":             &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":            &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
			"include_suspended": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repository, err := userRepository(s)
			if err != nil {
				return nil, err
			}
			options := userListOptions(p, false)
			options.IncludeChildren = userChildrenRequested(p.Info)
			users, total, err := repository.ListUsers(p.Context, options)
			if err != nil {
				return nil, userGraphQLError(err)
			}
			return map[string]interface{}{"items": projectGraphQLUsers(users), "total": total}, nil
		},
	}
}

func userChildrenRequested(info graphql.ResolveInfo) bool {
	// Nested memberships and credentials are fetched in bounded batches only
	// when selected. Scalar-only lists avoid unnecessary joins, while this AST
	// walk prevents resolver-per-user N+1 queries for fragments and aliases.
	for _, field := range info.FieldASTs {
		if userChildSelectionRequested(field.SelectionSet, info.Fragments, map[string]bool{}) {
			return true
		}
	}
	return false
}

func userChildSelectionRequested(selectionSet *ast.SelectionSet, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	if selectionSet == nil {
		return false
	}
	for _, selection := range selectionSet.Selections {
		if userChildFieldRequested(selection, fragments, visiting) {
			return true
		}
	}
	return false
}

func userChildFieldRequested(selection ast.Selection, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	switch selected := selection.(type) {
	case *ast.Field:
		name := selected.Name.Value
		return name == "memberships" || name == "memberships_truncated" || name == "credentials" || name == "credentials_truncated" || userChildSelectionRequested(selected.SelectionSet, fragments, visiting)
	case *ast.InlineFragment:
		return userChildSelectionRequested(selected.SelectionSet, fragments, visiting)
	case *ast.FragmentSpread:
		return userChildFragmentRequested(selected, fragments, visiting)
	default:
		return false
	}
}

func userChildFragmentRequested(spread *ast.FragmentSpread, fragments map[string]ast.Definition, visiting map[string]bool) bool {
	name := spread.Name.Value
	fragment, ok := fragments[name].(*ast.FragmentDefinition)
	// Cyclic or repeated fragments must stop here; GraphQL validation normally
	// rejects cycles, but authorization-facing query planning still fails safe.
	if !ok || visiting[name] {
		return false
	}
	visiting[name] = true
	defer delete(visiting, name)
	return userChildSelectionRequested(fragment.SelectionSet, fragments, visiting)
}

func userGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: userGraphQLType,
		Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			userID, err := requiredGraphQLResourceReference(p, s, "id", store.ReferenceUser, uuid.Nil)
			if err != nil {
				return nil, err
			}
			repository, err := userRepository(s)
			if err != nil {
				return nil, err
			}
			user, err := repository.GetUser(p.Context, userID)
			if err != nil {
				return nil, userGraphQLError(err)
			}
			return projectGraphQLUser(user), nil
		},
	}
}

func userEffectiveAccessGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: userEffectiveAccessGraphQLType,
		Args: graphql.FieldConfigArgument{"user_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			userID, err := requiredGraphQLResourceReference(p, s, "user_id", store.ReferenceUser, uuid.Nil)
			if err != nil {
				return nil, err
			}
			repository, err := userRepository(s)
			if err != nil {
				return nil, err
			}
			grants, revision, err := repository.GetUserEffectiveAccess(p.Context, userID)
			if err != nil {
				return nil, userGraphQLError(err)
			}
			return map[string]interface{}{
				"user_id": userID.String(), "authorization_revision": int(revision),
				"grants": projectGraphQLEffectiveAccessGrants(grants),
			}, nil
		},
	}
}

func teamMembersGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: teamMemberPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"team_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
			"limit":   &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			teamID, err := requiredGraphQLResourceReference(p, s, "team_id", store.ReferenceTeam, uuid.Nil)
			if err != nil {
				return nil, err
			}
			repository, err := userRepository(s)
			if err != nil {
				return nil, err
			}
			members, total, err := repository.ListTeamMembers(p.Context, teamID, userListOptions(p, true))
			if err != nil {
				return nil, userGraphQLError(err)
			}
			return map[string]interface{}{"items": projectGraphQLTeamMembers(members), "total": total}, nil
		},
	}
}

func createUserGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(userMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createUserGraphQLInput)},
	}, "user.create", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		input, _ := p.Args["input"].(map[string]interface{})
		result, err := repository.CreateUser(p.Context, store.CreateUserInput{
			Email: graphQLInputString(input, "email"), DisplayName: graphQLInputString(input, "display_name"), Actor: actor,
		})
		return projectUserMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func updateUserGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(userMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"id":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateUserGraphQLInput)},
	}, "user.update", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		userID, err := requiredGraphQLResourceReference(p, repository, "id", store.ReferenceUser, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		input, _ := p.Args["input"].(map[string]interface{})
		patch := store.UserPatch{Email: optionalGraphQLString(input, "email"), DisplayName: optionalGraphQLString(input, "display_name"), Actor: actor}
		if patch.Email == nil && patch.DisplayName == nil {
			return nil, 0, false, errEmptyUserPatch
		}
		result, err := repository.UpdateUser(p.Context, userID, patch)
		return projectUserMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func suspendUserGraphQLField(s store.Store) *graphql.Field {
	return userStatusMutationGraphQLField(s, "user.suspend", func(ctx context.Context, repository store.UserRepository, userID uuid.UUID, actor store.MutationActor) (store.UserMutationResult, error) {
		return repository.SuspendUser(ctx, userID, actor)
	})
}

func reactivateUserGraphQLField(s store.Store) *graphql.Field {
	return userStatusMutationGraphQLField(s, "user.reactivate", func(ctx context.Context, repository store.UserRepository, userID uuid.UUID, actor store.MutationActor) (store.UserMutationResult, error) {
		return repository.ReactivateUser(ctx, userID, actor)
	})
}

type userStatusMutation func(context.Context, store.UserRepository, uuid.UUID, store.MutationActor) (store.UserMutationResult, error)

func userStatusMutationGraphQLField(s store.Store, action string, mutate userStatusMutation) *graphql.Field {
	return userMutationField(userMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}, action, s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		userID, err := requiredGraphQLResourceReference(p, repository, "id", store.ReferenceUser, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		result, err := mutate(p.Context, repository, userID, actor)
		return projectUserMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func addTeamMemberGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(teamMembershipMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"team_id":         &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"email":           &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"membership_role": &graphql.ArgumentConfig{Type: teamMembershipRoleGraphQLEnum, DefaultValue: "member"},
	}, "team.member.add", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		teamID, err := requiredGraphQLResourceReference(p, repository, "team_id", store.ReferenceTeam, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		role, _ := p.Args["membership_role"].(string)
		result, err := repository.AddTeamMemberByEmail(p.Context, store.AddTeamMemberByEmailInput{
			TeamID: teamID, Email: graphQLArgString(p, "email"), Role: store.MembershipRole(role), Actor: actor,
		})
		return projectMembershipMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func removeTeamMemberGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(teamMembershipMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"team_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"user_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}, "team.member.remove", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		teamID, err := requiredGraphQLResourceReference(p, repository, "team_id", store.ReferenceTeam, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		userID, err := requiredGraphQLResourceReference(p, repository, "user_id", store.ReferenceUser, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		result, err := repository.RemoveTeamMember(p.Context, teamID, userID, actor)
		return projectMembershipMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func issueUserCredentialGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(issuedCredentialPayloadGraphQLType, graphql.FieldConfigArgument{
		"user_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"name":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}, "user.credential.issue", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		userID, err := requiredGraphQLResourceReference(p, repository, "user_id", store.ReferenceUser, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		result, err := repository.IssueUserControlCredential(p.Context, store.IssueCredentialInput{
			UserID: userID, Name: graphQLArgString(p, "name"), Actor: actor,
		})
		return projectIssuedCredentialResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

func revokeUserCredentialGraphQLField(s store.Store) *graphql.Field {
	return userMutationField(credentialMutationPayloadGraphQLType, graphql.FieldConfigArgument{
		"user_id":       &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		"credential_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}, "user.credential.revoke", s, func(p graphql.ResolveParams, repository store.UserRepository, actor store.MutationActor) (interface{}, int64, bool, error) {
		userID, err := requiredGraphQLResourceReference(p, repository, "user_id", store.ReferenceUser, uuid.Nil)
		if err != nil {
			return nil, 0, false, err
		}
		credentialID, err := requiredGraphQLResourceReference(p, repository, "credential_id", store.ReferenceCredential, userID)
		if err != nil {
			return nil, 0, false, err
		}
		result, err := repository.RevokeUserControlCredential(p.Context, userID, credentialID, actor)
		return projectCredentialMutationResult(result), result.AuthorizationRevision, result.Changed, err
	})
}

type userMutationResolver func(graphql.ResolveParams, store.UserRepository, store.MutationActor) (interface{}, int64, bool, error)

func userMutationField(resultType graphql.Output, args graphql.FieldConfigArgument, action string, s store.Store, resolve userMutationResolver) *graphql.Field {
	return &graphql.Field{Type: resultType, Args: args, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql."+action)
		defer span.End()
		p.Context = ctx
		repository, err := userRepository(s)
		if err != nil {
			return nil, recordUserMutationError(span, err)
		}
		actor, err := userMutationActor(ctx)
		if err != nil {
			return nil, recordUserMutationError(span, err)
		}
		span.SetAttributes(attribute.String("user_action", action), attribute.String("actor_subject_id", actor.SubjectID.String()))
		payload, revision, changed, err := resolve(p, repository, actor)
		if err != nil {
			return nil, userGraphQLError(recordUserMutationError(span, err))
		}
		invalidateAuthorizationRevision(ctx, revision, changed, nil)
		span.SetAttributes(attribute.String("outcome", "success"), attribute.Bool("engine.access.changed", changed))
		return payload, nil
	}}
}

func userRepository(s store.Store) (store.UserRepository, error) {
	repository, ok := s.(store.UserRepository)
	if !ok {
		return nil, errors.New("user management is unavailable")
	}
	return repository, nil
}

func userMutationActor(ctx context.Context) (store.MutationActor, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return store.MutationActor{}, accesscontrol.ErrAuthenticationRequired
	}
	return store.MutationActor{
		SubjectID: actor.SubjectID, CredentialID: actor.CredentialID,
		RequestID: middleware.GetReqID(ctx), TraceID: userMutationTraceID(ctx),
	}, nil
}

func userMutationTraceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func recordUserMutationError(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetAttributes(attribute.String("outcome", "failure"))
	return err
}

func userListOptions(p graphql.ResolveParams, teamMembers bool) store.UserListOptions {
	statuses := []store.UserStatus{store.UserStatusInvited, store.UserStatusActive}
	includeSuspended, _ := p.Args["include_suspended"].(bool)
	if teamMembers || includeSuspended {
		statuses = append(statuses, store.UserStatusSuspended)
	}
	search, _ := p.Args["search"].(string)
	limit, _ := p.Args["limit"].(int)
	offset, _ := p.Args["offset"].(int)
	return store.UserListOptions{Statuses: statuses, Search: search, Limit: limit, Offset: offset}
}

func graphQLInputString(input map[string]interface{}, key string) string {
	value, _ := input[key].(string)
	return value
}

func graphQLArgString(p graphql.ResolveParams, key string) string {
	value, _ := p.Args[key].(string)
	return value
}

var userGraphQLErrorMessages = []struct {
	target  error
	message string
}{
	{store.ErrUserNotFound, "user not found"},
	{store.ErrUserEmailConflict, "user email already exists"},
	{store.ErrUserArchived, "archived user cannot be changed"},
	{store.ErrTeamNotFound, "team not found"},
	{store.ErrTeamArchived, "archived team cannot be changed"},
	{store.ErrLastEffectiveOwner, "at least one effective workspace Owner must remain"},
	{store.ErrOwnerManagementForbidden, "only a workspace Owner can change an Owner or Owner-team member"},
	{store.ErrControlCredentialNotFound, "credential not found"},
}

func userGraphQLError(err error) error {
	if errors.Is(err, store.ErrResourceReferenceNotFound) {
		return resourceReferenceGraphQLError(err)
	}
	if errors.Is(err, errEmptyUserPatch) {
		return errEmptyUserPatch
	}
	for _, mapping := range userGraphQLErrorMessages {
		if errors.Is(err, mapping.target) {
			return errors.New(mapping.message)
		}
	}
	if errors.Is(err, store.ErrInvalidUser) || errors.Is(err, store.ErrInvalidTeamMembership) || errors.Is(err, store.ErrInvalidControlCredential) {
		return errors.New("invalid user access request")
	}
	return errors.New("user access operation failed")
}

func projectUserMutationResult(result store.UserMutationResult) map[string]interface{} {
	return map[string]interface{}{
		"user": projectGraphQLUser(result.User), "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed,
	}
}

func projectMembershipMutationResult(result store.MembershipMutationResult) map[string]interface{} {
	var membership interface{}
	if validMembershipMutationResult(result) {
		membership = projectGraphQLTeamMember(result.User, result.Membership)
	}
	return map[string]interface{}{
		"membership": membership, "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed,
	}
}

func validMembershipMutationResult(result store.MembershipMutationResult) bool {
	validRole := result.Membership.Role == store.MembershipRoleMember || result.Membership.Role == store.MembershipRoleManager
	return result.Membership.TeamID != uuid.Nil && result.User.ID != uuid.Nil && validRole && !result.Membership.CreatedAt.IsZero()
}

func projectIssuedCredentialResult(result store.IssuedControlCredential) map[string]interface{} {
	return map[string]interface{}{
		"credential": projectGraphQLControlCredential(result.Credential), "secret": result.RawKey,
		"authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed,
	}
}

func projectCredentialMutationResult(result store.CredentialMutationResult) map[string]interface{} {
	var credential interface{}
	if result.Credential.ID != uuid.Nil {
		credential = projectGraphQLControlCredential(result.Credential)
	}
	return map[string]interface{}{
		"credential": credential, "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed,
	}
}

func projectGraphQLTeamMembers(members []store.TeamMember) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(members))
	for _, member := range members {
		items = append(items, projectGraphQLTeamMemberRow(member))
	}
	return items
}

func projectGraphQLTeamMemberRow(member store.TeamMember) map[string]interface{} {
	return map[string]interface{}{
		"user_id": member.UserID.String(), "email": member.Email, "display_name": member.DisplayName,
		"status": string(member.UserStatus), "membership_role": string(member.MembershipRole),
		"created_at": graphQLUserTime(member.CreatedAt),
	}
}

func projectGraphQLTeamMember(user store.User, membership store.TeamMembership) map[string]interface{} {
	return map[string]interface{}{
		"user_id": user.ID.String(), "email": user.Email, "display_name": user.DisplayName,
		"status": string(user.Status), "membership_role": string(membership.Role),
		"created_at": graphQLUserTime(membership.CreatedAt),
	}
}

func projectGraphQLEffectiveAccessGrants(grants []store.EffectiveAccessGrant) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(grants))
	for _, grant := range grants {
		items = append(items, map[string]interface{}{
			"permission": string(grant.Permission), "resource_type": string(grant.Resource.Type), "resource_id": grant.Resource.ID.String(),
			"role_slug": grant.RoleSlug, "source_type": grant.SourceType, "source_id": grant.SourceID.String(), "source_display_name": grant.SourceDisplayName,
		})
	}
	return items
}
