package capability

import (
	"github.com/Usefused/engine/internal/shared/models"
)

// addImportedMCP includes revision identity and exact callable names in immutable authorization hashes.
func addImportedMCP(keys map[string]struct{}, selection models.SDKSelection) {
	binding := selection.ImportedMCP
	// Ordinary endpoint selections retain their existing capability surface.
	if binding == nil {
		return
	}
	prefix := "service:" + selection.ServiceID.String() + ":" + selection.ServiceVersionID.String() + ":mcp:" + binding.RevisionID.String()
	keys[prefix] = struct{}{}
	for _, item := range models.ImportedMCPCapabilities(selection) {
		keys[prefix+":operation:"+item.OperationID] = struct{}{}
	}
}
