package runtime

import _ "embed"

//go:embed mcp/dist/bundle.js
var MCPSharedRuntimeBundle []byte

// MCPMetadataBundle contains only trusted catalogue search and tool declarations, with no provider execution.
//
//go:embed mcp/dist/metadata-bundle.js
var MCPMetadataBundle string
