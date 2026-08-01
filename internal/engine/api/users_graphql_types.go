package api

import (
	"time"

	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/store"
)

var userStatusGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "UserStatus",
	Values: graphql.EnumValueConfigMap{
		"INVITED":   &graphql.EnumValueConfig{Value: "invited"},
		"ACTIVE":    &graphql.EnumValueConfig{Value: "active"},
		"SUSPENDED": &graphql.EnumValueConfig{Value: "suspended"},
		"ARCHIVED":  &graphql.EnumValueConfig{Value: "archived"},
	},
})

var teamMembershipRoleGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "TeamMembershipRole",
	Values: graphql.EnumValueConfigMap{
		"MEMBER":  &graphql.EnumValueConfig{Value: "member"},
		"MANAGER": &graphql.EnumValueConfig{Value: "manager"},
	},
})

var controlCredentialGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ControlCredential",
	Fields: graphql.Fields{
		"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"key_prefix":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"expires_at":   &graphql.Field{Type: graphql.String},
		"last_used_at": &graphql.Field{Type: graphql.String},
		"revoked_at":   &graphql.Field{Type: graphql.String},
		"created_at":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var userTeamMembershipGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "UserTeamMembership",
	Fields: graphql.Fields{
		"team_id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"team_name":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"team_slug":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"membership_role": &graphql.Field{Type: graphql.NewNonNull(teamMembershipRoleGraphQLEnum)},
		"created_at":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var userGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "User",
	Fields: graphql.Fields{
		"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"email":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"display_name":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"status":                &graphql.Field{Type: graphql.NewNonNull(userStatusGraphQLEnum)},
		"owner_protected":       &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"memberships":           &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(userTeamMembershipGraphQLType)))},
		"memberships_truncated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"credentials":           &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(controlCredentialGraphQLType)))},
		"credentials_truncated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"created_at":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"updated_at":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var userPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "UserPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(userGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var teamMemberGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamMember",
	Fields: graphql.Fields{
		"user_id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"email":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"display_name":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"status":          &graphql.Field{Type: graphql.NewNonNull(userStatusGraphQLEnum)},
		"membership_role": &graphql.Field{Type: graphql.NewNonNull(teamMembershipRoleGraphQLEnum)},
		"created_at":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var teamMemberPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamMemberPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(teamMemberGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var effectiveAccessGrantGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EffectiveAccessGrant",
	Fields: graphql.Fields{
		"permission":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_type":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"role_slug":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"source_type":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"source_id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"source_display_name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var userEffectiveAccessGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "UserEffectiveAccess",
	Fields: graphql.Fields{
		"user_id":                &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"grants":                 &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(effectiveAccessGrantGraphQLType)))},
	},
})

var userMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "UserMutationPayload",
	Fields: graphql.Fields{
		"user":                   &graphql.Field{Type: graphql.NewNonNull(userGraphQLType)},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var teamMembershipMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "TeamMembershipMutationPayload",
	Fields: graphql.Fields{
		"membership":             &graphql.Field{Type: teamMemberGraphQLType},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var issuedCredentialPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "IssuedCredentialPayload",
	Fields: graphql.Fields{
		"credential": &graphql.Field{Type: graphql.NewNonNull(controlCredentialGraphQLType)},
		// Secret exists only on this mutation payload and is never part of User
		// or credential metadata, so later reads cannot recover the raw key.
		"secret":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var credentialMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "CredentialMutationPayload",
	Fields: graphql.Fields{
		"credential":             &graphql.Field{Type: controlCredentialGraphQLType},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var createUserGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "CreateUserInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"email":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"display_name": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
	},
})

var updateUserGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "UpdateUserInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"email":        &graphql.InputObjectFieldConfig{Type: graphql.String},
		"display_name": &graphql.InputObjectFieldConfig{Type: graphql.String},
	},
})

func projectGraphQLUser(user store.User) map[string]interface{} {
	memberships := make([]map[string]interface{}, 0, len(user.Memberships))
	for _, membership := range user.Memberships {
		memberships = append(memberships, projectGraphQLUserMembership(membership))
	}
	credentials := make([]map[string]interface{}, 0, len(user.Credentials))
	for _, credential := range user.Credentials {
		credentials = append(credentials, projectGraphQLControlCredential(credential))
	}
	return map[string]interface{}{
		"id": user.ID.String(), "email": user.Email, "display_name": user.DisplayName,
		"status": string(user.Status), "owner_protected": user.OwnerProtected, "memberships": memberships, "memberships_truncated": user.MembershipsTruncated,
		"credentials": credentials, "credentials_truncated": user.CredentialsTruncated,
		"created_at": graphQLUserTime(user.CreatedAt), "updated_at": graphQLUserTime(user.UpdatedAt),
	}
}

func projectGraphQLUsers(users []store.User) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(users))
	for _, user := range users {
		items = append(items, projectGraphQLUser(user))
	}
	return items
}

func projectGraphQLUserMembership(membership store.TeamMembership) map[string]interface{} {
	return map[string]interface{}{
		"team_id": membership.TeamID.String(), "team_name": membership.TeamName,
		"team_slug": membership.TeamSlug, "membership_role": string(membership.Role),
		"created_at": graphQLUserTime(membership.CreatedAt),
	}
}

func projectGraphQLControlCredential(credential store.ControlCredential) map[string]interface{} {
	return map[string]interface{}{
		"id": credential.ID.String(), "name": credential.Name, "key_prefix": credential.KeyPrefix,
		"expires_at": graphQLOptionalUserTime(credential.ExpiresAt), "last_used_at": graphQLOptionalUserTime(credential.LastUsedAt),
		"revoked_at": graphQLOptionalUserTime(credential.RevokedAt), "created_at": graphQLUserTime(credential.CreatedAt),
	}
}

func graphQLUserTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func graphQLOptionalUserTime(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return graphQLUserTime(*value)
}
