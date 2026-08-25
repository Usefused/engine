package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func writeTempFixture(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write temp fixture: %v", err)
	}
	return path
}

const validFixtureJSON = `{
	"operations": [
		{
			"operation_id": "test.getWidget",
			"service_id": "svc-1",
			"name": "Get widget",
			"method": "GET",
			"path": "/widgets/{id}",
			"parameters": [
				{"name": "id", "in": "path", "required": true, "type": "string"}
			],
			"responses": {"200": {"type": "object"}}
		},
		{
			"operation_id": "test.createWidget",
			"service_id": "svc-1",
			"name": "Create widget",
			"method": "POST",
			"path": "/widgets",
			"request_content": {"required":true,"payload_parameter":"body","representations":[{"media_type":"application/octet-stream","serialization":"raw","schema":{"projection":{"type":"string","format":"binary"}}}]},
			"responses": {"201": {"type": "object"}}
		}
	]
}`

func TestLoadFixture_ParsesAndIndexesOperations(t *testing.T) {
	path := writeTempFixture(t, validFixtureJSON)

	f, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("LoadFixture() error = %v", err)
	}

	if len(f.Operations) != 2 {
		t.Fatalf("len(Operations) = %d, want 2", len(f.Operations))
	}

	op, ok := f.Resolve("test.getWidget")
	if !ok {
		t.Fatal("Resolve(\"test.getWidget\") = not found, want found")
	}
	if op.Method != "GET" || op.Path != "/widgets/{id}" {
		t.Errorf("resolved operation = %+v, want method GET path /widgets/{id}", op)
	}
	if len(op.Parameters) != 1 || op.Parameters[0].In != "path" {
		t.Errorf("resolved operation parameters = %+v, want one path param", op.Parameters)
	}
	raw, ok := f.Resolve("test.createWidget")
	if !ok || raw.RequestContent == nil || raw.RequestContent.PayloadParameter != "body" || raw.RequestContent.Representations[0].Schema.Projection.Format != "binary" {
		t.Fatalf("raw request content was not decoded: %#v", raw)
	}
}

func TestLoadFixture_UnregisteredOperationIdFailsToResolve(t *testing.T) {
	path := writeTempFixture(t, validFixtureJSON)
	f, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("LoadFixture() error = %v", err)
	}

	// This is the mechanical enforcement point for Trust and Governance Model
	// tier 1 (design doc): an operationId that was never registered simply
	// fails to resolve, rather than being explicitly denied by some separate
	// check that could be forgotten or bypassed.
	if _, ok := f.Resolve("not.registered"); ok {
		t.Error("Resolve(\"not.registered\") = found, want not found")
	}
}

func TestLoadFixture_DuplicateOperationIdIsRejected(t *testing.T) {
	dup := `{"operations": [
		{"operation_id": "dup", "method": "GET", "path": "/a", "responses": {}},
		{"operation_id": "dup", "method": "GET", "path": "/b", "responses": {}}
	]}`
	path := writeTempFixture(t, dup)

	if _, err := LoadFixture(path); err == nil {
		t.Fatal("LoadFixture() with duplicate operation_id = nil error, want error")
	}
}

// TestLoadFixtureIndexesUnifiedOperations proves exact authored logical names
// remain separate from the established physical resolver.
func TestLoadFixtureIndexesUnifiedOperations(t *testing.T) {
	raw := `{"operations":[],"unified_operations":{"schema_version":3,"operations":[{"name":"release.provision","input_schema":{"type":"object"},"targets":[]}]}}`
	fixture, err := LoadFixture(writeTempFixture(t, raw))
	if err != nil {
		t.Fatalf("LoadFixture() error = %v", err)
	}
	if fixture.UnifiedOperations == nil || len(fixture.UnifiedOperations.Operations) != 1 || fixture.UnifiedOperations.Operations[0].Name != "release.provision" {
		t.Fatalf("UnifiedOperations = %#v, want exact logical operation", fixture.UnifiedOperations)
	}
	// Physical resolution must not begin treating a logical descriptor as a
	// provider endpoint before the dedicated dispatch adapter exists.
	if _, ok := fixture.Resolve("release.provision"); ok {
		t.Fatal("Resolve() returned a Unified operation through the physical index")
	}
}

// TestLoadFixtureRejectsPhysicalUnifiedCollision keeps call(operationId)
// classification independent from input ordering.
func TestLoadFixtureRejectsPhysicalUnifiedCollision(t *testing.T) {
	raw := `{"operations":[{"operation_id":"release.provision","method":"POST","path":"/release","responses":{}}],"unified_operations":{"schema_version":3,"operations":[{"name":"release.provision","input_schema":{"type":"object"},"targets":[]}]}}`
	// Exact cross-kind overlap cannot safely inherit physical duplicate handling
	// because the logical graph has different execution semantics.
	if _, err := LoadFixture(writeTempFixture(t, raw)); err == nil {
		t.Fatal("LoadFixture() collision error = nil, want fail-closed rejection")
	}
}

// TestWriteSessionFixturePreservesUnifiedDescriptor covers the exact Go-to-Node
// file boundary used by every live MCP session.
func TestWriteSessionFixturePreservesUnifiedDescriptor(t *testing.T) {
	fixture := newFixtureFromOperations(t.Context(), nil)
	fixture.UnifiedOperations = &models.SDKUnifiedOperationDescriptors{
		SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion,
		Operations:    []models.SDKUnifiedOperationDescriptor{{Name: "release.provision", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	path, err := writeSessionFixture(t.TempDir(), fixture)
	if err != nil {
		t.Fatalf("writeSessionFixture() error = %v", err)
	}
	loaded, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("LoadFixture(written) error = %v", err)
	}
	if loaded.UnifiedOperations == nil || loaded.UnifiedOperations.Operations[0].Name != "release.provision" {
		t.Fatalf("written Unified descriptor = %#v", loaded.UnifiedOperations)
	}
}

func TestLoadFixture_MissingOperationIdIsRejected(t *testing.T) {
	missing := `{"operations": [{"method": "GET", "path": "/a", "responses": {}}]}`
	path := writeTempFixture(t, missing)

	if _, err := LoadFixture(path); err == nil {
		t.Fatal("LoadFixture() with missing operation_id = nil error, want error")
	}
}

func TestLoadFixture_MissingFileReturnsError(t *testing.T) {
	if _, err := LoadFixture("/nonexistent/fixture.json"); err == nil {
		t.Fatal("LoadFixture() on missing file = nil error, want error")
	}
}

func TestLoadFixture_MalformedJSONReturnsError(t *testing.T) {
	path := writeTempFixture(t, `{not valid json`)
	if _, err := LoadFixture(path); err == nil {
		t.Fatal("LoadFixture() on malformed JSON = nil error, want error")
	}
}

// TestFixtureResolveOnNilFailsClosed guards middleware against a malformed
// session reaching operation resolution without a scoped catalog.
func TestFixtureResolveOnNilFailsClosed(t *testing.T) {
	var f *Fixture
	if _, ok := f.Resolve("anything"); ok {
		t.Error("nil *Fixture.Resolve() = found, want not found")
	}
}
