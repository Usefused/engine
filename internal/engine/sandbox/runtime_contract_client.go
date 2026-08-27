package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxRuntimeContractsResponseBytes int64 = 128 << 20

var errRuntimeContractsResponseLimit = errors.New("runtime_contract_response_limit_exceeded: Registry response exceeds the 128 MiB limit; request fewer service versions")

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

// FetchRuntimeContracts admits one bounded Registry batch without retrying rejected contracts per service.
func (c *HTTPRegistryClient) FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error) {
	// Empty selections perform no network or Registry work.
	if len(versions) == 0 {
		return nil, nil
	}
	req, err := c.buildRuntimeContractsRequest(ctx, versions, apiKey)
	// Request construction failure cannot be repaired by dropping selected versions.
	if err != nil {
		return nil, err
	}
	resp, err := c.doWithCallerDeadline(req)
	// Transport failure preserves the one-batch ownership and recovery boundary.
	if err != nil {
		return nil, fmt.Errorf("FetchRuntimeContracts: request failed: %w", err)
	}
	defer resp.Body.Close()
	// Registry error content remains useful diagnostics but cannot allocate an unbounded body.
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("FetchRuntimeContracts: registry returned %d: %s", resp.StatusCode, runtimeContractErrorBody(resp.Body))
	}
	snapshots, err := decodeRuntimeContractsResponse(resp.Body, versions)
	recordPassiveContractSummary(ctx, snapshots, err)
	return snapshots, err
}

// runtimeContractErrorBody preserves bounded error context and makes truncation explicit rather than silently hiding it.
func runtimeContractErrorBody(body io.Reader) string {
	const limit = 64 << 10
	payload, err := io.ReadAll(io.LimitReader(body, limit+1))
	// An explicit marker distinguishes omitted tail content from the Registry's complete error message.
	if len(payload) > limit {
		return string(payload[:limit]) + " [Registry error body truncated at 64 KiB]"
	}
	// Partial transport reads are surfaced rather than presented as complete provider diagnostics.
	if err != nil {
		return string(payload) + " [Registry error body could not be read completely]"
	}
	return string(payload)
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
			revision
			source_hash
			generation_contract_hash
			schema_definitions
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
	provider { name handle }
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
	Errors []runtimeContractGraphQLError `json:"errors"`
}

type runtimeContractBatchItem struct {
	fusedobject.ExecutionContractEnvelope
	Revision               int                                   `json:"revision,omitempty"`
	SourceHash             string                                `json:"source_hash,omitempty"`
	GenerationContractHash string                                `json:"generation_contract_hash,omitempty"`
	SchemaDefinitions      map[string]fusedobject.SchemaContract `json:"schema_definitions"`
	ServiceID              uuid.UUID                             `json:"service_id"`
	ServiceVersionID       uuid.UUID                             `json:"service_version_id"`
	Version                string                                `json:"version"`
	Service                *runtimeContractService               `json:"service"`
	Operations             []fusedobject.Endpoint                `json:"operations"`
	Webhooks               []fusedobject.Webhook                 `json:"webhooks"`
}

// decodeRuntimeContractsResponse separates contract rejection from transport and identity failure.
func decodeRuntimeContractsResponse(body io.Reader, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	decoded, err := decodeBoundedRuntimeContracts(body, maxRuntimeContractsResponseBytes)
	// Unreadable responses provide no per-service proof and remain fatal to recovery.
	if err != nil {
		return nil, err
	}
	// Only an explicit, version-bound Registry classification is recoverable.
	if len(decoded.Errors) > 0 {
		return nil, classifyRuntimeContractGraphQLErrors(decoded.Errors, versions)
	}
	return runtimeContractSnapshotsFromBatch(decoded.Data.Contracts, versions)
}

// decodeBoundedRuntimeContracts bounds the entire batch and rejects trailing JSON instead of admitting a valid prefix.
func decodeBoundedRuntimeContracts(body io.Reader, limit int64) (runtimeContractsGraphQLResponse, error) {
	var decoded runtimeContractsGraphQLResponse
	reader := &io.LimitedReader{R: body, N: limit + 1}
	decoder := json.NewDecoder(reader)
	err := decoder.Decode(&decoded)
	// The sentinel byte distinguishes actual overrun from malformed but bounded input.
	if reader.N == 0 {
		return decoded, errRuntimeContractsResponseLimit
	}
	// Decode errors never echo untrusted payload fragments or suggest per-service retries.
	if err != nil {
		return decoded, incompatibleRuntimeResponse()
	}
	err = decoder.Decode(new(any))
	// Trailing whitespace is part of the aggregate budget, not an unlimited post-document stream.
	if reader.N == 0 {
		return decoded, errRuntimeContractsResponseLimit
	}
	// Exactly one response object is required; a second value cannot be silently ignored.
	if err != io.EOF {
		return decoded, incompatibleRuntimeResponse()
	}
	return decoded, nil
}

// incompatibleRuntimeResponse preserves the existing safe compatibility classification for malformed JSON.
func incompatibleRuntimeResponse() error {
	return fmt.Errorf("FetchRuntimeContracts: incompatible contract: %w", unsupportedExecutionCapability())
}

// runtimeContractSnapshotsFromBatch keeps ordinary callers atomic while allowing
// owned-service recovery to explicitly inspect independently validated snapshots.
func runtimeContractSnapshotsFromBatch(items []runtimeContractBatchItem, versions []store.WorkspaceServiceVersion) ([]store.ServiceContractSnapshot, error) {
	byVersion, err := indexRuntimeContractBatch(items, versions)
	// Missing, duplicate or cross-service identities cannot be treated as malformed contract content.
	if err != nil {
		return nil, err
	}
	out := make([]store.ServiceContractSnapshot, 0, len(versions))
	rejected := &runtimeContractRejections{}
	for _, version := range versions {
		snapshot, err := admittedRuntimeContractSnapshot(byVersion[version.ServiceVersionID], version)
		// No failed snapshot is returned as executable; only startup recovery consumes the partial set.
		if err != nil {
			rejected.failures = append(rejected.failures, ownedServiceRejection(version, err))
			continue
		}
		out = append(out, *snapshot)
	}
	// Normal plan/apply clients still receive no partial success when any item is rejected.
	if len(rejected.failures) > 0 {
		rejected.accepted = out
		return nil, rejected
	}
	return out, nil
}

// runtimeContractSnapshotFromBatchItem pins shared definitions beside the requested immutable version.
func runtimeContractSnapshotFromBatchItem(item runtimeContractBatchItem, requested store.WorkspaceServiceVersion) (*store.ServiceContractSnapshot, error) {
	// Requested identity is authoritative even when the Registry returns a valid but different contract.
	if item.Service == nil || item.ServiceID != requested.ServiceID || item.ServiceVersionID != requested.ServiceVersionID {
		return nil, fmt.Errorf("FetchRuntimeContracts: service %s version %s not found", requested.ServiceID, requested.ServiceVersionID)
	}
	version := item.Version
	// A missing display label may use the requested label, never a different version identity.
	if version == "" {
		version = requested.Version
	}
	snapshot := runtimeContractSnapshot(item.ExecutionContractEnvelope, requested.ServiceID, requested.ServiceVersionID, version, item.Service, item.Operations, item.Webhooks)
	// The generation pin is Registry-owned; the separately computed runtime hash cannot substitute for it.
	snapshot.Revision, snapshot.SourceHash, snapshot.GenerationContractHash = item.Revision, item.SourceHash, item.GenerationContractHash
	snapshot.ServiceMetadata.SchemaDefinitions = item.SchemaDefinitions
	// One incompatible operation rejects the complete version before Engine storage.
	if err := ValidateRuntimeContractSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// ValidateRuntimeContractSnapshot runs the complete Engine admission path and computes its hash without persisting the candidate.
func ValidateRuntimeContractSnapshot(snapshot *store.ServiceContractSnapshot) error {
	// A nil candidate cannot establish either runtime semantics or immutable identity.
	if snapshot == nil {
		return errors.New("runtime contract snapshot is missing")
	}
	// Runtime metadata must describe the same immutable pin used by local lookup and hashing.
	if snapshot.ServiceMetadata.ID != snapshot.ServiceID || snapshot.ServiceMetadata.ServiceVersionID != snapshot.ServiceVersionID {
		return errors.New("runtime contract snapshot identity does not match service metadata")
	}
	// Pagination target validation needs the effective service policy and every operation together.
	if err := validateRuntimePagination(&snapshot.ServiceMetadata, snapshot.Endpoints); err != nil {
		return err
	}
	// Execution policies are admitted before transport mapping can consume them.
	if err := validateRuntimeExecutionPolicies(&snapshot.ServiceMetadata); err != nil {
		return err
	}
	// Transport validation covers outbound operations and passive inbound contracts as one version.
	if err := validateTransportContract(&snapshot.ServiceMetadata, snapshot.Endpoints, snapshot.Webhooks); err != nil {
		return fmt.Errorf("FetchRuntimeContract: invalid transport contract: %w", err)
	}
	validated, err := store.ValidateServiceContractSnapshot(*snapshot)
	// Store admission independently verifies capability, schema, generation-pin, and immutable child identity.
	if err != nil {
		return err
	}
	*snapshot = validated
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

// validateRuntimePagination verifies service and operation policies against the exact executable request targets.
func validateRuntimePagination(metadata *fusedobject.ServiceMetadata, operations []fusedobject.Endpoint) error {
	// A missing service projection cannot provide effective pagination defaults.
	if metadata == nil {
		return errors.New("FetchRuntimeContract: service metadata is missing")
	}
	// Service pagination must be canonical before it can become an operation fallback.
	if err := validateRuntimePaginationConfig("service", metadata.Pagination); err != nil {
		return err
	}
	// Each operation must be checked because service fallback changes its effective target contract.
	for i := range operations {
		// An exact operation override is independently validated before target resolution.
		if err := validateRuntimePaginationConfig("operation", operations[i].Pagination); err != nil {
			return err
		}
		object := fusedToIntegrationObject(metadata, operations[i])
		// Target checks apply only to canonical v3; absent pagination keeps ordinary single-call execution.
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
	Provider              *models.ServiceProviderIdentity    `json:"provider,omitempty"`
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

// runtimeContractSnapshot projects one Registry version without discarding its credential-free provider identity.
func runtimeContractSnapshot(envelope fusedobject.ExecutionContractEnvelope, serviceID, serviceVersionID uuid.UUID, version string, service *runtimeContractService, operations []fusedobject.Endpoint, webhooks []fusedobject.Webhook) *store.ServiceContractSnapshot {
	metadata := fusedobject.ServiceMetadata{
		ExecutionContractEnvelope: envelope,
		ID:                        service.ID,
		ServiceVersionID:          serviceVersionID,
		Name:                      service.Name,
		Description:               service.Description,
		Provider:                  service.Provider,
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
