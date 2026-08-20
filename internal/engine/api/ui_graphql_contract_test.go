package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/parser"
)

type uiGraphQLScan struct {
	CallCount     int                 `json:"call_count"`
	DocumentCount int                 `json:"document_count"`
	Calls         []uiGraphQLCall     `json:"calls"`
	Documents     []uiGraphQLDocument `json:"documents"`
}

type uiGraphQLCall struct {
	Endpoint      string `json:"endpoint"`
	File          string `json:"file"`
	Call          int    `json:"call"`
	DocumentCount int    `json:"document_count"`
}

type uiGraphQLDocument struct {
	Endpoint string `json:"endpoint"`
	File     string `json:"file"`
	Call     int    `json:"call"`
	Variant  int    `json:"variant"`
	Document string `json:"document"`
}

// TestUIEngineGraphQLDocuments validates every Engine-bound UI document against the live schema definition.
func TestUIEngineGraphQLDocuments(t *testing.T) {
	scan := readUIGraphQLManifest(t)
	fixture := &workspaceTestStore{accountID: uuid.New()}
	schema, err := newMCPGraphQLSchema(
		&mockConfigStore{},
		fixture,
		&mockVerifier{},
		&mockRegistryClient{},
		[]byte("12345678901234567890123456789012"),
	)
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	validated := validateUIGraphQLDocuments(t, &schema, scan.Documents, "engine")
	if validated == 0 {
		t.Fatal("UI GraphQL scan returned no Engine documents")
	}
}

// readUIGraphQLManifest loads the UI-owned scan artifact without adding Node dependencies to Go tests.
func readUIGraphQLManifest(t *testing.T) uiGraphQLScan {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve GraphQL contract test path")
	}
	engineRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "../../.."))
	manifest := filepath.Join(engineRoot, "ui/testdata/graphql-documents.json")
	output, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read UI GraphQL manifest: %v", err)
	}
	var scan uiGraphQLScan
	if err := json.Unmarshal(output, &scan); err != nil {
		t.Fatalf("decode UI GraphQL scan: %v", err)
	}
	if scan.CallCount == 0 || scan.CallCount != len(scan.Calls) || scan.DocumentCount != len(scan.Documents) {
		t.Fatalf("invalid UI GraphQL scan counts: calls=%d documents=%d decoded=%d", scan.CallCount, scan.DocumentCount, len(scan.Documents))
	}
	assertUIGraphQLCallsAccountedFor(t, scan)
	return scan
}

// assertUIGraphQLCallsAccountedFor proves every discovered call produced at least one document.
func assertUIGraphQLCallsAccountedFor(t *testing.T, scan uiGraphQLScan) {
	t.Helper()
	documentCount := 0
	seen := map[string]struct{}{}
	for _, call := range scan.Calls {
		key := fmt.Sprintf("%s:%s:call-%d", call.Endpoint, call.File, call.Call)
		if _, exists := seen[key]; exists {
			t.Fatalf("UI GraphQL call was scanned twice: %s", key)
		}
		if call.DocumentCount == 0 {
			t.Fatalf("UI GraphQL call produced no documents: %s", key)
		}
		seen[key] = struct{}{}
		documentCount += call.DocumentCount
	}
	if documentCount != scan.DocumentCount {
		t.Fatalf("UI GraphQL call variants=%d documents=%d", documentCount, scan.DocumentCount)
	}
}

// validateUIGraphQLDocuments parses and validates documents owned by one GraphQL endpoint.
func validateUIGraphQLDocuments(t *testing.T, schema *graphql.Schema, documents []uiGraphQLDocument, endpoint string) int {
	t.Helper()
	validated := 0
	for _, document := range documents {
		if document.Endpoint != endpoint {
			continue
		}
		validated++
		validateUIGraphQLDocument(t, schema, document)
	}
	return validated
}

// validateUIGraphQLDocument reports schema failures at the originating UI call site.
func validateUIGraphQLDocument(t *testing.T, schema *graphql.Schema, document uiGraphQLDocument) {
	t.Helper()
	parsed, err := parser.Parse(parser.ParseParams{Source: document.Document})
	location := fmt.Sprintf("%s call %d variant %d", document.File, document.Call, document.Variant)
	if err != nil {
		t.Errorf("%s: parse GraphQL document: %v", location, err)
		return
	}
	result := graphql.ValidateDocument(schema, parsed, nil)
	if result.IsValid {
		return
	}
	messages := make([]string, 0, len(result.Errors))
	for _, validationErr := range result.Errors {
		messages = append(messages, validationErr.Message)
	}
	t.Errorf("%s: schema validation failed: %s", location, strings.Join(messages, "; "))
}
