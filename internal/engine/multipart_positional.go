package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"reflect"
	"strings"

	"github.com/Usefused/engine/internal/engine/streambody"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/models"
)

func hasPositionalMultipart(content *SelectedRequestRepresentation) bool {
	return len(content.PrefixEncoding) > 0 || content.ItemEncoding != nil
}

func buildPositionalMultipartRequestBody(content *SelectedRequestRepresentation, headers map[string]string, bodyParams map[string]any) (io.Reader, error) {
	return buildPositionalMultipartRequestBodyWithContext(context.Background(), content, headers, bodyParams)
}

// buildPositionalMultipartRequestBodyWithContext validates every part before
// starting its producer so contract errors cannot leave a partial wire body.
func buildPositionalMultipartRequestBodyWithContext(ctx context.Context, content *SelectedRequestRepresentation, headers map[string]string, bodyParams map[string]any) (io.Reader, error) {
	items, err := positionalRequestItems(content.PayloadParameter, bodyParams)
	if err != nil {
		return nil, err
	}
	formName, err := positionalFormDataName(content.MediaType, content.PayloadParameter)
	if err != nil {
		return nil, err
	}
	for index := range items {
		encoding, selectErr := positionalEncoding(content.PrefixEncoding, content.ItemEncoding, index)
		if selectErr != nil {
			return nil, selectErr
		}
		if err := validatePositionalEncoding(encoding, formName, content.PayloadParameter, 0); err != nil {
			return nil, err
		}
	}
	boundary := multipartReplayBoundary(ctx)
	if boundary == "" {
		boundary = multipart.NewWriter(io.Discard).Boundary()
	}
	contentType, err := multipartRequestContentType(content.MediaType, boundary)
	if err != nil {
		return nil, err
	}
	setAuthoritativeContentType(headers, contentType)
	return streambody.New(func(destination io.Writer) error {
		writer := multipart.NewWriter(destination)
		if err := writer.SetBoundary(boundary); err != nil {
			return errors.New("failed to set positional multipart boundary")
		}
		return writePositionalMultipart(writer, items, content.PrefixEncoding, content.ItemEncoding, content.ItemSchema, formName, content.PayloadParameter)
	}, positionalItemClosers(items, 0)...), nil
}

func positionalRequestItems(payloadParameter string, bodyParams map[string]any) ([]any, error) {
	name := strings.TrimSpace(payloadParameter)
	if name == "" || len(bodyParams) != 1 {
		return nil, errors.New("positional multipart requires one payload_parameter")
	}
	value, ok := bodyParams[name]
	if !ok {
		return nil, fmt.Errorf("missing positional multipart payload parameter %q", name)
	}
	return positionalItems(value)
}

func positionalItems(value any) ([]any, error) {
	raw := indirectQueryStringValue(reflect.ValueOf(value))
	if !raw.IsValid() || (raw.Kind() != reflect.Array && raw.Kind() != reflect.Slice) {
		return nil, errors.New("positional multipart payload must be an ordered array")
	}
	if _, bytesValue := value.([]byte); bytesValue {
		return nil, errors.New("positional multipart payload must contain multiple items")
	}
	items := make([]any, raw.Len())
	for index := range items {
		items[index] = raw.Index(index).Interface()
	}
	return items, nil
}

func positionalEncoding(prefix []models.RequestEncoding, item *models.RequestEncoding, index int) (models.RequestEncoding, error) {
	if index < len(prefix) {
		return prefix[index], nil
	}
	if item == nil {
		// OpenAPI applies Encoding Object defaults when no positional override is
		// present; the payload length must not be mistaken for a schema maxItems.
		return models.RequestEncoding{}, nil
	}
	return *item, nil
}

func writePositionalMultipart(writer *multipart.Writer, items []any, prefix []models.RequestEncoding, item *models.RequestEncoding, schema *models.SchemaContract, formName, payloadName string) error {
	for index, value := range items {
		encoding, _ := positionalEncoding(prefix, item, index)
		options := positionalPartOptions{schema: schemaProjection(schema), formName: formName, payloadName: payloadName}
		if err := writePositionalPart(writer, value, encoding, options); err != nil {
			return err
		}
	}
	return writer.Close()
}

type positionalPartOptions struct {
	schema      *models.Schema
	formName    string
	payloadName string
	depth       int
}

func writePositionalPart(writer *multipart.Writer, value any, encoding models.RequestEncoding, options positionalPartOptions) error {
	if len(encoding.Encoding) > 0 {
		return errors.New("named nested multipart encoding is unsupported")
	}
	if len(encoding.PrefixEncoding) > 0 || encoding.ItemEncoding != nil {
		return writeNestedPositionalPart(writer, value, encoding, options)
	}
	contentType := positionalPartContentType(options.schema, encoding.ContentType)
	header, err := positionalPartHeader(contentType, encoding.Headers, options.formName)
	if err != nil {
		return err
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return errors.New("failed to create positional multipart part")
	}
	return writePositionalValue(part, value, contentType, encoding.BinaryEncoding)
}

func hasNestedRequestEncoding(encoding models.RequestEncoding) bool {
	return len(encoding.Encoding) > 0 || len(encoding.PrefixEncoding) > 0 || encoding.ItemEncoding != nil
}

func writeNestedPositionalPart(writer *multipart.Writer, value any, encoding models.RequestEncoding, options positionalPartOptions) error {
	if options.depth >= 1 {
		return errors.New("nested multipart depth exceeds runtime limit")
	}
	items, err := positionalItems(value)
	if err != nil {
		return err
	}
	nested, err := newNestedPositionalWriter(writer, encoding, options.formName)
	if err != nil {
		return err
	}
	childFormName, err := positionalFormDataName(encoding.ContentType, options.payloadName)
	if err != nil {
		return err
	}
	for index, item := range items {
		child, selectErr := positionalEncoding(encoding.PrefixEncoding, encoding.ItemEncoding, index)
		if selectErr != nil {
			return selectErr
		}
		childOptions := positionalPartOptions{schema: nestedPositionalSchema(options.schema), formName: childFormName, payloadName: options.payloadName, depth: options.depth + 1}
		if err := writePositionalPart(nested, item, child, childOptions); err != nil {
			return err
		}
	}
	return nested.Close()
}

func newNestedPositionalWriter(writer *multipart.Writer, encoding models.RequestEncoding, formName string) (*multipart.Writer, error) {
	prototype := multipart.NewWriter(io.Discard)
	contentType, err := multipartRequestContentType(encoding.ContentType, prototype.Boundary())
	if err != nil {
		return nil, err
	}
	header, err := positionalPartHeader(contentType, encoding.Headers, formName)
	if err != nil {
		return nil, err
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, errors.New("failed to create nested multipart part")
	}
	nested := multipart.NewWriter(part)
	if err := nested.SetBoundary(prototype.Boundary()); err != nil {
		return nil, errors.New("failed to set nested multipart boundary")
	}
	return nested, nil
}

func positionalPartHeader(contentType string, headers map[string]models.HeaderContract, formName string) (textproto.MIMEHeader, error) {
	header := make(textproto.MIMEHeader)
	for name, contract := range headers {
		value, ok := staticEncodingHeaderValue(contract)
		if !ok {
			continue
		}
		if err := connectionprofile.ValidateResolvedHeader(name, value); err != nil {
			return nil, errors.New("positional multipart header is invalid")
		}
		header.Set(name, value)
	}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if err := ensureFormDataDisposition(header, formName); err != nil {
		return nil, err
	}
	return header, nil
}

// positionalPartContentType derives omitted media from the reviewed schema,
// never from the runtime Go value, so identical contracts frame identically.
func positionalPartContentType(schema *models.Schema, declared string) string {
	if declared != "" {
		return declared
	}
	if schema != nil && strings.EqualFold(schema.Format, "binary") {
		return "application/octet-stream"
	}
	if schema != nil && scalarSchemaType(schema.Type) {
		return "text/plain"
	}
	return "application/json"
}

func writePositionalValue(destination io.Writer, value any, contentType, binaryEncoding string) error {
	if binaryEncoding == models.RequestBinaryEncodingBase64 {
		encoded, ok := value.(string)
		if !ok {
			return errors.New("positional multipart base64 item must be a string")
		}
		_, err := io.Copy(destination, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded)))
		return err
	}
	if binaryEncoding != "" {
		return errors.New("positional multipart binary encoding is unsupported")
	}
	family, err := positionalValueFamily(contentType)
	if err != nil {
		return err
	}
	switch family {
	case "json":
		return json.NewEncoder(destination).Encode(value)
	case "text":
		return writePositionalText(destination, value)
	default:
		return writePositionalBinary(destination, value)
	}
}

func writePositionalBinary(destination io.Writer, value any) error {
	switch typed := value.(type) {
	case io.Reader:
		_, err := io.Copy(destination, typed)
		return err
	case []byte:
		_, err := destination.Write(typed)
		return err
	case string:
		_, err := io.WriteString(destination, typed)
		return err
	default:
		return errors.New("positional multipart binary item must be bytes, string, or reader")
	}
}

func schemaProjection(contract *models.SchemaContract) *models.Schema {
	if contract == nil {
		return nil
	}
	projection := contract.Projection
	return &projection
}

func scalarSchemaType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "boolean", "integer", "number", "string":
		return true
	default:
		return false
	}
}

func positionalValueFamily(contentType string) (string, error) {
	family, err := exactMediaFamily(contentType)
	if err != nil {
		return "", err
	}
	switch family {
	case "json":
		return "json", nil
	case "text", "xml":
		return "text", nil
	case "multipart":
		return "", errors.New("positional multipart part requires nested encoding")
	default:
		return "binary", nil
	}
}

func writePositionalText(destination io.Writer, value any) error {
	if payload, ok := value.([]byte); ok {
		_, err := destination.Write(payload)
		return err
	}
	raw := indirectQueryStringValue(reflect.ValueOf(value))
	if !raw.IsValid() || !scalarValueKind(raw.Kind()) {
		return errors.New("positional multipart text item must be scalar")
	}
	_, err := fmt.Fprint(destination, raw.Interface())
	return err
}

func scalarValueKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	default:
		return false
	}
}

func positionalFormDataName(mediaType, payloadParameter string) (string, error) {
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", errors.New("positional multipart media type is invalid")
	}
	if !strings.EqualFold(parsed, "multipart/form-data") {
		return "", nil
	}
	name := strings.TrimSpace(payloadParameter)
	if !validFormDataName(name) || mime.FormatMediaType("form-data", map[string]string{"name": name}) == "" {
		return "", errors.New("positional form-data requires a valid payload_parameter name")
	}
	return name, nil
}

func validFormDataName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func ensureFormDataDisposition(header textproto.MIMEHeader, formName string) error {
	if formName == "" {
		return nil
	}
	value := header.Get("Content-Disposition")
	if value == "" {
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": formName}))
		return nil
	}
	disposition, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(disposition, "form-data") || parameters["name"] != formName {
		return errors.New("positional form-data Content-Disposition must match payload_parameter")
	}
	return nil
}

func validatePositionalEncoding(encoding models.RequestEncoding, formName, payloadName string, depth int) error {
	if _, err := positionalPartHeader(encoding.ContentType, encoding.Headers, formName); err != nil {
		return err
	}
	if len(encoding.PrefixEncoding) == 0 && encoding.ItemEncoding == nil {
		return nil
	}
	if depth >= 1 {
		return errors.New("nested multipart depth exceeds runtime limit")
	}
	childName, err := positionalFormDataName(encoding.ContentType, payloadName)
	if err != nil {
		return err
	}
	for _, child := range encoding.PrefixEncoding {
		if err := validatePositionalEncoding(child, childName, payloadName, depth+1); err != nil {
			return err
		}
	}
	if encoding.ItemEncoding != nil {
		return validatePositionalEncoding(*encoding.ItemEncoding, childName, payloadName, depth+1)
	}
	return nil
}

func nestedPositionalSchema(schema *models.Schema) *models.Schema {
	if schema != nil && schema.Items != nil {
		return schema.Items
	}
	return schema
}

func positionalItemClosers(items []any, depth int) []io.Closer {
	closers := make([]io.Closer, 0)
	for _, value := range items {
		if closer, ok := value.(io.Closer); ok {
			closers = append(closers, closer)
			continue
		}
		if depth >= 1 {
			continue
		}
		nested, err := positionalItems(value)
		if err == nil {
			closers = append(closers, positionalItemClosers(nested, depth+1)...)
		}
	}
	return closers
}

func positionalMultipartMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.HasPrefix(strings.ToLower(mediaType), "multipart/")
}
