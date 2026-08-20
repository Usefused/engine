package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppOpenAPIDocumentationKeepsAuthAndVersionBoundariesDiscoverable ensures
// the public guide cannot collapse the control-plane export and runtime call.
func TestAppOpenAPIDocumentationKeepsAuthAndVersionBoundariesDiscoverable(t *testing.T) {
	documentation := readAppOpenAPIDocumentation(t, "docs", "app-execution-rest.md")
	required := []string{
		"GET /apps/{app_id}/openapi",
		"X-API-Key: <control-plane-credential>",
		"exact SDK `app_id` UUID",
		"`app.read`",
		"OpenAPI 3.1",
		"POST /v1/apps/{app_id}/executions",
		"SDK-wide execution token",
		"?operation=createIssue",
		"16 MiB",
	}
	for _, token := range required {
		if !strings.Contains(documentation, token) {
			t.Errorf("REST execution guide is missing OpenAPI export contract %q", token)
		}
	}
}

// TestEngineREADMELinksAppOpenAPIExport keeps the generated contract reachable
// without checking in a generic document that can drift from an app version.
func TestEngineREADMELinksAppOpenAPIExport(t *testing.T) {
	readme := readAppOpenAPIDocumentation(t, "README.md")
	for _, token := range []string{"per-version OpenAPI export", "docs/app-execution-rest.md"} {
		if !strings.Contains(readme, token) {
			t.Errorf("Engine README is missing OpenAPI export link token %q", token)
		}
	}
}

// readAppOpenAPIDocumentation reads one repository-root document from the API
// package test working directory and fails with the resolved path for diagnosis.
func readAppOpenAPIDocumentation(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
