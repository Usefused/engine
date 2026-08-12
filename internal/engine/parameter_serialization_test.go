package engine

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestPrepareRequestPartsSerializesOpenAPIParameters(t *testing.T) {
	explode := true
	allowReserved := true
	object := models.Parameter{
		Name: "filter", In: "query",
		Serialization: models.ParameterSerialization{Style: "deepObject", Explode: &explode},
	}
	reserved := models.Parameter{
		Name: "next", In: "query",
		Serialization: models.ParameterSerialization{Style: "form", AllowReserved: &allowReserved},
	}
	cookie := models.Parameter{Name: "prefs", In: "cookie", Serialization: models.ParameterSerialization{Style: "form", Explode: &explode}}
	requestURL, headers, _, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "GET", Path: "/items", Parameters: models.Parameters{object, reserved, cookie}},
		map[string]any{
			"filter": map[string]any{"state": "open", "owner": "me"},
			"next":   "https://next.example/a?x=1&admin=true#fragment",
			"prefs":  map[string]any{"theme": "dark", "view": "full"},
		}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	wantQuery := "filter%5Bowner%5D=me&filter%5Bstate%5D=open&next=https://next.example/a?x%3D1%26admin%3Dtrue%23fragment"
	if !strings.HasSuffix(requestURL, "?"+wantQuery) {
		t.Fatalf("request URL = %q, want query %q", requestURL, wantQuery)
	}
	if headers["Cookie"] != "theme=dark; view=full" {
		t.Fatalf("Cookie = %q", headers["Cookie"])
	}
}

func TestPrepareRequestPartsSerializesPathStyles(t *testing.T) {
	explode := true
	tests := []struct {
		name      string
		parameter models.Parameter
		value     any
		want      string
	}{
		{name: "label array", parameter: models.Parameter{Name: "ids", In: "path", Serialization: models.ParameterSerialization{Style: "label", Explode: &explode}}, value: []string{"a", "b"}, want: "/items/.a.b"},
		{name: "matrix object", parameter: models.Parameter{Name: "coords", In: "path", Serialization: models.ParameterSerialization{Style: "matrix", Explode: &explode}}, value: map[string]any{"x": 1, "y": 2}, want: "/items/;x=1;y=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, _, err := prepareRequestParts(
				&models.Service{BaseURL: "https://api.example.com"},
				&models.IntegrationObject{Method: "GET", Path: "/items/{" + test.parameter.Name + "}", Parameters: models.Parameters{test.parameter}},
				map[string]any{test.parameter.Name: test.value}, nil,
			)
			if err != nil {
				t.Fatalf("prepareRequestParts: %v", err)
			}
			if got != "https://api.example.com"+test.want {
				t.Fatalf("URL = %q, want %q", got, "https://api.example.com"+test.want)
			}
		})
	}
}

func TestPrepareRequestPartsSerializesOpenAPI32Parameters(t *testing.T) {
	allowReserved := true
	explode := true
	operation := &models.IntegrationObject{Method: "QUERY", Path: "/items/{scope}", Parameters: models.Parameters{
		{Name: "scope", In: "path", Required: true, Serialization: models.ParameterSerialization{AllowReserved: &allowReserved}},
		{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{"application/x-www-form-urlencoded": {}}},
		{Name: "prefs", In: "cookie", Serialization: models.ParameterSerialization{Style: "cookie", Explode: &explode}},
		{Name: "If-Match", In: "header", Serialization: models.ParameterSerialization{AllowReserved: &allowReserved}},
	}}
	requestURL, headers, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, operation, map[string]any{
		"scope": "team:blue/files", "filter": map[string]any{"q": "a + b", "active": true},
		"prefs": map[string]any{"region": "eu", "theme": "dark"}, "If-Match": `W/"safe"`,
	}, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	if requestURL != "https://api.example.com/items/team:blue%2Ffiles?active=true&q=a+%2B+b" {
		t.Fatalf("request URL = %q", requestURL)
	}
	if headers["Cookie"] != "region=eu; theme=dark" || headers["If-Match"] != `W/"safe"` {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestPrepareRequestPartsAlwaysBindsPathBeforeWholeQuery(t *testing.T) {
	path := models.Parameter{Name: "scope", In: "path", Required: true}
	query := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{"application/x-www-form-urlencoded": {}}}
	for _, definitions := range []models.Parameters{{path, query}, {query, path}} {
		operation := &models.IntegrationObject{Method: "QUERY", Path: "/items/{scope}", Parameters: definitions}
		for iteration := 0; iteration < 256; iteration++ {
			requestURL, _, _, err := prepareRequestParts(
				&models.Service{BaseURL: "https://api.example.com"}, operation,
				map[string]any{"filter": map[string]any{"active": true}, "scope": "team blue"}, nil,
			)
			if err != nil {
				t.Fatalf("definitions %#v iteration %d: prepareRequestParts: %v", definitions, iteration, err)
			}
			if requestURL != "https://api.example.com/items/team%20blue?active=true" {
				t.Fatalf("definitions %#v iteration %d: request URL = %q", definitions, iteration, requestURL)
			}
		}
	}
}

func TestPrepareRequestPartsUsesFormEscapingForQuerystringPropertyEncoding(t *testing.T) {
	parameter := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{
		"application/x-www-form-urlencoded": {Encoding: map[string]models.RequestEncoding{
			"q": {ContentType: "text/plain"},
		}},
	}}
	requestURL, _, _, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "QUERY", Path: "/items", Parameters: models.Parameters{parameter}},
		map[string]any{"filter": map[string]any{"q": "a + b"}}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	if requestURL != "https://api.example.com/items?q=a+%2B+b" {
		t.Fatalf("request URL = %q", requestURL)
	}
}

func TestPartialQuerystringEncodingLeavesUnlistedPropertiesOnDefaults(t *testing.T) {
	parameter := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{
		"application/x-www-form-urlencoded": {Encoding: map[string]models.RequestEncoding{
			"encoded": {ContentType: "text/plain"},
		}},
	}}
	requestURL, _, _, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "QUERY", Path: "/items", Parameters: models.Parameters{parameter}},
		map[string]any{"filter": map[string]any{
			"encoded": "a b", "unlisted": map[string]any{"z": 1, "a": 2}, "list": []string{"x", "y"}, "active": false,
		}}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	want := "https://api.example.com/items?active=false&encoded=a+b&list=x&list=y&unlisted=%7B%22a%22%3A2%2C%22z%22%3A1%7D"
	if requestURL != want {
		t.Fatalf("request URL = %q, want %q", requestURL, want)
	}
}

func TestWholeQueryStringSupportedMediaProduceExactURLs(t *testing.T) {
	tests := []struct {
		mediaType string
		value     any
		want      string
	}{
		{mediaType: "application/x-www-form-urlencoded; charset=utf-8", value: map[string]any{"q": "a + b", "active": false}, want: "https://api.example.com/items?active=false&q=a+%2B+b"},
		{mediaType: "application/json", value: map[string]any{"q": "a + b", "active": false}, want: "https://api.example.com/items?%7B%22active%22%3Afalse%2C%22q%22%3A%22a%20%2B%20b%22%7D"},
		{mediaType: "application/jsonpath", value: `$.items[?(@.name == "a+b")]`, want: "https://api.example.com/items?%24.items%5B%3F%28%40.name%20%3D%3D%20%22a%2Bb%22%29%5D"},
		{mediaType: "text/plain; charset=utf-8", value: "a + b", want: "https://api.example.com/items?a%20%2B%20b"},
	}
	for _, test := range tests {
		t.Run(test.mediaType, func(t *testing.T) {
			parameter := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{test.mediaType: {}}}
			got, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"},
				&models.IntegrationObject{Method: "QUERY", Path: "/items", Parameters: models.Parameters{parameter}},
				map[string]any{"filter": test.value}, nil)
			if err != nil || got != test.want {
				t.Fatalf("URL = %q err=%v, want %q", got, err, test.want)
			}
		})
	}
}

func TestWholeQueryStringRejectsUnsupportedMediaBeforeURLMutation(t *testing.T) {
	parameter := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{"application/xml": {}}}
	got, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "QUERY", Path: "/items", Parameters: models.Parameters{parameter}},
		map[string]any{"filter": "<secret/>"}, nil)
	if err == nil || got != "" || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("URL = %q err=%v", got, err)
	}
}

func FuzzSerializeFormQueryStringPartialEncoding(f *testing.F) {
	f.Add("a b", "x+y")
	f.Add("", "false")
	f.Fuzz(func(t *testing.T, encoded, unlisted string) {
		value := map[string]any{"encoded": encoded, "unlisted": unlisted}
		encodings := map[string]models.RequestEncoding{"encoded": {ContentType: "text/plain"}}
		first, firstErr := serializeFormQueryString(value, encodings)
		second, secondErr := serializeFormQueryString(value, encodings)
		if (firstErr == nil) != (secondErr == nil) || first != second {
			t.Fatalf("serialization was nondeterministic: %q/%v != %q/%v", first, firstErr, second, secondErr)
		}
		defaults, defaultsErr := serializeFormQueryString(map[string]any{"unlisted": unlisted}, nil)
		if firstErr != nil || defaultsErr != nil {
			return
		}
		firstValues, firstParseErr := url.ParseQuery(first)
		defaultValues, defaultParseErr := url.ParseQuery(defaults)
		if firstParseErr != nil || defaultParseErr != nil || strings.Join(firstValues["unlisted"], "\x00") != strings.Join(defaultValues["unlisted"], "\x00") {
			t.Fatalf("partial encoding changed unlisted default: partial=%q default=%q", first, defaults)
		}
	})
}

func TestPrepareRequestPartsRejectsQuerystringMixingAndHeaderInjection(t *testing.T) {
	queryString := models.Parameter{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{"application/json": {}}}
	query := models.Parameter{Name: "page", In: "query"}
	_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, &models.IntegrationObject{
		Method: "GET", Path: "/items", Parameters: models.Parameters{queryString, query},
	}, map[string]any{"filter": map[string]any{"active": true}, "page": 2}, nil)
	if err == nil {
		t.Fatal("querystring mixed with query parameter")
	}
	header := models.Parameter{Name: "X-Test", In: "header"}
	_, _, _, err = prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, &models.IntegrationObject{
		Method: "GET", Path: "/items", Parameters: models.Parameters{header},
	}, map[string]any{"X-Test": "safe\r\nX-Evil: yes"}, nil)
	if err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestPrepareProviderRequestRejectsOrdinaryCookieAuthCollision(t *testing.T) {
	obj := &models.IntegrationObject{
		Method: "GET", Path: "/items",
		Parameters: models.Parameters{{Name: "session", In: "cookie"}},
	}
	auths := models.AuthConfigs{{Name: "key", Type: "apiKey", Location: "cookie", KeyName: "session"}}
	_, err := prepareProviderRequest(context.Background(), &models.Service{BaseURL: "https://api.example.com"}, obj,
		map[string]any{"session": "caller"}, map[string]any{"key": "secret"}, nil, auths, trace.SpanFromContext(context.Background()))
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v, want cookie collision", err)
	}
}

func TestPrepareProviderRequestRecordsOnlyBoundedOpenAPIAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := provider.Tracer("test").Start(context.Background(), "provider")
	obj := &models.IntegrationObject{
		Method: "GET", Path: "/items",
		Parameters: models.Parameters{{Name: "region", In: "query"}},
	}
	service := &models.Service{BaseURL: "https://tenant.example.com", ServerSource: "operation"}
	if _, err := prepareProviderRequest(context.Background(), service, obj, map[string]any{"region": "private-eu"}, nil, nil, nil, span); err != nil {
		t.Fatalf("prepareProviderRequest: %v", err)
	}
	span.End()
	attributes := safeStringSpanAttributes(t, recorder.Ended()[0])
	if attributes["http.server.source"] != "operation" || attributes["http.parameter_serialization.outcome"] != "success" {
		t.Fatalf("attributes = %#v", attributes)
	}
	serialized := fmt.Sprint(attributes)
	for _, secret := range []string{"tenant.example.com", "private-eu", "https://"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("telemetry leaked %q: %s", secret, serialized)
		}
	}
}
