package sandbox

import (
	"os"
	"path/filepath"
	"testing"
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
			"request_body": {"type": "object", "required": ["name"]},
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
