package runtime

import _ "embed"

//go:embed mcp/dist/bundle.js
var MCPSharedRuntimeBundle []byte
