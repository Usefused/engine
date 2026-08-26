package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
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
		ServiceBaseURL:   o.BaseURL,
		ServerVariables:  cloneStringMap(o.ServerVariables),
		Servers:          mapServers(o.Servers),
		AuthConfigs:      mapAuthConfigs(o.AuthConfigs),
		DefaultHeaders:   models.DefaultHeaders(o.DefaultHeaders),
		RawWSDL:          o.RawWSDL,
		RateLimit:        o.RateLimit,
		RetryConfig:      mapRetryConfig(o.RetryConfig),
		Documentation:    mapServiceDocumentation(o.Documentation),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// mapRetryConfig converts the wire retry config to the models type
// dispatcher.go's resolveRetryPolicy reads. Returns nil when unset.
func mapRetryConfig(r *fusedobject.RetryConfig) *models.RetryConfig {
	if r == nil {
		return nil
	}
	mapped := *r
	mapped.Rules = append([]retrypolicy.Rule(nil), r.Rules...)
	return &mapped
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
	requestContent := mapRequestContent(ep.RequestContent)
	attachRequestDefinitionIndex(requestContent, o)
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
		OperationServers:     mapServers(ep.OperationServers),
		NormalizedPath:       ep.NormalizedPath,
		Deprecated:           ep.Deprecated,
		Parameters:           mapParameters(ep.Parameters),
		RequestContent:       requestContent,
		Responses:            mapResponses(ep.Responses),
		GraphQLQuery:         ep.GraphQLQuery,
		ProviderProtocol:     ep.ProviderProtocol,
		OperationKind:        ep.OperationKind,
		Pagination:           resolvePagination(ep.Pagination, o.Pagination),
		SecurityRequirements: ep.SecurityRequirements,
		Documentation:        mapOperationDocumentation(ep.Documentation),
	}
}

func mapRequestContent(content *fusedobject.RequestContent) *models.RequestContent {
	if content == nil {
		return nil
	}
	return &models.RequestContent{
		Required: content.Required, PayloadParameter: content.PayloadParameter,
		Representations: mapRequestRepresentations(content.Representations), DefaultMediaType: content.DefaultMediaType,
		UploadWorkflow: content.UploadWorkflow,
	}
}

func mapRequestRepresentations(values []fusedobject.RequestRepresentation) []models.RequestRepresentation {
	if values == nil {
		return nil
	}
	mapped := make([]models.RequestRepresentation, len(values))
	for i, value := range values {
		mapped[i] = models.RequestRepresentation{
			MediaType: value.MediaType, Serialization: value.Serialization,
			Schema: mapSchemaContract(value.Schema), ItemSchema: mapSchemaContract(value.ItemSchema),
			Encoding: mapRequestEncodings(value.Encoding), PrefixEncoding: mapRequestEncodingSlice(value.PrefixEncoding),
			ItemEncoding: mapOptionalRequestEncoding(value.ItemEncoding),
			Example:      value.Example, Examples: value.Examples,
		}
	}
	return mapped
}

func mapRequestEncodings(values map[string]fusedobject.RequestEncoding) map[string]models.RequestEncoding {
	if len(values) == 0 {
		return nil
	}
	mapped := make(map[string]models.RequestEncoding, len(values))
	for name, value := range values {
		mapped[name] = mapRequestEncoding(value)
	}
	return mapped
}

func mapRequestEncoding(value fusedobject.RequestEncoding) models.RequestEncoding {
	return models.RequestEncoding{
		ContentType: value.ContentType, Headers: mapHeaderContracts(value.Headers), Style: value.Style,
		Explode: value.Explode, AllowReserved: value.AllowReserved, BinaryEncoding: value.BinaryEncoding,
		Encoding: mapRequestEncodings(value.Encoding), PrefixEncoding: mapRequestEncodingSlice(value.PrefixEncoding),
		ItemEncoding: mapOptionalRequestEncoding(value.ItemEncoding),
	}
}

func mapRequestEncodingSlice(values []fusedobject.RequestEncoding) []models.RequestEncoding {
	if values == nil {
		return nil
	}
	mapped := make([]models.RequestEncoding, len(values))
	for index, value := range values {
		mapped[index] = mapRequestEncoding(value)
	}
	return mapped
}

func mapOptionalRequestEncoding(value *fusedobject.RequestEncoding) *models.RequestEncoding {
	if value == nil {
		return nil
	}
	mapped := mapRequestEncoding(*value)
	return &mapped
}

// effectiveProviderProtocol is shared with execution auditing so the receipt
// records the exact reviewed contract instead of deriving protocol semantics
// from unrelated fields.
func effectiveProviderProtocol(ep fusedobject.Endpoint) string {
	return ep.ProviderProtocol
}

func mapParameters(parameters fusedobject.Parameters) models.Parameters {
	if len(parameters) == 0 {
		return nil
	}
	mapped := make(models.Parameters, len(parameters))
	for i, parameter := range parameters {
		mapped[i] = models.Parameter{
			Name: parameter.Name, In: parameter.In, Required: parameter.Required,
			Description: parameter.Description, PathEncoding: parameter.PathEncoding,
			Serialization: models.ParameterSerialization{
				Style: parameter.Serialization.Style, Explode: parameter.Serialization.Explode,
				AllowReserved: parameter.Serialization.AllowReserved, AllowEmptyValue: parameter.Serialization.AllowEmptyValue,
			},
			Schema: mapSchemaContract(parameter.Schema), Content: mapParameterContent(parameter.Content),
			Deprecated: parameter.Deprecated, Example: parameter.Example, Examples: parameter.Examples,
		}
		if parameter.Schema == nil && len(parameter.Content) == 0 {
			// Direct Type is the baseline parameter contract. Once a canonical
			// schema/content contract exists it is authoritative, so carrying Type
			// as a second shape would let pagination and validation disagree.
			mapped[i].Type = parameter.Type
		}
	}
	return mapped
}

func mapServers(servers fusedobject.Servers) models.Servers {
	if len(servers) == 0 {
		return nil
	}
	mapped := make(models.Servers, len(servers))
	for i, server := range servers {
		mapped[i] = models.Server{
			URL: server.URL, Description: server.Description, Name: server.Name, Environment: server.Environment,
			IsDefault: server.IsDefault, Variables: server.Variables,
		}
	}
	return mapped
}

func mapParameterContent(content map[string]fusedobject.ParameterContent) map[string]models.ParameterContent {
	if len(content) == 0 {
		return nil
	}
	mapped := make(map[string]models.ParameterContent, len(content))
	for mediaType, value := range content {
		mapped[mediaType] = models.ParameterContent{
			Schema: mapSchemaContract(value.Schema), ItemSchema: mapSchemaContract(value.ItemSchema),
			Encoding: mapRequestEncodings(value.Encoding), PrefixEncoding: mapRequestEncodingSlice(value.PrefixEncoding), ItemEncoding: mapOptionalRequestEncoding(value.ItemEncoding),
			Example: value.Example, Examples: value.Examples,
		}
	}
	return mapped
}

// mapSchemaContract copies the compact root while sharing immutable dictionary identity across operations.
func mapSchemaContract(contract *fusedobject.SchemaContract) *models.SchemaContract {
	// Absent schemas remain absent rather than acquiring a permissive default.
	if contract == nil {
		return nil
	}
	return &models.SchemaContract{
		Dialect: contract.Dialect, Raw: append([]byte(nil), contract.Raw...), ContentHash: contract.ContentHash,
		SharedDefinitions: contract.SharedDefinitions, DefinitionIndex: contract.DefinitionIndex,
		Projection: *mapSchema(&contract.Projection), ProjectionDiagnostics: mapProjectionDiagnostics(contract.ProjectionDiagnostics),
	}
}

// attachRequestDefinitionIndex binds runtime-only body lookup to metadata already loaded for the exact version.
func attachRequestDefinitionIndex(content *models.RequestContent, metadata *fusedobject.ServiceMetadata) {
	// Body-less operations need no schema lookup and must not trigger metadata I/O.
	if content == nil {
		return
	}
	for position := range content.Representations {
		representation := &content.Representations[position]
		// Both ordinary bodies and sequential items may reference shared definitions.
		if representation.Schema != nil {
			representation.Schema.DefinitionIndex = metadata.DefinitionIndex
		}
		if representation.ItemSchema != nil {
			representation.ItemSchema.DefinitionIndex = metadata.DefinitionIndex
		}
	}
}

func mapProjectionDiagnostics(values []fusedobject.SchemaProjectionDiagnostic) []models.SchemaProjectionDiagnostic {
	if values == nil {
		return nil
	}
	mapped := make([]models.SchemaProjectionDiagnostic, len(values))
	for i, value := range values {
		mapped[i] = models.SchemaProjectionDiagnostic{
			Code: value.Code, Keyword: value.Keyword, Pointer: value.Pointer, Message: value.Message,
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
		mapped[status] = models.ResponseContract{
			Summary: response.Summary, Description: response.Description, Headers: mapHeaderContracts(response.Headers),
			Representations: mapResponseRepresentations(response.Representations), Links: mapLinkContracts(response.Links),
		}
	}
	return mapped
}

func mapHeaderContracts(values map[string]fusedobject.HeaderContract) map[string]models.HeaderContract {
	if len(values) == 0 {
		return nil
	}
	mapped := make(map[string]models.HeaderContract, len(values))
	for name, value := range values {
		mapped[name] = models.HeaderContract{
			Description: value.Description, Required: value.Required, Deprecated: value.Deprecated,
			Serialization: models.ParameterSerialization(value.Serialization), Schema: mapSchemaContract(value.Schema),
			Content: mapParameterContent(value.Content), Example: value.Example, Examples: value.Examples,
		}
	}
	return mapped
}

func mapResponseRepresentations(values []fusedobject.ResponseRepresentation) []models.ResponseRepresentation {
	if values == nil {
		return nil
	}
	mapped := make([]models.ResponseRepresentation, len(values))
	for i, value := range values {
		mapped[i] = models.ResponseRepresentation{
			MediaType: value.MediaType, Schema: mapSchemaContract(value.Schema), ItemSchema: mapSchemaContract(value.ItemSchema),
			SSE:            mapSSEResponseContract(value.SSE),
			PrefixEncoding: mapRequestEncodingSlice(value.PrefixEncoding), ItemEncoding: mapOptionalRequestEncoding(value.ItemEncoding),
			Example: value.Example, Examples: value.Examples,
		}
	}
	return mapped
}

func mapSSEResponseContract(value *fusedobject.SSEResponseContract) *models.SSEResponseContract {
	if value == nil {
		return nil
	}
	return &models.SSEResponseContract{ItemMode: value.ItemMode, DoneSentinel: cloneOptionalString(value.DoneSentinel)}
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func mapLinkContracts(values map[string]fusedobject.LinkContract) map[string]models.LinkContract {
	if len(values) == 0 {
		return nil
	}
	mapped := make(map[string]models.LinkContract, len(values))
	for name, value := range values {
		mapped[name] = models.LinkContract{
			OperationRef: value.OperationRef, OperationID: value.OperationID, Description: value.Description,
			Parameters: value.Parameters, RequestBody: value.RequestBody, Server: mapServer(value.Server), Extensions: mapNamespacedExtensions(value.Extensions),
		}
	}
	return mapped
}

// mapInboundOperationContract preserves documentary inbound security without joining the outbound credential namespace.
func mapInboundOperationContract(value *fusedobject.InboundOperationContract) *models.InboundOperationContract {
	// Legacy uploads intentionally have no standard inbound operation contract.
	if value == nil {
		return nil
	}
	return &models.InboundOperationContract{
		Kind: value.Kind, RuntimeExpression: value.RuntimeExpression, Parent: mapCallbackParent(value.Parent), Path: value.Path,
		Summary: value.Summary, Description: value.Description, Tags: append([]string(nil), value.Tags...),
		ExternalDocs: mapExternalDocumentation(value.ExternalDocs), Deprecated: value.Deprecated,
		OperationServers: mapServers(value.OperationServers), Parameters: mapParameters(value.Parameters),
		RequestContent: mapRequestContent(value.RequestContent), Responses: mapResponses(value.Responses),
		SecurityRequirements: value.SecurityRequirements, SecuritySchemes: mapInboundSecuritySchemes(value.SecuritySchemes),
		Extensions: mapNamespacedExtensions(value.Extensions),
	}
}

func mapCallbackParent(value *fusedobject.CallbackParent) *models.CallbackParent {
	if value == nil {
		return nil
	}
	return &models.CallbackParent{OperationID: value.OperationID, Method: value.Method, Path: value.Path, CallbackName: value.CallbackName}
}

func mapOperationDocumentation(value *fusedobject.OperationDocumentation) *models.OperationDocumentation {
	if value == nil {
		return nil
	}
	return &models.OperationDocumentation{
		Summary: value.Summary, Description: value.Description, Tags: append([]string(nil), value.Tags...),
		ExternalDocs: mapExternalDocumentation(value.ExternalDocs), Extensions: mapNamespacedExtensions(value.Extensions),
	}
}

func mapServiceDocumentation(value *fusedobject.ServiceDocumentation) *models.ServiceDocumentation {
	if value == nil {
		return nil
	}
	mapped := &models.ServiceDocumentation{
		TermsOfService: value.TermsOfService, Contact: mapContactDocumentation(value.Contact),
		License: mapLicenseDocumentation(value.License), ExternalDocs: mapExternalDocumentation(value.ExternalDocs),
		Extensions: mapNamespacedExtensions(value.Extensions), Tags: make([]models.TagDocumentation, len(value.Tags)),
	}
	for index, tag := range value.Tags {
		mapped.Tags[index] = models.TagDocumentation{
			Name: tag.Name, Summary: tag.Summary, Description: tag.Description,
			Parent: tag.Parent, Kind: tag.Kind, ExternalDocs: mapExternalDocumentation(tag.ExternalDocs),
		}
	}
	return mapped
}

func mapContactDocumentation(value *fusedobject.ContactDocumentation) *models.ContactDocumentation {
	if value == nil {
		return nil
	}
	return &models.ContactDocumentation{Name: value.Name, URL: value.URL, Email: value.Email}
}

func mapLicenseDocumentation(value *fusedobject.LicenseDocumentation) *models.LicenseDocumentation {
	if value == nil {
		return nil
	}
	return &models.LicenseDocumentation{Name: value.Name, Identifier: value.Identifier, URL: value.URL}
}

func mapExternalDocumentation(value *fusedobject.ExternalDocumentation) *models.ExternalDocumentation {
	if value == nil {
		return nil
	}
	return &models.ExternalDocumentation{Description: value.Description, URL: value.URL}
}

func mapNamespacedExtensions(values fusedobject.NamespacedExtensions) models.NamespacedExtensions {
	if values == nil {
		return nil
	}
	mapped := make(models.NamespacedExtensions, len(values))
	for name, value := range values {
		mapped[name] = models.NamespacedExtension{Value: append([]byte(nil), value.Value...), Provenance: value.Provenance}
	}
	return mapped
}

func mapServer(server *fusedobject.Server) *models.Server {
	if server == nil {
		return nil
	}
	mapped := mapServers(fusedobject.Servers{*server})
	return &mapped[0]
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
			Name:                    authCredentialName(a),
			Type:                    a.Type,
			Scheme:                  a.Scheme,
			BasicPasswordMode:       a.BasicPasswordMode,
			Location:                a.Location,
			KeyName:                 a.KeyName,
			TokenEndpointAuthMethod: models.TokenEndpointAuthMethod(a.TokenEndpointAuthMethod),
			TokenRequestMediaType:   models.TokenRequestMediaType(a.TokenRequestMediaType),
			OpenIdConnectUrl:        a.OpenIdConnectUrl,
			OAuth2MetadataURL:       a.OAuth2MetadataURL, Deprecated: a.Deprecated,
			PKCERequired: a.PKCERequired, ScopesDelimiter: a.ScopesDelimiter,
			ExtraAuthParams: a.ExtraAuthParams, ExtraTokenParams: a.ExtraTokenParams,
			RefreshTokenRotates: a.RefreshTokenRotates, RefreshTokenRequired: a.RefreshTokenRequired,
			OAuth2Flows: mapOAuth2Flows(a.OAuth2Flows),
			Strategy:    mapAuthStrategy(a.Strategy), PolicyProvenance: a.PolicyProvenance,
		})
	}
	return out
}

func mapOAuth2Flows(values map[string]fusedobject.OAuth2FlowContract) map[string]models.OAuth2FlowContract {
	if len(values) == 0 {
		return nil
	}
	mapped := make(map[string]models.OAuth2FlowContract, len(values))
	for name, value := range values {
		mapped[name] = models.OAuth2FlowContract{
			AuthorizationURL: value.AuthorizationURL, DeviceAuthorizationURL: value.DeviceAuthorizationURL, TokenURL: value.TokenURL,
			RefreshURL: value.RefreshURL, Scopes: value.Scopes,
		}
	}
	return mapped
}

func mapAuthStrategy(value *fusedobject.AuthRuntimeStrategy) *models.AuthRuntimeStrategy {
	if value == nil {
		return nil
	}
	mapped := &models.AuthRuntimeStrategy{Kind: value.Kind}
	if value.OAuth1 != nil {
		mapped.OAuth1 = &models.OAuth1Strategy{
			SignatureMethod: value.OAuth1.SignatureMethod, ParameterLocation: value.OAuth1.ParameterLocation,
		}
	}
	if value.Challenge != nil {
		mapped.Challenge = &models.HTTPChallengeStrategy{Scheme: value.Challenge.Scheme}
	}
	return mapped
}
