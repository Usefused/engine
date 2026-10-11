package features

import (
	"errors"
	"os"
)

const ImportedMCPEnvironment = "FUSED_ENABLE_IMPORTED_MCP"

var ErrImportedMCPDisabled = errors.New("imported MCP is disabled; set FUSED_ENABLE_IMPORTED_MCP=true to enable it")

// ImportedMCPEnabled requires explicit operator opt-in across import, admission, and runtime boundaries.
func ImportedMCPEnabled() bool {
	return os.Getenv(ImportedMCPEnvironment) == "true"
}
