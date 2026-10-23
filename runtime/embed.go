package runtime

import _ "embed"

//go:embed mcp/dist/bundle.js
var MCPSharedRuntimeBundle []byte

// MCPMetadataBundle contains only trusted catalogue search and tool declarations, with no provider execution.
//
//go:embed mcp/dist/metadata-bundle.js
var MCPMetadataBundle string

// MCPGuidanceJSON is the shared server-owned guidance, never provider-authored instructions.
//
//go:embed mcp/src/guidance.json
var MCPGuidanceJSON []byte
