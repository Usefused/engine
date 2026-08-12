package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	snapshots, err := decodeRuntimeContractsResponse(resp.Body, versions)
	recordPassiveContractSummary(ctx, snapshots, err)
	return snapshots, err
}

// recordPassiveContractSummary reports bounded aggregate evidence rather than
// copying provider documentation or runtime expressions into telemetry.
func recordPassiveContractSummary(ctx context.Context, snapshots []store.ServiceContractSnapshot, validationErr error) {
	callbacks, webhooks, links := passiveContractCounts(snapshots)
	outcome := "accepted"
	if validationErr != nil {
		outcome = "rejected"
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("contract.callback_count", boundedPassiveCount(callbacks)),
		attribute.Int("contract.webhook_count", boundedPassiveCount(webhooks)),
		attribute.Int("contract.link_count", boundedPassiveCount(links)),
		attribute.String("contract.passive_validation_outcome", outcome),
	)
}

func passiveContractCounts(snapshots []store.ServiceContractSnapshot) (int, int, int) {
	callbacks, webhooks, links := 0, 0, 0
	for _, snapshot := range snapshots {
		for _, endpoint := range snapshot.Endpoints {
			links += responseLinkCount(endpoint.Responses)
		}
		for _, webhook := range snapshot.Webhooks {
			if webhook.Contract != nil && webhook.Contract.Kind == fusedobject.InboundOperationKindCallback {
				callbacks++
			} else {
				webhooks++
			}
			if webhook.Contract != nil {
				links += responseLinkCount(webhook.Contract.Responses)
			}
		}
	}
	return callbacks, webhooks, links
}

func responseLinkCount(responses fusedobject.Responses) int {
	count := 0
	for _, response := range responses {
		count += len(response.Links)
	}
	return count
}

func boundedPassiveCount(value int) int {
	if value > 10000 {
		return 10000
	}
	return value
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
	support := fusedobject.EngineExecutionContractSupport()
	return map[string]interface{}{
		"refs":                    refs,
		"engine_contract_version": support.ContractVersion,
		"engine_capabilities":     support.RequiredCapabilities,
	}
}

const runtimeContractsQuery = `
	query EngineRuntimeContracts($refs: [ServiceVersionRefInput!]!, $engine_contract_version: Int!, $engine_capabilities: [String!]!) {
		serviceRuntimeContracts(refs: $refs, engine_contract_version: $engine_contract_version, engine_capabilities: $engine_capabilities) {
			contract_version
			required_capabilities
			service_id
			service_version_id
			version
			service {` + runtimeContractServiceFields + `}
			operations {` + runtimeContractOperationFields + `}
			webhooks {
				id service_id name method description request_body contract
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
		name
		description
		environment
		is_default
		variables { name default enum required }
	}
	default_headers
	connect_config
	auth_configs {` + registryAuthConfigGraphQLFields + `}
	rate_limit {` + runtimeRateLimitFields + `}
	retry_config {` + runtimeRetryFields + `}
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
	documentation
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
	operation_servers {
		url
		name
		description
		environment
		is_default
		variables { name default enum required }
	}
	deprecated
	parameters {
		name
		in
		required
		type
		description
		path_encoding
		serialization { style explode allow_reserved allow_empty_value }
		schema
		content
		deprecated
		example
		examples
	}
	request_content
	responses
	graphql_query
	provider_protocol
	operation_kind
	pagination {` + runtimePaginationFields + `}
	security_requirements {` + runtimeSecurityRequirementFields + `}
	documentation
`

const runtimeRateLimitFields = `
	version
	policies {
		name
		mode
		unit
		identity { inputs { kind binding name } }
		cost { default rules { operation cost } }
		algorithm
		fixed_window { limit duration_ms }
		rolling_window { limit duration_ms }
		token_bucket { capacity refill_units refill_interval_ms }
		concurrency { limit }
		response_signals {
			limit { source name path }
			remaining { source name path }
			reset { signal { source name path } format }
			cost { source name path }
		}
	}
	cooldown {
		statuses { min max }
		headers { name formats max_delay_ms }
	}
`

const runtimeRetryFields = `
	version
	rules {
		predicates {
			methods operation_kinds statuses { min max } errors body_replayability
			idempotency_key { requirement header }
			required_provider_headers
		}
		action {
			max_attempts max_elapsed_ms
			backoff { strategy base_delay_ms max_delay_ms jitter_ms }
			retry_after_headers { name formats max_delay_ms }
		}
	}
`

const runtimeSecurityRequirementFields = `
	schemes { scheme scopes }
	server_selection
`

const runtimePaginationFields = `
	version
	request {
		state
		target { location name }
		value_type
		initial { type string integer boolean }
		constant { type string integer boolean }
		apply
	}
	response {
		items {
			path
			paths { path when { state operator value { type string integer boolean } } }
		}
		values {
			name
			source {
				location path name relation value_type
				paths { path when { state operator value { type string integer boolean } } }
				item { position path }
			}
		}
	}
	continuation {
		kind state response_value
		increment { mode value }
		origin { mode allowed_origins }
	}
	termination {
		stop_on_empty_items
		stop_on_short_page { request_state }
		stop_on_missing_values
		conditions { response_value state operator value { type string integer boolean } }
		repeated_value
	}
	graphql {
		variables { name state value_type }
		result_aliases { name alias }
		first_page_template
		subsequent_page_template
	}
	limits { max_pages max_items max_bytes max_duration_ms }
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
	fusedobject.ExecutionContractEnvelope
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
		return nil, fmt.Errorf("FetchRuntimeContracts: incompatible contract: %w", unsupportedExecutionCapability())
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchRuntimeContracts: graphql error: %s", decoded.Errors[0].Message)
	}
	if err := validateRuntimeContractBatchEnvelopes(decoded.Data.Contracts); err != nil {
		return nil, err
	}
	return runtimeContractSnapshotsFromBatch(decoded.Data.Contracts, versions)
}

// validateRuntimeContractBatchEnvelopes validates the complete batch before any
// snapshot is persisted so incompatible services cannot be partially activated.
func validateRuntimeContractBatchEnvelopes(items []runtimeContractBatchItem) error {
	for _, item := range items {
		if err := fusedobject.ValidateExecutionContractEnvelope(item.ExecutionContractEnvelope); err != nil {
			return fmt.Errorf("FetchRuntimeContracts: incompatible contract: %w", err)
		}
	}
	return nil
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
	snapshot := runtimeContractSnapshot(item.ExecutionContractEnvelope, requested.ServiceID, requested.ServiceVersionID, version, item.Service, item.Operations, item.Webhooks)
	if err := validateRuntimeSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateRuntimeSnapshot(snapshot *store.ServiceContractSnapshot) error {
	if err := validateRuntimeExecutionPolicies(&snapshot.ServiceMetadata); err != nil {
		return err
	}
	if err := validateTransportContract(&snapshot.ServiceMetadata, snapshot.Endpoints, snapshot.Webhooks); err != nil {
		return fmt.Errorf("FetchRuntimeContract: invalid transport contract: %w", err)
	}
	return nil
}

// validateRuntimeExecutionPolicies accepts exact v3 only so a stale Registry
// row cannot acquire execution meaning when copied into an Engine snapshot.
func validateRuntimeExecutionPolicies(metadata *fusedobject.ServiceMetadata) error {
	if metadata.RateLimit != nil {
		if err := metadata.RateLimit.Validate(); err != nil {
			return incompatibleRuntimePolicy("rate limit")
		}
	}
	if err := retrypolicy.Validate(metadata.RetryConfig); err != nil {
		return incompatibleRuntimePolicy("retry")
	}
	return nil
}

func incompatibleRuntimePolicy(policy string) error {
	return fmt.Errorf("FetchRuntimeContract: incompatible %s policy: %w", policy, unsupportedExecutionCapability())
}

func validateRuntimePagination(service *runtimeContractService, operations []fusedobject.Endpoint) error {
	if err := validateRuntimePaginationConfig("service", service.Pagination); err != nil {
		return err
	}
	metadata := &fusedobject.ServiceMetadata{Pagination: service.Pagination}
	for i := range operations {
		if err := validateRuntimePaginationConfig("operation", operations[i].Pagination); err != nil {
			return err
		}
		object := fusedToIntegrationObject(metadata, operations[i])
		if object.Pagination != nil && object.Pagination.Version == paginationpolicy.Version {
			if err := engine.ValidatePaginationV3Targets(object, (*paginationpolicy.Config)(object.Pagination)); err != nil {
				return fmt.Errorf("FetchRuntimeContract: invalid operation pagination target: %w", err)
			}
		}
	}
	return nil
}

func validateRuntimePaginationConfig(scope string, config *fusedobject.PaginationConfig) error {
	if config == nil {
		return nil
	}
	if err := paginationpolicy.Validate(config); err != nil {
		return incompatibleRuntimePolicy(scope + " pagination")
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
	Documentation         *fusedobject.ServiceDocumentation  `json:"documentation"`
}

func runtimeContractSnapshot(envelope fusedobject.ExecutionContractEnvelope, serviceID, serviceVersionID uuid.UUID, version string, service *runtimeContractService, operations []fusedobject.Endpoint, webhooks []fusedobject.Webhook) *store.ServiceContractSnapshot {
	metadata := fusedobject.ServiceMetadata{
		ExecutionContractEnvelope: envelope,
		ID:                        service.ID,
		ServiceVersionID:          serviceVersionID,
		Name:                      service.Name,
		Description:               service.Description,
		BaseURL:                   service.BaseURL,
		Servers:                   service.Servers,
		AuthConfigs:               service.AuthConfigs,
		RateLimit:                 service.RateLimit,
		RetryConfig:               service.RetryConfig,
		TimeoutMs:                 service.TimeoutMs,
		Pagination:                service.Pagination,
		DefaultHeaders:            service.DefaultHeaders,
		ConnectConfig:             service.ConnectConfig,
		EventExtractionPath:       service.EventExtractionPath,
		IncomingWebhookConfig:     service.IncomingWebhookConfig,
		Documentation:             service.Documentation,
	}
	return &store.ServiceContractSnapshot{
		ExecutionContractEnvelope: envelope,
		ServiceID:                 serviceID,
		ServiceVersionID:          serviceVersionID,
		Version:                   version,
		Status:                    "active",
		ServiceMetadata:           metadata,
		Endpoints:                 operations,
		Webhooks:                  webhooks,
	}
}
