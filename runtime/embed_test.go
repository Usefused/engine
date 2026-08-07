package runtime

import (
	"bytes"
	"testing"
)

func TestMCPSharedRuntimeBundleContainsRuntimeDependencies(t *testing.T) {
	markers := [][]byte{
		[]byte("McpServer"),
		[]byte("StdioServerTransport"),
		[]byte("Zod"),
		[]byte("search_docs"),
	}
	for _, marker := range markers {
		if !bytes.Contains(MCPSharedRuntimeBundle, marker) {
			t.Fatalf("embedded MCP runtime is missing %q", marker)
		}
	}
}
