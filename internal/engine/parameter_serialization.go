package engine

import (
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/shared/models"
)

type queryValue struct {
	value         string
	allowReserved bool
}

type queryParameters map[string][]queryValue

func (q queryParameters) Has(name string) bool { _, ok := q[name]; return ok }

func (q queryParameters) Set(name, value string) {
	q[name] = []queryValue{{value: value}}
}

func (q queryParameters) Add(name, value string, allowReserved bool) {
	q[name] = append(q[name], queryValue{value: value, allowReserved: allowReserved})
}

func (q queryParameters) Encode() string {
	return q.encodeWith(encodeQueryComponent)
}

func (q queryParameters) EncodeForm() string {
	return q.encodeWith(encodeFormComponent)
}

func (q queryParameters) encodeWith(encode func(string, bool) string) string {
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(q))
	for _, name := range names {
		for _, value := range q[name] {
			parts = append(parts, encode(name, false)+"="+encode(value.value, value.allowReserved))
		}
	}
	return strings.Join(parts, "&")
}

func encodeFormComponent(value string, allowReserved bool) string {
	encoded := neturl.QueryEscape(value)
	if !allowReserved {
		return encoded
	}
	for escaped, literal := range safeQueryReserved {
		// In form encoding, a literal plus represents a space. Keeping it escaped
		// preserves the caller's exact value even when reserved characters are allowed.
		if escaped != "%2B" {
			encoded = strings.ReplaceAll(encoded, escaped, literal)
		}
	}
	return encoded
}

// Delimiters that can change query structure stay escaped even with
// allowReserved. This preserves the OpenAPI intent without permitting a value
// to inject another parameter or a fragment into the provider request.
func encodeQueryComponent(value string, allowReserved bool) string {
	encoded := strings.ReplaceAll(neturl.QueryEscape(value), "+", "%20")
	if !allowReserved {
		return encoded
	}
	for escaped, literal := range safeQueryReserved {
		encoded = strings.ReplaceAll(encoded, escaped, literal)
	}
	return encoded
}

var safeQueryReserved = map[string]string{
	"%3A": ":", "%2F": "/", "%3F": "?", "%5B": "[", "%5D": "]", "%40": "@",
	"%21": "!", "%24": "$", "%27": "'", "%28": "(", "%29": ")", "%2A": "*",
	"%2B": "+", "%2C": ",", "%3B": ";",
}

type parameterParts struct {
	scalar string
	array  []string
	object map[string]string
}

func splitParameterValue(value any) (parameterParts, error) {
	if value == nil {
		return parameterParts{scalar: ""}, nil
	}
	raw := reflect.ValueOf(value)
	for raw.Kind() == reflect.Pointer || raw.Kind() == reflect.Interface {
		if raw.IsNil() {
			return parameterParts{scalar: ""}, nil
		}
		raw = raw.Elem()
	}
	switch raw.Kind() {
	case reflect.Array, reflect.Slice:
		values := make([]string, raw.Len())
		for i := 0; i < raw.Len(); i++ {
			values[i] = fmt.Sprint(raw.Index(i).Interface())
		}
		return parameterParts{array: values}, nil
	case reflect.Map:
		return splitParameterObject(raw)
	default:
		return parameterParts{scalar: fmt.Sprint(value)}, nil
	}
}

func splitParameterObject(raw reflect.Value) (parameterParts, error) {
	if raw.Type().Key().Kind() != reflect.String {
		return parameterParts{}, errors.New("OpenAPI parameter objects require string keys")
	}
	values := make(map[string]string, raw.Len())
	iterator := raw.MapRange()
	for iterator.Next() {
		values[iterator.Key().String()] = fmt.Sprint(iterator.Value().Interface())
	}
	return parameterParts{object: values}, nil
}

func effectiveParameterStyle(parameter models.Parameter) string {
	if parameter.Serialization.Style != "" {
		return parameter.Serialization.Style
	}
	if parameter.In == "query" || parameter.In == "cookie" {
		return "form"
	}
	return "simple"
}

func effectiveParameterExplode(parameter models.Parameter) bool {
	if parameter.Serialization.Explode != nil {
		return *parameter.Serialization.Explode
	}
	style := effectiveParameterStyle(parameter)
	return style == "form" || style == "cookie"
}

func serializeQueryParameter(parameter models.Parameter, value any, query queryParameters) error {
	parts, err := splitParameterValue(value)
	if err != nil {
		return err
	}
	allowReserved := boolValue(parameter.Serialization.AllowReserved)
	switch effectiveParameterStyle(parameter) {
	case "deepObject":
		return addDeepObjectQuery(parameter.Name, parts, query, allowReserved)
	case "spaceDelimited":
		return addDelimitedQuery(parameter.Name, parts, " ", query, allowReserved)
	case "pipeDelimited":
		return addDelimitedQuery(parameter.Name, parts, "|", query, allowReserved)
	default:
		return addFormQuery(parameter.Name, parts, effectiveParameterExplode(parameter), query, allowReserved)
	}
}

func addFormQuery(name string, parts parameterParts, explode bool, query queryParameters, allowReserved bool) error {
	if parts.array != nil {
		if explode {
			for _, value := range parts.array {
				query.Add(name, value, allowReserved)
			}
			return nil
		}
		query.Add(name, strings.Join(parts.array, ","), allowReserved)
		return nil
	}
	if parts.object != nil {
		return addFormObject(name, parts.object, explode, query, allowReserved)
	}
	query.Add(name, parts.scalar, allowReserved)
	return nil
}

func addFormObject(name string, object map[string]string, explode bool, query queryParameters, allowReserved bool) error {
	keys := serializationSortedKeys(object)
	if explode {
		for _, key := range keys {
			query.Add(key, object[key], allowReserved)
		}
		return nil
	}
	query.Add(name, flattenedObject(object, ",", ","), allowReserved)
	return nil
}

func addDelimitedQuery(name string, parts parameterParts, delimiter string, query queryParameters, allowReserved bool) error {
	if parts.object != nil {
		query.Add(name, flattenedObject(parts.object, delimiter, delimiter), allowReserved)
		return nil
	}
	if parts.array != nil {
		query.Add(name, strings.Join(parts.array, delimiter), allowReserved)
		return nil
	}
	query.Add(name, parts.scalar, allowReserved)
	return nil
}

func addDeepObjectQuery(name string, parts parameterParts, query queryParameters, allowReserved bool) error {
	if parts.object == nil {
		return errors.New("deepObject parameter requires an object")
	}
	for _, key := range serializationSortedKeys(parts.object) {
		query.Add(name+"["+key+"]", parts.object[key], allowReserved)
	}
	return nil
}

func serializeSimpleValue(parts parameterParts, explode bool) string {
	if parts.array != nil {
		return strings.Join(parts.array, ",")
	}
	if parts.object != nil {
		if explode {
			return flattenedObject(parts.object, "=", ",")
		}
		return flattenedObject(parts.object, ",", ",")
	}
	return parts.scalar
}

func serializePathParameter(parameter models.Parameter, value any) (string, error) {
	parts, err := splitParameterValue(value)
	if err != nil {
		return "", err
	}
	explode := effectiveParameterExplode(parameter)
	allowReserved := boolValue(parameter.Serialization.AllowReserved)
	switch effectiveParameterStyle(parameter) {
	case "label":
		return serializeLabelPath(parts, explode, allowReserved), nil
	case "matrix":
		return serializeMatrixPath(parameter.Name, parts, explode, allowReserved), nil
	default:
		return encodePathParameterValue(serializeSimpleValue(parts, explode), parameter.PathEncoding, allowReserved), nil
	}
}

func serializeLabelPath(parts parameterParts, explode, allowReserved bool) string {
	delimiter := ","
	if explode {
		delimiter = "."
	}
	return "." + encodePathParameterValue(serializeDelimitedValue(parts, explode, delimiter), "", allowReserved)
}

func serializeMatrixPath(name string, parts parameterParts, explode, allowReserved bool) string {
	if parts.object != nil && explode {
		segments := make([]string, 0, len(parts.object))
		for _, key := range serializationSortedKeys(parts.object) {
			segments = append(segments, ";"+neturl.PathEscape(key)+"="+encodePathParameterValue(parts.object[key], "", allowReserved))
		}
		return strings.Join(segments, "")
	}
	if parts.array != nil && explode {
		segments := make([]string, len(parts.array))
		for i, value := range parts.array {
			segments[i] = ";" + neturl.PathEscape(name) + "=" + encodePathParameterValue(value, "", allowReserved)
		}
		return strings.Join(segments, "")
	}
	return ";" + neturl.PathEscape(name) + "=" + encodePathParameterValue(serializeDelimitedValue(parts, false, ","), "", allowReserved)
}

func serializeDelimitedValue(parts parameterParts, explode bool, delimiter string) string {
	if parts.array != nil {
		return strings.Join(parts.array, delimiter)
	}
	if parts.object != nil {
		pairDelimiter := delimiter
		if explode {
			pairDelimiter = "="
		}
		return flattenedObject(parts.object, pairDelimiter, delimiter)
	}
	return parts.scalar
}

func serializeHeaderParameter(parameter models.Parameter, value any) (string, error) {
	parts, err := splitParameterValue(value)
	if err != nil {
		return "", err
	}
	serialized := serializeSimpleValue(parts, effectiveParameterExplode(parameter))
	if strings.ContainsAny(serialized, "\r\n") {
		return "", errors.New("OpenAPI header parameter contains invalid characters")
	}
	return serialized, nil
}

func serializeCookieParameter(parameter models.Parameter, value any, cookies map[string]string) error {
	parts, err := splitParameterValue(value)
	if err != nil {
		return err
	}
	if effectiveParameterStyle(parameter) == "cookie" {
		return addRawCookieValue(parameter.Name, parts, effectiveParameterExplode(parameter), cookies)
	}
	return addFormCookieValue(parameter.Name, parts, effectiveParameterExplode(parameter), boolValue(parameter.Serialization.AllowReserved), cookies)
}

func addFormCookieValue(name string, parts parameterParts, explode, allowReserved bool, cookies map[string]string) error {
	if parts.object != nil && explode {
		for _, key := range serializationSortedKeys(parts.object) {
			if err := addCookie(cookies, key, parts.object[key], allowReserved); err != nil {
				return err
			}
		}
		return nil
	}
	return addCookie(cookies, name, serializeSimpleValue(parts, false), allowReserved)
}

func addCookie(cookies map[string]string, name, value string, allowReserved bool) error {
	if _, exists := cookies[name]; exists {
		return errors.New("OpenAPI cookie parameter collision")
	}
	pair, err := rawCookiePair(name, encodeQueryComponent(value, allowReserved))
	if err != nil {
		return err
	}
	cookies[name] = pair
	return nil
}

func addRawCookieValue(name string, parts parameterParts, explode bool, cookies map[string]string) error {
	if parts.object != nil && explode {
		for _, key := range serializationSortedKeys(parts.object) {
			if err := addRawCookie(cookies, key, parts.object[key]); err != nil {
				return err
			}
		}
		return nil
	}
	if parts.array != nil && explode {
		pairs := make([]string, len(parts.array))
		for index, value := range parts.array {
			pair, err := rawCookiePair(name, value)
			if err != nil {
				return err
			}
			pairs[index] = pair
		}
		cookies[name] = strings.Join(pairs, "; ")
		return nil
	}
	return addRawCookie(cookies, name, serializeSimpleValue(parts, false))
}

func addRawCookie(cookies map[string]string, name, value string) error {
	if _, exists := cookies[name]; exists {
		return errors.New("OpenAPI cookie parameter collision")
	}
	pair, err := rawCookiePair(name, value)
	if err != nil {
		return err
	}
	cookies[name] = pair
	return nil
}

func rawCookiePair(name, value string) (string, error) {
	if err := (&http.Cookie{Name: name, Value: value}).Valid(); err != nil {
		return "", errors.New("OpenAPI cookie parameter is invalid")
	}
	return name + "=" + value, nil
}

func cookieHeader(cookies map[string]string) string {
	keys := serializationSortedKeys(cookies)
	values := make([]string, len(keys))
	for i, key := range keys {
		values[i] = cookies[key]
	}
	return strings.Join(values, "; ")
}

func flattenedObject(object map[string]string, pairDelimiter, itemDelimiter string) string {
	items := make([]string, 0, len(object))
	for _, key := range serializationSortedKeys(object) {
		items = append(items, key+pairDelimiter+object[key])
	}
	return strings.Join(items, itemDelimiter)
}

func serializationSortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func boolValue(value *bool) bool { return value != nil && *value }
