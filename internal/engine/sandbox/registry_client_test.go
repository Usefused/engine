package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func TestRuntimeRateLimitProjectionExcludesLegacyV2Fields(t *testing.T) {
	// Why: Registry normalizes v2 inputs before persistence and deliberately
	// omits those compatibility-only fields from its GraphQL execution contract.
	for _, field := range []string{"scope", "default_cost", "operation_costs", "response_headers", "retry_after"} {
		if strings.Contains(runtimeRateLimitFields, field) {
			t.Fatalf("runtime rate-limit projection still requests legacy field %q", field)
		}
	}
}

func TestRuntimeRetryProjectionExcludesLegacyV2Fields(t *testing.T) {
	// Why: retry strategy/max-retries/backoff are normalized into v3 rules and
	// are intentionally absent from Registry's runtime GraphQL projection.
	if strings.Contains(runtimeRetryFields, "\n\tstrategy max_retries backoff_ms") {
		t.Fatal("runtime retry projection still requests legacy v2 fields")
	}
}

// TestFetchEligibleConnectionProfilesTransportsExactAuthName proves named and unnamed Registry streams remain distinct on the Engine boundary.
func TestFetchEligibleConnectionProfilesTransportsExactAuthName(t *testing.T) {
	versionID, profileID := uuid.New(), uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			// Capturing the decoded GraphQL envelope verifies transport rather than string-building internals.
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"eligibleConnectionProfiles":[{"profile_id":"` + profileID.String() + `","service_id":"` + uuid.NewString() + `","service_version_id":"` + versionID.String() + `","name":"Jira","auth_type":"oauth","auth_name":"jiraOAuth","revision":2,"profile_hash":"hash","config":{"auth_type":"oauth","auth_name":"jiraOAuth"},"provenance":"provider"}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	profiles, err := client.FetchEligibleConnectionProfiles(context.Background(), []ConnectionProfileRef{
		{ServiceVersionID: versionID, AuthType: "oauth", AuthName: "jiraOAuth"},
		{ServiceVersionID: versionID, AuthType: "oauth", AuthName: ""},
	}, "api-key")
	// A valid named Registry response must decode through the Engine DTO.
	if err != nil {
		t.Fatalf("FetchEligibleConnectionProfiles() error = %v", err)
	}
	// Response identity is needed again for exact plan grouping.
	if len(profiles) != 1 || profiles[0].AuthName != "jiraOAuth" {
		t.Fatalf("auth_name response was not decoded: %#v", profiles)
	}
	// Omitting auth_name from the projection would silently decode every response as legacy unnamed.
	if !strings.Contains(requestBody.Query, "auth_type auth_name revision") {
		t.Fatalf("eligible profile projection omitted auth_name: %q", requestBody.Query)
	}
	refs := requestBody.Variables["refs"].([]interface{})
	// Explicit empty transport distinguishes legacy exact matching from an omitted wildcard interpretation.
	if refs[0].(map[string]interface{})["auth_name"] != "jiraOAuth" || refs[1].(map[string]interface{})["auth_name"] != "" {
		t.Fatalf("named or legacy ref changed: %#v", refs)
	}
}

func TestFetchServiceVersionAuthConfigsUsesGraphQLBatch(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.Path != "/graphql" {
				t.Fatalf("expected GraphQL POST, got %s %s", request.Method, request.URL.Path)
			}
			if request.Header.Get("X-API-Key") != "engine-license-key" || request.Header.Get("Authorization") != "Bearer engine-license-key" {
				t.Fatalf("Registry did not receive only the Engine licence identity")
			}
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceVersionAuthConfigs":[{"service_id":"` + serviceID.String() + `","version":"1.0.0","service_version_id":"` + versionID.String() + `","auth_configs":[{"name":"oauth","type":"oauth2","token_endpoint_auth_method":"client_secret_basic","pkce_required":true,"scopes_delimiter":"comma","extra_auth_params":{"prompt":"consent"},"extra_token_params":{"audience":"payments"},"refresh_token_rotates":true}]}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	configs, err := client.FetchServiceVersionAuthConfigs(context.Background(), []ServiceVersionRef{{ServiceID: serviceID, Version: versionID.String()}}, "fsk_test")
	if err != nil {
		t.Fatalf("FetchServiceVersionAuthConfigs() error = %v", err)
	}
	if len(configs) != 1 || configs[0].ServiceVersionID != versionID {
		t.Fatalf("unexpected auth configs: %#v", configs)
	}
	if len(configs[0].AuthConfigs) != 1 || configs[0].AuthConfigs[0].TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("token endpoint auth method did not decode: %#v", configs[0].AuthConfigs)
	}
	auth := configs[0].AuthConfigs[0]
	if !auth.PKCERequired || auth.ScopesDelimiter != "comma" || auth.ExtraAuthParams["prompt"] != "consent" || auth.ExtraTokenParams["audience"] != "payments" || !auth.RefreshTokenRotates {
		t.Fatalf("OAuth edge policy did not decode: %#v", auth)
	}
	if !strings.Contains(requestBody.Query, "serviceVersionAuthConfigs") {
		t.Fatalf("expected GraphQL auth-config query, got %q", requestBody.Query)
	}
	if strings.Contains(requestBody.Query, "operation_names") || !containsAll(requestBody.Query, "token_endpoint_auth_method", "pkce_required", "scopes_delimiter", "extra_auth_params", "extra_token_params", "refresh_token_rotates") {
		t.Fatalf("legacy auth-config projection changed unexpectedly: %q", requestBody.Query)
	}
	refs, ok := requestBody.Variables["refs"].([]interface{})
	if !ok || len(refs) != 1 {
		t.Fatalf("expected one batched service ref, got %#v", requestBody.Variables["refs"])
	}
	ref, ok := refs[0].(map[string]interface{})
	if !ok || ref["service_id"] != serviceID.String() || ref["version"] != versionID.String() {
		t.Fatalf("unexpected batched service ref: %#v", refs[0])
	}
}

func TestFetchServiceVersionExecutionAuthContractsDecodesOperationSecurity(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceVersionExecutionAuthContracts":[{"service_id":"` + serviceID.String() + `","version":"v1","service_version_id":"` + versionID.String() + `","operation_names":["listItems"],"select_all":false,"auth_configs":[{"name":"bearerAuth","type":"http","scheme":"bearer"},{"name":"basicAuth","type":"http","scheme":"basic","basic_password_mode":"required"}],"operations":[{"name":"listItems","security_requirements":[{"schemes":[{"scheme":"bearerAuth","scopes":[]}]}]}]}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	contracts, err := client.FetchServiceVersionExecutionAuthContracts(context.Background(), []ServiceVersionExecutionAuthSelection{{
		ServiceID: serviceID, Version: "v1", OperationNames: []string{"listItems"},
	}}, "ignored-user-key")
	if err != nil {
		t.Fatalf("FetchServiceVersionExecutionAuthContracts() error = %v", err)
	}
	if len(contracts) != 1 || len(contracts[0].Operations) != 1 || contracts[0].OperationNames[0] != "listItems" || contracts[0].Operations[0].SecurityRequirements[0].Schemes[0].Scheme != "bearerAuth" {
		t.Fatalf("unexpected execution auth contracts: %#v", contracts)
	}
	if len(contracts[0].AuthConfigs) != 2 || contracts[0].AuthConfigs[1].BasicPasswordMode != authrouting.BasicPasswordRequired {
		t.Fatalf("basic password mode was not preserved: %#v", contracts[0].AuthConfigs)
	}
	if !strings.Contains(requestBody.Query, "serviceVersionExecutionAuthContracts") {
		t.Fatalf("expected selection-aware GraphQL query, got %q", requestBody.Query)
	}
	if !strings.Contains(requestBody.Query, "operation_names select_all") || !strings.Contains(requestBody.Query, "basic_password_mode") || strings.Contains(requestBody.Query, "key_name") || strings.Contains(requestBody.Query, "token_endpoint_auth_method") {
		t.Fatalf("execution auth contract did not use its minimal correlated projection: %q", requestBody.Query)
	}
	selections, ok := requestBody.Variables["selections"].([]interface{})
	if !ok || len(selections) != 1 {
		t.Fatalf("expected one batched selection, got %#v", requestBody.Variables)
	}
	selection := selections[0].(map[string]interface{})
	if selection["service_id"] != serviceID.String() || selection["select_all"] != false {
		t.Fatalf("unexpected selection payload: %#v", selection)
	}
}

func TestPublishExecutionPoliciesUseRegistryRESTOrigin(t *testing.T) {
	serviceID := uuid.New()
	requests := make([]string, 0, 2)
	bodies := make([]map[string]any, 0, 2)
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql/",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request.URL.Path)
			if request.Method != http.MethodPut || request.Header.Get("X-API-Key") != "engine-license-key" || request.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected policy request: %s %s", request.Method, request.Header.Get("X-API-Key"))
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode policy body: %v", err)
			}
			bodies = append(bodies, body)
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
		})},
	}

	policy := map[string]any{"timeout_ms": 1500, "retry": map[string]any{"version": 3, "rules": []any{}}}
	if err := client.PublishServiceExecutionPolicy(context.Background(), serviceID, policy, "user-api-key"); err != nil {
		t.Fatalf("PublishServiceExecutionPolicy() error = %v", err)
	}
	if err := client.PublishServiceVersionExecutionPolicy(context.Background(), serviceID, "2026-08-03", policy, "user-api-key"); err != nil {
		t.Fatalf("PublishServiceVersionExecutionPolicy() error = %v", err)
	}
	want := []string{
		"/integrations/" + serviceID.String() + "/execution-policy",
		"/integrations/" + serviceID.String() + "/versions/2026-08-03/execution-policy",
	}
	if len(requests) != len(want) || requests[0] != want[0] || requests[1] != want[1] {
		t.Fatalf("policy paths = %#v, want %#v", requests, want)
	}
	if len(bodies) != 2 || !reflect.DeepEqual(bodies[0], bodies[1]) || len(bodies[0]) != 2 || bodies[0]["timeout_ms"] != float64(1500) {
		t.Fatalf("policy bodies = %#v", bodies)
	}
}

func TestRegistryBaseURLPreservesNonGraphQLBasePath(t *testing.T) {
	client := &HTTPRegistryClient{endpoint: "https://registry.example/api"}
	if got := client.registryBaseURL(); got != "https://registry.example/api" {
		t.Fatalf("registryBaseURL() = %q", got)
	}
}

func TestRenewSDKPackageLeasesUsesEngineIdentityAndBatchPayload(t *testing.T) {
	appID, familyID := uuid.New(), uuid.New()
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.Path != "/sdk-packages/leases/renew" {
				t.Fatalf("unexpected lease request: %s %s", request.Method, request.URL.Path)
			}
			if request.Header.Get("Authorization") != "Bearer engine-license-key" || request.Header.Get("X-API-Key") != "engine-license-key" {
				t.Fatal("lease request did not use the Engine licence identity")
			}
			if request.Header.Get("X-Engine-Signature") == "" {
				t.Fatal("lease request was not signed")
			}
			var body sdkPackageLeaseRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode lease request: %v", err)
			}
			if len(body.Apps) != 1 || body.Apps[0].AppID != appID || body.Apps[0].AppFamilyID != familyID {
				t.Fatalf("unexpected lease payload: %#v", body.Apps)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"renewed":1}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	renewed, err := client.RenewSDKPackageLeases(context.Background(), []models.SDKPackageLeaseRenewal{{
		AppID: appID, AppFamilyID: familyID,
	}})
	if err != nil {
		t.Fatalf("RenewSDKPackageLeases() error = %v", err)
	}
	if renewed != 1 {
		t.Fatalf("RenewSDKPackageLeases() = %d, want 1", renewed)
	}
}

func TestDownloadSDKPackageUsesExactAppCacheRoute(t *testing.T) {
	appID := uuid.New()
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet || request.URL.Path != "/sdk-packages/"+appID.String()+"/download" {
				t.Fatalf("unexpected package download: %s %s", request.Method, request.URL.Path)
			}
			if request.Header.Get("X-API-Key") != "engine-license-key" {
				t.Fatal("package download did not use Engine identity")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("zip")), Header: make(http.Header)}, nil
		})},
	}

	response, err := client.DownloadSDKPackage(context.Background(), appID)
	if err != nil {
		t.Fatalf("DownloadSDKPackage() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("DownloadSDKPackage() status = %d", response.StatusCode)
	}
}

func TestFetchServiceMetadataBatchUsesOneSetBasedGraphQLRequest(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	requestCount := 0
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount++
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceWebhookMetadata":[
				{"service_id":"` + first.String() + `","service_version_id":"` + uuid.NewString() + `","version":"v1","name":"First","event_extraction_path":"event.type"},
				{"service_id":"` + second.String() + `","service_version_id":"` + uuid.NewString() + `","version":"v2","name":"Second","incoming_webhook_config":{"auth_type":"hmac_signature"}}
			]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
	refs := []ServiceMetadataRef{{ServiceID: first, Version: "v1"}, {ServiceID: second, Version: "v2"}}
	metadata, err := client.FetchServiceMetadataBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("FetchServiceMetadataBatch: %v", err)
	}
	if requestCount != 1 || !strings.Contains(requestBody.Query, "serviceWebhookMetadata(refs: $refs)") || strings.Contains(requestBody.Query, "s0: service") {
		t.Fatalf("requests=%d query=%q, want one set-based batch", requestCount, requestBody.Query)
	}
	if metadata[ServiceMetadataRefKey(refs[0])].EventExtractionPath != "event.type" || metadata[ServiceMetadataRefKey(refs[1])].IncomingWebhookConfig.AuthType != "hmac_signature" {
		t.Fatalf("unexpected batched metadata: %#v", metadata)
	}
}

func TestFetchRuntimeContractUsesBundledGraphQLProjection(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	webhookID := uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("X-API-Key") != "engine-license-key" || request.Header.Get("Authorization") != "Bearer engine-license-key" {
				t.Fatalf("Registry auth = %q / %q", request.Header.Get("X-API-Key"), request.Header.Get("Authorization"))
			}
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceRuntimeContracts":[{
				"contract_version":2,
				"required_capabilities":[],
				"service_id":"` + serviceID.String() + `",
				"service_version_id":"` + serviceVersionID.String() + `",
				"version":"2026-07-23",
				"service":{
					"id":"` + serviceID.String() + `",
					"current_service_version":"2026-07-23",
					"name":"Runtime Service",
					"description":"runtime projection",
					"base_url":"https://api.example.com",
					"servers":[{"url":"https://api.example.com","environment":"prod","is_default":true}],
					"default_headers":{"X-Provider":"example"},
					"connect_config":null,
					"auth_configs":[{"name":"oauth","type":"oauth2","oauth2_flows":{"authorizationCode":{"authorization_url":"https://auth.example/authorize","token_url":"https://auth.example/token","scopes":{}}},"token_endpoint_auth_method":"client_secret_post","pkce_required":true,"scopes_delimiter":"comma","extra_auth_params":{"prompt":"consent"},"extra_token_params":{"audience":"payments"},"refresh_token_rotates":true}],
					"rate_limit":{"version":3,"policies":[{"name":"requests","mode":"enforce","unit":"requests","identity":{"inputs":[{"kind":"service_version"}]},"cost":{"default":1,"rules":[]},"algorithm":"fixed_window","fixed_window":{"limit":10,"duration_ms":1000}}]},
					"retry_config":{"version":3,"rules":[{"predicates":{"methods":["POST"],"operation_kinds":["query"],"statuses":[{"min":500,"max":599}],"errors":[],"body_replayability":"replayable","idempotency_key":{"requirement":"required","header":"Idempotency-Key"},"required_provider_headers":[]},"action":{"max_attempts":3,"max_elapsed_ms":1000,"backoff":{"strategy":"fixed","base_delay_ms":100,"max_delay_ms":100,"jitter_ms":0},"retry_after_headers":[]}}]},
					"timeout_ms":45000,
					"event_extraction_path":"event.type",
					"incoming_webhook_config":{"auth_type":"signature","signature_header":"X-Signature"}
				},
				"operations":[{"id":"` + endpointID.String() + `","name":"listInvoices","description":"List invoices","resource_name":"query","version":"2026-07-23","method":"POST","path":"/graphql/{resource}","normalized_path":"/graphql/{resource}","deprecated":false,"security_requirements":[{"schemes":[{"scheme":"oauth","scopes":[]}]}],"parameters":[{"name":"resource","in":"path","required":true,"type":"string","description":"Resource","path_encoding":"preserve_slashes"},{"name":"cursor","in":"query","required":false,"type":"string"}],"request_content":{"representations":[{"media_type":"application/vnd.api+json","serialization":"json"}]},"responses":{"200":{"description":"ok","representations":[]}},"graphql_query":"query ListInvoices($limit: Int) { invoices(limit: $limit) { id } }","provider_protocol":"graphql","operation_kind":"query","pagination":{"version":3,"request":[{"state":"cursor","target":{"location":"query","name":"cursor"},"value_type":"string","initial":{"type":"string","string":""},"apply":"all"}],"response":{"items":{"path":"$.items"},"values":[{"name":"next","source":{"location":"body","path":"$.next","value_type":"string"}}]},"continuation":[{"kind":"token","state":"cursor","response_value":"next"}],"termination":{"stop_on_empty_items":true,"stop_on_missing_values":["next"],"repeated_value":"error"},"limits":{"max_pages":100,"max_items":10000,"max_bytes":16777216,"max_duration_ms":120000}}}],
				"webhooks":[{"id":"` + webhookID.String() + `","service_id":"` + serviceID.String() + `","name":"invoice.created","method":"POST","description":"Invoice created","request_body":{"type":"object"}}]
			}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	snapshot, err := client.FetchRuntimeContract(context.Background(), serviceID, serviceVersionID, "", "user-api-key")
	if err != nil {
		t.Fatalf("FetchRuntimeContract() error = %v", err)
	}
	if snapshot.ServiceID != serviceID || snapshot.ServiceVersionID != serviceVersionID || snapshot.Version != "2026-07-23" {
		t.Fatalf("unexpected snapshot identity: %#v", snapshot)
	}
	if snapshot.ServiceMetadata.Name != "Runtime Service" || snapshot.ServiceMetadata.ServiceVersionID != serviceVersionID {
		t.Fatalf("unexpected metadata: %#v", snapshot.ServiceMetadata)
	}
	if snapshot.ServiceMetadata.TimeoutMs == nil || *snapshot.ServiceMetadata.TimeoutMs != 45000 {
		t.Fatalf("runtime timeout was not decoded: %v", snapshot.ServiceMetadata.TimeoutMs)
	}
	if len(snapshot.ServiceMetadata.AuthConfigs) != 1 || snapshot.ServiceMetadata.AuthConfigs[0].TokenEndpointAuthMethod != "client_secret_post" {
		t.Fatalf("runtime OAuth token endpoint auth method was not decoded: %#v", snapshot.ServiceMetadata.AuthConfigs)
	}
	auth := snapshot.ServiceMetadata.AuthConfigs[0]
	if !auth.PKCERequired || auth.ScopesDelimiter != "comma" || auth.ExtraAuthParams["prompt"] != "consent" || auth.ExtraTokenParams["audience"] != "payments" || !auth.RefreshTokenRotates {
		t.Fatalf("runtime OAuth edge policy was not decoded: %#v", auth)
	}
	if len(snapshot.Endpoints) != 1 || snapshot.Endpoints[0].ID != endpointID || snapshot.Endpoints[0].Pagination == nil {
		t.Fatalf("unexpected endpoints: %#v", snapshot.Endpoints)
	}
	if snapshot.Endpoints[0].GraphQLQuery == nil || snapshot.Endpoints[0].ProviderProtocol != "graphql" || snapshot.Endpoints[0].OperationKind != "query" {
		t.Fatalf("GraphQL execution metadata was not decoded: %#v", snapshot.Endpoints[0])
	}
	if snapshot.Endpoints[0].RequestContent == nil || len(snapshot.Endpoints[0].RequestContent.Representations) != 1 || snapshot.Endpoints[0].RequestContent.Representations[0].MediaType != "application/vnd.api+json" || snapshot.Endpoints[0].RequestContent.Representations[0].Serialization != "json" {
		t.Fatalf("request content was not decoded: %#v", snapshot.Endpoints[0])
	}
	if len(snapshot.Endpoints[0].Parameters) != 2 || snapshot.Endpoints[0].Parameters[0].PathEncoding != "preserve_slashes" {
		t.Fatalf("path encoding was not decoded: %#v", snapshot.Endpoints[0].Parameters)
	}
	if len(snapshot.Webhooks) != 1 || snapshot.Webhooks[0].ID != webhookID {
		t.Fatalf("unexpected webhooks: %#v", snapshot.Webhooks)
	}
	if !containsAll(requestBody.Query, "serviceRuntimeContracts", "contract_version", "required_capabilities", "operations", "webhooks", "timeout_ms", "request_content", "path_encoding", "graphql_query", "provider_protocol", "operation_kind", "token_endpoint_auth_method", "pkce_required", "scopes_delimiter", "extra_auth_params", "extra_token_params", "refresh_token_rotates") {
		t.Fatalf("runtime contract query did not bundle service operations and webhooks: %s", requestBody.Query)
	}
	variablesJSON, _ := json.Marshal(requestBody.Variables)
	if !bytes.Contains(variablesJSON, []byte(serviceID.String())) || !bytes.Contains(variablesJSON, []byte(serviceVersionID.String())) {
		t.Fatalf("unexpected runtime contract variables: %#v", requestBody.Variables)
	}
	if requestBody.Variables["engine_contract_version"] != float64(2) && requestBody.Variables["engine_contract_version"] != 2 {
		t.Fatalf("engine contract version = %#v", requestBody.Variables["engine_contract_version"])
	}
	capabilities, ok := requestBody.Variables["engine_capabilities"].([]interface{})
	assertEngineCapabilities(t, capabilities, ok, variablesJSON)
}

func TestFetchRuntimeContractPreservesTagHierarchy(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	client := &HTTPRegistryClient{
		endpoint: "https://registry.example/graphql", licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			body := `{"data":{"serviceRuntimeContracts":[{"contract_version":2,"required_capabilities":[],"service_id":"` + serviceID.String() + `","service_version_id":"` + versionID.String() + `","version":"2026-07-23","service":` + runtimeContractServiceJSON(serviceID, versionID, "Tagged") + `,"operations":[],"webhooks":[]}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
	snapshot, err := client.FetchRuntimeContract(context.Background(), serviceID, versionID, "", "user-api-key")
	if err != nil {
		t.Fatalf("FetchRuntimeContract: %v", err)
	}
	assertRuntimeTagHierarchy(t, fusedToService(&snapshot.ServiceMetadata))
}

func assertRuntimeTagHierarchy(t *testing.T, service *models.Service) {
	t.Helper()
	if service.Documentation == nil {
		t.Fatalf("runtime service tag documentation = %#v", service.Documentation)
	}
	if len(service.Documentation.Tags) != 1 {
		t.Fatalf("runtime service tags = %#v", service.Documentation.Tags)
	}
	tag := service.Documentation.Tags[0]
	want := models.TagDocumentation{
		Name: "orders", Summary: "Order operations", Description: "Order APIs", Parent: "commerce", Kind: "badge",
		ExternalDocs: &models.ExternalDocumentation{Description: "Tag guide", URL: "https://docs.example.com/tags/orders"},
	}
	if !reflect.DeepEqual(tag, want) {
		t.Fatalf("runtime tag hierarchy = %#v, want %#v", tag, want)
	}
}

func assertEngineCapabilities(t *testing.T, capabilities []interface{}, decoded bool, variablesJSON []byte) {
	t.Helper()
	supported := fusedobject.EngineExecutionContractSupport().RequiredCapabilities
	if !decoded || len(capabilities) != len(supported) {
		t.Fatalf("engine capabilities = %#v, want current supported set", capabilities)
	}
	for _, capability := range supported {
		if !bytes.Contains(variablesJSON, []byte(capability)) {
			t.Fatalf("engine capabilities = %#v, missing %q", capabilities, capability)
		}
	}
}

func TestFetchRuntimeContractsUsesSingleSetBasedGraphQLRequest(t *testing.T) {
	firstServiceID := uuid.New()
	firstVersionID := uuid.New()
	secondServiceID := uuid.New()
	secondVersionID := uuid.New()
	var requestBody graphqlQuery
	requestCount := 0
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount++
			if request.Header.Get("X-API-Key") != "engine-license-key" || request.Header.Get("Authorization") != "Bearer engine-license-key" {
				t.Fatalf("Registry did not receive only the Engine licence identity")
			}
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceRuntimeContracts":[
				{"contract_version":2,"required_capabilities":[],"service_id":"` + firstServiceID.String() + `","service_version_id":"` + firstVersionID.String() + `","version":"2026-07-22","service":` + runtimeContractServiceJSON(firstServiceID, firstVersionID, "First") + `,"operations":[{"id":"` + uuid.NewString() + `","name":"firstOp","method":"GET","path":"/first","provider_protocol":"rest","security_requirements":[{"schemes":[]}]}],"webhooks":[]},
				{"contract_version":2,"required_capabilities":[],"service_id":"` + secondServiceID.String() + `","service_version_id":"` + secondVersionID.String() + `","version":"2026-07-23","service":` + runtimeContractServiceJSON(secondServiceID, secondVersionID, "Second") + `,"operations":[{"id":"` + uuid.NewString() + `","name":"secondOp","method":"POST","path":"/second","provider_protocol":"rest","security_requirements":[{"schemes":[]}]}],"webhooks":[]}
			]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	snapshots, err := client.FetchRuntimeContracts(context.Background(), []store.WorkspaceServiceVersion{
		{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, Version: "2026-07-22"},
		{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, Version: "2026-07-23"},
	}, "user-api-key")
	if err != nil {
		t.Fatalf("FetchRuntimeContracts() error = %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("expected one GraphQL request, got %d", requestCount)
	}
	if len(snapshots) != 2 || snapshots[0].ServiceID != firstServiceID || snapshots[1].ServiceID != secondServiceID {
		t.Fatalf("unexpected snapshots: %#v", snapshots)
	}
	if !strings.Contains(requestBody.Query, "serviceRuntimeContracts(refs: $refs, engine_contract_version: $engine_contract_version, engine_capabilities: $engine_capabilities)") || strings.Contains(requestBody.Query, "service0: service") {
		t.Fatalf("expected set-based runtime contract query, got %s", requestBody.Query)
	}
	variablesJSON, _ := json.Marshal(requestBody.Variables)
	if !bytes.Contains(variablesJSON, []byte(firstServiceID.String())) || !bytes.Contains(variablesJSON, []byte(secondVersionID.String())) {
		t.Fatalf("unexpected variables: %#v", requestBody.Variables)
	}
}

func TestDecodeRuntimeContractsResponseRejectsIncompatibleEnvelope(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	requested := []store.WorkspaceServiceVersion{{ServiceID: serviceID, ServiceVersionID: versionID, Version: "2026-08-11"}}
	tests := []struct {
		name     string
		envelope string
		wantErr  string
	}{
		{name: "missing envelope", envelope: ``, wantErr: fusedobject.ExecutionContractReasonUnsupportedVersion},
		{name: "future version", envelope: `"contract_version":3,"required_capabilities":[],`, wantErr: fusedobject.ExecutionContractReasonUnsupportedVersion},
		{name: "missing capabilities", envelope: `"contract_version":2,`, wantErr: fusedobject.ExecutionContractReasonMissingCapabilities},
		{name: "unknown capability", envelope: `"contract_version":2,"required_capabilities":["http.future.v1"],`, wantErr: fusedobject.ExecutionContractReasonUnsupportedCapability},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"data":{"serviceRuntimeContracts":[{` + test.envelope +
				`"service_id":"` + serviceID.String() + `","service_version_id":"` + versionID.String() + `","version":"2026-08-11","service":` +
				runtimeContractServiceJSON(serviceID, versionID, "Envelope") + `,"operations":[],"webhooks":[]}]}}`
			_, err := decodeRuntimeContractsResponse(strings.NewReader(body), requested)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("decodeRuntimeContractsResponse() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func runtimeContractServiceJSON(serviceID, _ uuid.UUID, name string) string {
	return `{
		"id":"` + serviceID.String() + `",
		"current_service_version":"2026-07-23",
		"name":"` + name + `",
		"description":"",
		"base_url":"https://api.example.com",
		"servers":[],
		"default_headers":{},
		"connect_config":null,
		"auth_configs":[],
		"rate_limit":null,
		"retry_config":null,
		"documentation":{"tags":[{"name":"orders","summary":"Order operations","description":"Order APIs","parent":"commerce","kind":"badge","external_docs":{"description":"Tag guide","url":"https://docs.example.com/tags/orders"}}]},
		"event_extraction_path":"",
		"incoming_webhook_config":null
	}`
}

// TestFetchConnectionProfileContractsUsesGraphQLBatch verifies workspace
// validation sends all version IDs through one authenticated GraphQL request.
func TestFetchConnectionProfileContractsUsesGraphQLBatch(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"connectionProfileContracts":[{"service_id":"` + uuid.NewString() + `","service_version_id":"` + first.String() + `","auth_types":["oauth2"],"servers":[],"operations":[]}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
	contracts, err := client.FetchConnectionProfileContracts(context.Background(), []uuid.UUID{first, second}, "fsk_test")
	if err != nil {
		t.Fatalf("FetchConnectionProfileContracts() error = %v", err)
	}
	if len(contracts) != 1 || contracts[0].ServiceVersionID != first {
		t.Fatalf("contracts = %#v", contracts)
	}
	if !strings.Contains(requestBody.Query, "connectionProfileContracts") {
		t.Fatalf("query = %q", requestBody.Query)
	}
	ids, ok := requestBody.Variables["ids"].([]interface{})
	if !ok || len(ids) != 2 {
		t.Fatalf("version IDs = %#v", requestBody.Variables["ids"])
	}
}

// TestVerifyServiceExists_ReturnsNameAndCurrentVersion covers the happy path:
// the Registry resolves the service and returns both the cached display name
// and the concrete version tag the Engine should pin for older clients.
func TestVerifyServiceExists_ReturnsNameAndCurrentVersion(t *testing.T) {
	svcID := uuid.New()
	versionID := uuid.New()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"service":{"id":"` + svcID.String() + `","name":"Test Service","slug":"test/test-service","current_service_version":"2023-01-01","service_versions":[{"id":"` + versionID.String() + `","name":"2023-01-01"}]}}}`))
	}))
	defer ts.Close()
	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(ts.URL, "engine-license-key")

	name, slug, currentVersionTag, gotServiceVersionID, err := c.VerifyServiceExists(context.Background(), svcID, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "Test Service" {
		t.Errorf("expected name %q, got %q", "Test Service", name)
	}
	if slug != "test/test-service" {
		t.Errorf("expected slug %q, got %q", "test/test-service", slug)
	}
	if currentVersionTag != "2023-01-01" {
		t.Errorf("expected current_service_version %q, got %q", "2023-01-01", currentVersionTag)
	}
	if gotServiceVersionID != versionID {
		t.Errorf("expected service_version_id %s, got %s", versionID, gotServiceVersionID)
	}
}

func TestFetchServiceVisibilityUsesGraphQLBatch(t *testing.T) {
	firstID := uuid.New()
	secondID := uuid.New()
	var gotQuery string
	var gotVariables map[string]interface{}
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Fatalf("expected GraphQL POST /graphql, got %s %s", r.Method, r.URL.Path)
		}
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		gotVariables = body.Variables
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"servicesByIds": []map[string]any{
					{"id": firstID.String(), "is_owner": true, "is_public": false, "provider": map[string]any{"name": "Mine", "handle": "mine"}, "canonical_ref": "@mine/first"},
					{"id": secondID.String(), "is_owner": false, "is_public": true, "provider": map[string]any{"name": "Acme", "handle": "acme"}, "canonical_ref": "@acme/second"},
				},
			},
		})
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	client := NewHTTPRegistryClient(registryMock.URL+"/graphql", "engine-license-key")
	got, err := client.FetchServiceVisibility(context.Background(), []uuid.UUID{firstID, secondID}, "user-key")
	if err != nil {
		t.Fatalf("FetchServiceVisibility: %v", err)
	}
	if !strings.Contains(gotQuery, "servicesByIds") {
		t.Fatalf("expected servicesByIds query, got %s", gotQuery)
	}
	if ids, ok := gotVariables["serviceIds"].([]interface{}); !ok || len(ids) != 2 {
		t.Fatalf("expected batched serviceIds variable, got %#v", gotVariables["serviceIds"])
	}
	if !got[firstID].IsOwner || got[firstID].IsPublic {
		t.Fatalf("unexpected first visibility: %+v", got[firstID])
	}
	if got[secondID].IsOwner || !got[secondID].IsPublic {
		t.Fatalf("unexpected second visibility: %+v", got[secondID])
	}
	if got[secondID].Provider.Name != "Acme" || got[secondID].Provider.Handle != "acme" || got[secondID].CanonicalRef != "@acme/second" {
		t.Fatalf("unexpected second provider identity: %+v", got[secondID])
	}
}

func TestUpdateServicePublicUsesGraphQLMutation(t *testing.T) {
	serviceID := uuid.New()
	var gotQuery string
	var gotKey string
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Fatalf("expected GraphQL POST /graphql, got %s %s", r.Method, r.URL.Path)
		}
		gotKey = r.Header.Get("X-API-Key")
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		if body.Variables["serviceId"] != serviceID.String() || body.Variables["isPublic"] != true {
			t.Fatalf("unexpected variables: %#v", body.Variables)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"updateServicePublic": true},
		})
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	client := NewHTTPRegistryClient(registryMock.URL+"/graphql", "engine-license-key")
	if err := client.UpdateServicePublic(context.Background(), serviceID, true, "user-key"); err != nil {
		t.Fatalf("UpdateServicePublic: %v", err)
	}
	if gotKey != "engine-license-key" {
		t.Fatalf("expected Engine licence key, got %q", gotKey)
	}
	if !strings.Contains(gotQuery, "mutation UpdateServicePublic") || !strings.Contains(gotQuery, "updateServicePublic") {
		t.Fatalf("expected updateServicePublic mutation, got %s", gotQuery)
	}
}

// TestVerifyServiceExists_NullService_ReturnsErrServiceNotFound covers both
// "service doesn't exist" and "service exists but caller isn't authorized to
// see it" -- the Registry's own service resolver returns null for both cases
// (schema.go: isAuthorizedForService returns nil, not an error), so this
// method surfaces both identically as ErrServiceNotFound.
func TestVerifyServiceExists_NullService_ReturnsErrServiceNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"service":null}}`))
	}))
	defer ts.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(ts.URL, "engine-license-key")

	_, _, _, _, err := c.VerifyServiceExists(context.Background(), uuid.New(), "user-supplied-api-key")
	if err == nil {
		t.Fatal("expected an error for a null service, got nil")
	}
	if err != ErrServiceNotFound {
		t.Errorf("expected ErrServiceNotFound, got %v", err)
	}
}

// TestFetchDriftSnapshotsForServices_SendsAllIDsInOneRequest is the
// regression guard for the drift-inbox N+1 fix on the Engine client side:
// asking about several services must be exactly one HTTP round trip
// carrying every service ID, not one request per service.
func TestFetchDriftSnapshotsForServices_SendsAllIDsInOneRequest(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	var requestCount int
	var gotVariables map[string]interface{}

	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var body graphqlQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotVariables = body.Variables

		_, _ = w.Write([]byte(`{"data":{"driftSnapshotsForServices":[
			{"id":"` + uuid.New().String() + `","integration_object_id":"` + uuid.New().String() + `","status":"pending","diff":[]}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	snapshots, err := c.FetchDriftSnapshotsForServices(context.Background(), []uuid.UUID{svcA, svcB}, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if requestCount != 1 {
		t.Fatalf("expected exactly 1 HTTP request for 2 services, got %d", requestCount)
	}
	ids, ok := gotVariables["serviceIds"].([]interface{})
	if !ok || len(ids) != 2 {
		t.Fatalf("expected both service IDs sent in one request, got %#v", gotVariables["serviceIds"])
	}
}

// TestFetchDriftSnapshotsForServices_EmptyInputSkipsRequest asserts the
// short-circuit for a workspace with no activated services -- no HTTP call
// should be made at all.
func TestFetchDriftSnapshotsForServices_EmptyInputSkipsRequest(t *testing.T) {
	var requestCount int
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		_, _ = w.Write([]byte(`{"data":{"driftSnapshotsForServices":[]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	snapshots, err := c.FetchDriftSnapshotsForServices(context.Background(), nil, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("expected no snapshots, got %d", len(snapshots))
	}
	if requestCount != 0 {
		t.Fatalf("expected no HTTP request for an empty service ID slice, got %d", requestCount)
	}
}

// TestVerifyServiceExists_RegistryUnreachable_ReturnsWrappedError covers the
// "Registry is down" case, which must be distinguishable from
// ErrServiceNotFound so the HTTP handler can return 502 instead of 404.
func TestVerifyServiceExists_RegistryUnreachable_ReturnsWrappedError(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient("http://127.0.0.1:0", "engine-license-key")

	_, _, _, _, err := c.VerifyServiceExists(context.Background(), uuid.New(), "user-supplied-api-key")
	if err == nil {
		t.Fatal("expected an error when the Registry is unreachable, got nil")
	}
	if err == ErrServiceNotFound {
		t.Error("a network failure must not be reported as ErrServiceNotFound")
	}
}

func TestFetchLatestServiceVersions_SendsAllIDsInOneGraphQLRequest(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	versionID := uuid.New()
	var requestCount int
	var gotAPIKey string
	var gotQuery string
	var gotVariables map[string]interface{}

	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		gotAPIKey = r.Header.Get("X-API-Key")
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		gotVariables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"latestServiceVersions":[
			{"service_id":"` + svcA.String() + `","version":"2026-07-15","service_version_id":"` + versionID.String() + `"}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	got, err := c.FetchLatestServiceVersions(context.Background(), []uuid.UUID{svcA, svcB}, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("FetchLatestServiceVersions: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("expected one GraphQL request, got %d", requestCount)
	}
	if gotAPIKey != "engine-license-key" {
		t.Fatalf("expected Engine licence key, got %q", gotAPIKey)
	}
	if !strings.Contains(gotQuery, "latestServiceVersions") {
		t.Fatalf("expected latestServiceVersions query, got %s", gotQuery)
	}
	ids, ok := gotVariables["serviceIds"].([]interface{})
	if !ok || len(ids) != 2 {
		t.Fatalf("expected both service IDs in variables, got %#v", gotVariables["serviceIds"])
	}
	if len(got) != 1 || got[0].ServiceID != svcA || got[0].Version != "2026-07-15" || got[0].ServiceVersionID != versionID {
		t.Fatalf("unexpected latest versions: %#v", got)
	}
}

// TestResolveServiceIDsBySlugs_SendsBatchedInputsInOneRequest is Task 5's
// core regression guard (engine_workspace_registration_plan.md): resolving
// several bare slugs must be exactly one Registry request against the
// batched serviceIdsBySlugs query, not the old per-slug GraphQL-alias trick.
// A bare slug (no "@provider/" prefix) must be sent with an empty provider,
// meaning "the caller's own account".
func TestResolveServiceIDsBySlugs_SendsBatchedInputsInOneRequest(t *testing.T) {
	oktaID := uuid.New()
	githubID := uuid.New()
	var calls int
	var gotAPIKey string
	var gotQuery string
	var gotVariables map[string]interface{}
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAPIKey = r.Header.Get("X-API-Key")
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		gotVariables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"serviceIdsBySlugs":[
			{"slug":"okta","provider":"","serviceId":"` + oktaID.String() + `"},
			{"slug":"github","provider":"","serviceId":"` + githubID.String() + `"}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	got, err := c.ResolveServiceIDsBySlugs(context.Background(), []string{"okta", "github"}, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("ResolveServiceIDsBySlugs: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one Registry request, got %d", calls)
	}
	if gotAPIKey != "engine-license-key" {
		t.Fatalf("expected Engine licence key, got %q", gotAPIKey)
	}
	if !strings.Contains(gotQuery, "serviceIdsBySlugs") {
		t.Fatalf("expected the batched serviceIdsBySlugs query, got %s", gotQuery)
	}
	inputs, ok := gotVariables["inputs"].([]interface{})
	if !ok || len(inputs) != 2 {
		t.Fatalf("expected 2 batched inputs, got %#v", gotVariables["inputs"])
	}
	first, _ := inputs[0].(map[string]interface{})
	if first["slug"] != "okta" || first["provider"] != "" {
		t.Errorf("expected bare slug sent with empty provider, got %#v", first)
	}
	if got["okta"] != oktaID || got["github"] != githubID {
		t.Fatalf("unexpected resolution: %#v", got)
	}
}

// TestResolveServiceIDsBySlugs_ParsesProviderPrefixFromKey is Task 5's
// provider-passthrough AC: a "@provider/slug" config map key must resolve
// against that provider's account, and the result map must still be keyed
// by the original composite string (unresolvedWorkspaceServiceSlugs and its
// SDK-config sibling look results up by the exact key they passed in).
func TestResolveServiceIDsBySlugs_ParsesProviderPrefixFromKey(t *testing.T) {
	crmID := uuid.New()
	var gotVariables map[string]interface{}
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body graphqlQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotVariables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"serviceIdsBySlugs":[
			{"slug":"custom-crm","provider":"acme-inc","serviceId":"` + crmID.String() + `"}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	got, err := c.ResolveServiceIDsBySlugs(context.Background(), []string{"@acme-inc/custom-crm"}, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("ResolveServiceIDsBySlugs: %v", err)
	}
	inputs, ok := gotVariables["inputs"].([]interface{})
	if !ok || len(inputs) != 1 {
		t.Fatalf("expected 1 batched input, got %#v", gotVariables["inputs"])
	}
	sent, _ := inputs[0].(map[string]interface{})
	if sent["slug"] != "custom-crm" || sent["provider"] != "acme-inc" {
		t.Fatalf("expected slug/provider split from the composite key, got %#v", sent)
	}
	if got["@acme-inc/custom-crm"] != crmID {
		t.Fatalf("expected result keyed by the original composite key, got %#v", got)
	}
}

// TestResolveServiceIDsBySlugs_UnresolvedSlugOmittedFromMap mirrors the old
// alias-trick behavior: a slug the Registry couldn't resolve (null
// serviceId) must be absent from the result map, not present with a zero
// UUID -- callers (unresolvedWorkspaceServiceSlugs) distinguish "found" from
// "not found" via ok, not by checking for uuid.Nil.
func TestResolveServiceIDsBySlugs_UnresolvedSlugOmittedFromMap(t *testing.T) {
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"serviceIdsBySlugs":[
			{"slug":"no-such-service","provider":"","serviceId":null}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	got, err := c.ResolveServiceIDsBySlugs(context.Background(), []string{"no-such-service"}, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("ResolveServiceIDsBySlugs: %v", err)
	}
	if _, ok := got["no-such-service"]; ok {
		t.Errorf("expected unresolved slug to be absent from the map, got %#v", got)
	}
}

// TestResolveServiceIDsBySlugs_EmptyInputReturnsEmptyMapNoRequest guards the
// pre-existing short-circuit: no slugs means no Registry round trip at all.
func TestResolveServiceIDsBySlugs_EmptyInputReturnsEmptyMapNoRequest(t *testing.T) {
	var calls int
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"serviceIdsBySlugs":[]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	got, err := c.ResolveServiceIDsBySlugs(context.Background(), nil, "user-supplied-api-key")
	if err != nil {
		t.Fatalf("ResolveServiceIDsBySlugs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %#v", got)
	}
	if calls != 0 {
		t.Errorf("expected no Registry request for empty input, got %d", calls)
	}
}

// TestParseSlugProviderKey covers parseSlugProviderKey's own splitting rules
// directly, since ResolveServiceIDsBySlugs' tests above only exercise it
// indirectly through the sent GraphQL variables.
func TestParseSlugProviderKey(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		wantSlug     string
		wantProvider string
	}{
		{"bare slug", "stripe", "stripe", ""},
		{"provider-prefixed", "@acme-inc/custom-crm", "custom-crm", "acme-inc"},
		{"malformed no slash", "@acme-inc", "@acme-inc", ""},
		{"empty string", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, provider := parseSlugProviderKey(tt.key)
			if slug != tt.wantSlug || provider != tt.wantProvider {
				t.Errorf("parseSlugProviderKey(%q) = (%q, %q), want (%q, %q)", tt.key, slug, provider, tt.wantSlug, tt.wantProvider)
			}
		})
	}
}

func TestFetchEndpointsByNames_UsesSingleGraphQLRequest(t *testing.T) {
	serviceID := uuid.New()
	var calls int
	var gotQuery string
	var gotVariables map[string]interface{}
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		gotVariables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"endpointsByNames":[
			{"id":"` + uuid.New().String() + `","name":"listLogEvents","method":"POST","path":"/logs","request_content":{"representations":[{"media_type":"multipart/form-data","serialization":"multipart","encoding":{"attachment":{"content_type":"application/pdf","binary_encoding":"base64"}}}]},"pagination":{"version":3,"request":[{"state":"cursor","target":{"location":"query","name":"after"},"value_type":"string","apply":"all"}],"response":{"items":{"path":"$.items"},"values":[{"name":"next","source":{"location":"body","path":"$.page.next","value_type":"string"}}]},"continuation":[{"kind":"token","state":"cursor","response_value":"next"}],"termination":{"stop_on_empty_items":true,"stop_on_missing_values":["next"],"repeated_value":"stop"},"limits":{"max_pages":100,"max_items":10000,"max_bytes":16777216,"max_duration_ms":120000}}},
			{"id":"` + uuid.New().String() + `","name":"getUser","method":"GET","path":"/users/{id}","parameters":[{"name":"id","in":"path","required":true,"type":"string","path_encoding":"preserve_slashes"}]}
		]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")
	serviceVersionID := uuid.New()

	endpoints, err := c.FetchEndpointsByNames(context.Background(), serviceID, serviceVersionID, []string{"listLogEvents", "getUser"})
	if err != nil {
		t.Fatalf("FetchEndpointsByNames: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one Registry request, got %d", calls)
	}
	if !strings.Contains(gotQuery, "endpointsByNames") {
		t.Fatalf("expected endpointsByNames query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "pagination") {
		t.Fatalf("expected endpoint pagination in query, got %s", gotQuery)
	}
	for _, field := range []string{"request_content", "path_encoding", "graphql_query", "provider_protocol", "operation_kind", "responses"} {
		if !strings.Contains(gotQuery, field) {
			t.Fatalf("expected %s in endpoint projection, got %s", field, gotQuery)
		}
	}
	names, ok := gotVariables["names"].([]interface{})
	if !ok || len(names) != 2 {
		t.Fatalf("expected both endpoint names in one request, got %#v", gotVariables["names"])
	}
	if gotVariables["serviceVersionId"] != serviceVersionID.String() {
		t.Fatalf("expected serviceVersionId %s, got %#v", serviceVersionID, gotVariables["serviceVersionId"])
	}
	if len(endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(endpoints))
	}
	if endpoints[0].Pagination == nil || endpoints[0].Pagination.Request[0].Target.Name != "after" || endpoints[0].Pagination.Response.Values[0].Source.Path != "$.page.next" {
		t.Fatalf("expected decoded runtime pagination, got %+v", endpoints[0].Pagination)
	}
	if endpoints[0].RequestContent == nil || endpoints[0].RequestContent.Representations[0].Serialization != "multipart" || endpoints[0].RequestContent.Representations[0].Encoding["attachment"].BinaryEncoding != "base64" {
		t.Fatalf("expected decoded request content, got %#v", endpoints[0].RequestContent)
	}
	if len(endpoints[1].Parameters) != 1 || endpoints[1].Parameters[0].PathEncoding != "preserve_slashes" {
		t.Fatalf("expected decoded path encoding, got %#v", endpoints[1].Parameters)
	}
}

func TestFetchEndpointByName_UsesServiceVersionIDNativeQuery(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	var gotQuery string
	var gotVariables map[string]interface{}
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		gotVariables = body.Variables
		_, _ = w.Write([]byte(`{"data":{"endpointsByNames":[{"id":"` + endpointID.String() + `","name":"meta/get","method":"GET","path":"/meta"}]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	c := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")

	endpoint, err := c.FetchEndpointByName(context.Background(), serviceID, serviceVersionID.String(), "meta/get")
	if err != nil {
		t.Fatalf("FetchEndpointByName: %v", err)
	}
	if endpoint.ID != endpointID {
		t.Fatalf("expected endpoint %s, got %s", endpointID, endpoint.ID)
	}
	if !strings.Contains(gotQuery, "endpointsByNames") || strings.Contains(gotQuery, "endpointByName") {
		t.Fatalf("expected version-id-native endpointsByNames query, got %s", gotQuery)
	}
	if gotVariables["serviceVersionId"] != serviceVersionID.String() {
		t.Fatalf("expected serviceVersionId %s, got %#v", serviceVersionID, gotVariables["serviceVersionId"])
	}
}

func TestFetchServiceOperations_RequestsAndDecodesRequestContent(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	var gotQuery string
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		_, _ = w.Write([]byte(`{"data":{"serviceOperations":[{"id":"` + endpointID.String() + `","name":"uploadWidget","method":"POST","path":"/widgets","request_content":{"payload_parameter":"body","representations":[{"media_type":"application/octet-stream","serialization":"raw","schema":{"projection":{"type":"string","format":"binary"}}}]}}]}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	client := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")
	operations, err := client.FetchServiceOperations(context.Background(), serviceID, serviceVersionID)
	if err != nil {
		t.Fatalf("FetchServiceOperations: %v", err)
	}
	if !strings.Contains(gotQuery, "request_content") {
		t.Fatalf("service operations projection omitted request_content: %s", gotQuery)
	}
	if len(operations) != 1 || operations[0].RequestContent == nil || operations[0].RequestContent.PayloadParameter != "body" || operations[0].RequestContent.Representations[0].Schema.Projection.Format != "binary" {
		t.Fatalf("expected decoded operation request content, got %#v", operations)
	}
}

func TestFetchServiceMetadata_RequestsAndDecodesRawProviderRuntimeFields(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	var gotQuery string
	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body graphqlQuery
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotQuery = body.Query
		_, _ = w.Write([]byte(`{"data":{"service":{"id":"` + serviceID.String() + `","current_service_version":"2026-07-15","service_versions":[{"id":"` + serviceVersionID.String() + `","name":"2026-07-15","contract_version":2,"required_capabilities":[]}],"name":"Widgets","description":"API","base_url":"https://provider.example.test","servers":[{"url":"https://provider.example.test","description":"Live endpoint","environment":"prod","is_default":true},{"url":"https://sandbox.example.test","description":"Developer test area","environment":"sandbox"}],"auth_configs":[{"name":"oauth","type":"oauth2","token_endpoint_auth_method":"client_secret_basic"}],"rate_limit":{"version":3,"policies":[{"name":"requests","mode":"enforce","unit":"requests","identity":{"inputs":[{"kind":"service_version"}]},"cost":{"default":1,"rules":[]},"algorithm":"fixed_window","fixed_window":{"limit":3,"duration_ms":1000}}]},"retry_config":{"version":3,"rules":[{"predicates":{"methods":["GET"],"operation_kinds":["read"],"statuses":[{"min":500,"max":599}],"errors":[],"body_replayability":"any","idempotency_key":{"requirement":"any"},"required_provider_headers":[]},"action":{"max_attempts":2,"max_elapsed_ms":1000,"backoff":{"strategy":"fixed","base_delay_ms":0,"max_delay_ms":1,"jitter_ms":0},"retry_after_headers":[]}}]},"default_headers":{"X-Tenant":"one"}}}}`))
	}))
	defer registryMock.Close()

	t.Setenv("FUSED_ENV", "development")
	client := NewHTTPRegistryClient(registryMock.URL, "engine-license-key")
	metadata, err := client.FetchServiceMetadata(context.Background(), serviceID, "2026-07-15")
	if err != nil {
		t.Fatalf("FetchServiceMetadata: %v", err)
	}
	for _, field := range []string{"base_url", "environment", "is_default", "servers", "default_headers", "rate_limit", "retry_config"} {
		if !strings.Contains(gotQuery, field) {
			t.Fatalf("metadata query omitted %s: %s", field, gotQuery)
		}
	}
	if strings.Contains(gotQuery, "provider_base_url") || strings.Contains(gotQuery, "provider_servers") {
		t.Fatalf("metadata query must not request legacy provider-prefixed fields: %s", gotQuery)
	}
	if strings.Contains(gotQuery, "service_version_id") {
		t.Fatalf("metadata query must not request removed root service_version_id field: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "current_service_version") || !strings.Contains(gotQuery, "service_versions") || !strings.Contains(gotQuery, "contract_version") || !strings.Contains(gotQuery, "required_capabilities") {
		t.Fatalf("metadata query must resolve service version identity through service_versions: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "auth_configs") || !strings.Contains(gotQuery, "name") || !strings.Contains(gotQuery, "token_endpoint_auth_method") {
		t.Fatalf("metadata query must request auth config names for connected-auth injection: %s", gotQuery)
	}
	if metadata.ServiceVersionID != serviceVersionID {
		t.Fatalf("expected service version id %s, got %s", serviceVersionID, metadata.ServiceVersionID)
	}
	if len(metadata.AuthConfigs) != 1 || metadata.AuthConfigs[0].Name != "oauth" || metadata.AuthConfigs[0].TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("expected auth config name to survive runtime metadata fetch, got %+v", metadata.AuthConfigs)
	}
	if metadata.BaseURL != "https://provider.example.test" || metadata.DefaultHeaders["X-Tenant"] != "one" {
		t.Fatalf("effective runtime metadata was not decoded: %+v", metadata)
	}
	if len(metadata.Servers) != 2 || metadata.Servers[0].Environment != "prod" || !metadata.Servers[0].IsDefault || metadata.Servers[1].Environment != "sandbox" {
		t.Fatalf("expected decoded runtime servers, got %+v", metadata.Servers)
	}
}
