package models

import (
	"encoding/json"
	"github.com/google/uuid"
)

// ImportedMCPSelection names a reviewed catalog revision and explicit upstream capabilities.
type ImportedMCPSelection struct {
	RevisionID        uuid.UUID `json:"revision_id"`
	Tools             []string  `json:"tools,omitempty"`
	Prompts           []string  `json:"prompts,omitempty"`
	Resources         []string  `json:"resources,omitempty"`
	ResourceTemplates []string  `json:"resource_templates,omitempty"`
}

// ImportedMCPBinding freezes only admitted definitions; credentials remain Engine-owned references.
type ImportedMCPBinding struct {
	RevisionID        uuid.UUID         `json:"revision_id"`
	URL               string            `json:"url"`
	SecretName        string            `json:"secret_name,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	Prompts           []json.RawMessage `json:"prompts,omitempty"`
	Resources         []json.RawMessage `json:"resources,omitempty"`
	ResourceTemplates []json.RawMessage `json:"resource_templates,omitempty"`
}

// ImportedMCPOperation keeps token grants and documentation in the same service-qualified namespace.
func ImportedMCPOperation(service uuid.UUID, kind, name string) string {
	return "mcp:" + service.String() + ":" + kind + ":" + name
}

// ImportedMCPCapability is a credential-free immutable name used by app details and token selection.
type ImportedMCPCapability struct {
	Kind        string
	Name        string
	OperationID string
}

// ImportedMCPCapabilities enumerates all callable namespaces in deterministic display order.
func ImportedMCPCapabilities(selection SDKSelection) []ImportedMCPCapability {
	result := []ImportedMCPCapability{}
	binding := selection.ImportedMCP
	// Physical-only selections contribute no imported grants.
	if binding == nil {
		return result
	}
	groups := []struct {
		kind  string
		items []json.RawMessage
	}{{"tool", binding.Tools}, {"prompt", binding.Prompts}, {"resource", binding.Resources}, {"template", binding.ResourceTemplates}}
	for _, group := range groups {
		for _, raw := range group.items {
			var item struct {
				Name     string `json:"name"`
				URI      string `json:"uri"`
				Template string `json:"uriTemplate"`
			}
			_ = json.Unmarshal(raw, &item)
			key := item.Name
			// Resources grant exact URI identity, independently of their non-unique display names.
			switch group.kind {
			case "resource":
				key = item.URI
			case "template":
				key = item.Template
			}
			result = append(result, ImportedMCPCapability{Kind: group.kind, Name: item.Name, OperationID: ImportedMCPOperation(selection.ServiceID, group.kind, key)})
		}
	}
	return result
}
