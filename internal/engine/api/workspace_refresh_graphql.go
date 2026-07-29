package api

import (
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/store"
)

var refreshServiceContractResultType = graphql.NewObject(graphql.ObjectConfig{
	Name: "RefreshServiceContractResult",
	Fields: graphql.Fields{
		"service_id":         &graphql.Field{Type: graphql.String},
		"service_version_id": &graphql.Field{Type: graphql.String},
		"version":            &graphql.Field{Type: graphql.String},
		"contract_hash":      &graphql.Field{Type: graphql.String},
		"error":              &graphql.Field{Type: graphql.String},
	},
})

var refreshMissingServiceContractsType = graphql.NewObject(graphql.ObjectConfig{
	Name: "RefreshMissingServiceContractsPayload",
	Fields: graphql.Fields{
		"status":    &graphql.Field{Type: graphql.String},
		"missing":   &graphql.Field{Type: graphql.Int},
		"refreshed": &graphql.Field{Type: graphql.Int},
		"failed":    &graphql.Field{Type: graphql.Int},
		"results":   &graphql.Field{Type: graphql.NewList(refreshServiceContractResultType)},
	},
})

func refreshMissingServiceContractsGraphQLField(s store.Store, batchFetcher BatchRuntimeContractFetcher) *graphql.Field {
	return &graphql.Field{
		Type: refreshMissingServiceContractsType,
		Args: graphql.FieldConfigArgument{
			"limit": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 100},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			result, err := refreshMissingServiceContracts(p.Context, s, batchFetcher, refreshMissingContractsCall{
				accountID: actor.accountID,
				apiKey:    refreshGraphQLAPIKey(p),
				limit:     refreshGraphQLLimit(p.Args["limit"]),
			})
			if err != nil {
				return nil, err
			}
			return refreshMissingContractsGraphQLPayload(result), nil
		},
	}
}

func refreshGraphQLAPIKey(p graphql.ResolveParams) string {
	request := requestFromContext(p.Context)
	if request == nil {
		return ""
	}
	return request.Header.Get("X-API-Key")
}

func refreshGraphQLLimit(raw interface{}) int {
	limit, _ := raw.(int)
	// Why clamp here as well as in CLI: GraphQL is the server boundary, so the
	// Engine must keep rollout/backfill work bounded even for hand-written calls.
	if limit <= 0 || limit > 100 {
		return 100
	}
	return limit
}

func refreshMissingContractsGraphQLPayload(response *refreshMissingContractsResponse) map[string]interface{} {
	results := make([]map[string]interface{}, 0, len(response.Results))
	for _, result := range response.Results {
		results = append(results, map[string]interface{}{
			"service_id":         result.ServiceID,
			"service_version_id": result.ServiceVersionID,
			"version":            result.Version,
			"contract_hash":      result.ContractHash,
			"error":              result.Error,
		})
	}
	return map[string]interface{}{
		"status":    response.Status,
		"missing":   response.Missing,
		"refreshed": response.Refreshed,
		"failed":    response.Failed,
		"results":   results,
	}
}
