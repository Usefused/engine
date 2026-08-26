package fusedobject

import (
	"github.com/Usefused/engine/internal/shared/catalogcontract"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/schemaref"
	"github.com/google/uuid"
)

// ServiceMetadata represents the high-level configuration of a service.
// It acts as a lightweight version of FusedObject without containing every single endpoint.
type ServiceMetadata struct {
	// ExecutionContractEnvelope is runtime-only metadata populated from the
	// snapshot columns. Keeping it out of nested JSON avoids inventing a second
	// wire location while allowing every cache hit and dispatch to revalidate it.
	ExecutionContractEnvelope `json:"-"`
	ID                        uuid.UUID `json:"id"`
	ServiceVersionID          uuid.UUID `json:"service_version_id"`
	Name                      string    `json:"name"`
	Description               string    `json:"description"`
	BaseURL                   string    `json:"base_url"`
	Servers                   Servers   `json:"servers,omitempty"`
	// ServerVariables are workspace-local execution inputs and must never be
	// persisted in Registry snapshots or emitted through runtime telemetry.
	ServerVariables map[string]string `json:"-"`
	AuthConfigs     AuthConfigs       `json:"auth_configs"`
	RawWSDL         string            `json:"raw_wsdl,omitempty"`
	// Definitions belong to this exact version and are serialized once, never on every operation.
	SchemaDefinitions map[string]SchemaContract `json:"schema_definitions,omitempty"`
	DefinitionIndex   *schemaref.Index          `json:"-"`

	// EventExtractionPath and IncomingWebhookConfig describe how this
	// service's provider signs and shapes inbound webhook events. Populated
	// once at workspace-apply time (see upsertDesiredWorkspaceServices,
	// engine/api/workspace_config_handlers.go) and denormalized onto each
	// fused_workspace_webhooks row so webhook ingress never needs to fetch
	// this per inbound request.
	EventExtractionPath   string                 `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *IncomingWebhookConfig `json:"incoming_webhook_config,omitempty"`

	RateLimit   *RateLimitConfig `json:"rate_limit,omitempty"`
	RetryConfig *RetryConfig     `json:"retry_config,omitempty"`
	TimeoutMs   *int             `json:"timeout_ms,omitempty"`
	// Pagination is the service-level execution_policy fallback the dispatcher
	// uses when an endpoint has no spec-derived pagination of its own (see
	// plans/plan-service-config-restructure.md item 1). Endpoint.Pagination
	// still wins whenever the spec declared it -- this is only consulted when
	// that field is nil.
	Pagination     *PaginationConfig            `json:"pagination,omitempty"`
	DefaultHeaders DefaultHeaders               `json:"default_headers,omitempty"`
	ConnectConfig  *ServiceConnectConfig        `json:"connect_config,omitempty"`
	Documentation  *ServiceDocumentation        `json:"documentation,omitempty"`
	Catalog        *catalogcontract.Composition `json:"catalog,omitempty"`
}

// ServiceConnectConfig is the Engine-facing projection of Registry's
// versioned x-fused-connect declaration.
type ServiceConnectConfig = connectionprofile.Profile
type ResourceDiscoveryConfig = connectionprofile.ResourceDiscoveryConfig
type ResourceInputConfig = connectionprofile.ResourceInputConfig
type ResourceInputField = connectionprofile.ResourceInputField
type ResourceInputOption = connectionprofile.ResourceInputOption
type ConnectionBinding = connectionprofile.Binding
