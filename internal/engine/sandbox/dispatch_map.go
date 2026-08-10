package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// fusedToService projects the connection-cached Fused object onto the lean
// models.Service the dispatcher needs to build and authenticate a vendor call.
// Only execution-relevant fields are carried; authoring/registry fields are
// intentionally dropped.
func fusedToService(o *fusedobject.ServiceMetadata) *models.Service {
	return &models.Service{
		ID:               o.ID,
		ServiceVersionID: o.ServiceVersionID,
		Name:             o.Name,
		BaseURL:          o.BaseURL,
		AuthConfigs:      mapAuthConfigs(o.AuthConfigs),
		DefaultHeaders:   models.DefaultHeaders(o.DefaultHeaders),
		RawWSDL:          o.RawWSDL,
		RateLimit:        o.RateLimit,
		RetryConfig:      mapRetryConfig(o.RetryConfig),
	}
}

// mapRetryConfig converts the wire retry config to the models type
// dispatcher.go's resolveRetryPolicy reads. Returns nil when unset.
func mapRetryConfig(r *fusedobject.RetryConfig) *models.RetryConfig {
	if r == nil {
		return nil
	}
	return &models.RetryConfig{
		Strategy:   r.Strategy,
		MaxRetries: r.MaxRetries,
		BackoffMs:  r.BackoffMs,
	}
}

// fusedToIntegrationObject projects a single Fused endpoint onto the
// models.IntegrationObject the dispatcher executes against, including pagination
// config so the dispatcher's executePaginated path triggers for paginated
// endpoints.
//
// Pagination falls back to the service-level execution_policy value
// (o.Pagination) when the endpoint has none of its own -- most providers
// paginate identically across every endpoint, so per-endpoint spec-derived
// pagination (when present) always wins, but a service/version owner can
// declare one default instead of annotating every operation individually
// (see plans/plan-service-config-restructure.md item 1).
func fusedToIntegrationObject(o *fusedobject.ServiceMetadata, ep fusedobject.Endpoint) *models.IntegrationObject {
	return &models.IntegrationObject{
		ID:                   ep.ID,
		StableKey:            ep.StableKey,
		ServiceID:            o.ID,
		Name:                 ep.Name,
		Description:          ep.Description,
		ResourceName:         ep.ResourceName,
		Version:              ep.Version,
		Method:               ep.Method,
		Path:                 ep.Path,
		NormalizedPath:       ep.NormalizedPath,
		Deprecated:           ep.Deprecated,
		IsSSE:                ep.IsSSE,
		Parameters:           mapParameters(ep.Parameters),
		RequestContent:       mapRequestContent(ep.RequestContent),
		Responses:            mapResponses(ep.Responses),
		GraphQLQuery:         ep.GraphQLQuery,
		ProviderProtocol:     effectiveProviderProtocol(ep),
		OperationKind:        effectiveOperationKind(ep),
		Pagination:           resolvePagination(ep.Pagination, o.Pagination),
		SecurityRequirements: ep.SecurityRequirements,
	}
}

func mapRequestContent(content *fusedobject.RequestContent) *models.RequestContent {
	if content == nil {
		return nil
	}
	return &models.RequestContent{
		MediaType: content.MediaType, Serialization: content.Serialization,
		Required: content.Required, Schema: mapSchema(content.Schema),
		PayloadParameter: content.PayloadParameter, BinaryEncoding: content.BinaryEncoding,
		Parts: mapRequestParts(content.Parts),
	}
}

func mapRequestParts(parts map[string]fusedobject.RequestPart) map[string]models.RequestPart {
	if parts == nil {
		return nil
	}
	mapped := make(map[string]models.RequestPart, len(parts))
	for name, part := range parts {
		mapped[name] = models.RequestPart{ContentType: part.ContentType, BinaryEncoding: part.BinaryEncoding}
	}
	return mapped
}

// effectiveProviderProtocol keeps snapshots written before the explicit field
// executable. The query document is authoritative enough for this one-way
// migration, while new snapshots always carry the provider protocol directly.
func effectiveProviderProtocol(ep fusedobject.Endpoint) string {
	if ep.ProviderProtocol != "" {
		return ep.ProviderProtocol
	}
	if ep.GraphQLQuery != nil {
		return models.ProviderProtocolGraphQL
	}
	return models.ProviderProtocolREST
}

// effectiveOperationKind uses the legacy resource bucket only for old GraphQL
// snapshots; new contracts persist the semantic kind explicitly.
func effectiveOperationKind(ep fusedobject.Endpoint) string {
	if ep.OperationKind != "" {
		return ep.OperationKind
	}
	if ep.ProviderProtocol == "" && ep.GraphQLQuery == nil {
		return ""
	}
	switch ep.ResourceName {
	case models.OperationKindQuery, models.OperationKindMutation:
		return ep.ResourceName
	default:
		return ""
	}
}

func mapParameters(parameters fusedobject.Parameters) models.Parameters {
	if len(parameters) == 0 {
		return nil
	}
	mapped := make(models.Parameters, len(parameters))
	for i, parameter := range parameters {
		mapped[i] = models.Parameter{
			Name: parameter.Name, In: parameter.In, Required: parameter.Required,
			Type: parameter.Type, Description: parameter.Description, PathEncoding: parameter.PathEncoding,
		}
	}
	return mapped
}

func mapSchema(schema *fusedobject.Schema) *models.Schema {
	if schema == nil {
		return nil
	}
	mapped := models.Schema{Ref: schema.Ref, Type: schema.Type, Format: schema.Format, Required: schema.Required, Example: schema.Example}
	if schema.Items != nil {
		mapped.Items = mapSchema(schema.Items)
	}
	if schema.AdditionalProperties != nil {
		mapped.AdditionalProperties = mapSchema(schema.AdditionalProperties)
	}
	if len(schema.Properties) > 0 {
		mapped.Properties = make(map[string]models.Schema, len(schema.Properties))
		for name, property := range schema.Properties {
			mapped.Properties[name] = *mapSchema(&property)
		}
	}
	return &mapped
}

func mapResponses(responses fusedobject.Responses) models.Responses {
	if len(responses) == 0 {
		return nil
	}
	mapped := make(models.Responses, len(responses))
	for status, response := range responses {
		mapped[status] = *mapSchema(&response)
	}
	return mapped
}

// resolvePagination applies the endpoint-wins-over-service fallback rule
// described on fusedToIntegrationObject, then maps whichever value won to
// the models type the dispatcher reads.
func resolvePagination(endpoint, service *fusedobject.PaginationConfig) *models.PaginationConfig {
	if endpoint != nil {
		return mapPagination(endpoint)
	}
	return mapPagination(service)
}

// mapPagination converts the wire pagination config back to the models type the
// dispatcher reads. Returns nil for non-paginated endpoints.
func mapPagination(p *fusedobject.PaginationConfig) *models.PaginationConfig {
	if p == nil {
		return nil
	}
	// The shared versioned contract is immutable after cache decode; copying
	// the wrapper prevents endpoint mapping from aliasing the cached pointer.
	mapped := models.PaginationConfig(*p)
	return &mapped
}

// mapAuthConfigs converts the Fused-object auth configs to the models auth
// configs the dispatcher's applyAuth understands. Field names are 1:1; only the
// fields applyAuth reads (Type, Scheme, Location, KeyName) plus common OAuth
// metadata are carried.
func mapAuthConfigs(in fusedobject.AuthConfigs) models.AuthConfigs {
	if len(in) == 0 {
		return nil
	}
	out := make(models.AuthConfigs, 0, len(in))
	for _, a := range in {
		out = append(out, models.AuthConfig{
			Name:              authCredentialName(a),
			Type:              a.Type,
			Flow:              a.Flow,
			Scheme:            a.Scheme,
			BasicPasswordMode: a.BasicPasswordMode,
			Location:          a.Location,
			KeyName:           a.KeyName,
			TokenURL:          a.TokenURL,
		})
	}
	return out
}
