package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemaref"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
)

const (
	requestMediaSelectionNone    = "none"
	requestMediaSelectionSingle  = "single"
	requestMediaSelectionDefault = "reviewed_default"
	requestMediaSelectionReject  = "rejected"
)

// SelectedRequestRepresentation is an execution-only view of one canonical
// representation. It prevents runtime selection from mutating the persisted
// RequestContent DTO or rebuilding the retired singular wire fields.
type SelectedRequestRepresentation struct {
	Required                   bool
	PayloadParameter           string
	UploadWorkflow             *workflowcontract.UploadWorkflow
	MediaType                  string
	Serialization              string
	Schema                     *models.Schema
	ItemSchema                 *models.SchemaContract
	Encoding                   map[string]models.RequestEncoding
	PrefixEncoding             []models.RequestEncoding
	ItemEncoding               *models.RequestEncoding
	DeclaredBodyFields         map[string]struct{}
	AllowsAdditionalBodyFields bool
}

// SelectRequestContent chooses only an explicit reviewed default or the sole
// representation. Array order is preserved for fidelity, but never used as an
// implicit preference because a reordered spec must not change provider wire.
func SelectRequestContent(content *models.RequestContent) (*SelectedRequestRepresentation, string, error) {
	if content == nil {
		return nil, requestMediaSelectionNone, nil
	}
	if len(content.Representations) == 0 {
		return nil, requestMediaSelectionReject, errors.New("request representations are required")
	}
	representation, outcome, err := selectRequestRepresentation(content)
	if err != nil {
		return nil, requestMediaSelectionReject, err
	}
	serialization, err := effectiveMediaSerialization(representation.MediaType, representation.Serialization)
	if err != nil {
		return nil, requestMediaSelectionReject, err
	}
	declaredBodyFields, allowsAdditionalBodyFields, err := requestBodyDeclaration(representation.Schema)
	if err != nil {
		return nil, requestMediaSelectionReject, err
	}
	return &SelectedRequestRepresentation{
		Required: content.Required, PayloadParameter: content.PayloadParameter, UploadWorkflow: content.UploadWorkflow,
		MediaType: representation.MediaType, Serialization: serialization, Schema: projectionSchema(representation.Schema),
		ItemSchema: representation.ItemSchema, Encoding: representation.Encoding,
		PrefixEncoding: representation.PrefixEncoding, ItemEncoding: representation.ItemEncoding,
		DeclaredBodyFields: declaredBodyFields, AllowsAdditionalBodyFields: allowsAdditionalBodyFields,
	}, outcome, nil
}

// requestBodyDeclaration reads referenced body fields from one immutable index without expanding its graph.
func requestBodyDeclaration(contract *models.SchemaContract) (map[string]struct{}, bool, error) {
	fields := make(map[string]struct{})
	allowsAdditional := false
	// A body-less contract grants no additional caller fields.
	if contract == nil {
		return fields, false, nil
	}
	for name := range contract.Projection.Properties {
		fields[name] = struct{}{}
	}
	allowsAdditional = contract.Projection.AdditionalProperties != nil
	// Legacy fixtures without canonical raw truth retain their explicit projection-only semantics.
	if len(contract.Raw) == 0 {
		return fields, allowsAdditional, nil
	}
	// Canonical raw truth replaces the lossy projection; it must not grant extra fields omitted by the actual schema.
	fields = make(map[string]struct{})
	allowsAdditional = false
	var root any
	// Malformed raw schema cannot become a permissive projection fallback.
	if err := json.Unmarshal(contract.Raw, &root); err != nil {
		return nil, false, errors.New("request schema raw contract is invalid")
	}
	// A compact contract cannot fall back to its lossy projection when its dictionary is missing.
	if err := contract.DefinitionIndex.Validate(contract.Raw, contract.SharedDefinitions); err != nil {
		return nil, false, err
	}
	scope := bodySchemaScope{root: root}
	// Only explicitly marked roots may consult service-wide definitions.
	if contract.SharedDefinitions {
		scope.index = contract.DefinitionIndex
	}
	collectBodyDeclarations(root, scope, fields, &allowsAdditional, make(map[string]bool), 0)
	return fields, allowsAdditional, nil
}

type bodySchemaScope struct {
	root  any
	index *schemaref.Index
	name  string
}

// collectBodyDeclarations follows only root composition and references, not nested property declarations.
func collectBodyDeclarations(node any, root bodySchemaScope, fields map[string]struct{}, allowsAdditional *bool, visited map[string]bool, depth int) {
	// A literal true schema explicitly allows arbitrary JSON, unlike an omitted object declaration.
	if unconstrained, boolean := node.(bool); boolean {
		*allowsAdditional = *allowsAdditional || unconstrained
		return
	}
	object, ok := node.(map[string]any)
	// Only object schema nodes declare named fields; bounded traversal also terminates recursive compositions.
	if !ok || depth > 32 {
		return
	}
	collectBodyProperties(object, fields)
	*allowsAdditional = *allowsAdditional || schemaAllowsAdditionalProperties(object)
	collectReferencedBodyDeclaration(object, root, fields, allowsAdditional, visited, depth)
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		for _, branch := range schemaArray(object[keyword]) {
			collectBodyDeclarations(branch, root, fields, allowsAdditional, visited, depth+1)
		}
	}
}

func collectBodyProperties(object map[string]any, fields map[string]struct{}) {
	properties, _ := object["properties"].(map[string]any)
	for name := range properties {
		fields[name] = struct{}{}
	}
}

func schemaAllowsAdditionalProperties(object map[string]any) bool {
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties"} {
		switch value := object[keyword].(type) {
		case bool:
			if value {
				return true
			}
		case map[string]any:
			return true
		}
	}
	return false
}

// collectReferencedBodyDeclaration retains owning-document identity across shared and local reference scopes.
func collectReferencedBodyDeclaration(object map[string]any, root bodySchemaScope, fields map[string]struct{}, allowsAdditional *bool, visited map[string]bool, depth int) {
	ref, _ := object["$ref"].(string)
	key := root.name + "\x00" + ref
	// Reference cycles are normal schemas; visit each scope/edge only once for root field discovery.
	if ref == "" || visited[key] {
		return
	}
	target, document, name, ok := root.index.Resolve(root.root, ref)
	// Admission has already verified edges; a absent target never grants undeclared fields.
	if !ok {
		return
	}
	visited[key] = true
	root.root = document
	// Local pointers retain their prior scope; a dictionary hop begins a new document scope.
	if name != "" {
		root.name = name
	}
	collectBodyDeclarations(target, root, fields, allowsAdditional, visited, depth+1)
}

func schemaArray(value any) []any {
	items, _ := value.([]any)
	return items
}

func selectRequestRepresentation(content *models.RequestContent) (models.RequestRepresentation, string, error) {
	if content.DefaultMediaType != "" {
		for _, representation := range content.Representations {
			if strings.EqualFold(strings.TrimSpace(representation.MediaType), strings.TrimSpace(content.DefaultMediaType)) {
				return representation, requestMediaSelectionDefault, nil
			}
		}
		return models.RequestRepresentation{}, requestMediaSelectionReject, errors.New("reviewed request default_media_type is not declared")
	}
	if len(content.Representations) == 1 {
		return content.Representations[0], requestMediaSelectionSingle, nil
	}
	return models.RequestRepresentation{}, requestMediaSelectionReject, errors.New("request media representation is ambiguous without a reviewed default")
}

func effectiveMediaSerialization(mediaType, declared string) (string, error) {
	family, err := exactMediaFamily(mediaType)
	if err != nil {
		return "", err
	}
	expected := serializationForMediaFamily(family)
	if declared == "" {
		return "", errors.New("request representation serialization is required")
	}
	if expected != "" && declared != expected {
		return "", fmt.Errorf("request representation serialization conflicts with media type")
	}
	if boundedRequestSerialization(declared) == "unknown" {
		return "", errors.New("request representation serialization is unsupported")
	}
	return declared, nil
}

func exactMediaFamily(value string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || strings.TrimSpace(mediaType) == "" {
		return "", errors.New("request representation media_type is invalid")
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return "json", nil
	case mediaType == "application/x-www-form-urlencoded":
		return "form", nil
	case strings.HasPrefix(mediaType, "multipart/"):
		return "multipart", nil
	case strings.HasPrefix(mediaType, "text/"):
		return "text", nil
	case strings.Contains(mediaType, "xml"):
		return "xml", nil
	case mediaType == "application/octet-stream", strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
		return "binary", nil
	default:
		return "other", nil
	}
}

func serializationForMediaFamily(family string) string {
	switch family {
	case "json":
		return models.RequestSerializationJSON
	case "form":
		return models.RequestSerializationForm
	case "multipart":
		return models.RequestSerializationMultipart
	case "text", "xml", "binary":
		return models.RequestSerializationRaw
	default:
		return ""
	}
}

func projectionSchema(contract *models.SchemaContract) *models.Schema {
	if contract == nil {
		return nil
	}
	projection := contract.Projection
	return &projection
}

func selectedBinaryEncoding(content *SelectedRequestRepresentation) string {
	if content != nil && content.Schema != nil && content.Schema.Format == "binary" {
		return models.RequestBinaryEncodingBase64
	}
	return ""
}
