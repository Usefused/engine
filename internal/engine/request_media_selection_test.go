package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSelectRequestContentResolvesBundledSchemaDeclarations(t *testing.T) {
	content := &models.RequestContent{Representations: []models.RequestRepresentation{{
		MediaType: "application/json", Serialization: models.RequestSerializationJSON,
		Schema: &models.SchemaContract{
			Raw:        json.RawMessage(`{"$ref":"#/$defs/Payload","$defs":{"Payload":{"type":"object","properties":{"label":{"type":"string"}},"required":["label"]}}}`),
			Projection: models.Schema{Ref: "#/components/schemas/Payload"},
		},
	}}}
	selected, _, err := SelectRequestContent(content)
	if err != nil {
		t.Fatalf("SelectRequestContent: %v", err)
	}
	if err := ValidateDeclaredExecutionParameters(nil, selected, map[string]any{"label": "external"}); err != nil {
		t.Fatalf("bundled schema property was rejected: %v", err)
	}
	if err := ValidateDeclaredExecutionParameters(nil, selected, map[string]any{"unknown": true}); err == nil {
		t.Fatal("undeclared bundled-schema property was accepted")
	}
}

func TestSelectRequestContentUsesReviewedVendorJSONDefault(t *testing.T) {
	content := &models.RequestContent{
		Required: true, DefaultMediaType: "application/vnd.api+json",
		Representations: []models.RequestRepresentation{
			{MediaType: "text/plain", Serialization: models.RequestSerializationRaw},
			{MediaType: "application/vnd.api+json", Serialization: models.RequestSerializationJSON, Schema: &models.SchemaContract{Projection: models.Schema{Type: "object", AdditionalProperties: &models.Schema{}}}},
		},
	}
	selected, outcome, err := SelectRequestContent(content)
	if err != nil {
		t.Fatalf("SelectRequestContent: %v", err)
	}
	if outcome != requestMediaSelectionDefault || selected.MediaType != "application/vnd.api+json" || selected.Serialization != models.RequestSerializationJSON {
		t.Fatalf("selected=%#v outcome=%q", selected, outcome)
	}
	_, headers, body, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "POST", Path: "/items", RequestContent: content},
		map[string]any{"name": "widget"}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	payload, _ := io.ReadAll(body)
	if headers["Content-Type"] != "application/vnd.api+json" || string(payload) != `{"name":"widget"}` {
		t.Fatalf("headers=%#v payload=%s", headers, payload)
	}
}

func TestMediaSelectionTelemetryIsBoundedAndSecretSafe(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previousProvider)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.private+json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	content := &models.RequestContent{
		DefaultMediaType: "application/vnd.private+json",
		Representations: []models.RequestRepresentation{
			{MediaType: "text/plain", Serialization: models.RequestSerializationRaw},
			{MediaType: "application/vnd.private+json", Serialization: models.RequestSerializationJSON, Schema: &models.SchemaContract{Projection: models.Schema{Type: "object", AdditionalProperties: &models.Schema{}}}},
		},
	}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{
		Method: "POST", Path: "/items", RequestContent: content,
		Responses: models.Responses{
			"200": {Representations: []models.ResponseRepresentation{{MediaType: "application/vnd.private+json"}}},
			"201": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream", SSE: &models.SSEResponseContract{ItemMode: "data", DoneSentinel: stringPointer("tenant-secret-sentinel")}}}},
		},
	})
	ctx, executionSpan := provider.Tracer("test").Start(context.Background(), "engine.execution")
	if _, err := NewDispatcher().ExecuteStream(ctx, &models.Service{BaseURL: server.URL}, operation, map[string]any{"private": "value"}, nil, nil, &mockStream{}); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	executionSpan.End()
	executionAttrs := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.execution"))
	if executionAttrs["request.media_family"] != "json" || executionAttrs["request.media_selection.outcome"] != "reviewed_default" {
		t.Fatalf("execution attributes = %#v", executionAttrs)
	}
	providerAttrs := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.dispatch.vendor_call"))
	if providerAttrs["response.media_family"] != "json" || providerAttrs["response.media_selection.outcome"] != "matched" {
		t.Fatalf("provider attributes = %#v", providerAttrs)
	}
	serialized := fmt.Sprint(executionAttrs, providerAttrs)
	for _, secret := range []string{"vnd.private", "private", "value", "tenant-secret-sentinel", server.URL} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("telemetry leaked %q: %s", secret, serialized)
		}
	}
}

func TestSelectRequestContentRejectsAmbiguousRepresentations(t *testing.T) {
	content := &models.RequestContent{Representations: []models.RequestRepresentation{
		{MediaType: "application/json", Serialization: models.RequestSerializationJSON},
		{MediaType: "application/xml", Serialization: models.RequestSerializationRaw},
	}}
	if _, outcome, err := SelectRequestContent(content); err == nil || outcome != requestMediaSelectionReject {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
}

func TestSelectRequestContentRejectsInferredCanonicalSerialization(t *testing.T) {
	content := &models.RequestContent{Representations: []models.RequestRepresentation{{MediaType: "application/json"}}}
	if _, outcome, err := SelectRequestContent(content); err == nil || outcome != requestMediaSelectionReject {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
}

func TestFormEncodingUsesOpenAPISerialization(t *testing.T) {
	explode := true
	content := &models.RequestContent{
		Representations: []models.RequestRepresentation{{
			MediaType:     "application/x-www-form-urlencoded",
			Serialization: models.RequestSerializationForm,
			Encoding: map[string]models.RequestEncoding{
				"tags":   {Style: "form", Explode: &explode},
				"filter": {Style: "deepObject", Explode: &explode},
				"note":   {ContentType: "text/plain"},
			},
		}},
	}
	_, _, body, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "POST", Path: "/items", RequestContent: content},
		map[string]any{"tags": []string{"a", "b"}, "filter": map[string]any{"state": "open"}, "note": "a + b"}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	payload, _ := io.ReadAll(body)
	if string(payload) != "filter%5Bstate%5D=open&note=a+%2B+b&tags=a&tags=b" {
		t.Fatalf("form payload = %q", payload)
	}
}

func TestRequestBodyWritersRejectNamedNestedEncoding(t *testing.T) {
	nested := models.RequestEncoding{ContentType: "multipart/mixed", PrefixEncoding: []models.RequestEncoding{{ContentType: "text/plain"}}}
	form := &SelectedRequestRepresentation{Encoding: map[string]models.RequestEncoding{"payload": nested}}
	if _, err := buildFormRequestBody(form, map[string]any{"payload": []string{"one"}}); err == nil {
		t.Fatal("form writer accepted named nested encoding")
	}
	multipartContent := &SelectedRequestRepresentation{MediaType: "multipart/form-data", Encoding: map[string]models.RequestEncoding{"payload": nested}}
	if _, err := buildMultipartRequestBody(multipartContent, map[string]string{}, map[string]any{"payload": []string{"one"}}); err == nil {
		t.Fatal("multipart writer accepted named nested encoding")
	}
}

func TestMultipartEncodingAppliesReviewedPartMetadata(t *testing.T) {
	content := &models.RequestContent{Representations: []models.RequestRepresentation{{
		MediaType:     "multipart/form-data",
		Serialization: models.RequestSerializationMultipart,
		Encoding: map[string]models.RequestEncoding{"file": {
			ContentType: "text/plain",
			Headers:     map[string]models.HeaderContract{"X-Part-Kind": {Example: "reviewed"}},
		}},
	}}}
	_, headers, body, err := prepareRequestParts(
		&models.Service{BaseURL: "https://api.example.com"},
		&models.IntegrationObject{Method: "POST", Path: "/upload", RequestContent: content},
		map[string]any{"file": "hello"}, nil,
	)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	_, parameters, err := mime.ParseMediaType(headers["Content-Type"])
	if err != nil {
		t.Fatalf("multipart content type: %v", err)
	}
	part, err := multipart.NewReader(body, parameters["boundary"]).NextPart()
	if err != nil {
		t.Fatalf("read multipart part: %v", err)
	}
	payload, _ := io.ReadAll(part)
	if part.Header.Get("Content-Type") != "text/plain" || part.Header.Get("X-Part-Kind") != "reviewed" || string(payload) != "hello" {
		t.Fatalf("part headers=%#v payload=%q", part.Header, payload)
	}
}

func TestSelectRequestContentTreatsXMLAndBinaryAsRaw(t *testing.T) {
	for _, mediaType := range []string{"application/xml", "application/octet-stream"} {
		selected, _, err := SelectRequestContent(&models.RequestContent{Representations: []models.RequestRepresentation{{MediaType: mediaType, Serialization: models.RequestSerializationRaw}}})
		if err != nil || selected.Serialization != models.RequestSerializationRaw {
			t.Fatalf("media=%q selected=%#v err=%v", mediaType, selected, err)
		}
	}
}

func TestResponseMediaSelectionSupportsStatusRangesAndVendorJSON(t *testing.T) {
	responses := models.Responses{"2XX": {Representations: []models.ResponseRepresentation{{MediaType: "application/vnd.test+json"}}}}
	if outcome := responseMediaSelectionOutcome(responses, 201, "application/vnd.test+json; charset=utf-8"); outcome != "matched" {
		t.Fatalf("outcome = %q", outcome)
	}
	if family := boundedResponseMediaFamily("application/vnd.test+json"); family != "json" {
		t.Fatalf("family = %q", family)
	}
	if strings.Contains(responseMediaSelectionOutcome(responses, 201, "text/plain"), "text/plain") {
		t.Fatal("selection outcome must remain bounded")
	}
}
