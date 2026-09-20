package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/testcontract"
	"github.com/google/uuid"
)

// TestWebhookMetadataRetainsAuthenticatedPolicy exercises the real batch decode and verifies both query builders request every security field.
func TestWebhookMetadataRetainsAuthenticatedPolicy(t *testing.T) {
	id, versionID := uuid.New(), uuid.New()
	policy := testcontract.AuthenticatedSignature()
	client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "test-engine-license", httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var query graphqlQuery
		// Inspect the actual request before supplying the synthetic Registry response.
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Fatal(err)
		}
		assertSignatureFields(t, query.Query)
		body, err := json.Marshal(map[string]any{"data": map[string]any{"serviceWebhookMetadata": []any{map[string]any{"service_id": id, "service_version_id": versionID, "version": "v1", "incoming_webhook_config": map[string]any{"signature_policy": policy}}}}})
		// Encoding failure invalidates the fixture rather than weakening the decode assertion.
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}}
	ref := ServiceMetadataRef{ServiceID: id, Version: "v1"}
	result, err := client.FetchServiceMetadataBatch(context.Background(), []ServiceMetadataRef{ref})
	// A valid policy must survive the same typed decode that webhook planning consumes.
	if err != nil {
		t.Fatal(err)
	}
	actual := result[ServiceMetadataRefKey(ref)].IncomingWebhookConfig.SignaturePolicy
	// The entire tree matters: a missing response or timestamp could weaken authentication.
	if !reflect.DeepEqual(actual, &policy) {
		t.Fatal("signature policy changed during metadata transport")
	}
	request, err := client.buildServiceMetadataRequest(context.Background(), id.String(), "v1")
	// The single-service fallback is a second production projection and needs the same guarantee.
	if err != nil {
		t.Fatal(err)
	}
	var query graphqlQuery
	// Decode the generated envelope rather than testing a disconnected constant.
	if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
		t.Fatal(err)
	}
	assertSignatureFields(t, query.Query)
}

// assertSignatureFields guards the nested GraphQL selection at each production metadata boundary.
func assertSignatureFields(t *testing.T, query string) {
	t.Helper()
	for _, field := range []string{"signature_policy", "response { value { location name path } body_field status_code }", "components { value kind names join algorithm encoding }", "timestamp { header max_age_ms max_future_ms }"} {
		// Each field is required to preserve verifier behavior rather than optional display metadata.
		if !strings.Contains(query, field) {
			t.Fatalf("metadata query omitted %s", field)
		}
	}
}
