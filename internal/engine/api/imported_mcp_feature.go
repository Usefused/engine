package api

import "github.com/Usefused/engine/internal/shared/features"

// validateImportedMCPFeature rejects authored imports without affecting ordinary MCP apps.
func validateImportedMCPFeature(doc sdkConfigDocument) error {
	for _, service := range doc.Services {
		// Stored desired state is checked again on apply so an older plan cannot bypass a disabled flag.
		if service.MCP != nil && !features.ImportedMCPEnabled() {
			return features.ErrImportedMCPDisabled
		}
	}
	return nil
}
