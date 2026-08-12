package sandbox

import (
	"errors"
	"mime"
	"regexp"
	"strings"

	engine "github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemacontract"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
)

var (
	exactResponseStatus = regexp.MustCompile(`^[1-5][0-9]{2}$`)
	rangeResponseStatus = regexp.MustCompile(`^[1-5]XX$`)
	contractHeaderName  = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
)

// validateEndpointMediaContracts applies the same framing boundary to request
// and response contracts before either can influence provider execution.
func validateEndpointMediaContracts(endpoint fusedobject.Endpoint) error {
	if err := validateRequestContentContract(endpoint.RequestContent); err != nil {
		return err
	}
	return validateResponseContracts(endpoint.Responses)
}

func validateRequestContentContract(content *fusedobject.RequestContent) error {
	if content == nil {
		return nil
	}
	if err := workflowcontract.Validate(content.UploadWorkflow); err != nil {
		return err
	}
	if len(content.Representations) == 0 {
		return errors.New("runtime request content has no representations")
	}
	if len(content.Representations) > 32 {
		return errors.New("runtime request content has too many representations")
	}
	if err := validateRequestRepresentationSelection(content); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(content.Representations))
	for _, representation := range content.Representations {
		if err := validateRequestRepresentation(representation); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(representation.MediaType))
		if _, duplicate := seen[key]; duplicate {
			return errors.New("runtime request media type is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateRequestRepresentationSelection reuses runtime selection so snapshot
// admission cannot disagree with the later dispatch decision.
func validateRequestRepresentationSelection(content *fusedobject.RequestContent) error {
	mapped := make([]models.RequestRepresentation, len(content.Representations))
	for i, value := range content.Representations {
		mapped[i] = models.RequestRepresentation{MediaType: value.MediaType, Serialization: value.Serialization}
	}
	_, _, err := engine.SelectRequestContent(&models.RequestContent{
		Representations: mapped, DefaultMediaType: content.DefaultMediaType,
	})
	return err
}

func validateRequestRepresentation(value fusedobject.RequestRepresentation) error {
	if value.Serialization == "" {
		// The Registry reviews wire framing. Inferring it again from a media type
		// would create a second contract that can drift from generated clients.
		return errors.New("runtime request representation serialization is required")
	}
	mapped := models.RequestRepresentation{MediaType: value.MediaType, Serialization: value.Serialization}
	if _, _, err := engine.SelectRequestContent(&models.RequestContent{Representations: []models.RequestRepresentation{mapped}}); err != nil {
		return err
	}
	if value.Example != nil && len(value.Examples) > 0 {
		return errors.New("runtime request representation example and examples are mutually exclusive")
	}
	if err := validateSchemaContract(value.Schema); err != nil {
		return err
	}
	if err := validateSchemaContract(value.ItemSchema); err != nil {
		return err
	}
	return validateRequestEncodings(value)
}

func validateRequestEncodings(representation fusedobject.RequestRepresentation) error {
	baseMediaType, _, _ := mime.ParseMediaType(representation.MediaType)
	if err := validateMediaEncodingShape(baseMediaType, representation.Schema, representation.ItemSchema, representation.Encoding, representation.PrefixEncoding, representation.ItemEncoding); err != nil {
		return err
	}
	if len(representation.Encoding) > 0 && !requestNamedEncodingMediaType(baseMediaType) {
		return errors.New("runtime request encoding is invalid for media type")
	}
	if representation.ItemSchema != nil && !multipartMediaType(baseMediaType) && !supportedSequentialRequestMediaType(baseMediaType) {
		return errors.New("runtime sequential request media type is unsupported")
	}
	return validateRequestEncodingSet(representation)
}

func requestNamedEncodingMediaType(mediaType string) bool {
	return mediaType == "application/x-www-form-urlencoded" || multipartMediaType(mediaType)
}

func validateRequestEncodingSet(representation fusedobject.RequestRepresentation) error {
	for property, encoding := range representation.Encoding {
		if strings.TrimSpace(property) == "" {
			return errors.New("runtime request encoding property is invalid")
		}
		if err := validateNamedRequestEncoding(encoding); err != nil {
			return err
		}
	}
	for _, encoding := range representation.PrefixEncoding {
		if err := validateRequestEncoding(encoding, 0); err != nil {
			return err
		}
	}
	if representation.ItemEncoding != nil {
		return validateRequestEncoding(*representation.ItemEncoding, 0)
	}
	return nil
}

func validateNamedRequestEncoding(encoding fusedobject.RequestEncoding) error {
	if len(encoding.Encoding) > 0 || len(encoding.PrefixEncoding) > 0 || encoding.ItemEncoding != nil {
		return unsupportedExecutionCapability()
	}
	return validateRequestEncoding(encoding, 0)
}

func validateRequestEncoding(encoding fusedobject.RequestEncoding, depth int) error {
	if encoding.ContentType != "" {
		if _, _, err := mime.ParseMediaType(encoding.ContentType); err != nil {
			return errors.New("runtime request encoding content type is invalid")
		}
	}
	if encoding.BinaryEncoding != "" && encoding.BinaryEncoding != fusedobject.RequestBinaryEncodingBase64 {
		return errors.New("runtime request encoding binary encoding is invalid")
	}
	if encoding.Style != "" && !validParameterStyle("query", encoding.Style) {
		return errors.New("runtime request encoding style is invalid")
	}
	if err := validateHeaderContracts(encoding.Headers); err != nil {
		return err
	}
	return validateNestedRequestEncoding(encoding, depth)
}

func validateNestedRequestEncoding(encoding fusedobject.RequestEncoding, depth int) error {
	hasPositional := len(encoding.PrefixEncoding) > 0 || encoding.ItemEncoding != nil
	if len(encoding.Encoding) > 0 {
		// Named nested multipart needs a property-aware writer; the current
		// runtime only claims the ordered prefix/item vertical slice.
		return unsupportedExecutionCapability()
	}
	if !hasPositional {
		return nil
	}
	if depth >= 1 {
		return errors.New("runtime nested multipart encoding is unsupported")
	}
	if !multipartMediaType(encoding.ContentType) {
		return errors.New("runtime nested multipart encoding is unsupported")
	}
	return validatePositionalEncodingChildren(encoding, depth+1)
}

func validatePositionalEncodingChildren(encoding fusedobject.RequestEncoding, depth int) error {
	for _, child := range encoding.PrefixEncoding {
		if err := validateRequestEncoding(child, depth); err != nil {
			return err
		}
	}
	if encoding.ItemEncoding != nil {
		return validateRequestEncoding(*encoding.ItemEncoding, depth)
	}
	return nil
}

func supportedSequentialRequestMediaType(mediaType string) bool {
	switch strings.ToLower(mediaType) {
	case "application/jsonl", "application/json-seq", "application/x-ndjson":
		return true
	default:
		return false
	}
}

func validateMediaEncodingShape(mediaType string, schema, itemSchema *fusedobject.SchemaContract, named map[string]fusedobject.RequestEncoding, prefix []fusedobject.RequestEncoding, item *fusedobject.RequestEncoding) error {
	hasPositional := len(prefix) > 0 || item != nil
	if len(prefix) > 32 {
		return errors.New("runtime positional encoding prefix is too large")
	}
	if len(named) > 0 && hasPositional {
		return errors.New("runtime media encoding modes conflict")
	}
	if err := validatePositionalMediaType(mediaType, hasPositional); err != nil {
		return err
	}
	if itemSchema != nil && !sequentialMediaType(mediaType) {
		return errors.New("runtime item schema requires sequential media")
	}
	return validatePositionalSchema(schema, itemSchema, hasPositional)
}

func validatePositionalMediaType(mediaType string, positional bool) error {
	if positional && !multipartMediaType(mediaType) {
		return errors.New("runtime positional encoding requires multipart media")
	}
	return nil
}

func validatePositionalSchema(schema, itemSchema *fusedobject.SchemaContract, positional bool) error {
	if !positional || itemSchema != nil {
		return nil
	}
	if schema == nil || schema.Projection.Type != "array" {
		return errors.New("runtime positional encoding requires an array or item schema")
	}
	return nil
}

func multipartMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.HasPrefix(strings.ToLower(mediaType), "multipart/")
}

func sequentialMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/jsonl" || mediaType == "application/json-seq" || mediaType == "application/x-ndjson" || mediaType == "text/event-stream" || strings.HasPrefix(mediaType, "multipart/")
}

func validateResponseContracts(responses fusedobject.Responses) error {
	for status, response := range responses {
		if !validResponseStatus(status) {
			return errors.New("runtime response status key is invalid")
		}
		if response.Representations == nil {
			return errors.New("runtime response representations must be explicit")
		}
		if err := validateHeaderContracts(response.Headers); err != nil {
			return err
		}
		if err := validateResponseRepresentations(response.Representations); err != nil {
			return err
		}
		if err := validateLinkContracts(response.Links); err != nil {
			return err
		}
	}
	return nil
}

func validResponseStatus(status string) bool {
	return status == "default" || exactResponseStatus.MatchString(status) || rangeResponseStatus.MatchString(status)
}

func validateResponseRepresentations(values []fusedobject.ResponseRepresentation) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateResponseRepresentation(value, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateResponseRepresentation(value fusedobject.ResponseRepresentation, seen map[string]struct{}) error {
	mediaType, _, err := mime.ParseMediaType(value.MediaType)
	if err != nil {
		return errors.New("runtime response media type is invalid")
	}
	key := strings.ToLower(strings.TrimSpace(value.MediaType))
	if _, duplicate := seen[key]; duplicate {
		return errors.New("runtime response media type is duplicated")
	}
	seen[key] = struct{}{}
	if value.Example != nil && len(value.Examples) > 0 {
		return errors.New("runtime response example and examples are mutually exclusive")
	}
	if err := validateResponseSchemas(value); err != nil {
		return err
	}
	if len(value.PrefixEncoding) > 0 || value.ItemEncoding != nil {
		// Provider multipart responses remain raw byte streams. Accepting
		// positional response metadata would imply part-aware execution.
		return unsupportedExecutionCapability()
	}
	if value.ItemSchema != nil && strings.ToLower(mediaType) != "text/event-stream" {
		// SSE is the only response transport whose item boundaries the Engine
		// currently parses; other sequential formats remain raw byte streams.
		return unsupportedExecutionCapability()
	}
	return validateSSEResponseContract(value, mediaType)
}

func validateSSEResponseContract(value fusedobject.ResponseRepresentation, mediaType string) error {
	if value.SSE == nil {
		return nil
	}
	if !strings.EqualFold(mediaType, "text/event-stream") || value.SSE.ItemMode != "data" {
		return unsupportedExecutionCapability()
	}
	if sentinel := value.SSE.DoneSentinel; sentinel != nil {
		if *sentinel == "" || len(*sentinel) > 256 || strings.ContainsAny(*sentinel, "\r\n") {
			return unsupportedExecutionCapability()
		}
	}
	return nil
}

func validateResponseSchemas(value fusedobject.ResponseRepresentation) error {
	if err := validateSchemaContract(value.Schema); err != nil {
		return err
	}
	return validateSchemaContract(value.ItemSchema)
}

func validateHeaderContracts(headers map[string]fusedobject.HeaderContract) error {
	for name, header := range headers {
		if err := validateHeaderContract(name, header); err != nil {
			return err
		}
	}
	return nil
}

func validateHeaderContract(name string, header fusedobject.HeaderContract) error {
	if !contractHeaderName.MatchString(name) {
		return errors.New("runtime header contract name is invalid")
	}
	if header.Schema != nil && len(header.Content) > 0 {
		return errors.New("runtime header schema and content are mutually exclusive")
	}
	if header.Example != nil && len(header.Examples) > 0 {
		return errors.New("runtime header example and examples are mutually exclusive")
	}
	if err := validateSchemaContract(header.Schema); err != nil {
		return err
	}
	return validateHeaderContent(header.Content)
}

func validateHeaderContent(contents map[string]fusedobject.ParameterContent) error {
	for _, content := range contents {
		if err := schemacontract.ValidateParameterContent(content, 0); err != nil {
			return err
		}
		if content.ItemSchema != nil || len(content.PrefixEncoding) > 0 || content.ItemEncoding != nil {
			return unsupportedExecutionCapability()
		}
	}
	return nil
}

func validateLinkContracts(links map[string]fusedobject.LinkContract) error {
	for name, link := range links {
		if strings.TrimSpace(name) == "" || (link.OperationRef == "") == (link.OperationID == "") {
			return errors.New("runtime response link target is invalid")
		}
		if link.Server != nil {
			if err := validateOperationServer(*link.Server); err != nil {
				return err
			}
		}
		if err := validateNamespacedExtensions(link.Extensions); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaContract(contract *fusedobject.SchemaContract) error {
	return schemacontract.Validate(contract)
}
