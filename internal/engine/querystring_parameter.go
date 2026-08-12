package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"reflect"
	"strings"

	"github.com/Usefused/engine/internal/shared/models"
)

// serializeWholeQueryString treats querystring as exclusive ownership of the
// complete query so ordinary query parameters cannot create ambiguous wire data.
func serializeWholeQueryString(parameter models.Parameter, value any) (string, error) {
	mediaType, representation, err := parameterContentRepresentation(parameter.Content)
	if err != nil {
		return "", err
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		return serializeFormQueryString(value, representation.Encoding)
	case "application/json":
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return "", errors.New("querystring JSON value is invalid")
		}
		return encodeQueryComponent(string(payload), false), nil
	case "text/plain", "application/jsonpath":
		text, ok := value.(string)
		if !ok {
			return "", errors.New("querystring textual value must be a string")
		}
		return encodeQueryComponent(text, false), nil
	default:
		return "", fmt.Errorf("querystring media type %q is unsupported", mediaType)
	}
}

func parameterContentRepresentation(content map[string]models.ParameterContent) (string, models.ParameterContent, error) {
	if len(content) != 1 {
		return "", models.ParameterContent{}, errors.New("querystring parameter requires one content media type")
	}
	for value, representation := range content {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil {
			return "", models.ParameterContent{}, errors.New("querystring parameter media type is invalid")
		}
		return strings.ToLower(mediaType), representation, nil
	}
	return "", models.ParameterContent{}, errors.New("querystring parameter requires content")
}

func serializeFormQueryString(value any, encodings map[string]models.RequestEncoding) (string, error) {
	raw := indirectQueryStringValue(reflect.ValueOf(value))
	if !raw.IsValid() || raw.Kind() != reflect.Map || raw.Type().Key().Kind() != reflect.String {
		return "", errors.New("form querystring value must be an object")
	}
	return serializeReviewedFormQueryString(raw, encodings)
}

func serializeReviewedFormQueryString(raw reflect.Value, encodings map[string]models.RequestEncoding) (string, error) {
	values := queryParameters{}
	iterator := raw.MapRange()
	for iterator.Next() {
		name := iterator.Key().String()
		value := iterator.Value().Interface()
		encoding, reviewed := encodings[name]
		if !reviewed {
			// A partial encoding map overrides only named properties. Unlisted
			// properties retain the same form defaults as if no map existed.
			if err := addDefaultFormQueryStringValue(values, name, value); err != nil {
				return "", err
			}
			continue
		}
		if err := addReviewedQueryStringProperty(values, name, value, encoding); err != nil {
			return "", err
		}
	}
	return values.EncodeForm(), nil
}

func addReviewedQueryStringProperty(values queryParameters, name string, value any, encoding models.RequestEncoding) error {
	if hasQueryStringSerialization(encoding) {
		return serializeReviewedQueryStringValue(values, name, value, encoding)
	}
	mediaType, _, err := mime.ParseMediaType(encoding.ContentType)
	if encoding.ContentType == "" || (err == nil && mediaType == "text/plain") {
		return addDefaultReviewedQueryStringValue(values, name, value)
	}
	if err != nil || mediaType != "application/json" {
		return errors.New("querystring property content type is unsupported")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("querystring JSON property is invalid")
	}
	values.Add(name, string(payload), false)
	return nil
}

func hasQueryStringSerialization(encoding models.RequestEncoding) bool {
	return encoding.Style != "" || encoding.Explode != nil || encoding.AllowReserved != nil
}

func serializeReviewedQueryStringValue(values queryParameters, name string, value any, encoding models.RequestEncoding) error {
	parameter := models.Parameter{Name: name, In: "query", Serialization: models.ParameterSerialization{
		Style: encoding.Style, Explode: encoding.Explode, AllowReserved: encoding.AllowReserved,
	}}
	return serializeQueryParameter(parameter, value, values)
}

func addDefaultReviewedQueryStringValue(values queryParameters, name string, value any) error {
	return addDefaultFormQueryStringValue(values, name, value)
}

func addDefaultFormQueryStringValue(values queryParameters, name string, value any) error {
	parts, err := splitParameterValue(value)
	if err != nil {
		return err
	}
	if parts.array != nil {
		for _, item := range parts.array {
			values.Add(name, item, false)
		}
		return nil
	}
	if parts.object != nil {
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return errors.New("form querystring object property is invalid")
		}
		values.Add(name, string(payload), false)
		return nil
	}
	values.Add(name, parts.scalar, false)
	return nil
}

func indirectQueryStringValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

// appendWholeQueryString runs only after path substitution; parsing earlier
// would escape unresolved braces and make the declared path parameter unusable.
func appendWholeQueryString(requestURL, rawQuery string) (string, error) {
	parsed, err := url.Parse(requestURL)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("querystring parameter conflicts with the request URL")
	}
	parsed.RawQuery = rawQuery
	return parsed.String(), nil
}

func hasWholeQueryStringParameter(parameters models.Parameters) bool {
	for _, parameter := range parameters {
		if strings.EqualFold(parameter.In, "querystring") {
			return true
		}
	}
	return false
}
