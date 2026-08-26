package store

import (
	"fmt"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/schemacontract"
)

// normalizeServiceContractHashInput validates shared identities before hashing compact roots and metadata once.
func normalizeServiceContractHashInput(input serviceContractHashInput) (serviceContractHashInput, error) {
	input.ServiceMetadata.ExecutionContractEnvelope = fusedobject.ExecutionContractEnvelope{ContractVersion: input.ContractVersion, RequiredCapabilities: input.RequiredCapabilities}
	// The database path repeats admission independently of the Registry transport boundary.
	if err := schemacontract.PrepareSnapshot(&input.ServiceMetadata, input.Endpoints, input.Webhooks); err != nil {
		return input, err
	}
	// Canonicalize only embedded raw values: a service snapshot can legitimately
	// exceed the per-schema bound when it contains many independently bounded schemas.
	metadata, err := normalizeHashServiceMetadata(input.ServiceMetadata)
	// Invalid metadata cannot be bypassed by otherwise valid child contracts.
	if err != nil {
		return input, err
	}
	endpoints, err := normalizeHashEndpoints(input.Endpoints)
	// Every selected operation contributes authoritative schema truth to the snapshot identity.
	if err != nil {
		return input, err
	}
	webhooks, err := normalizeHashWebhooks(input.Webhooks)
	// Inbound contracts share the same integrity boundary as outbound operations.
	if err != nil {
		return input, err
	}
	input.ServiceMetadata, input.Endpoints, input.Webhooks = metadata, endpoints, webhooks
	return input, nil
}

// normalizeHashServiceMetadata keeps dictionary hashes stable across JSONB member and number rewriting.
func normalizeHashServiceMetadata(value fusedobject.ServiceMetadata) (fusedobject.ServiceMetadata, error) {
	definitions, err := normalizeHashDefinitions(value)
	// Invalid definition truth cannot be hidden by an otherwise unchanged operation list.
	if err != nil {
		return value, err
	}
	value.SchemaDefinitions = definitions
	// Documentation is optional independently of executable schema definitions.
	if value.Documentation == nil {
		return value, nil
	}
	documentation := *value.Documentation
	extensions, err := normalizeHashExtensions(documentation.Extensions)
	// Extension normalization cannot weaken the surrounding metadata hash on failure.
	if err != nil {
		return value, err
	}
	documentation.Extensions = extensions
	value.Documentation = &documentation
	return value, nil
}

// normalizeHashDefinitions hashes each version-owned raw definition once rather than per referencing operation.
func normalizeHashDefinitions(metadata fusedobject.ServiceMetadata) (map[string]fusedobject.SchemaContract, error) {
	// Preserve omission for legacy standalone snapshots so their identity remains stable.
	if metadata.SchemaDefinitions == nil {
		return nil, nil
	}
	result := make(map[string]fusedobject.SchemaContract, len(metadata.SchemaDefinitions))
	for name, definition := range metadata.SchemaDefinitions {
		definition.DefinitionIndex = metadata.DefinitionIndex
		normalized, err := normalizeHashSchema(&definition)
		// A single corrupted definition invalidates the complete immutable snapshot.
		if err != nil {
			return nil, err
		}
		result[name] = *normalized
	}
	return result, nil
}

func normalizeHashEndpoints(values []fusedobject.Endpoint) ([]fusedobject.Endpoint, error) {
	out := append([]fusedobject.Endpoint(nil), values...)
	for index := range out {
		normalized, err := normalizeHashEndpoint(out[index])
		if err != nil {
			return nil, err
		}
		out[index] = normalized
	}
	return out, nil
}

func normalizeHashEndpoint(value fusedobject.Endpoint) (fusedobject.Endpoint, error) {
	var err error
	if value.Parameters, err = normalizeHashParameters(value.Parameters); err != nil {
		return value, err
	}
	if value.RequestContent, err = normalizeHashRequestContent(value.RequestContent); err != nil {
		return value, err
	}
	if value.Responses, err = normalizeHashResponses(value.Responses); err != nil {
		return value, err
	}
	if value.Documentation != nil {
		documentation := *value.Documentation
		documentation.Extensions, err = normalizeHashExtensions(documentation.Extensions)
		value.Documentation = &documentation
	}
	return value, err
}

func normalizeHashWebhooks(values []fusedobject.Webhook) ([]fusedobject.Webhook, error) {
	out := append([]fusedobject.Webhook(nil), values...)
	for index := range out {
		if out[index].Contract == nil {
			continue
		}
		contract, err := normalizeHashInboundContract(*out[index].Contract)
		if err != nil {
			return nil, err
		}
		out[index].Contract = &contract
	}
	return out, nil
}

func normalizeHashInboundContract(value fusedobject.InboundOperationContract) (fusedobject.InboundOperationContract, error) {
	var err error
	if value.Parameters, err = normalizeHashParameters(value.Parameters); err != nil {
		return value, err
	}
	if value.RequestContent, err = normalizeHashRequestContent(value.RequestContent); err != nil {
		return value, err
	}
	if value.Responses, err = normalizeHashResponses(value.Responses); err != nil {
		return value, err
	}
	value.Extensions, err = normalizeHashExtensions(value.Extensions)
	return value, err
}

func normalizeHashParameters(values fusedobject.Parameters) (fusedobject.Parameters, error) {
	out := append(fusedobject.Parameters(nil), values...)
	for index := range out {
		var err error
		if out[index].Schema, err = normalizeHashSchema(out[index].Schema); err != nil {
			return nil, err
		}
		if out[index].Content, err = normalizeHashParameterContents(out[index].Content, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeHashRequestContent(value *fusedobject.RequestContent) (*fusedobject.RequestContent, error) {
	if value == nil {
		return nil, nil
	}
	out := *value
	out.Representations = append([]fusedobject.RequestRepresentation(nil), value.Representations...)
	for index := range out.Representations {
		normalized, err := normalizeHashRequestRepresentation(out.Representations[index])
		if err != nil {
			return nil, err
		}
		out.Representations[index] = normalized
	}
	return &out, nil
}

func normalizeHashRequestRepresentation(value fusedobject.RequestRepresentation) (fusedobject.RequestRepresentation, error) {
	var err error
	if value.Schema, err = normalizeHashSchema(value.Schema); err != nil {
		return value, err
	}
	if value.ItemSchema, err = normalizeHashSchema(value.ItemSchema); err != nil {
		return value, err
	}
	if value.Encoding, err = normalizeHashEncodings(value.Encoding, 0); err != nil {
		return value, err
	}
	value.PrefixEncoding, value.ItemEncoding, err = normalizeHashPositionalEncodings(value.PrefixEncoding, value.ItemEncoding, 0)
	return value, err
}

func normalizeHashParameterContents(values map[string]fusedobject.ParameterContent, depth int) (map[string]fusedobject.ParameterContent, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]fusedobject.ParameterContent, len(values))
	for name, value := range values {
		normalized, err := normalizeHashParameterContent(value, depth)
		if err != nil {
			return nil, err
		}
		out[name] = normalized
	}
	return out, nil
}

func normalizeHashParameterContent(value fusedobject.ParameterContent, depth int) (fusedobject.ParameterContent, error) {
	var err error
	if value.Schema, err = normalizeHashSchema(value.Schema); err != nil {
		return value, err
	}
	if value.ItemSchema, err = normalizeHashSchema(value.ItemSchema); err != nil {
		return value, err
	}
	if value.Encoding, err = normalizeHashEncodings(value.Encoding, depth); err != nil {
		return value, err
	}
	value.PrefixEncoding, value.ItemEncoding, err = normalizeHashPositionalEncodings(value.PrefixEncoding, value.ItemEncoding, depth)
	return value, err
}

func normalizeHashResponses(values fusedobject.Responses) (fusedobject.Responses, error) {
	if values == nil {
		return nil, nil
	}
	out := make(fusedobject.Responses, len(values))
	for status, value := range values {
		normalized, err := normalizeHashResponse(value)
		if err != nil {
			return nil, err
		}
		out[status] = normalized
	}
	return out, nil
}

func normalizeHashResponse(value fusedobject.ResponseContract) (fusedobject.ResponseContract, error) {
	var err error
	if value.Headers, err = normalizeHashHeaders(value.Headers, 0); err != nil {
		return value, err
	}
	value.Representations = append([]fusedobject.ResponseRepresentation(nil), value.Representations...)
	for index := range value.Representations {
		if value.Representations[index], err = normalizeHashResponseRepresentation(value.Representations[index]); err != nil {
			return value, err
		}
	}
	value.Links, err = normalizeHashLinks(value.Links)
	return value, err
}

func normalizeHashResponseRepresentation(value fusedobject.ResponseRepresentation) (fusedobject.ResponseRepresentation, error) {
	var err error
	if value.Schema, err = normalizeHashSchema(value.Schema); err != nil {
		return value, err
	}
	if value.ItemSchema, err = normalizeHashSchema(value.ItemSchema); err != nil {
		return value, err
	}
	value.PrefixEncoding, value.ItemEncoding, err = normalizeHashPositionalEncodings(value.PrefixEncoding, value.ItemEncoding, 0)
	return value, err
}

func normalizeHashEncodings(values map[string]fusedobject.RequestEncoding, depth int) (map[string]fusedobject.RequestEncoding, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]fusedobject.RequestEncoding, len(values))
	for name, value := range values {
		normalized, err := normalizeHashEncoding(value, depth)
		if err != nil {
			return nil, err
		}
		out[name] = normalized
	}
	return out, nil
}

func normalizeHashEncoding(value fusedobject.RequestEncoding, depth int) (fusedobject.RequestEncoding, error) {
	// The transport validator permits only a shallower reviewed shape, but the
	// hash boundary still caps hostile pointer graphs before recursive copying.
	if depth >= canonicaljson.MaxDepth {
		return value, fmt.Errorf("service contract encoding exceeds maximum depth %d", canonicaljson.MaxDepth)
	}
	var err error
	if value.Headers, err = normalizeHashHeaders(value.Headers, depth+1); err != nil {
		return value, err
	}
	if value.Encoding, err = normalizeHashEncodings(value.Encoding, depth+1); err != nil {
		return value, err
	}
	value.PrefixEncoding, value.ItemEncoding, err = normalizeHashPositionalEncodings(value.PrefixEncoding, value.ItemEncoding, depth+1)
	return value, err
}

func normalizeHashPositionalEncodings(values []fusedobject.RequestEncoding, item *fusedobject.RequestEncoding, depth int) ([]fusedobject.RequestEncoding, *fusedobject.RequestEncoding, error) {
	out := append([]fusedobject.RequestEncoding(nil), values...)
	for index := range out {
		normalized, err := normalizeHashEncoding(out[index], depth)
		if err != nil {
			return nil, nil, err
		}
		out[index] = normalized
	}
	if item == nil {
		return out, nil, nil
	}
	normalized, err := normalizeHashEncoding(*item, depth)
	return out, &normalized, err
}

func normalizeHashHeaders(values map[string]fusedobject.HeaderContract, depth int) (map[string]fusedobject.HeaderContract, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]fusedobject.HeaderContract, len(values))
	for name, value := range values {
		normalized, err := normalizeHashHeader(value, depth)
		if err != nil {
			return nil, err
		}
		out[name] = normalized
	}
	return out, nil
}

func normalizeHashHeader(value fusedobject.HeaderContract, depth int) (fusedobject.HeaderContract, error) {
	var err error
	if value.Schema, err = normalizeHashSchema(value.Schema); err != nil {
		return value, err
	}
	value.Content, err = normalizeHashParameterContents(value.Content, depth)
	return value, err
}

func normalizeHashLinks(values map[string]fusedobject.LinkContract) (map[string]fusedobject.LinkContract, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]fusedobject.LinkContract, len(values))
	for name, value := range values {
		extensions, err := normalizeHashExtensions(value.Extensions)
		if err != nil {
			return nil, err
		}
		value.Extensions = extensions
		out[name] = value
	}
	return out, nil
}

func normalizeHashExtensions(values fusedobject.NamespacedExtensions) (fusedobject.NamespacedExtensions, error) {
	if values == nil {
		return nil, nil
	}
	out := make(fusedobject.NamespacedExtensions, len(values))
	for name, value := range values {
		canonical, err := canonicaljson.Canonicalize(value.Value)
		if err != nil {
			return nil, fmt.Errorf("canonicalize service contract extension: %w", err)
		}
		value.Value = canonical
		out[name] = value
	}
	return out, nil
}

// normalizeHashSchema preserves schema identity across jsonb reserialization using Registry's schema profile.
func normalizeHashSchema(value *fusedobject.SchemaContract) (*fusedobject.SchemaContract, error) {
	// Absent schemas must remain absent in the service identity.
	if value == nil {
		return nil, nil
	}
	// Integrity is checked before normalized bytes enter the service hash input.
	if err := schemacontract.Validate(value); err != nil {
		return nil, err
	}
	out := *value
	canonical, err := canonicaljson.CanonicalizeSchema(out.Raw)
	// Never hash a partial or over-budget canonical representation.
	if err != nil {
		return nil, fmt.Errorf("canonicalize service contract schema: %w", err)
	}
	out.Raw = canonical
	return &out, nil
}
