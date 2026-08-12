package engine

import (
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// setProviderAcceptHeader advertises every reviewed representation while
// preserving an explicit caller header rather than inventing a preferred shape.
func setProviderAcceptHeader(request *http.Request, responses models.Responses) {
	if request == nil || request.Header.Get("Accept") != "" {
		return
	}
	mediaTypes := declaredResponseMediaTypes(responses)
	if len(mediaTypes) > 0 {
		request.Header.Set("Accept", strings.Join(mediaTypes, ", "))
	}
}

func declaredResponseMediaTypes(responses models.Responses) []string {
	unique := make(map[string]string)
	for _, response := range responses {
		for _, representation := range response.Representations {
			mediaType, _, err := mime.ParseMediaType(representation.MediaType)
			if err == nil && mediaType != "" {
				unique[strings.ToLower(mediaType)] = mediaType
			}
		}
	}
	result := make([]string, 0, len(unique))
	for _, mediaType := range unique {
		result = append(result, mediaType)
	}
	sort.Strings(result)
	return result
}

func operationSuccessResponsesAreSSE(responses models.Responses) bool {
	found := false
	for status, response := range responses {
		if !strings.HasPrefix(strings.ToUpper(status), "2") {
			continue
		}
		for _, representation := range response.Representations {
			found = true
			if boundedResponseMediaFamily(representation.MediaType) != "sse" {
				return false
			}
		}
	}
	return found
}

func recordResponseMedia(span trace.Span, operation *models.IntegrationObject, status int, contentType string) (string, string) {
	family := boundedResponseMediaFamily(contentType)
	outcome := responseMediaSelectionOutcome(operation.Responses, status, contentType)
	span.SetAttributes(
		attribute.String("response.media_family", family),
		attribute.String("response.media_selection.outcome", outcome),
		attribute.Bool("response.item_schema.present", responseItemSchemaPresent(operation.Responses, status, contentType)),
	)
	return family, outcome
}

func responseItemSchemaPresent(responses models.Responses, status int, contentType string) bool {
	representation, ok := responseRepresentationForStatusAndMedia(responses, status, contentType)
	return ok && representation.ItemSchema != nil
}

func responseMediaSelectionOutcome(responses models.Responses, status int, contentType string) string {
	contract, ok := responseContractForStatus(responses, status)
	if !ok {
		return "undocumented"
	}
	if len(contract.Representations) == 0 {
		return "no_content"
	}
	actual, _, err := mime.ParseMediaType(contentType)
	if err != nil || actual == "" {
		return "missing_or_invalid"
	}
	if _, ok := responseRepresentationForStatusAndMedia(responses, status, actual); ok {
		return "matched"
	}
	return "mismatched"
}

// responseRepresentationForStatusAndMedia selects from actual status and media;
// an operation-wide success preference cannot describe mixed provider responses.
func responseRepresentationForStatusAndMedia(responses models.Responses, status int, contentType string) (*models.ResponseRepresentation, bool) {
	contract, ok := responseContractForStatus(responses, status)
	if !ok {
		return nil, false
	}
	actual, _, err := mime.ParseMediaType(contentType)
	if err != nil || actual == "" {
		return nil, false
	}
	for index := range contract.Representations {
		declared, _, parseErr := mime.ParseMediaType(contract.Representations[index].MediaType)
		if parseErr == nil && strings.EqualFold(actual, declared) {
			return &contract.Representations[index], true
		}
	}
	return nil, false
}

// responseContractForStatus follows OpenAPI precedence so a range or default
// contract cannot shadow an exact provider status.
func responseContractForStatus(responses models.Responses, status int) (models.ResponseContract, bool) {
	if contract, ok := responses[fmt.Sprintf("%d", status)]; ok {
		return contract, true
	}
	if contract, ok := responses[fmt.Sprintf("%dXX", status/100)]; ok {
		return contract, true
	}
	contract, ok := responses["default"]
	return contract, ok
}

func boundedResponseMediaFamily(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		return "unknown"
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "text/event-stream":
		return "sse"
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return "json"
	case strings.Contains(mediaType, "xml"):
		return "xml"
	case strings.HasPrefix(mediaType, "text/"):
		return "text"
	case mediaType == "application/octet-stream", strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
		return "binary"
	default:
		return "other"
	}
}
