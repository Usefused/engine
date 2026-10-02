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

// TestUnifiedAppFetchRejectsInvalidPagination keeps malformed page bounds out of provider dispatch.
func TestUnifiedAppFetchRejectsInvalidPagination(t *testing.T) {
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

// TestUnifiedAppSelectedOperationBound permits event-only manifests while retaining the finite provider limit.
func TestUnifiedAppSelectedOperationBound(t *testing.T) {
	for _, test := range []struct {
		count   int
		allowed bool
	}{{0, true}, {maxUnifiedAppSelectedOperations, true}, {maxUnifiedAppSelectedOperations + 1, false}} {
		manifest := unifiedAppManifest{
			SchemaVersion: 1, InputSchema: json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}
		for index := 0; index < test.count; index++ {
			manifest.SelectedOperations = append(manifest.SelectedOperations, unifiedAppManifestOperation{
				Service: "selected", Operation: fmt.Sprintf("op_%d", index),
				ServiceID: uuid.New(), ServiceVersionID: uuid.New(), EndpointID: uuid.New(),
			})
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseUnifiedAppManifest(raw)
		// Empty physical scope is valid for an event-only app; excessive scope still fails admission.
		if (err == nil) != test.allowed {
			t.Fatalf("selected operations=%d admitted=%v error=%v", test.count, err == nil, err)
		}
	}
}

// TestUnifiedAppEventOnlyConfigScope requires an explicit trigger when physical operation scope is empty.
func TestUnifiedAppEventOnlyConfigScope(t *testing.T) {
	// An event-only app has an inbound capability despite having no provider method binding.
	if err := validateExecutionOperationScope(map[string]sdkConfigServiceDoc{"issues": {Webhooks: []string{"issue.created"}}}); err != nil {
		t.Fatalf("event-only scope rejected: %v", err)
	}
	// An empty service pin cannot create a callable or triggered hosted app.
	if err := validateExecutionOperationScope(map[string]sdkConfigServiceDoc{"issues": {}}); err == nil {
		t.Fatal("empty scope accepted")
	}
}
