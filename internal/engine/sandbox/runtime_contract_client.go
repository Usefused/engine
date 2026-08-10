package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/google/uuid"
)

type RuntimeContractFetcher interface {
	FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error)
}

type BatchRuntimeContractFetcher interface {
	FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error)
}

func (c *HTTPRegistryClient) FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error) {
	snapshots, err := c.FetchRuntimeContracts(ctx, []store.WorkspaceServiceVersion{{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: version}}, apiKey)
	if err != nil {
		return nil, err
	}
	if len(snapshots) != 1 {
		return nil, fmt.Errorf("FetchRuntimeContract: service %s version %s not found", serviceID, serviceVersionID)
	}
	return &snapshots[0], nil
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

func (c *HTTPRegistryClient) buildRuntimeContractsRequest(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) (*http.Request, error) {
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query:     runtimeContractsQuery,
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

func runtimeContractBatchVariables(versions []store.WorkspaceServiceVersion) map[string]interface{} {
	refs := make([]ServiceVersionRef, 0, len(versions))
	for _, version := range versions {
		refs = append(refs, ServiceVersionRef{ServiceID: version.ServiceID, Version: version.ServiceVersionID.String()})
	}
	return map[string]interface{}{"refs": refs}
}

const runtimeContractsQuery = `
	query EngineRuntimeContracts($refs: [ServiceVersionRefInput!]!) {
		serviceRuntimeContracts(refs: $refs) {
			service_id
			service_version_id
			version
			service {` + runtimeContractServiceFields + `}
			operations {` + runtimeContractOperationFields + `}
			webhooks {
				id service_id name method description request_body
			}
		}
	}
`

const runtimeContractServiceFields = `
	id
	current_service_version
	name
	description
	base_url
	servers {
		url
		description
		environment
		is_default
		variables { name default enum required }
	}
	default_headers
	connect_config
	auth_configs {
		name
		type
		flow
		scheme
		basic_password_mode
		location
		key_name
		token_url
		authorization_url
		open_id_connect_url
		scopes
	}
	rate_limit {` + runtimeRateLimitFields + `}
	retry_config {
		strategy
		max_retries
		backoff_ms
	}
	timeout_ms
	pagination {` + runtimePaginationFields + `}
	event_extraction_path
	incoming_webhook_config {
		auth_type
		auth_location
		auth_key_name
		signature_header
		verification_headers
	}
`

const runtimeContractOperationFields = `
	id
	stable_key
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
		path_encoding
	}
	request_content
	responses
	graphql_query
	provider_protocol
	operation_kind
	pagination {` + runtimePaginationFields + `}
	security_requirements {` + runtimeSecurityRequirementFields + `}
`

const runtimeRateLimitFields = `
	version
	policies {
		name
		unit
		scope
		default_cost
		operation_costs
		algorithm
		fixed_window { limit duration_ms }
		token_bucket { capacity refill_units refill_interval_ms }
		response_headers {
			limit
			remaining
			reset { name format }
		}
	}
	retry_after { enabled max_delay_ms }
`

const runtimeSecurityRequirementFields = `
	schemes { scheme scopes }
`

const runtimePaginationFields = `
	version
	type
	items_path
	limits { max_pages max_items max_bytes max_duration_ms }
	cursor {
		request { location name }
		initial { type string integer }
		next { location path name relation value_type }
		has_more { location path name relation value_type }
	}
	offset {
		request { location name }
		start
		increment { mode value }
		page_size { target { location name } value }
		next_offset { location path name relation value_type }
		total_items { location path name relation value_type }
		has_more { location path name relation value_type }
		stop_on_short_page
	}
	page_number {
		request { location name }
		start
		increment
		page_size { target { location name } value }
		total_pages { location path name relation value_type }
		has_more { location path name relation value_type }
		stop_on_short_page
	}
	next_url { next { location path name relation value_type } }
`

type runtimeContractsGraphQLResponse struct {
	Data struct {
		Contracts []runtimeContractBatchItem `json:"serviceRuntimeContracts"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type runtimeContractBatchItem struct {
	ServiceID        uuid.UUID               `json:"service_id"`
	ServiceVersionID uuid.UUID               `json:"service_version_id"`
	Version          string                  `json:"version"`
	Service          *runtimeContractService `json:"service"`
	Operations       []fusedobject.Endpoint  `json:"operations"`
	Webhooks         []fusedobject.Webhook   `json:"webhooks"`
}

func decodeRuntimeContractsResponse(body io.Reader, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	var decoded runtimeContractsGraphQLResponse
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchRuntimeContracts: graphql error: %s", decoded.Errors[0].Message)
	}
	return runtimeContractSnapshotsFromBatch(decoded.Data.Contracts, versions)
}

func runtimeContractSnapshotsFromBatch(items []runtimeContractBatchItem, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	byVersion := make(map[uuid.UUID]runtimeContractBatchItem, len(items))
	for _, item := range items {
		byVersion[item.ServiceVersionID] = item
	}
	out := make([]store.ServiceContractSnapshot, 0, len(versions))
	for _, version := range versions {
		snapshot, err := runtimeContractSnapshotFromBatchItem(byVersion[version.ServiceVersionID], version)
		if err != nil {
			return nil, err
		}
		out = append(out, *snapshot)
	}
	return out, nil
}

func runtimeContractSnapshotFromBatchItem(item runtimeContractBatchItem, requested store.WorkspaceServiceVersion) (*store.ServiceContractSnapshot, error) {
	if item.Service == nil || item.ServiceID != requested.ServiceID || item.ServiceVersionID != requested.ServiceVersionID {
		return nil, fmt.Errorf("FetchRuntimeContracts: service %s version %s not found", requested.ServiceID, requested.ServiceVersionID)
	}
	if err := validateRuntimePagination(item.Service, item.Operations); err != nil {
		return nil, err
	}
	version := item.Version
	if version == "" {
		version = requested.Version
	}
	snapshot := runtimeContractSnapshot(requested.ServiceID, requested.ServiceVersionID, version, item.Service, item.Operations, item.Webhooks)
	if err := validateRuntimeSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateRuntimeSnapshot(snapshot *store.ServiceContractSnapshot) error {
	if err := validateTransportContract(&snapshot.ServiceMetadata, snapshot.Endpoints); err != nil {
		return fmt.Errorf("FetchRuntimeContract: invalid transport contract: %w", err)
	}
	return nil
}

func validateRuntimePagination(service *runtimeContractService, operations []fusedobject.Endpoint) error {
	if err := validateRuntimePaginationConfig("service", service.Pagination); err != nil {
		return err
	}
	for i := range operations {
		if err := validateRuntimePaginationConfig("operation", operations[i].Pagination); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimePaginationConfig(scope string, config *fusedobject.PaginationConfig) error {
	if config == nil {
		return nil
	}
	if err := paginationpolicy.Validate(config); err != nil {
		return fmt.Errorf("FetchRuntimeContract: invalid %s pagination: %w", scope, err)
	}
	return nil
}

type runtimeContractService struct {
	ID                    uuid.UUID                          `json:"id"`
	CurrentServiceVersion string                             `json:"current_service_version"`
	Name                  string                             `json:"name"`
	Description           string                             `json:"description"`
	BaseURL               string                             `json:"base_url"`
	Servers               fusedobject.Servers                `json:"servers"`
	AuthConfigs           fusedobject.AuthConfigs            `json:"auth_configs"`
	RateLimit             *fusedobject.RateLimitConfig       `json:"rate_limit"`
	RetryConfig           *fusedobject.RetryConfig           `json:"retry_config"`
	TimeoutMs             *int                               `json:"timeout_ms"`
	Pagination            *fusedobject.PaginationConfig      `json:"pagination"`
	DefaultHeaders        fusedobject.DefaultHeaders         `json:"default_headers"`
	ConnectConfig         *fusedobject.ServiceConnectConfig  `json:"connect_config"`
	EventExtractionPath   string                             `json:"event_extraction_path"`
	IncomingWebhookConfig *fusedobject.IncomingWebhookConfig `json:"incoming_webhook_config"`
}

func runtimeContractSnapshot(serviceID, serviceVersionID uuid.UUID, version string, service *runtimeContractService, operations []fusedobject.Endpoint, webhooks []fusedobject.Webhook) *store.ServiceContractSnapshot {
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
		TimeoutMs:             service.TimeoutMs,
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
		Webhooks:         webhooks,
	}
}
