package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// TestFetchServiceVersionAuthConfigsTransportsTokenRequestMediaType pins the
// GraphQL field and typed decode used by connect and refresh flows.
func TestFetchServiceVersionAuthConfigsTransportsTokenRequestMediaType(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceVersionAuthConfigs":[{"service_id":"` + serviceID.String() + `","version":"1.0.0","service_version_id":"` + versionID.String() + `","auth_configs":[{"name":"oauth","type":"oauth2","token_request_media_type":"application/json"}]}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	configs, err := client.FetchServiceVersionAuthConfigs(context.Background(), []ServiceVersionRef{{ServiceID: serviceID, Version: "1.0.0"}}, "fsk_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || len(configs[0].AuthConfigs) != 1 || configs[0].AuthConfigs[0].TokenRequestMediaType != fusedobject.TokenRequestMediaTypeJSON {
		t.Fatalf("token request media did not decode: %#v", configs)
	}
	if !strings.Contains(requestBody.Query, "token_request_media_type") {
		t.Fatalf("auth config query omitted token_request_media_type: %s", requestBody.Query)
	}
}
