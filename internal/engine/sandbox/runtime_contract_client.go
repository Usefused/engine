package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type RuntimeContractFetcher interface {
	FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error)
}

type BatchRuntimeContractFetcher interface {
	FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error)
}

func (c *HTTPRegistryClient) FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error) {
	req, err := c.buildRuntimeContractRequest(ctx, serviceID, serviceVersionID, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("FetchRuntimeContract: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("FetchRuntimeContract: registry returned %d: %s", resp.StatusCode, string(body))
	}

	var decoded runtimeContractGraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchRuntimeContract: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchRuntimeContract: graphql error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.Service == nil {
		return nil, fmt.Errorf("FetchRuntimeContract: service %s version %s not found", serviceID, serviceVersionID)
	}
	return runtimeContractSnapshot(serviceID, serviceVersionID, version, decoded.Data.Service, decoded.Data.ServiceOperations), nil
}

func (c *HTTPRegistryClient) FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error) {
	if len(versions) == 0 {
		return nil, nil
	}
	req, err := c.buildRuntimeContractsRequest(ctx, versions, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("FetchRuntimeContracts: registry returned %d: %s", resp.StatusCode, string(body))
	}
	return decodeRuntimeContractsResponse(resp.Body, versions)
}

func (c *HTTPRegistryClient) buildRuntimeContractRequest(ctx context.Context, serviceID, serviceVersionID uuid.UUID, apiKey string) (*http.Request, error) {
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query: runtimeContractQuery,
		Variables: map[string]interface{}{
			"serviceId":        serviceID.String(),
			"serviceVersionId": serviceVersionID.String(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("FetchRuntimeContract: create request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-API-Key", apiKey)
	}
	return req, nil
}

func (c *HTTPRegistryClient) buildRuntimeContractsRequest(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) (*http.Request, error) {
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query:     buildRuntimeContractsQuery(len(versions)),
		Variables: runtimeContractBatchVariables(versions),
	})
	if err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: create request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-API-Key", apiKey)
	}
	return req, nil
}

func buildRuntimeContractsQuery(count int) string {
	var variables strings.Builder
	var body strings.Builder
	for i := 0; i < count; i++ {
		if i > 0 {
			variables.WriteString(", ")
		}
		variables.WriteString(fmt.Sprintf("$serviceId%d: String!, $serviceVersionId%d: String!", i, i))
		body.WriteString(fmt.Sprintf(`
		service%d: service(id: $serviceId%d, version: $serviceVersionId%d) {%s}
		serviceOperations%d: serviceOperations(serviceId: $serviceId%d, version: $serviceVersionId%d) {%s}
`, i, i, i, runtimeContractServiceFields, i, i, i, runtimeContractOperationFields))
	}
	return fmt.Sprintf("query EngineRuntimeContracts(%s) {%s\n}", variables.String(), body.String())
}

func runtimeContractBatchVariables(versions []store.WorkspaceServiceVersion) map[string]interface{} {
	variables := make(map[string]interface{}, len(versions)*2)
	for i, version := range versions {
		variables[fmt.Sprintf("serviceId%d", i)] = version.ServiceID.String()
		variables[fmt.Sprintf("serviceVersionId%d", i)] = version.ServiceVersionID.String()
	}
	return variables
}

const runtimeContractQuery = `
	query EngineRuntimeContract($serviceId: String!, $serviceVersionId: String!) {
		service(id: $serviceId, version: $serviceVersionId) {` + runtimeContractServiceFields + `}
		serviceOperations(serviceId: $serviceId, version: $serviceVersionId) {` + runtimeContractOperationFields + `}
	}
`

const runtimeContractServiceFields = `
	id
	current_service_version
	service_versions {
		id
		name
	}
	name
	description
	base_url
	servers {
		url
		description
		environment
		is_default
	}
	default_headers
	connect_config
	auth_configs {
		name
		type
		flow
		scheme
		location
		key_name
		token_url
		authorization_url
		open_id_connect_url
		scopes
	}
	rate_limit {
		strategy
		requests_per_second
		requests_per_minute
	}
	retry_config {
		strategy
		max_retries
		backoff_ms
	}
	pagination {
		type
		request_param
		response_path
	}
	event_extraction_path
	incoming_webhook_config {
		auth_type
		auth_location
		auth_key_name
		signature_header
		verification_headers
	}
	webhooks {
		id
		service_id
		name
		method
		description
		request_body
	}
`

const runtimeContractOperationFields = `
	id
	name
	description
	resource_name
	version
	method
	path
	normalized_path
	deprecated
	is_sse
	parameters {
		name
		in
		required
		type
		description
	}
	request_body
	responses
	graphql_query
	provider_protocol
	operation_kind
	pagination
`

type runtimeContractGraphQLResponse struct {
	Data struct {
		Service           *runtimeContractService `json:"service"`
		ServiceOperations []fusedobject.Endpoint  `json:"serviceOperations"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type runtimeContractsGraphQLResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func decodeRuntimeContractsResponse(body io.Reader, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	var decoded runtimeContractsGraphQLResponse
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchRuntimeContracts: graphql error: %s", decoded.Errors[0].Message)
	}
	return runtimeContractSnapshotsFromBatch(decoded.Data, versions)
}

func runtimeContractSnapshotsFromBatch(data map[string]json.RawMessage, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	out := make([]store.ServiceContractSnapshot, 0, len(versions))
	for i, version := range versions {
		snapshot, err := runtimeContractSnapshotFromBatchItem(data, version, i)
		if err != nil {
			return nil, err
		}
		out = append(out, *snapshot)
	}
	return out, nil
}

func runtimeContractSnapshotFromBatchItem(data map[string]json.RawMessage, version store.WorkspaceServiceVersion, index int) (*store.ServiceContractSnapshot, error) {
	service, err := decodeRuntimeContractService(data, fmt.Sprintf("service%d", index), version)
	if err != nil {
		return nil, err
	}
	operations, err := decodeRuntimeContractOperations(data, fmt.Sprintf("serviceOperations%d", index), version)
	if err != nil {
		return nil, err
	}
	return runtimeContractSnapshot(version.ServiceID, version.ServiceVersionID, version.Version, service, operations), nil
}

func decodeRuntimeContractService(data map[string]json.RawMessage, key string, version store.WorkspaceServiceVersion) (*runtimeContractService, error) {
	var service *runtimeContractService
	if err := json.Unmarshal(data[key], &service); err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: decode %s: %w", key, err)
	}
	if service == nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: service %s version %s not found", version.ServiceID, version.ServiceVersionID)
	}
	return service, nil
}

func decodeRuntimeContractOperations(data map[string]json.RawMessage, key string, version store.WorkspaceServiceVersion) ([]fusedobject.Endpoint, error) {
	var operations []fusedobject.Endpoint
	if err := json.Unmarshal(data[key], &operations); err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: decode operations for %s version %s: %w", version.ServiceID, version.ServiceVersionID, err)
	}
	return operations, nil
}

type runtimeContractService struct {
	ID                    uuid.UUID                          `json:"id"`
	CurrentServiceVersion string                             `json:"current_service_version"`
	ServiceVersions       []runtimeContractServiceVersion    `json:"service_versions"`
	Name                  string                             `json:"name"`
	Description           string                             `json:"description"`
	BaseURL               string                             `json:"base_url"`
	Servers               fusedobject.Servers                `json:"servers"`
	AuthConfigs           fusedobject.AuthConfigs            `json:"auth_configs"`
	RateLimit             *fusedobject.RateLimitConfig       `json:"rate_limit"`
	RetryConfig           *fusedobject.RetryConfig           `json:"retry_config"`
	Pagination            *fusedobject.PaginationConfig      `json:"pagination"`
	DefaultHeaders        fusedobject.DefaultHeaders         `json:"default_headers"`
	ConnectConfig         *fusedobject.ServiceConnectConfig  `json:"connect_config"`
	EventExtractionPath   string                             `json:"event_extraction_path"`
	IncomingWebhookConfig *fusedobject.IncomingWebhookConfig `json:"incoming_webhook_config"`
	Webhooks              []fusedobject.Webhook              `json:"webhooks"`
}

type runtimeContractServiceVersion struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func runtimeContractSnapshot(serviceID, serviceVersionID uuid.UUID, version string, service *runtimeContractService, operations []fusedobject.Endpoint) *store.ServiceContractSnapshot {
	if version == "" {
		version = runtimeContractVersionName(serviceVersionID, service.CurrentServiceVersion, service.ServiceVersions)
	}
	metadata := fusedobject.ServiceMetadata{
		ID:                    service.ID,
		ServiceVersionID:      serviceVersionID,
		Name:                  service.Name,
		Description:           service.Description,
		BaseURL:               service.BaseURL,
		Servers:               service.Servers,
		AuthConfigs:           service.AuthConfigs,
		RateLimit:             service.RateLimit,
		RetryConfig:           service.RetryConfig,
		Pagination:            service.Pagination,
		DefaultHeaders:        service.DefaultHeaders,
		ConnectConfig:         service.ConnectConfig,
		EventExtractionPath:   service.EventExtractionPath,
		IncomingWebhookConfig: service.IncomingWebhookConfig,
	}
	return &store.ServiceContractSnapshot{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		Version:          version,
		Status:           "active",
		ServiceMetadata:  metadata,
		Endpoints:        operations,
		Webhooks:         service.Webhooks,
	}
}

func runtimeContractVersionName(serviceVersionID uuid.UUID, current string, versions []runtimeContractServiceVersion) string {
	for _, version := range versions {
		if version.ID == serviceVersionID {
			return version.Name
		}
	}
	return current
}
