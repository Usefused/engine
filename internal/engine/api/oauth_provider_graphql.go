package api

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
)

// Admin CRUD for third-party OAuth clients: registration, listing, and
// revocation. The consent/token/revoke protocol itself is REST-only
// (oauth_provider_handlers.go) since OAuth2 mandates specific HTTP endpoints;
// this file is only the workspace Owner/Admin management surface, mirroring
// the Teams GraphQL admin pattern.

var oauthClientTypeGraphQLEnum = graphql.NewEnum(graphql.EnumConfig{
	Name: "OAuthClientType",
	Values: graphql.EnumValueConfigMap{
		"CONFIDENTIAL": &graphql.EnumValueConfig{Value: string(store.OAuthClientConfidential)},
		"PUBLIC":       &graphql.EnumValueConfig{Value: string(store.OAuthClientPublic)},
	},
})

var oauthClientGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "OAuthClient",
	Fields: graphql.Fields{
		"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"client_id": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		// Reuse the same enum as the input side so the output serializes back
		// to "CONFIDENTIAL"/"PUBLIC" instead of leaking the lowercase storage
		// value, matching the frontend's OAuthClientType union.
		"client_type":    &graphql.Field{Type: graphql.NewNonNull(oauthClientTypeGraphQLEnum)},
		"redirect_uris":  &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"allowed_scopes": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"has_secret":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"created_at":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"revoked_at":     &graphql.Field{Type: graphql.String},
	},
})

var oauthClientCreatedGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "OAuthClientCreatedPayload",
	Fields: graphql.Fields{
		"client": &graphql.Field{Type: graphql.NewNonNull(oauthClientGraphQLType)},
		// client_secret is populated once, only for a confidential client's
		// creation response; it is never persisted or returned again.
		"client_secret": &graphql.Field{Type: graphql.String},
	},
})

var createOAuthClientGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "CreateOAuthClientInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"name":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"client_type":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(oauthClientTypeGraphQLEnum)},
		"redirect_uris":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		"allowed_scopes": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
	},
})

func oauthClientsGraphQLField(service OAuthProviderService) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(oauthClientGraphQLType))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			clients, err := oauthProviderFromField(service).ListClients(p.Context)
			if err != nil {
				return nil, oauthGraphQLError(err)
			}
			return projectGraphQLOAuthClients(clients), nil
		},
	}
}

// oauthScopeGraphQLType describes one entry of the built-in OAuth scope
// catalog: the raw permission string a client registration stores, plus the
// human label shown in both the consent screen and the scope picker.
var oauthScopeGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "OAuthScope",
	Fields: graphql.Fields{
		"value": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"label": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

// oauthScopeCatalogGraphQLField exposes the same oauthScopeDescriptions map
// used to label the consent screen (oauth_provider_handlers.go) as a query,
// so the workspace UI's scope picker fetches its value/label pairs from the
// server instead of duplicating them as static frontend data that could drift
// out of sync when a new permission is added.
func oauthScopeCatalogGraphQLField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(oauthScopeGraphQLType))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return projectGraphQLOAuthScopeCatalog(), nil
		},
	}
}

// projectGraphQLOAuthScopeCatalog turns oauthScopeDescriptions into a stable,
// alphabetically sorted list (map iteration order is randomized in Go) so
// repeated requests return scopes in the same order.
func projectGraphQLOAuthScopeCatalog() []map[string]interface{} {
	values := make([]string, 0, len(oauthScopeDescriptions))
	for value := range oauthScopeDescriptions {
		values = append(values, value)
	}
	sort.Strings(values)
	catalog := make([]map[string]interface{}, 0, len(values))
	for _, value := range values {
		catalog = append(catalog, map[string]interface{}{"value": value, "label": oauthScopeDescriptions[value]})
	}
	return catalog
}

// oauthRegistrationKeyGraphQLType is the settings status projection of a
// per-user registration key; the raw value is only returned by the create
// mutation, never by a read.
var oauthRegistrationKeyGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "OAuthRegistrationKeyStatus",
	Fields: graphql.Fields{
		"exists": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var oauthRegistrationKeyCreatedGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "OAuthRegistrationKeyCreatedPayload",
	Fields: graphql.Fields{
		// The raw key is returned exactly once at mint/rotation, matching every
		// other one-time credential in Engine.
		"key": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

func oauthRegistrationKeyGraphQLField(service OAuthProviderService) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(oauthRegistrationKeyGraphQLType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, ok := accesscontrol.ActorFromContext(p.Context)
			if !ok {
				return nil, accesscontrol.ErrAuthenticationRequired
			}
			s := oauthProviderFromField(service)
			if s == nil {
				return nil, oauthGraphQLError(errors.New("OAuth provider management is unavailable"))
			}
			exists, err := s.HasRegistrationKey(p.Context, actor)
			if err != nil {
				return nil, oauthGraphQLError(err)
			}
			return map[string]interface{}{"exists": exists}, nil
		},
	}
}

func createOAuthRegistrationKeyGraphQLField(service OAuthProviderService) *graphql.Field {
	return oauthMutationField(oauthRegistrationKeyCreatedGraphQLType, graphql.FieldConfigArgument{}, "oauth_registration_key.create", service,
		func(p graphql.ResolveParams, s OAuthProviderService, actor accesscontrol.Actor) (interface{}, error) {
			rawKey, err := s.SetRegistrationKey(p.Context, actor)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"key": rawKey}, nil
		})
}

func revokeOAuthRegistrationKeyGraphQLField(service OAuthProviderService) *graphql.Field {
	return oauthMutationField(graphql.NewNonNull(graphql.Boolean), graphql.FieldConfigArgument{}, "oauth_registration_key.revoke", service,
		func(p graphql.ResolveParams, s OAuthProviderService, actor accesscontrol.Actor) (interface{}, error) {
			if err := s.RevokeRegistrationKey(p.Context, actor); err != nil {
				return nil, err
			}
			return true, nil
		})
}

func createOAuthClientGraphQLField(service OAuthProviderService) *graphql.Field {
	return oauthMutationField(oauthClientCreatedGraphQLType, graphql.FieldConfigArgument{
		"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createOAuthClientGraphQLInput)},
	}, "oauth_client.create", service, func(p graphql.ResolveParams, s OAuthProviderService, actor accesscontrol.Actor) (interface{}, error) {
		input, _ := p.Args["input"].(map[string]interface{})
		name, _ := input["name"].(string)
		clientType, _ := input["client_type"].(string)
		redirectURIs := graphQLStringList(input["redirect_uris"])
		allowedScopes := graphQLStringList(input["allowed_scopes"])
		result, err := s.CreateClient(p.Context, actor, oauthprovider.CreateClientInput{
			Name: name, ClientType: store.OAuthClientType(clientType),
			RedirectURIs: redirectURIs, AllowedScopes: allowedScopes,
		})
		if err != nil {
			return nil, err
		}
		return projectGraphQLOAuthClientCreated(result), nil
	})
}

func revokeOAuthClientGraphQLField(service OAuthProviderService) *graphql.Field {
	return oauthMutationField(graphql.NewNonNull(graphql.Boolean), graphql.FieldConfigArgument{
		"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
	}, "oauth_client.revoke", service, func(p graphql.ResolveParams, s OAuthProviderService, actor accesscontrol.Actor) (interface{}, error) {
		id, err := uuid.Parse(graphQLStringArg(p, "id"))
		if err != nil {
			return nil, errors.New("invalid OAuth client id")
		}
		if err := s.RevokeClient(p.Context, actor, id); err != nil {
			return nil, err
		}
		return true, nil
	})
}

type oauthMutationResolver func(graphql.ResolveParams, OAuthProviderService, accesscontrol.Actor) (interface{}, error)

// oauthMutationField mirrors teamMutationField's span/actor/error-safety
// wrapper without the Teams-specific SSO entitlement gate, which does not
// apply to OAuth client management.
func oauthMutationField(resultType graphql.Output, args graphql.FieldConfigArgument, action string, service OAuthProviderService, resolve oauthMutationResolver) *graphql.Field {
	return &graphql.Field{Type: resultType, Args: args, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
		ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql."+action)
		defer span.End()
		p.Context = ctx
		s := oauthProviderFromField(service)
		if s == nil {
			return nil, recordOAuthMutationError(span, errors.New("OAuth provider management is unavailable"))
		}
		actor, err := oauthMutationActor(ctx)
		if err != nil {
			return nil, recordOAuthMutationError(span, err)
		}
		span.SetAttributes(attribute.String("actor_subject_id", actor.SubjectID.String()))
		result, err := resolve(p, s, actor)
		if err != nil {
			recordOAuthMutationError(span, err)
			return nil, oauthGraphQLError(err)
		}
		span.SetAttributes(attribute.String("outcome", "success"))
		return result, nil
	}}
}

func oauthProviderFromField(service OAuthProviderService) OAuthProviderService {
	return service
}

func oauthMutationActor(ctx context.Context) (accesscontrol.Actor, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok {
		return accesscontrol.Actor{}, accesscontrol.ErrAuthenticationRequired
	}
	return actor, nil
}

func recordOAuthMutationError(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetAttributes(attribute.String("outcome", "failure"))
	return err
}

func oauthGraphQLError(err error) error {
	switch {
	case errors.Is(err, store.ErrOAuthClientNotFound):
		return errors.New("OAuth client not found")
	case errors.Is(err, store.ErrInvalidOAuthClient):
		return errors.New("invalid OAuth client request")
	case errors.Is(err, accesscontrol.ErrAuthenticationRequired):
		return err
	default:
		return errors.New("OAuth client operation failed")
	}
}

func graphQLStringList(value interface{}) []string {
	raw, _ := value.([]interface{})
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

func projectGraphQLOAuthClients(clients []store.OAuthClient) []map[string]interface{} {
	projected := make([]map[string]interface{}, 0, len(clients))
	for _, client := range clients {
		projected = append(projected, projectGraphQLOAuthClient(client))
	}
	return projected
}

func projectGraphQLOAuthClient(client store.OAuthClient) map[string]interface{} {
	var revokedAt interface{}
	if client.RevokedAt != nil {
		revokedAt = client.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]interface{}{
		"id": client.ID.String(), "name": client.Name, "client_id": client.ClientID,
		"client_type": string(client.ClientType), "redirect_uris": client.RedirectURIs,
		"allowed_scopes": client.AllowedScopes, "has_secret": client.HasSecret,
		"created_at": client.CreatedAt.UTC().Format(time.RFC3339Nano), "revoked_at": revokedAt,
	}
}

func projectGraphQLOAuthClientCreated(result oauthprovider.CreateClientResult) map[string]interface{} {
	payload := map[string]interface{}{"client": projectGraphQLOAuthClient(result.Client)}
	if result.ClientSecret != "" {
		payload["client_secret"] = result.ClientSecret
	}
	return payload
}
