package schemacontract

import (
	"encoding/json"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/schemaref"
)

// PrepareDefinitions admits the version-owned dictionary once before attaching it to consumers.
func PrepareDefinitions(metadata *fusedobject.ServiceMetadata) error {
	// Reject dictionary width before allocating a second map or hashing any attacker-controlled roots.
	if len(metadata.SchemaDefinitions) > schemaref.MaxDefinitions {
		return schemaref.ErrInvalid
	}
	raw := make(map[string]json.RawMessage, len(metadata.SchemaDefinitions))
	remaining := schemaref.MaxBytes
	for name, definition := range metadata.SchemaDefinitions {
		// Aggregate bytes are admitted before canonical hashing can multiply temporary allocations.
		if len(definition.Raw) > remaining {
			return schemaref.ErrInvalid
		}
		remaining -= len(definition.Raw)
		standalone := definition
		standalone.SharedDefinitions = false
		// Each raw hash is verified once, independently of how many operations refer to it.
		if err := Validate(&standalone); err != nil {
			return err
		}
		raw[name] = definition.Raw
	}
	index, err := schemaref.New(raw)
	// Incomplete dictionaries cannot be cached as usable service metadata.
	if err != nil {
		return err
	}
	metadata.DefinitionIndex = index
	return nil
}

// PrepareSnapshot validates all schema references before persistence without copying dictionary payloads.
func PrepareSnapshot(metadata *fusedobject.ServiceMetadata, endpoints []fusedobject.Endpoint, webhooks []fusedobject.Webhook) error {
	// A fresh admission never trusts an index cached from earlier mutable metadata.
	if err := PrepareDefinitions(metadata); err != nil {
		return err
	}
	bind := func(contract *fusedobject.SchemaContract) error {
		contract.DefinitionIndex = metadata.DefinitionIndex
		// Unused dictionary entries are inert; only a root opting into shared lookup requires negotiation.
		if contract.SharedDefinitions && !supportsSharedDefinitions(metadata.RequiredCapabilities) {
			return schemaref.ErrInvalid
		}
		return metadata.DefinitionIndex.Validate(contract.Raw, contract.SharedDefinitions)
	}
	// Operation and inbound schemas share the same exact-version dictionary.
	if err := Walk(endpoints, bind); err != nil {
		return err
	}
	return Walk(webhooks, bind)
}

// supportsSharedDefinitions keeps schema lookup capability distinct from merely storing declared definitions.
func supportsSharedDefinitions(capabilities []string) bool {
	for _, capability := range capabilities {
		// Exact capability identity is required; similar names cannot opt into new reference semantics.
		if capability == fusedobject.ExecutionCapabilityJSONSchemaSharedDefinitionsV1 {
			return true
		}
	}
	return false
}
