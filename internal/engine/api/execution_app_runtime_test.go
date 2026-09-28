package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestExecutionAppSelectedOperationBound keeps manifest parsing and scope admission on one finite limit.
func TestExecutionAppSelectedOperationBound(t *testing.T) {
	for _, test := range []struct {
		count   int
		allowed bool
	}{{0, false}, {maxExecutionAppSelectedOperations, true}, {maxExecutionAppSelectedOperations + 1, false}} {
		manifest := executionAppManifest{
			SchemaVersion: 1, InputSchema: json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}
		for index := 0; index < test.count; index++ {
			manifest.SelectedOperations = append(manifest.SelectedOperations, executionAppManifestOperation{
				Service: "selected", Operation: fmt.Sprintf("op_%d", index),
				ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New(),
			})
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseExecutionAppManifest(raw)
		// Empty and excessive manifests must fail before deployment or execution can resolve provider scope.
		if (err == nil) != test.allowed {
			t.Fatalf("selected operations=%d admitted=%v error=%v", test.count, err == nil, err)
		}
	}
}
