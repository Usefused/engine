package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/schemacontract"
	"github.com/Usefused/engine/internal/shared/schemaref"
)

type mcpDefinitionLoader interface {
	MCPDefinitionsForSelections([]models.SDKSelection) (map[string]map[string]fusedobject.SchemaContract, error)
}

// MCPDefinitionsForSelections shares already-loaded exact-version dictionaries without a query per service.
func (cache *LocalObjectCache) MCPDefinitionsForSelections(selections []models.SDKSelection) (map[string]map[string]fusedobject.SchemaContract, error) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	result := make(map[string]map[string]fusedobject.SchemaContract)
	for _, selection := range selections {
		version, err := selectionVersionIdentity(selection)
		// Scope identity must match the same canonical cache key used during ConnectSDK.
		if err != nil {
			return nil, err
		}
		metadata := cache.serviceMetadataCache[selection.ServiceID.String()+":"+version]
		// Standalone schemas need no dictionary; marked roots still fail closed in fixture admission if absent.
		if metadata == nil {
			continue
		}
		// Omission preserves legacy fixture identity without empty dictionary entries.
		if len(metadata.SchemaDefinitions) > 0 {
			result[selection.ServiceVersionID.String()] = metadata.SchemaDefinitions
		}
	}
	return result, nil
}

// attachMCPDefinitions keeps schema truth beside the catalogue, not duplicated inside each operation.
func attachMCPDefinitions(fixture *Fixture, cache ObjectCache, selections []models.SDKSelection) error {
	loader, ok := cache.(mcpDefinitionLoader)
	// Older test/offline adapters remain valid only for standalone schema envelopes.
	if !ok {
		return validateMCPSharedSchemaReferences(fixture)
	}
	definitions, err := loader.MCPDefinitionsForSelections(selections)
	// Metadata lookup failure must not turn shared references into empty documentation.
	if err != nil {
		return err
	}
	fixture.SchemaDefinitions = definitions
	return validateMCPSharedSchemaReferences(fixture)
}

// validateMCPSharedSchemaReferences validates each dictionary once and binds roots to their exact version.
func validateMCPSharedSchemaReferences(fixture *Fixture) error {
	indexes := make(map[string]*schemaref.Index, len(fixture.SchemaDefinitions))
	for version, definitions := range fixture.SchemaDefinitions {
		metadata := fusedobject.ServiceMetadata{SchemaDefinitions: definitions, ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{RequiredCapabilities: []string{fusedobject.ExecutionCapabilityJSONSchemaSharedDefinitionsV1}}}
		// Offline fixture admission repeats the trusted Engine dictionary invariant.
		if err := schemacontract.PrepareDefinitions(&metadata); err != nil {
			return ErrMCPSchemaInvalid
		}
		indexes[version] = metadata.DefinitionIndex
	}
	for position := range fixture.Operations {
		operation := &fixture.Operations[position]
		// The visitor checks every schema-bearing media/header surface without raw schema expansion.
		if err := bindMCPDefinitionIndex(operation, indexes[operation.ServiceVersionID]); err != nil {
			return err
		}
	}
	return nil
}

// bindMCPDefinitionIndex uses the common typed-envelope walker to avoid a second list of schema surfaces.
func bindMCPDefinitionIndex(operation *FixtureOperation, index *schemaref.Index) error {
	return schemacontract.Walk(operation, func(contract *models.SchemaContract) error {
		contract.DefinitionIndex = index
		// Missing dictionaries and dangling roots fail before the Node process receives the fixture.
		if err := index.Validate(contract.Raw, contract.SharedDefinitions); err != nil {
			return ErrMCPSchemaInvalid
		}
		return nil
	})
}

// admitDefinitions applies existing MCP per-schema and aggregate limits to each shared definition once.
func (admission *mcpSchemaAdmission) admitDefinitions(versions map[string]map[string]fusedobject.SchemaContract) error {
	for _, definitions := range versions {
		for _, definition := range definitions {
			// Compact references cannot hide oversized, deep, or wide schema truth from session admission.
			if err := admission.admitSchemaParts(definition.Raw, definition.Projection); err != nil {
				return err
			}
		}
	}
	return nil
}
