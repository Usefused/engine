package models

import (
	"encoding/json"
	"sort"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/catalogcontract"
	"github.com/google/uuid"
)

// InlineSDKServiceContract carries a credential-free, Engine-admitted service
// snapshot to an Enterprise Local Registry when compiling a mixed Local+Cloud SDK.
type InlineSDKServiceContract struct {
	ServiceID             uuid.UUID                    `json:"service_id"`
	ServiceVersionID      uuid.UUID                    `json:"service_version_id"`
	Version               string                       `json:"version"`
	ContractVersion       int                          `json:"contract_version"`
	RequiredCapabilities  []string                     `json:"required_capabilities,omitempty"`
	ContentSHA256         string                       `json:"content_sha256,omitempty"`
	Name                  string                       `json:"name"`
	Slug                  string                       `json:"slug,omitempty"`
	Description           string                       `json:"description,omitempty"`
	Provider              *ServiceProviderIdentity     `json:"provider,omitempty"`
	BaseURL               string                       `json:"base_url,omitempty"`
	Servers               Servers                      `json:"servers,omitempty"`
	AuthConfigs           AuthConfigs                  `json:"auth_configs,omitempty"`
	RateLimit             *RateLimitConfig             `json:"rate_limit,omitempty"`
	RetryConfig           *RetryConfig                 `json:"retry_config,omitempty"`
	TimeoutMs             *int                         `json:"timeout_ms,omitempty"`
	Pagination            *PaginationConfig            `json:"pagination,omitempty"`
	DefaultHeaders        DefaultHeaders               `json:"default_headers,omitempty"`
	EventExtractionPath   string                       `json:"event_extraction_path,omitempty"`
	IncomingWebhookConfig *IncomingWebhookConfig       `json:"incoming_webhook_config,omitempty"`
	Documentation         *ServiceDocumentation        `json:"documentation,omitempty"`
	Catalog               *catalogcontract.Composition `json:"catalog,omitempty"`
	SchemaDefinitions     map[string]SchemaContract    `json:"schema_definitions,omitempty"`
	Endpoints             []IntegrationObject          `json:"endpoints"`
	Webhooks              []WebhookObject              `json:"webhooks,omitempty"`
}

// ComputeDigest returns a deterministic sha256:<hex> digest over the canonical contract bytes.
func (c InlineSDKServiceContract) ComputeDigest() (string, error) {
	normalized := c
	// Exclude the digest field itself so verification hashes the contract payload cleanly.
	normalized.ContentSHA256 = ""
	if len(normalized.RequiredCapabilities) > 0 {
		caps := append([]string(nil), normalized.RequiredCapabilities...)
		sort.Strings(caps)
		normalized.RequiredCapabilities = caps
	}
	if len(normalized.Endpoints) > 0 {
		eps := append([]IntegrationObject(nil), normalized.Endpoints...)
		sort.Slice(eps, func(i, j int) bool {
			// Tie-break on Name when synthetic test endpoints share an ID.
			if eps[i].ID == eps[j].ID {
				return eps[i].Name < eps[j].Name
			}
			return eps[i].ID.String() < eps[j].ID.String()
		})
		normalized.Endpoints = eps
	}
	if len(normalized.Webhooks) > 0 {
		whs := append([]WebhookObject(nil), normalized.Webhooks...)
		sort.Slice(whs, func(i, j int) bool {
			// Tie-break on Name when synthetic test webhooks share an ID.
			if whs[i].ID == whs[j].ID {
				return whs[i].Name < whs[j].Name
			}
			return whs[i].ID.String() < whs[j].ID.String()
		})
		normalized.Webhooks = whs
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	hexDigest, err := canonicaljson.HexSchemaSHA256(raw)
	if err != nil {
		return "", err
	}
	return "sha256:" + hexDigest, nil
}
