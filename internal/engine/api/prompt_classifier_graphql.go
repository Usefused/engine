package api

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

type promptOperationClassifier interface {
	ClassifyServiceOperation(context.Context, uuid.UUID, string, string) (string, error)
}

// classifyPromptOperationGraphQLField keeps prompt discovery behind Engine catalogue authorization and the existing licensed Registry client.
func classifyPromptOperationGraphQLField(registry sandbox.RegistryClient) *graphql.Field {
	return &graphql.Field{
		Type: graphql.String,
		Args: graphql.FieldConfigArgument{
			"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"version":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"query":      &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		// Resolve accepts version identity and intent, never caller-authored candidate catalogues or provider keys.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
			// Invalid identities must fail before any Registry request.
			if err != nil {
				return nil, err
			}
			classifier, ok := registry.(promptOperationClassifier)
			// Older or unconfigured clients fail explicitly instead of silently using lexical selection.
			if !ok {
				return nil, errors.New("Jev operation discovery is unavailable")
			}
			return classifier.ClassifyServiceOperation(p.Context, serviceID, p.Args["version"].(string), p.Args["query"].(string))
		},
	}
}
