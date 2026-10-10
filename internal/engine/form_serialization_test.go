package engine

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestCheckoutFormPreservesArrayObjects exercises the reported shape through the real request builder without provider effects.
func TestCheckoutFormPreservesArrayObjects(t *testing.T) {
	content := &models.RequestContent{Representations: []models.RequestRepresentation{{
		MediaType: "application/x-www-form-urlencoded", Serialization: models.RequestSerializationForm,
		Schema:   &models.SchemaContract{Raw: json.RawMessage(`{"type":"object","properties":{"customer":{"type":"string"},"mode":{"type":"string"},"success_url":{"type":"string"},"line_items":{"type":"array","items":{"type":"object","properties":{"price":{"type":"string"},"quantity":{"type":"integer"}}}}}}`)},
		Encoding: map[string]models.RequestEncoding{"line_items": {Style: "deepObject"}},
	}}}
	_, headers, body, err := prepareRequestParts(&models.Service{BaseURL: "https://provider.invalid"}, &models.IntegrationObject{Method: "POST", Path: "/v1/checkout/sessions", RequestContent: content}, map[string]any{
		"customer": "cus_example", "mode": "subscription", "success_url": "https://usefused.com/success",
		"line_items": []any{map[string]any{"price": "price_id", "quantity": 1}, map[string]any{"price": "price_second", "quantity": 2}},
	}, nil)
	// Request preparation must accept the declared array shape before any outbound dispatch.
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	// Reading an in-memory request should be infallible, but never assert on incomplete wire data.
	if err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(string(raw))
	// Indexed form fields retain both grouping and scalar values on the wire.
	if err != nil || headers["Content-Type"] != "application/x-www-form-urlencoded" || values.Get("line_items[0][price]") != "price_id" || values.Get("line_items[1][quantity]") != "2" || values.Get("customer") != "cus_example" {
		t.Fatalf("incorrect form: %s headers=%v err=%v", raw, headers, err)
	}
}

// TestDeepFormBoundsAndNestedValues ensures nested encoding is finite and never stringifies a structured object.
func TestDeepFormBoundsAndNestedValues(t *testing.T) {
	form := queryParameters{}
	value := []map[string]any{{"price_data": map[string]any{"recurring": map[string]any{"interval": "month"}, "label": "a + b&c"}}}
	// Typed slices and nested objects must use the same encoding as decoded JSON inputs.
	if err := addDeepFormValue("line_items", value, form, false, 0); err != nil {
		t.Fatal(err)
	}
	values, _ := url.ParseQuery(form.EncodeForm())
	// Nested objects and reserved characters must survive a complete form round trip.
	if values.Get("line_items[0][price_data][recurring][interval]") != "month" || values.Get("line_items[0][price_data][label]") != "a + b&c" {
		t.Fatalf("incorrect nested form: %s", form.EncodeForm())
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	// Provider union fields may select a scalar rather than their structured branch.
	if err := addDeepFormValue("value", "scalar", form, false, 0); err != nil {
		t.Fatal(err)
	}
	values, _ = url.ParseQuery(form.EncodeForm())
	// The selected scalar must retain its original field name and value.
	if values.Get("value") != "scalar" {
		t.Fatal("scalar union form property changed")
	}
	// Cycles from internal callers must fail rather than exhaust the Engine stack.
	if err := addDeepFormValue("value", cycle, queryParameters{}, false, 0); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("cycle accepted: %v", err)
	}
	// Extending form encoding must not relax the distinct OpenAPI query contract.
	if err := serializeQueryParameter(models.Parameter{Name: "filter", In: "query", Serialization: models.ParameterSerialization{Style: "deepObject"}}, []any{"value"}, queryParameters{}); err == nil {
		t.Fatal("query deepObject array accepted")
	}
}
