package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

// TestExecutionAppFetchRejectsInvalidPagination keeps malformed page bounds out of provider dispatch.
func TestExecutionAppFetchRejectsInvalidPagination(t *testing.T) {
	host := &executionCapabilityHost{bindings: map[string]sandbox.ExactOperationBinding{}}
	for _, maxPages := range []int{0, -1} {
		request := fmt.Sprintf(`{"service":"crm","operation":"list","input":{},"pagination":{"maxPages":%d}}`, maxPages)
		_, err := host.Fetch(context.Background(), json.RawMessage(request))
		// Invalid bounds must fail before binding lookup or a provider-capable runtime is needed.
		if !errors.Is(err, engine.ErrPaginationIntentInvalid) {
			t.Fatalf("maxPages=%d error = %v, want pagination intent error", maxPages, err)
		}
	}
}

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
