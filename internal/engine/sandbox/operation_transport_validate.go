package sandbox

import (
	"errors"
	"mime"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemacontract"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

var runtimeMethod = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// validateEndpointTransport requires an explicit provider protocol because
// inferring REST or GraphQL from method and body would create a second contract.
func validateEndpointTransport(endpoint fusedobject.Endpoint) error {
	if err := validateOperationMethod(endpoint.Method); err != nil {
		return err
	}
	if err := validateProviderProtocolContract(endpoint); err != nil {
		return err
	}
	return validateOperationWireContract(endpoint)
}

func validateInboundOperationTransport(endpoint fusedobject.Endpoint) error {
	if err := validateOperationMethod(endpoint.Method); err != nil {
		return err
	}
	return validateOperationWireContract(endpoint)
}

func validateOperationMethod(method string) error {
	// OpenAPI 3.2 additionalOperations deliberately permits extension methods.
	// HTTP token validation keeps that extensibility without admitting request
	// splitting characters or unbounded values at the execution boundary.
	if method == "" || len(method) > 32 || !runtimeMethod.MatchString(method) {
		return errors.New("runtime operation method is invalid")
	}
	return nil
}

func validateOperationWireContract(endpoint fusedobject.Endpoint) error {
	if err := validateOperationServers(endpoint.OperationServers); err != nil {
		return err
	}
	if err := validateRuntimeParameters(endpoint.Parameters); err != nil {
		return err
	}
	if err := validateRuntimePathParameters(endpoint.Path, endpoint.Parameters); err != nil {
		return err
	}
	return validateEndpointMediaContracts(endpoint)
}

// validateRuntimePathParameters enforces a one-to-one declaration mapping so no
// placeholder is invented or silently left unresolved during dispatch.
func validateRuntimePathParameters(path string, parameters fusedobject.Parameters) error {
	if strings.HasPrefix(strings.TrimSpace(path), "{$") {
		// OpenAPI callback runtime expressions are resolved from the parent
		// exchange and are not outbound URI-template parameters.
		return nil
	}
	declared := make(map[string]struct{})
	for _, parameter := range parameters {
		if strings.EqualFold(parameter.In, "path") {
			declared[parameter.Name] = struct{}{}
		}
	}
	placeholders := regexp.MustCompile(`\{([^{}]+)\}`).FindAllStringSubmatch(path, -1)
	for _, placeholder := range placeholders {
		if _, ok := declared[placeholder[1]]; !ok {
			return errors.New("runtime path placeholder is undeclared")
		}
		delete(declared, placeholder[1])
	}
	if len(declared) != 0 {
		return errors.New("runtime path parameter has no placeholder")
	}
	return nil
}

func validateProviderProtocolContract(endpoint fusedobject.Endpoint) error {
	switch endpoint.ProviderProtocol {
	case models.ProviderProtocolREST:
		if endpoint.GraphQLQuery != nil || endpoint.OperationKind != "" {
			return errors.New("runtime REST operation has GraphQL semantics")
		}
		return nil
	case models.ProviderProtocolGraphQL:
		if endpoint.GraphQLQuery == nil || strings.TrimSpace(*endpoint.GraphQLQuery) == "" {
			return errors.New("runtime GraphQL operation query is missing")
		}
		if endpoint.OperationKind != models.OperationKindQuery && endpoint.OperationKind != models.OperationKindMutation {
			return errors.New("runtime GraphQL operation kind is invalid")
		}
		return nil
	default:
		return errors.New("runtime provider protocol is invalid")
	}
}

func validateOperationServers(servers fusedobject.Servers) error {
	seen := make(map[string]struct{}, len(servers))
	seenNames := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if err := validateOperationServer(server); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(server.Environment)) + "\x00" + strings.TrimSpace(server.URL)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("runtime operation server is duplicated")
		}
		seen[key] = struct{}{}
		if name := strings.ToLower(runtimeServerName(server.Name, server.Environment)); name != "" {
			if _, duplicate := seenNames[name]; duplicate {
				return errors.New("runtime operation server name is duplicated")
			}
			seenNames[name] = struct{}{}
		}
	}
	return nil
}

// validateOperationServer admits routing templates without treating unresolved
// provider variables as literal URL syntax or requiring tenant credentials early.
func validateOperationServer(server fusedobject.Server) error {
	raw := strings.TrimSpace(server.URL)
	// Bound authored input before variable substitution can enlarge its URL.
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") {
		return errors.New("runtime operation server URL is invalid")
	}
	// Environment selectors remain bounded independently from routing variables.
	if len(server.Name) > 128 || strings.ContainsAny(server.Name, "\r\n") {
		return errors.New("runtime operation server name is invalid")
	}
	// Preserve exact declaration, duplicate, and enum checks before any expansion.
	if err := validateRuntimeServers(fusedobject.Servers{server}); err != nil {
		return err
	}
	return serverrouting.ValidateReferenceTemplate(raw, server.Variables)
}

func validateRuntimeParameters(parameters fusedobject.Parameters) error {
	if err := validateRuntimeParameterLocations(parameters); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(parameters))
	for _, parameter := range parameters {
		if err := validateRuntimeParameter(parameter); err != nil {
			return err
		}
		key := strings.ToLower(parameter.In) + "\x00" + canonicalParameterName(parameter)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("runtime operation parameter is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateRuntimeParameterLocations(parameters fusedobject.Parameters) error {
	queryCount := 0
	queryStringCount := 0
	for _, parameter := range parameters {
		switch strings.ToLower(parameter.In) {
		case "query":
			queryCount++
		case "querystring":
			queryStringCount++
		}
	}
	if queryStringCount > 1 || (queryStringCount > 0 && queryCount > 0) {
		return errors.New("runtime querystring parameter conflicts with query parameters")
	}
	return nil
}

func canonicalParameterName(parameter fusedobject.Parameter) string {
	if strings.EqualFold(parameter.In, "header") {
		return strings.ToLower(parameter.Name)
	}
	return parameter.Name
}

func validateRuntimeParameter(parameter fusedobject.Parameter) error {
	location := strings.ToLower(parameter.In)
	if strings.TrimSpace(parameter.Name) == "" || len(parameter.Name) > 256 || !validParameterLocation(location) {
		return errors.New("runtime operation parameter identity is invalid")
	}
	if location == "path" && !parameter.Required {
		return errors.New("runtime path parameter must be required")
	}
	if err := validateParameterExamplesAndContent(parameter); err != nil {
		return err
	}
	if location == "querystring" {
		return validateQueryStringParameter(parameter)
	}
	return validateParameterSerialization(location, parameter.Serialization)
}

func validateQueryStringParameter(parameter fusedobject.Parameter) error {
	serialization := parameter.Serialization
	if parameter.Schema != nil || len(parameter.Content) != 1 || serialization.Style != "" || serialization.Explode != nil || serialization.AllowReserved != nil || serialization.AllowEmptyValue != nil {
		return errors.New("runtime querystring parameter must use content serialization")
	}
	for mediaType, content := range parameter.Content {
		if !supportedQueryStringMediaType(mediaType) {
			return errors.New("runtime querystring media type is unsupported")
		}
		if err := validateQueryStringEncodings(mediaType, content.Encoding); err != nil {
			return err
		}
	}
	return nil
}

func validateQueryStringEncodings(mediaType string, encodings map[string]fusedobject.RequestEncoding) error {
	parsed, _, _ := mime.ParseMediaType(mediaType)
	if len(encodings) == 0 || parsed == "application/x-www-form-urlencoded" {
		for property, encoding := range encodings {
			if strings.TrimSpace(property) == "" || !validQueryStringEncoding(encoding) {
				return errors.New("runtime querystring property encoding is invalid")
			}
		}
		return nil
	}
	return errors.New("runtime querystring encoding requires form media")
}

func validQueryStringEncoding(encoding fusedobject.RequestEncoding) bool {
	if queryStringEncodingHasUnsupportedFields(encoding) {
		return false
	}
	if encoding.Style != "" && !validParameterStyle("query", encoding.Style) {
		return false
	}
	if encoding.ContentType == "" {
		return true
	}
	return validQueryStringEncodingContentType(encoding.ContentType)
}

func queryStringEncodingHasUnsupportedFields(encoding fusedobject.RequestEncoding) bool {
	return len(encoding.Headers) > 0 || encoding.BinaryEncoding != "" || len(encoding.Encoding) > 0 ||
		len(encoding.PrefixEncoding) > 0 || encoding.ItemEncoding != nil
}

func validQueryStringEncodingContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "text/plain" || mediaType == "application/json"
}

func supportedQueryStringMediaType(value string) bool {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed) {
	case "application/x-www-form-urlencoded", "application/json", "application/jsonpath", "text/plain":
		return true
	default:
		return false
	}
}

func validateParameterExamplesAndContent(parameter fusedobject.Parameter) error {
	if err := validateParameterShape(parameter); err != nil {
		return err
	}
	if err := validateSchemaContract(parameter.Schema); err != nil {
		return err
	}
	for mediaType, content := range parameter.Content {
		if err := validateParameterContent(mediaType, content); err != nil {
			return err
		}
	}
	return nil
}

func validateParameterShape(parameter fusedobject.Parameter) error {
	hasSchema := parameter.Schema != nil
	hasContent := len(parameter.Content) > 0
	if hasSchema && hasContent {
		return errors.New("runtime parameter schema and content are mutually exclusive")
	}
	if !hasSchema && !hasContent && strings.TrimSpace(parameter.Type) == "" {
		return errors.New("runtime parameter has no executable type contract")
	}
	if len(parameter.Content) > 1 {
		return errors.New("runtime parameter content must select one media type")
	}
	if parameter.Example != nil && len(parameter.Examples) > 0 {
		return errors.New("runtime parameter example and examples are mutually exclusive")
	}
	return nil
}

func validateParameterContent(mediaType string, content fusedobject.ParameterContent) error {
	if err := schemacontract.ValidateParameterContent(content, 0); err != nil {
		return err
	}
	if content.ItemSchema != nil || len(content.PrefixEncoding) > 0 || content.ItemEncoding != nil {
		// Engine does not treat headers, cookies, or query values as item
		// streams; accepting positional metadata here would be preservation-only.
		return unsupportedExecutionCapability()
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return errors.New("runtime parameter content media type is invalid")
	}
	return nil
}

func validParameterLocation(location string) bool {
	switch location {
	case "query", "querystring", "path", "header", "cookie":
		return true
	default:
		return false
	}
}

func validateParameterSerialization(location string, serialization fusedobject.ParameterSerialization) error {
	style := serialization.Style
	if style != "" && !validParameterStyle(location, style) {
		return errors.New("runtime parameter serialization style is invalid")
	}
	if boolValue(serialization.AllowEmptyValue) && location != "query" {
		return errors.New("runtime allow_empty_value is invalid outside query parameters")
	}
	return nil
}

func validParameterStyle(location, style string) bool {
	styles, known := runtimeParameterStyles[location]
	return known && styles[style]
}

var runtimeParameterStyles = map[string]map[string]bool{
	"query":  {"form": true, "spaceDelimited": true, "pipeDelimited": true, "deepObject": true},
	"path":   {"simple": true, "label": true, "matrix": true},
	"header": {"simple": true},
	"cookie": {"form": true, "cookie": true},
}

func boolValue(value *bool) bool { return value != nil && *value }
