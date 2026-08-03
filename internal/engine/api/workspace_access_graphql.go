package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

var workspaceShareGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceShare",
	Fields: graphql.Fields{
		"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"role_slug":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"role_display_name":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_type":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_id":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"resource_display_name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"created_at":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var workspaceSharePageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceSharePage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(workspaceShareGraphQLType)))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var workspaceShareMutationPayloadGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceShareMutationPayload",
	Fields: graphql.Fields{
		"share":                  &graphql.Field{Type: workspaceShareGraphQLType},
		"authorization_revision": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"changed":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var workspaceShareResourceGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "WorkspaceShareResource",
	Values: graphql.EnumValueConfigMap{
		"BUCKET":   &graphql.EnumValueConfig{Value: accesscontrol.ResourceBucket},
		"ARTIFACT": &graphql.EnumValueConfig{Value: accesscontrol.ResourceArtifact},
	},
})

func workspaceSharesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceSharePageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"resource_type": &graphql.ArgumentConfig{Type: workspaceShareResourceGraphQLEnum},
			"limit":         &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset":        &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repository, err := workspaceAccessRepository(s)
			if err != nil {
				return nil, err
			}
			options := store.WorkspaceShareListOptions{Limit: p.Args["limit"].(int), Offset: p.Args["offset"].(int)}
			if resourceType, ok := p.Args["resource_type"].(accesscontrol.ResourceType); ok {
				options.ResourceType = &resourceType
			}
			shares, total, err := repository.ListWorkspaceShares(p.Context, options)
			if err != nil {
				return nil, workspaceShareGraphQLError(err)
			}
			return map[string]interface{}{"items": projectWorkspaceShares(shares), "total": total}, nil
		},
	}
}

func grantWorkspaceBucketAccessGraphQLField(s store.Store) *graphql.Field {
	return workspaceShareMutationGraphQLField(s, "workspace.bucket_access.grant", accesscontrol.ResourceBucket, "bucket_id", true)
}

func revokeWorkspaceBucketAccessGraphQLField(s store.Store) *graphql.Field {
	return workspaceShareMutationGraphQLField(s, "workspace.bucket_access.revoke", accesscontrol.ResourceBucket, "bucket_id", false)
}

func grantWorkspaceArtifactAccessGraphQLField(s store.Store) *graphql.Field {
	return workspaceShareMutationGraphQLField(s, "workspace.artifact_access.grant", accesscontrol.ResourceArtifact, "artifact_id", true)
}

func revokeWorkspaceArtifactAccessGraphQLField(s store.Store) *graphql.Field {
	return workspaceShareMutationGraphQLField(s, "workspace.artifact_access.revoke", accesscontrol.ResourceArtifact, "artifact_id", false)
}

func workspaceShareMutationGraphQLField(s store.Store, action string, resourceType accesscontrol.ResourceType, argument string, grant bool) *graphql.Field {
	return &graphql.Field{
		Type: workspaceShareMutationPayloadGraphQLType,
		Args: graphql.FieldConfigArgument{argument: &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql."+action)
			defer span.End()
			kind, err := referenceKindForResource(resourceType)
			if err != nil {
				return nil, err
			}
			resourceID, err := requiredGraphQLResourceReference(p, s, argument, kind, uuid.Nil)
			if err != nil {
				return nil, err
			}
			repository, err := workspaceAccessRepository(s)
			if err != nil {
				return nil, err
			}
			actor, err := teamMutationActor(ctx)
			if err != nil {
				return nil, workspaceShareGraphQLError(err)
			}
			span.SetAttributes(attribute.String("user_action", action), attribute.String("actor_subject_id", actor.SubjectID.String()))
			mutation := store.WorkspaceShareMutation{Resource: accesscontrol.ResourceRef{Type: resourceType, ID: resourceID}, Actor: actor}
			result, err := mutateWorkspaceShare(ctx, repository, mutation, grant)
			invalidateAuthorizationRevision(ctx, result.AuthorizationRevision, result.Changed, err)
			if err != nil {
				return nil, workspaceShareGraphQLError(err)
			}
			span.SetAttributes(attribute.String("outcome", "success"))
			return projectWorkspaceShareMutationResult(result), nil
		},
	}
}

func workspaceAccessRepository(s store.Store) (store.WorkspaceAccessRepository, error) {
	repository, ok := s.(store.WorkspaceAccessRepository)
	if !ok {
		return nil, errors.New("workspace access management is unavailable")
	}
	return repository, nil
}

func mutateWorkspaceShare(ctx context.Context, repository store.WorkspaceAccessRepository, input store.WorkspaceShareMutation, grant bool) (store.WorkspaceShareMutationResult, error) {
	if grant {
		return repository.GrantWorkspaceShare(ctx, input)
	}
	return repository.RevokeWorkspaceShare(ctx, input)
}

func projectWorkspaceShares(shares []store.WorkspaceShare) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(shares))
	for _, share := range shares {
		items = append(items, projectWorkspaceShare(share))
	}
	return items
}

func projectWorkspaceShare(share store.WorkspaceShare) map[string]interface{} {
	return map[string]interface{}{
		"id": share.ID.String(), "role_slug": share.RoleSlug, "role_display_name": share.RoleDisplayName,
		"resource_type": string(share.Resource.Type), "resource_id": share.Resource.ID.String(),
		"resource_display_name": share.ResourceDisplayName, "created_at": share.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func projectWorkspaceShareMutationResult(result store.WorkspaceShareMutationResult) map[string]interface{} {
	var share interface{}
	if result.Share.ID != uuid.Nil {
		share = projectWorkspaceShare(result.Share)
	}
	return map[string]interface{}{"share": share, "authorization_revision": int(result.AuthorizationRevision), "changed": result.Changed}
}

func workspaceShareGraphQLError(err error) error {
	if errors.Is(err, store.ErrResourceReferenceNotFound) {
		return resourceReferenceGraphQLError(err)
	}
	if errors.Is(err, store.ErrInvalidWorkspaceShare) {
		return errors.New("invalid workspace share request")
	}
	return errors.New("workspace share operation failed")
}
