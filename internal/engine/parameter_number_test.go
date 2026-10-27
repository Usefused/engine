package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestDispatcherNumericArticleLookup reproduces the hosted failure through JSON decoding and real provider HTTP.
func TestDispatcherNumericArticleLookup(t *testing.T) {
	// The provider accepts a decimal identifier, as the real article lookup does.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Scientific notation is a different resource path even when its numeric value is identical.
		if r.URL.Path != "/api/articles/4917886" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":4917886}`))
	}))
	defer provider.Close()
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{
		Method: http.MethodGet, Path: "/api/articles/{id}",
		Parameters: models.Parameters{{Name: "id", In: "path", Type: "integer", Required: true}},
	})
	for _, raw := range []string{`{"id":4917886}`, `{"id":4.917886e6}`} {
		// Equivalent JSON number spellings must resolve to the same provider resource.
		t.Run(raw, func(t *testing.T) {
			var input map[string]any
			// Match the normal Unified App host's JSON-to-interface decoding boundary.
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				t.Fatal(err)
			}
			status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation, input, nil, nil, NewBufferStream())
			// A successful real HTTP response proves the identifier survived the complete request builder.
			if err != nil || status != http.StatusOK {
				t.Fatalf("numeric article lookup: status=%d err=%v", status, err)
			}
		})
	}
}

// TestNumericParametersAcrossWireLocations verifies callers share decimal formatting beyond the original path lookup.
func TestNumericParametersAcrossWireLocations(t *testing.T) {
	operation := &models.IntegrationObject{Method: http.MethodGet, Path: "/articles/{id}", Parameters: models.Parameters{
		{Name: "id", In: "path"}, {Name: "after", In: "query"},
		{Name: "X-Article-ID", In: "header"}, {Name: "article", In: "cookie"},
	}}
	value := float64(4917886)
	requestURL, headers, _, err := prepareRequestParts(&models.Service{BaseURL: "https://provider.invalid"}, operation,
		map[string]any{"id": value, "after": value, "X-Article-ID": value, "article": value}, nil)
	// A numeric lookup must use identical decimal identifiers in every non-JSON wire location.
	if err != nil || requestURL != "https://provider.invalid/articles/4917886?after=4917886" || headers["X-Article-ID"] != "4917886" || headers["Cookie"] != "article=4917886" {
		t.Fatalf("URL=%q headers=%v err=%v", requestURL, headers, err)
	}
	form := queryParameters{}
	// Nested form leaves also carry identifiers and must not fall back to Go's exponent formatter.
	if err := addDeepFormValue("items", []any{map[string]any{"id": value}}, form, false, 0); err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(form.EncodeForm())
	// Check the provider-decoded form value rather than the internal container representation.
	if err != nil || values.Get("items[0][id]") != "4917886" {
		t.Fatalf("form=%q err=%v", form.EncodeForm(), err)
	}
}

// TestParameterNumbersUseDecimalNotation covers shared scalar, array, and object serialization without rounding identifiers.
func TestParameterNumbersUseDecimalNotation(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"article ID", float64(4917886), "4917886"},
		{"negative ID", float64(-4917886), "-4917886"},
		{"largest safe JS integer", float64(9007199254740991), "9007199254740991"},
		{"small fractional number", float64(0.0000001), "0.0000001"},
		{"fraction is not truncated", float64(4917886.25), "4917886.25"},
		{"float32", float32(4917886), "4917886"},
		{"integer", int64(4917886), "4917886"},
		{"exact JSON integer", json.Number("9007199254740993"), "9007199254740993"},
		{"numeric string stays literal", "4.917886e6", "4.917886e6"},
		{"boolean", true, "true"},
	}
	for _, test := range cases {
		// The same value must retain its representation in every supported parameter container.
		t.Run(test.name, func(t *testing.T) {
			shapes := []struct {
				value any
				want  string
			}{
				{test.value, test.want},
				{[]any{test.value, test.value}, test.want + "," + test.want},
				{map[string]any{"id": test.value}, "id=" + test.want},
			}
			for _, shape := range shapes {
				parts, err := splitParameterValue(shape.value)
				// Serialization failures cannot be hidden by an empty expected wire value.
				if err != nil {
					t.Fatal(err)
				}
				// Header-style serialization exposes exact scalar text without URL-escaping differences.
				if got := serializeSimpleValue(parts, true); got != shape.want {
					t.Errorf("value=%#v: got %q, want %q", shape.value, got, shape.want)
				}
			}
		})
	}
}
